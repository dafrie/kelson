// Package direct implements the "direct" delivery adapter (issue #33): kelson
// performs the last mile itself with a server-side apply under its own field
// manager, instead of handing rendered output to Flux or Argo.
//
// Three properties make direct mode safe enough to be a default:
//
//   - Field management. Every apply is a server-side apply as field manager
//     "kelson", never forced. A field another controller owns comes back as a
//     409 and is surfaced as a structured delivery/conflict naming the
//     resource and the field — ADR-0001's "never silent last-write-wins".
//   - Provenance-scoped pruning. Only resources carrying kelson's provenance
//     labels for this (project, environment) are ever deleted. Anything else
//     is reported as delivery/not-provenanced and left untouched.
//   - Order preservation. ManifestSet.Manifests arrive apply-ordered from the
//     renderer (namespaces first, workloads, CRDs before CRs). The adapter
//     applies them in exactly that order and prunes in the reverse of it.
//
// The purity rule (ADR-0001) is unaffected: this package talks to a cluster,
// internal/renderer never does. The adapter only ever consumes an
// already-rendered ManifestSet and never re-renders or mutates it.
package direct

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/delivery"
)

// AdapterName is the delivery mode this adapter serves (model.DeliveryDirect).
const AdapterName = "direct"

// FieldManager is the field-manager identity kelson claims on every
// server-side apply. It is deliberately distinct from kubectl's
// ("kubectl-client-side-apply") and from any controller's, so that shared
// ownership of a field is reported by the API server as a conflict instead of
// being silently taken over.
const FieldManager = "kelson"

// Provenance keys, mirroring what the renderer stamps
// (docs/architecture.md, Provenance).
const (
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelProject     = "kelson.dev/project"
	labelEnvironment = "kelson.dev/environment"
	annSpecHash      = "kelson.dev/spec-hash"
	annRevision      = "kelson.dev/revision"

	managedByKelson = "kelson"
)

// Mapper resolves a GroupKind to the resource and scope needed for a dynamic
// client call. *meta.RESTMapper (e.g. a discovery-backed
// restmapper.DeferredDiscoveryRESTMapper) satisfies it; tests use a static
// meta.DefaultRESTMapper.
type Mapper interface {
	RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error)
}

// Options configures the direct adapter.
type Options struct {
	// Client is the dynamic client used for apply, readback and prune.
	Client dynamic.Interface
	// Mapper resolves kinds to resources.
	Mapper Mapper
	// History is the rendered-history store (issue #38).
	History *Store
	// FieldManager overrides the field-manager identity. Defaults to
	// FieldManager; overriding it is for tests and for running two kelson
	// instances against one cluster deliberately.
	FieldManager string
	// Now is injectable so history timestamps are deterministic in tests.
	Now func() time.Time
}

// Adapter is the direct-mode delivery.Adapter.
type Adapter struct {
	client  dynamic.Interface
	mapper  Mapper
	history *Store
	manager string
	now     func() time.Time
}

var _ delivery.Adapter = (*Adapter)(nil)

// New returns a direct adapter.
func New(opts Options) (*Adapter, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("direct: a dynamic client is required")
	case opts.Mapper == nil:
		return nil, errors.New("direct: a REST mapper is required")
	case opts.History == nil:
		return nil, errors.New("direct: a history store is required")
	}
	manager := opts.FieldManager
	if manager == "" {
		manager = FieldManager
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{client: opts.Client, mapper: opts.Mapper, history: opts.History, manager: manager, now: now}, nil
}

// RegisterDirect builds a direct adapter from opts and registers it in reg
// under the "direct" delivery mode. Adding an adapter is exactly this:
// implement delivery.Adapter, register it (issue #32).
func RegisterDirect(reg *delivery.Registry, opts Options) (*Adapter, error) {
	a, err := New(opts)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(a); err != nil {
		return nil, err
	}
	return a, nil
}

// Name implements delivery.Adapter.
func (a *Adapter) Name() string { return AdapterName }

