// Package eject implements `kelson eject --to-git` (issue #41): it replays a
// direct-mode environment's rendered history into a real Git repository and
// hands the environment over to a Git-mode adapter.
//
// # Why this package exists
//
// ADR-0001 claims hybrid delivery is ONE path and not two, and the sharpest
// form of that claim is: "direct mode is Git mode with an implicit
// repository". Eject is the executable proof. Direct mode already versions its
// rendered output in an append-only journal
// (internal/delivery/direct/history.go), so ejecting is a *replay*, not a
// migration — nothing is re-rendered, nothing is re-modelled, and the only
// spec change is the delivery stanza. The day eject needs to re-render to
// produce a usable repository is the day the two modes have drifted and
// ADR-0001 has failed. This package is built to keep them honest.
//
// # Guarantees
//
//  1. Chronology. Entries are replayed oldest first, one commit per recorded
//     revision, so `git log` reads exactly like the direct-mode journal.
//  2. Authorship. Every commit carries the author and CommittedAt recorded in
//     the journal. kelson is the committer — the tool that wrote the commit —
//     and the recorded actor is the author, the same split the git writer uses
//     (internal/delivery/git/identity.go).
//  3. Byte-identical tip. The final commit's tree under the delivery path is
//     byte-for-byte the rendered output recorded at the latest revision. This
//     is the acceptance criterion of issue #41: after ejecting, the
//     environment keeps running with no redeploy and no manifest changes.
//  4. Layout parity. Files are laid out by git.ManifestFiles — the same pure
//     function every Git-mode adapter writes through — so an ejected
//     repository is indistinguishable from one kelson had been committing to
//     all along, and the Flux/Argo adapters take it over without a diff.
//
// # Why go-git directly instead of git.Writer
//
// git.Writer is one read-modify-write cycle with one identity and one clock:
// exactly right for a deploy, wrong for a replay. Eject needs N commits with N
// distinct authors and N distinct timestamps, it must be able to record a
// revision whose rendered output did not change (a rollback to the current
// state is still a history entry, and dropping it would silently rewrite the
// past), and it should not clone the remote once per revision. So the replay
// drives go-git directly while reusing the writer's public vocabulary —
// git.Target, git.File, git.Message, git.ManifestFiles — so the bytes and the
// layout stay the writer's, not a second implementation of them.
package eject

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
	"github.com/dafrie/kelson/internal/delivery/git"
)

// Target delivery modes eject can switch an environment to. They are the
// Git-mode names from model.DeliveryMode; the eject package does not import
// the model so the delivery plane stays independent of the authoring plane.
const (
	ModeFlux   = "flux"
	ModeArgoCD = "argocd"
)

// TrailerEjectedFrom marks a replayed commit with the direct-mode revision it
// came from. A commit dated last Tuesday that was actually created during an
// eject must say so: the trailer is how history stays honest without lying
// about the author date.
const TrailerEjectedFrom = "Kelson-Ejected-From"

// Source is the rendered history eject replays. *direct.Store satisfies it.
//
// It is an interface, and deliberately the mode-independent half of the store:
// eject consumes recorded revisions and recorded bytes, never direct-mode
// internals. That is the same constraint the CLI/UI see through
// delivery.Adapter.History, which is why replaying needs no special access.
type Source interface {
	// Entries returns the recorded revisions, newest first.
	Entries(project, environment string) ([]delivery.Entry, error)
	// Rendered returns the rendered output recorded at one revision, verbatim
	// and in apply order.
	Rendered(project, environment, revision string) ([]byte, error)
}

