package dryrun

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
)

// A fake API server standing in for a real cluster. Unlike the direct
// adapter's fake (which does server-side apply), this one emulates *dry-run*
// apply: the reactor intercepts the server-side apply and returns whatever the
// per-test handler says the API server would persist — without touching the
// live tracker — or an error, depending on the test's simulated cluster shape.
type cluster struct {
	dyn *dynamicfake.FakeDynamicClient

	mu      sync.Mutex
	applied []string
	// handle is the per-resource API-server behaviour: given the submitted
	// object it returns the object that would persist and an error (nil on
	// success). It is the single seam every test configures.
	handle func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

func newCluster() *cluster {
	c := &cluster{
		dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(testScheme(), nil),
	}
	c.dyn.PrependReactor("patch", "*", c.reactDryRun)
	return c
}

func (c *cluster) reactDryRun(action k8stesting.Action) (bool, runtime.Object, error) {
	pa, ok := action.(k8stesting.PatchActionImpl)
	if !ok || pa.PatchType != types.ApplyPatchType {
		return false, nil, nil
	}
	if !containsString(pa.PatchOptions.DryRun, metav1.DryRunAll) {
		return false, nil, nil
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(pa.Patch); err != nil {
		return true, nil, err
	}
	c.mu.Lock()
	c.applied = append(c.applied, obj.GetKind()+"|"+obj.GetNamespace()+"|"+obj.GetName())
	c.mu.Unlock()
	if c.handle == nil {
		return true, obj, nil
	}
	out, err := c.handle(obj)
	return true, out, err
}

// seedLive inserts a live object into the tracker (standing in for current
// cluster state).
func (c *cluster) seedLive(u *unstructured.Unstructured) {
	gvr, _ := meta.UnsafeGuessKindToResource(u.GroupVersionKind())
	_ = c.dyn.Tracker().Create(gvr, u, u.GetNamespace())
}

func containsString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// --- scheme and mapper ------------------------------------------------------

var testKinds = []schema.GroupVersionKind{
	{Version: "v1", Kind: "Namespace"},
	{Version: "v1", Kind: "Service"},
	{Version: "v1", Kind: "ConfigMap"},
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "batch", Version: "v1", Kind: "CronJob"},
	{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
	{Group: "example.com", Version: "v1", Kind: "Widget"},
	{Group: "kyverno.io", Version: "v1alpha2", Kind: "PolicyReport"},
	{Group: "kyverno.io", Version: "v1alpha2", Kind: "ClusterPolicyReport"},
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, k := range testKinds {
		s.AddKnownTypeWithName(k, &unstructured.Unstructured{})
		lk := k
		lk.Kind += "List"
		s.AddKnownTypeWithName(lk, &unstructured.UnstructuredList{})
	}
	return s
}

func testMapper() meta.RESTMapper {
	versions := make([]schema.GroupVersion, 0, len(testKinds))
	for _, k := range testKinds {
		versions = append(versions, k.GroupVersion())
	}
	m := meta.NewDefaultRESTMapper(versions)
	for _, k := range testKinds {
		scope := meta.RESTScopeNamespace
		if k.Kind == "Namespace" || k.Kind == "CustomResourceDefinition" || k.Kind == "ClusterPolicyReport" {
			scope = meta.RESTScopeRoot
		}
		m.Add(k, scope)
	}
	return m
}

func newEngine(t *testing.T, c *cluster) *DryRun {
	t.Helper()
	e, err := New(Options{Client: c.dyn, Mapper: testMapper()})
	if err != nil {
		t.Fatalf("new dryrun: %v", err)
	}
	return e
}

// --- fixtures ---------------------------------------------------------------

const (
	tProj = "shop"
	tEnv  = "prod"
	tNS   = "shop-prod"
)

// manifest builds a rendered document carrying kelson provenance, matching the
// shape the renderer and direct adapter emit.
func manifest(t *testing.T, apiVersion, kind, name, namespace string, body map[string]any) delivery.Manifest {
	t.Helper()
	md := map[string]any{
		"name": name,
		"labels": map[string]any{
			"app.kubernetes.io/managed-by": "kelson",
			"kelson.dev/project":           tProj,
			"kelson.dev/environment":       tEnv,
		},
	}
	if namespace != "" {
		md["namespace"] = namespace
	}
	root := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": md}
	for k, v := range body {
		root[k] = v
	}
	y, err := sigsyaml.Marshal(root)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return delivery.Manifest{APIVersion: apiVersion, Kind: kind, Name: name, Namespace: namespace, YAML: y}
}

func set(ms ...delivery.Manifest) delivery.ManifestSet {
	return delivery.ManifestSet{Project: tProj, Environment: tEnv, SpecHash: "sha256:test", Manifests: ms}
}

// liveDeployment is current cluster state: one container, image nginx:1.20,
// replicas 2.
func liveDeployment() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      "checkout",
			"namespace": tNS,
		},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"replicas": int64(2),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":            "web",
							"image":           "nginx:1.20",
							"imagePullPolicy": "IfNotPresent",
						},
					},
				},
			},
		},
	}}
}

