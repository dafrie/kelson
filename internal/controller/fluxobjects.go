package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/model"
)

// FieldOwner is the field manager every write in this package carries.
//
// Server-side apply attributes each field to the manager that set it, so two
// managers disagreeing about a field is a conflict the API server reports
// rather than a silent overwrite (docs/delivery.md, "Field ownership"). The
// name is the process, not the object: an operator running `kubectl get
// kustomization -o yaml` sees `kelson-controller` against every field kelson
// owns and their own name against anything they added.
const FieldOwner = client.FieldOwner("kelson-controller")

// ObjectName is the name of both Flux objects for one pair. They share it on
// purpose: a Kustomization's sourceRef is a name in the same namespace, so one
// name means the pair cannot be mismatched, and `kubectl get
// ocirepository,kustomization -n kelson-system checkout-production` is the
// whole deployment.
func ObjectName(project, environment string) string { return project + "-" + environment }

// ociRepositoryGVK and kustomizationGVK are the coordinates kelson writes.
// They come from internal/delivery/flux rather than from a second set of
// constants here, because the version this controller *applies* has to be the
// version that package *reads back* — the alternative is a controller whose own
// observer does not watch what it wrote (ADR-0028 decision 3).
var (
	ociRepositoryGVK = schema.GroupVersionKind{
		Group:   flux.OCIRepositoryGVR.Group,
		Version: flux.OCIRepositoryGVR.Version,
		Kind:    flux.KindOCIRepository,
	}
	kustomizationGVK = schema.GroupVersionKind{
		Group:   flux.KustomizationGVR.Group,
		Version: flux.KustomizationGVR.Version,
		Kind:    flux.KindKustomization,
	}

	// The same two coordinates as apiVersion/kind strings, exported because
	// cmd/kelson-controller has to name them to scope the manager's cache — and
	// naming them there as literals would be the second spelling this variable
	// block exists to prevent.
	OCIRepositoryAPIVersion = ociRepositoryGVK.GroupVersion().String()
	KustomizationAPIVersion = kustomizationGVK.GroupVersion().String()
)

// The kinds behind those API versions, re-exported so a caller needs one import
// rather than two.
const (
	KindOCIRepository = flux.KindOCIRepository
	KindKustomization = flux.KindKustomization
)

// ensure is ADR-0028 step 5: server-side apply the OCIRepository and
// Kustomization pair that consume the artifact just published.
//
// # Why both objects live in kelson-system
//
// They are kelson's objects, not the application's. A Kustomization that can be
// deleted by someone tidying an application namespace is a deployment that
// silently stops reconciling, and a `prune: true` object with that blast radius
// is not something to leave in a namespace an application team owns (ADR-0028
// decision 3).
//
// # Why the apply is forced
//
// kelson owns every field it writes here and nothing else should be writing
// them. Without ForceOwnership, an operator who once ran `kubectl apply` over
// the Kustomization by hand leaves their field manager on `spec.interval`
// forever, and every subsequent reconcile fails on a conflict about a value
// nobody disagrees with. Forcing takes the field back and says so in the
// managed fields. What is *not* forced is the name: [ReasonNameConflict] is
// checked before anything is written, because taking a field is recoverable and
// taking somebody else's deployment is not.
func (d *FluxDeliverer) ensure(ctx context.Context, rev Revision, repository, tag, digest string) error {
	name := ObjectName(rev.Project, rev.Environment)
	labels := d.labels(rev)

	if err := d.checkConflict(ctx, ociRepositoryGVK, name, rev); err != nil {
		return err
	}
	if err := d.checkConflict(ctx, kustomizationGVK, name, rev); err != nil {
		return err
	}

	if err := d.apply(ctx, d.ociRepository(name, labels, repository, tag, digest)); err != nil {
		return err
	}
	return d.apply(ctx, d.kustomization(name, labels, rev))
}

// labels are the provenance every kelson object carries, plus the one this pair
// needs and no other object does: which namespace the deployment lands in.
// See [delivery.LabelEnvironmentNamespace] for why.
func (d *FluxDeliverer) labels(rev Revision) map[string]string {
	return map[string]string{
		delivery.LabelManagedBy:            delivery.ManagedByKelson,
		delivery.LabelProject:              rev.Project,
		delivery.LabelEnvironment:          rev.Environment,
		delivery.LabelEnvironmentNamespace: rev.EnvironmentNamespace,
	}
}

