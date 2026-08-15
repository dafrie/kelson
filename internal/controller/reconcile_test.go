package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
)

// The reconciler's half of the spine: rollback, history, the finalizer and the
// error taxonomy. Everything here is about what goes into `.status` and what
// controller-runtime is asked to do next, so the Deliverer is a fake.

func progressing(t *testing.T, conditions []metav1.Condition) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(conditions, v1alpha1.ConditionProgressing)
	if condition == nil {
		t.Fatalf("no Progressing condition was set")
	}
	return condition
}

// deliveredEnvironment is an Environment that has already published two
// revisions, with the status a real reconcile would have left.
func deliveredEnvironment() *v1alpha1.Environment {
	env := validEnvironment()
	env.Generation = 7
	env.Finalizers = []string{Finalizer}
	env.Status.Revision = "7-1a2b3c4d"
	env.Status.Phase = v1alpha1.PhaseHealthy
	env.Status.History = []v1alpha1.HistoryEntry{
		{Revision: "7-1a2b3c4d", Digest: "sha256:aaa", Outcome: v1alpha1.PhaseHealthy},
		{Revision: "6-9f0a1b2c", Digest: "sha256:bbb", Outcome: v1alpha1.PhaseHealthy},
	}
	return env
}

// --- rollback ---------------------------------------------------------------

// TestRollbackPinsAndSuspendsRendering is ADR-0028 decision 5 at the
// reconciler: the target is handed to the deliverer, nothing is rendered, and
// the state is visible rather than implicit.
func TestRollbackPinsAndSuspendsRendering(t *testing.T) {
	env := deliveredEnvironment()
	env.Annotations = map[string]string{v1alpha1.AnnotationRollbackTo: "6-9f0a1b2c"}

	spy := &spyDeliverer{outcome: Outcome{
		Revision: "6-9f0a1b2c", Phase: v1alpha1.PhaseHealthy, RolledBack: true,
	}}
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spy.got.PinnedTo != "6-9f0a1b2c" {
		t.Fatalf("the deliverer was not told to pin: %q", spy.got.PinnedTo)
	}
	if len(spy.got.Manifests) != 0 {
		t.Errorf("the spec was rendered under a rollback (%d manifests); steps 3 and 4 must not run",
			len(spy.got.Manifests))
	}

	got := readEnvironment(t, c, "production")
	if got.Status.Revision != "6-9f0a1b2c" {
		t.Errorf("revision = %q, want the rollback target", got.Status.Revision)
	}
	if got.Status.RollbackRevision != "6-9f0a1b2c" || got.Status.RollbackGeneration != 7 {
		t.Errorf("the rollback was not recorded: %q at generation %d",
			got.Status.RollbackRevision, got.Status.RollbackGeneration)
	}
	ready := ready(t, got.Status.Conditions)
	if ready.Status != metav1.ConditionTrue || ready.Reason != v1alpha1.ReasonRolledBack {
		t.Errorf("Ready is %s/%s, want True/%s", ready.Status, ready.Reason, v1alpha1.ReasonRolledBack)
	}
	// The state is visible: Progressing says the environment is deliberately
	// not tracking its spec, and names both ways out.
	prog := progressing(t, got.Status.Conditions)
	if prog.Status != metav1.ConditionFalse || prog.Reason != v1alpha1.ReasonRollbackPinned {
		t.Fatalf("Progressing is %s/%s, want False/%s", prog.Status, prog.Reason, v1alpha1.ReasonRollbackPinned)
	}
	if !strings.Contains(prog.Message, v1alpha1.AnnotationRollbackTo) ||
		!strings.Contains(prog.Message, "remove the annotation") ||
		!strings.Contains(prog.Message, "edit the spec") {
		t.Errorf("the message does not name the annotation and both ways out: %q", prog.Message)
	}
	// Nothing new was published, so nothing was added to the history.
	if len(got.Status.History) != 2 {
		t.Errorf("history grew to %d entries on a rollback", len(got.Status.History))
	}
}

