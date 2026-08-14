package direct

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/delivery"
)

const (
	testProject = "shop"
	testEnv     = "production"
	testNS      = "shop-production"
)

func newAdapter(t *testing.T, c *cluster) *Adapter {
	t.Helper()
	store, err := OpenStore(StoreOptions{Dir: t.TempDir(), Keep: 5, Now: fixedClock()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	a, err := New(Options{Client: c.dyn, Mapper: testMapper(), History: store, Now: fixedClock()})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a
}

// fixedClock keeps recorded timestamps deterministic.
func fixedClock() func() time.Time {
	base := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

// manifest builds a rendered document carrying the provenance the renderer
// stamps (docs/architecture.md, Provenance).
func manifest(t *testing.T, apiVersion, kind, name, namespace string, mutate ...func(obj map[string]any)) delivery.Manifest {
	t.Helper()
	metadata := map[string]any{
		"name": name,
		"labels": map[string]any{
			labelManagedBy:   managedByKelson,
			labelProject:     testProject,
			labelEnvironment: testEnv,
		},
		"annotations": map[string]any{
			annSpecHash: "sha256:test",
		},
	}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	obj := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}
	for _, m := range mutate {
		m(obj)
	}
	y, err := sigsyaml.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return delivery.Manifest{APIVersion: apiVersion, Kind: kind, Name: name, Namespace: namespace, YAML: y}
}

func spec(fields map[string]any) func(map[string]any) {
	return func(obj map[string]any) { obj["spec"] = fields }
}

func labelSet(kv map[string]string) func(map[string]any) {
	return func(obj map[string]any) {
		md, _ := obj["metadata"].(map[string]any)
		lbls, _ := md["labels"].(map[string]any)
		for k, v := range kv {
			if v == "" {
				delete(lbls, k)
				continue
			}
			lbls[k] = v
		}
	}
}

func set(manifests ...delivery.Manifest) delivery.ManifestSet {
	return delivery.ManifestSet{
		Project:     testProject,
		Environment: testEnv,
		SpecHash:    "sha256:test",
		Manifests:   manifests,
	}
}

func fullSet(t *testing.T) delivery.ManifestSet {
	t.Helper()
	return set(
		manifest(t, "v1", "Namespace", testNS, ""),
		manifest(t, "apps/v1", "Deployment", "checkout", testNS, spec(map[string]any{"replicas": int64(2)})),
		manifest(t, "v1", "Service", "checkout", testNS),
	)
}

// TestApplyPreservesRendererOrder is the #33 ordering acceptance: manifests
// reach the API server in exactly the order the renderer produced them, under
// kelson's own field manager, never forced.
func TestApplyPreservesRendererOrder(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)

	res, err := a.Apply(context.Background(), fullSet(t))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Applied || res.Revision != "rev-00000001" {
		t.Fatalf("result = %+v", res)
	}

	want := []string{"Namespace//" + testNS, "Deployment/" + testNS + "/checkout", "Service/" + testNS + "/checkout"}
	got := c.applyLog()
	if len(got) != len(want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("apply order = %v, want %v", got, want)
		}
	}
	for i, m := range c.managers {
		if m != FieldManager {
			t.Errorf("apply %d used field manager %q, want %q", i, m, FieldManager)
		}
		if c.forced[i] {
			t.Errorf("apply %d was forced; forcing is a silent overwrite (ADR-0001)", i)
		}
	}

	// The adapter stamps the direct-mode revision as provenance so Status can
	// correlate without owning the apply.
	live := c.get(t, "deployments", testNS, "checkout")
	if live == nil {
		t.Fatal("Deployment was not created")
	}
	if got := live.GetAnnotations()[annRevision]; got != "rev-00000001" {
		t.Fatalf("live revision annotation = %q", got)
	}

	// Cluster-scoped resources are applied at root scope.
	if ns := c.get(t, "namespaces", "", testNS); ns == nil {
		t.Fatal("Namespace was not created")
	}
}

