// Package git is the git writer that sits under every Git-mode delivery
// adapter (issue #39). Flux and Argo differ in who reconciles the repository,
// not in how kelson writes to it, so the write side is built exactly once:
// commit mode and pull-request mode, provider-independent mechanics, one
// concurrency story.
//
// # Guarantees
//
// The four properties this package exists to guarantee, each covered by a
// test:
//
//  1. kelson writes ONLY within its configured path. Unrelated repository
//     content is never touched — not by a write, not by a prune.
//  2. Read-modify-write against the latest HEAD. Every write clones fresh
//     (never from cached state), and the push is a compare-and-swap against
//     the HEAD that was read. If HEAD moved in between, the write fails with
//     delivery/conflict (issue #40). kelson never force-pushes.
//  3. Attribution is honest. Agent commits are authored by the kelson agent
//     identity, never by a human; the requesting human is recorded in a
//     trailer for audit (docs/architecture.md, agent principals).
//  4. Pull-request mode is the same mechanism everywhere. It is what gates
//     agent action in production (issue #8), so it is one implementation.
package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/dafrie/kelson/internal/delivery"
)

// Mode selects how a write reaches the target branch.
type Mode string

const (
	// ModeCommit commits straight to the configured branch.
	ModeCommit Mode = "commit"
	// ModePullRequest commits to a kelson branch and opens a pull request.
	ModePullRequest Mode = "pull-request"
)

// DefaultBranch is used when a target does not name one.
const DefaultBranch = "main"

// DefaultPRBranchPrefix namespaces kelson's branches so they are obvious in
// the forge UI and easy to protect with branch rules.
const DefaultPRBranchPrefix = "kelson/"

// Target is where kelson writes: the resolved Delivery.Git of an Environment.
type Target struct {
	// Repo is the git remote URL (or a local path, which is what tests use).
	Repo string
	// Branch is the branch kelson commits to (or targets with a PR).
	Branch string
	// Path is the directory within the repository kelson owns. Everything
	// outside it is off limits; an empty path means the repository root and
	// should be avoided for shared repositories.
	Path string
}

// PRConfig configures pull-request mode.
type PRConfig struct {
	// Title and Body support {{project}}, {{environment}}, {{specHash}},
	// {{revision}}, {{branch}} and {{path}} placeholders.
	Title string
	Body  string
	// Reviewers are forge usernames; TeamReviewers are org teams.
	Reviewers     []string
	TeamReviewers []string
	// Draft opens the pull request as a draft.
	Draft bool
	// BranchPrefix defaults to DefaultPRBranchPrefix.
	BranchPrefix string
	// Branch pins the head branch name instead of deriving it from the
	// project/environment/spec-hash.
	Branch string
	// Owner and Repo override the slug parsed from Target.Repo (self-hosted
	// forges behind odd URLs, or enterprise setups where they differ).
	Owner string
	Repo  string
}

// Config configures a Writer.
type Config struct {
	Target   Target
	Mode     Mode
	Identity Identity
	Auth     Auth
	PR       PRConfig
	// Provider creates pull requests. Required for ModePullRequest.
	Provider Provider
	// Now is injectable so tests get deterministic commit timestamps.
	Now func() time.Time
}

// File is one file kelson writes, addressed relative to Target.Path.
type File struct {
	Path string
	Data []byte
}

// WriteRequest is one read-modify-write cycle.
type WriteRequest struct {
	Files   []File
	Message Message
}

// WriteResult reports what landed.
type WriteResult struct {
	// Revision is the commit sha (or, when NoChange, the unchanged head).
	Revision string
	// Branch is the branch the commit landed on: the target branch in commit
	// mode, the kelson branch in pull-request mode.
	Branch string
	// Mode records which mode produced this result.
	Mode Mode
	// NoChange is true when the rendered output already matched the
	// repository. Re-applying an unchanged manifest set is idempotent and must
	// not create empty commits (docs/delivery.md).
	NoChange bool
	// PullRequest is set in pull-request mode.
	PullRequest *PullRequest
}

// HistoryFilter narrows the commit log to one project/environment.
type HistoryFilter struct {
	Project     string
	Environment string
	// Limit caps the number of entries; 0 means DefaultHistoryLimit.
	Limit int
}

// DefaultHistoryLimit bounds history reads on large repositories.
const DefaultHistoryLimit = 50