// Capabilities implements delivery.Adapter. Direct mode owns the apply, so it
// can roll back; it has no repository, so it has neither PRs nor git.
func (a *Adapter) Capabilities() delivery.Capabilities {
	return delivery.Capabilities{
		SupportsPR:       false,
		RequiresGit:      false,
		SupportsRollback: true,
	}
}

// Apply server-side applies the rendered set in renderer order, records the
// rendered output in the history store, then prunes what this (project,
// environment) owned before and no longer declares.
func (a *Adapter) Apply(ctx context.Context, set delivery.ManifestSet) (delivery.Result, error) {
	return a.applySet(ctx, set, Record{Type: TypeDeploy, Message: "deploy " + set.SpecHash})
}

// History implements delivery.Adapter: newest first, in the same shape the Git
// modes derive from their repository.
func (a *Adapter) History(_ context.Context, set delivery.ManifestSet) ([]delivery.Entry, error) {
	return a.history.Entries(set.Project, set.Environment)
}

// Rollback re-applies the rendered output recorded for entry `to` — the exact
// bytes that were live at that revision, never a re-render — under a fresh
// revision, and records a rollback entry.
func (a *Adapter) Rollback(ctx context.Context, set delivery.ManifestSet, to delivery.Entry) (delivery.Result, error) {
	ref := set.Project + "/" + set.Environment
	if to.Revision == "" {
		return delivery.Result{}, delivery.ApplyFailed(ref, "revision",
			"rollback target has no revision",
			"pass a delivery.Entry returned by History()")
	}
	rec, err := a.history.Get(set.Project, set.Environment, to.Revision)
	if err != nil {
		return delivery.Result{}, err
	}
	if rec == nil {
		return delivery.Result{}, delivery.ApplyFailed(ref, "revision",
			fmt.Sprintf("revision %q is not in the retained history", to.Revision),
			"list History() for the retained revisions; older ones are pruned by the retention policy")
	}
	rendered, err := a.history.Rendered(set.Project, set.Environment, to.Revision)
	if err != nil {
		return delivery.Result{}, err
	}
	manifests, err := SplitDocuments(rendered)
	if err != nil {
		return delivery.Result{}, delivery.ApplyFailed(ref, "", err.Error(),
			"the recorded rendered output is unreadable; inspect the history data dir")
	}

	target := delivery.ManifestSet{
		Project:     set.Project,
		Environment: set.Environment,
		SpecHash:    rec.SpecHash,
		Manifests:   manifests,
	}
	return a.applySet(ctx, target, Record{
		Type:         TypeRollback,
		Message:      "rollback to " + to.Revision,
		RolledBackTo: to.Revision,
	})
}

// applySet is the shared apply path for deploys and rollbacks.
func (a *Adapter) applySet(ctx context.Context, set delivery.ManifestSet, rec Record) (delivery.Result, error) {
	if set.Project == "" || set.Environment == "" {
		return delivery.Result{}, delivery.ApplyFailed("(manifest set)", "",
			"manifest set is missing project or environment",
			"populate ManifestSet.Project and .Environment before applying")
	}
	revision := set.Revision
	if revision == "" {
		next, err := a.history.NextRevision(set.Project, set.Environment)
		if err != nil {
			return delivery.Result{}, err
		}
		revision = next
	}

	// Decode and validate everything before touching the cluster: a set that
	// is malformed halfway through must not be half-applied.
	targets, err := a.targets(set, revision)
	if err != nil {
		return delivery.Result{}, err
	}

	// The previous revision is the prune baseline; read it before recording
	// this one.
	previous, err := a.history.Latest(set.Project, set.Environment)
	if err != nil {
		return delivery.Result{}, err
	}

	for _, t := range targets {
		if _, err := a.resource(t).Apply(ctx, t.obj.GetName(), t.obj, metav1.ApplyOptions{
			FieldManager: a.manager,
			// Never Force: taking a field from its owner without saying so is
			// exactly the silent overwrite ADR-0001 forbids.
			Force: false,
		}); err != nil {
			return delivery.Result{}, applyError(t.ref, err)
		}
	}

	rendered, err := renderedBytes(set.Manifests)
	if err != nil {
		return delivery.Result{}, err
	}
	rec.Revision = revision
	rec.SpecHash = set.SpecHash
	rec.CommittedAt = a.now().UTC().Format(time.RFC3339)
	rec.Resources = refs(targets)
	if _, err := a.history.Append(set.Project, set.Environment, rec, rendered); err != nil {
		return delivery.Result{}, err
	}

	if err := a.prune(ctx, set, targets, previous); err != nil {
		// The apply itself succeeded and is recorded; the prune failure is
		// still surfaced loudly rather than swallowed.
		return delivery.Result{Revision: revision, Applied: true}, err
	}
	return delivery.Result{Revision: revision, Applied: true}, nil
}

