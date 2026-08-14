package uninstall

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
)

// LabelManagedSecret marks a Secret written by `kelson secret set` (issue
// #116), mirroring internal/secret.LabelManaged. It is mirrored rather than
// imported for the same reason the provenance labels used to be: this plane
// must not depend on the secret store to recognise the store's output.
//
// A managed Secret is included in an uninstall by default and gets its own line
// in the preview. It is kelson's — it carries the same provenance labels as
// everything else — and leaving credentials behind for a deployment that no
// longer exists is not a kindness. It is called out separately because it is
// the one deletion nothing else can reproduce: kelson does not store secret
// values (ADR-0009), so the cluster was the only copy.
const LabelManagedSecret = "kelson.dev/managed-secret"

// labelCNPGCluster is how CloudNativePG marks the PersistentVolumeClaims it
// creates for a Cluster. kelson never deletes them — the operator's own garbage
// collection does that when the Cluster goes — but the preview names them,
// because "your database is deleted" and "these five volumes are deleted" are
// the same fact stated at two levels of concreteness and only one of them is
// checkable with kubectl.
const labelCNPGCluster = "cnpg.io/cluster"

// namespacesGVR and pvcGVR are the two resources this package addresses
// directly rather than through the Catalog.
var (
	namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	pvcGVR        = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
)

// Scope addresses what to remove.
type Scope struct {
	// Project is required.
	Project string
	// Environment scopes the uninstall to one environment. Empty is only valid
	// with AllEnvironments.
	Environment string
	// AllEnvironments removes every environment of the project.
	AllEnvironments bool
	// Namespace overrides the derived <project>-<environment>, for an
	// Environment whose spec sets spec.namespace. It never widens the sweep:
	// namespaces are found by label query first, and this is the fallback for a
	// cluster where kelson may not list namespaces.
	Namespace string
}

// Validate reports whether the scope addresses something.
func (s Scope) Validate() error {
	switch {
	case s.Project == "":
		return delivery.ApplyFailed("(scope)", "project",
			"an uninstall is addressed by project",
			"pass --project; kelson deletes by provenance label, and the project label is half of that selector")
	case s.Environment == "" && !s.AllEnvironments:
		return delivery.ApplyFailed(s.Project, "environment",
			"an uninstall is addressed by environment",
			"pass --env <name>, or --all-environments to remove every environment of this project")
	case s.Environment != "" && s.AllEnvironments:
		return delivery.ApplyFailed(s.Project, "environment",
			"--env and --all-environments both name what to remove, and they disagree",
			"pass one of them")
	}
	return nil
}

// Selector is the label query that finds this scope's resources. It is
// exported so an operator can run exactly the query kelson runs — with kubectl,
// before or after — rather than reconstructing it and getting it subtly wrong.
func (s Scope) Selector() string {
	set := labels.Set{
		delivery.LabelManagedBy: delivery.ManagedByKelson,
		delivery.LabelProject:   s.Project,
	}
	if !s.AllEnvironments {
		set[delivery.LabelEnvironment] = s.Environment
	}
	return labels.SelectorFromSet(set).String()
}

// String is how the scope is named in output.
func (s Scope) String() string {
	if s.AllEnvironments {
		return s.Project + " (every environment)"
	}
	return s.Project + "/" + s.Environment
}

// owns reports whether live labels put a resource inside this scope.
func (s Scope) owns(lbls map[string]string) bool {
	env := s.Environment
	if s.AllEnvironments {
		env = ""
	}
	return delivery.OwnedBy(lbls, s.Project, env)
}

// namespaceHint is the namespace to look in when the cluster will not let
// kelson list namespaces by label. Empty with --all-environments: the default
// derivation needs an environment name, and guessing one would sweep a
// namespace nobody named.
func (s Scope) namespaceHint() string {
	switch {
	case s.Namespace != "":
		return s.Namespace
	case s.AllEnvironments:
		return ""
	default:
		return model.DefaultNamespace(s.Project, s.Environment)
	}
}

// Ref is the identity of one resource, in the shape the preview prints it.
type Ref struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

