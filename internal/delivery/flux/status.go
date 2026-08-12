package flux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
)

// ConditionState mirrors a Kubernetes condition status.
type ConditionState string

const (
	ConditionTrue    ConditionState = "True"
	ConditionFalse   ConditionState = "False"
	ConditionUnknown ConditionState = "Unknown"
)

// Kustomization is the slice of a Flux Kustomization kelson reads. It is a
// plain struct, not a typed client object, so the status source stays an
// interface: kubectl/flux today, client-go or a watch cache later, a fake in
// tests. None of that changes the phase mapping below.
type Kustomization struct {
	Name      string
	Namespace string
	// Path is spec.path, the directory in the source repository this
	// Kustomization applies (e.g. "./clusters/prod").
	Path string
	// SourceKind/SourceName reference the GitRepository (or OCIRepository).
	SourceKind string
	SourceName string
	// SourceURL/SourceBranch are resolved from the source object when
	// available; they let kelson tell "watching my repo" from "watching a
	// different repo with a similar path".
	SourceURL    string
	SourceBranch string

	Suspended bool

	// Ready is the Ready condition with its reason and message.
	Ready         ConditionState
	Reason        string
	Message       string
	Reconciling   bool
	LastAppliedRevision   string
	LastAttemptedRevision string
}

// HelmRelease is the slice of a Flux HelmRelease kelson reads. Kustomization
// readiness alone does not answer "is my change live" for chart-based
// workloads: a Kustomization can be Ready while the HelmRelease it created is
// failing its upgrade.
type HelmRelease struct {
	Name      string
	Namespace string
	Ready     ConditionState
	Reason    string
	Message   string
}

// StatusReader reads Flux objects from the cluster. Implementations may use
// the Kubernetes API, the flux CLI or kubectl; tests use a fake.
type StatusReader interface {
	Kustomizations(ctx context.Context) ([]Kustomization, error)
	HelmReleases(ctx context.Context) ([]HelmRelease, error)
}

// Flux condition reasons kelson maps onto the delivery state machine. The
// distinction that matters (docs/delivery.md): a change the reconciler refused
// (Rejected) is a different user action from a change that is live and
// unhealthy (Degraded).
const (
	reasonBuildFailed      = "BuildFailed"
	reasonArtifactFailed   = "ArtifactFailed"
	reasonValidationFailed = "ValidationFailed"
	reasonReconcileFailed  = "ReconciliationFailed"
	reasonHealthCheckFail  = "HealthCheckFailed"
	reasonPruneFailed      = "PruneFailed"
	reasonDependencyNR     = "DependencyNotReady"
	reasonProgressing      = "Progressing"
)

// rejectedReasons are failures where Flux processed the change and refused it:
// the manifests never became live, so the user must fix the spec.
var rejectedReasons = map[string]bool{
	reasonBuildFailed:      true,
	reasonArtifactFailed:   true,
	reasonValidationFailed: true,
	reasonReconcileFailed:  true,
}

// degradedReasons are failures after the apply landed: it is live and wrong.
var degradedReasons = map[string]bool{
	reasonHealthCheckFail: true,
	reasonPruneFailed:     true,
}