// Status correlates the live cluster against the set's provenance and reports
// one phase (issue #37). Direct mode owns the apply, so there is no
// "Committed but not reconciled" state: a resource is missing, stale, live, or
// live and unhealthy.
func (a *Adapter) Status(ctx context.Context, set delivery.ManifestSet) (delivery.Status, error) {
	revision := set.Revision
	if revision == "" {
		latest, err := a.history.Latest(set.Project, set.Environment)
		if err != nil {
			return delivery.Status{}, err
		}
		if latest == nil {
			return delivery.Status{
				Phase: delivery.PhaseProposed,
				Cause: "direct: nothing has been applied for " + set.Project + "/" + set.Environment,
			}, nil
		}
		revision = latest.Revision
	}

	targets, err := a.targets(set, revision)
	if err != nil {
		return delivery.Status{}, err
	}

	status := delivery.Status{Phase: delivery.PhaseHealthy, Revision: revision}
	live, degraded, progressing := 0, 0, 0
	for _, t := range targets {
		obj, err := a.resource(t).Get(ctx, t.obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return proposed(revision, len(targets), live, fmt.Sprintf("%s is not live yet", t.ref)), nil
		}
		if err != nil {
			return delivery.Status{}, delivery.ApplyFailed(t.ref.String(), "",
				"reading live state failed: "+err.Error(),
				"check the API server connection and RBAC for this resource")
		}
		if got := obj.GetAnnotations()[annRevision]; got != revision {
			return proposed(revision, len(targets), live,
				fmt.Sprintf("%s is at revision %q, want %q", t.ref, got, revision)), nil
		}
		if want := t.obj.GetAnnotations()[annSpecHash]; want != "" {
			if got := obj.GetAnnotations()[annSpecHash]; got != want {
				return proposed(revision, len(targets), live,
					fmt.Sprintf("%s carries spec-hash %q, want %q", t.ref, got, want)), nil
			}
		}
		live++

		switch state, cause := health(obj); state {
		case healthDegraded:
			degraded++
			if status.Cause == "" {
				status.Cause = fmt.Sprintf("%s: %s", t.ref, cause)
			}
		case healthProgressing:
			progressing++
			if status.Cause == "" {
				status.Cause = fmt.Sprintf("%s: %s", t.ref, cause)
			}
		case healthOK:
		}
	}

	switch {
	case degraded > 0:
		status.Phase = delivery.PhaseDegraded
	case progressing > 0:
		status.Phase = delivery.PhaseApplied
	default:
		status.Phase = delivery.PhaseHealthy
		status.Cause = ""
	}
	status.Detail = map[string]string{
		"resources": itoa(len(targets)),
		"live":      itoa(live),
		"degraded":  itoa(degraded),
	}
	return status, nil
}

func proposed(revision string, total, live int, cause string) delivery.Status {
	return delivery.Status{
		Phase:    delivery.PhaseProposed,
		Revision: revision,
		Cause:    "direct: " + cause,
		Detail: map[string]string{
			"resources": itoa(total),
			"live":      itoa(live),
		},
	}
}

