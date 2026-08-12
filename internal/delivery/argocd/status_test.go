package argocd

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

const ourRevision = "1111111111111111111111111111111111111111"
const otherRevision = "2222222222222222222222222222222222222222"

func app(mut ...func(*Application)) Application {
	a := Application{
		Name: "shop-production", Namespace: "argocd", Project: "default",
		RepoURL: "https://forge.example/acme/deploy.git", Path: "manifests", TargetRevision: "main",
		SyncStatus: SyncUnknown, Health: HealthUnknown,
	}
	for _, m := range mut {
		m(&a)
	}
	return a
}

// TestPhaseFor pins the mapping from Argo's own state onto the delivery phase
// machine. The three answers stay distinct: not-synced-yet is Committed,
// refused is Rejected, live-and-unhealthy is Degraded.
func TestPhaseFor(t *testing.T) {
	cases := []struct {
		name  string
		app   Application
		want  delivery.Phase
		cause string
	}{
		{
			name: "synced and healthy",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncSynced, ourRevision, HealthHealthy
			}),
			want: delivery.PhaseHealthy,
		},
		{
			name: "synced but degraded",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncSynced, ourRevision, HealthDegraded
				a.HealthMessage = "Deployment checkout has 0/3 replicas available"
			}),
			want:  delivery.PhaseDegraded,
			cause: "0/3 replicas",
		},
		{
			name: "synced but still progressing",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncSynced, ourRevision, HealthProgressing
			}),
			want:  delivery.PhaseApplied,
			cause: "Progressing",
		},
		{
			name: "synced, healthy, but drifted",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncOutOfSync, ourRevision, HealthHealthy
			}),
			want:  delivery.PhaseApplied,
			cause: "drifted",
		},
		{
			name: "out of sync on an older revision is committed, not rejected",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncOutOfSync, otherRevision, HealthHealthy
			}),
			want:  delivery.PhaseCommitted,
			cause: "has not synced this revision yet",
		},
		{
			name: "unknown sync status is committed",
			app:  app(),
			want: delivery.PhaseCommitted,
		},
		{
			name: "degraded on somebody else's revision keeps our change committed",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncOutOfSync, otherRevision, HealthDegraded
				a.HealthMessage = "CrashLoopBackOff"
			}),
			want:  delivery.PhaseCommitted,
			cause: "currently Degraded",
		},
		{
			name: "sync operation running",
			app: app(func(a *Application) {
				a.Operation = Operation{Phase: OperationRunning, Revision: ourRevision}
			}),
			want: delivery.PhaseReconciling,
		},
		{
			name: "sync of our revision failed",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision = SyncOutOfSync, otherRevision
				a.Operation = Operation{Phase: OperationFailed, Revision: ourRevision, Message: "one or more objects failed to apply"}
			}),
			want:  delivery.PhaseRejected,
			cause: "failed to apply",
		},
		{
			name: "sync of another revision failed leaves ours committed",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision = SyncOutOfSync, otherRevision
				a.Operation = Operation{Phase: OperationFailed, Revision: otherRevision, Message: "boom"}
			}),
			want: delivery.PhaseCommitted,
		},
		{
			name: "comparison error blocks every revision",
			app: app(func(a *Application) {
				a.Conditions = []Condition{{Type: "ComparisonError", Message: "rpc error: repository not accessible"}}
			}),
			want:  delivery.PhaseRejected,
			cause: "repository not accessible",
		},
		{
			name: "warning conditions are not failures",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncSynced, ourRevision, HealthHealthy
				a.Conditions = []Condition{{Type: "OrphanedResourceWarning", Message: "1 orphaned resource"}}
			}),
			want: delivery.PhaseHealthy,
		},
		{
			name: "applied but resources are missing",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision, a.Health = SyncSynced, ourRevision, HealthMissing
			}),
			want:  delivery.PhaseApplied,
			cause: "Missing",
		},
		{
			name: "our revision was attempted but is not the synced one yet",
			app: app(func(a *Application) {
				a.SyncStatus, a.SyncedRevision = SyncOutOfSync, otherRevision
				a.Operation = Operation{Phase: OperationSucceeded, Revision: ourRevision}
			}),
			want: delivery.PhaseReconciling,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := phaseFor(tc.app, ourRevision)
			if got.Phase != tc.want {
				t.Fatalf("phase = %q, want %q (cause %q)", got.Phase, tc.want, got.Cause)
			}
			if got.Revision != ourRevision {
				t.Fatalf("revision = %q", got.Revision)
			}
			if tc.cause != "" && !strings.Contains(got.Cause, tc.cause) {
				t.Fatalf("cause = %q, want it to mention %q", got.Cause, tc.cause)
			}
			if tc.want != delivery.PhaseHealthy && got.Detail["application"] != "argocd/shop-production" {
				t.Fatalf("detail must name the Application: %v", got.Detail)
			}
		})
	}
}

