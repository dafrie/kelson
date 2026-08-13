package dryrun

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
)

// FieldManager is kelson's field-manager identity on every server-side apply,
// shared with the direct adapter so both exercise the API server under one
// consistent identity.
const FieldManager = "kelson"

// Mapper resolves a GroupKind to the resource and scope needed for a dynamic
// client call, matching the direct adapter's contract.
type Mapper interface {
	RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error)
}

// Options configures the L2 preview engine.
type Options struct {
	// Client is the dynamic client used for dry-run applies and live readback.
	Client dynamic.Interface
	// Mapper resolves kinds to resources.
	Mapper Mapper
	// FieldManager overrides the field-manager identity. Defaults to
	// FieldManager.
	FieldManager string
	// ClusterProfile is the target cluster's detected capabilities. Preview
	// uses it to decide whether an unexplained rejection is plausibly a policy
	// finding at all (issue #45): a cluster running no policy engine must not
	// have its failures dressed up as one.
	ClusterProfile clusterprofile.ClusterProfile
}

// DryRun produces L2 server-side dry-run previews (issue #43). It consumes an
// already-rendered delivery.ManifestSet and never re-renders or mutates it.
type DryRun struct {
	client  dynamic.Interface
	mapper  Mapper
	manager string
	profile clusterprofile.ClusterProfile
}

// New validates options and returns an L2 engine.
func New(opts Options) (*DryRun, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("dryrun: a dynamic client is required")
	case opts.Mapper == nil:
		return nil, errors.New("dryrun: a REST mapper is required")
	}
	manager := opts.FieldManager
	if manager == "" {
		manager = FieldManager
	}
	return &DryRun{client: opts.Client, mapper: opts.Mapper, manager: manager, profile: opts.ClusterProfile}, nil
}

// ResourceRef identifies one Kubernetes resource.
type ResourceRef struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

func (r ResourceRef) String() string {
	if r.Namespace != "" {
		return fmt.Sprintf("%s/%s", r.Kind, r.Name)
	}
	return r.Kind + "/" + r.Name
}

// Preview runs the L2 server-side dry-run for a rendered set and returns the
// API server's verdict as a diff.Diff (issue #43).
//
// It submits every resource in the renderer's order with DryRun:"All" under
// kelson's field manager, diffs each returned object against live state, and
// parses rejections into diff.PolicyViolation entries (issue #45). A dry-run
// apply runs the cluster's ValidatingAdmissionPolicies and validating webhooks,
// so a denial is a real admission verdict and is reported as a blocker naming
// the policy or webhook — never as a generic apply error, and never left to
// surface at deploy time after a preview that claimed to be clean.
//
// What the dry-run cannot see is reported too, as diff.Unvalidated: a webhook
// that refuses dry-run requests, and (from the cluster's webhook
// configurations) one the request would not reach at all. If the user lacks
// dry-run permission (a 403/401 that is not a policy rejection) it degrades to
// a rendered (L1) diff with Degraded set rather than failing the whole preview
// — the L2 failure must never toast the preview.
func (d *DryRun) Preview(ctx context.Context, set delivery.ManifestSet) (*diff.Diff, error) {
	targets, err := d.targets(set)
	if err != nil {
		return nil, err
	}

	out := &diff.Diff{
		Level:       diff.LevelServer,
		Project:     set.Project,
		Environment: set.Environment,
	}
	batch := batchInfo{namespaces: namespacesOf(targets), crds: crdsOf(targets)}

	// Audit-mode findings (issue #45) are read from policy reports, not from
	// request rejections — an audit-mode policy does not veto the request, so
	// the dry-run succeeds and the violation is reported as a warning.
	audit := d.readAuditReports(ctx)

	for _, t := range targets {
		rd, found, err := d.evaluate(ctx, t, batch)
		if err != nil {
			// A permission problem, not a policy or validation rejection:
			// degrade to L1 instead of failing the preview.
			if isPermission(err) {
				return d.degraded(ctx, out.Level, set, err), nil
			}
			return nil, err
		}
		if rd != nil {
			out.Resources = append(out.Resources, *rd)
		}
		out.Violations = append(out.Violations, found.violations...)
		out.Unvalidated = append(out.Unvalidated, found.unvalidated...)
	}

	out.Violations = append(out.Violations, audit...)
	// What the dry-run could not see. A validating webhook the request never
	// reaches has not approved anything, and a preview that stayed quiet about
	// it would be claiming a coverage it does not have (issue #45).
	out.Unvalidated = append(out.Unvalidated, d.coverageGaps(ctx, targets)...)
	out.Summary = summarize(out)
	// L2 is the one preview level that reads live objects back, so it is the one
	// that can pull a Secret's stored content into a diff even for a spec that
	// never held a value (issue #117). The L1 engine redacts inside diff.Between;
	// this engine builds its Diff itself and so must say so itself.
	return diff.Redact(out), nil
}

