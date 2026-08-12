package git

import (
	"context"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/dafrie/kelson/internal/delivery"
)

func fixedNow() time.Time {
	return time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
}

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

func testWriter(t *testing.T, remote string, mutate func(*Config)) *Writer {
	t.Helper()
	cfg := Config{
		Target:   Target{Repo: remote, Branch: "main", Path: "manifests"},
		Mode:     ModeCommit,
		Identity: Identity{Name: "Test User", Email: "test@example.com"},
		Now:      fixedNow,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	return w
}

func deployMsg() Message {
	return Message{Subject: "deploy shop", Project: "shop", Environment: "production", SpecHash: "sha256:abc123"}
}

// remoteTree returns the files at the head of the remote branch.
func remoteTree(t *testing.T, remote, branch string) map[string]string {
	t.Helper()
	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve %s: %v", branch, err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	out := map[string]string{}
	err = tree.Files().ForEach(func(f *object.File) error {
		c, err := f.Contents()
		if err != nil {
			return err
		}
		out[f.Name] = c
		return nil
	})
	if err != nil {
		t.Fatalf("walk tree: %v", err)
	}
	return out
}

// TestWriteCommitsToBranch is the core acceptance: rendered files land on the
// configured branch, at the configured path, with the trailer convention that
// makes history and provenance correlation work (#39).
func TestWriteCommitsToBranch(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)

	res, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("apiVersion: apps/v1\nkind: Deployment\n")}},
		Message: deployMsg(),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.NoChange || res.Revision == "" {
		t.Fatalf("result = %+v, want a new revision", res)
	}
	if res.Branch != "main" || res.Mode != ModeCommit {
		t.Fatalf("branch/mode = %q/%q, want main/commit", res.Branch, res.Mode)
	}

	tree := remoteTree(t, remote, "main")
	got, ok := tree["manifests/web.yaml"]
	if !ok {
		t.Fatalf("manifests/web.yaml missing from remote tree, got %v", keys(tree))
	}
	if got != "apiVersion: apps/v1\nkind: Deployment\n" {
		t.Fatalf("content = %q", got)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestWriteOnlyWithinPath verifies kelson writes strictly inside the
// configured path and never disturbs unrelated repository content (#39).
func TestWriteOnlyWithinPath(t *testing.T) {
	remote := bareRemote(t)
	// Seed the remote with unrelated content before kelson ever runs.
	seed := testWriter(t, remote, func(c *Config) { c.Target.Path = "" })
	if _, err := seed.Write(context.Background(), WriteRequest{
		Files: []File{
			{Path: "README.md", Data: []byte("# deployment repo\n")},
			{Path: "scripts/rotate.sh", Data: []byte("#!/bin/sh\n")},
		},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := testWriter(t, remote, nil)
	if _, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("x: 1\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	tree := remoteTree(t, remote, "main")
	if tree["README.md"] != "# deployment repo\n" {
		t.Fatalf("README.md was disturbed: %q", tree["README.md"])
	}
	if tree["scripts/rotate.sh"] != "#!/bin/sh\n" {
		t.Fatalf("scripts/rotate.sh was disturbed: %q", tree["scripts/rotate.sh"])
	}
	if _, ok := tree["manifests/web.yaml"]; !ok {
		t.Fatalf("manifests/web.yaml missing")
	}
}

// TestStagePrunesOnlyOwnedFiles verifies that a file kelson previously wrote
// and no longer renders is pruned, while dotfiles (repository conventions like
// .sops.yaml) survive (#34, #39).
func TestStagePrunesOnlyOwnedFiles(t *testing.T) {
	remote := bareRemote(t)
	cfg := func(c *Config) {
		c.Target.Path = "manifests"
		c.Target.Repo = remote
	}
	w1 := testWriter(t, remote, cfg)
	if _, err := w1.Write(context.Background(), WriteRequest{
		Files: []File{
			{Path: "web.yaml", Data: []byte("x: 1\n")},
			{Path: "api.yaml", Data: []byte("y: 2\n")},
			{Path: ".sops.yaml", Data: []byte("creation_rules: []\n")},
		},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Second write drops api.yaml: it must be pruned. .sops.yaml survives.
	if _, err := w1.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("x: 1\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	tree := remoteTree(t, remote, "main")
	if _, ok := tree["manifests/web.yaml"]; !ok {
		t.Fatalf("web.yaml missing")
	}
	if _, ok := tree["manifests/api.yaml"]; ok {
		t.Fatalf("api.yaml was not pruned")
	}
	if tree["manifests/.sops.yaml"] != "creation_rules: []\n" {
		t.Fatalf(".sops.yaml must survive pruning")
	}
}

// TestNoChangeIsIdempotent verifies that re-applying an unchanged manifest set
// produces no commit at all (issue #39 acceptance: no empty commits).
func TestNoChangeIsIdempotent(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)
	req := WriteRequest{Files: []File{{Path: "web.yaml", Data: []byte("x: 1\n")}}, Message: deployMsg()}

	if _, err := w.Write(context.Background(), req); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first := remoteTree(t, remote, "main")

	res, err := w.Write(context.Background(), req)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if !res.NoChange {
		t.Fatalf("second write changed the repository, want NoChange")
	}
	if got := remoteTree(t, remote, "main"); len(got) != len(first) {
		t.Fatalf("tree changed on no-change write")
	}
}

// TestConflictWhenHeadMoves is the optimistic-concurrency acceptance (#40):
// a session that read the remote at one HEAD must refuse to land if the remote
// moved past it — never a force-push.
func TestConflictWhenHeadMoves(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)
	msg := deployMsg()

	// Read the remote at its (empty) HEAD.
	s, err := w.Open(context.Background(), msg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A competing writer lands first.
	comp := testWriter(t, remote, nil)
	if _, err := comp.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "other.yaml", Data: []byte("competitor: true\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("competitor write: %v", err)
	}

	// Our session must now conflict.
	if err := s.Stage([]File{{Path: "web.yaml", Data: []byte("x: 1\n")}}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	_, err = s.Commit(context.Background())
	if err == nil {
		t.Fatalf("commit landed over a moved HEAD; want conflict")
	}
	if !delivery.AsConflict(err) {
		t.Fatalf("error = %v, want delivery/conflict", err)
	}
}

// TestAttributionDistinguishesAgent verifies the #39 attribution convention:
// an agent commit is authored by the agent identity, never by the human who
// asked, and the human is recorded in a trailer.
func TestAttributionDistinguishesAgent(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, func(c *Config) {
		c.Identity = Identity{Actor: ActorAgent, AgentID: "ci-bot", RequestedBy: "alice"}
	})
	if _, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("x: 1\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	commit, _ := repo.CommitObject(ref.Hash())
	if commit.Author.Name != "kelson agent ci-bot" {
		t.Fatalf("author name = %q, want agent attribution", commit.Author.Name)
	}
	if commit.Author.Email != "ci-bot@agents.kelson.dev" {
		t.Fatalf("author email = %q", commit.Author.Email)
	}
	_, trailers := parseTrailers(commit.Message)
	if trailers[TrailerActor] != string(ActorAgent) {
		t.Fatalf("Kelson-Actor trailer = %q", trailers[TrailerActor])
	}
	if trailers[TrailerRequestedBy] != "alice" {
		t.Fatalf("Kelson-Requested-By trailer = %q, want the human who asked", trailers[TrailerRequestedBy])
	}
	if strings.Contains(commit.Author.Name, "alice") {
		t.Fatalf("agent commit must not be authored by the requesting human")
	}
}

// TestHumanAttribution verifies human-authored commits carry the configured
// name/email.
func TestHumanAttribution(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)
	if _, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("x: 1\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	repo, _ := gogit.PlainOpen(remote)
	ref, _ := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	commit, _ := repo.CommitObject(ref.Hash())
	if commit.Author.Name != "Test User" || commit.Author.Email != "test@example.com" {
		t.Fatalf("author = %s <%s>", commit.Author.Name, commit.Author.Email)
	}
}

// TestHistoryFiltersByTrailers verifies History() returns only kelson commits,
// newest first, filtered by project/environment trailers.
func TestHistoryFiltersByTrailers(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)

	for i, tc := range []struct{ project, env string }{
		{"shop", "production"},
		{"shop", "production"},
		{"checkout", "staging"},
	} {
		msg := Message{Subject: "deploy", Project: tc.project, Environment: tc.env, SpecHash: "sha256:" + tc.project}
		if _, err := w.Write(context.Background(), WriteRequest{
			Files:   []File{{Path: "web.yaml", Data: []byte(tc.project + "-" + tc.env + "-" + itoa2(i) + "\n")}},
			Message: msg,
		}); err != nil {
			t.Fatalf("write %s/%s: %v", tc.project, tc.env, err)
		}
	}

	entries, err := w.History(context.Background(), HistoryFilter{Project: "shop", Environment: "production"})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("history = %d entries, want 2", len(entries))
	}
	if entries[0].Revision == entries[1].Revision {
		t.Fatalf("revisions must differ")
	}
	if entries[0].SpecHash != "sha256:shop" || entries[1].SpecHash != "sha256:shop" {
		t.Fatalf("spec hashes = %q, %q", entries[0].SpecHash, entries[1].SpecHash)
	}
	if entries[0].Author == "" {
		t.Fatalf("entry author empty")
	}
}

// TestFilesAtAndRollbackReplay verifies FilesAt reads a past revision and that
// the files round-trip (rollback = write them again as a forward commit).
func TestFilesAtAndRollbackReplay(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)

	res1, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("v1\n")}, {Path: "db.yaml", Data: []byte("db1\n")}},
		Message: deployMsg(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("v2\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatal(err)
	}

	files, err := w.FilesAt(context.Background(), res1.Revision)
	if err != nil {
		t.Fatalf("filesAt: %v", err)
	}
	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = string(f.Data)
	}
	if byPath["web.yaml"] != "v1\n" || byPath["db.yaml"] != "db1\n" {
		t.Fatalf("filesAt = %v", byPath)
	}

	if _, err := w.FilesAt(context.Background(), "deadbeef"); err == nil {
		t.Fatalf("FilesAt with unknown revision must fail")
	}
}

// seedMain creates a base branch on the remote, as every real deployment
// repository has one (PR mode targets it).
func seedMain(t *testing.T, remote string) {
	t.Helper()
	seed := testWriter(t, remote, func(c *Config) { c.Target.Path = "" })
	if _, err := seed.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "README.md", Data: []byte("# deploy\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("seed main: %v", err)
	}
}

// TestPRModeUsesProvider verifies pull-request mode commits to a kelson
// branch and hands the change to the forge provider (#39, #8).
func TestPRModeUsesProvider(t *testing.T) {
	remote := bareRemote(t)
	seedMain(t, remote)
	var created []PullRequestRequest
	provider := &recordingProvider{create: func(req PullRequestRequest) {
		created = append(created, req)
	}}
	w := testWriter(t, remote, func(c *Config) {
		c.Mode = ModePullRequest
		c.Provider = provider
		c.PR = PRConfig{
			Title: "Deploy {{project}}/{{environment}}",
			Body:  "hash {{specHash}}",
			Owner: "acme",
			Repo:  "deploy",
		}
	})

	res, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "web.yaml", Data: []byte("x: 1\n")}},
		Message: deployMsg(),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.PullRequest == nil {
		t.Fatalf("no pull request in result")
	}
	if !strings.HasPrefix(res.Branch, DefaultPRBranchPrefix) {
		t.Fatalf("PR branch = %q, want prefix %q", res.Branch, DefaultPRBranchPrefix)
	}
	if len(created) != 1 {
		t.Fatalf("provider called %d times, want 1", len(created))
	}
	req := created[0]
	if req.Base != "main" || req.Head != res.Branch {
		t.Fatalf("PR head/base = %q->%q, want %q->main", req.Head, req.Base, res.Branch)
	}
	if req.Title != "Deploy shop/production" {
		t.Fatalf("PR title = %q (placeholders not expanded)", req.Title)
	}
	if !strings.Contains(req.Body, "sha256:abc123") {
		t.Fatalf("PR body = %q, want spec hash", req.Body)
	}
}

// TestPRModeIsIdempotent verifies re-delivering the same spec updates the same
// kelson branch and does not force-push (the second write updates the PR).
func TestPRModeIsIdempotent(t *testing.T) {
	remote := bareRemote(t)
	seedMain(t, remote)
	w := testWriter(t, remote, func(c *Config) {
		c.Mode = ModePullRequest
		c.Provider = &recordingProvider{}
		c.PR = PRConfig{Owner: "acme", Repo: "deploy"}
	})
	req := WriteRequest{Files: []File{{Path: "web.yaml", Data: []byte("x: 1\n")}}, Message: deployMsg()}

	if _, err := w.Write(context.Background(), req); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := w.Write(context.Background(), req); err != nil {
		t.Fatalf("second write: %v", err)
	}
}

type recordingProvider struct {
	create func(PullRequestRequest)
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) CreatePullRequest(_ context.Context, req PullRequestRequest) (PullRequest, error) {
	if p.create != nil {
		p.create(req)
	}
	return PullRequest{Number: 7, URL: "https://example.test/pr/7"}, nil
}

var (
	_ Provider = (*recordingProvider)(nil)
)

func itoa2(n int) string { return []string{"a", "b", "c", "d", "e"}[n] }
