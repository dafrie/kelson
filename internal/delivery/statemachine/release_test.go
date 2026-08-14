package statemachine_test

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
)

// The release-command hook in the state machine's vocabulary (issue #104,
// ADR-0019).
//
// A migration does NOT get a phase of its own. The seven phases are shared with
// the CLI, the API and the UI, and an eighth one would mean every consumer of
// delivery.Phase has to learn a word that only one delivery mode can ever
// report. What a migration is, in the vocabulary that already exists, is:
//
//   - running: Reconciling — something is actively working on this revision,
//     with the Job named in Cause and Detail;
//   - failed: Rejected — the revision was processed and refused, it is not
//     live, and the previous one is still serving. That is the definition of
//     Rejected, and State.Err() already says the right sentence for it.
//
// These tests pin that mapping, because it is the half of the design a future
// reader is most likely to want to "fix" by adding a phase.

// releaseStatus is what a delivery plane reports while a release
// command runs (its Progress sink), reproduced here so the two halves of the
// contract are asserted against the same shape.
func releaseStatus(p delivery.Phase, cause, state string) delivery.Status {
	return delivery.Status{
		Phase:    p,
		Revision: targetRev,
		Cause:    cause,
		Detail: map[string]string{
			"releaseJob":   "release-web-5be0c165",
			"releaseState": state,
		},
	}
}

// TestReleaseCommandProgressesThenSettlesHealthy is the successful path: the
// migration reads as progress, not as a wait for something else, and the
// deployment settles exactly as it would without a release hook.
func TestReleaseCommandProgressesThenSettlesHealthy(t *testing.T) {
	rec := &recorder{}
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		releaseStatus(delivery.PhaseReconciling, "direct: release job release-web-5be0c165 is running", "running"),
		releaseStatus(delivery.PhaseReconciling, "direct: release job release-web-5be0c165 completed", "succeeded"),
		status(delivery.PhaseHealthy, "", map[string]string{"ready": "3/3"}),
	}, hold: true}

	engine := newEngine(t, statemachine.Config{Source: src, Component: "direct", OnState: rec.on})
	state, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Answer() != statemachine.AnswerLive {
		t.Fatalf("answer = %s, want %s", state.Answer(), statemachine.AnswerLive)
	}
	if got := rec.seen(); !containsPhase(got, delivery.PhaseReconciling) {
		t.Fatalf("a running migration did not read as progress: %v", got)
	}
}

// TestFailedReleaseCommandIsRejectedAndNamesTheJob: the answer a human gets is
// "rejected", the cause names the Job, and the structured error says the change
// is not live — which is the whole point of failing before the workloads roll.
func TestFailedReleaseCommandIsRejectedAndNamesTheJob(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		releaseStatus(delivery.PhaseReconciling, "direct: release job release-web-5be0c165 is running", "running"),
		releaseStatus(delivery.PhaseRejected,
			`direct: release job release-web-5be0c165 failed: the release command failed: BackoffLimitExceeded`,
			"failed"),
	}, hold: true}

	engine := newEngine(t, statemachine.Config{Source: src, Component: "direct"})
	state, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Answer() != statemachine.AnswerRejected {
		t.Fatalf("answer = %s, want %s", state.Answer(), statemachine.AnswerRejected)
	}
	if !strings.Contains(state.Cause.String(), "release-web-5be0c165") {
		t.Errorf("the cause does not name the Job: %s", state.Cause)
	}
	if state.Detail["releaseJob"] != "release-web-5be0c165" {
		t.Errorf("the Job is not machine-readable in Detail: %v", state.Detail)
	}
	err = state.Err()
	if err == nil {
		t.Fatal("a failed migration produced no structured error")
	}
	var de delivery.Error
	if !asDeliveryError(err, &de) {
		t.Fatalf("error %v does not carry the delivery taxonomy", err)
	}
	if !strings.Contains(de.Message, "rejected") || !strings.Contains(de.Remediation, "not live") {
		t.Errorf("the error does not say the change never went live: %+v", de)
	}
}

// TestReleaseCommandStuckWaitIsNotHealthy: a migration that never finishes is a
// stuck deployment, reported against the phase it is wedged in rather than as a
// success nobody watched.
func TestReleaseCommandStuckWaitIsNotHealthy(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		releaseStatus(delivery.PhaseReconciling, "direct: release job release-web-5be0c165 is running", "running"),
	}, hold: true}

	engine := newEngine(t, statemachine.Config{Source: src, Component: "direct", Timeout: stuckTimeout})
	state, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Answer() != statemachine.AnswerStuck {
		t.Fatalf("answer = %s, want %s", state.Answer(), statemachine.AnswerStuck)
	}
	if state.Phase != delivery.PhaseReconciling {
		t.Fatalf("stuck in %s, want %s — the phase it is wedged in IS the diagnosis",
			state.Phase, delivery.PhaseReconciling)
	}
}

func containsPhase(phases []delivery.Phase, want delivery.Phase) bool {
	for _, p := range phases {
		if p == want {
			return true
		}
	}
	return false
}

func asDeliveryError(err error, out *delivery.Error) bool {
	de, ok := err.(delivery.Error)
	if !ok {
		return false
	}
	*out = de
	return true
}
