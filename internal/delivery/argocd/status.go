package argocd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
)

// SyncStatus is Argo's status.sync.status.
type SyncStatus string

const (
	SyncSynced    SyncStatus = "Synced"
	SyncOutOfSync SyncStatus = "OutOfSync"
	SyncUnknown   SyncStatus = "Unknown"
)

// HealthStatus is Argo's status.health.status. kelson consumes these verbatim:
// Argo owns health assessment (including custom Lua health checks operators
// have written for their CRDs) and a second opinion computed by kelson could
// only ever disagree with the tool that is actually doing the applying.
type HealthStatus string

const (
	HealthHealthy     HealthStatus = "Healthy"
	HealthProgressing HealthStatus = "Progressing"
	HealthDegraded    HealthStatus = "Degraded"
	HealthSuspended   HealthStatus = "Suspended"
	HealthMissing     HealthStatus = "Missing"
	HealthUnknown     HealthStatus = "Unknown"
)

// OperationPhase is the phase of Argo's last sync operation.
type OperationPhase string

const (
	OperationRunning     OperationPhase = "Running"
	OperationSucceeded   OperationPhase = "Succeeded"
	OperationFailed      OperationPhase = "Failed"
	OperationError       OperationPhase = "Error"
	OperationTerminating OperationPhase = "Terminating"
)

// Operation is the slice of status.operationState kelson reads. Revision is the
// revision the operation targeted, which is how kelson tells "Argo failed on my
// change" from "Argo failed on someone else's".
type Operation struct {
	Phase    OperationPhase
	Message  string
	Revision string
}

// Condition is one status.conditions entry. Argo names its failure conditions
// with an "Error" suffix (ComparisonError, InvalidSpecError, SyncError) and its
// advisory ones with a "Warning" suffix; kelson keys off that suffix rather
// than enumerating a list that Argo may extend.
type Condition struct {
	Type    string
	Message string
}

// Application is the slice of an Argo CD Application kelson reads. It is a
// plain struct, not a generated API type, so the reader stays an interface: the
// REST API today, a watch cache later, a fake in tests. None of that changes
// the phase mapping below.
type Application struct {
	Name      string
	Namespace string
	// Project is the Argo AppProject the Application belongs to.
	Project string

	// RepoURL, Path and TargetRevision are spec.source: what this Application
	// tracks. They are what makes "kelson wrote somewhere nothing watches"
	// detectable instead of silent.
	RepoURL        string
	Path           string
	TargetRevision string

	// AutoSync and SelfHeal report spec.syncPolicy.automated. They do not
	// change the phase mapping; they change what happens when kelson's own sync
	// call fails, so they are surfaced in Status detail.
	AutoSync bool
	SelfHeal bool

	// SyncStatus and SyncedRevision are status.sync: Argo's own comparison
	// result and the revision it reflects.
	SyncStatus     SyncStatus
	SyncedRevision string

	// Health and HealthMessage are status.health: Argo's own assessment.
	Health        HealthStatus
	HealthMessage string

	Operation  Operation
	Conditions []Condition
}

// ApplicationReader reads Argo Applications. Implementations use the Argo CD
// REST API; tests use a fake. The bool reports whether the Application exists —
// "not there" is a configuration answer, not an error to unwrap.
type ApplicationReader interface {
	Application(ctx context.Context, name string) (Application, bool, error)
}