// findings is what one resource's evaluation contributes to the preview
// besides its diff. Policy findings and unevaluated resources travel
// separately because they answer different questions: "this was rejected" and
// "this was never checked" (#43).
type findings struct {
	violations  []diff.PolicyViolation
	unvalidated []diff.Unvalidated
}

// evaluate dry-runs one resource and returns its resource diff and any
// findings. It returns a non-nil error only for unexpected failures and for
// permission problems (which Preview interprets as the L1-degradation trigger).
func (d *DryRun) evaluate(ctx context.Context, t target, batch batchInfo) (*diff.ResourceDiff, findings, error) {
	live, getErr := d.get(ctx, t)
	liveMap := map[string]any{}
	if getErr == nil && live != nil {
		liveMap = live.Object
	}

	returned, err := d.apply(ctx, t)
	if err != nil {
		return d.rejection(t, liveMap, batch, err)
	}

	op := diff.OpModified
	if live == nil || apierrors.IsNotFound(getErr) {
		op = diff.OpAdded
	}
	fields := diffLiveAndReturned(liveMap, returned.Object, t.obj.Object)
	rd := &diff.ResourceDiff{
		APIVersion: t.ref.APIVersion,
		Kind:       t.ref.Kind,
		Name:       t.ref.Name,
		Namespace:  t.ref.Namespace,
		Op:         op,
		Fields:     fields,
	}
	rd.Risk = resourceRisk(op, fields, t.ref.Kind)
	return rd, findings{}, nil
}

// rejection maps a failed dry-run apply onto the diff taxonomy.
func (d *DryRun) rejection(t target, liveMap map[string]any, batch batchInfo, err error) (*diff.ResourceDiff, findings, error) {
	var status apierrors.APIStatus
	if errors.As(err, &status) && apierrors.IsNotFound(err) {
		// A prerequisite that does not exist yet — either it is being created
		// in this same batch (ordering, report as such, not a policy failure) or
		// it is genuinely missing (an error the apply would also hit).
		if batch.prerequisitePending(t, err) {
			// Benign: applying the batch in the renderer's order creates the
			// prerequisite first, so this resource is expected to validate.
			// Reported, never swallowed, and never as a policy finding (#43).
			target := diff.ResourceDiff{
				APIVersion: t.ref.APIVersion,
				Kind:       t.ref.Kind,
				Name:       t.ref.Name,
				Namespace:  t.ref.Namespace,
				Op:         diff.OpAdded,
				Risk:       diff.RiskAdditive,
			}
			u := diff.Unvalidated{
				Resource: t.ref.String(),
				Requires: batch.requirementOf(t, err),
				InBatch:  true,
				Reason:   diff.ReasonMissingPrerequisite,
				Message:  errorMessage(err),
			}
			return &target, findings{unvalidated: []diff.Unvalidated{u}}, nil
		}
		// Genuinely missing prerequisite — the apply would fail too. The
		// resource still carries a disruptive ResourceDiff, so a CI gate
		// branching on MaxRisk trips on it.
		rd := disrupting(t, liveMap)
		u := diff.Unvalidated{
			Resource: t.ref.String(),
			Requires: batch.requirementOf(t, err),
			InBatch:  false,
			Reason:   diff.ReasonMissingPrerequisite,
			Message:  errorMessage(err),
		}
		return rd, findings{unvalidated: []diff.Unvalidated{u}}, nil
	}

	c := classify(t.ref, err)
	if c.permission {
		// RBAC: the caller lacks permission to dry-run. Surfaces as
		// degradation to L1 at the Preview level.
		return nil, findings{}, err
	}
	if c.dryRunUnsupported {
		// A webhook the dry-run request cannot reach. Nothing rejected this
		// resource, so it is neither a violation nor disruptive: report the
		// rendered-vs-live change we can still compute honestly, and say that
		// the admission verdict is missing (#45).
		return l1Resource(t, liveMap), findings{unvalidated: []diff.Unvalidated{{
			Resource: t.ref.String(),
			Requires: "admission webhook " + c.webhook,
			InBatch:  false,
			Reason:   diff.ReasonDryRunUnsupported,
			Message:  errorMessage(err),
		}}}, nil
	}
	if c.policyRejected {
		rd := disrupting(t, liveMap)
		return rd, findings{violations: c.violations}, nil
	}

	// Not a recognised permission/policy/validation shape. We cannot attribute
	// the rejection, so we must not dress it up as a particular policy — an
	// invented name like "dryrun-rejected" would let an agent branch on a
	// policy that does not exist (#45). Report it honestly as not-evaluated and
	// let the cluster's detected engines shape how unlikely a policy rejection
	// is: none present means this is something else entirely.
	rd := disrupting(t, liveMap)
	return rd, findings{unvalidated: []diff.Unvalidated{{
		Resource: t.ref.String(),
		InBatch:  false,
		Reason:   diff.ReasonUnattributedRejection,
		Message:  unattributedMessage(d.profile, errorMessage(err)),
	}}}, nil
}

