// Package flux implements the flux delivery adapter (issue #34): kelson
// commits rendered manifests to a deployment repository and Flux does the
// apply. kelson does not own the apply step, so the adapter's job is to write,
// to make reconciliation happen now rather than at the poll interval, and to
// read Flux's own state back to answer "is my change live" (issue #37).
//
// The adapter is deliberately thin over three building blocks in this package:
//
//   - the git writer (internal/delivery/git) performs the commit, in commit or
//     pull-request mode, with the read-modify-write conflict guarantee (issue
//     #40);
//   - a Reconciler triggers immediate reconciliation of the Kustomization(s)
//     covering the written path (webhook or flux CLI);
//   - a StatusReader turns Flux Kustomization/HelmRelease state into the
//     delivery.Phase state machine, keeping the three answers distinct:
//     not-picked-up-yet (Committed), rejected (Rejected), applied-but-unhealthy
//     (Degraded) — see statemachine docs.
package flux

import (
	"context"
	"fmt"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/git"
)

// Adapter is the flux-mode delivery adapter.
type Adapter struct {
	writer    *git.Writer
	reconcile Reconciler
	status    StatusReader
}

// Options configures the flux adapter.
type Options struct {
	// Writer is the git writer configuration: Target (repo/branch/path),
	// Mode (commit or pull-request), Identity, Auth, PR and Now.
	Writer git.Config
	// Reconciler triggers reconciliation after a commit. A Chain of
	// {webhook, CLI} is the typical value; NoopReconciler disables the trigger.
	Reconciler Reconciler
	// Status reads Flux objects back from the cluster.
	Status StatusReader
}

// New validates and returns the flux adapter.
func New(opts Options) (*Adapter, error) {
	if opts.Reconciler == nil {
		opts.Reconciler = NoopReconciler{}
	}
	if opts.Status == nil {
		return nil, delivery.ApplyFailed("flux/config", "status",
			"the flux adapter needs a status reader",
			"configure a CLIStatusReader or another StatusReader so kelson can answer is-my-change-live")
	}
	w, err := git.New(opts.Writer)
	if err != nil {
		return nil, err
	}
	return &Adapter{writer: w, reconcile: opts.Reconciler, status: opts.Status}, nil
}

