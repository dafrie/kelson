package v1alpha1

import (
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
)

// TestPhaseVocabularyMatchesStateMachine is the drift gate on the one thing
// status.go copies rather than imports.
//
// EnvironmentStatus.Phase is the state machine's phase (ADR-0028 decision 1,
// step 6), but internal/delivery cannot appear in a public package's contract,
// so the constants are restated here. A restated vocabulary that nothing
// compares is a vocabulary that grows a seventh value on one side and reports
// an empty phase on the other.
func TestPhaseVocabularyMatchesStateMachine(t *testing.T) {
	// The pairs, in the order the state machine declares them.
	pairs := []struct {
		here  string
		there delivery.Phase
	}{
		{PhaseProposed, delivery.PhaseProposed},
		{PhaseCommitted, delivery.PhaseCommitted},
		{PhaseReconciling, delivery.PhaseReconciling},
		{PhaseApplied, delivery.PhaseApplied},
		{PhaseHealthy, delivery.PhaseHealthy},
		{PhaseRejected, delivery.PhaseRejected},
		{PhaseDegraded, delivery.PhaseDegraded},
	}
	for _, p := range pairs {
		if p.here != string(p.there) {
			t.Errorf("phase %q here is %q in internal/delivery", p.here, p.there)
		}
		if !statemachine.Known(p.there) {
			t.Errorf("phase %q is not known to the state machine", p.there)
		}
	}

	// And the other direction: a phase added to the state machine and not here
	// would be a value the status can hold and the CRD's printer column cannot
	// explain.
	known := map[string]bool{}
	for _, p := range pairs {
		known[p.here] = true
	}
	for _, p := range []delivery.Phase{
		delivery.PhaseProposed, delivery.PhaseCommitted, delivery.PhaseReconciling,
		delivery.PhaseApplied, delivery.PhaseHealthy, delivery.PhaseRejected,
		delivery.PhaseDegraded,
	} {
		if !known[string(p)] {
			t.Errorf("internal/delivery has phase %q with no constant in this package", p)
		}
	}
}