func findResource(t *testing.T, d *diff.Diff, kind string) diff.ResourceDiff {
	t.Helper()
	for _, r := range d.Resources {
		if r.Kind == kind {
			return r
		}
	}
	t.Fatalf("no resource of kind %s in %+v", kind, d.Resources)
	return diff.ResourceDiff{}
}

// --- tests ------------------------------------------------------------------

// TestPreviewSuccessfulModify is the L2 happy path (#43): live Deployment has
// replicas 2, the user sets 3, the server would persist 3. The diff reports a
// modified resource, the replicas field change attributed to the user, and the
// ordering the renderer produced survives to the API.
func TestPreviewSuccessfulModify(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	e := newEngine(t, c)

	dep := manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"replicas": int64(3),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "nginx:1.20"}},
				},
			},
		}})
	out, err := e.Preview(context.Background(), set(
		manifest(t, "v1", "Namespace", tNS, "", nil),
		dep,
	))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if out.Level != diff.LevelServer {
		t.Fatalf("level = %q, want server", out.Level)
	}
	if out.Degraded {
		t.Fatalf("unexpected degradation: %s", out.DegradedReason)
	}
	r := findResource(t, out, "Deployment")
	if r.Op != diff.OpModified {
		t.Fatalf("op = %q, want modified", r.Op)
	}
	if len(r.Fields) == 0 {
		t.Fatal("expected a field diff for replicas")
	}
	var found bool
	for _, f := range r.Fields {
		if f.Path == "spec.replicas" {
			found = true
			if f.Origin != diff.OriginSpec {
				t.Errorf("replicas origin = %q, want spec", f.Origin)
			}
		}
	}
	if !found {
		t.Fatalf("no spec.replicas field diff in %+v", r.Fields)
	}
	if out.Summary.Modified != 1 {
		t.Errorf("summary.Modified = %d, want 1", out.Summary.Modified)
	}
}

// TestPreviewDistinguishesMutationFromUserEdit is issue #43's readability
// acceptance: the API server injects a sidecar container and defaults
// imagePullPolicy on the way in. Those must be attributed OriginAdmission and
// OriginDefaulting respectively — never mistaken for user edits — while a real
// user change stays OriginSpec.
func TestPreviewDistinguishesMutationFromUserEdit(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	c.handle = func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		// The API server (a mutating webhook) appends an injected sidecar and
		// (the defaulting pass) fills imagePullPolicy=Always.
		out := obj.DeepCopy()
		_ = unstructured.SetNestedField(out.Object, "Always", "spec", "template", "spec", "containers", "0", "imagePullPolicy")
		_ = unstructured.SetNestedSlice(out.Object, []any{
			map[string]any{"name": "web", "image": "nginx:1.20", "imagePullPolicy": "Always"},
			map[string]any{"name": "kube-inject", "image": "injector:latest", "imagePullPolicy": "Always"},
		}, "spec", "template", "spec", "containers")
		return out, nil
	}
	e := newEngine(t, c)

	dep := manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"replicas": int64(3),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "nginx:1.20"}},
				},
			},
		}})
	out, err := e.Preview(context.Background(), set(dep))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	r := findResource(t, out, "Deployment")

	var sidecar, defaulting, spec bool
	for _, f := range r.Fields {
		switch f.Path {
		case "spec.template.spec.containers[1]":
			if f.Origin != diff.OriginAdmission {
				t.Errorf("sidecar origin = %q, want admission", f.Origin)
			}
			sidecar = true
		case "spec.template.spec.containers[0].imagePullPolicy":
			if f.Origin != diff.OriginDefaulting {
				t.Errorf("defaulted imagePullPolicy origin = %q, want defaulting", f.Origin)
			}
			defaulting = true
		case "spec.replicas":
			spec = true
		}
	}
	if !sidecar {
		t.Error("expected an admission-origin sidecar field")
	}
	if !defaulting {
		t.Error("expected a defaulting-origin imagePullPolicy field")
	}
	if !spec {
		t.Error("expected a spec-origin replicas field")
	}
	if r.Risk != diff.RiskRestart {
		t.Errorf("resource risk = %q, want restart-required (pods roll)", r.Risk)
	}
}

