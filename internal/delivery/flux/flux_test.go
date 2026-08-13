package flux

import (
	"context"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/git"
)

// fakeStatus is a scriptable StatusReader for adapter tests.
type fakeStatus struct {
	ks  []Kustomization
	err error
}

func (f *fakeStatus) Kustomizations(context.Context) ([]Kustomization, error) { return f.ks, f.err }
func (f *fakeStatus) HelmReleases(context.Context) ([]HelmRelease, error)     { return nil, nil }

// recordingReconciler records the Kustomizations it was asked to reconcile.
type recordingReconciler struct {
	got []Kustomization
	err error
}

func (r *recordingReconciler) Reconcile(_ context.Context, k Kustomization) error {
	r.got = append(r.got, k)
	return r.err
}

func bareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}
	return dir
}

func fixedNow() time.Time { return time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC) }

// newTestAdapter wires a flux adapter against a local bare repo with a fake
// status reader and reconciler. The Kustomization in fakeStatus covers
// ./manifests so Apply's not-watched check passes.
func newTestAdapter(t *testing.T) (*Adapter, *fakeStatus, *recordingReconciler, string) {
	t.Helper()
	remote := bareRemote(t)
	status := &fakeStatus{ks: []Kustomization{{
		Name: "web", Namespace: "apps", Path: "./manifests",
		SourceKind: "GitRepository", SourceName: "deploy",
	}}}
	rec := &recordingReconciler{}
	a, err := New(Options{
		Writer: git.Config{
			Target:   git.Target{Repo: remote, Branch: "main", Path: "manifests"},
			Mode:     git.ModeCommit,
			Identity: git.Identity{Name: "Test User", Email: "test@example.com"},
			Now:      fixedNow,
		},
		Reconciler: rec,
		Status:     status,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a, status, rec, remote
}

func testSet() delivery.ManifestSet {
	return delivery.ManifestSet{
		Project:     "shop",
		Environment: "production",
		SpecHash:    "sha256:abc123",
		Revision:    "deadbeef",
		Manifests: []delivery.Manifest{
			{APIVersion: "v1", Kind: "Namespace", Name: "shop-production", YAML: []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: shop-production\n")},
			{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-production", YAML: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: checkout\n  namespace: shop-production\n")},
		},
	}
}

// TestApplyCommitsAndTriggers is the #34 acceptance: the manifests land in the
// repo at the configured path and reconciliation is triggered immediately, not
// left to the poll interval.
func TestApplyCommitsAndTriggers(t *testing.T) {
	a, _, rec, remote := newTestAdapter(t)
	res, err := a.Apply(context.Background(), testSet())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Applied || res.Revision == "" {
		t.Fatalf("result = %+v", res)
	}
	if len(rec.got) != 1 {
		t.Fatalf("reconciled %d kustomizations, want 1", len(rec.got))
	}
	if rec.got[0].Name != "web" || rec.got[0].Namespace != "apps" {
		t.Fatalf("reconciled = %+v", rec.got[0])
	}

	// The commit landed under the path.
	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	commit, _ := repo.CommitObject(ref.Hash())
	tree, _ := commit.Tree()
	if _, err := tree.File("manifests/001-namespace-shop-production.yaml"); err != nil {
		t.Fatalf("rendered namespace file missing from tree: %v", err)
	}
}

// TestApplyReportsNotWatched is the #34 acceptance: writing somewhere no
// Kustomization watches is delivery/not-watched, never silent.
func TestApplyReportsNotWatched(t *testing.T) {
	a, status, _, _ := newTestAdapter(t)
	status.ks = nil // no Kustomization covers the path

	res, err := a.Apply(context.Background(), testSet())
	if err == nil {
		t.Fatalf("apply succeeded (%+v) with nothing watching the path", res)
	}
	if !delivery.AsNotWatched(err) {
		t.Fatalf("error = %v, want delivery/not-watched", err)
	}
}

// TestApplyReconcileFailureIsLoud verifies a failed trigger after a successful
// commit is reported as apply-failed (the commit is safe; latency degrades).
func TestApplyReconcileFailureIsLoud(t *testing.T) {
	a, _, rec, _ := newTestAdapter(t)
	rec.err = delivery.ApplyFailed("x", "", "webhook down", "fix it")
	_, err := a.Apply(context.Background(), testSet())
	if err == nil {
		t.Fatalf("apply must fail when reconciliation fails")
	}
	if !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want apply-failed", err)
	}
}

// TestStatusMapsPhase verifies the adapter answers is-my-change-live by reading
// the Kustomization covering the path.
func TestStatusMapsPhase(t *testing.T) {
	a, status, _, _ := newTestAdapter(t)
	status.ks = []Kustomization{{
		Name: "web", Namespace: "apps", Path: "./manifests",
		Ready: ConditionTrue, LastAppliedRevision: "main@sha1:deadbeef",
	}}
	st, err := a.Status(context.Background(), testSet())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %q, want healthy", st.Phase)
	}
}