// RegisterFlux builds a flux adapter from opts and registers it in reg under
// the name "flux". It returns the adapter so callers can reuse it directly.
func RegisterFlux(reg *delivery.Registry, opts Options) (*Adapter, error) {
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
func (a *Adapter) Name() string { return "flux" }

// Capabilities implements delivery.Adapter. Flux mode rides on a git
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

// Apply implements delivery.Adapter: commit the rendered output to the
// configured repository and path, then trigger immediate reconciliation of the
// Kustomization(s) covering that path so perceived latency matches direct mode.
//
// The commit and the trigger are separate concerns reported separately: a
// successful commit that could not be triggered returns a delivery/apply-failed
// whose message states the change is committed and will still arrive at the
// poll interval (reconcile.go, triggerErr). Writing somewhere no Kustomization
// watches is a delivery/not-watched error — misconfiguration is reported, never
// left hanging as a deployment that silently never arrives.
func (a *Adapter) Apply(ctx context.Context, set delivery.ManifestSet) (delivery.Result, error) {
	msg := git.Message{
		Subject:         fmt.Sprintf("kelson: update %s/%s", set.Project, set.Environment),
		Project:         set.Project,
		Environment:     set.Environment,
		SpecHash:        set.SpecHash,
		Revision:        set.Revision,
	}
	res, err := a.writer.Write(ctx, git.WriteRequest{
		Files:   git.ManifestFiles(set),
		Message: msg,
	})
	if err != nil {
		return delivery.Result{}, err
	}

	target := a.writer.Target()
	watched, err := a.covering(ctx, target.Repo, target.Path)
	if err != nil {
		return delivery.Result{}, err
	}
	if len(watched) == 0 {
		return delivery.Result{}, delivery.NotWatched(
			"flux/reconcile", target.Path,
			fmt.Sprintf("no Flux Kustomization covers path %q in repository %s", target.Path, target.Repo),
			"point the delivery target at a path a Kustomization reconciles, or create one; "+
				"the commit is on the branch but nothing will ever apply it")
	}

	for _, k := range watched {
		if err := a.reconcile.Reconcile(ctx, k); err != nil {
			return delivery.Result{}, err
		}
	}
	return delivery.Result{Revision: res.Revision, Applied: true}, nil
}

// covering returns the Kustomizations that reconcile the given repository and
// path. A Kustomization with an unknown source URL is matched on path alone
// (status.go, covers); when Flux resolved a source, the repository is compared
// so a Kustomization watching a different repo with a similar path is not
// mistaken for "watched".
func (a *Adapter) covering(ctx context.Context, repo, path string) ([]Kustomization, error) {
	ks, err := a.status.Kustomizations(ctx)
	if err != nil {
		return nil, err
	}
	var out []Kustomization
	for _, k := range ks {
		if k.covers(repo, path) {
			out = append(out, k)
		}
	}
	return out, nil
}

// Status implements delivery.Adapter: it reads the Kustomization(s) covering
// the configured path and maps their Ready/revision state onto the phase
// machine. A revision that Flux has not attempted yet maps to Committed (keep
// waiting), a rejected change to Rejected (with the Flux reason named), and an
// applied-but-unhealthy change to Degraded.
func (a *Adapter) Status(ctx context.Context, set delivery.ManifestSet) (delivery.Status, error) {
	target := a.writer.Target()
	ks, err := a.status.Kustomizations(ctx)
	if err != nil {
		return delivery.Status{}, err
	}
	watched := filterCovering(ks, target.Repo, target.Path)
	if len(watched) == 0 {
		return delivery.Status{}, delivery.NotWatched(
			"flux/status", target.Path,
			fmt.Sprintf("no Flux Kustomization covers path %q in repository %s", target.Path, target.Repo),
			"point the delivery target at a path a Kustomization reconciles, or create one")
	}
	// Prefer a Kustomization that has observed our revision; otherwise the
	// most specific path match.
	best := watched[0]
	for _, k := range watched {
		if revisionMatches(k.LastAppliedRevision, set.Revision) || revisionMatches(k.LastAttemptedRevision, set.Revision) {
			best = k
			break
		}
		if len(k.Path) > len(best.Path) {
			best = k
		}
	}
	return phaseFor(best, set.Revision), nil
}

func filterCovering(ks []Kustomization, repo, path string) []Kustomization {
	var out []Kustomization
	for _, k := range ks {
		if k.covers(repo, path) {
			out = append(out, k)
		}
	}
	return out
}

// History implements delivery.Adapter, reading kelson commits from the
// repository (the same uniform shape direct mode exposes from its journal).
func (a *Adapter) History(ctx context.Context, set delivery.ManifestSet) ([]delivery.Entry, error) {
	return a.writer.History(ctx, git.HistoryFilter{
		Project:     set.Project,
		Environment: set.Environment,
	})
}

// Rollback implements delivery.Adapter: read the files as of the target
// revision and commit them again as a forward change — never a force-push —
// then trigger reconciliation, exactly like Apply.
func (a *Adapter) Rollback(ctx context.Context, set delivery.ManifestSet, to delivery.Entry) (delivery.Result, error) {
	files, err := a.writer.FilesAt(ctx, to.Revision)
	if err != nil {
		return delivery.Result{}, err
	}
	if len(files) == 0 {
		return delivery.Result{}, delivery.ApplyFailed("flux/rollback", "revision",
			fmt.Sprintf("revision %q contains no files under the configured path", to.Revision),
			"pick a revision from history that kelson wrote")
	}
	set.Revision = to.Revision
	set.SpecHash = to.SpecHash
	msg := git.Message{
		Subject:     fmt.Sprintf("kelson: rollback %s/%s to %s", set.Project, set.Environment, short(to.Revision)),
		Project:     set.Project,
		Environment: set.Environment,
		SpecHash:    to.SpecHash,
		Revision:    to.Revision,
		Extra:       map[string]string{"Kelson-Rollback-From": set.Revision},
	}
	res, err := a.writer.Write(ctx, git.WriteRequest{Files: files, Message: msg})
	if err != nil {
		return delivery.Result{}, err
	}
	target := a.writer.Target()
	watched, err := a.covering(ctx, target.Repo, target.Path)
	if err != nil {
		return delivery.Result{}, err
	}
	if len(watched) == 0 {
		return delivery.Result{}, delivery.NotWatched(
			"flux/rollback", target.Path,
			fmt.Sprintf("no Flux Kustomization covers path %q in repository %s", target.Path, target.Repo),
			"the rollback commit is on the branch but nothing will apply it")
	}
	for _, k := range watched {
		if err := a.reconcile.Reconcile(ctx, k); err != nil {
			return delivery.Result{}, err
		}
	}
	return delivery.Result{Revision: res.Revision, Applied: true}, nil
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

var _ delivery.Adapter = (*Adapter)(nil)