// TestRollbackToAnUnknownRevisionIsRefused: kelson will not point an
// OCIRepository at a tag it cannot confirm it published.
func TestRollbackToAnUnknownRevisionIsRefused(t *testing.T) {
	env := deliveredEnvironment()
	env.Annotations = map[string]string{v1alpha1.AnnotationRollbackTo: "3-deadbeef"}

	spy := &spyDeliverer{}
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	result, err := r.Reconcile(context.Background(), request("production"))
	if err != nil {
		t.Fatalf("an unknown rollback target must not be an error return: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("requeue = %s, want none: nothing changes on its own", result.RequeueAfter)
	}
	if spy.calls != 0 {
		t.Error("the deliverer was called for a target that could not be verified")
	}
	condition := ready(t, readEnvironment(t, c, "production").Status.Conditions)
	if condition.Reason != v1alpha1.ReasonRollbackTargetUnknown {
		t.Fatalf("Ready reason = %q, want %q", condition.Reason, v1alpha1.ReasonRollbackTargetUnknown)
	}
	// The window is the answer, and the message has to say so — a correct
	// target older than twenty entries looks identical to a typo otherwise.
	if !strings.Contains(condition.Message, "6-9f0a1b2c") || !strings.Contains(condition.Message, "registry query") {
		t.Errorf("the refusal does not say what is known or where else to look: %q", condition.Message)
	}
}

// TestRollbackGoesInertOnASpecEdit is the second of ADR-0028's two ways out: an
// operator who has just fixed the bug should not have to remember to clear an
// annotation as well.
func TestRollbackGoesInertOnASpecEdit(t *testing.T) {
	env := deliveredEnvironment()
	env.Annotations = map[string]string{v1alpha1.AnnotationRollbackTo: "6-9f0a1b2c"}
	// The controller already honoured the rollback at generation 7…
	env.Status.RollbackRevision = "6-9f0a1b2c"
	env.Status.RollbackGeneration = 7
	// …and the spec has been edited since.
	env.Generation = 8

	spy := &spyDeliverer{outcome: Outcome{
		Revision: "8-abcdef01", Phase: v1alpha1.PhaseCommitted, Published: true,
	}}
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spy.got.PinnedTo != "" {
		t.Fatalf("the annotation still pinned after a spec edit: %q", spy.got.PinnedTo)
	}
	if len(spy.got.Manifests) == 0 {
		t.Error("normal publishing did not resume: nothing was rendered")
	}
	got := readEnvironment(t, c, "production")
	// The bookkeeping survives the inert reconcile. Clearing it here would
	// hand the next reconcile a standing annotation against an empty
	// status.rollbackRevision — case 1 of rollbackFor, a *new* rollback —
	// and the environment would flap back to the pinned tag one reconcile
	// after publishing the edit.
	if got.Status.RollbackRevision != "6-9f0a1b2c" || got.Status.RollbackGeneration != 7 {
		t.Errorf("the inert rollback lost its bookkeeping: %q at generation %d",
			got.Status.RollbackRevision, got.Status.RollbackGeneration)
	}
	// An annotation that silently stopped mattering is worse than one that
	// never worked, so the condition says it is inert.
	prog := progressing(t, got.Status.Conditions)
	if !strings.Contains(prog.Message, "inert") {
		t.Errorf("Progressing does not say the annotation is inert: %q", prog.Message)
	}

	// And it stays inert: the annotation is untouched, so the next reconcile
	// must read the same state, not re-pin.
	spy.outcome = Outcome{Revision: "8-abcdef01", Phase: v1alpha1.PhaseCommitted}
	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if spy.got.PinnedTo != "" {
		t.Fatalf("the inert rollback re-armed on the next reconcile: pinned to %q", spy.got.PinnedTo)
	}
	if len(spy.got.Manifests) == 0 {
		t.Error("the second reconcile did not render: the inert rollback suspended publishing")
	}
}

// TestRollbackToANewTargetAtTheSameGenerationIsHonoured: re-annotating with a
// different revision is a new statement of intent, even with no spec edit.
func TestRollbackToANewTargetAtTheSameGenerationIsHonoured(t *testing.T) {
	env := deliveredEnvironment()
	env.Status.History = append(env.Status.History, v1alpha1.HistoryEntry{Revision: "5-11223344"})
	env.Annotations = map[string]string{v1alpha1.AnnotationRollbackTo: "5-11223344"}
	env.Status.RollbackRevision = "6-9f0a1b2c"
	env.Status.RollbackGeneration = 7

	spy := &spyDeliverer{outcome: Outcome{Revision: "5-11223344", RolledBack: true}}
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spy.got.PinnedTo != "5-11223344" {
		t.Fatalf("the new target was not honoured: %q", spy.got.PinnedTo)
	}
}

// --- history ----------------------------------------------------------------

// TestHistoryGrowsOnlyOnANewPublish: a healthy environment reconciling on its
// interval must not fill the twenty-entry window with one revision.
func TestHistoryGrowsOnlyOnANewPublish(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	spy := &spyDeliverer{outcome: Outcome{
		Revision: "1-1a2b3c4d", Digest: "sha256:abc", Phase: v1alpha1.PhaseCommitted,
		Published: true,
		Images:    []v1alpha1.ComponentImage{{Component: "web", Image: "ghcr.io/acme/checkout:1.0.0"}},
	}}
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := readEnvironment(t, c, "production")
	if len(got.Status.History) != 1 {
		t.Fatalf("history has %d entries, want 1", len(got.Status.History))
	}
	entry := got.Status.History[0]
	if entry.Revision != "1-1a2b3c4d" || entry.Digest != "sha256:abc" {
		t.Errorf("entry = %+v", entry)
	}
	if entry.SpecHash == "" || len(entry.SpecHash) != 64 {
		t.Errorf("the entry carries no full spec hash: %q", entry.SpecHash)
	}
	if len(entry.ComponentImages) != 1 || entry.ComponentImages[0].Component != "web" ||
		entry.ComponentImages[0].Image != "ghcr.io/acme/checkout:1.0.0" {
		t.Errorf("componentImages = %+v, want the image under the component that resolved it",
			entry.ComponentImages)
	}
	// The deprecated flat field is written as a mirror for one release, so a
	// status reader built against the older shape still sees what ran.
	//nolint:staticcheck // asserting the deprecated mirror is the point of this block.
	if len(entry.Images) != 1 || entry.Images[0] != "ghcr.io/acme/checkout:1.0.0" {
		t.Errorf("images = %v, want the flat mirror of componentImages", entry.Images)
	}
	if entry.Outcome != v1alpha1.PhaseCommitted || entry.Timestamp.IsZero() {
		t.Errorf("outcome = %q at %v", entry.Outcome, entry.Timestamp)
	}

	// The same revision observed again: no new entry, and the outcome moves.
	spy.outcome = Outcome{Revision: "1-1a2b3c4d", Phase: v1alpha1.PhaseHealthy}
	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	got = readEnvironment(t, c, "production")
	if len(got.Status.History) != 1 {
		t.Fatalf("history grew to %d on a re-observation", len(got.Status.History))
	}
	if got.Status.History[0].Outcome != v1alpha1.PhaseHealthy {
		t.Errorf("the head entry's outcome is %q, want it refreshed to Healthy",
			got.Status.History[0].Outcome)
	}
}

// TestHistoryIsBoundedAndDeduped covers the fold directly, because reaching
// twenty entries through the reconciler would be twenty fixtures.
func TestHistoryIsBoundedAndDeduped(t *testing.T) {
	now := metav1.Now()
	var history []v1alpha1.HistoryEntry
	for i := 30; i > 0; i-- {
		out := Outcome{Revision: revisionName(i), Published: true, Phase: v1alpha1.PhaseHealthy}
		history = recordHistory(history, out, testHash, now)
	}
	if len(history) != v1alpha1.MaxHistoryEntries {
		t.Fatalf("history holds %d entries, want the bound of %d", len(history), v1alpha1.MaxHistoryEntries)
	}
	if history[0].Revision != revisionName(1) {
		t.Errorf("newest entry is %q, want the most recent publish", history[0].Revision)
	}

	// Republishing an unchanged tag refreshes rather than duplicates: a tag is
	// immutable, so two entries for one revision are the same artifact.
	before := len(history)
	history = recordHistory(history, Outcome{
		Revision: history[3].Revision, Digest: "sha256:new", Published: true,
	}, testHash, now)
	if len(history) != before {
		t.Errorf("republishing an existing tag changed the length from %d to %d", before, len(history))
	}
	seen := map[string]int{}
	for _, e := range history {
		seen[e.Revision]++
		if seen[e.Revision] > 1 {
			t.Fatalf("%s appears twice", e.Revision)
		}
	}
	if history[0].Digest != "sha256:new" {
		t.Errorf("the refreshed entry did not move to the head: %+v", history[0])
	}
}

func revisionName(i int) string {
	return string(rune('a'+i%26)) + "-0000000" + string(rune('0'+i%10))
}

// TestAnOverlappingStatusWriteConflictsRatherThanLosingHistory: a merge patch of
// `status.history` replaces the whole array, so two writers that both read the
// same history and each fold in an outcome produce two arrays and the later
// write wins outright — silently dropping a revision out of the record ADR-0028
// decision 4 says the status mirrors. The optimistic lock turns that into a
// conflict, and a conflict is a requeue that re-reads and folds again.
func TestAnOverlappingStatusWriteConflictsRatherThanLosingHistory(t *testing.T) {
	env := deliveredEnvironment()
	env.Generation = 8
	c := newClient(t, validProject(), env)

	// The other writer lands while this reconcile is inside the deliverer:
	// after the object was read, before its status is written.
	spy := &spyDeliverer{outcome: Outcome{Revision: "8-99887766", Phase: v1alpha1.PhaseCommitted, Published: true}}
	spy.during = func() {
		other := readEnvironment(t, c, "production")
		other.Status.History = append([]v1alpha1.HistoryEntry{{
			Revision: "8-cafebabe", Outcome: v1alpha1.PhaseHealthy,
		}}, other.Status.History...)
		if err := c.Status().Update(context.Background(), other); err != nil {
			t.Fatalf("the concurrent writer failed: %v", err)
		}
	}
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	_, err := r.Reconcile(context.Background(), request("production"))
	if err == nil {
		t.Fatal("the overlapping write was overwritten instead of conflicting: a merge patch of " +
			"status.history replaces the array, so the other writer's entry is gone")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("error = %v, want a conflict so controller-runtime requeues and folds again", err)
	}
	if got := readEnvironment(t, c, "production").Status.History; len(got) == 0 || got[0].Revision != "8-cafebabe" {
		t.Errorf("the other writer's entry did not survive: %+v", got)
	}
}

// --- the error taxonomy at the reconciler ------------------------------------

// TestDeliveryRefusalsRequeueAsTheTableSays is the whole point of the closed
// set: what a reader sees in a condition also tells them whether anything will
// happen next without them.
func TestDeliveryRefusalsRequeueAsTheTableSays(t *testing.T) {
	cases := []struct {
		reason  string
		requeue time.Duration
		errored bool
	}{
		{v1alpha1.ReasonFluxNotInstalled, operatorRetry, false},
		{v1alpha1.ReasonPushDenied, operatorRetry, false},
		{v1alpha1.ReasonFluxApplyForbidden, operatorRetry, false},
		{v1alpha1.ReasonFieldManagerConflict, operatorRetry, false},
		{v1alpha1.ReasonRegistryNotConfigured, 0, false},
		{v1alpha1.ReasonArtifactRefInvalid, 0, false},
		{v1alpha1.ReasonNameConflict, 0, false},
		{v1alpha1.ReasonRegistryUnreachable, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			c := newClient(t, validProject(), validEnvironment())
			r := &EnvironmentReconciler{
				Client: c, Profiles: StaticProfileSource{},
				Delivery: &spyDeliverer{err: newDeliveryError(tc.reason, "nope", nil)},
			}
			result, err := r.Reconcile(context.Background(), request("production"))
			if tc.errored != (err != nil) {
				t.Fatalf("error = %v, want errored=%v", err, tc.errored)
			}
			if result.RequeueAfter != tc.requeue {
				t.Errorf("requeue = %s, want %s", result.RequeueAfter, tc.requeue)
			}
			condition := ready(t, readEnvironment(t, c, "production").Status.Conditions)
			if condition.Status != metav1.ConditionFalse || condition.Reason != tc.reason {
				t.Errorf("Ready = %s/%s, want False/%s", condition.Status, condition.Reason, tc.reason)
			}
			// Nothing is in flight for any refusal, whether or not something
			// will be retried.
			if prog := progressing(t, readEnvironment(t, c, "production").Status.Conditions); prog.Status != metav1.ConditionFalse {
				t.Errorf("Progressing = %s, want False for a refusal", prog.Status)
			}
		})
	}
}

