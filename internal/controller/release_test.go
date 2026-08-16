package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// The Kustomization topology a release hook produces (issue #227), and the
// sentence an operator reads when one has failed.
//
// The renderer decides which resources are on which side of the barrier and
// these tests do not re-assert that (internal/renderer/release_test.go does).
// What is pinned here is the half ADR-0028 decision 3 puts in the controller:
// how many Flux objects there are, what each of them says, what happens to them
// when the hook is removed or a rollback is pinned, and what the environment's
// phase and cause become while the migration runs and after it has failed.

// releaseRevision is [testRevision] with a release hook on its one component.
func releaseRevision(t *testing.T) Revision {
	t.Helper()
	p := validProject()
	p.Spec.Components[0].Release = &model.Release{
		Command: []string{"./manage.py", "migrate"},
		Timeout: "15m",
	}
	mp, me := modelProject(p), modelEnvironment(validEnvironment())
	resolved, errs := model.Resolve(mp, me)
	if len(errs) > 0 {
		t.Fatalf("resolving the fixture: %v", errs)
	}
	manifests, err := renderer.Render(resolved, clusterprofile.ClusterProfile{}, nil)
	if err != nil {
		t.Fatalf("rendering the fixture: %v", err)
	}
	hash, err := model.SpecHash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	rev := testRevision(t)
	rev.SpecHash, rev.Manifests, rev.Resolved = hash, manifests, resolved
	return rev
}

const releaseObject = "checkout-production-release"

func missing(t *testing.T, d *FluxDeliverer, name string) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(kustomizationGVK)
	key := types.NamespacedName{Namespace: fluxNamespace, Name: name}
	err := d.Client.Get(context.Background(), key, u)
	return apierrors.IsNotFound(err)
}

// TestReleaseHookSplitsTheKustomizations is the shape of the whole feature: two
// Kustomizations over one OCIRepository, the workload one held behind the
// release one, and each pointed at its own half of the artifact.
func TestReleaseHookSplitsTheKustomizations(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	if _, err := d.Deliver(context.Background(), releaseRevision(t)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	release := liveObject(t, d.Client, kustomizationGVK, releaseObject)
	if got := nested(t, release, "spec", "path"); got != "./"+delivery.ReleaseStageDir {
		t.Errorf("the release Kustomization builds %v, want ./%s", got, delivery.ReleaseStageDir)
	}
	// wait: true is the barrier itself — kstatus reads a Job as healthy only
	// once it has completed, so Ready on this object means the migration
	// finished.
	if got := nested(t, release, "spec", "wait"); got != true {
		t.Error("the release Kustomization does not wait; without it Ready means 'the Job exists'")
	}
	// prune: false is what keeps the Job's name meaning something. A pruned Job
	// is a migration that runs again the next time its revision is applied.
	if got := nested(t, release, "spec", "prune"); got != false {
		t.Errorf("the release Kustomization prunes (%v); the completed Jobs are the idempotency key", got)
	}
	// 15m of release timeout plus the margin, so kustomize-controller does not
	// report a healthy migration failed before it has finished.
	if got := nested(t, release, "spec", "timeout"); got != "960s" {
		t.Errorf("the release Kustomization's timeout is %v, want 960s (15m + the margin)", got)
	}
	if got := nested(t, release, "spec", "sourceRef", "name"); got != "checkout-production" {
		t.Errorf("the release Kustomization reads from %v, want the one OCIRepository", got)
	}

	workload := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	if got := nested(t, workload, "spec", "path"); got != "./" {
		t.Errorf("the workload Kustomization builds %v, want ./ — the artifact root is still the "+
			"workload stage, and an artifact published before #227 has to resolve under it", got)
	}
	deps, ok := nested(t, workload, "spec", "dependsOn").([]any)
	if !ok || len(deps) != 1 {
		t.Fatalf("the workload Kustomization dependsOn %v, want exactly the release stage",
			workload.Object["spec"])
	}
	if name := deps[0].(map[string]any)["name"]; name != releaseObject {
		t.Errorf("the workload Kustomization depends on %v, want %s", name, releaseObject)
	}
}

// TestNoReleaseHookKeepsOneKustomization: the split must not tax an environment
// that does not use it. No third object, no dependsOn, nothing to explain.
func TestNoReleaseHookKeepsOneKustomization(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !missing(t, d, releaseObject) {
		t.Error("an environment with no release hook was given a release Kustomization")
	}
	workload := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	if _, found, _ := unstructured.NestedSlice(workload.Object, "spec", "dependsOn"); found {
		t.Error("an environment with no release hook was given a dependsOn")
	}
}

// TestRemovingTheHookRemovesTheReleaseKustomization. Taking `release:` out of a
// spec must take the object with it, or the workload Kustomization keeps
// depending on a stage nothing publishes into and stops reconciling for good.
//
// Deleting it is safe by construction: it prunes nothing, so it owns nothing,
// so it takes nothing with it — the completed Jobs stay in the namespace where
// the next deploy can still find them.
func TestRemovingTheHookRemovesTheReleaseKustomization(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	if _, err := d.Deliver(context.Background(), releaseRevision(t)); err != nil {
		t.Fatal(err)
	}
	liveObject(t, d.Client, kustomizationGVK, releaseObject)

	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatal(err)
	}
	if !missing(t, d, releaseObject) {
		t.Error("the release Kustomization outlived the hook that declared it")
	}
	workload := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	if _, found, _ := unstructured.NestedSlice(workload.Object, "spec", "dependsOn"); found {
		t.Error("the workload Kustomization still depends on a stage that no longer exists")
	}
}