// TestApplyConflictIsLoud is the #33 conflict acceptance: a field owned by
// another controller surfaces as delivery/conflict naming resource and field,
// never a silent overwrite.
func TestApplyConflictIsLoud(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	c.ownedElsewhere("Deployment/"+testNS+"/checkout", "hpa-controller", ".spec.replicas")

	_, err := a.Apply(context.Background(), fullSet(t))
	if err == nil {
		t.Fatal("expected the conflicting apply to fail")
	}
	if !delivery.AsConflict(err) {
		t.Fatalf("expected delivery/conflict, got %v", err)
	}
	var de delivery.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected a structured delivery.Error, got %T", err)
	}
	if !strings.Contains(de.Resource, "checkout") {
		t.Errorf("conflict must name the resource, got %q", de.Resource)
	}
	if de.Field != ".spec.replicas" {
		t.Errorf("conflict must name the field, got %q", de.Field)
	}
	if !strings.Contains(de.Message, "hpa-controller") {
		t.Errorf("conflict must name the owning field manager, got %q", de.Message)
	}
	if de.DocsURL == "" || de.Remediation == "" {
		t.Error("structured errors carry remediation and a docs URL")
	}

	// The apply stopped at the conflict: the Service after it was never
	// applied, and nothing was overwritten by force.
	for _, key := range c.applyLog() {
		if strings.HasPrefix(key, "Service/") {
			t.Fatal("apply continued past a conflict")
		}
	}
	if _, err := a.History(context.Background(), fullSet(t)); err != nil {
		t.Fatalf("history: %v", err)
	}
	entries, _ := a.History(context.Background(), fullSet(t))
	if len(entries) != 0 {
		t.Fatalf("a failed apply must not be recorded as history: %+v", entries)
	}
}

// TestApplyImmutableFieldNamesTheDelete is ADR-0027's operational consequence
// made legible. Renaming the selector label orphans every Deployment applied
// before the rename: the API server refuses the apply because spec.selector is
// immutable, and it will refuse it again on every retry. The generic
// apply-failed remediation ("fix the spec") is precisely the wrong advice here
// — the spec is what the object should be — so this failure gets its own code
// and a remediation that names the delete.
func TestApplyImmutableFieldNamesTheDelete(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	c.refusesImmutable("Deployment/"+testNS+"/checkout", "spec.selector")

	_, err := a.Apply(context.Background(), fullSet(t))
	if err == nil {
		t.Fatal("expected the apply to fail on the immutable selector")
	}
	if !delivery.AsImmutableField(err) {
		t.Fatalf("expected delivery/immutable-field, got %v", err)
	}
	var de delivery.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected a structured delivery.Error, got %T", err)
	}
	if !strings.Contains(de.Resource, "checkout") {
		t.Errorf("the refusal must name the resource, got %q", de.Resource)
	}
	if de.Field != "spec.selector" {
		t.Errorf("the refusal must name the stuck field, got %q", de.Field)
	}
	if !strings.Contains(de.Remediation, "delete") {
		t.Errorf("the remediation must name the delete — a retry cannot help; got %q", de.Remediation)
	}
	if de.Cause == "" {
		t.Error("the API server's own words must survive in Cause")
	}
	// Not the generic rejection: a caller switching on the code must be able to
	// tell "your spec is wrong" from "delete this object and deploy again".
	if delivery.AsApplyFailed(err) {
		t.Error("an immutable field must not be classified as a generic apply failure")
	}
}