// --- targets ---------------------------------------------------------------

// target is one decoded manifest plus everything needed to address it.
type target struct {
	ref     ResourceRef
	obj     *unstructured.Unstructured
	mapping *meta.RESTMapping
}

// targets decodes the set in renderer order, validating provenance and
// stamping the direct-mode revision. The ManifestSet itself is never mutated:
// decoding produces fresh objects.
func (a *Adapter) targets(set delivery.ManifestSet, revision string) ([]target, error) {
	out := make([]target, 0, len(set.Manifests))
	for _, m := range set.Manifests {
		obj, err := decode(m.YAML)
		if err != nil {
			return nil, delivery.ApplyFailed(manifestRef(m), "", err.Error(),
				"the rendered output is not valid YAML; this is a renderer bug — report it with the spec")
		}
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" || obj.GetName() == "" {
			return nil, delivery.ApplyFailed(manifestRef(m), "",
				"rendered document is missing kind or metadata.name",
				"re-render the environment; every applied document needs an identity")
		}
		ref := ResourceRef{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
		}
		if err := checkProvenance(ref, obj.GetLabels(), set); err != nil {
			return nil, err
		}
		mapping, err := a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, delivery.ApplyFailed(ref.String(), "",
				fmt.Sprintf("no API resource for %s: %v", gvk, err),
				"install the CRD before the resources that use it, or refresh the cluster profile")
		}
		if mapping.Scope.Name() == meta.RESTScopeNameRoot {
			ref.Namespace = ""
			obj.SetNamespace("")
		}

		// The revision is a direct-mode sequence id the renderer cannot know,
		// so the adapter stamps it. It is provenance only — never spec — and
		// it is what Status() correlates on.
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[annRevision] = revision
		obj.SetAnnotations(ann)

		out = append(out, target{ref: ref, obj: obj, mapping: mapping})
	}
	return out, nil
}

// checkProvenance refuses to apply a document that does not carry this
// (project, environment)'s provenance labels. Pruning scopes deletions by
// exactly these labels, so an unlabelled apply would create a resource the
// next deploy could never clean up.
func checkProvenance(ref ResourceRef, lbls map[string]string, set delivery.ManifestSet) error {
	for key, want := range map[string]string{
		labelManagedBy:   managedByKelson,
		labelProject:     set.Project,
		labelEnvironment: set.Environment,
	} {
		if got := lbls[key]; got != want {
			return delivery.ApplyFailed(ref.String(), "metadata.labels."+key,
				fmt.Sprintf("rendered resource carries %q, want %q", got, want),
				"every applied resource must carry kelson provenance labels; re-render the environment")
		}
	}
	return nil
}

func (a *Adapter) resource(t target) dynamic.ResourceInterface {
	if t.mapping.Scope.Name() == meta.RESTScopeNameRoot {
		return a.client.Resource(t.mapping.Resource)
	}
	return a.client.Resource(t.mapping.Resource).Namespace(t.obj.GetNamespace())
}

// --- prune -----------------------------------------------------------------