func (d *FluxDeliverer) ociRepository(name string, labels map[string]string, repository, tag, digest string) *unstructured.Unstructured {
	u := d.newObject(ociRepositoryGVK, name, labels)
	// Both halves of the reference, when kelson knows both. A tag is a name
	// somebody can rewrite — nothing in kelson does, but a registry is a shared
	// system and an operator, a mirror or a retention policy can — while the
	// digest is the bytes themselves. source-controller prefers the digest when
	// the two are set together, so pinning both means what the cluster pulls
	// cannot drift from what this controller pushed, and the tag stays in the
	// object for the human reading `kubectl get ocirepository`.
	//
	// The digest is unknown in exactly one case worth stating: a rollback to a
	// history entry recorded before the digest was (or by a version that did not
	// record one). That is a tag-only pin, which is what the whole spine did
	// before this, and it stays correct because a kelson tag is written once.
	ref := map[string]any{"tag": tag}
	if digest != "" {
		ref["digest"] = digest
	}
	spec := map[string]any{
		"interval": d.interval(),
		"url":      "oci://" + repository,
		"ref":      ref,
	}
	// Plain HTTP is an operator's named exception and never a guess: a registry
	// that fails its TLS handshake must not be silently downgraded, because
	// that is how a credential ends up on the wire in clear
	// (internal/build/registry/insecure.go).
	if d.insecure(repository) {
		spec["insecure"] = true
	}
	// A private registry needs a pull credential *in the cluster* as well as
	// one in the controller: the controller pushes and source-controller pulls,
	// and they are different processes with different service accounts.
	if d.PullSecret != "" {
		spec["secretRef"] = map[string]any{"name": d.PullSecret}
	}
	u.Object["spec"] = spec
	return u
}

func (d *FluxDeliverer) kustomization(name string, labels map[string]string, rev Revision) *unstructured.Unstructured {
	u := d.newObject(kustomizationGVK, name, labels)
	spec := map[string]any{
		"interval": d.interval(),
		// The artifact is a flat directory of rendered manifests at its root
		// (ADR-0017 decision 10), which is what the publisher writes and what
		// this builds.
		"path":  "./",
		"prune": true,
		// wait: true is what makes Ready mean *healthy* rather than *applied*.
		// kustomize-controller assesses the health of everything it applied and
		// only then reports Ready, so kelson's phase mapping gets the answer
		// from the component that already computes it — and R1 needs no RBAC on
		// the workloads at all to report on them.
		"wait":            true,
		"targetNamespace": rev.TargetNamespace,
		"sourceRef": map[string]any{
			"kind": flux.KindOCIRepository,
			"name": name,
		},
	}
	// SOPS (ADR-0022, ADR-0028 decision 7). The encrypted Secrets ship inside
	// the artifact alongside the workloads that reference them, and
	// kustomize-controller decrypts per Kustomization — it does not care
	// whether the source is git or OCI. kelson writing this block is what makes
	// the old "committed but never decrypted" failure structurally unreachable.
	//
	// The named Secret is read from the Kustomization's *own* namespace, which
	// is the Flux namespace and not the workload's: that is
	// kustomize-controller's rule, and it is where the age identity has to be.
	if rev.Resolved != nil && rev.Resolved.Environment.Secrets.Backend == model.SecretsSOPS {
		spec["decryption"] = map[string]any{
			"provider":  "sops",
			"secretRef": map[string]any{"name": rev.Resolved.Environment.Secrets.AgeKeySecret},
		}
	}
	u.Object["spec"] = spec
	return u
}

func (d *FluxDeliverer) newObject(gvk schema.GroupVersionKind, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(d.namespace())
	u.SetLabels(labels)
	return u
}

// apply is the server-side apply itself.
//
// The object is handed over as an *apply configuration* rather than as a patch,
// which is controller-runtime's own distinction and the correct one here: an
// apply configuration is a statement of the fields this manager intends to own,
// and an unstructured object kelson built field by field is exactly that. The
// warning attached to ApplyConfigurationFromUnstructured — do not pass an
// unstructured converted *from* an API object, because a zero value then cannot
// be told from an unset one — is why these are built literally in
// [FluxDeliverer.ociRepository] and [FluxDeliverer.kustomization] and never
// round-tripped through a live object.
func (d *FluxDeliverer) apply(ctx context.Context, obj *unstructured.Unstructured) error {
	config := client.ApplyConfigurationFromUnstructured(obj)
	if err := d.Client.Apply(ctx, config, FieldOwner, client.ForceOwnership); err != nil {
		return classifyApply(err, obj)
	}
	return nil
}