// TestMissingFluxNeverCrashLoops is the sentence the whole taxonomy exists for:
// a cluster with no Flux is the expected state of a fresh install, and a
// controller that crash-looped on it would make "kelson is broken" the first
// impression of a product whose install story is "offers, never assumes".
func TestMissingFluxNeverCrashLoops(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{
		Client: c, Profiles: StaticProfileSource{},
		Delivery: &spyDeliverer{err: newDeliveryError(v1alpha1.ReasonFluxNotInstalled,
			"no Flux; install it with `kelson install`", nil)},
	}
	result, err := r.Reconcile(context.Background(), request("production"))
	if err != nil {
		t.Fatalf("a cluster with no Flux must not produce an error return: %v", err)
	}
	if result.RequeueAfter != operatorRetry {
		t.Errorf("requeue = %s, want a %s timer", result.RequeueAfter, operatorRetry)
	}
	condition := ready(t, readEnvironment(t, c, "production").Status.Conditions)
	if !strings.Contains(condition.Message, "kelson install") {
		t.Errorf("the condition does not name the fix: %q", condition.Message)
	}
}

// --- phases and requeues -----------------------------------------------------

// TestNonTerminalPhasesRequeue: the watch on the Flux objects is what reports
// progress, and this is the belt under it — for an informer that has not
// synced, or an event dropped during a leader-election handover.
func TestPhaseDecidesTheRequeue(t *testing.T) {
	for phase, want := range map[string]time.Duration{
		v1alpha1.PhaseCommitted:   nonTerminalRequeue,
		v1alpha1.PhaseReconciling: nonTerminalRequeue,
		v1alpha1.PhaseApplied:     nonTerminalRequeue,
		v1alpha1.PhaseHealthy:     0,
		v1alpha1.PhaseRejected:    0,
		v1alpha1.PhaseDegraded:    0,
	} {
		c := newClient(t, validProject(), validEnvironment())
		r := &EnvironmentReconciler{
			Client: c, Profiles: StaticProfileSource{},
			Delivery: &spyDeliverer{outcome: Outcome{Revision: "1-abcd1234", Phase: phase, Published: true}},
		}
		result, err := r.Reconcile(context.Background(), request("production"))
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		if result.RequeueAfter != want {
			t.Errorf("%s requeues after %s, want %s", phase, result.RequeueAfter, want)
		}
	}
}

