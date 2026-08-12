package argocd

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

// fakeReader is a scriptable ApplicationReader for adapter tests.
type fakeReader struct {
	app   Application
	found bool
	err   error
}

func (f *fakeReader) Application(context.Context, string) (Application, bool, error) {
	return f.app, f.found, f.err
}

// syncCall records one Sync invocation.
type syncCall struct {
	app      Application
	revision string
}

type recordingSyncer struct {
	got []syncCall
	err error
}

func (r *recordingSyncer) Sync(_ context.Context, app Application, revision string) error {
	r.got = append(r.got, syncCall{app: app, revision: revision})
	return r.err
}

type fakeProvider struct{ created []git.PullRequestRequest }

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) CreatePullRequest(_ context.Context, req git.PullRequestRequest) (git.PullRequest, error) {
	f.created = append(f.created, req)
	return git.PullRequest{Number: 7, URL: "https://forge.example/pr/7"}, nil
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

// newTestAdapter wires an argocd adapter against a local bare repo with a fake
// Application reader and syncer. The Application covers the delivery target so
// Apply's not-watched check passes.
func newTestAdapter(t *testing.T) (*Adapter, *fakeReader, *recordingSyncer, string) {
	t.Helper()
	remote := bareRemote(t)
	reader := &fakeReader{found: true, app: Application{
		Name: "shop-production", Namespace: "argocd", Project: "default",
		RepoURL: remote, Path: "manifests", TargetRevision: "main",
		AutoSync: true, SyncStatus: SyncUnknown, Health: HealthUnknown,
	}}
	syncer := &recordingSyncer{}
	a, err := New(Options{
		Writer: git.Config{
			Target:   git.Target{Repo: remote, Branch: "main", Path: "manifests"},
			Mode:     git.ModeCommit,
			Identity: git.Identity{Name: "Test User", Email: "test@example.com"},
			Now:      fixedNow,
		},
		Reader: reader,
		Syncer: syncer,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a, reader, syncer, remote
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

// TestApplyCommitsAndSyncs is the #35 acceptance: the manifests land in the
// repo at the configured path and the Argo Application is synced immediately to
// the committed revision, not left to Argo's poll interval.
func TestApplyCommitsAndSyncs(t *testing.T) {
	a, _, syncer, remote := newTestAdapter(t)
	res, err := a.Apply(context.Background(), testSet())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Applied || res.Revision == "" {
		t.Fatalf("result = %+v", res)
	}
	if len(syncer.got) != 1 {
		t.Fatalf("synced %d applications, want 1", len(syncer.got))
	}
	if syncer.got[0].app.Name != "shop-production" || syncer.got[0].app.Namespace != "argocd" {
		t.Fatalf("synced application = %+v", syncer.got[0].app)
	}
	if syncer.got[0].revision != res.Revision {
		t.Fatalf("synced revision = %q, want the committed revision %q", syncer.got[0].revision, res.Revision)
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

// TestApplyDerivesApplicationName pins the one-Application-per-project/environment
// mapping: the adapter looks up "{project}-{environment}".
func TestApplyDerivesApplicationName(t *testing.T) {
	if got := ApplicationName("shop", "production"); got != "shop-production" {
		t.Fatalf("ApplicationName = %q", got)
	}
	if got := ApplicationName("Shop Front", "pre_prod"); got != "shop-front-pre-prod" {
		t.Fatalf("ApplicationName = %q", got)
	}
	if got := ApplicationName(strings.Repeat("a", 40), strings.Repeat("b", 40)); len(got) > 63 {
		t.Fatalf("ApplicationName length = %d, want <= 63", len(got))
	}

	a, _, _, _ := newTestAdapter(t)
	if got := a.nameFor(testSet()); got != "shop-production" {
		t.Fatalf("nameFor = %q", got)
	}
}

// TestApplyReportsNotWatchedWhenApplicationMissing is the #35 analog of the flux
// not-watched acceptance: committing where no Argo Application exists is loud.
func TestApplyReportsNotWatchedWhenApplicationMissing(t *testing.T) {
	a, reader, _, _ := newTestAdapter(t)
	reader.found = false

	res, err := a.Apply(context.Background(), testSet())
	if err == nil {
		t.Fatalf("apply succeeded (%+v) with no Argo Application", res)
	}
	if !delivery.AsNotWatched(err) {
		t.Fatalf("error = %v, want delivery/not-watched", err)
	}
	if !strings.Contains(err.Error(), "shop-production") {
		t.Fatalf("error must name the Application kelson looked for: %v", err)
	}
}

// TestApplyReportsNotWatchedWhenApplicationTracksElsewhere verifies the source
// check: an Application watching another path, repo or branch does not count as
// watching kelson's commit.
func TestApplyReportsNotWatchedWhenApplicationTracksElsewhere(t *testing.T) {
	cases := map[string]func(*Application){
		"other path":   func(app *Application) { app.Path = "other" },
		"other repo":   func(app *Application) { app.RepoURL = "https://forge.example/someone/else.git" },
		"other branch": func(app *Application) { app.TargetRevision = "staging" },
		"pinned tag":   func(app *Application) { app.TargetRevision = "v1.2.3" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, reader, syncer, _ := newTestAdapter(t)
			mutate(&reader.app)
			if _, err := a.Apply(context.Background(), testSet()); !delivery.AsNotWatched(err) {
				t.Fatalf("error = %v, want delivery/not-watched", err)
			}
			if len(syncer.got) != 0 {
				t.Fatalf("must not sync an Application that does not track the commit")
			}
		})
	}
}

// TestApplyRejectsForeignArgoProject verifies the Argo project guard: kelson
// does not sync an Application owned by a different AppProject.
func TestApplyRejectsForeignArgoProject(t *testing.T) {
	a, reader, syncer, _ := newTestAdapter(t)
	reader.app.Project = "platform"

	_, err := a.Apply(context.Background(), testSet())
	if err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
	if !strings.Contains(err.Error(), "platform") || !strings.Contains(err.Error(), "RBAC") {
		t.Fatalf("error must name the project and the RBAC fix: %v", err)
	}
	if len(syncer.got) != 0 {
		t.Fatalf("must not sync an Application in another project")
	}
}

// TestApplySyncFailureIsLoud verifies a failed sync after a successful commit is
// reported as apply-failed (the commit is safe; delivery degrades).
func TestApplySyncFailureIsLoud(t *testing.T) {
	a, _, syncer, _ := newTestAdapter(t)
	syncer.err = syncErr(Application{Name: "shop-production", Namespace: "argocd", AutoSync: true},
		context.DeadlineExceeded)

	_, err := a.Apply(context.Background(), testSet())
	if err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
	if !strings.Contains(err.Error(), "committed") {
		t.Fatalf("error must note the change is committed: %v", err)
	}
}

// TestPullRequestModeDoesNotSync verifies the approval gate: in pull-request
// mode the commit sits on a kelson branch Argo does not track, so kelson never
// syncs it and reports the last mile as incomplete.
func TestPullRequestModeDoesNotSync(t *testing.T) {
	remote := bareRemote(t)
	reader := &fakeReader{found: true, app: Application{
		Name: "shop-production", Namespace: "argocd", Project: "default",
		RepoURL: remote, Path: "manifests", TargetRevision: "main",
	}}
	syncer := &recordingSyncer{}
	provider := &fakeProvider{}
	a, err := New(Options{
		Writer: git.Config{
			Target:   git.Target{Repo: remote, Branch: "main", Path: "manifests"},
			Mode:     git.ModePullRequest,
			Identity: git.Identity{Name: "Test User", Email: "test@example.com"},
			PR:       git.PRConfig{Owner: "acme", Repo: "deploy"},
			Provider: provider,
			Now:      fixedNow,
		},
		Reader: reader,
		Syncer: syncer,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	res, err := a.Apply(context.Background(), testSet())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied {
		t.Fatalf("pull-request mode must not report the change applied: %+v", res)
	}
	if len(syncer.got) != 0 {
		t.Fatalf("pull-request mode must not sync an unmerged revision")
	}
	if len(provider.created) != 1 {
		t.Fatalf("pull request not opened: %+v", provider.created)
	}
}

// TestStatusMapsArgoState verifies the adapter answers is-my-change-live by
// reading Argo's own sync and health status for the target Application.
func TestStatusMapsArgoState(t *testing.T) {
	a, reader, _, _ := newTestAdapter(t)
	reader.app.SyncStatus = SyncSynced
	reader.app.SyncedRevision = "deadbeef"
	reader.app.Health = HealthHealthy

	st, err := a.Status(context.Background(), testSet())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %q, want Healthy", st.Phase)
	}
	if st.Detail["autoSync"] != "true" {
		t.Fatalf("detail must report the Application's sync policy: %v", st.Detail)
	}
}

// TestStatusNeverHealthyWhenArgoDegraded is the #35 acceptance criterion.
func TestStatusNeverHealthyWhenArgoDegraded(t *testing.T) {
	a, reader, _, _ := newTestAdapter(t)
	reader.app.SyncStatus = SyncSynced
	reader.app.SyncedRevision = "deadbeef"
	reader.app.Health = HealthDegraded
	reader.app.HealthMessage = "Deployment checkout has 0/3 replicas available"

	st, err := a.Status(context.Background(), testSet())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != delivery.PhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", st.Phase)
	}
	if !strings.Contains(st.Cause, "argocd/shop-production") || !strings.Contains(st.Cause, "0/3 replicas") {
		t.Fatalf("cause must name the Application and Argo's health reason: %q", st.Cause)
	}
}

// TestStatusReportsNotWatched verifies Status answers "nothing watches this"
// too, rather than fabricating a phase from nothing.
func TestStatusReportsNotWatched(t *testing.T) {
	a, reader, _, _ := newTestAdapter(t)
	reader.found = false
	if _, err := a.Status(context.Background(), testSet()); !delivery.AsNotWatched(err) {
		t.Fatalf("status error = %v, want delivery/not-watched", err)
	}
}

// TestHistoryAndRollback verifies History reads kelson commits from the
// repository and Rollback replays a past revision as a forward commit plus a
// sync — never a force-push.
func TestHistoryAndRollback(t *testing.T) {
	a, _, syncer, _ := newTestAdapter(t)
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

	syncer.got = nil
	res, err := a.Rollback(context.Background(), testSet(), entries[1])
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !res.Applied {
		t.Fatalf("rollback not applied")
	}
	if len(syncer.got) != 1 || syncer.got[0].revision != res.Revision {
		t.Fatalf("rollback did not sync the rollback commit: %+v", syncer.got)
	}
	entries2, _ := a.History(context.Background(), testSet())
	if len(entries2) != 3 {
		t.Fatalf("history after rollback = %d entries, want 3", len(entries2))
	}
}

// TestRollbackRejectsUnknownRevision verifies a revision kelson never wrote is a
// structured failure rather than an empty commit.
func TestRollbackRejectsUnknownRevision(t *testing.T) {
	a, _, _, _ := newTestAdapter(t)
	if _, err := a.Apply(context.Background(), testSet()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	_, err := a.Rollback(context.Background(), testSet(), delivery.Entry{Revision: "0000000000000000000000000000000000000000"})
	if err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
}

// TestRegisterArgoCD verifies registration under the "argocd" name, the name the
// Registry selects for delivery mode "argocd".
func TestRegisterArgoCD(t *testing.T) {
	reg := delivery.NewRegistry()
	remote := bareRemote(t)
	a, err := RegisterArgoCD(reg, Options{
		Writer: git.Config{
			Target:   git.Target{Repo: remote, Branch: "main", Path: "manifests"},
			Identity: git.Identity{Name: "Test User", Email: "test@example.com"},
			Now:      fixedNow,
		},
		API: ClientConfig{BaseURL: "https://argocd.example.com", Token: "t"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if a.project != DefaultProject {
		t.Fatalf("project = %q, want %q", a.project, DefaultProject)
	}
	got, err := reg.Select("argocd")
	if err != nil || got.Name() != "argocd" {
		t.Fatalf("select argocd = %v, %v", got, err)
	}
	if _, err := RegisterArgoCD(reg, Options{
		Writer: git.Config{Target: git.Target{Repo: remote}},
		API:    ClientConfig{BaseURL: "https://argocd.example.com"},
	}); err == nil {
		t.Fatalf("duplicate registration must fail")
	}
}

// TestNewRequiresAnArgoServer verifies the adapter refuses to exist without a
// way to read Argo state: a Git-mode adapter that cannot answer
// is-my-change-live is worse than no adapter.
func TestNewRequiresAnArgoServer(t *testing.T) {
	_, err := New(Options{Writer: git.Config{Target: git.Target{Repo: bareRemote(t)}}})
	if err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
}

// TestCapabilities declares what callers can rely on.
func TestCapabilities(t *testing.T) {
	a, _, _, _ := newTestAdapter(t)
	c := a.Capabilities()
	if !c.RequiresGit || !c.SupportsPR || !c.SupportsRollback {
		t.Fatalf("capabilities = %+v", c)
	}
	if a.Name() != "argocd" {
		t.Fatalf("name = %q", a.Name())
	}
}
