package install

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/dafrie/kelson/internal/delivery"
)

// Tier is a removal stage. The zero value is TierCustomResource, which is
// deliberate: an unrecognised kind is most likely a custom resource of a CRD
// this component installed, and those must go before the CRD that serves them.
type Tier int

const (
	// TierCustomResource is the component's own custom resources — the
	// FluxInstance kelson authored, and anything else in a group the component
	// brought. They go first so the operator that owns them is still running to
	// finalize them.
	TierCustomResource Tier = iota
	// TierWorkload is what runs: the controller Deployments and their pods.
	// After the CRs so a finalizer has something to talk to, before the RBAC
	// they depend on.
	TierWorkload
	// TierConfig is everything else labelled: ServiceAccounts, Services,
	// ConfigMaps, Secrets, RBAC and webhook configurations.
	TierConfig
	// TierCRD is the CustomResourceDefinitions. Last but one, because deleting
	// a CRD deletes every custom resource in the cluster that uses it —
	// including resources kelson never created and never labelled.
	TierCRD
	// TierNamespace is the component's namespace, and only when kelson created
	// it.
	TierNamespace
)

// String names the tier as the preview prints it.
func (t Tier) String() string {
	switch t {
	case TierCustomResource:
		return "Custom resources"
	case TierWorkload:
		return "Workloads"
	case TierConfig:
		return "Configuration"
	case TierCRD:
		return "Custom resource definitions"
	case TierNamespace:
		return "Namespace"
	default:
		return "Other"
	}
}

// Tiers is the removal order, which is also the order the preview reads in.
var Tiers = []Tier{TierCustomResource, TierWorkload, TierConfig, TierCRD, TierNamespace}

// RemovalTarget is one object a removal will delete.
type RemovalTarget struct {
	Ref  Ref
	GVR  schema.GroupVersionResource
	Tier Tier
	// UID is what the delete's precondition is checked against.
	UID types.UID
	// Collateral names what goes when this object goes, without kelson deleting
	// it. Deleting a CustomResourceDefinition takes every custom resource of
	// that kind in the cluster with it, and those are named because a count of
	// CRDs is not what a user needs to read before answering the prompt.
	Collateral []string
}

// Bystander is an object carrying the component's label that the removal will
// NOT delete, and why. It is reported rather than silently skipped: "kelson
// removed the component" and "kelson removed the parts of the component it
// created" are different claims, and only the second one is ever true.
type Bystander struct {
	Ref    Ref
	Reason string
}

// Removal is what a component uninstall would do.
type Removal struct {
	Component Component
	// Targets are the objects to delete, in removal order.
	Targets []RemovalTarget
	// Kept are the labelled objects kelson will not delete.
	Kept []Bystander
	// Unreadable names the kinds and groups the sweep could not read.
	Unreadable []string
}

// Empty reports whether there is nothing to remove.
func (r *Removal) Empty() bool { return len(r.Targets) == 0 }

// Tier returns the removal's targets for one tier, in removal order.
func (r *Removal) Tier(t Tier) []RemovalTarget {
	var out []RemovalTarget
	for _, target := range r.Targets {
		if target.Tier == t {
			out = append(out, target)
		}
	}
	return out
}

// Selector is the label query that finds a component's objects. It is exported
// so an operator can run exactly the query kelson runs, with kubectl, before or
// after.
func Selector(name string) string { return LabelComponent + "=" + name }

// RemoverOptions configures a Remover.
type RemoverOptions struct {
	// Client lists, reads and deletes.
	Client dynamic.Interface
	// Catalog enumerates the kinds to sweep.
	Catalog Catalog
}

// Remover plans and performs component uninstalls.
type Remover struct {
	client  dynamic.Interface
	catalog Catalog
}

// NewRemover returns a Remover.
func NewRemover(opts RemoverOptions) (*Remover, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("install: a dynamic client is required")
	case opts.Catalog == nil:
		return nil, errors.New("install: a catalog is required to know what kinds to sweep")
	}
	return &Remover{client: opts.Client, catalog: opts.Catalog}, nil
}