// TestPruneRemovesOwnedResourcesOnly covers the happy path of #33 pruning:
// what kelson applied and no longer declares is deleted, in the reverse of the
// apply order.
func TestPruneRemovesOwnedResources(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	ctx := context.Background()

	first := set(
		manifest(t, "v1", "Namespace", testNS, ""),
		manifest(t, "apps/v1", "Deployment", "checkout", testNS),
		manifest(t, "v1", "ConfigMap", "legacy", testNS),
	)
	if _, err := a.Apply(ctx, first); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	second := set(
		manifest(t, "v1", "Namespace", testNS, ""),
		manifest(t, "apps/v1", "Deployment", "checkout", testNS),
	)
	if _, err := a.Apply(ctx, second); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if got := c.get(t, "configmaps", testNS, "legacy"); got != nil {
		t.Fatal("a resource dropped from the spec must be pruned")
	}
	if got := c.get(t, "deployments", testNS, "checkout"); got == nil {
		t.Fatal("a declared resource must survive pruning")
	}
	if got := c.get(t, "namespaces", "", testNS); got == nil {
		t.Fatal("the namespace must survive pruning")
	}
	if want := "configmaps/" + testNS + "/legacy"; len(c.deleteLog()) != 1 || c.deleteLog()[0] != want {
		t.Fatalf("deletes = %v, want exactly [%s]", c.deleteLog(), want)
	}
}

// TestPruneNeverDeletesANamespace: since the renderer emits the environment's
// Namespace (#150) it sits in kelson's provenance-labelled inventory, so a
// renamed namespace would otherwise be pruned — cascading to everything inside
// it, including resources kelson never created (#59).
func TestPruneNeverDeletesANamespace(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	ctx := context.Background()

	first := set(
		manifest(t, "v1", "Namespace", testNS, ""),
		manifest(t, "apps/v1", "Deployment", "checkout", testNS),
	)
	if _, err := a.Apply(ctx, first); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// The environment moves to a different namespace: the old one leaves the
	// desired set entirely.
	second := set(
		manifest(t, "v1", "Namespace", "checkout-next", ""),
		manifest(t, "apps/v1", "Deployment", "checkout", "checkout-next"),
	)
	if _, err := a.Apply(ctx, second); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if c.get(t, "namespaces", "", testNS) == nil {
		t.Fatal("the abandoned namespace was deleted; uninstall must decide that, not prune")
	}
	for _, d := range c.deleteLog() {
		if strings.HasPrefix(d, "namespaces/") {
			t.Fatalf("prune issued a namespace delete: %v", c.deleteLog())
		}
	}
}

// TestPruneRefusesResourceWithoutProvenance is the #33 safety acceptance:
// kelson never deletes a resource that does not carry its provenance labels.
func TestPruneRefusesResourceWithoutProvenance(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	ctx := context.Background()

	first := set(
		manifest(t, "apps/v1", "Deployment", "checkout", testNS),
		manifest(t, "v1", "ConfigMap", "shared", testNS),
	)
	if _, err := a.Apply(ctx, first); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Someone else takes ownership of the ConfigMap between deploys.
	live := c.get(t, "configmaps", testNS, "shared")
	live.SetLabels(map[string]string{labelManagedBy: "Helm"})
	c.update(t, "configmaps", live)

	second := set(manifest(t, "apps/v1", "Deployment", "checkout", testNS))
	res, err := a.Apply(ctx, second)
	if err == nil {
		t.Fatal("expected a not-provenanced refusal")
	}
	if !delivery.AsNotProvenanced(err) {
		t.Fatalf("expected delivery/not-provenanced, got %v", err)
	}
	if !res.Applied {
		t.Error("the apply itself succeeded; only the prune was refused")
	}
	var de delivery.Error
	if errors.As(err, &de) && !strings.Contains(de.Resource, "shared") {
		t.Errorf("refusal must name the resource, got %q", de.Resource)
	}
	if c.get(t, "configmaps", testNS, "shared") == nil {
		t.Fatal("a resource without kelson provenance must never be deleted")
	}
	for _, d := range c.deleteLog() {
		if strings.Contains(d, "shared") {
			t.Fatal("delete was issued for a resource kelson does not own")
		}
	}
}