// unattributedMessage explains an unrecognised rejection without pretending to
// know which policy fired. HasPolicyEngine() decides whether a policy rejection
// is even a plausible culprit, so a cluster running no policy engine is pointed
// away from policy rather than toward it (issue #45).
func unattributedMessage(p clusterprofile.ClusterProfile, msg string) string {
	prefix := "dry-run rejected the resource for a reason preview could not attribute"
	if p.HasPolicyEngine() {
		return prefix + "; the cluster runs admission-policy engines, so this may be a policy rejection in an unrecognised shape: " + msg
	}
	return prefix + "; the cluster runs no policy engine, so this is not a policy finding: " + msg
}

// l1Resource builds the rendered-vs-live (L1) diff for one resource, used
// wherever the API server's own verdict is unavailable: the caller lacks
// dry-run permission, or a webhook the request cannot reach leaves the resource
// unevaluated. The change is still reported with its real risk — what is
// missing is the admission verdict, not the diff.
func l1Resource(t target, liveMap map[string]any) *diff.ResourceDiff {
	op := diff.OpAdded
	if len(liveMap) > 0 {
		op = diff.OpModified
	}
	fields := diffSetAndLive(t.obj.Object, liveMap)
	rd := &diff.ResourceDiff{
		APIVersion: t.ref.APIVersion,
		Kind:       t.ref.Kind,
		Name:       t.ref.Name,
		Namespace:  t.ref.Namespace,
		Op:         op,
		Fields:     fields,
	}
	rd.Risk = resourceRisk(op, fields, t.ref.Kind)
	return rd
}

// disrupting builds a resource diff for a resource the API server rejected: the
// write would not persist, so its risk is disruptive (issue #43 acceptance: an
// immutable-field change is caught before any write is attempted).
func disrupting(t target, liveMap map[string]any) *diff.ResourceDiff {
	op := diff.OpModified
	if len(liveMap) == 0 {
		op = diff.OpAdded
	}
	return &diff.ResourceDiff{
		APIVersion: t.ref.APIVersion,
		Kind:       t.ref.Kind,
		Name:       t.ref.Name,
		Namespace:  t.ref.Namespace,
		Op:         op,
		Risk:       diff.RiskDisruptive,
	}
}