// TestPreviewImmutableFieldRejected is issue #43's second acceptance criterion:
// a change to an immutable field — the classic "cannot change selector on a
// Deployment" — is caught at preview time before any write is attempted, and
// reported as disruptive naming the offending field.
func TestPreviewImmutableFieldRejected(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	c.handle = func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: "apps", Kind: "Deployment"}, "checkout",
			field.ErrorList{
				field.Invalid(field.NewPath("spec", "selector"), "map[app:other]",
					"field is immutable"),
			})
	}
	e := newEngine(t, c)

	dep := manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "other"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "other"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "nginx:1.20"}},
				},
			},
		}})
	out, err := e.Preview(context.Background(), set(dep))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	r := findResource(t, out, "Deployment")
	if r.Risk != diff.RiskDisruptive {
		t.Fatalf("resource risk = %q, want disruptive", r.Risk)
	}
	if out.Summary.MaxRisk != diff.RiskDisruptive {
		t.Fatalf("maxRisk = %q, want disruptive (a CI gate must block)", out.Summary.MaxRisk)
	}
	var v *diff.PolicyViolation
	for i := range out.Violations {
		if out.Violations[i].Policy == "field-is-immutable" {
			v = &out.Violations[i]
		}
	}
	if v == nil {
		t.Fatalf("expected a field-is-immutable violation, got %+v", out.Violations)
	}
	if v.Engine != "kubernetes" || v.Path != "spec.selector" {
		t.Errorf("violation = %+v, want engine kubernetes path spec.selector", v)
	}
	if v.Enforcement != diff.EnforcementEnforce {
		t.Errorf("enforcement = %q, want enforce (a rejection is a blocker)", v.Enforcement)
	}
}

// TestPreviewKyvernoEnforceRejection is issue #43's first acceptance criterion
// and issue #45's core: a change violating a Kyverno policy (enforce mode) is
// reported at preview time naming the policy, the rule and the engine's own
// message.
func TestPreviewKyvernoEnforceRejection(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	c.handle = func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "checkout",
			fmt.Errorf("resource [%s/checkout] was blocked due to the following policies\n\nrequire-safe-image:\n  require-nonlatest-tag: image 'nginx:latest' uses the 'latest' tag", tNS))
	}
	e := newEngine(t, c)

	dep := manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "nginx:latest"}},
				},
			},
		}})
	out, err := e.Preview(context.Background(), set(dep))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if out.Degraded {
		t.Fatal("a Kyverno enforcement rejection must not be misread as degradation")
	}
	if len(out.Violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", out.Violations)
	}
	v := out.Violations[0]
	if v.Engine != "kyverno" {
		t.Errorf("engine = %q, want kyverno", v.Engine)
	}
	if v.Policy != "require-safe-image" {
		t.Errorf("policy = %q, want require-safe-image", v.Policy)
	}
	if v.Rule != "require-nonlatest-tag" {
		t.Errorf("rule = %q, want require-nonlatest-tag", v.Rule)
	}
	if v.Enforcement != diff.EnforcementEnforce {
		t.Errorf("enforcement = %q, want enforce", v.Enforcement)
	}
	if out.Summary.MaxRisk != diff.RiskDisruptive {
		t.Errorf("maxRisk = %q, want disruptive (the write would be rejected)", out.Summary.MaxRisk)
	}
}