// TestApplyRejectsUnprovenancedManifest guards the invariant pruning relies on:
// nothing is applied that pruning could not later recognise as kelson's.
func TestApplyRejectsUnprovenancedManifest(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)

	bad := set(manifest(t, "apps/v1", "Deployment", "checkout", testNS,
		labelSet(map[string]string{labelManagedBy: ""})))
	_, err := a.Apply(context.Background(), bad)
	var de delivery.Error
	if !errors.As(err, &de) || de.Code != delivery.ErrApplyFailed {
		t.Fatalf("expected delivery/apply-failed, got %v", err)
	}
	if len(c.applyLog()) != 0 {
		t.Fatal("validation must happen before anything reaches the cluster")
	}
}

// TestStatusPhases exercises the #37 readback: correlation by revision, then
// the Deployment condition readback that separates Applied from Healthy and
// Degraded.
func TestStatusPhases(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	ctx := context.Background()
	s := fullSet(t)

	before, err := a.Status(ctx, s)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if before.Phase != delivery.PhaseProposed {
		t.Fatalf("phase before apply = %q, want Proposed", before.Phase)
	}

	if _, err := a.Apply(ctx, s); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Applied, but the workload reports no conditions yet.
	applied, err := a.Status(ctx, s)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if applied.Phase != delivery.PhaseApplied {
		t.Fatalf("phase = %q (%s), want Applied", applied.Phase, applied.Cause)
	}
	if applied.Revision != "rev-00000001" {
		t.Fatalf("status revision = %q", applied.Revision)
	}

	// Healthy needs the rollout to have finished for THIS generation, not just
	// an Available condition — see TestStatusDoesNotReportHealthyMidRollout.
	setRolloutCounts(t, c, 2, 2, 2)
	setCondition(t, c, "Available", "True", "MinimumReplicasAvailable", "")
	healthy, err := a.Status(ctx, s)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if healthy.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %q (%s), want Healthy", healthy.Phase, healthy.Cause)
	}
	if healthy.Detail["live"] != "3" {
		t.Errorf("detail = %v", healthy.Detail)
	}

	setCondition(t, c, "Available", "False", "MinimumReplicasUnavailable", "0/2 replicas are available")
	degraded, err := a.Status(ctx, s)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if degraded.Phase != delivery.PhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", degraded.Phase)
	}
	if !strings.Contains(degraded.Cause, "MinimumReplicasUnavailable") {
		t.Errorf("degraded status must name the cause, got %q", degraded.Cause)
	}

	// A live resource at an older revision is not the change the caller asked
	// about: that is Proposed, not Healthy.
	stale := fullSet(t)
	stale.Revision = "rev-00000009"
	if got, err := a.Status(ctx, stale); err != nil || got.Phase != delivery.PhaseProposed {
		t.Fatalf("stale revision status = %+v (err %v), want Proposed", got, err)
	}
}