// TestPhaseTransitionsAreGuarded: an observation about a previous revision's
// lifecycle arriving after a new one started is normal, not a failure, so the
// impossible step is dropped and the status keeps the phase it had.
//
// The guard is about *one revision*, which is why the status here already names
// the revision the observation is about. Applying it across revisions is the bug
// TestANewRevisionRestartsThePhase covers.
func TestPhaseTransitionsAreGuarded(t *testing.T) {
	env := validEnvironment()
	env.Status.Phase = v1alpha1.PhaseHealthy
	env.Status.Revision = "1-abcd1234"
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{
		Client: c, Profiles: StaticProfileSource{},
		// Healthy cannot become Committed: a revision cannot become
		// uncommitted (internal/delivery/statemachine's table).
		Delivery: &spyDeliverer{outcome: Outcome{Revision: "1-abcd1234", Phase: v1alpha1.PhaseCommitted}},
	}
	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("an illegal transition must not fail the reconcile: %v", err)
	}
	if got := readEnvironment(t, c, "production").Status.Phase; got != v1alpha1.PhaseHealthy {
		t.Errorf("phase moved backwards to %q", got)
	}

	// And a legal one still moves.
	if got := nextPhase(v1alpha1.PhaseHealthy, v1alpha1.PhaseDegraded); got != v1alpha1.PhaseDegraded {
		t.Errorf("Healthy → Degraded was refused, got %q", got)
	}
	if got := nextPhase("", v1alpha1.PhaseCommitted); got != v1alpha1.PhaseCommitted {
		t.Errorf("the first observation was dropped, got %q", got)
	}
}