// phaseFor maps one Kustomization's observation onto the delivery phase for a
// specific revision.
func phaseFor(k Kustomization, revision string) delivery.Status {
	detail := map[string]string{
		"kustomization":       k.Namespace + "/" + k.Name,
		"path":                k.Path,
		"lastAppliedRevision": k.LastAppliedRevision,
	}
	ref := fmt.Sprintf("flux: Kustomization %s/%s", k.Namespace, k.Name)

	if k.Suspended {
		return delivery.Status{
			Phase:    delivery.PhaseCommitted,
			Revision: revision,
			Cause:    ref + " is suspended: it will not reconcile until resumed (flux resume kustomization)",
			Detail:   detail,
		}
	}

	applied := revisionMatches(k.LastAppliedRevision, revision)
	attempted := revisionMatches(k.LastAttemptedRevision, revision)

	// Failures are reported against whichever revision Flux is working on: if
	// it never attempted ours, our change is still merely committed.
	if k.Ready == ConditionFalse {
		switch {
		case rejectedReasons[k.Reason] && (attempted || applied):
			return delivery.Status{
				Phase: delivery.PhaseRejected, Revision: revision,
				Cause: fmt.Sprintf("%s rejected the change (%s): %s", ref, k.Reason, k.Message), Detail: detail,
			}
		case degradedReasons[k.Reason]:
			return delivery.Status{
				Phase: delivery.PhaseDegraded, Revision: revision,
				Cause: fmt.Sprintf("%s applied the change but it is unhealthy (%s): %s", ref, k.Reason, k.Message), Detail: detail,
			}
		case k.Reason == reasonDependencyNR:
			return delivery.Status{
				Phase: delivery.PhaseReconciling, Revision: revision,
				Cause: fmt.Sprintf("%s is waiting on a dependency: %s", ref, k.Message), Detail: detail,
			}
		case attempted || applied:
			return delivery.Status{
				Phase: delivery.PhaseRejected, Revision: revision,
				Cause: fmt.Sprintf("%s is not ready (%s): %s", ref, k.Reason, k.Message), Detail: detail,
			}
		}
	}

	switch {
	case applied && k.Ready == ConditionTrue:
		// Flux reports Ready only once the apply succeeded and any configured
		// health checks passed.
		return delivery.Status{Phase: delivery.PhaseHealthy, Revision: revision, Detail: detail}
	case applied:
		return delivery.Status{Phase: delivery.PhaseApplied, Revision: revision, Detail: detail}
	case attempted || k.Reconciling || k.Reason == reasonProgressing:
		return delivery.Status{Phase: delivery.PhaseReconciling, Revision: revision, Detail: detail}
	default:
		// Committed, but Flux has not picked it up yet. This is the "keep
		// waiting" answer, deliberately distinct from Rejected.
		return delivery.Status{
			Phase:    delivery.PhaseCommitted,
			Revision: revision,
			Cause:    ref + " has not observed this revision yet",
			Detail:   detail,
		}
	}
}

// revisionMatches compares a Flux revision string against a git sha. Flux
// writes "<branch>@sha1:<sha>" (v2) or "<branch>/<sha>" (older); either side
// may be abbreviated.
func revisionMatches(fluxRevision, sha string) bool {
	if fluxRevision == "" || sha == "" {
		return false
	}
	got := fluxRevision
	if _, after, ok := cutLast(got, ":"); ok {
		got = after
	} else if _, after, ok := cutLast(got, "/"); ok {
		got = after
	}
	got, sha = strings.ToLower(strings.TrimSpace(got)), strings.ToLower(strings.TrimSpace(sha))
	if got == "" {
		return false
	}
	return strings.HasPrefix(got, sha) || strings.HasPrefix(sha, got)
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// covers reports whether this Kustomization reconciles the given repository
// path. Flux paths are conventionally written "./dir"; a Kustomization at
// "./clusters/prod" covers "clusters/prod/apps/web".
func (k Kustomization) covers(repo, repoPath string) bool {
	kp := normalizePath(k.Path)
	target := normalizePath(repoPath)
	if kp != "" && target != kp && !strings.HasPrefix(target, kp+"/") {
		return false
	}
	// A source URL is only compared when Flux told us one: kelson must not
	// silently decide it is unwatched because it could not resolve the source.
	if k.SourceURL != "" && repo != "" && !sameRepo(k.SourceURL, repo) {
		return false
	}
	return true
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	return strings.Trim(p, "/")
}

// sameRepo compares git URLs across the scheme/credential/.git spellings of
// the same repository.
func sameRepo(a, b string) bool {
	return normalizeRepo(a) == normalizeRepo(b)
}

func normalizeRepo(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if _, after, ok := strings.Cut(s, "@"); ok {
		s = after
	}
	s = strings.ReplaceAll(s, ":", "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.Trim(s, "/")
}

// Runner executes an external command and returns its stdout. It is the seam
// that keeps the CLI-backed readers testable without a cluster.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands for real.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		// Surface stderr: "connection refused" is the useful half of a failed
		// kubectl call and Output() hides it.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, err
	}
	return out, nil
}

// CLIStatusReader reads Flux objects with kubectl. It deliberately avoids a
// client-go dependency in the control plane for now: the JSON shape below is
// the Flux API and swapping in a typed client later is a change to this file
// only (the StatusReader interface stays put).
type CLIStatusReader struct {
	// Bin is the kubectl binary; defaults to "kubectl".
	Bin string
	// Namespace limits the query; empty means all namespaces.
	Namespace string
	// Run defaults to ExecRunner.
	Run Runner
}

func (c CLIStatusReader) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "kubectl"
}

func (c CLIStatusReader) run() Runner {
	if c.Run != nil {
		return c.Run
	}
	return ExecRunner
}

func (c CLIStatusReader) scope() []string {
	if c.Namespace == "" {
		return []string{"--all-namespaces"}
	}
	return []string{"-n", c.Namespace}
}