// Writer commits rendered manifests to a deployment repository.
type Writer struct {
	cfg Config
}

// New validates the configuration and returns a Writer.
func New(cfg Config) (*Writer, error) {
	if strings.TrimSpace(cfg.Target.Repo) == "" {
		return nil, configErr("repo", "delivery git target has no repository",
			"set delivery.git.repo on the Environment")
	}
	if cfg.Target.Branch == "" {
		cfg.Target.Branch = DefaultBranch
	}
	clean, err := cleanPath(cfg.Target.Path)
	if err != nil {
		return nil, err
	}
	cfg.Target.Path = clean

	switch cfg.Mode {
	case "":
		cfg.Mode = ModeCommit
	case ModeCommit, ModePullRequest:
	default:
		return nil, configErr("mode", fmt.Sprintf("unknown git delivery mode %q", cfg.Mode),
			"use "+string(ModeCommit)+" or "+string(ModePullRequest))
	}
	if cfg.Mode == ModePullRequest && cfg.Provider == nil {
		return nil, configErr("provider", "pull-request mode requires a forge provider",
			"configure the provider for the repository's forge (github, gitlab, gitea)")
	}
	if cfg.Auth == nil {
		cfg.Auth = Anonymous{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PR.BranchPrefix == "" {
		cfg.PR.BranchPrefix = DefaultPRBranchPrefix
	}
	return &Writer{cfg: cfg}, nil
}

// Target returns the configured target.
func (w *Writer) Target() Target { return w.cfg.Target }

// Mode returns the configured mode.
func (w *Writer) Mode() Mode { return w.cfg.Mode }

// Write performs a full read-modify-write: clone at the latest HEAD, stage the
// files, commit and push (opening a pull request in PR mode).
func (w *Writer) Write(ctx context.Context, req WriteRequest) (WriteResult, error) {
	s, err := w.Open(ctx, req.Message)
	if err != nil {
		return WriteResult{}, err
	}
	if err := s.Stage(req.Files); err != nil {
		return WriteResult{}, err
	}
	return s.Commit(ctx)
}

// PutRequest is one additive read-modify-write: files to write, files to
// remove, and nothing else touched.
//
// It is [WriteRequest]'s sibling and the difference is the whole point.
// [Writer.Write] makes the delivery path *match* a rendered set, pruning what
// is no longer rendered; Put changes exactly the files it names. The sops
// backend needs the second shape (issue #81): `kelson secret set` writes one
// encrypted Secret into a directory full of manifests it did not render and
// must not remove, and a prune there would delete the environment.
type PutRequest struct {
	// Files are written or overwritten, addressed relative to Target.Path.
	Files []File
	// Remove are deleted if tracked, addressed the same way. A path that is
	// not in the repository is not an error: `delete` is idempotent, and the
	// caller has already decided the Secret should not exist.
	Remove  []string
	Message Message
}

// Put performs an additive read-modify-write: clone at the latest HEAD, write
// and remove the named files, commit and push. Everything else under the
// delivery path is left exactly as it was.
func (w *Writer) Put(ctx context.Context, req PutRequest) (WriteResult, error) {
	s, err := w.Open(ctx, req.Message)
	if err != nil {
		return WriteResult{}, err
	}
	if err := s.Put(req.Files, req.Remove); err != nil {
		return WriteResult{}, err
	}
	return s.Commit(ctx)
}

// Put stages an additive change: write these files, remove those, prune
// nothing. See [PutRequest] for why the distinction from [Session.Stage] is
// load-bearing rather than a convenience.
func (s *Session) Put(files []File, remove []string) error {
	for _, rel := range remove {
		full, err := s.w.resolve(rel)
		if err != nil {
			return err
		}
		// A missing file is not an error. `kelson secret delete` is idempotent
		// and go-git reports an absent path as a filesystem error that would
		// otherwise surface as "failed while removing" for a Secret the user
		// had already deleted.
		if _, err := s.fs.Stat(full); err != nil {
			continue
		}
		if _, err := s.wt.Remove(full); err != nil {
			return wrap(err, "removing "+full)
		}
	}
	for _, f := range files {
		full, err := s.w.resolve(f.Path)
		if err != nil {
			return err
		}
		if err := writeFile(s.fs, full, f.Data); err != nil {
			return wrap(err, "writing "+full)
		}
		if _, err := s.wt.Add(full); err != nil {
			return wrap(err, "staging "+full)
		}
	}
	return nil
}

// SecretFiles lists the tracked encrypted Secrets under the delivery path, as
// paths relative to it, sorted.
//
// It reads the committed tree rather than walking the filesystem, exactly as
// the prune does, so nothing outside the configured path is ever enumerated.
func (s *Session) SecretFiles() ([]string, error) {
	tracked, err := s.trackedUnderPath()
	if err != nil {
		return nil, err
	}
	prefix := s.w.secretsPrefix()
	var out []string
	for _, p := range tracked {
		if !underPath(p, prefix) {
			continue
		}
		if _, ok := SecretName(p); !ok {
			continue
		}
		rel, err := filepath.Rel(s.w.cfg.Target.Path, p)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out, nil
}

// ReadFile reads one file from the session's checkout, addressed relative to
// the delivery path. It is how the sops store reads an encrypted Secret's
// public half — its key names and its recipients — without decrypting it.
func (s *Session) ReadFile(rel string) ([]byte, error) {
	full, err := s.w.resolve(rel)
	if err != nil {
		return nil, err
	}
	f, err := s.fs.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle on an in-memory filesystem
	return io.ReadAll(f)
}

// Open starts a write session by reading the repository at its current HEAD.
// It is exported so callers (and tests) can observe the read point: the HEAD
// recorded here is the one the eventual push is required to still find.
func (w *Writer) Open(ctx context.Context, msg Message) (*Session, error) {
	auth, err := w.cfg.Auth.GitAuth()
	if err != nil {
		return nil, err
	}
	fs := memfs.New()
	repo, empty, err := w.cloneOrInit(ctx, fs, auth)
	if err != nil {
		return nil, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, wrap(err, "opening the repository worktree")
	}

	s := &Session{w: w, repo: repo, wt: wt, fs: fs, auth: auth, msg: msg, empty: empty}
	if !empty {
		head, err := repo.Reference(plumbing.NewBranchReferenceName(w.cfg.Target.Branch), true)
		if err != nil {
			return nil, wrap(err, "resolving branch "+w.cfg.Target.Branch)
		}
		s.base = head.Hash()
	}
	s.branch = w.cfg.Target.Branch

	if w.cfg.Mode == ModePullRequest {
		if err := s.startPRBranch(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// cloneOrInit clones the target branch, or initialises a repository when the
// remote is empty (a fresh deployment repo is a normal first-run state, not an
// error the user should have to fix by hand).
func (w *Writer) cloneOrInit(ctx context.Context, fs billy.Filesystem, auth transport.AuthMethod) (*gogit.Repository, bool, error) {
	branchRef := plumbing.NewBranchReferenceName(w.cfg.Target.Branch)
	repo, err := gogit.CloneContext(ctx, memory.NewStorage(), fs, &gogit.CloneOptions{
		URL:           w.cfg.Target.Repo,
		Auth:          auth,
		ReferenceName: branchRef,
		SingleBranch:  true,
	})
	switch {
	case err == nil:
		return repo, false, nil
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		repo, initErr := gogit.InitWithOptions(memory.NewStorage(), fs, gogit.InitOptions{DefaultBranch: branchRef})
		if initErr != nil {
			return nil, false, wrap(initErr, "initialising the empty deployment repository")
		}
		if _, initErr = repo.CreateRemote(&config.RemoteConfig{Name: gogit.DefaultRemoteName, URLs: []string{w.cfg.Target.Repo}}); initErr != nil {
			return nil, false, wrap(initErr, "configuring the remote for the empty deployment repository")
		}
		return repo, true, nil
	default:
		e := delivery.ApplyFailed("git/write", "repo",
			"could not read the deployment repository at its latest HEAD",
			"check the repository URL, the branch name and the delivery credential")
		e.Cause = err.Error()
		return nil, false, e
	}
}

// Session is one read-modify-write cycle against a repository read at a known
// HEAD. Splitting Open/Stage/Commit is what makes the concurrency guarantee
// testable: a competing writer can move the remote between the two calls.
type Session struct {
	w     *Writer
	repo  *gogit.Repository
	wt    *gogit.Worktree
	fs    billy.Filesystem
	auth  transport.AuthMethod
	msg   Message
	empty bool

	// base is the head of the target branch at read time.
	base plumbing.Hash
	// branch is the branch being committed to (the PR branch in PR mode).
	branch string
	// require is the remote ref value the push demands to still find; zero
	// means "the ref must not have existed" is not asserted.
	require plumbing.Hash
	// prExisting is true when the PR branch already existed remotely.
	prExisting bool
}

// BaseRevision is the HEAD this session read. The push will refuse to land if
// the remote has moved past it.
func (s *Session) BaseRevision() string {
	if s.base.IsZero() {
		return ""
	}
	return s.base.String()
}

// Branch is the branch this session commits to.
func (s *Session) Branch() string { return s.branch }

// startPRBranch points the session at the kelson branch, reusing it when it
// already exists remotely so a re-run updates the open pull request instead of
// needing a force-push.
func (s *Session) startPRBranch(ctx context.Context) error {
	name := s.w.prBranchName(s.msg)
	s.branch = name
	ref := plumbing.NewBranchReferenceName(name)

	start := s.base
	if remoteHash, ok, err := s.remoteRef(ctx, name); err != nil {
		return err
	} else if ok {
		start = remoteHash
		s.prExisting = true
		s.require = remoteHash
		spec := config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", name, name))
		if err := s.repo.FetchContext(ctx, &gogit.FetchOptions{
			RemoteName: gogit.DefaultRemoteName,
			Auth:       s.auth,
			RefSpecs:   []config.RefSpec{spec},
		}); err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return wrap(err, "fetching the existing kelson branch "+name)
		}
	}

	if start.IsZero() {
		// Unborn branch in an empty repository: just move HEAD.
		return s.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, ref))
	}
	return s.wt.Checkout(&gogit.CheckoutOptions{Branch: ref, Hash: start, Create: true})
}

// Stage writes the rendered files and prunes the files kelson previously wrote
// that are no longer rendered — both strictly within the configured path.
//
// Pruning is confined to the configured path and skips dotfiles and the
// [SecretsDir] subtree, so what lives alongside the manifests without being
// rendered survives. `.sops.yaml` is the original motivating case — a
// SOPS-encrypted repository keeps its rule file in the directory tree — and
// the encrypted Secrets `kelson secret set` writes under the sops backend are
// the second (issue #81): they belong to the same path a deploy owns, and
// nothing in a *render* knows they exist, because the renderer has no
// plaintext and emits no Secret.
func (s *Session) Stage(files []File) error {
	want := make(map[string][]byte, len(files))
	for _, f := range files {
		full, err := s.w.resolve(f.Path)
		if err != nil {
			return err
		}
		if _, dup := want[full]; dup {
			return configErr("files", fmt.Sprintf("two rendered files resolve to the same path %q", full),
				"this is a renderer bug: file names within a manifest set must be unique")
		}
		want[full] = f.Data
	}

	existing, err := s.trackedUnderPath()
	if err != nil {
		return err
	}
	stale := make([]string, 0, len(existing))
	for _, p := range existing {
		if _, keep := want[p]; keep {
			continue
		}
		if s.w.preserved(p) {
			continue
		}
		stale = append(stale, p)
	}
	sort.Strings(stale)
	for _, p := range stale {
		if _, err := s.wt.Remove(p); err != nil {
			return wrap(err, "pruning "+p)
		}
	}

	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := writeFile(s.fs, p, want[p]); err != nil {
			return wrap(err, "writing "+p)
		}
		if _, err := s.wt.Add(p); err != nil {
			return wrap(err, "staging "+p)
		}
	}
	return nil
}

// trackedUnderPath lists the committed files under the configured path. It
// reads the tree rather than walking the filesystem so nothing outside the
// path is ever enumerated, let alone modified.
func (s *Session) trackedUnderPath() ([]string, error) {
	if s.empty {
		return nil, nil
	}
	head, err := s.repo.Head()
	if err != nil {
		return nil, wrap(err, "resolving HEAD")
	}
	commit, err := s.repo.CommitObject(head.Hash())
	if err != nil {
		return nil, wrap(err, "reading the HEAD commit")
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, wrap(err, "reading the HEAD tree")
	}
	var out []string
	prefix := s.w.cfg.Target.Path
	err = tree.Files().ForEach(func(f *object.File) error {
		if underPath(f.Name, prefix) {
			out = append(out, f.Name)
		}
		return nil
	})
	if err != nil {
		return nil, wrap(err, "listing the files kelson manages")
	}
	return out, nil
}

// Commit records the staged changes and pushes them. The push is a
// compare-and-swap: if the remote branch is no longer at the revision this
// session read, the write fails with delivery/conflict instead of being
// force-pushed over a concurrent change (issue #40).
func (s *Session) Commit(ctx context.Context) (WriteResult, error) {
	status, err := s.wt.Status()
	if err != nil {
		return WriteResult{}, wrap(err, "inspecting the staged changes")
	}
	if status.IsClean() {
		// Idempotent re-apply of an unchanged manifest set.
		return WriteResult{Revision: s.BaseRevision(), Branch: s.branch, Mode: s.w.cfg.Mode, NoChange: true}, nil
	}

	if err := s.assertRemoteUnmoved(ctx); err != nil {
		return WriteResult{}, err
	}

	now := s.w.cfg.Now()
	id := s.w.cfg.Identity
	hash, err := s.wt.Commit(s.msg.withIdentity(id).String(), &gogit.CommitOptions{
		Author:    ptr(id.author(now)),
		Committer: ptr(id.committer(now)),
	})
	if err != nil {
		return WriteResult{}, wrap(err, "creating the commit")
	}

	pushErr := s.push(ctx)
	if pushErr != nil {
		return WriteResult{}, pushErr
	}

	res := WriteResult{Revision: hash.String(), Branch: s.branch, Mode: s.w.cfg.Mode}
	if s.w.cfg.Mode == ModePullRequest {
		pr, err := s.w.openPullRequest(ctx, s.msg, s.branch, hash.String())
		if err != nil {
			return res, err
		}
		res.PullRequest = &pr
	}
	return res, nil
}

// assertRemoteUnmoved re-reads the remote before writing. The push below also
// enforces this atomically; checking first turns the common case into a clear
// conflict error rather than a transport-level rejection message.
func (s *Session) assertRemoteUnmoved(ctx context.Context) error {
	if s.w.cfg.Mode == ModePullRequest {
		// The PR branch, not the base branch, is what must not have moved: a
		// base branch that advanced is what pull requests are for.
		if !s.prExisting {
			return nil
		}
		current, ok, err := s.remoteRef(ctx, s.branch)
		if err != nil {
			return err
		}
		if !ok || current != s.require {
			return conflictErr(s.w.cfg.Target, s.branch, s.require.String(), hashString(current, ok))
		}
		return nil
	}

	current, ok, err := s.remoteRef(ctx, s.branch)
	if err != nil {
		return err
	}
	if s.empty {
		if ok {
			return conflictErr(s.w.cfg.Target, s.branch, "(branch absent)", current.String())
		}
		return nil
	}
	if !ok || current != s.base {
		return conflictErr(s.w.cfg.Target, s.branch, s.base.String(), hashString(current, ok))
	}
	return nil
}

func (s *Session) push(ctx context.Context) error {
	refspec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", s.branch, s.branch))
	opts := &gogit.PushOptions{
		RemoteName: gogit.DefaultRemoteName,
		Auth:       s.auth,
		RefSpecs:   []config.RefSpec{refspec},
		Force:      false,
	}
	// Compare-and-swap on the remote ref: go-git verifies the advertised ref
	// still matches before sending the pack. This is the atomic half of the
	// read-modify-write guarantee.
	if expect := s.expectedRemote(); !expect.IsZero() {
		opts.RequireRemoteRefs = []config.RefSpec{
			config.RefSpec(fmt.Sprintf("%s:refs/heads/%s", expect.String(), s.branch)),
		}
	}

	err := s.repo.PushContext(ctx, opts)
	switch {
	case err == nil, errors.Is(err, gogit.NoErrAlreadyUpToDate):
		return nil
	case isNonFastForward(err):
		return conflictErr(s.w.cfg.Target, s.branch, s.expectedRemote().String(), "(moved)")
	default:
		e := delivery.ApplyFailed("git/write", "push",
			"pushing the rendered manifests failed",
			"check write access to the branch and any branch protection rules")
		e.Cause = err.Error()
		return e
	}
}

// expectedRemote is the remote value the push demands: the read HEAD in commit
// mode, the previous kelson-branch head when updating an open pull request.
func (s *Session) expectedRemote() plumbing.Hash {
	if s.w.cfg.Mode == ModePullRequest {
		return s.require
	}
	if s.empty {
		return plumbing.ZeroHash
	}
	return s.base
}

// remoteRef reads a branch head straight from the remote (git ls-remote).
func (s *Session) remoteRef(ctx context.Context, branch string) (plumbing.Hash, bool, error) {
	remote, err := s.repo.Remote(gogit.DefaultRemoteName)
	if err != nil {
		return plumbing.ZeroHash, false, wrap(err, "resolving the origin remote")
	}
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{Auth: s.auth})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return plumbing.ZeroHash, false, nil
		}
		e := delivery.ApplyFailed("git/write", "repo",
			"could not re-read the deployment repository before writing",
			"check network reachability of the git remote and the delivery credential")
		e.Cause = err.Error()
		return plumbing.ZeroHash, false, e
	}
	want := plumbing.NewBranchReferenceName(branch)
	for _, r := range refs {
		if r.Name() == want {
			return r.Hash(), true, nil
		}
	}
	return plumbing.ZeroHash, false, nil
}

