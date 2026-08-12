package eject

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
	"github.com/dafrie/kelson/internal/delivery/git"
)

const (
	testProject = "shop"
	testEnv     = "production"
)

// bareRemote creates a bare repository to serve as the remote, so tests never
// touch the network or the ambient git config.
func bareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}
	return dir
}

// namespaceDoc / deploymentDoc / configMapDoc are minimal rendered documents:
// eject never interprets them beyond kind and name, so a realistic-looking
// stub is enough and keeps byte comparisons readable.
func namespaceDoc() string {
	return "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: shop-production\n"
}

func deploymentDoc(image string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: shop-production\nspec:\n  template:\n    spec:\n      containers:\n        - name: web\n          image: " + image + "\n"
}

func configMapDoc() string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: legacy\n  namespace: shop-production\ndata:\n  a: b\n"
}

// renderedStream joins documents the way the direct store records them
// (direct.renderedBytes): a separator before every document.
func renderedStream(docs ...string) []byte {
	var b bytes.Buffer
	for _, d := range docs {
		b.WriteString("---\n")
		b.WriteString(d)
		if !strings.HasSuffix(d, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

type revision struct {
	rendered []byte
	rec      direct.Record
}

// historyWith builds a real direct.Store in a temp dir and appends the given
// revisions in order. Using the real store rather than a fake is deliberate:
// eject's contract is with what direct mode actually writes to disk.
func historyWith(t *testing.T, revs ...revision) *direct.Store {
	t.Helper()
	store, err := direct.OpenStore(direct.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, r := range revs {
		if _, err := store.Append(testProject, testEnv, r.rec, r.rendered); err != nil {
			t.Fatalf("append %s: %v", r.rec.Revision, err)
		}
	}
	return store
}

// threeRevisions is the standard fixture: a deploy, a change, and a third
// revision that drops a resource — so the replay must prune, not just add.
func threeRevisions(t *testing.T) *direct.Store {
	t.Helper()
	return historyWith(t,
		revision{
			rendered: renderedStream(namespaceDoc(), deploymentDoc("shop:1"), configMapDoc()),
			rec: direct.Record{
				Revision: "rev-00000001", SpecHash: "sha256:aaa", CommittedAt: "2026-08-01T10:00:00Z",
				Message: "deploy sha256:aaa", Author: "Alice Ng <alice@example.com>",
			},
		},
		revision{
			rendered: renderedStream(namespaceDoc(), deploymentDoc("shop:2"), configMapDoc()),
			rec: direct.Record{
				Revision: "rev-00000002", SpecHash: "sha256:bbb", CommittedAt: "2026-08-02T11:30:00Z",
				Message: "deploy sha256:bbb", Author: "Bob Ito <bob@example.com>",
			},
		},
		revision{
			rendered: renderedStream(namespaceDoc(), deploymentDoc("shop:3")),
			rec: direct.Record{
				Revision: "rev-00000003", SpecHash: "sha256:ccc", CommittedAt: "2026-08-03T12:45:00Z",
				Message: "deploy sha256:ccc", Author: "Alice Ng <alice@example.com>",
			},
		},
	)
}

func testEjector(t *testing.T, store Source, remote string, mutate func(*Options)) *Ejector {
	t.Helper()
	opts := Options{
		Project:     testProject,
		Environment: testEnv,
		History:     store,
		Target:      git.Target{Repo: remote, Branch: "main", Path: "shop/production"},
		Mode:        ModeFlux,
		Now:         func() time.Time { return time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC) },
	}
	if mutate != nil {
		mutate(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatalf("new ejector: %v", err)
	}
	return e
}

// commits returns the remote branch's commits, oldest first.
func commits(t *testing.T, remote, branch string) []*object.Commit {
	t.Helper()
	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve %s: %v", branch, err)
	}
	iter, err := repo.Log(&gogit.LogOptions{From: ref.Hash()})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	defer iter.Close()
	var newestFirst []*object.Commit
	if err := iter.ForEach(func(c *object.Commit) error {
		newestFirst = append(newestFirst, c)
		return nil
	}); err != nil {
		t.Fatalf("walk log: %v", err)
	}
	out := make([]*object.Commit, 0, len(newestFirst))
	for i := len(newestFirst) - 1; i >= 0; i-- {
		out = append(out, newestFirst[i])
	}
	return out
}

// treeAt returns every file in a commit, path → contents.
func treeAt(t *testing.T, c *object.Commit) map[string]string {
	t.Helper()
	tree, err := c.Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	out := map[string]string{}
	if err := tree.Files().ForEach(func(f *object.File) error {
		contents, err := f.Contents()
		if err != nil {
			return err
		}
		out[f.Name] = contents
		return nil
	}); err != nil {
		t.Fatalf("walk tree: %v", err)
	}
	return out
}

func hasBranch(t *testing.T, remote, branch string) bool {
	t.Helper()
	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	_, err = repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	return err == nil
}

// TestReplayPreservesChronology: one commit per recorded revision, oldest
// first, so `git log` reads exactly like `kelson history` (issue #41).
func TestReplayPreservesChronology(t *testing.T) {
	remote := bareRemote(t)
	e := testEjector(t, threeRevisions(t), remote, nil)

	res, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Commits != 3 {
		t.Fatalf("commits = %d, want 3", res.Commits)
	}
	if res.TipRevision != "rev-00000003" {
		t.Fatalf("tip revision = %q, want rev-00000003", res.TipRevision)
	}

	got := commits(t, remote, "main")
	wantRevisions := []string{"rev-00000001", "rev-00000002", "rev-00000003"}
	if len(got) != len(wantRevisions) {
		t.Fatalf("got %d commits, want %d", len(got), len(wantRevisions))
	}
	for i, c := range got {
		if !strings.Contains(c.Message, "Kelson-Revision: "+wantRevisions[i]) {
			t.Fatalf("commit %d does not carry %s:\n%s", i, wantRevisions[i], c.Message)
		}
		if !strings.Contains(c.Message, TrailerEjectedFrom+": direct/"+wantRevisions[i]) {
			t.Fatalf("commit %d is not marked as replayed:\n%s", i, c.Message)
		}
		if i > 0 && !c.Author.When.After(got[i-1].Author.When) {
			t.Fatalf("commit %d is not after commit %d (%s vs %s)", i, i-1, c.Author.When, got[i-1].Author.When)
		}
	}
	if head := got[len(got)-1].Hash.String(); head != res.Head {
		t.Fatalf("result head %q is not the branch tip %q", res.Head, head)
	}
}

// TestReplayPreservesAuthorship: the recorded actor is the commit author with
// the recorded timestamp; kelson is only the committer. Attribution is the
// half of history that an export cannot fabricate later.
func TestReplayPreservesAuthorship(t *testing.T) {
	remote := bareRemote(t)
	e := testEjector(t, threeRevisions(t), remote, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := commits(t, remote, "main")
	want := []struct {
		name, email, when string
	}{
		{"Alice Ng", "alice@example.com", "2026-08-01T10:00:00Z"},
		{"Bob Ito", "bob@example.com", "2026-08-02T11:30:00Z"},
		{"Alice Ng", "alice@example.com", "2026-08-03T12:45:00Z"},
	}
	for i, w := range want {
		if got[i].Author.Name != w.name || got[i].Author.Email != w.email {
			t.Fatalf("commit %d author = %s <%s>, want %s <%s>",
				i, got[i].Author.Name, got[i].Author.Email, w.name, w.email)
		}
		wantWhen, err := time.Parse(time.RFC3339, w.when)
		if err != nil {
			t.Fatal(err)
		}
		if !got[i].Author.When.Equal(wantWhen) {
			t.Fatalf("commit %d author date = %s, want %s", i, got[i].Author.When, wantWhen)
		}
		if !got[i].Committer.When.Equal(wantWhen) {
			t.Fatalf("commit %d committer date = %s, want %s", i, got[i].Committer.When, wantWhen)
		}
		if got[i].Committer.Name != git.DefaultCommitName {
			t.Fatalf("commit %d committer = %q, want %q", i, got[i].Committer.Name, git.DefaultCommitName)
		}
	}
}

// TestTipIsByteIdenticalToLatestRevision is the acceptance criterion of issue
// #41: after ejecting, the environment keeps running with no redeploy and no
// manifest changes, because the repository tip holds exactly the bytes the
// direct store recorded at the latest revision, in the layout the Git-mode
// adapters write.
func TestTipIsByteIdenticalToLatestRevision(t *testing.T) {
	remote := bareRemote(t)
	store := threeRevisions(t)
	e := testEjector(t, store, remote, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	rendered, err := store.Rendered(testProject, testEnv, "rev-00000003")
	if err != nil {
		t.Fatalf("rendered: %v", err)
	}
	manifests, err := direct.SplitDocuments(rendered)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	want := map[string]string{}
	for _, f := range git.ManifestFiles(delivery.ManifestSet{
		Project: testProject, Environment: testEnv, Manifests: manifests,
	}) {
		want["shop/production/"+f.Path] = string(f.Data)
	}

	all := commits(t, remote, "main")
	got := treeAt(t, all[len(all)-1])
	if len(got) != len(want) {
		t.Fatalf("tip holds %d files, want %d: got %v", len(got), len(want), keys(got))
	}
	for path, data := range want {
		if got[path] != data {
			t.Fatalf("tip file %s is not byte-identical:\ngot:\n%q\nwant:\n%q", path, got[path], data)
		}
	}
}

// TestReplayPrunesDroppedResources: a resource that disappeared between two
// direct revisions must disappear in the replayed commit too, or the tip would
// not match the recorded state.
func TestReplayPrunesDroppedResources(t *testing.T) {
	remote := bareRemote(t)
	e := testEjector(t, threeRevisions(t), remote, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	all := commits(t, remote, "main")
	second := treeAt(t, all[1])
	if _, ok := second["shop/production/003-configmap-legacy.yaml"]; !ok {
		t.Fatalf("revision 2 should still hold the ConfigMap, got %v", keys(second))
	}
	tip := treeAt(t, all[2])
	if _, ok := tip["shop/production/003-configmap-legacy.yaml"]; ok {
		t.Fatalf("revision 3 dropped the ConfigMap but the tip still has it: %v", keys(tip))
	}
}

// TestPlanWritesNothing: --dry-run is built on Plan(), which must not create a
// branch, a commit or a file anywhere.
func TestPlanWritesNothing(t *testing.T) {
	remote := bareRemote(t)
	e := testEjector(t, threeRevisions(t), remote, nil)

	plan, err := e.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Commits) != 3 {
		t.Fatalf("plan has %d commits, want 3", len(plan.Commits))
	}
	if plan.Commits[0].Revision != "rev-00000001" || plan.Tip().Revision != "rev-00000003" {
		t.Fatalf("plan is not oldest-first: %s .. %s", plan.Commits[0].Revision, plan.Tip().Revision)
	}
	if hasBranch(t, remote, "main") {
		t.Fatal("Plan() created a branch on the remote")
	}
}

// TestPlanFilesUseTheGitLayout: the planned paths are exactly what a Git-mode
// adapter would have written (git.ManifestFiles under the target path). If
// these diverge, an ejected repository is not the repository Flux/Argo expects.
func TestPlanFilesUseTheGitLayout(t *testing.T) {
	e := testEjector(t, threeRevisions(t), bareRemote(t), nil)
	plan, err := e.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := []string{
		"shop/production/001-namespace-shop-production.yaml",
		"shop/production/002-deployment-web.yaml",
	}
	var got []string
	for _, f := range plan.Tip().Files {
		got = append(got, f.Path)
	}
	if len(got) != len(want) {
		t.Fatalf("tip files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tip file %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReplayIntoRepositoryRoot covers an empty delivery path: the layout is
// then the repository root and nothing may escape it.
func TestReplayIntoRepositoryRoot(t *testing.T) {
	remote := bareRemote(t)
	e := testEjector(t, threeRevisions(t), remote, func(o *Options) { o.Target.Path = "" })
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	all := commits(t, remote, "main")
	tip := treeAt(t, all[len(all)-1])
	if _, ok := tip["001-namespace-shop-production.yaml"]; !ok {
		t.Fatalf("tip = %v, want files at the repository root", keys(tip))
	}
}

// TestReplayLeavesUnrelatedContentAlone: eject writes only within its
// configured path — the same guarantee the git writer makes — and does not own
// dotfiles even inside it.
func TestReplayLeavesUnrelatedContentAlone(t *testing.T) {
	remote := bareRemote(t)
	seedRemote(t, remote, "main", map[string]string{
		"README.md":                  "hand written\n",
		"other-team/app.yaml":        "not ours\n",
		"shop/production/.sops.yaml": "creation_rules: []\n",
	})
	e := testEjector(t, threeRevisions(t), remote, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	all := commits(t, remote, "main")
	tip := treeAt(t, all[len(all)-1])
	for path, want := range map[string]string{
		"README.md":                  "hand written\n",
		"other-team/app.yaml":        "not ours\n",
		"shop/production/.sops.yaml": "creation_rules: []\n",
	} {
		if tip[path] != want {
			t.Fatalf("eject touched %s: got %q, want %q", path, tip[path], want)
		}
	}
}

// TestReplayKeepsUnchangedRevisions: a revision whose rendered output equals
// the previous one is still a history entry (a rollback to the live state, a
// re-deploy of the same spec). Collapsing it would silently rewrite the past.
func TestReplayKeepsUnchangedRevisions(t *testing.T) {
	remote := bareRemote(t)
	store := historyWith(t,
		revision{
			rendered: renderedStream(namespaceDoc(), deploymentDoc("shop:1")),
			rec: direct.Record{Revision: "rev-00000001", SpecHash: "sha256:aaa",
				CommittedAt: "2026-08-01T10:00:00Z", Message: "deploy sha256:aaa"},
		},
		revision{
			rendered: renderedStream(namespaceDoc(), deploymentDoc("shop:1")),
			rec: direct.Record{Revision: "rev-00000002", SpecHash: "sha256:aaa", Type: direct.TypeRollback,
				CommittedAt: "2026-08-02T10:00:00Z", Message: "rollback to rev-00000001"},
		},
	)
	e := testEjector(t, store, remote, nil)
	res, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Commits != 2 {
		t.Fatalf("commits = %d, want 2", res.Commits)
	}
	if got := commits(t, remote, "main"); len(got) != 2 {
		t.Fatalf("remote has %d commits, want 2", len(got))
	}
}

// TestUnrecordedAuthorFallsBackToKelson: direct-mode deploys record no author
// today. Such an entry must not be attributed to whoever ran the eject.
func TestUnrecordedAuthorFallsBackToKelson(t *testing.T) {
	remote := bareRemote(t)
	store := historyWith(t, revision{
		rendered: renderedStream(namespaceDoc()),
		rec:      direct.Record{Revision: "rev-00000001", SpecHash: "sha256:aaa", Message: "deploy sha256:aaa"},
	})
	e := testEjector(t, store, remote, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := commits(t, remote, "main")[0]
	if got.Author.Name != git.DefaultCommitName || got.Author.Email != git.DefaultCommitEmail {
		t.Fatalf("author = %s <%s>, want %s <%s>",
			got.Author.Name, got.Author.Email, git.DefaultCommitName, git.DefaultCommitEmail)
	}
}

// TestEmptyHistoryIsRefused: eject replays; it never re-renders. An
// environment that has never deployed has nothing to replay, and the error
// must say so rather than producing an empty repository.
func TestEmptyHistoryIsRefused(t *testing.T) {
	store, err := direct.OpenStore(direct.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	e := testEjector(t, store, bareRemote(t), nil)
	if _, err := e.Plan(); err == nil {
		t.Fatal("expected an error for an empty history")
	} else if !strings.Contains(err.Error(), "no rendered history") {
		t.Fatalf("error = %v, want it to name the empty history", err)
	}
}

// TestInvalidOptions covers the configuration refusals eject makes up front.
func TestInvalidOptions(t *testing.T) {
	store := threeRevisions(t)
	base := func() Options {
		return Options{Project: testProject, Environment: testEnv, History: store,
			Target: git.Target{Repo: "/tmp/x"}, Mode: ModeFlux}
	}
	for name, mutate := range map[string]func(*Options){
		"no project":     func(o *Options) { o.Project = "" },
		"no environment": func(o *Options) { o.Environment = "" },
		"no history":     func(o *Options) { o.History = nil },
		"no repo":        func(o *Options) { o.Target.Repo = "" },
		"unknown mode":   func(o *Options) { o.Mode = "helm" },
		"escaping path":  func(o *Options) { o.Target.Path = "../elsewhere" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := base()
			mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Fatal("expected a configuration error")
			}
		})
	}
}

// TestDefaultsAreApplied: an unset branch and mode resolve to the Git-mode
// defaults rather than failing.
func TestDefaultsAreApplied(t *testing.T) {
	e, err := New(Options{
		Project: testProject, Environment: testEnv, History: threeRevisions(t),
		Target: git.Target{Repo: "/tmp/x", Path: "/shop/production/"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := e.Target().Branch; got != git.DefaultBranch {
		t.Fatalf("branch = %q, want %q", got, git.DefaultBranch)
	}
	if got := e.Target().Path; got != "shop/production" {
		t.Fatalf("path = %q, want %q", got, "shop/production")
	}
	plan, err := e.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Mode != ModeFlux {
		t.Fatalf("mode = %q, want %q", plan.Mode, ModeFlux)
	}
}

// TestConcurrentRemoteMoveIsAConflict: eject pushes as a compare-and-swap
// against the HEAD it read, so a repository that moved underneath is reported
// as delivery/conflict and never force-pushed over (issue #40).
func TestConcurrentRemoteMoveIsAConflict(t *testing.T) {
	remote := bareRemote(t)
	seedRemote(t, remote, "main", map[string]string{"README.md": "one\n"})
	// A competing writer lands after the ejector has read the branch but
	// before it pushes — the compare-and-swap window.
	e := testEjector(t, threeRevisions(t), remote, func(o *Options) {
		o.AfterClone = func() {
			seedRemote(t, remote, "main", map[string]string{"README.md": "two\n"})
		}
	})

	plan, err := e.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if _, err := e.Write(context.Background(), plan); err == nil {
		t.Fatal("expected a conflict")
	} else if !delivery.AsConflict(err) {
		t.Fatalf("error = %v, want delivery/conflict", err)
	}
}

func TestParseAuthor(t *testing.T) {
	for _, tc := range []struct {
		in, name, email string
		ok              bool
	}{
		{"Alice Ng <alice@example.com>", "Alice Ng", "alice@example.com", true},
		{"<alice@example.com>", "alice@example.com", "alice@example.com", true},
		{"alice@example.com", "alice@example.com", "alice@example.com", true},
		{"alice", "alice", "", true},
		{"   ", "", "", false},
	} {
		name, email, ok := parseAuthor(tc.in)
		if ok != tc.ok || name != tc.name || email != tc.email {
			t.Fatalf("parseAuthor(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, name, email, ok, tc.name, tc.email, tc.ok)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// seedRemote commits files to a bare remote through a temporary clone, so
// tests can model a repository that already has content or that moves under a
// running eject.
func seedRemote(t *testing.T, remote, branch string, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	// Clone the named branch explicitly: a bare remote seeded earlier has its
	// HEAD symref still pointing at the default (master), which go-git would
	// otherwise try to check out and fail to track the branch we push.
	repo, err := gogit.PlainClone(dir, false, &gogit.CloneOptions{
		URL:            remote,
		SingleBranch:   true,
		ReferenceName:  plumbing.NewBranchReferenceName(branch),
	})
	if err != nil {
		// An empty remote cannot be cloned; initialise instead.
		repo, err = gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
			InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
		})
		if err != nil {
			t.Fatalf("init seed clone: %v", err)
		}
		if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remote}}); err != nil {
			t.Fatalf("create remote: %v", err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for path, data := range files {
		if err := writeFile(wt.Filesystem, path, []byte(data)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if _, err := wt.Add(path); err != nil {
			t.Fatalf("add %s: %v", path, err)
		}
	}
	when := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	if _, err := wt.Commit("seed", &gogit.CommitOptions{
		Author:            &object.Signature{Name: "Seed", Email: "seed@example.com", When: when},
		AllowEmptyCommits: true,
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push seed: %v", err)
	}
}