// prune deletes what this (project, environment) owned and no longer declares.
//
// Candidates come from two places: the previous revision's recorded resources,
// and a live listing by provenance selector over every resource/namespace pair
// the previous and current revisions touch (which also catches leftovers from
// an apply that failed halfway). Every candidate is re-read and re-checked
// against its live labels before deletion — a resource without kelson
// provenance is reported as delivery/not-provenanced and left alone.
//
// Deletion runs in the reverse of the renderer's apply order, so dependents go
// before the namespaces that contain them.
func (a *Adapter) prune(ctx context.Context, set delivery.ManifestSet, targets []target, previous *Record) error {
	desired := map[string]bool{}
	for _, t := range targets {
		desired[t.ref.key()] = true
	}

	candidates, err := a.pruneCandidates(ctx, set, targets, previous, desired)
	if err != nil {
		return err
	}

	var kept []string
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		live, err := a.client.Resource(c.gvr).Namespace(c.ref.Namespace).Get(ctx, c.ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return delivery.ApplyFailed(c.ref.String(), "",
				"reading a prune candidate failed: "+err.Error(),
				"check the API server connection and RBAC for this resource")
		}
		if !ownedBy(live.GetLabels(), set) {
			kept = append(kept, c.ref.String())
			continue
		}
		opts := metav1.DeleteOptions{}
		if uid := live.GetUID(); uid != "" {
			opts.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		if err := a.client.Resource(c.gvr).Namespace(c.ref.Namespace).Delete(ctx, c.ref.Name, opts); err != nil && !apierrors.IsNotFound(err) {
			return delivery.ApplyFailed(c.ref.String(), "",
				"pruning failed: "+err.Error(),
				"delete the resource manually or check RBAC, then re-deploy")
		}
	}

	if len(kept) > 0 {
		return delivery.NotProvenanced(strings.Join(kept, ", "), "metadata.labels."+labelManagedBy,
			"refusing to prune resources that do not carry kelson provenance labels",
			"kelson only deletes what it created; remove these resources by hand if they are unwanted")
	}
	return nil
}

// candidate is a prune candidate: an identity plus the resource to address it
// through.
type candidate struct {
	ref ResourceRef
	gvr schema.GroupVersionResource
}

// pruneCandidates enumerates, in apply order, everything that may need to go.
func (a *Adapter) pruneCandidates(ctx context.Context, set delivery.ManifestSet, targets []target, previous *Record, desired map[string]bool) ([]candidate, error) {
	var out []candidate
	seen := map[string]bool{}

	add := func(ref ResourceRef, gvr schema.GroupVersionResource) {
		if desired[ref.key()] || seen[ref.key()] {
			return
		}
		seen[ref.key()] = true
		out = append(out, candidate{ref: ref, gvr: gvr})
	}

	// Scopes to sweep: every (resource, namespace) pair the current or the
	// previous revision touches. Both are needed — a kind that disappears
	// entirely from the spec is only known to the previous revision.
	scopes := map[schema.GroupVersionResource]map[string]bool{}
	addScope := func(gvr schema.GroupVersionResource, ns string) {
		if scopes[gvr] == nil {
			scopes[gvr] = map[string]bool{}
		}
		scopes[gvr][ns] = true
	}
	for _, t := range targets {
		addScope(t.mapping.Resource, t.ref.Namespace)
	}
	if previous != nil {
		for _, ref := range previous.Resources {
			gvr, err := a.resourceFor(ref)
			if err != nil {
				// A kind that no longer exists in the cluster cannot hold a
				// live resource to prune; that is not a delivery failure.
				continue
			}
			addScope(gvr, ref.Namespace)
			add(ref, gvr)
		}
	}

	selector := labels.SelectorFromSet(labels.Set{
		labelManagedBy:   managedByKelson,
		labelProject:     set.Project,
		labelEnvironment: set.Environment,
	}).String()
	for gvr, namespaces := range scopes {
		for ns := range namespaces {
			list, err := a.client.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				if apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) {
					continue
				}
				return nil, delivery.ApplyFailed(gvr.String(), "",
					"listing owned resources for pruning failed: "+err.Error(),
					"check RBAC: pruning needs list on every resource kelson applies")
			}
			for i := range list.Items {
				item := &list.Items[i]
				gvk := item.GroupVersionKind()
				add(ResourceRef{
					APIVersion: gvk.GroupVersion().String(),
					Kind:       gvk.Kind,
					Name:       item.GetName(),
					Namespace:  item.GetNamespace(),
				}, gvr)
			}
		}
	}
	return out, nil
}

func (a *Adapter) resourceFor(ref ResourceRef) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	mapping, err := a.mapper.RESTMapping(gv.WithKind(ref.Kind).GroupKind(), gv.Version)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	return mapping.Resource, nil
}