func (r Ref) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Target is one resource the plan will delete.
type Target struct {
	Ref Ref
	GVR schema.GroupVersionResource
	// Tier is the deletion stage; Targets is already sorted by it.
	Tier Tier
	// UID is what the delete's precondition is checked against, so a resource
	// replaced between the preview and the delete is refused rather than
	// deleted on the strength of a check against the object it replaced.
	UID types.UID
	// Note is the irreversibility line for a data resource, empty otherwise.
	Note string
	// Collateral names objects that go when this one goes, without kelson
	// deleting them: a CloudNativePG Cluster's PersistentVolumeClaims are the
	// case this exists for.
	Collateral []string
	// ManagedSecret marks a Secret written by `kelson secret set`.
	ManagedSecret bool
	// Reconciled names the Git reconciler that owns this resource, empty when
	// none does. A resource Flux reconciles comes back after it is deleted, and
	// saying so before the delete is the difference between an uninstall and a
	// confusing five minutes.
	Reconciled string
}

// Namespace is one namespace the sweep covered, and the verdict on the
// Namespace object itself.
type Namespace struct {
	Name string
	// Ownership is the live value of delivery.AnnNamespaceOwnership: "created",
	// "adopted", "declared", or empty when the annotation is absent.
	Ownership string
	// Delete is true when uninstall may remove the Namespace itself: kelson
	// created it AND nothing of another deployment's is still in it.
	Delete bool
	// Reason states why it stays, when it stays.
	Reason string
}

// Plan is what an uninstall would do, computed before anything is deleted.
type Plan struct {
	Scope Scope
	// Namespaces is every namespace the sweep covered, sorted by name.
	Namespaces []Namespace
	// Targets are the resources to delete, in deletion order.
	Targets []Target
	// Kept is what --keep-data excluded. It is reported, never deleted.
	Kept []Target
	// KeepData records the option, because it also decides whether a
	// kelson-created namespace may go.
	KeepData bool
	// Unreadable names the kinds and API groups the sweep could not read, so an
	// incomplete sweep is never reported as a complete one.
	Unreadable []string
}

// Empty reports whether there is nothing to do.
func (p *Plan) Empty() bool { return len(p.Targets) == 0 }

// Tier returns the plan's targets for one tier, in deletion order.
func (p *Plan) Tier(t Tier) []Target {
	var out []Target
	for _, target := range p.Targets {
		if target.Tier == t {
			out = append(out, target)
		}
	}
	return out
}

// Reconcilers names the Git reconcilers that own something in this plan.
// Deleting what a reconciler owns is a temporary state: it puts it back.
func (p *Plan) Reconcilers() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range p.Targets {
		if t.Reconciled != "" && !seen[t.Reconciled] {
			seen[t.Reconciled] = true
			out = append(out, t.Reconciled)
		}
	}
	sort.Strings(out)
	return out
}

// Options configures an Uninstaller.
type Options struct {
	// Client lists, reads and deletes.
	Client dynamic.Interface
	// Catalog enumerates the namespaced kinds to sweep.
	Catalog Catalog
	// KeepData excludes the data tier, and with it the namespace.
	KeepData bool
}

// Uninstaller plans and performs uninstalls.
type Uninstaller struct {
	client   dynamic.Interface
	catalog  Catalog
	keepData bool
}

// New returns an Uninstaller.
func New(opts Options) (*Uninstaller, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("uninstall: a dynamic client is required")
	case opts.Catalog == nil:
		return nil, errors.New("uninstall: a catalog is required to know what kinds to sweep")
	}
	return &Uninstaller{client: opts.Client, catalog: opts.Catalog, keepData: opts.KeepData}, nil
}