// TestStatusDoesNotReportHealthyMidRollout is the regression for the bug the
// first real cluster run found: deploying a second, broken revision exited 0
// immediately.
//
// A server-side apply returns as soon as the object is written. The previous
// ReplicaSet's pods are still Ready at that moment, so the Deployment's
// Available condition and availableReplicas describe the OLD revision — and
// keep describing it forever when the new pods crash-loop, because the rolling
// update never retires the old ones. Health must therefore be pinned to the
// applied generation: mid-rollout is Applied, never Healthy.
func TestStatusDoesNotReportHealthyMidRollout(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	ctx := context.Background()
	s := fullSet(t)

	if _, err := a.Apply(ctx, s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The completed first rollout: this is the state the masking starts from.
	setRolloutCounts(t, c, 2, 2, 2)
	setCondition(t, c, "Available", "True", "MinimumReplicasAvailable", "")
	if got, err := a.Status(ctx, s); err != nil || got.Phase != delivery.PhaseHealthy {
		t.Fatalf("first revision status = %+v (err %v), want Healthy", got, err)
	}

	cases := []struct {
		name                              string
		generation, observed              int64
		updated, total, availableReplicas int64
		wantCause                         string
	}{
		{
			name: "controller has not observed the new generation",
			// The window right after an apply, before the controller reacts.
			generation: 2, observed: 1,
			updated: 2, total: 2, availableReplicas: 2,
			wantCause: "has not observed the latest generation",
		},
		{
			name: "new pods are not created yet",
			// Available is still True: the old ReplicaSet is fully up.
			generation: 2, observed: 2,
			updated: 0, total: 2, availableReplicas: 2,
			wantCause: "0 of 2 updated replicas have been created",
		},
		{
			name: "old replicas are still running",
			// The crash-loop signature: the new pods exist and never become
			// available, so the rolling update keeps the old ones alive.
			generation: 2, observed: 2,
			updated: 2, total: 4, availableReplicas: 2,
			wantCause: "2 replicas of the previous revision are pending termination",
		},
		{
			name:       "updated pods are not available",
			generation: 2, observed: 2,
			updated: 2, total: 2, availableReplicas: 1,
			wantCause: "1 of 2 updated replicas are available",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGeneration(t, c, tc.generation, tc.observed)
			setRolloutCounts(t, c, tc.updated, tc.total, tc.availableReplicas)
			setCondition(t, c, "Available", "True", "MinimumReplicasAvailable", "")

			got, err := a.Status(ctx, s)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if got.Phase == delivery.PhaseHealthy {
				t.Fatalf("phase = Healthy while the rollout is in flight; an apply is not a live revision")
			}
			if got.Phase != delivery.PhaseApplied {
				t.Fatalf("phase = %q, want Applied — a rollout in flight is not broken either", got.Phase)
			}
			if !strings.Contains(got.Cause, tc.wantCause) {
				t.Fatalf("cause = %q, want it to name %q", got.Cause, tc.wantCause)
			}
		})
	}
}

// setRolloutCounts writes the replica counters the Deployment controller
// maintains: how many pods run the current generation, how many run at all,
// and how many are available.
func setRolloutCounts(t *testing.T, c *cluster, updated, total, available int64) {
	t.Helper()
	live := c.get(t, "deployments", testNS, "checkout")
	if live == nil {
		t.Fatal("Deployment is not live")
	}
	for field, value := range map[string]int64{
		"updatedReplicas":   updated,
		"replicas":          total,
		"availableReplicas": available,
	} {
		if err := unstructured.SetNestedField(live.Object, value, "status", field); err != nil {
			t.Fatalf("set status.%s: %v", field, err)
		}
	}
	c.update(t, "deployments", live)
}

// setGeneration writes metadata.generation and the controller's
// status.observedGeneration, which is how "this revision" is identified.
func setGeneration(t *testing.T, c *cluster, generation, observed int64) {
	t.Helper()
	live := c.get(t, "deployments", testNS, "checkout")
	if live == nil {
		t.Fatal("Deployment is not live")
	}
	live.SetGeneration(generation)
	if err := unstructured.SetNestedField(live.Object, observed, "status", "observedGeneration"); err != nil {
		t.Fatalf("set observedGeneration: %v", err)
	}
	c.update(t, "deployments", live)
}

func setCondition(t *testing.T, c *cluster, condType, status, reason, message string) {
	t.Helper()
	live := c.get(t, "deployments", testNS, "checkout")
	if live == nil {
		t.Fatal("Deployment is not live")
	}
	cond := map[string]any{"type": condType, "status": status, "reason": reason}
	if message != "" {
		cond["message"] = message
	}
	if err := unstructured.SetNestedSlice(live.Object, []any{cond}, "status", "conditions"); err != nil {
		t.Fatalf("set condition: %v", err)
	}
	c.update(t, "deployments", live)
}