// ownedBy reports whether the live labels mark the resource as kelson's, for
// this project and environment.
func ownedBy(lbls map[string]string, set delivery.ManifestSet) bool {
	return lbls[labelManagedBy] == managedByKelson &&
		lbls[labelProject] == set.Project &&
		lbls[labelEnvironment] == set.Environment
}

// --- errors ----------------------------------------------------------------

// applyError maps an API server failure onto the delivery taxonomy. A 409 from
// a server-side apply is the field-ownership conflict: it names the resource
// and every contested field, and is never retried by forcing.
func applyError(ref ResourceRef, err error) error {
	if apierrors.IsConflict(err) {
		field, owners := conflictDetail(err)
		msg := "server-side apply conflict: field is owned by another field manager"
		if owners != "" {
			msg = "server-side apply conflict: field owned by " + owners
		}
		conflict := delivery.Conflict(ref.String(), field, msg,
			"stop managing the field in the spec, or take ownership deliberately after checking what else writes it")
		conflict.Cause = err.Error()
		return conflict
	}
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return withCause(delivery.ApplyFailed(ref.String(), "",
			"the API server rejected the resource: "+err.Error(),
			"fix the spec or the admission policy that rejected it, then re-deploy"), err)
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return withCause(delivery.ApplyFailed(ref.String(), "",
			"not permitted to apply this resource: "+err.Error(),
			"grant kelson's service account apply and delete on this resource"), err)
	}
	return withCause(delivery.ApplyFailed(ref.String(), "",
		"server-side apply failed: "+err.Error(),
		"retry the deploy; if it persists, check the API server and the audit trail"), err)
}

func withCause(e delivery.Error, cause error) delivery.Error {
	e.Cause = cause.Error()
	return e
}

// conflictDetail pulls the contested field paths and owning managers out of a
// 409 status, so the conflict names what actually diverged.
func conflictDetail(err error) (field, owners string) {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return "", ""
	}
	var fields, managers []string
	seen := map[string]bool{}
	for _, cause := range status.Status().Details.Causes {
		if cause.Field != "" && !seen["f:"+cause.Field] {
			seen["f:"+cause.Field] = true
			fields = append(fields, cause.Field)
		}
		if m := ownerFromCause(cause.Message); m != "" && !seen["m:"+m] {
			seen["m:"+m] = true
			managers = append(managers, m)
		}
	}
	return strings.Join(fields, ", "), strings.Join(managers, ", ")
}

// ownerFromCause extracts the field manager from an apply-conflict cause
// message, which the API server formats as: Apply failed with 1 conflict:
// conflict with "other-controller" using apps/v1: .spec.replicas
func ownerFromCause(msg string) string {
	first := strings.Index(msg, `"`)
	if first < 0 {
		return ""
	}
	rest := msg[first+1:]
	last := strings.Index(rest, `"`)
	if last < 0 {
		return ""
	}
	return rest[:last]
}

// --- health ----------------------------------------------------------------

type healthState int

const (
	healthOK healthState = iota
	healthProgressing
	healthDegraded
)

// health reads back the conditions kelson understands. It is deliberately
// narrow: Deployments and CronJobs are what the renderer emits as workloads
// (internal/renderer/workload.go), and a kind with no health signal never
// blocks Healthy — reporting "unknown" as "unhealthy" would make every deploy
// look broken.
func health(obj *unstructured.Unstructured) (healthState, string) {
	switch obj.GroupVersionKind().GroupKind() {
	case schema.GroupKind{Group: "apps", Kind: "Deployment"}:
		return deploymentHealth(obj)
	case schema.GroupKind{Group: "batch", Kind: "CronJob"}:
		return cronJobHealth(obj)
	default:
		return healthOK, ""
	}
}