// isPermission reports whether an error is the RBAC authorization class (not a
// policy rejection) that should trigger L1 degradation.
func isPermission(err error) bool {
	c := classify(ResourceRef{}, err)
	if c.policyRejected {
		return false
	}
	var status apierrors.APIStatus
	return errors.As(err, &status) && (apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err))
}

// degraded builds the L1 (rendered) fallback preview when L2 is unavailable
// because the caller lacks dry-run permission (issue #43). It never fails —
// the whole point is that a missing permission shows the user an L1 preview
// with a clear reason, not an error.
func (d *DryRun) degraded(ctx context.Context, _ diff.Level, set delivery.ManifestSet, cause error) *diff.Diff {
	out := &diff.Diff{
		Level:          diff.LevelRendered,
		Project:        set.Project,
		Environment:    set.Environment,
		Degraded:       true,
		DegradedReason: permissionReason(cause),
	}
	targets, _ := d.targets(set)
	for _, t := range targets {
		live, err := d.get(ctx, t)
		liveMap := map[string]any{}
		if err == nil && live != nil {
			liveMap = live.Object
		}
		out.Resources = append(out.Resources, *l1Resource(t, liveMap))
	}
	out.Summary = summarize(out)
	return diff.Redact(out)
}

// permissionReason turns an RBAC rejection into a human-readable reason naming
// the missing permission.
func permissionReason(err error) string {
	if err == nil {
		return "L2 server-side dry-run unavailable"
	}
	return fmt.Sprintf("L2 server-side dry-run unavailable: %s", errorMessage(err))
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) || apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		var status apierrors.APIStatus
		if errors.As(err, &status) && status.Status().Message != "" {
			return status.Status().Message
		}
	}
	return err.Error()
}

// --- readback ---------------------------------------------------------------

func (d *DryRun) get(ctx context.Context, t target) (*unstructured.Unstructured, error) {
	if t.mapping.Scope.Name() == meta.RESTScopeNameRoot {
		return d.client.Resource(t.mapping.Resource).Get(ctx, t.obj.GetName(), metav1.GetOptions{})
	}
	return d.client.Resource(t.mapping.Resource).Namespace(t.obj.GetNamespace()).Get(ctx, t.obj.GetName(), metav1.GetOptions{})
}

func (d *DryRun) apply(ctx context.Context, t target) (*unstructured.Unstructured, error) {
	var ri dynamic.ResourceInterface = d.client.Resource(t.mapping.Resource)
	if t.mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = d.client.Resource(t.mapping.Resource).Namespace(t.obj.GetNamespace())
	}
	return ri.Apply(ctx, t.obj.GetName(), t.obj, metav1.ApplyOptions{
		FieldManager: d.manager,
		DryRun:       []string{metav1.DryRunAll},
		Force:        false,
	})
}

// --- decoding and addressability -------------------------------------------

// target is one decoded manifest plus everything needed to address it.
type target struct {
	ref     ResourceRef
	obj     *unstructured.Unstructured
	mapping *meta.RESTMapping
}

func (d *DryRun) targets(set delivery.ManifestSet) ([]target, error) {
	out := make([]target, 0, len(set.Manifests))
	for _, m := range set.Manifests {
		obj, err := decode(m.YAML)
		if err != nil {
			return nil, fmt.Errorf("dryrun: decode %s: %w", m.Name, err)
		}
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" || obj.GetName() == "" {
			return nil, fmt.Errorf("dryrun: rendered document is missing kind or metadata.name")
		}
		ref := ResourceRef{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: obj.GetName(), Namespace: obj.GetNamespace()}
		mapping, err := d.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("dryrun: no API resource for %s: %w; install the CRD before the resources that use it", gvk, err)
		}
		if mapping.Scope.Name() == meta.RESTScopeNameRoot {
			ref.Namespace = ""
			obj.SetNamespace("")
		}
		out = append(out, target{ref: ref, obj: obj, mapping: mapping})
	}
	return out, nil
}

// decode turns one rendered YAML document into an unstructured object.
func decode(doc []byte) (*unstructured.Unstructured, error) {
	json, err := sigsyaml.YAMLToJSON(doc)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(json); err != nil {
		return nil, err
	}
	return obj, nil
}