// TestANewRevisionRestartsThePhase is the bug the guard above caused when it was
// applied to every observation rather than to observations of one revision.
//
// statemachine's table has Healthy → {Degraded} and knows nothing about
// revisions, so once an environment reached Healthy every later phase was an
// illegal transition and was dropped: a spec edit published revision 2 and the
// status still said Healthy, from the instant of the publish, whatever Flux
// then did with it. Everything downstream believed it — requeueFor saw a
// terminal phase and stopped polling, and the server, the CLI and the UI all
// reported the new revision as live before anything had looked at it.
func TestANewRevisionRestartsThePhase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome Outcome
		want    string
		requeue time.Duration
	}{
		{
			name:    "a freshly published revision is not live yet",
			outcome: Outcome{Revision: "8-99887766", Phase: v1alpha1.PhaseCommitted, Published: true},
			want:    v1alpha1.PhaseCommitted,
			requeue: nonTerminalRequeue,
		},
		{
			// The case that mattered most: Flux refused the new revision and the
			// environment kept reporting the old one as healthy.
			name:    "a revision Flux refused says so",
			outcome: Outcome{Revision: "8-99887766", Phase: v1alpha1.PhaseRejected, Published: true},
			want:    v1alpha1.PhaseRejected,
			requeue: 0,
		},
		{
			// A rollback repoints the pair, so the pinned revision's lifecycle
			// starts again too — even though nothing was published.
			name:    "a rollback target starts its own lifecycle",
			outcome: Outcome{Revision: "6-9f0a1b2c", Phase: v1alpha1.PhaseReconciling, RolledBack: true},
			want:    v1alpha1.PhaseReconciling,
			requeue: nonTerminalRequeue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := deliveredEnvironment()
			env.Generation = 8
			c := newClient(t, validProject(), env)
			r := &EnvironmentReconciler{
				Client: c, Profiles: StaticProfileSource{},
				Delivery: &spyDeliverer{outcome: tc.outcome},
			}
			result, err := r.Reconcile(context.Background(), request("production"))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got := readEnvironment(t, c, "production")
			if got.Status.Phase != tc.want {
				t.Errorf("phase = %q, want %q: the status froze on the previous revision's phase",
					got.Status.Phase, tc.want)
			}
			if got.Status.Revision != tc.outcome.Revision {
				t.Errorf("revision = %q, want %q", got.Status.Revision, tc.outcome.Revision)
			}
			if result.RequeueAfter != tc.requeue {
				t.Errorf("requeue = %s, want %s: a frozen terminal phase also kills the poll",
					result.RequeueAfter, tc.requeue)
			}
		})
	}

	// And the unit underneath, stated directly: same revision, the guard;
	// different revision, the observation.
	if got := phaseFor(v1alpha1.PhaseHealthy, "7-1a2b3c4d",
		Outcome{Revision: "7-1a2b3c4d", Phase: v1alpha1.PhaseCommitted}); got != v1alpha1.PhaseHealthy {
		t.Errorf("phaseFor let one revision move backwards: %q", got)
	}
	if got := phaseFor(v1alpha1.PhaseHealthy, "7-1a2b3c4d",
		Outcome{Revision: "8-99887766", Phase: v1alpha1.PhaseCommitted}); got != v1alpha1.PhaseCommitted {
		t.Errorf("phaseFor froze a new revision at %q", got)
	}
	// A phase the table does not know is not written into the status at all.
	if got := phaseFor(v1alpha1.PhaseHealthy, "7-1a2b3c4d",
		Outcome{Revision: "8-99887766", Phase: "Ascendant"}); got != v1alpha1.PhaseHealthy {
		t.Errorf("phaseFor wrote an unknown phase: %q", got)
	}
}