// TestRollbackDropsTheReleaseStage is ADR-0019 decision 6 — "a rollback does not
// re-run the release command" — made true of a plane that re-applies whatever
// the artifact holds.
//
// A rollback repoints the OCIRepository at an older immutable tag. If the
// release Kustomization followed it, it would apply that revision's `release/`
// directory and run a migration the database moved past long ago. So the stage
// is dropped for the duration of the pin and the workload Kustomization loses
// its dependsOn with it; clearing the annotation brings both back, addressing
// the Job that already ran.
func TestRollbackDropsTheReleaseStage(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	rev := releaseRevision(t)
	if _, err := d.Deliver(context.Background(), rev); err != nil {
		t.Fatal(err)
	}
	liveObject(t, d.Client, kustomizationGVK, releaseObject)

	pinned := releaseRevision(t)
	pinned.PinnedTo = "3-aabbccdd"
	pinned.Manifests = nil // a rollback renders nothing
	out, err := d.Deliver(context.Background(), pinned)
	if err != nil {
		t.Fatalf("Deliver under a pin: %v", err)
	}
	if !out.RolledBack {
		t.Fatalf("outcome = %+v, want a rollback", out)
	}
	if !missing(t, d, releaseObject) {
		t.Error("a pinned rollback kept the release Kustomization; it would re-run an old migration")
	}
	workload := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	if _, found, _ := unstructured.NestedSlice(workload.Object, "spec", "dependsOn"); found {
		t.Error("a rolled-back environment still waits on a release stage that is not there")
	}
	if got := nested(t, workload, "spec", "path"); got != "./" {
		t.Errorf("the rolled-back workload Kustomization builds %v, want ./ — the root of an older "+
			"artifact is what it has to resolve", got)
	}
}