// openPullRequest creates (or finds) the pull request for the kelson branch.
func (w *Writer) openPullRequest(ctx context.Context, msg Message, head, revision string) (PullRequest, error) {
	slug, err := w.slug()
	if err != nil {
		return PullRequest{}, err
	}
	vars := map[string]string{
		"project":     msg.Project,
		"environment": msg.Environment,
		"specHash":    msg.SpecHash,
		"revision":    revision,
		"branch":      w.cfg.Target.Branch,
		"path":        w.cfg.Target.Path,
	}
	title := expand(w.cfg.PR.Title, vars)
	if strings.TrimSpace(title) == "" {
		title = strings.TrimSpace(msg.Subject)
	}
	body := expand(w.cfg.PR.Body, vars)
	if strings.TrimSpace(body) == "" {
		body = defaultPRBody(msg, w.cfg, revision)
	}
	return w.cfg.Provider.CreatePullRequest(ctx, PullRequestRequest{
		Slug:          slug,
		Head:          head,
		Base:          w.cfg.Target.Branch,
		Title:         title,
		Body:          body,
		Draft:         w.cfg.PR.Draft,
		Reviewers:     w.cfg.PR.Reviewers,
		TeamReviewers: w.cfg.PR.TeamReviewers,
	})
}

func defaultPRBody(msg Message, cfg Config, revision string) string {
	var b strings.Builder
	b.WriteString("Rendered by kelson.\n\n")
	fmt.Fprintf(&b, "- Project: `%s`\n", msg.Project)
	fmt.Fprintf(&b, "- Environment: `%s`\n", msg.Environment)
	fmt.Fprintf(&b, "- Path: `%s`\n", cfg.Target.Path)
	if msg.SpecHash != "" {
		fmt.Fprintf(&b, "- Spec hash: `%s`\n", msg.SpecHash)
	}
	if revision != "" {
		fmt.Fprintf(&b, "- Commit: `%s`\n", revision)
	}
	if cfg.Identity.IsAgent() {
		agent := cfg.Identity.AgentID
		if agent == "" {
			agent = "kelson agent"
		}
		fmt.Fprintf(&b, "\nProposed by agent `%s`", agent)
		if cfg.Identity.RequestedBy != "" {
			fmt.Fprintf(&b, " on behalf of `%s`", cfg.Identity.RequestedBy)
		}
		b.WriteString(". Merging this pull request is the human approval step.\n")
	}
	return b.String()
}