// --- both conditions, on every refusal ----------------------------------------

// TestARefusalClearsAStaleProgressing: a refusal that writes only Ready leaves
// whatever Progressing the last reconcile wrote standing.
//
// That is not cosmetic. "Settled" is read elsewhere as *Progressing exists and
// is not True* (internal/api's deploy stream), so a stale Progressing=True hangs
// a caller waiting on this generation for its whole budget and then reports a
// timeout instead of the refusal it was already holding — and an Environment
// that has never delivered has no Progressing condition at all, which hangs the
// same way.
func TestARefusalClearsAStaleProgressing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    func() *v1alpha1.Environment
		reason string
	}{
		{
			name: "a render kelson cannot do",
			env: func() *v1alpha1.Environment {
				return environment("production", model.EnvironmentSpec{
					Project:  "checkout",
					Overlays: []model.Overlay{{Patch: "overlays/patch.yaml"}},
				})
			},
			reason: v1alpha1.ReasonRenderFailed,
		},
		{
			name: "a spec that does not validate",
			env: func() *v1alpha1.Environment {
				return environment("production", model.EnvironmentSpec{
					Project:    "checkout",
					Components: []model.ComponentOverride{{Name: "does-not-exist", Image: "ghcr.io/acme/x:1"}},
				})
			},
			reason: v1alpha1.ReasonSpecInvalid,
		},
		{
			name: "a Project that is not there",
			env: func() *v1alpha1.Environment {
				return environment("production", model.EnvironmentSpec{Project: "no-such-project"})
			},
			reason: v1alpha1.ReasonProjectNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env()
			// The state a previous reconcile of a healthy deploy left behind.
			setProgressing(&env.Status.Conditions, env.Generation, metav1.ConditionTrue,
				v1alpha1.ReasonReconciling, "waiting for Flux to reconcile revision 1-abcd1234")
			c := newClient(t, validProject(), env)
			r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}

			if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got := readEnvironment(t, c, "production")
			if condition := ready(t, got.Status.Conditions); condition.Reason != tc.reason {
				t.Fatalf("Ready reason = %q, want %q", condition.Reason, tc.reason)
			}
			prog := progressing(t, got.Status.Conditions)
			if prog.Status != metav1.ConditionFalse {
				t.Errorf("Progressing = %s/%s after a refusal, want False: nothing is in flight, and a "+
					"caller waiting on this generation would hang for its whole budget",
					prog.Status, prog.Reason)
			}
		})
	}
}

// TestAProfileFailureIsNotReportedAsAMissingFlux: a probe that failed and a
// cluster with no Flux are the same empty profile and mean opposite things, and
// only one of them is fixed by `kelson install`.
func TestAProfileFailureIsNotReportedAsAMissingFlux(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: failingProfile{}}

	if _, err := r.Reconcile(context.Background(), request("production")); err == nil {
		t.Fatal("a profile that could not be read must be retried")
	}
	got := readEnvironment(t, c, "production")
	condition := ready(t, got.Status.Conditions)
	if condition.Reason != v1alpha1.ReasonClusterProfileUnavailable {
		t.Errorf("Ready reason = %q, want %q", condition.Reason, v1alpha1.ReasonClusterProfileUnavailable)
	}
	if !strings.Contains(condition.Message, "unreachable") {
		t.Errorf("the condition does not relay why the probe failed: %q", condition.Message)
	}
	if prog := progressing(t, got.Status.Conditions); prog.Status != metav1.ConditionFalse {
		t.Errorf("Progressing = %s, want False: nothing is in flight while the cluster cannot be read", prog.Status)
	}
}