// Plan enumerates what an uninstall would delete, without deleting anything.
//
// Nothing about it is a guess: every entry is a live object read back from the
// cluster through the provenance selector. That is what makes the preview a
// statement about this cluster rather than about what the renderer would
// produce today — a spec that has changed since the deploy, or been deleted
// entirely, does not change what is live.
func (u *Uninstaller) Plan(ctx context.Context, scope Scope) (*Plan, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	plan := &Plan{Scope: scope, KeepData: u.keepData}

	namespaces, err := u.namespaces(ctx, scope)
	if err != nil {
		return nil, err
	}
	plan.Namespaces = namespaces

	resources, gaps, err := u.catalog.Namespaced(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range gaps {
		plan.Unreadable = append(plan.Unreadable,
			"API group "+g+" could not be read, so anything of kelson's in it was not found")
	}

	selector := scope.Selector()
	for _, ns := range namespaces {
		for _, res := range resources {
			gvk := res.GVR.GroupVersion().WithKind(res.Kind)
			if !swept(gvk.GroupKind()) {
				continue
			}
			list, err := u.client.Resource(res.GVR).Namespace(ns.Name).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				if unreadable, why := readFailure(err); unreadable {
					plan.Unreadable = append(plan.Unreadable,
						fmt.Sprintf("%s in namespace %s could not be listed (%s), so anything of kelson's of that kind was not found",
							res.GVR.Resource, ns.Name, why))
					continue
				}
				return nil, delivery.ApplyFailed(res.GVR.String(), "",
					"listing what to uninstall failed: "+err.Error(),
					"check RBAC: an uninstall needs list, get and delete on every kind kelson applied")
			}
			for i := range list.Items {
				item := &list.Items[i]
				// The selector already answered this; asking the object's own
				// labels again is what keeps a mis-built selector from being the
				// only thing standing between kelson and somebody else's
				// resource.
				if !scope.owns(item.GetLabels()) {
					continue
				}
				plan.add(u.target(ctx, res, item))
			}
		}
	}

	if u.keepData {
		var keep, del []Target
		for _, t := range plan.Targets {
			if t.Tier == TierData {
				keep = append(keep, t)
				continue
			}
			del = append(del, t)
		}
		plan.Targets, plan.Kept = del, keep
	}

	// The namespace tier is the only deletion that reaches resources this scope
	// never selected, so its licence is checked against who is living in the
	// namespace as well as against who created it (issue #215). --keep-data has
	// already spared every namespace, so the queries would buy nothing.
	if !u.keepData {
		u.checkTenancy(ctx, scope, plan, resources, gaps)
	}

	plan.appendNamespaces()
	sortTargets(plan.Targets)
	return plan, nil
}

// checkTenancy withdraws the licence to delete a namespace that another kelson
// deployment is living in, and says whose it is.
//
// It runs at plan time so the PREVIEW is already honest: an operator who is
// told the namespace goes, and then finds it standing afterwards, has been
// misled twice. The same question is asked again immediately before the delete
// (execute.go), because a plan is a snapshot and the window between them is
// exactly when another project's first apply lands.
func (u *Uninstaller) checkTenancy(ctx context.Context, scope Scope, plan *Plan, resources []APIResource, gaps []string) {
	for i := range plan.Namespaces {
		ns := &plan.Namespaces[i]
		if !ns.Delete {
			continue
		}
		tenants := u.otherTenants(ctx, scope, ns.Name, resources, gaps)
		if tenants.clear() {
			continue
		}
		ns.Delete = false
		ns.Reason = tenants.reason()
	}
}

// add appends a target to the plan.
func (p *Plan) add(t Target) { p.Targets = append(p.Targets, t) }

// target builds one Target from a live object, including the detail that only
// matters for the preview.
func (u *Uninstaller) target(ctx context.Context, res APIResource, obj *unstructured.Unstructured) Target {
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" {
		gvk = res.GVR.GroupVersion().WithKind(res.Kind)
	}
	t := Target{
		Ref: Ref{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
		},
		GVR:           res.GVR,
		Tier:          tierOf(gvk.GroupKind()),
		UID:           obj.GetUID(),
		Note:          dataNote(gvk.GroupKind()),
		ManagedSecret: obj.GetLabels()[LabelManagedSecret] == "true",
		Reconciled:    reconciledBy(obj.GetLabels()),
	}
	if gvk.GroupKind() == gk("postgresql.cnpg.io", "Cluster") {
		t.Collateral = u.cnpgVolumes(ctx, obj)
	}
	return t
}