func (w *Writer) slug() (Slug, error) {
	if w.cfg.PR.Owner != "" && w.cfg.PR.Repo != "" {
		return Slug{Owner: w.cfg.PR.Owner, Name: w.cfg.PR.Repo}, nil
	}
	return ParseRepo(w.cfg.Target.Repo)
}

var branchUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// prBranchName derives a stable branch name, so re-delivering the same spec
// updates the same pull request instead of opening a second one.
func (w *Writer) prBranchName(msg Message) string {
	if w.cfg.PR.Branch != "" {
		return w.cfg.PR.Branch
	}
	parts := []string{}
	for _, p := range []string{msg.Project, msg.Environment, shortHash(msg.SpecHash)} {
		if s := strings.Trim(branchUnsafe.ReplaceAllString(strings.ToLower(p), "-"), "-"); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "update")
	}
	return w.cfg.PR.BranchPrefix + strings.Join(parts, "-")
}

// History returns kelson commits, newest first. kelson commits are identified
// by their trailers, so hand edits in the same repository are never mistaken
// for kelson history.
func (w *Writer) History(ctx context.Context, filter HistoryFilter) ([]delivery.Entry, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	auth, err := w.cfg.Auth.GitAuth()
	if err != nil {
		return nil, err
	}
	repo, empty, err := w.cloneOrInit(ctx, memfs.New(), auth)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, nil
	}
	head, err := repo.Reference(plumbing.NewBranchReferenceName(w.cfg.Target.Branch), true)
	if err != nil {
		return nil, wrap(err, "resolving branch "+w.cfg.Target.Branch)
	}
	iter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		return nil, wrap(err, "reading the commit log")
	}
	defer iter.Close()

	var out []delivery.Entry
	stop := errors.New("stop")
	err = iter.ForEach(func(c *object.Commit) error {
		subject, trailers := parseTrailers(c.Message)
		if trailers[TrailerSpecHash] == "" {
			return nil
		}
		if filter.Project != "" && trailers[TrailerProject] != filter.Project {
			return nil
		}
		if filter.Environment != "" && trailers[TrailerEnvironment] != filter.Environment {
			return nil
		}
		out = append(out, delivery.Entry{
			Revision:    c.Hash.String(),
			SpecHash:    trailers[TrailerSpecHash],
			CommittedAt: c.Author.When.UTC().Format(time.RFC3339),
			Message:     subject,
			Author:      fmt.Sprintf("%s <%s>", c.Author.Name, c.Author.Email),
		})
		if len(out) >= limit {
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return nil, wrap(err, "reading the commit log")
	}
	return out, nil
}

// FilesAt returns the files kelson manages as of a revision, addressed
// relative to the configured path. Rollback is "write these files again as a
// new commit" — never a force-push over history.
func (w *Writer) FilesAt(ctx context.Context, revision string) ([]File, error) {
	auth, err := w.cfg.Auth.GitAuth()
	if err != nil {
		return nil, err
	}
	repo, empty, err := w.cloneOrInit(ctx, memfs.New(), auth)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, wrap(errors.New("repository is empty"), "reading revision "+revision)
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		e := delivery.ApplyFailed("git/read", "revision",
			fmt.Sprintf("revision %q does not exist in the deployment repository", revision),
			"pick a revision from history that kelson wrote")
		e.Cause = err.Error()
		return nil, e
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, wrap(err, "reading commit "+revision)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, wrap(err, "reading the tree of "+revision)
	}

	prefix := w.cfg.Target.Path
	var out []File
	err = tree.Files().ForEach(func(f *object.File) error {
		if !underPath(f.Name, prefix) {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		rel := f.Name
		if prefix != "" {
			rel = strings.TrimPrefix(rel, prefix+"/")
		}
		out = append(out, File{Path: rel, Data: []byte(content)})
		return nil
	})
	if err != nil {
		return nil, wrap(err, "reading the files of "+revision)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// resolve maps a request-relative file path to a repository path, refusing
// anything that would escape the configured path. This is the enforcement
// point for "kelson writes only within its configured path".
func (w *Writer) resolve(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", pathErr(rel, "rendered file has an empty path")
	}
	if strings.HasPrefix(rel, "/") {
		return "", pathErr(rel, "rendered file path is absolute")
	}
	joined := path.Join(w.cfg.Target.Path, rel)
	cleaned := path.Clean(joined)
	if cleaned == "." || cleaned == "/" {
		return "", pathErr(rel, "rendered file path does not name a file")
	}
	if !underPath(cleaned, w.cfg.Target.Path) {
		return "", pathErr(rel, "rendered file path escapes the configured delivery path")
	}
	return cleaned, nil
}

// underPath reports whether repoPath lies inside prefix (or anywhere, when the
// prefix is the repository root).
func underPath(repoPath, prefix string) bool {
	if prefix == "" {
		return true
	}
	return repoPath == prefix || strings.HasPrefix(repoPath, prefix+"/")
}

// SecretsDir is the subdirectory of the delivery path holding the encrypted
// Secrets of the `sops` backend (issue #81, ADR-0022):
// `<delivery path>/secrets/<name>.enc.yaml`.
//
// It is inside the delivery path because that is what makes the mechanism
// work: Flux's kustomize-controller walks the path it reconciles recursively,
// so a file here is applied by the same Kustomization as the workloads that
// reference it, decrypted by that Kustomization's `spec.decryption`, and
// pruned by it when the file goes. Putting the encrypted Secrets outside the
// path would need a second Kustomization, a second decryption block and a
// second thing to remember to delete.
//
// It is a *subdirectory* rather than a file-name convention so that "what a
// deploy owns" and "what a secret write owns" are two directories rather than
// two glob patterns over one, which is the difference between a prune rule a
// reader can check and one they have to trust.
const SecretsDir = "secrets"

// preserved reports whether a file under the configured path must survive a
// [Session.Stage] prune.
//
// Two kinds of file are not a deploy's to remove. Dotfiles are repository
// conventions (.sops.yaml, .gitkeep, .gitattributes) that kelson does not own
// even inside its own directory. And everything under [SecretsDir] is written
// by `kelson secret set` and is invisible to a render — the renderer has no
// plaintext and emits no Secret — so a deploy that pruned it would delete
// every credential in the environment on the next commit.
func (w *Writer) preserved(repoPath string) bool {
	if strings.HasPrefix(path.Base(repoPath), ".") {
		return true
	}
	return underPath(repoPath, w.secretsPrefix())
}

// secretsPrefix is [SecretsDir] resolved against the configured delivery path.
func (w *Writer) secretsPrefix() string {
	return path.Join(w.cfg.Target.Path, SecretsDir)
}

// SecretPath is where the encrypted form of the named Secret lives, relative
// to the configured delivery path — the argument [Writer.Put] and
// [Writer.Delete] take.
//
// It is a function rather than a format string in three places because the
// writer, the reader and the prune rule must agree byte for byte: a `set` that
// wrote one spelling and a `delete` that looked for another would leave a
// credential in Git that kelson reports as gone.
func SecretPath(name string) string { return path.Join(SecretsDir, name+".enc.yaml") }

// SecretName is [SecretPath] read backwards: the Secret a repository path
// names, or ok=false for a path that is not one of kelson's encrypted Secrets.
func SecretName(repoPath string) (string, bool) {
	base := path.Base(repoPath)
	name, ok := strings.CutSuffix(base, ".enc.yaml")
	if !ok || name == "" || path.Base(path.Dir(repoPath)) != SecretsDir {
		return "", false
	}
	return name, true
}

func cleanPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", configErr("path", fmt.Sprintf("delivery path %q escapes the repository", p),
			"set delivery.git.path to a directory inside the repository")
	}
	return cleaned, nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func hashString(h plumbing.Hash, ok bool) string {
	if !ok {
		return "(branch absent)"
	}
	return h.String()
}

func isNonFastForward(err error) bool {
	if errors.Is(err, gogit.ErrForceNeeded) || errors.Is(err, gogit.ErrNonFastForwardUpdate) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "required to be") ||
		strings.Contains(msg, "fetch first")
}

func conflictErr(t Target, branch, expected, actual string) error {
	e := delivery.Conflict("git/write", "branch",
		fmt.Sprintf("%s branch %q moved while kelson was rendering (expected %s, found %s)",
			t.Repo, branch, shortHash(expected), shortHash(actual)),
		"re-run the deployment: kelson re-reads the branch and rebuilds the change on top of it. "+
			"kelson never force-pushes over a concurrent change")
	return e
}

func configErr(field, msg, remediation string) error {
	return delivery.ApplyFailed("git/config", field, msg, remediation)
}

func pathErr(rel, msg string) error {
	e := delivery.ApplyFailed("git/write", "files", msg,
		"kelson only writes within delivery.git.path; this is a bug in the caller if it persists")
	e.Cause = rel
	return e
}

func wrap(err error, doing string) error {
	e := delivery.ApplyFailed("git/write", "", "failed while "+doing,
		"check the repository state and the delivery credential")
	e.Cause = err.Error()
	return e
}

func ptr[T any](v T) *T { return &v }