// Options configures an Ejector.
type Options struct {
	// Project and Environment identify the direct-mode environment to eject.
	Project     string
	Environment string

	// History is the direct-mode rendered-history store to replay.
	History Source

	// Target is the repository the history is replayed into: repo URL (or a
	// local path), branch, and the path within the repository kelson will own.
	Target git.Target

	// Mode is the delivery mode the environment switches to: ModeFlux or
	// ModeArgoCD.
	Mode string

	// Auth authenticates clone and push. Nil means git.Anonymous, which is
	// what local paths and public remotes need.
	Auth git.Auth

	// Identity attributes commits whose history entry recorded no author.
	// It is a fallback only: a recorded author always wins.
	Identity git.Identity

	// Bootstrap configures the reconciler manifests emitted alongside the
	// replay.
	Bootstrap BootstrapOptions

	// Now is injectable so tests are deterministic. It is only consulted for
	// entries with an unusable CommittedAt.
	Now func() time.Time

	// AfterClone runs immediately after the target branch has been read and
	// before the replay starts. It is a test seam: it gives a competing writer
	// a window in which to move the remote between the read and the push, the
	// only way to exercise the compare-and-swap (issue #40) without racing
	// goroutines.
	AfterClone func()
}

// Commit is one planned commit: one history entry, laid out as repository
// files with the attribution it will be committed under.
type Commit struct {
	// Revision is the direct-mode revision this commit replays.
	Revision string
	SpecHash string

	// Subject is the commit subject; Message is the full message including
	// kelson's machine-readable trailers.
	Subject string
	Message string

	// AuthorName/AuthorEmail are the recorded author of the entry.
	AuthorName  string
	AuthorEmail string
	// CommittedAt is the entry's recorded wall-clock time, used as both the
	// author and the committer date so `git log` matches `kelson history`.
	CommittedAt time.Time

	// Files are repository-root-relative, already under Target.Path.
	Files []git.File
}

// Plan is the complete, side-effect-free description of an eject: every commit
// that would be made, in order, plus the reconciler configuration that would
// be emitted. `--dry-run` prints a Plan and stops.
type Plan struct {
	Project     string
	Environment string
	Mode        string
	Target      git.Target

	// Commits are oldest first, so the final one leaves the repository at
	// exactly the latest recorded revision.
	Commits []Commit

	// Bootstrap is the Flux/Argo configuration that makes the ejected
	// repository actually apply. It is nil when bootstrap emission is off.
	Bootstrap *Bootstrap
}

// Tip returns the last planned commit — the one whose tree must be
// byte-identical to the latest recorded revision.
func (p Plan) Tip() Commit {
	if len(p.Commits) == 0 {
		return Commit{}
	}
	return p.Commits[len(p.Commits)-1]
}

// Result reports what an eject actually wrote.
type Result struct {
	// Commits is the number of commits created.
	Commits int
	// Head is the sha of the final commit.
	Head string
	// Branch is the branch the commits landed on.
	Branch string
	// TipRevision is the direct-mode revision the repository now holds.
	TipRevision string
}

// Ejector replays one environment's rendered history into a repository.
type Ejector struct {
	opts Options
}