// TestNeverHealthyWhenArgoReportsDegraded is the #35 acceptance criterion,
// checked across every sync status and operation phase: whatever else kelson
// concludes, it never claims Healthy while Argo says Degraded.
func TestNeverHealthyWhenArgoReportsDegraded(t *testing.T) {
	for _, sync := range []SyncStatus{SyncSynced, SyncOutOfSync, SyncUnknown} {
		for _, op := range []OperationPhase{"", OperationRunning, OperationSucceeded, OperationFailed, OperationError, OperationTerminating} {
			for _, rev := range []string{ourRevision, otherRevision} {
				a := app(func(a *Application) {
					a.SyncStatus, a.SyncedRevision, a.Health = sync, rev, HealthDegraded
					a.HealthMessage = "CrashLoopBackOff"
					a.Operation = Operation{Phase: op, Revision: rev}
				})
				if got := phaseFor(a, ourRevision); got.Phase == delivery.PhaseHealthy {
					t.Fatalf("sync=%s op=%s synced=%s reported Healthy while Argo is Degraded", sync, op, rev)
				}
			}
		}
	}
}

// TestCovers pins the not-watched check: an Application only covers a delivery
// target when it tracks the same repository, an ancestor-or-equal path, and the
// branch kelson commits to.
func TestCovers(t *testing.T) {
	repo := "https://forge.example/acme/deploy.git"
	cases := []struct {
		name string
		app  Application
		want bool
	}{
		{"exact", app(), true},
		{"parent path", app(func(a *Application) { a.Path = "" }), true},
		{"ancestor path", app(func(a *Application) { a.Path = "manifests" }), true},
		{"ssh spelling of the same repo", app(func(a *Application) { a.RepoURL = "git@forge.example:acme/deploy.git" }), true},
		{"HEAD tracks the delivery branch", app(func(a *Application) { a.TargetRevision = "HEAD" }), true},
		{"refs/heads spelling", app(func(a *Application) { a.TargetRevision = "refs/heads/main" }), true},
		{"sibling path", app(func(a *Application) { a.Path = "other" }), false},
		{"child path", app(func(a *Application) { a.Path = "manifests/shop/production/extra" }), false},
		{"other repo", app(func(a *Application) { a.RepoURL = "https://forge.example/acme/other.git" }), false},
		{"other branch", app(func(a *Application) { a.TargetRevision = "staging" }), false},
		{"pinned revision", app(func(a *Application) { a.TargetRevision = otherRevision }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.app.covers(repo, "manifests/shop/production", "main"); got != tc.want {
				t.Fatalf("covers = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRevisionMatches verifies abbreviated shas correlate with Argo's full ones.
func TestRevisionMatches(t *testing.T) {
	if !revisionMatches(ourRevision, ourRevision[:8]) {
		t.Fatalf("abbreviated sha must match")
	}
	if revisionMatches(ourRevision, otherRevision) {
		t.Fatalf("different shas must not match")
	}
	if revisionMatches("", ourRevision) || revisionMatches(ourRevision, "") {
		t.Fatalf("an empty revision matches nothing")
	}
}
