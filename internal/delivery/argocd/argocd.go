// Package argocd implements the Argo CD delivery adapter (issue #35): kelson
// commits rendered manifests to a deployment repository and Argo CD does the
// apply. kelson does not own the apply step, so the adapter's job is to write,
// to make the sync happen now rather than at Argo's poll interval, and to read
// Argo's own state back to answer "is my change live" (issue #37).
//
// Like the flux adapter it is deliberately thin over three building blocks:
//
//   - the git writer (internal/delivery/git) performs the commit, in commit or
//     pull-request mode, with the read-modify-write conflict guarantee (issue
//     #40);
//   - a Syncer triggers an immediate sync of the Argo Application;
//   - an ApplicationReader reads the Application back so status.go can map
//     Argo's computed sync+health onto the delivery phase machine.
//
// Everything talks to Argo over its REST API (net/http + encoding/json). Argo
// CD is reachable as an HTTP service, so the adapter needs no Kubernetes client
// and no cluster credential — only a server URL and an API token.
//
// # How kelson applications map to Argo Applications
//
// One Argo Application per kelson project/environment, named deterministically
// as "{project}-{environment}" (ApplicationName). The alternative — generating
// an ApplicationSet and letting Argo fan out — was rejected: it would make
// kelson own Argo's configuration, which is exactly the "works against an
// existing Argo installation without reconfiguring it" property #35 asks for.
// kelson reads the Application that the operator already created; it never
// creates, patches or templates one. An install whose naming differs sets
// Options.Application explicitly.
//
// The consequence is a real misconfiguration check: the Application kelson
// targets must exist, and its spec.source must actually cover what kelson
// writes (same repository, a path at or above the delivery path, and a
// targetRevision that tracks the delivery branch). When it does not, Apply
// fails with delivery/not-watched rather than leaving a commit that nothing
// will ever apply — the same guarantee the flux adapter gives (issue #34).
//
// # Argo projects and RBAC
//
// Options.Project is the Argo AppProject the target Application is expected to
// belong to (default "default"). It is a guard, not a creation step: an
// Application found under a different project is reported as a configuration
// error rather than synced, because syncing someone else's Application is not
// something kelson should do by accident.
//
// The API token must be allowed to read and sync that Application. In Argo RBAC
// terms the minimum is:
//
//	p, role:kelson, applications, get,  <project>/<app>, allow
//	p, role:kelson, applications, sync, <project>/<app>, allow
//
// A 401/403 from Argo is reported as delivery/apply-failed naming the token and
// the two policy lines above, so the fix is in the error rather than in a wiki.
//
// # Auto-sync and manual sync
//
// Both are supported, and the adapter behaves the same way in both: after a
// successful commit it POSTs the sync. With auto-sync enabled the commit alone
// would eventually be picked up by Argo's poll (~3 minutes by default), so the
// explicit sync only removes latency; with manual sync it is what makes the
// change live at all. Whether the target Application has auto-sync enabled is
// read from spec.syncPolicy.automated and reported in Status detail
// ("autoSync"), because it changes what happens if kelson's sync call fails:
// auto-sync installs still converge, manual-sync installs stay committed until
// someone syncs.
//
// In pull-request mode the commit lands on a kelson branch, not on the branch
// Argo tracks, so no sync is triggered and Apply reports Applied=false. Syncing
// an unmerged revision would defeat the point of the approval gate (issue #8).
package argocd

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/git"
)

// DefaultProject is the Argo AppProject kelson expects when none is configured.
const DefaultProject = "default"

// Adapter is the argocd-mode delivery adapter.
type Adapter struct {
	writer  *git.Writer
	syncer  Syncer
	reader  ApplicationReader
	project string
	appName string
}

// Options configures the argocd adapter.
type Options struct {
	// Writer is the git writer configuration: Target (repo/branch/path),
	// Mode (commit or pull-request), Identity, Auth, PR and Now.
	Writer git.Config
	// API configures the Argo CD REST client used for both status readback and
	// the sync trigger. It is consulted only when Reader (or Syncer) is nil, so
	// tests and alternative transports can inject their own.
	API ClientConfig
	// Reader reads Argo Applications back. Defaults to a client built from API.
	Reader ApplicationReader
	// Syncer triggers the sync after a commit. Defaults to the same client;
	// NoopSyncer disables the trigger and leaves delivery to Argo's poll.
	Syncer Syncer
	// Project is the Argo AppProject the target Application must belong to.
	// Defaults to DefaultProject.
	Project string
	// Application pins the Argo Application name instead of deriving it from
	// the project/environment (ApplicationName).
	Application string
}