// New validates the options and returns an Ejector.
func New(opts Options) (*Ejector, error) {
	switch {
	case strings.TrimSpace(opts.Project) == "":
		return nil, configErr("project", "eject needs a project name",
			"pass the project the environment belongs to")
	case strings.TrimSpace(opts.Environment) == "":
		return nil, configErr("environment", "eject needs an environment name",
			"pass the environment to eject")
	case opts.History == nil:
		return nil, configErr("history", "eject needs the direct-mode rendered history store",
			"point --history at the kelson data directory holding the rendered history")
	}
	switch opts.Mode {
	case "":
		opts.Mode = ModeFlux
	case ModeFlux, ModeArgoCD:
	default:
		return nil, configErr("mode", fmt.Sprintf("unknown target delivery mode %q", opts.Mode),
			"eject switches an environment to "+ModeFlux+" or "+ModeArgoCD)
	}
	if strings.TrimSpace(opts.Target.Repo) == "" {
		return nil, configErr("repo", "eject needs a target repository",
			"pass --to-git <repository> — the git remote the rendered history is replayed into")
	}
	if opts.Target.Branch == "" {
		opts.Target.Branch = git.DefaultBranch
	}
	clean, err := cleanPath(opts.Target.Path)
	if err != nil {
		return nil, err
	}
	opts.Target.Path = clean
	if opts.Auth == nil {
		opts.Auth = git.Anonymous{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Ejector{opts: opts}, nil
}

// Target returns the resolved target.
func (e *Ejector) Target() git.Target { return e.opts.Target }

// Plan reads the rendered history and builds the commit sequence. It touches
// no repository and no spec file: it is the whole of `--dry-run`, and it is
// what makes the replay unit-testable without a remote.
func (e *Ejector) Plan() (Plan, error) {
	entries, err := e.opts.History.Entries(e.opts.Project, e.opts.Environment)
	if err != nil {
		return Plan{}, err
	}
	if len(entries) == 0 {
		return Plan{}, delivery.ApplyFailed(e.ref(), "history",
			fmt.Sprintf("no rendered history is recorded for %s", e.ref()),
			"deploy the environment at least once in direct mode before ejecting: "+
				"eject replays recorded revisions, it never re-renders")
	}

	plan := Plan{
		Project:     e.opts.Project,
		Environment: e.opts.Environment,
		Mode:        e.opts.Mode,
		Target:      e.opts.Target,
		Commits:     make([]Commit, 0, len(entries)),
	}
	// Entries arrive newest first. Replaying oldest first is what makes the
	// final tip the latest revision rather than the earliest.
	for i := len(entries) - 1; i >= 0; i-- {
		c, err := e.commitFor(entries[i])
		if err != nil {
			return Plan{}, err
		}
		plan.Commits = append(plan.Commits, c)
	}

	bootstrap, err := e.bootstrap()
	if err != nil {
		return Plan{}, err
	}
	plan.Bootstrap = bootstrap
	return plan, nil
}

// commitFor turns one history entry into a planned commit. The rendered bytes
// are passed through verbatim — split into documents only so the layout
// function can name the files — and are never re-encoded.
func (e *Ejector) commitFor(entry delivery.Entry) (Commit, error) {
	rendered, err := e.opts.History.Rendered(e.opts.Project, e.opts.Environment, entry.Revision)
	if err != nil {
		return Commit{}, err
	}
	manifests, err := direct.SplitDocuments(rendered)
	if err != nil {
		return Commit{}, delivery.ApplyFailed(e.ref(), "history",
			fmt.Sprintf("the rendered output recorded at %s is unreadable: %v", entry.Revision, err),
			"inspect the history data directory; the recorded revision cannot be replayed")
	}
	if len(manifests) == 0 {
		return Commit{}, delivery.ApplyFailed(e.ref(), "history",
			fmt.Sprintf("the revision %s recorded no rendered documents", entry.Revision),
			"inspect the history data directory; a revision with no output cannot be replayed")
	}

	set := delivery.ManifestSet{
		Project:     e.opts.Project,
		Environment: e.opts.Environment,
		SpecHash:    entry.SpecHash,
		Revision:    entry.Revision,
		Manifests:   manifests,
	}
	files := git.ManifestFiles(set)
	repoFiles := make([]git.File, 0, len(files))
	for _, f := range files {
		full, err := e.resolve(f.Path)
		if err != nil {
			return Commit{}, err
		}
		repoFiles = append(repoFiles, git.File{Path: full, Data: f.Data})
	}

	subject := strings.TrimSpace(entry.Message)
	if subject == "" {
		subject = fmt.Sprintf("kelson: %s/%s at %s", e.opts.Project, e.opts.Environment, entry.Revision)
	}
	msg := git.Message{
		Subject:     subject,
		Project:     e.opts.Project,
		Environment: e.opts.Environment,
		SpecHash:    entry.SpecHash,
		Revision:    entry.Revision,
		// The renderer version is deliberately not stamped: these manifests
		// were rendered by whichever kelson was live at the time, and claiming
		// the ejecting binary's version would be a lie in the audit trail.
		Extra: map[string]string{TrailerEjectedFrom: "direct/" + entry.Revision},
	}

	name, email := e.authorOf(entry)
	return Commit{
		Revision:    entry.Revision,
		SpecHash:    entry.SpecHash,
		Subject:     subject,
		Message:     msg.String(),
		AuthorName:  name,
		AuthorEmail: email,
		CommittedAt: e.committedAt(entry),
		Files:       repoFiles,
	}, nil
}

// authorOf resolves the commit author: the recorded actor when the journal has
// one, the configured identity otherwise. Attribution is never invented — an
// entry with no recorded author becomes a kelson-authored commit rather than
// being attributed to whoever happens to be running the eject.
func (e *Ejector) authorOf(entry delivery.Entry) (name, email string) {
	if n, m, ok := parseAuthor(entry.Author); ok {
		return n, m
	}
	id := e.opts.Identity
	if id.IsAgent() {
		agent := git.DefaultAgentName
		mail := git.DefaultAgentEmail
		if id.AgentID != "" {
			agent = git.DefaultAgentName + " " + id.AgentID
			mail = id.AgentID + "@agents.kelson.dev"
		}
		return agent, mail
	}
	name, email = id.Name, id.Email
	if name == "" {
		name = git.DefaultCommitName
	}
	if email == "" {
		email = git.DefaultCommitEmail
	}
	return name, email
}

// parseAuthor accepts "Name <email>", a bare email, or a bare name.
func parseAuthor(s string) (name, email string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if open := strings.LastIndex(s, "<"); open >= 0 && strings.HasSuffix(s, ">") {
		name = strings.TrimSpace(s[:open])
		email = strings.TrimSpace(s[open+1 : len(s)-1])
		if name == "" {
			name = email
		}
		return name, email, true
	}
	if strings.Contains(s, "@") {
		return s, s, true
	}
	return s, "", true
}

// committedAt reads the entry's recorded time. An entry whose timestamp is
// missing or unparseable falls back to the clock rather than to the zero time:
// a commit dated year 1 is worse than a commit dated now, and the revision
// order is carried by the replay order regardless.
func (e *Ejector) committedAt(entry delivery.Entry) time.Time {
	if entry.CommittedAt != "" {
		if t, err := time.Parse(time.RFC3339, entry.CommittedAt); err == nil {
			return t.UTC()
		}
	}
	return e.opts.Now().UTC()
}

// Run plans the eject and writes it: clone (or initialise) the target, replay
// every commit in order, and push once. The push is a compare-and-swap against
// the HEAD that was read, so an eject never force-pushes over a repository
// somebody else is writing to (issue #40).
func (e *Ejector) Run(ctx context.Context) (Result, error) {
	plan, err := e.Plan()
	if err != nil {
		return Result{}, err
	}
	return e.Write(ctx, plan)
}

// Write replays an already-built Plan. It is separate from Run so a caller can
// show the plan, confirm, and then execute exactly what was shown.
func (e *Ejector) Write(ctx context.Context, plan Plan) (Result, error) {
	if len(plan.Commits) == 0 {
		return Result{}, delivery.ApplyFailed(e.ref(), "history",
			"the eject plan contains no commits",
			"build the plan with Plan(); an empty history cannot be ejected")
	}
	auth, err := e.opts.Auth.GitAuth()
	if err != nil {
		return Result{}, err
	}
	fs := memfs.New()
	repo, empty, err := e.cloneOrInit(ctx, fs, auth)
	if err != nil {
		return Result{}, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return Result{}, wrap(err, "opening the repository worktree")
	}

	var base plumbing.Hash
	if !empty {
		head, err := repo.Reference(plumbing.NewBranchReferenceName(e.opts.Target.Branch), true)
		if err != nil {
			return Result{}, wrap(err, "resolving branch "+e.opts.Target.Branch)
		}
		base = head.Hash()
	}
	if e.opts.AfterClone != nil {
		e.opts.AfterClone()
	}

	var head plumbing.Hash
	for _, c := range plan.Commits {
		if err := e.stage(repo, wt, fs, c.Files); err != nil {
			return Result{}, err
		}
		hash, err := wt.Commit(c.Message, &gogit.CommitOptions{
			Author:    &object.Signature{Name: c.AuthorName, Email: c.AuthorEmail, When: c.CommittedAt},
			Committer: &object.Signature{Name: git.DefaultCommitName, Email: git.DefaultCommitEmail, When: c.CommittedAt},
			// A revision whose rendered output is unchanged (a rollback to the
			// state already live, a re-deploy of the same spec) is still a
			// history entry. Dropping it would silently rewrite the past, so
			// the replay keeps the empty commit and history stays 1:1.
			AllowEmptyCommits: true,
		})
		if err != nil {
			return Result{}, wrap(err, "replaying revision "+c.Revision)
		}
		head = hash
	}

	if err := e.push(ctx, repo, auth, base, empty); err != nil {
		return Result{}, err
	}
	return Result{
		Commits:     len(plan.Commits),
		Head:        head.String(),
		Branch:      e.opts.Target.Branch,
		TipRevision: plan.Tip().Revision,
	}, nil
}

// cloneOrInit reads the target branch, initialising a repository when the
// remote is empty. A fresh, empty deployment repository is the normal case for
// an eject — that is exactly what a user creates before running it.
func (e *Ejector) cloneOrInit(ctx context.Context, fs billy.Filesystem, auth transport.AuthMethod) (*gogit.Repository, bool, error) {
	branchRef := plumbing.NewBranchReferenceName(e.opts.Target.Branch)
	repo, err := gogit.CloneContext(ctx, memory.NewStorage(), fs, &gogit.CloneOptions{
		URL:           e.opts.Target.Repo,
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
		if _, initErr = repo.CreateRemote(&config.RemoteConfig{Name: gogit.DefaultRemoteName, URLs: []string{e.opts.Target.Repo}}); initErr != nil {
			return nil, false, wrap(initErr, "configuring the remote for the empty deployment repository")
		}
		return repo, true, nil
	default:
		ejectErr := delivery.ApplyFailed("eject/repo", "repo",
			fmt.Sprintf("could not read %s at branch %q", e.opts.Target.Repo, e.opts.Target.Branch),
			"create the deployment repository (an empty one is fine) and check the branch name and the credential")
		ejectErr.Cause = err.Error()
		return nil, false, ejectErr
	}
}

// stage writes one revision's files and removes the files kelson wrote for the
// previous revision that this one no longer declares — both strictly within
// the configured path. A resource deleted between two direct-mode revisions
// must be deleted in the replayed commit too, or the tip would not match.
//
// The path confinement and the dotfile exemption mirror git.Session.Stage
// deliberately rather than by import: the rule ("kelson writes only within its
// configured path, and never owns dotfiles inside it") is a guarantee of the
// delivery plane, so each writer states and enforces it for itself.
func (e *Ejector) stage(repo *gogit.Repository, wt *gogit.Worktree, fs billy.Filesystem, files []git.File) error {
	want := make(map[string][]byte, len(files))
	for _, f := range files {
		if _, dup := want[f.Path]; dup {
			return configErr("files", fmt.Sprintf("two rendered files resolve to the same path %q", f.Path),
				"this is a renderer bug: file names within a manifest set must be unique")
		}
		want[f.Path] = f.Data
	}

	tracked, err := e.trackedUnderPath(repo)
	if err != nil {
		return err
	}
	stale := make([]string, 0, len(tracked))
	for _, p := range tracked {
		if _, keep := want[p]; keep {
			continue
		}
		if preserved(p) {
			continue
		}
		stale = append(stale, p)
	}
	sort.Strings(stale)
	for _, p := range stale {
		if _, err := wt.Remove(p); err != nil {
			return wrap(err, "pruning "+p)
		}
	}

	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := writeFile(fs, p, want[p]); err != nil {
			return wrap(err, "writing "+p)
		}
		if _, err := wt.Add(p); err != nil {
			return wrap(err, "staging "+p)
		}
	}
	return nil
}

// trackedUnderPath lists the committed files under the configured path. It
// reads the HEAD tree rather than walking the filesystem, so nothing outside
// the path is ever enumerated, let alone removed. An unborn HEAD (the first
// commit into an empty repository) is an empty listing, not an error.
func (e *Ejector) trackedUnderPath(repo *gogit.Repository) ([]string, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, nil //nolint:nilerr // an unborn branch simply has no tracked files
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, wrap(err, "reading the HEAD commit")
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, wrap(err, "reading the HEAD tree")
	}
	var out []string
	err = tree.Files().ForEach(func(f *object.File) error {
		if underPath(f.Name, e.opts.Target.Path) {
			out = append(out, f.Name)
		}
		return nil
	})
	if err != nil {
		return nil, wrap(err, "listing the files kelson manages")
	}
	return out, nil
}

// push sends the replayed history in one shot, demanding the remote branch is
// still where it was read. kelson never force-pushes, and that holds for eject:
// a repository that moved under us is a conflict the user must see.
func (e *Ejector) push(ctx context.Context, repo *gogit.Repository, auth transport.AuthMethod, base plumbing.Hash, empty bool) error {
	branch := e.opts.Target.Branch
	refspec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))
	opts := &gogit.PushOptions{
		RemoteName: gogit.DefaultRemoteName,
		Auth:       auth,
		RefSpecs:   []config.RefSpec{refspec},
		Force:      false,
	}
	if !empty && !base.IsZero() {
		opts.RequireRemoteRefs = []config.RefSpec{
			config.RefSpec(fmt.Sprintf("%s:refs/heads/%s", base.String(), branch)),
		}
	}
	err := repo.PushContext(ctx, opts)
	switch {
	case err == nil, errors.Is(err, gogit.NoErrAlreadyUpToDate):
		return nil
	case isNonFastForward(err):
		return delivery.Conflict("eject/push", "branch",
			fmt.Sprintf("%s branch %q moved while kelson was replaying the rendered history", e.opts.Target.Repo, branch),
			"re-run `kelson eject`: it re-reads the branch and replays on top of it. kelson never force-pushes")
	default:
		pushErr := delivery.ApplyFailed("eject/push", "push",
			"pushing the replayed history failed",
			"check write access to the branch and any branch protection rules")
		pushErr.Cause = err.Error()
		return pushErr
	}
}