// cnpgVolumes names the PersistentVolumeClaims CloudNativePG created for a
// Cluster. kelson does not delete them: they carry the operator's labels, not
// kelson's, and the operator removes them itself when the Cluster goes. They
// are named because they are what "the database is deleted" means on disk.
//
// A read failure here is silence rather than an error. The preview is better
// with the volume names and still correct without them, and an uninstall must
// not be blocked by a supporting query.
func (u *Uninstaller) cnpgVolumes(ctx context.Context, cluster *unstructured.Unstructured) []string {
	list, err := u.client.Resource(pvcGVR).Namespace(cluster.GetNamespace()).List(ctx, metav1.ListOptions{
		LabelSelector: labelCNPGCluster + "=" + cluster.GetName(),
	})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, "PersistentVolumeClaim/"+list.Items[i].GetName())
	}
	sort.Strings(out)
	return out
}

// reconciledBy names the Git reconciler that owns a resource, if one does.
//
// It reads Flux's own provenance labels rather than asking the spec what
// delivery mode it declares, because the question is about the live cluster: in
// Flux mode kelson wrote a commit and the cluster applied it, so deleting the
// object here is undone at the reconciler's next pass. Saying that in the
// preview is the difference between an uninstall and a confusing five minutes.
func reconciledBy(lbls map[string]string) string {
	if name := lbls["kustomize.toolkit.fluxcd.io/name"]; name != "" {
		ns := lbls["kustomize.toolkit.fluxcd.io/namespace"]
		if ns != "" {
			return "Flux Kustomization " + ns + "/" + name
		}
		return "Flux Kustomization " + name
	}
	if name := lbls["helm.toolkit.fluxcd.io/name"]; name != "" {
		return "Flux HelmRelease " + name
	}
	return ""
}