// TestRollbackReappliesRecordedRevision is the #38 rollback acceptance: the
// stored rendered output is re-applied verbatim, under a new revision, and the
// rollback is itself recorded.
func TestRollbackReappliesRecordedRevision(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	ctx := context.Background()

	v1 := set(
		manifest(t, "apps/v1", "Deployment", "checkout", testNS, spec(map[string]any{"replicas": int64(2)})),
		manifest(t, "v1", "ConfigMap", "flags", testNS),
	)
	if _, err := a.Apply(ctx, v1); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2 := set(manifest(t, "apps/v1", "Deployment", "checkout", testNS, spec(map[string]any{"replicas": int64(5)})))
	if _, err := a.Apply(ctx, v2); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if c.get(t, "configmaps", testNS, "flags") != nil {
		t.Fatal("v2 should have pruned the ConfigMap")
	}

	entries, err := a.History(ctx, v2)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 2 || entries[0].Revision != "rev-00000002" {
		t.Fatalf("history = %+v", entries)
	}

	res, err := a.Rollback(ctx, v2, entries[1])
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if res.Revision != "rev-00000003" || !res.Applied {
		t.Fatalf("rollback result = %+v", res)
	}

	dep := c.get(t, "deployments", testNS, "checkout")
	replicas, found, err := unstructured.NestedInt64(dep.Object, "spec", "replicas")
	if err != nil || !found || replicas != 2 {
		t.Fatalf("replicas = %d (found %v, err %v), want the rolled-back value 2", replicas, found, err)
	}
	if c.get(t, "configmaps", testNS, "flags") == nil {
		t.Fatal("rollback must restore resources the newer revision pruned")
	}

	after, err := a.History(ctx, v2)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(after) != 3 || after[0].Revision != "rev-00000003" {
		t.Fatalf("history after rollback = %+v", after)
	}
	if !strings.Contains(after[0].Message, "rollback to rev-00000001") {
		t.Errorf("the rollback must be recorded as such, got %q", after[0].Message)
	}
	if after[0].SpecHash != entries[1].SpecHash {
		t.Errorf("a rollback entry carries the spec-hash it restored: %q", after[0].SpecHash)
	}
}

func TestRollbackToUnknownRevision(t *testing.T) {
	c := newCluster()
	a := newAdapter(t, c)
	_, err := a.Rollback(context.Background(), fullSet(t), delivery.Entry{Revision: "rev-00000042"})
	var de delivery.Error
	if !errors.As(err, &de) || de.Code != delivery.ErrApplyFailed {
		t.Fatalf("expected delivery/apply-failed, got %v", err)
	}
}

// TestRegisterDirect is the #32 acceptance from the direct adapter's side:
// implement Adapter, register it, select it by delivery mode.
func TestRegisterDirect(t *testing.T) {
	c := newCluster()
	store, err := OpenStore(StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	reg := delivery.NewRegistry()
	a, err := RegisterDirect(reg, Options{Client: c.dyn, Mapper: testMapper(), History: store})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := reg.Select("direct")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != delivery.Adapter(a) || got.Name() != AdapterName {
		t.Fatalf("selected %v", got.Name())
	}
	caps := got.Capabilities()
	if caps.SupportsPR || caps.RequiresGit || !caps.SupportsRollback {
		t.Fatalf("capabilities = %+v, want no PR, no git, rollback", caps)
	}
	if _, err := RegisterDirect(reg, Options{Client: c.dyn, Mapper: testMapper(), History: store}); err == nil {
		t.Fatal("registering twice must fail")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a client is required")
	}
	c := newCluster()
	if _, err := New(Options{Client: c.dyn}); err == nil {
		t.Fatal("a mapper is required")
	}
	if _, err := New(Options{Client: c.dyn, Mapper: testMapper()}); err == nil {
		t.Fatal("a history store is required")
	}
}

// TestSplitDocumentsRoundTrip: the rendered stream recorded in history splits
// back into the same manifests, byte-identically and in order — the property
// rollback and `kelson eject --to-git` both rely on.
func TestSplitDocumentsRoundTrip(t *testing.T) {
	original := fullSet(t).Manifests
	stream, err := renderedBytes(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := SplitDocuments(stream)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(back) != len(original) {
		t.Fatalf("split %d documents, want %d", len(back), len(original))
	}
	for i := range original {
		if back[i].Kind != original[i].Kind || back[i].Name != original[i].Name || back[i].Namespace != original[i].Namespace {
			t.Fatalf("document %d = %+v, want %+v", i, back[i], original[i])
		}
		if strings.TrimSpace(string(back[i].YAML)) != strings.TrimSpace(string(original[i].YAML)) {
			t.Fatalf("document %d bytes changed:\n%s\nwant:\n%s", i, back[i].YAML, original[i].YAML)
		}
	}
}

func TestHealthReadback(t *testing.T) {
	cronjob := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "CronJob",
		"metadata": map[string]any{"name": "nightly"},
		"spec":     map[string]any{"suspend": true},
	}}
	if got, cause := health(cronjob); got != healthProgressing || cause == "" {
		t.Fatalf("suspended CronJob = %v (%q), want progressing with a cause", got, cause)
	}
	if err := unstructured.SetNestedField(cronjob.Object, false, "spec", "suspend"); err != nil {
		t.Fatal(err)
	}
	if got, _ := health(cronjob); got != healthOK {
		t.Fatalf("running CronJob = %v, want healthy", got)
	}

	// A kind with no health signal never blocks Healthy.
	svc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service"}}
	if got, _ := health(svc); got != healthOK {
		t.Fatalf("Service = %v, want healthy", got)
	}
}