// phaseFor maps one Application's observation onto the delivery phase for a
// specific revision. The three answers that must stay distinct (docs/delivery.md):
// not-picked-up-yet is Committed, refused is Rejected, live-and-unhealthy is
// Degraded.
//
// The health gate is the load-bearing rule of #35: PhaseHealthy requires Argo
// to say Healthy. Every other health value on a change that did land is Applied
// at best, and Degraded is reported as Degraded with the Application and Argo's
// own health reason named in the Cause.
func phaseFor(app Application, revision string) delivery.Status {
	detail := map[string]string{
		"application":    app.Namespace + "/" + app.Name,
		"argoProject":    app.Project,
		"syncStatus":     string(app.SyncStatus),
		"health":         string(app.Health),
		"syncedRevision": app.SyncedRevision,
		"autoSync":       strconv.FormatBool(app.AutoSync),
	}
	ref := fmt.Sprintf("argocd: Application %s/%s", app.Namespace, app.Name)

	synced := revisionMatches(app.SyncedRevision, revision)
	attempted := synced || revisionMatches(app.Operation.Revision, revision)

	// An error condition blocks every revision, not just ours: Argo cannot
	// compare or apply anything until the Application is fixed, so waiting
	// would be the wrong advice.
	if c, ok := app.errorCondition(); ok {
		return delivery.Status{
			Phase: delivery.PhaseRejected, Revision: revision,
			Cause:  fmt.Sprintf("%s rejected the change (%s): %s", ref, c.Type, c.Message),
			Detail: detail,
		}
	}

	switch app.Operation.Phase {
	case OperationRunning, OperationTerminating:
		return delivery.Status{Phase: delivery.PhaseReconciling, Revision: revision, Detail: detail}
	case OperationFailed, OperationError:
		if attempted {
			return delivery.Status{
				Phase: delivery.PhaseRejected, Revision: revision,
				Cause: fmt.Sprintf("%s failed to sync this revision (%s): %s",
					ref, app.Operation.Phase, app.Operation.Message),
				Detail: detail,
			}
		}
	}

	if !attempted {
		// Committed, but Argo has not synced it yet. This is the "keep waiting"
		// answer, deliberately distinct from Rejected — and it stays distinct
		// even when the Application is currently unhealthy on an older
		// revision, which is somebody else's change, not ours.
		cause := ref + " has not synced this revision yet"
		if app.Health == HealthDegraded {
			cause += fmt.Sprintf(" (the Application is currently Degraded on revision %s: %s)",
				short(app.SyncedRevision), app.HealthMessage)
		}
		return delivery.Status{Phase: delivery.PhaseCommitted, Revision: revision, Cause: cause, Detail: detail}
	}

	switch {
	case app.Health == HealthDegraded:
		return delivery.Status{
			Phase: delivery.PhaseDegraded, Revision: revision,
			Cause:  fmt.Sprintf("%s applied the change but it is unhealthy (Degraded): %s", ref, app.HealthMessage),
			Detail: detail,
		}
	case !synced:
		// Argo attempted our revision but status.sync does not reflect it yet.
		return delivery.Status{Phase: delivery.PhaseReconciling, Revision: revision, Detail: detail}
	case app.SyncStatus == SyncSynced && app.Health == HealthHealthy:
		return delivery.Status{Phase: delivery.PhaseHealthy, Revision: revision, Detail: detail}
	case app.SyncStatus == SyncSynced:
		return delivery.Status{
			Phase: delivery.PhaseApplied, Revision: revision,
			Cause:  fmt.Sprintf("%s applied the change; Argo reports health %s: %s", ref, health(app.Health), app.HealthMessage),
			Detail: detail,
		}
	default:
		// Our revision is the synced one, yet Argo still reports OutOfSync:
		// something drifted in the cluster after the sync.
		return delivery.Status{
			Phase: delivery.PhaseApplied, Revision: revision,
			Cause:  fmt.Sprintf("%s applied the change but reports %s: live resources have drifted from the rendered manifests", ref, app.SyncStatus),
			Detail: detail,
		}
	}
}

func health(h HealthStatus) HealthStatus {
	if h == "" {
		return HealthUnknown
	}
	return h
}

// errorCondition returns the first Argo condition that reports a failure.
func (app Application) errorCondition() (Condition, bool) {
	for _, c := range app.Conditions {
		if strings.HasSuffix(c.Type, "Error") {
			return c, true
		}
	}
	return Condition{}, false
}

// covers reports whether this Application tracks what kelson writes: the same
// repository, a path at or above the delivery path, and a targetRevision that
// follows the delivery branch. An Application pinned to a tag or a fixed sha
// does not cover the branch kelson commits to — and saying so is the whole
// point of the check.
func (app Application) covers(repo, repoPath, branch string) bool {
	if app.RepoURL != "" && repo != "" && !sameRepo(app.RepoURL, repo) {
		return false
	}
	ap, target := normalizePath(app.Path), normalizePath(repoPath)
	if ap != "" && target != ap && !strings.HasPrefix(target, ap+"/") {
		return false
	}
	return tracksBranch(app.TargetRevision, branch)
}

// tracksBranch reports whether an Argo targetRevision follows the given branch.
// An empty revision and "HEAD" mean the repository's default branch, which
// kelson cannot resolve from here, so they are accepted.
func tracksBranch(targetRevision, branch string) bool {
	tr := strings.TrimSpace(targetRevision)
	if tr == "" || strings.EqualFold(tr, "HEAD") || branch == "" {
		return true
	}
	tr = strings.TrimPrefix(tr, "refs/heads/")
	return strings.EqualFold(tr, branch)
}

// revisionMatches compares an Argo revision against a git sha. Argo records
// full shas, but either side may be abbreviated by the caller.
func revisionMatches(argoRevision, sha string) bool {
	got := strings.ToLower(strings.TrimSpace(argoRevision))
	want := strings.ToLower(strings.TrimSpace(sha))
	if got == "" || want == "" {
		return false
	}
	return strings.HasPrefix(got, want) || strings.HasPrefix(want, got)
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	return strings.Trim(p, "/")
}

// sameRepo compares git URLs across the scheme/credential/.git spellings of the
// same repository.
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