// namespaces resolves the namespaces to sweep and the verdict on each
// Namespace object.
//
// The label query is the primary source, because it is the same handle
// everything else in this package uses and it finds every namespace of a
// project without being told their names — which is what --all-environments
// needs. The addressed namespace is a fallback for a cluster where listing
// namespaces is not permitted; a namespace that does not exist is simply not
// swept.
func (u *Uninstaller) namespaces(ctx context.Context, scope Scope) ([]Namespace, error) {
	found := map[string]Namespace{}

	list, err := u.client.Resource(namespacesGVR).List(ctx, metav1.ListOptions{LabelSelector: scope.Selector()})
	switch {
	case err == nil:
		for i := range list.Items {
			item := &list.Items[i]
			found[item.GetName()] = namespaceVerdict(item, scope)
		}
	case apierrors.IsForbidden(err):
		// Not permitted to list namespaces cluster-wide. The addressed
		// namespace below still works, and a scope that named none gets an
		// honest empty answer rather than a silent partial sweep.
	default:
		return nil, delivery.ApplyFailed("Namespace", "",
			"listing kelson's namespaces failed: "+err.Error(),
			"check RBAC: an uninstall lists namespaces by label to find what it deployed, or pass --namespace")
	}

	if hint := scope.namespaceHint(); hint != "" {
		if _, already := found[hint]; !already {
			obj, err := u.client.Resource(namespacesGVR).Get(ctx, hint, metav1.GetOptions{})
			switch {
			case err == nil:
				found[hint] = namespaceVerdict(obj, scope)
			case apierrors.IsNotFound(err):
				// Nothing to sweep there.
			case apierrors.IsForbidden(err):
				// The namespace may exist and hold kelson's resources; sweeping
				// it is still worth trying, and the Namespace itself is not
				// deletable on evidence kelson could not read.
				found[hint] = Namespace{
					Name:   hint,
					Reason: "kelson may not read the Namespace object, so it cannot tell whether it created it",
				}
			default:
				return nil, delivery.ApplyFailed("Namespace/"+hint, "",
					"reading the namespace failed: "+err.Error(),
					"check RBAC: an uninstall reads the Namespace to decide whether kelson created it")
			}
		}
	}

	out := make([]Namespace, 0, len(found))
	for _, ns := range found {
		out = append(out, ns)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// namespaceVerdict decides whether the Namespace's ORIGIN permits deleting it.
//
// Only one answer is a yes, and it is the one the delivery plane recorded at
// apply time: kelson created this namespace (delivery.AnnNamespaceOwnership).
// Every other reading — the renderer's "declared", an "adopted" namespace, a
// missing annotation, labels that do not match — leaves it standing. A
// namespace delete cascades to everything inside it, and everything inside it
// is precisely what kelson does not claim to own.
//
// That yes is half a licence, not a whole one. It is a fact about the past, and
// a namespace kelson created can have acquired another project's resources
// since; checkTenancy withdraws the licence when it has (issue #215).
func namespaceVerdict(obj *unstructured.Unstructured, scope Scope) Namespace {
	ns := Namespace{
		Name:      obj.GetName(),
		Ownership: obj.GetAnnotations()[delivery.AnnNamespaceOwnership],
	}
	switch {
	case !scope.owns(obj.GetLabels()):
		ns.Reason = "it carries no kelson provenance for " + scope.String() +
			": kelson swept it for labelled resources but does not claim the namespace"
	case ns.Ownership == delivery.NamespaceOwnershipCreated:
		ns.Delete = true
	case ns.Ownership == delivery.NamespaceOwnershipAdopted:
		ns.Reason = "kelson adopted this namespace rather than creating it, so deleting it would take " +
			"whatever else lives here with it"
	case ns.Ownership == delivery.NamespaceOwnershipDeclared:
		ns.Reason = "kelson's rendered set declares this namespace but nothing recorded that kelson created it " +
			"(the deploy predates that record, or the read failed at apply time)"
	default:
		ns.Reason = "it carries no " + delivery.AnnNamespaceOwnership + " annotation, so kelson cannot tell " +
			"whether it created it"
	}
	return ns
}

// appendNamespaces adds the deletable Namespaces as the last tier, and records
// on the rest why they stay.
//
// --keep-data is what stops a namespace kelson created from going: leaving the
// data behind and deleting the namespace it lives in would delete the data.
// That is stated on the namespace rather than left to be inferred.
func (p *Plan) appendNamespaces() {
	for i := range p.Namespaces {
		ns := &p.Namespaces[i]
		if !ns.Delete {
			continue
		}
		if p.KeepData {
			ns.Delete = false
			ns.Reason = "--keep-data keeps the data resources in it, and deleting the namespace would delete them"
			continue
		}
		p.add(Target{
			Ref:  Ref{APIVersion: "v1", Kind: "Namespace", Name: ns.Name},
			GVR:  namespacesGVR,
			Tier: TierNamespace,
			Note: "kelson created this namespace, so it goes last; anything else still in it goes with it",
		})
	}
}

// sortTargets puts the plan in deletion order and makes that order
// deterministic, so two previews of the same cluster read identically.
func sortTargets(targets []Target) {
	sort.SliceStable(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Ref.Kind != b.Ref.Kind {
			return a.Ref.Kind < b.Ref.Kind
		}
		if a.Ref.Namespace != b.Ref.Namespace {
			return a.Ref.Namespace < b.Ref.Namespace
		}
		return a.Ref.Name < b.Ref.Name
	})
}

// readFailure classifies a list error as "this kind is unreadable, keep going"
// or as a real failure.
//
// A kind that is registered but has no storage answers NotFound; one that does
// not support listing answers MethodNotAllowed; one the credentials may not see
// answers Forbidden. None of those is a reason to abandon an uninstall, and all
// of them are a reason to say the sweep was incomplete.
func readFailure(err error) (bool, string) {
	switch {
	case apierrors.IsNotFound(err):
		return true, "the kind has no storage in this cluster"
	case apierrors.IsMethodNotSupported(err):
		return true, "the kind cannot be listed"
	case apierrors.IsForbidden(err):
		return true, "not permitted"
	default:
		return false, ""
	}
}

// Describe renders the selector and the boundary in one line each, for a
// caller that wants to print what kelson is about to act on without knowing how
// the selector is built.
func (p *Plan) Describe() string {
	var b strings.Builder
	b.WriteString(p.Scope.String())
	b.WriteString(" — selector ")
	b.WriteString(p.Scope.Selector())
	return b.String()
}