func deploymentHealth(obj *unstructured.Unstructured) (healthState, string) {
	gen, _, _ := unstructured.NestedInt64(obj.Object, "metadata", "generation")
	observed, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if found && observed < gen {
		return healthProgressing, "the controller has not observed the latest generation yet"
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var available, progressing map[string]any
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch c["type"] {
		case "Available":
			available = c
		case "Progressing":
			progressing = c
		case "ReplicaFailure":
			if c["status"] == "True" {
				return healthDegraded, "replica failure: " + conditionText(c)
			}
		}
	}
	// A rollout that ran out of its progress deadline is stuck, not slow.
	if progressing != nil && progressing["status"] == "False" {
		return healthDegraded, "rollout is not progressing: " + conditionText(progressing)
	}
	if available != nil && available["status"] != "True" {
		return healthDegraded, "no available replicas: " + conditionText(available)
	}
	if available == nil {
		return healthProgressing, "no Available condition reported yet"
	}
	return healthOK, ""
}

// cronJobHealth is intentionally minimal: batch/v1 CronJobs carry no
// conditions, so "live and not suspended" is the whole health signal available
// without inspecting Jobs.
func cronJobHealth(obj *unstructured.Unstructured) (healthState, string) {
	if suspended, found, _ := unstructured.NestedBool(obj.Object, "spec", "suspend"); found && suspended {
		return healthProgressing, "the CronJob is suspended and will not schedule"
	}
	return healthOK, ""
}

func conditionText(c map[string]any) string {
	reason, _ := c["reason"].(string)
	msg, _ := c["message"].(string)
	switch {
	case reason != "" && msg != "":
		return reason + ": " + msg
	case reason != "":
		return reason
	default:
		return msg
	}
}

// --- encoding --------------------------------------------------------------

// decode turns one rendered YAML document into an unstructured object. It goes
// through JSON so integral numbers stay int64 rather than becoming floats.
func decode(doc []byte) (*unstructured.Unstructured, error) {
	jsonBytes, err := sigsyaml.YAMLToJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("decode rendered document: %w", err)
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(jsonBytes); err != nil {
		return nil, fmt.Errorf("decode rendered document: %w", err)
	}
	return obj, nil
}

// renderedBytes joins a manifest set into the multi-document stream stored as
// the revision's rendered output — the same shape `kelson render` writes, so
// history stays diffable and replayable.
func renderedBytes(manifests []delivery.Manifest) ([]byte, error) {
	var buf bytes.Buffer
	for _, m := range manifests {
		buf.WriteString("---\n")
		buf.Write(m.YAML)
		if len(m.YAML) > 0 && !bytes.HasSuffix(m.YAML, []byte("\n")) {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), nil
}

// SplitDocuments is the inverse of renderedBytes: it splits a stored rendered
// stream back into manifests, preserving document bytes and order. It is
// exported because replaying history (rollback today, `kelson eject --to-git`
// later) is the reason the store keeps the rendered output at all.
func SplitDocuments(stream []byte) ([]delivery.Manifest, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(stream)))
	var out []delivery.Manifest
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("split rendered stream: %w", err)
		}
		// The YAMLReader retains the leading document-separator line of the
		// first document (it is written to the buffer before a non-empty
		// buffer exists). renderedBytes writes "---" before every document, so
		// a lone leading separator is an artifact of the split, not content.
		doc = bytes.TrimPrefix(doc, []byte("---\n"))
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj, err := decode(doc)
		if err != nil {
			return nil, err
		}
		if obj.GetKind() == "" {
			continue
		}
		gvk := obj.GroupVersionKind()
		out = append(out, delivery.Manifest{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
			YAML:       doc,
		})
	}
}

func refs(targets []target) []ResourceRef {
	out := make([]ResourceRef, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.ref)
	}
	return out
}

func manifestRef(m delivery.Manifest) string {
	ref := ResourceRef{APIVersion: m.APIVersion, Kind: m.Kind, Name: m.Name, Namespace: m.Namespace}
	if ref.Kind == "" {
		return "(rendered document)"
	}
	return ref.String()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
