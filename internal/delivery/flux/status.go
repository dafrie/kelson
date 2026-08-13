package flux

import (
	"context"
	"fmt"
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
// interface: the dynamic client today (dynamic.go), a watch cache later, a
// fake in tests. None of that changes the phase mapping below.
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
	Ready                 ConditionState
	Reason                string
	Message               string
	Reconciling           bool
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

// StatusReader reads Flux objects from the cluster. The production
// implementation is DynamicStatusReader (dynamic.go); tests use a fake.
type StatusReader interface {
	Kustomizations(ctx context.Context) ([]Kustomization, error)
	HelmReleases(ctx context.Context) ([]HelmRelease, error)
}

// Health is the state of the Flux control plane itself, which is a different
// question from the state of one Kustomization: a change that Flux has not
// observed looks identical whether Flux is merely slow or its source-controller
// is down (issue #137).
type Health struct {
	// Source names where the answer came from — HealthFromReport when
	// flux-operator publishes a FluxReport, HealthFromControllers when kelson
	// aggregated the controller Deployments itself. Reported so a human can
	// tell an authoritative answer from an inferred one.
	Source string
	// Version is the Flux distribution version when the source knows it.
	Version string
	Ready   bool
	// Unready names the components that are not ready, sorted.
	Unready []string
	// Message is the one-line human summary.
	Message string
}

// Health sources.
const (
	HealthFromReport      = "FluxReport"
	HealthFromControllers = "controllers"
)

// HealthReader is the optional half of a StatusReader. It is an extension
// rather than a StatusReader method because a status source that cannot see
// the Flux namespace is still a perfectly good status source: the adapter
// type-asserts for it and treats a missing or failing implementation as "no
// extra explanation available", never as a status failure.
type HealthReader interface {
	Health(ctx context.Context) (Health, error)
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

// readErr wraps a failed cluster read as a delivery error naming what could
// not be read.
func readErr(what string, err error) error {
	e := delivery.ApplyFailed("flux/status", "",
		"could not read Flux "+what+" from the cluster",
		"check cluster connectivity and that the Flux CRDs are installed (flux check)")
	e.Cause = err.Error()
	return e
}