func (e *Ejector) ref() string { return e.opts.Project + "/" + e.opts.Environment }

// resolve maps a layout-relative file name to a repository path, refusing
// anything that would escape the configured path.
func (e *Ejector) resolve(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", configErr("files", "rendered file has an empty path",
			"this is a bug in the manifest layout function")
	}
	cleaned := path.Clean(path.Join(e.opts.Target.Path, rel))
	if cleaned == "." || cleaned == "/" || !underPath(cleaned, e.opts.Target.Path) {
		return "", configErr("files", fmt.Sprintf("rendered file path %q escapes the configured delivery path", rel),
			"kelson only writes within delivery.git.path")
	}
	return cleaned, nil
}

func underPath(repoPath, prefix string) bool {
	if prefix == "" {
		return true
	}
	return repoPath == prefix || strings.HasPrefix(repoPath, prefix+"/")
}

// preserved reports whether a file under the configured path survives pruning.
// Dotfiles are repository conventions (.sops.yaml, .gitkeep) that kelson does
// not own even inside its own directory.
func preserved(repoPath string) bool {
	return strings.HasPrefix(path.Base(repoPath), ".")
}

func writeFile(fs billy.Filesystem, p string, data []byte) error {
	if dir := path.Dir(p); dir != "." && dir != "/" {
		if err := fs.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return billyutil.WriteFile(fs, p, data, 0o644)
}

func cleanPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", configErr("path", fmt.Sprintf("delivery path %q escapes the repository", p),
			"set --path to a directory inside the repository")
	}
	return cleaned, nil
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

func configErr(field, msg, remediation string) error {
	return delivery.ApplyFailed("eject/config", field, msg, remediation)
}

func wrap(err error, doing string) error {
	e := delivery.ApplyFailed("eject", "", "failed while "+doing,
		"check the repository state and the delivery credential")
	e.Cause = err.Error()
	return e
}