// TestTeardownRemovesAllThreeInOrder. The release Kustomization sits between the
// workload one and the source: after the dependent, so nothing is left reporting
// DependencyNotReady instead of pruning, and before the OCIRepository, because a
// source has to outlive every Kustomization that names it.
func TestTeardownRemovesAllThreeInOrder(t *testing.T) {
	var deleted []string
	d := testDeliverer(t, &fakePusher{})
	if _, err := d.Deliver(context.Background(), releaseRevision(t)); err != nil {
		t.Fatal(err)
	}
	d.Client = recordDeletes(t, d.Client, &deleted)

	if err := d.Teardown(context.Background(), "checkout", "production"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	want := []string{
		"Kustomization/checkout-production",
		"Kustomization/" + releaseObject,
		"OCIRepository/checkout-production",
	}
	if len(deleted) != len(want) {
		t.Fatalf("teardown deleted %v, want %v", deleted, want)
	}
	for i := range want {
		if deleted[i] != want[i] {
			t.Errorf("delete %d is %s, want %s (full order: %v)", i, deleted[i], want[i], deleted)
		}
	}
}

// TestFailedReleaseStageIsRejectedAndSaysWhy is scope item 3, and the reason
// [FluxDeliverer.observeRelease] exists at all.
//
// Flux does the blocking: the workload Kustomization reports DependencyNotReady
// and never applies the new revision, so the previous one keeps serving. What
// Flux cannot do is say that a *migration* is why — its message names another
// Kustomization. So the release stage is read as well, and its failure is
// reported in the workload Kustomization's place, as Rejected: the revision was
// processed and refused, it is not live, and the previous one is still serving.
func TestFailedReleaseStageIsRejectedAndSaysWhy(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	first, err := d.Deliver(context.Background(), releaseRevision(t))
	if err != nil {
		t.Fatal(err)
	}

	// What kustomize-controller writes when `wait: true` outlives a Job that
	// exhausted its backoffLimit, and what it writes on the object held behind
	// it.
	setStatus(t, d, releaseObject, first.Revision, first.Digest, "False", "HealthCheckFailed",
		"Health check failed after 15m0s: Job/checkout-production/release-web-1a2b3c4d status: 'Failed'")
	setStatus(t, d, "checkout-production", first.Revision, first.Digest, "False", "DependencyNotReady",
		"dependency 'kelson-system/checkout-production-release' is not ready")

	rev := releaseRevision(t)
	rev.Observed = first.Revision
	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Phase != v1alpha1.PhaseRejected {
		t.Fatalf("phase = %q (%s), want %q — nothing of the new revision was applied",
			out.Phase, out.Cause, v1alpha1.PhaseRejected)
	}
	for _, want := range []string{
		"release hook did not succeed",
		"previous revision is still serving",
		"release-web-",      // the Job to read logs from
		"HealthCheckFailed", // Flux's own words, relayed
		"kubectl logs",      // and the next step
	} {
		if !strings.Contains(out.Cause, want) {
			t.Errorf("the cause does not carry %q:\n%s", want, out.Cause)
		}
	}
	// The workloads are not read back on this path: there are none of this
	// revision to classify, and an empty readback reads as "everything is gone".
	if out.Workloads != nil {
		t.Errorf("a blocked revision reported workload counts: %+v", out.Workloads)
	}
}

// TestRunningReleaseStageReadsAsProgress is the other half of ADR-0019 decision
// 5's mapping: a migration in flight is Reconciling — something is actively
// working on this revision — and never a phase of its own.
func TestRunningReleaseStageReadsAsProgress(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	first, err := d.Deliver(context.Background(), releaseRevision(t))
	if err != nil {
		t.Fatal(err)
	}
	setStatus(t, d, releaseObject, first.Revision, first.Digest, "Unknown", "Progressing",
		"Reconciliation in progress")
	setStatus(t, d, "checkout-production", first.Revision, first.Digest, "False", "DependencyNotReady",
		"dependency 'kelson-system/checkout-production-release' is not ready")

	rev := releaseRevision(t)
	rev.Observed = first.Revision
	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Phase != v1alpha1.PhaseReconciling {
		t.Fatalf("phase = %q (%s), want %q", out.Phase, out.Cause, v1alpha1.PhaseReconciling)
	}
	if !strings.Contains(out.Cause, "release hook is running") {
		t.Errorf("the cause does not say what is being waited on:\n%s", out.Cause)
	}
}

// TestHealthyReleaseStageStepsOutOfTheWay: once the barrier is open the release
// stage has nothing to add, and the workload Kustomization's own phase is the
// whole answer again.
func TestHealthyReleaseStageStepsOutOfTheWay(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	first, err := d.Deliver(context.Background(), releaseRevision(t))
	if err != nil {
		t.Fatal(err)
	}
	setStatus(t, d, releaseObject, first.Revision, first.Digest, "True", "ReconciliationSucceeded", "applied")
	setStatus(t, d, "checkout-production", first.Revision, first.Digest, "True", "ReconciliationSucceeded", "applied")

	rev := releaseRevision(t)
	rev.Observed = first.Revision
	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Phase != v1alpha1.PhaseHealthy {
		t.Fatalf("phase = %q (%s), want %q", out.Phase, out.Cause, v1alpha1.PhaseHealthy)
	}
}

// TestPreviewReleasePathMatchesTheArtifactLayout is the drift guard the
// renderer's own comment points at. internal/renderer may not import the
// delivery plane, so the preview template spells `./release` as a literal; this
// package imports both, and is where the two are held to the same string.
//
// A drift here is silent and total: the preview's Kustomization would build a
// directory nobody publishes into, so every preview of a project with a
// migration would stop at "path not found".
func TestPreviewReleasePathMatchesTheArtifactLayout(t *testing.T) {
	resolved := releaseRevision(t).Resolved
	resolved.Environment.Previews = &model.ResolvedPreviews{
		Provider:  model.PreviewGitHub,
		Repo:      "https://github.com/acme/checkout",
		SecretRef: "github-auth",
		Interval:  "10m",
		Filter:    model.ResolvedPreviewFilter{Limit: model.PreviewDefaultLimit},
		Artifacts: model.PreviewArtifacts{Repository: "oci://ghcr.io/acme/checkout-previews"},
	}
	manifests, err := renderer.Render(resolved, clusterprofile.ClusterProfile{}, nil)
	if err != nil {
		t.Fatalf("rendering a preview-bearing environment: %v", err)
	}
	var template string
	for _, m := range manifests {
		if m.Kind != "ResourceSet" {
			continue
		}
		body, err := m.YAML()
		if err != nil {
			t.Fatal(err)
		}
		template = string(body)
	}
	if template == "" {
		t.Fatal("no ResourceSet was rendered")
	}
	if !strings.Contains(template, "path: ./"+delivery.ReleaseStageDir) {
		t.Errorf("the preview template does not point at ./%s, which is where the publisher writes "+
			"the release stage:\n%s", delivery.ReleaseStageDir, template)
	}
}

// recordDeletes wraps a client so the deletion order of *named* objects is
// observable. It is [interceptDeletes] with the name kept: two of the three
// objects a staged environment owns are Kustomizations, so the kind alone no
// longer says which one went first.
func recordDeletes(t *testing.T, base client.Client, order *[]string) client.Client {
	t.Helper()
	var existing []client.Object
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{
		{kustomizationGVK, "checkout-production"},
		{kustomizationGVK, releaseObject},
		{ociRepositoryGVK, "checkout-production"},
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(o.gvk)
		key := types.NamespacedName{Namespace: fluxNamespace, Name: o.name}
		if err := base.Get(context.Background(), key, u); err != nil {
			t.Fatalf("reading %s %s back: %v", o.gvk.Kind, o.name, err)
		}
		u.SetResourceVersion("")
		existing = append(existing, u)
	}
	return fake.NewClientBuilder().
		WithScheme(base.Scheme()).
		WithObjects(existing...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				*order = append(*order, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
				return nil
			},
		}).
		Build()
}

// setStatus writes the slice of a Kustomization's status that [flux.PhaseFor]
// reads, as kustomize-controller would have written it.
func setStatus(t *testing.T, d *FluxDeliverer, name, revision, digest, status, reason, message string) {
	t.Helper()
	full := revision + "@" + digest
	ks := liveObject(t, d.Client, kustomizationGVK, name)
	if err := unstructured.SetNestedMap(ks.Object, map[string]any{
		"lastAppliedRevision":   full,
		"lastAttemptedRevision": full,
		"conditions": []any{
			map[string]any{"type": "Ready", "status": status, "reason": reason, "message": message},
		},
	}, "status"); err != nil {
		t.Fatal(err)
	}
	if err := d.Client.Update(context.Background(), ks); err != nil {
		t.Fatalf("updating the %s status: %v", name, err)
	}
}