// Plan enumerates what removing a component would delete.
//
// It reads the cluster and never the pins table's manifest: a removal must work
// without network access, and more importantly the question is what is live and
// labelled, not what upstream's manifest said at the version kelson happens to
// pin today. Everything in the plan is a live object carrying
// kelson.dev/installed-component=<name>, split into what kelson created and may
// delete, and what kelson adopted and may not.
func (r *Remover) Plan(ctx context.Context, name string) (*Removal, error) {
	component, ok := Lookup(name)
	if !ok {
		return nil, delivery.ApplyFailed("(request)", "component",
			fmt.Sprintf("%q is not a component kelson knows about", name),
			"kelson tracks: "+strings.Join(Names(), ", ")+". A component kelson did not install is not "+
				"kelson's to remove (ADR-0005)")
	}
	removal := &Removal{Component: component}

	resources, gaps, err := r.catalog.Resources(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range gaps {
		removal.Unreadable = append(removal.Unreadable,
			"API group "+g+" could not be read, so anything of kelson's in it was not found")
	}

	selector := Selector(name)
	for _, res := range resources {
		// A namespaced resource listed through the cluster-scoped client lists
		// every namespace, which is what a component sweep wants: kelson knows
		// where it put things, but an upstream manifest may place objects in
		// kube-system or in a namespace a later release moved.
		list, err := r.client.Resource(res.GVR).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			if unreadable, why := readFailure(err); unreadable {
				removal.Unreadable = append(removal.Unreadable,
					fmt.Sprintf("%s could not be listed (%s), so anything of kelson's of that kind was not found",
						res.GVR.Resource, why))
				continue
			}
			return nil, delivery.ApplyFailed(res.GVR.String(), "",
				"listing what to uninstall failed: "+err.Error(),
				"check RBAC: removing a platform component needs list, get and delete on every kind its install "+
					"manifest contains, including cluster-scoped RBAC, CRDs and webhook configurations")
		}
		for idx := range list.Items {
			r.classify(ctx, removal, res, &list.Items[idx])
		}
	}

	sortTargets(removal.Targets)
	sort.Slice(removal.Kept, func(i, j int) bool { return removal.Kept[i].Ref.String() < removal.Kept[j].Ref.String() })
	return removal, nil
}

// classify decides whether one labelled object is kelson's to delete.
//
// The label alone is never enough. It says "kelson applied this", and kelson
// applies over objects it did not create — that is what adoption is. The
// annotation is the licence, and only one value grants it.
func (r *Remover) classify(ctx context.Context, removal *Removal, res APIResource, obj *unstructured.Unstructured) {
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" {
		gvk = res.GVR.GroupVersion().WithKind(res.Kind)
	}
	ref := Ref{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       obj.GetName(),
		Namespace:  obj.GetNamespace(),
	}
	if obj.GetLabels()[LabelComponent] != removal.Component.Name {
		// The selector already answered this; asking the object's own labels
		// again is what keeps a mis-built selector from being the only thing
		// between kelson and somebody else's resource.
		return
	}
	switch ownership := obj.GetAnnotations()[AnnOwnership]; ownership {
	case OwnershipCreated:
	case OwnershipAdopted:
		removal.Kept = append(removal.Kept, Bystander{Ref: ref,
			Reason: "it was already in the cluster when kelson installed " + removal.Component.Name +
				", so kelson adopted it rather than creating it"})
		return
	default:
		removal.Kept = append(removal.Kept, Bystander{Ref: ref,
			Reason: "it carries no " + AnnOwnership + " annotation, so kelson cannot tell whether it created it"})
		return
	}

	target := RemovalTarget{Ref: ref, GVR: res.GVR, Tier: tierOf(gvk), UID: obj.GetUID()}
	if target.Tier == TierCRD {
		target.Collateral = r.customResources(ctx, obj)
	}
	removal.Targets = append(removal.Targets, target)
}

// customResources names the live custom resources a CRD deletion would take
// with it.
//
// This is the loud part of a component uninstall and the reason it has a data
// section at all. Deleting CloudNativePG's CRDs deletes every `Cluster` in the
// cluster — every managed Postgres database, including ones kelson never
// rendered — and a preview that said "CustomResourceDefinition/clusters.postgresql.cnpg.io"
// without saying that would be technically complete and practically a trap.
//
// A read failure here is silence rather than an error, for the reason
// uninstall.cnpgVolumes gives: the preview is better with the names and still
// correct without them.
func (r *Remover) customResources(ctx context.Context, crd *unstructured.Unstructured) []string {
	group, _, err := unstructured.NestedString(crd.Object, "spec", "group")
	if err != nil || group == "" {
		return nil
	}
	plural, _, err := unstructured.NestedString(crd.Object, "spec", "names", "plural")
	if err != nil || plural == "" {
		return nil
	}
	kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	if kind == "" {
		kind = plural
	}
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil {
		return nil
	}
	var served string
	for _, raw := range versions {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := v["name"].(string)
		if v["storage"] == true && name != "" {
			served = name
			break
		}
		if served == "" && name != "" {
			served = name
		}
	}
	if served == "" {
		return nil
	}
	list, err := r.client.Resource(schema.GroupVersionResource{Group: group, Version: served, Resource: plural}).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list.Items))
	for idx := range list.Items {
		item := &list.Items[idx]
		ref := Ref{Kind: kind, Name: item.GetName(), Namespace: item.GetNamespace()}
		out = append(out, ref.String())
	}
	sort.Strings(out)
	return out
}