// TestRollbackRefusalLeavesACoherentStatus is the controller half of a refusal
// the *server* has to be able to read.
//
// followRollback gates on status.rollbackRevision naming the target before it
// looks at Ready, and a refusal returns before that field is written — so the
// pair a caller can actually see has to be enough on its own: Ready=False with
// RollbackTargetUnknown, and Progressing=False so "settled" is true and the
// stream stops waiting rather than timing out on a verdict already recorded.
func TestRollbackRefusalLeavesACoherentStatus(t *testing.T) {
	env := deliveredEnvironment()
	env.Annotations = map[string]string{v1alpha1.AnnotationRollbackTo: "3-deadbeef"}
	// A rollback is requested while the last deploy is still in flight, which is
	// exactly when a stale Progressing=True would be left behind.
	setProgressing(&env.Status.Conditions, env.Generation, metav1.ConditionTrue,
		v1alpha1.ReasonReconciling, "waiting for Flux to reconcile revision 7-1a2b3c4d")

	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: &spyDeliverer{}}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := readEnvironment(t, c, "production")
	condition := ready(t, got.Status.Conditions)
	if condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonRollbackTargetUnknown {
		t.Fatalf("Ready = %s/%s, want False/%s", condition.Status, condition.Reason,
			v1alpha1.ReasonRollbackTargetUnknown)
	}
	prog := progressing(t, got.Status.Conditions)
	if prog.Status != metav1.ConditionFalse {
		t.Fatalf("Progressing = %s/%s, want False: the refusal is the verdict, and a caller that waits "+
			"for one will wait out its whole budget", prog.Status, prog.Reason)
	}
	// And the refusal changed nothing else: the environment is still serving
	// what it was serving.
	if got.Status.Revision != "7-1a2b3c4d" || got.Status.RollbackRevision != "" {
		t.Errorf("a refused rollback moved the environment: revision %q, rollbackRevision %q",
			got.Status.Revision, got.Status.RollbackRevision)
	}
}

// --- the finalizer -----------------------------------------------------------

// TestFinalizerIsAddedAfterTheFirstSuccessfulEnsure: adding it earlier would
// put a deletion blocker on an object with nothing to clean up, and an
// Environment whose spec never validated would need it stripped by hand.
func TestFinalizerIsAddedAfterTheFirstSuccessfulEnsure(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{
		Client: c, Profiles: StaticProfileSource{},
		Delivery: &spyDeliverer{outcome: Outcome{Revision: "1-abcd1234", Published: true}},
	}
	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatal(err)
	}
	if got := readEnvironment(t, c, "production").Finalizers; len(got) != 1 || got[0] != Finalizer {
		t.Fatalf("finalizers = %v, want [%s]", got, Finalizer)
	}
}

func TestFinalizerIsNotAddedWhenDeliveryRefused(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{
		Client: c, Profiles: StaticProfileSource{},
		Delivery: &spyDeliverer{err: newDeliveryError(v1alpha1.ReasonRegistryNotConfigured, "no registry", nil)},
	}
	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatal(err)
	}
	if got := readEnvironment(t, c, "production").Finalizers; len(got) != 0 {
		t.Errorf("finalizers = %v; an Environment that never published must delete without one", got)
	}
}