// TestPreviewKyvernoAuditWarning is issue #45's enforce-vs-audit acceptance: an
// audit-mode policy does not veto the request, so the dry-run succeeds; its
// finding arrives via the Kyverno PolicyReport and must be reported as a
// warning (audit), never a blocker.
func TestPreviewKyvernoAuditWarning(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	e := newEngine(t, c)

	report := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kyverno.io/v1alpha2",
		"kind":       "PolicyReport",
		"metadata":   map[string]any{"name": "shop-reports", "namespace": tNS},
		"results": []any{
			map[string]any{
				"policy":  "require-labels",
				"rule":    "require-owner",
				"result":  "fail",
				"message": "validation error: rule require-owner failed; ValidationError: Missing label 'owner'",
				"resources": []any{
					map[string]any{"kind": "Deployment", "namespace": tNS, "name": "checkout"},
				},
			},
		},
	}}
	c.seedLive(report)

	dep := manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "nginx:1.20"}},
				},
			},
		}})
	out, err := e.Preview(context.Background(), set(dep))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	var v *diff.PolicyViolation
	for i := range out.Violations {
		if out.Violations[i].Policy == "require-labels" {
			v = &out.Violations[i]
		}
	}
	if v == nil {
		t.Fatalf("expected an audit-mode kyverno violation, got %+v", out.Violations)
	}
	if v.Enforcement != diff.EnforcementAudit {
		t.Errorf("enforcement = %q, want audit (warning, not a blocker)", v.Enforcement)
	}
	if v.Engine != "kyverno" || v.Rule != "require-owner" {
		t.Errorf("violation = %+v", v)
	}
	if out.Summary.MaxRisk == diff.RiskDisruptive {
		t.Error("an audit-mode warning must not raise MaxRisk to disruptive")
	}
}

// TestPreviewDegradesOnForbidden is issue #43's graceful-degradation
// acceptance: when the user lacks dry-run permission (a plain RBAC 403, not a
// policy rejection) the preview falls back to L1 with Degraded set and a reason
// naming the permission — it never fails the whole preview.
func TestPreviewDegradesOnForbidden(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	c.handle = func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "checkout",
			fmt.Errorf(`user "system:anonymous" cannot patch resource "deployments" in API group "apps"`))
	}
	e := newEngine(t, c)

	dep := manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "nginx:1.20"}},
				},
			},
		}})
	out, err := e.Preview(context.Background(), set(dep))
	if err != nil {
		t.Fatalf("preview returned an error instead of degrading: %v", err)
	}
	if !out.Degraded {
		t.Fatal("expected a degraded preview for an RBAC 403")
	}
	if out.Level != diff.LevelRendered {
		t.Errorf("degraded level = %q, want rendered (L1)", out.Level)
	}
	if out.DegradedReason == "" || !strings.Contains(out.DegradedReason, "dry-run") {
		t.Errorf("degraded reason must say what permission is missing, got %q", out.DegradedReason)
	}
}

// TestPreviewMissingPrerequisiteOrdering is issue #43's ordering acceptance: a
// Deployment whose Namespace is only being created in the same batch fails
// dry-run for an uninteresting reason, and is reported as such — an audit-mode
// ordering finding, not a policy violation, and not silently swallowed.
func TestPreviewMissingPrerequisiteOrdering(t *testing.T) {
	c := newCluster()
	c.handle = func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		if obj.GetKind() == "Namespace" {
			return obj, nil
		}
		return nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "", Resource: "namespaces"}, tNS)
	}
	e := newEngine(t, c)

	dep := manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "nginx:1.20"}},
				},
			},
		}})
	out, err := e.Preview(context.Background(), set(
		manifest(t, "v1", "Namespace", tNS, "", nil),
		dep,
	))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	var pending *diff.PolicyViolation
	for i := range out.Violations {
		if out.Violations[i].Policy == "dryrun-prerequisite-pending" {
			pending = &out.Violations[i]
		}
	}
	if pending == nil {
		t.Fatalf("expected an ordering (prerequisite-pending) finding, got %+v", out.Violations)
	}
	if pending.Enforcement != diff.EnforcementAudit {
		t.Errorf("enforcement = %q, want audit (ordering is a warning, not a blocker)", pending.Enforcement)
	}
	if out.Summary.MaxRisk == diff.RiskDisruptive {
		t.Error("an ordering finding must not raise MaxRisk to disruptive")
	}
}
