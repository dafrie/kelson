package v1alpha1

import (
	"testing"

	"github.com/dafrie/kelson/internal/observation"
)

// TestWorkloadVocabularyMatchesObservation is the drift gate on the second
// thing status.go copies rather than imports (issue #240).
//
// It is [TestPhaseVocabularyMatchesStateMachine] for the health codes:
// `status.workloads[].code` is internal/observation's Code, internal/observation
// cannot appear in a public package's contract, so the constants are restated —
// and a restated vocabulary that nothing compares is one that grows a value on
// one side and writes a code nobody can branch on into the other.
func TestWorkloadVocabularyMatchesObservation(t *testing.T) {
	pairs := []struct {
		here  string
		there observation.Code
	}{
		{WorkloadHealthy, observation.CodeHealthy},
		{WorkloadProgressing, observation.CodeProgressing},
		{WorkloadCrashLoopBackOff, observation.CodeCrashLoopBackOff},
		{WorkloadImagePullBackOff, observation.CodeImagePullBackOff},
		{WorkloadFailingProbe, observation.CodeFailingProbe},
		{WorkloadInsufficientResources, observation.CodeInsufficientResources},
		{WorkloadSchedulingFailed, observation.CodeSchedulingFailed},
		{WorkloadMissing, observation.CodeMissing},
		{WorkloadSecretSyncFailed, observation.CodeSecretSyncFailed},
	}
	for _, p := range pairs {
		if p.here != string(p.there) {
			t.Errorf("workload code %q here is %q in internal/observation", p.here, p.there)
		}
	}

	// And the other direction, for the half that matters most: every code
	// internal/observation calls a *failure* must have a constant here, because
	// those are the ones that reach `status.workloads.unhealthy[].code`. The
	// wait codes are checked above; a new failure code with no constant here is
	// a diagnosis the status would carry and no reader could name.
	known := map[string]bool{}
	for _, p := range pairs {
		known[p.here] = true
	}
	for _, c := range []observation.Code{
		observation.CodeCrashLoopBackOff, observation.CodeImagePullBackOff,
		observation.CodeFailingProbe, observation.CodeInsufficientResources,
		observation.CodeSchedulingFailed, observation.CodeSecretSyncFailed,
	} {
		if !observation.IsFailure(c) {
			t.Errorf("internal/observation no longer treats %q as a failure; this list is stale", c)
		}
		if !known[string(c)] {
			t.Errorf("internal/observation has failure code %q with no constant in this package", c)
		}
	}
}