// TestDeletionBeforeTheFinalizerStillTearsDown closes the window the finalizer
// cannot cover.
//
// The finalizer is added only after the first successful apply (ADR-0028's
// amendment, and TestFinalizerIsAddedAfterTheFirstSuccessfulEnsure says why). A
// delete that lands between the two takes the custom resource immediately —
// there is nothing to block it — so finalize never runs, and a `prune: true`
// Kustomization plus everything it applied keep running with nothing in the
// cluster saying whose they were. The reconcile that lost the race tears the
// pair down itself.
func TestDeletionBeforeTheFinalizerStillTearsDown(t *testing.T) {
	deleted := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(validProject(), validEnvironment()).
		WithStatusSubresource(&v1alpha1.Project{}, &v1alpha1.Environment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			// The delete arrives while the finalizer patch is in flight, which
			// is the whole window: the object has no finalizer yet, so it goes
			// at once and the patch comes back NotFound.
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption) error {
				if env, ok := obj.(*v1alpha1.Environment); ok && !deleted {
					deleted = true
					if err := cl.Delete(ctx, env.DeepCopy()); err != nil {
						return err
					}
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	spy := &spyDeliverer{outcome: Outcome{Revision: "1-abcd1234", Phase: v1alpha1.PhaseCommitted, Published: true}}
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("the pair was applied %d times, want 1 — the test does not exercise the window otherwise",
			spy.calls)
	}
	if len(spy.tornDown) != 1 || spy.tornDown[0] != "checkout/production" {
		t.Fatalf("teardown was called with %v; the Flux pair outlives the Environment that declared it "+
			"and nothing will ever sweep it", spy.tornDown)
	}
	var env v1alpha1.Environment
	if err := c.Get(context.Background(), request("production").NamespacedName, &env); err == nil {
		t.Errorf("the environment is still there (%v); the test's premise is wrong", env.Finalizers)
	}
}

// TestDeletionTearsDownThenReleases is the deletion path. The sharp edge is
// stated in ADR-0028's amendment and in docs/delivery.md: deleting an
// Environment deletes its workloads, because deleting the Kustomization prunes
// what it applied.
func TestDeletionTearsDownThenReleases(t *testing.T) {
	env := deliveredEnvironment()
	now := metav1.Now()
	env.DeletionTimestamp = &now

	spy := &spyDeliverer{}
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(spy.tornDown) != 1 || spy.tornDown[0] != "checkout/production" {
		t.Fatalf("teardown was called with %v", spy.tornDown)
	}
	if spy.calls != 0 {
		t.Error("a deleting Environment was reconciled forward as well as torn down")
	}
	// The finalizer is gone, so the API server may now remove the object.
	var got v1alpha1.Environment
	err := c.Get(context.Background(), request("production").NamespacedName, &got)
	if err == nil && len(got.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want none once the teardown succeeded", got.Finalizers)
	}
}

// TestDeletionKeepsTheFinalizerWhileTeardownIsRefused: an object stuck in
// Terminating is the honest state while its workloads are still running.
func TestDeletionKeepsTheFinalizerWhileTeardownIsRefused(t *testing.T) {
	env := deliveredEnvironment()
	now := metav1.Now()
	env.DeletionTimestamp = &now

	spy := &spyDeliverer{teardown: newDeliveryError(v1alpha1.ReasonFluxApplyForbidden, "no RBAC", nil)}
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	result, err := r.Reconcile(context.Background(), request("production"))
	if err != nil {
		t.Fatalf("a refused teardown must not crash-loop: %v", err)
	}
	if result.RequeueAfter != operatorRetry {
		t.Errorf("requeue = %s, want %s", result.RequeueAfter, operatorRetry)
	}
	if got := readEnvironment(t, c, "production").Finalizers; len(got) != 1 {
		t.Errorf("finalizers = %v; the blocker must stay while the workloads are still there", got)
	}
}

// TestDeletionOfAnInvalidSpecStillWorks: deletion is decided before validation,
// because an Environment whose spec stopped validating must still be deletable.
func TestDeletionOfAnInvalidSpecStillWorks(t *testing.T) {
	env := deliveredEnvironment()
	env.Spec.Project = "no-such-project"
	now := metav1.Now()
	env.DeletionTimestamp = &now

	spy := &spyDeliverer{}
	c := newClient(t, env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(spy.tornDown) != 1 {
		t.Errorf("teardown was not attempted for an Environment with no Project: %v", spy.tornDown)
	}
}

// --- the reverse mapping -----------------------------------------------------

// TestEnvironmentOfFluxObject: a Kustomization going Ready is what turns an
// Environment Healthy, and the labels are the only handle — an owner reference
// cannot cross namespaces, which is also why the finalizer exists.
func TestEnvironmentOfFluxObject(t *testing.T) {
	object := func(labels map[string]string) client.Object {
		env := validEnvironment()
		env.SetLabels(labels)
		return env
	}
	got := environmentOfFluxObject(context.Background(), object(map[string]string{
		delivery.LabelManagedBy:            delivery.ManagedByKelson,
		delivery.LabelEnvironment:          "production",
		delivery.LabelEnvironmentNamespace: "checkout",
	}))
	if len(got) != 1 || got[0].Name != "production" || got[0].Namespace != "checkout" {
		t.Fatalf("got %v, want checkout/production", got)
	}

	// Somebody else's Kustomization enqueues nothing.
	if got := environmentOfFluxObject(context.Background(), object(map[string]string{
		delivery.LabelEnvironment: "production", delivery.LabelEnvironmentNamespace: "checkout",
	})); len(got) != 0 {
		t.Errorf("an object that is not kelson's enqueued %v", got)
	}
	// A half-labelled object is not guessed at.
	if got := environmentOfFluxObject(context.Background(), object(map[string]string{
		delivery.LabelManagedBy: delivery.ManagedByKelson, delivery.LabelEnvironment: "production",
	})); len(got) != 0 {
		t.Errorf("an object with no environment-namespace enqueued %v", got)
	}
}