// New validates and returns the argocd adapter.
func New(opts Options) (*Adapter, error) {
	if opts.Project == "" {
		opts.Project = DefaultProject
	}
	if opts.Reader == nil || opts.Syncer == nil {
		if opts.API.BaseURL == "" && opts.Reader == nil {
			return nil, delivery.ApplyFailed("argocd/config", "server",
				"the argocd adapter needs an Argo CD API server",
				"set the Argo CD base URL and API token, or inject an ApplicationReader")
		}
		if opts.API.BaseURL != "" {
			c, err := NewClient(opts.API)
			if err != nil {
				return nil, err
			}
			if opts.Reader == nil {
				opts.Reader = c
			}
			if opts.Syncer == nil {
				opts.Syncer = c
			}
		}
	}
	if opts.Syncer == nil {
		opts.Syncer = NoopSyncer{}
	}
	w, err := git.New(opts.Writer)
	if err != nil {
		return nil, err
	}
	return &Adapter{
		writer:  w,
		syncer:  opts.Syncer,
		reader:  opts.Reader,
		project: opts.Project,
		appName: opts.Application,
	}, nil
}

// RegisterArgoCD builds an argocd adapter from opts and registers it in reg
// under the name "argocd". It returns the adapter so callers can reuse it
// directly.
func RegisterArgoCD(reg *delivery.Registry, opts Options) (*Adapter, error) {
	a, err := New(opts)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(a); err != nil {
		return nil, err
	}
	return a, nil
}

// Name implements delivery.Adapter.
func (a *Adapter) Name() string { return "argocd" }

// Capabilities implements delivery.Adapter. Argo mode rides on a git
// repository, can deliver through a pull request (the approval gate for agent
// action, issue #8), and rolls back by committing the previous revision's
// files — never by force-pushing history away.
func (a *Adapter) Capabilities() delivery.Capabilities {
	return delivery.Capabilities{
		RequiresGit:      true,
		SupportsPR:       true,
		SupportsRollback: true,
	}
}

var nameUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// ApplicationName is the deterministic Argo Application name for a kelson
// project/environment: "{project}-{environment}", lowercased and reduced to the
// character set Kubernetes object names allow. Determinism is what lets kelson
// find the Application again on the next deployment without storing a mapping.
func ApplicationName(project, environment string) string {
	parts := make([]string, 0, 2)
	for _, p := range []string{project, environment} {
		if s := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(p), "-"), "-"); s != "" {
			parts = append(parts, s)
		}
	}
	name := strings.Join(parts, "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

func (a *Adapter) nameFor(set delivery.ManifestSet) string {
	if a.appName != "" {
		return a.appName
	}
	return ApplicationName(set.Project, set.Environment)
}

// Apply implements delivery.Adapter: commit the rendered output to the
// configured repository and path, then sync the Argo Application covering that
// path so perceived latency matches direct mode.
//
// The commit and the sync are separate concerns reported separately: a
// successful commit that could not be synced returns a delivery/apply-failed
// whose message states the change is committed (sync.go, syncErr). Writing
// somewhere no Argo Application tracks is a delivery/not-watched error —
// misconfiguration is reported, never left hanging as a deployment that
// silently never arrives.
func (a *Adapter) Apply(ctx context.Context, set delivery.ManifestSet) (delivery.Result, error) {
	res, err := a.writer.Write(ctx, git.WriteRequest{
		Files: git.ManifestFiles(set),
		Message: git.Message{
			Subject:     fmt.Sprintf("kelson: update %s/%s", set.Project, set.Environment),
			Project:     set.Project,
			Environment: set.Environment,
			SpecHash:    set.SpecHash,
			Revision:    set.Revision,
		},
	})
	if err != nil {
		return delivery.Result{}, err
	}
	return a.deliver(ctx, set, "argocd/apply", res)
}

// Rollback implements delivery.Adapter: read the files as of the target
// revision and commit them again as a forward change — never a force-push —
// then sync, exactly like Apply.
func (a *Adapter) Rollback(ctx context.Context, set delivery.ManifestSet, to delivery.Entry) (delivery.Result, error) {
	files, err := a.writer.FilesAt(ctx, to.Revision)
	if err != nil {
		return delivery.Result{}, err
	}
	if len(files) == 0 {
		return delivery.Result{}, delivery.ApplyFailed("argocd/rollback", "revision",
			fmt.Sprintf("revision %q contains no files under the configured path", to.Revision),
			"pick a revision from history that kelson wrote")
	}
	from := set.Revision
	set.Revision = to.Revision
	set.SpecHash = to.SpecHash
	res, err := a.writer.Write(ctx, git.WriteRequest{
		Files: files,
		Message: git.Message{
			Subject:     fmt.Sprintf("kelson: rollback %s/%s to %s", set.Project, set.Environment, short(to.Revision)),
			Project:     set.Project,
			Environment: set.Environment,
			SpecHash:    to.SpecHash,
			Revision:    to.Revision,
			Extra:       map[string]string{"Kelson-Rollback-From": from},
		},
	})
	if err != nil {
		return delivery.Result{}, err
	}
	return a.deliver(ctx, set, "argocd/rollback", res)
}

// deliver is the shared tail of Apply and Rollback: resolve the Application
// that must cover the write, then sync it.
//
// In pull-request mode the commit is on a kelson branch that Argo does not
// track, so the sync is skipped and the result reports Applied=false: the last
// mile is the human merging the pull request. The Application is still resolved
// first, so a misconfigured target fails now rather than after the merge.
func (a *Adapter) deliver(ctx context.Context, set delivery.ManifestSet, where string, res git.WriteResult) (delivery.Result, error) {
	app, err := a.application(ctx, set, where)
	if err != nil {
		return delivery.Result{}, err
	}
	if a.writer.Mode() == git.ModePullRequest {
		return delivery.Result{Revision: res.Revision, Applied: false}, nil
	}
	if err := a.syncer.Sync(ctx, app, res.Revision); err != nil {
		return delivery.Result{}, err
	}
	return delivery.Result{Revision: res.Revision, Applied: true}, nil
}

// Status implements delivery.Adapter: it reads the Argo Application tracking
// the configured path and maps Argo's own computed sync and health status onto
// the phase machine. kelson never recomputes health — Argo owns health
// assessment, and duplicating it would produce two answers that disagree.
func (a *Adapter) Status(ctx context.Context, set delivery.ManifestSet) (delivery.Status, error) {
	app, err := a.application(ctx, set, "argocd/status")
	if err != nil {
		return delivery.Status{}, err
	}
	return phaseFor(app, set.Revision), nil
}

// History implements delivery.Adapter, reading kelson commits from the
// repository (the same uniform shape direct mode exposes from its journal).
func (a *Adapter) History(ctx context.Context, set delivery.ManifestSet) ([]delivery.Entry, error) {
	return a.writer.History(ctx, git.HistoryFilter{
		Project:     set.Project,
		Environment: set.Environment,
	})
}

// application resolves and validates the Argo Application for a manifest set:
// it must exist, belong to the configured Argo project, and its source must
// cover the repository, path and branch kelson writes to.
func (a *Adapter) application(ctx context.Context, set delivery.ManifestSet, where string) (Application, error) {
	if a.reader == nil {
		return Application{}, delivery.ApplyFailed(where, "reader",
			"the argocd adapter has no way to read Argo Applications",
			"configure the Argo CD API base URL and token so kelson can answer is-my-change-live")
	}
	name := a.nameFor(set)
	target := a.writer.Target()

	app, found, err := a.reader.Application(ctx, name)
	if err != nil {
		return Application{}, err
	}
	if !found {
		return Application{}, delivery.NotWatched(where, target.Path,
			fmt.Sprintf("no Argo CD Application named %q exists", name),
			fmt.Sprintf("create an Application named %q pointing at %s (branch %s, path %s), or set the delivery "+
				"application name to the existing one; the commit is on the branch but nothing will apply it",
				name, target.Repo, target.Branch, pathOrRoot(target.Path)))
	}
	if a.project != "" && app.Project != "" && app.Project != a.project {
		return Application{}, delivery.ApplyFailed(where, "project",
			fmt.Sprintf("Argo CD Application %q belongs to project %q, not the configured project %q",
				name, app.Project, a.project),
			"point the delivery project at the AppProject that owns this Application, and make sure the kelson "+
				"API token is allowed 'applications, get/sync' on it (argocd RBAC)")
	}
	if !app.covers(target.Repo, target.Path, target.Branch) {
		return Application{}, delivery.NotWatched(where, target.Path,
			fmt.Sprintf("Argo CD Application %q tracks %s (revision %s, path %s), but kelson writes %s (branch %s, path %s)",
				name, app.RepoURL, revisionOrHead(app.TargetRevision), pathOrRoot(app.Path),
				target.Repo, target.Branch, pathOrRoot(target.Path)),
			"point the Application's source at the delivery repository, branch and path — or point the delivery "+
				"target at what the Application already tracks; the commit is on the branch but nothing will apply it")
	}
	return app, nil
}

func pathOrRoot(p string) string {
	if p == "" {
		return "(repository root)"
	}
	return p
}

func revisionOrHead(r string) string {
	if r == "" {
		return "HEAD"
	}
	return r
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

var _ delivery.Adapter = (*Adapter)(nil)