// TestApplyRecordsNamespaceAuthorship is the fact `kelson uninstall` runs on
// (issue #59). The renderer stamps kelson.dev/namespace-ownership=declared and
// deliberately claims nothing about authorship; whether kelson CREATED the
// namespace is observable only here, in the instant before the apply.
//
// Three readings, and the uncertain ones resolve towards leaving the namespace
// alone: one that existed first is "adopted", and one kelson created stays
// "created" across every later deploy — a redeploy must not demote the namespace
// the first deploy made.
func TestApplyRecordsNamespaceAuthorship(t *testing.T) {
	ctx := context.Background()

	t.Run("created when the apply brings it into existence", func(t *testing.T) {
		c := newCluster()
		a := newAdapter(t, c)
		if _, err := a.Apply(ctx, fullSet(t)); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := namespaceOwnership(t, c); got != delivery.NamespaceOwnershipCreated {
			t.Fatalf("ownership = %q, want %q — uninstall refuses to remove a namespace it cannot prove kelson made",
				got, delivery.NamespaceOwnershipCreated)
		}
	})

	t.Run("stays created across a redeploy", func(t *testing.T) {
		c := newCluster()
		a := newAdapter(t, c)
		if _, err := a.Apply(ctx, fullSet(t)); err != nil {
			t.Fatalf("first apply: %v", err)
		}
		if _, err := a.Apply(ctx, fullSet(t)); err != nil {
			t.Fatalf("second apply: %v", err)
		}
		if got := namespaceOwnership(t, c); got != delivery.NamespaceOwnershipCreated {
			t.Fatalf("ownership = %q after a redeploy, want %q", got, delivery.NamespaceOwnershipCreated)
		}
	})

	t.Run("adopted when the namespace was already there", func(t *testing.T) {
		c := newCluster()
		a := newAdapter(t, c)
		c.create(t, "namespaces", &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": testNS},
		}})

		if _, err := a.Apply(ctx, fullSet(t)); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := namespaceOwnership(t, c); got != delivery.NamespaceOwnershipAdopted {
			t.Fatalf("ownership = %q, want %q — deleting an adopted namespace would take its other tenants with it",
				got, delivery.NamespaceOwnershipAdopted)
		}
	})
}

func namespaceOwnership(t *testing.T, c *cluster) string {
	t.Helper()
	ns := c.get(t, "namespaces", "", testNS)
	if ns == nil {
		t.Fatalf("namespace %s was not applied", testNS)
	}
	return ns.GetAnnotations()[delivery.AnnNamespaceOwnership]
}