// checkConflict refuses to write over an object of the same name that belongs
// to a different environment.
//
// Two Environments called `production`, in two namespaces, both binding a
// Project called `checkout`, resolve to one object name in one Flux namespace.
// Server-side apply would take the object without a word, and the second
// environment's workloads would land in the first one's namespace — a data
// incident produced by a naming collision. So the live object's
// environment-namespace label is read first, and a mismatch stops everything:
// nothing is written, the status says which namespace already holds the name,
// and no requeue is scheduled because nothing will change on its own.
func (d *FluxDeliverer) checkConflict(ctx context.Context, gvk schema.GroupVersionKind, name string, rev Revision) error {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gvk)
	key := types.NamespacedName{Namespace: d.namespace(), Name: name}
	if err := d.Client.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return classifyApply(err, live)
	}
	owner := live.GetLabels()[delivery.LabelEnvironmentNamespace]
	// An object with no label at all is one kelson wrote before the label
	// existed, or one an operator hand-wrote. Adopting it is the right answer:
	// the alternative is a controller that refuses to converge on an object it
	// would otherwise have created identically.
	if owner == "" || owner == rev.EnvironmentNamespace {
		return nil
	}
	return newDeliveryError(v1alpha1.ReasonNameConflict, fmt.Sprintf(
		"%s %s/%s already belongs to the Environment in namespace %s, and this one is in %s. "+
			"Two Environments of the same name binding Projects of the same name resolve to one object "+
			"name in %s. Rename one of the pair, or run them in one namespace.",
		gvk.Kind, d.namespace(), name, owner, rev.EnvironmentNamespace, d.namespace()), nil)
}

// classifyApply maps an API-server refusal onto the taxonomy. Forbidden is
// RBAC — an operator's to grant, so it waits on a timer rather than backing off
// against a permission that will not appear by itself. A conflict survives
// ForceOwnership only when something structural is contended, which is also a
// human's to look at.
func classifyApply(err error, obj *unstructured.Unstructured) error {
	doing := fmt.Sprintf("applying %s %s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())
	switch {
	case kindNotServed(err):
		// No such kind: the Flux CRDs are absent, which is "Flux is not
		// installed" arriving from the API server instead of from the profile.
		// It is checked first because a RESTMapper failure is not an apierror
		// at all and would otherwise fall through to the default.
		return newDeliveryError(v1alpha1.ReasonFluxNotInstalled, doing, err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return newDeliveryError(v1alpha1.ReasonFluxApplyForbidden, doing, err)
	case apierrors.IsConflict(err):
		return newDeliveryError(v1alpha1.ReasonFieldManagerConflict, doing, err)
	default:
		return newDeliveryError(v1alpha1.ReasonFluxApplyForbidden, doing, err)
	}
}

// kindNotServed reports that the API server does not serve this kind.
//
// It is matched two ways because it arrives two ways. controller-runtime
// resolves a GVK through a RESTMapper before it ever issues a request, and a
// group the cluster does not serve fails there — as a *meta.NoKindMatchError,
// which is not a Kubernetes API status and so is invisible to every
// apierrors.Is* helper. A discovery cache that is merely stale gets as far as
// the request and comes back as a 404. Both mean the same thing to an operator:
// install Flux.
func kindNotServed(err error) bool {
	if err == nil {
		return false
	}
	var noMatch *apimeta.NoKindMatchError
	if errors.As(err, &noMatch) {
		return true
	}
	return apierrors.IsNotFound(err) && strings.Contains(err.Error(), "server could not find the requested resource")
}

// teardown removes the pair, Kustomization first.
//
// The order is the whole decision, and it is why this is not a loop over two
// names. Deleting the Kustomization is what removes the workloads: it prunes
// its inventory on the way out, which is the behaviour `prune: true` bought.
// Deleting the OCIRepository first would leave the Kustomization pointing at a
// source that no longer exists, so it would stop reconciling with an error and
// prune nothing — and the workloads would outlive the Environment that declared
// them, with nothing left in the cluster that says whose they were.
//
// A missing object is a success. Teardown runs on every reconcile of a deleting
// Environment until the finalizer clears, so it has to be idempotent, and
// "already gone" is the state it is trying to reach.
func (d *FluxDeliverer) teardown(ctx context.Context, project, environment string) error {
	name := ObjectName(project, environment)
	for _, gvk := range []schema.GroupVersionKind{kustomizationGVK, ociRepositoryGVK} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetName(name)
		obj.SetNamespace(d.namespace())
		if err := d.Client.Delete(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) || kindNotServed(err) {
				continue
			}
			return classifyApply(err, obj)
		}
	}
	return nil
}