// tierOf places a kind in the removal order.
func tierOf(gvk schema.GroupVersionKind) Tier {
	switch {
	case gvk.Group == "" && gvk.Kind == "Namespace":
		return TierNamespace
	case gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition":
		return TierCRD
	}
	switch gvk.Kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob", "Pod":
		return TierWorkload
	}
	// Anything in a group Kubernetes itself ships is configuration; anything
	// else is a custom resource of a CRD this component installed, and goes
	// first so its controller can still finalize it.
	switch gvk.Group {
	case "", "apps", "batch", "rbac.authorization.k8s.io", "admissionregistration.k8s.io",
		"policy", "coordination.k8s.io", "networking.k8s.io", "autoscaling", "scheduling.k8s.io",
		"apiregistration.k8s.io", "storage.k8s.io", "flowcontrol.apiserver.k8s.io":
		return TierConfig
	}
	return TierCustomResource
}

// sortTargets puts the removal in deletion order and makes it deterministic, so
// two previews of the same cluster read identically.
func sortTargets(targets []RemovalTarget) {
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

// RemovalOutcome is what happened to one removal target.
type RemovalOutcome string

const (
	// RemovalDeleted: kelson deleted it.
	RemovalDeleted RemovalOutcome = "deleted"
	// RemovalGone: it was already absent. A removal run twice reports this for
	// everything and succeeds.
	RemovalGone RemovalOutcome = "gone"
	// RemovalLeft: kelson refused. Its live labels or ownership annotation no
	// longer say it is kelson's, or it was replaced between plan and delete.
	RemovalLeft RemovalOutcome = "left"
	// RemovalFailed: the API server refused the delete.
	RemovalFailed RemovalOutcome = "failed"
)

// RemovalResult is one target's outcome.
type RemovalResult struct {
	Ref     Ref
	Outcome RemovalOutcome
	Detail  string
}

// RemovalReport is what a removal did.
type RemovalReport struct {
	Results []RemovalResult
	Deleted int
	Gone    int
	Left    int
	Failed  int
}

// Execute deletes the removal's targets in order.
//
// Every target is re-read immediately before its delete and re-checked against
// its LIVE label and ownership annotation, and every delete carries a UID
// precondition. That is the same three-part refusal internal/delivery/uninstall
// makes, for the same reason: a plan is a snapshot and the cluster is not, and
// the acceptance criterion of issue #60 is a statement about what is NOT
// deleted.
func (r *Remover) Execute(ctx context.Context, removal *Removal) (*RemovalReport, error) {
	if removal == nil {
		return nil, delivery.ApplyFailed("(plan)", "",
			"no removal plan to execute",
			"call Plan() first; kelson never deletes anything it did not preview")
	}
	report := &RemovalReport{}
	var failures []string

	for _, t := range removal.Targets {
		res := r.deleteTarget(ctx, removal.Component, t)
		report.Results = append(report.Results, res)
		switch res.Outcome {
		case RemovalDeleted:
			report.Deleted++
		case RemovalGone:
			report.Gone++
		case RemovalLeft:
			report.Left++
		case RemovalFailed:
			report.Failed++
			failures = append(failures, t.Ref.String()+": "+res.Detail)
		}
	}

	if len(failures) > 0 {
		return report, delivery.ApplyFailed(removal.Component.Name, "",
			"the API server refused to delete: "+strings.Join(failures, "; "),
			"check RBAC for these kinds, remove them by hand, or re-run — the removal is idempotent, and what "+
				"it already deleted reports as gone the second time")
	}
	return report, nil
}

func (r *Remover) deleteTarget(ctx context.Context, component Component, t RemovalTarget) RemovalResult {
	client := r.client.Resource(t.GVR).Namespace(t.Ref.Namespace)

	live, err := client.Get(ctx, t.Ref.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return RemovalResult{Ref: t.Ref, Outcome: RemovalGone, Detail: "already absent"}
	case err != nil:
		return RemovalResult{Ref: t.Ref, Outcome: RemovalFailed,
			Detail: "could not be read before deleting: " + err.Error()}
	}

	if live.GetLabels()[LabelComponent] != component.Name {
		return RemovalResult{Ref: t.Ref, Outcome: RemovalLeft,
			Detail: "it no longer carries " + Selector(component.Name) + ", so kelson does not claim it"}
	}
	if live.GetAnnotations()[AnnOwnership] != OwnershipCreated {
		return RemovalResult{Ref: t.Ref, Outcome: RemovalLeft,
			Detail: "it no longer records that kelson created it"}
	}

	opts := metav1.DeleteOptions{}
	if uid := live.GetUID(); uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	switch err := client.Delete(ctx, t.Ref.Name, opts); {
	case err == nil:
		return RemovalResult{Ref: t.Ref, Outcome: RemovalDeleted}
	case apierrors.IsNotFound(err):
		return RemovalResult{Ref: t.Ref, Outcome: RemovalGone, Detail: "deleted by something else first"}
	case apierrors.IsConflict(err):
		return RemovalResult{Ref: t.Ref, Outcome: RemovalLeft,
			Detail: "it was replaced between the preview and the delete, so this is a different object now"}
	default:
		return RemovalResult{Ref: t.Ref, Outcome: RemovalFailed, Detail: err.Error()}
	}
}

// readFailure classifies a list error as "this kind is unreadable, keep going"
// or as a real failure, exactly as uninstall.readFailure does.
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
