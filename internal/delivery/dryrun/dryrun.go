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
}

// DryRun produces L2 server-side dry-run previews (issue #43). It consumes an
// already-rendered delivery.ManifestSet and never re-renders or mutates it.
type DryRun struct {
	client  dynamic.Interface
	mapper  Mapper
	manager string
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
	return &DryRun{client: opts.Client, mapper: opts.Mapper, manager: manager}, nil
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
// parses rejections into diff.PolicyViolation entries (issue #45). If the user
// lacks dry-run permission (a 403/401 that is not a policy rejection) it
// degrades to a rendered (L1) diff with Degraded set rather than failing the
// whole preview — the L2 failure must never toast the preview.
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
		rd, violations, err := d.evaluate(ctx, t, batch)
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
		out.Violations = append(out.Violations, violations...)
	}

	out.Violations = append(out.Violations, audit...)
	out.Summary = summarize(out)
	return out, nil
}

// evaluate dry-runs one resource and returns its resource diff and any
// violations. It returns a non-nil error only for unexpected failures and for
// permission problems (which Preview interprets as the L1-degradation trigger).
func (d *DryRun) evaluate(ctx context.Context, t target, batch batchInfo) (*diff.ResourceDiff, []diff.PolicyViolation, error) {
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
	return rd, nil, nil
}

// rejection maps a failed dry-run apply onto the diff taxonomy.
func (d *DryRun) rejection(t target, liveMap map[string]any, batch batchInfo, err error) (*diff.ResourceDiff, []diff.PolicyViolation, error) {
	var status apierrors.APIStatus
	if errors.As(err, &status) && apierrors.IsNotFound(err) {
		// A prerequisite that does not exist yet — either it is being created
		// in this same batch (ordering, report as such, not a policy failure) or
		// it is genuinely missing (an error the apply would also hit).
		if batch.prerequisitePending(t, err) {
			target := diff.ResourceDiff{
				APIVersion: t.ref.APIVersion,
				Kind:       t.ref.Kind,
				Name:       t.ref.Name,
				Namespace:  t.ref.Namespace,
				Op:         diff.OpAdded,
				Risk:       diff.RiskAdditive,
			}
			v := diff.PolicyViolation{
				Engine:   "kubernetes",
				Policy:   "dryrun-prerequisite-pending",
				Resource: t.ref.String(),
				Message:  fmt.Sprintf("%s could not be validated: a prerequisite is being created in the same batch (%s)", t.ref.String(), errorMessage(err)),
				// This is a non-blocker: once the batch is applied in the
				// renderer's order the prerequisite exists and the resource
				// validates. Reporting it as audit keeps it from gating while
				// still surfacing the ordering explicitly (#43).
				Enforcement: diff.EnforcementAudit,
			}
			return &target, []diff.PolicyViolation{v}, nil
		}
		// Genuinely missing prerequisite — the apply would fail too.
		rd := disrupting(t, liveMap)
		return rd, []diff.PolicyViolation{
			{
				Engine:      "kubernetes",
				Policy:      "missing-prerequisite",
				Resource:    t.ref.String(),
				Message:     errorMessage(err),
				Enforcement: diff.EnforcementEnforce,
			},
		}, nil
	}

	c := classify(t.ref, err)
	if c.permission {
		// RBAC: the caller lacks permission to dry-run. Surfaces as
		// degradation to L1 at the Preview level.
		return nil, nil, err
	}
	if c.policyRejected {
		rd := disrupting(t, liveMap)
		return rd, c.violations, nil
	}

	// Not a recognised permission/policy/validation shape — surface as a
	// generic disruptive finding carrying the API's own message rather than
	// silently dropping the rejection.
	rd := disrupting(t, liveMap)
	return rd, []diff.PolicyViolation{{
		Engine:      "kubernetes",
		Policy:      "dryrun-rejected",
		Resource:    t.ref.String(),
		Message:     errorMessage(err),
		Enforcement: diff.EnforcementEnforce,
	}}, nil
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
		out.Resources = append(out.Resources, *rd)
	}
	out.Summary = summarize(out)
	return out
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