// unhealthyStatus is a status reader whose cluster runs a broken Flux.
type unhealthyStatus struct {
	fakeStatus
	calls int
}

func (u *unhealthyStatus) Health(context.Context) (Health, error) {
	u.calls++
	return Health{
		Source:  HealthFromReport,
		Ready:   false,
		Unready: []string{"source-controller"},
		Message: "Flux is not ready: source-controller unavailable",
	}, nil
}

// TestStatusExplainsUnhealthyFlux is the #137 readback: a revision Flux has not
// observed stays Committed — the phase is a fact about the change, not about
// Flux — but the cause names the broken controller instead of leaving the user
// to guess whether waiting will help.
func TestStatusExplainsUnhealthyFlux(t *testing.T) {
	a, _, _, _ := newTestAdapter(t)
	unhealthy := &unhealthyStatus{fakeStatus: fakeStatus{ks: []Kustomization{{
		Name: "web", Namespace: "apps", Path: "./manifests", Ready: ConditionUnknown,
	}}}}
	a.status = unhealthy

	st, err := a.Status(context.Background(), testSet())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != delivery.PhaseCommitted {
		t.Fatalf("phase = %q, want committed", st.Phase)
	}
	if !strings.Contains(st.Cause, "source-controller") {
		t.Fatalf("cause = %q, want the unready component named", st.Cause)
	}
	if st.Detail["fluxHealth"] == "" {
		t.Fatalf("detail = %v, want a fluxHealth entry", st.Detail)
	}
	if unhealthy.calls != 1 {
		t.Fatalf("health read %d times, want 1", unhealthy.calls)
	}
}

// TestStatusSkipsHealthWhenFluxIsDemonstrablyWorking verifies the extra read is
// not made when the Kustomization already proves Flux applied something.
func TestStatusSkipsHealthWhenFluxIsDemonstrablyWorking(t *testing.T) {
	a, _, _, _ := newTestAdapter(t)
	unhealthy := &unhealthyStatus{fakeStatus: fakeStatus{ks: []Kustomization{{
		Name: "web", Namespace: "apps", Path: "./manifests",
		Ready: ConditionTrue, LastAppliedRevision: "main@sha1:deadbeef",
	}}}}
	a.status = unhealthy

	st, err := a.Status(context.Background(), testSet())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %q, want healthy", st.Phase)
	}
	if unhealthy.calls != 0 {
		t.Fatalf("health read %d times on an applied revision, want 0", unhealthy.calls)
	}
}

// TestStatusReportsNotWatched verifies Status answers "unwatched" too, rather
// than fabricating a phase from nothing.
func TestStatusReportsNotWatched(t *testing.T) {
	a, status, _, _ := newTestAdapter(t)
	status.ks = nil
	_, err := a.Status(context.Background(), testSet())
	if err == nil || !delivery.AsNotWatched(err) {
		t.Fatalf("status error = %v, want not-watched", err)
	}
}

// TestHistoryAndRollback verifies the adapter's History reads kelson commits
// and Rollback replays a past revision as a forward commit.
func TestHistoryAndRollback(t *testing.T) {
	a, _, rec, _ := newTestAdapter(t)
	if _, err := a.Apply(context.Background(), testSet()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	set2 := testSet()
	set2.SpecHash = "sha256:newhash"
	set2.Manifests[1].YAML = []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: checkout\n  namespace: shop-production\nspec:\n  replicas: 3\n")
	if _, err := a.Apply(context.Background(), set2); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	entries, err := a.History(context.Background(), testSet())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("history = %d entries, want 2", len(entries))
	}

	// Rollback to the first entry.
	rec.got = nil
	res, err := a.Rollback(context.Background(), testSet(), entries[1])
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !res.Applied {
		t.Fatalf("rollback not applied")
	}
	if len(rec.got) != 1 {
		t.Fatalf("rollback did not trigger reconciliation")
	}
	entries2, _ := a.History(context.Background(), testSet())
	if len(entries2) != 3 {
		t.Fatalf("history after rollback = %d entries, want 3", len(entries2))
	}
}

// TestRegisterFlux verifies registration under the "flux" name and duplicate
// rejection.
func TestRegisterFlux(t *testing.T) {
	reg := delivery.NewRegistry()
	a, _, _, _ := newTestAdapter(t)
	if err := reg.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(a); err == nil {
		t.Fatalf("duplicate registration must fail")
	}
	got, err := reg.Get("flux")
	if err != nil || got.Name() != "flux" {
		t.Fatalf("get flux = %v, %v", got, err)
	}
}

// TestCapabilities declares what callers can rely on.
func TestCapabilities(t *testing.T) {
	a, _, _, _ := newTestAdapter(t)
	c := a.Capabilities()
	if !c.RequiresGit || !c.SupportsPR || !c.SupportsRollback {
		t.Fatalf("capabilities = %+v", c)
	}
	if !strings.Contains(a.Name(), "flux") {
		t.Fatalf("name = %q", a.Name())
	}
}