// fluxList is the subset of the Kustomization/HelmRelease CRD JSON kelson
// needs.
type fluxList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Path      string `json:"path"`
			Suspend   bool   `json:"suspend"`
			SourceRef struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"sourceRef"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			LastAppliedRevision   string `json:"lastAppliedRevision"`
			LastAttemptedRevision string `json:"lastAttemptedRevision"`
		} `json:"status"`
	} `json:"items"`
}

// gitRepoList is the subset of GitRepository JSON used to resolve a
// Kustomization's source URL.
type gitRepoList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			URL string `json:"url"`
			Ref struct {
				Branch string `json:"branch"`
			} `json:"ref"`
		} `json:"spec"`
	} `json:"items"`
}

func (c CLIStatusReader) Kustomizations(ctx context.Context) ([]Kustomization, error) {
	args := append([]string{"get", "kustomizations.kustomize.toolkit.fluxcd.io"}, c.scope()...)
	args = append(args, "-o", "json")
	out, err := c.run()(ctx, c.bin(), args...)
	if err != nil {
		return nil, readErr("Kustomizations", err)
	}
	var list fluxList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, readErr("Kustomizations", err)
	}

	sources, err := c.gitRepositories(ctx)
	if err != nil {
		// Source resolution is best effort: without it kelson matches on path
		// alone, which is still correct for single-repo installs.
		sources = nil
	}

	ks := make([]Kustomization, 0, len(list.Items))
	for _, it := range list.Items {
		k := Kustomization{
			Name:                  it.Metadata.Name,
			Namespace:             it.Metadata.Namespace,
			Path:                  it.Spec.Path,
			Suspended:             it.Spec.Suspend,
			SourceKind:            it.Spec.SourceRef.Kind,
			SourceName:            it.Spec.SourceRef.Name,
			LastAppliedRevision:   it.Status.LastAppliedRevision,
			LastAttemptedRevision: it.Status.LastAttemptedRevision,
			Ready:                 ConditionUnknown,
		}
		for _, cond := range it.Status.Conditions {
			switch cond.Type {
			case "Ready":
				k.Ready = ConditionState(cond.Status)
				k.Reason, k.Message = cond.Reason, cond.Message
			case "Reconciling":
				k.Reconciling = cond.Status == string(ConditionTrue)
			}
		}
		if src, ok := sources[it.Metadata.Namespace+"/"+it.Spec.SourceRef.Name]; ok {
			k.SourceURL, k.SourceBranch = src.url, src.branch
		}
		ks = append(ks, k)
	}
	return ks, nil
}

type gitSource struct{ url, branch string }

func (c CLIStatusReader) gitRepositories(ctx context.Context) (map[string]gitSource, error) {
	args := append([]string{"get", "gitrepositories.source.toolkit.fluxcd.io"}, c.scope()...)
	args = append(args, "-o", "json")
	out, err := c.run()(ctx, c.bin(), args...)
	if err != nil {
		return nil, err
	}
	var list gitRepoList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	m := map[string]gitSource{}
	for _, it := range list.Items {
		m[it.Metadata.Namespace+"/"+it.Metadata.Name] = gitSource{url: it.Spec.URL, branch: it.Spec.Ref.Branch}
	}
	return m, nil
}

func (c CLIStatusReader) HelmReleases(ctx context.Context) ([]HelmRelease, error) {
	args := append([]string{"get", "helmreleases.helm.toolkit.fluxcd.io"}, c.scope()...)
	args = append(args, "-o", "json")
	out, err := c.run()(ctx, c.bin(), args...)
	if err != nil {
		return nil, readErr("HelmReleases", err)
	}
	var list fluxList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, readErr("HelmReleases", err)
	}
	hrs := make([]HelmRelease, 0, len(list.Items))
	for _, it := range list.Items {
		hr := HelmRelease{Name: it.Metadata.Name, Namespace: it.Metadata.Namespace, Ready: ConditionUnknown}
		for _, cond := range it.Status.Conditions {
			if cond.Type == "Ready" {
				hr.Ready = ConditionState(cond.Status)
				hr.Reason, hr.Message = cond.Reason, cond.Message
			}
		}
		hrs = append(hrs, hr)
	}
	return hrs, nil
}

func readErr(what string, err error) error {
	e := delivery.ApplyFailed("flux/status", "",
		"could not read Flux "+what+" from the cluster",
		"check cluster connectivity and that the Flux CRDs are installed (flux check)")
	e.Cause = err.Error()
	return e
}

var _ StatusReader = CLIStatusReader{}
