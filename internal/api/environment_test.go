package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
)

// TestAnnotationKeysMatchTheCustomResource: this plane spells the two ADR-0028
// annotations itself, because it holds no Kubernetes types — so the copy is
// asserted against the resource's own, exactly as api/kelson/v1alpha1's
// phase_test asserts the phase vocabulary against the delivery plane's. A
// mistyped key here would be a rollback the controller never sees.
func TestAnnotationKeysMatchTheCustomResource(t *testing.T) {
	if annotationRollbackTo != v1alpha1.AnnotationRollbackTo {
		t.Errorf("rollback annotation = %q, want %q", annotationRollbackTo, v1alpha1.AnnotationRollbackTo)
	}
	// The promotion stamp has no constant on the resource (nothing in the
	// controller reads it — it is provenance for humans and `kubectl get`), so
	// what is asserted is the spelling docs/model.md and ADR-0028 decision 6
	// both state.
	if annotationPromotedFrom != "kelson.dev/promoted-from" {
		t.Errorf("promotion annotation = %q", annotationPromotedFrom)
	}
}

// TestRollbackRefusalReasonMatchesTheCustomResource: followRollback terminates
// on the controller's own refusal reason, which this plane also spells itself.
// A mistyped copy would restore the timeout this check exists to remove.
func TestRollbackRefusalReasonMatchesTheCustomResource(t *testing.T) {
	if reasonRollbackTargetUnknown != v1alpha1.ReasonRollbackTargetUnknown {
		t.Errorf("refusal reason = %q, want %q", reasonRollbackTargetUnknown, v1alpha1.ReasonRollbackTargetUnknown)
	}
}

// TestHistoryBoundMatchesTheCustomResource: this plane decides whether a failed
// registry query is worth refusing a History over by asking whether the mirror
// is full, so a copy that drifted from the controller's bound would make that
// judgement about the wrong number.
func TestHistoryBoundMatchesTheCustomResource(t *testing.T) {
	if maxHistoryEntries != v1alpha1.MaxHistoryEntries {
		t.Errorf("history bound = %d, want %d", maxHistoryEntries, v1alpha1.MaxHistoryEntries)
	}
}

// TestPhasesMatchTheCustomResource: the wire's phase strings come from
// internal/delivery and the controller writes api/kelson/v1alpha1's. The two
// lists are asserted identical in the resource's own package; this is the third
// corner of the same triangle, because it is this package that passes one
// through as the other.
func TestPhasesMatchTheCustomResource(t *testing.T) {
	for _, pair := range [][2]string{
		{string(delivery.PhaseHealthy), v1alpha1.PhaseHealthy},
		{string(delivery.PhaseRejected), v1alpha1.PhaseRejected},
		{string(delivery.PhaseDegraded), v1alpha1.PhaseDegraded},
		{string(delivery.PhaseCommitted), v1alpha1.PhaseCommitted},
	} {
		if pair[0] != pair[1] {
			t.Errorf("phase %q is spelled %q on the custom resource", pair[0], pair[1])
		}
	}
}

// TestSettledErrorPrefersTheSpecsOwnTaxonomy: an author whose spec is wrong
// must read `schema/unknown-field`, not `delivery/apply-failed`. There is one
// taxonomy and the status carries it intact (ADR-0027 decision 5).
func TestSettledErrorPrefersTheSpecsOwnTaxonomy(t *testing.T) {
	st := controlstore.EnvironmentState{
		Project: "hello", Environment: "development",
		Generation: 1, ObservedGeneration: 1,
		Conditions: []controlstore.Condition{
			{Type: "Ready", Status: "False", Reason: "SpecInvalid", Message: "2 errors"},
		},
		ValidationErrors: model.Errors{
			{Code: model.ErrUnknownField, Field: "$.spec.nope", Message: "unknown field"},
			{Code: model.ErrUnknownField, Field: "$.spec.other", Message: "unknown field"},
		},
	}
	wire := wireError(settledError(st, deliveryState(st, false)))
	if wire.GetCode() != string(model.ErrUnknownField) {
		t.Fatalf("code = %q, want the spec's own", wire.GetCode())
	}
	// One wire slot, two findings: the count survives, because a spec with two
	// errors must not look like a spec with one.
	if want := "(and 1 more)"; !strings.Contains(wire.GetMessage(), want) {
		t.Errorf("message = %q, want it to carry %q", wire.GetMessage(), want)
	}
}

// A delivery refusal the state machine has no phase for — no registry, no Flux,
// a missing Project — is still a delivery failure, and the controller's own
// reason rides along as the cause rather than being translated away.
func TestSettledErrorCarriesTheControllersReason(t *testing.T) {
	st := controlstore.EnvironmentState{
		Project: "hello", Environment: "development",
		Generation: 1, ObservedGeneration: 1,
		Conditions: []controlstore.Condition{
			{Type: "Ready", Status: "False", Reason: v1alpha1.ReasonRegistryNotConfigured,
				Message: "this controller was started without a registry"},
		},
	}
	err := settledError(st, deliveryState(st, false))
	if err == nil {
		t.Fatal("a Ready=False environment settled with no error")
	}
	var de delivery.Error
	if !errors.As(err, &de) {
		t.Fatalf("err = %T, want a delivery.Error", err)
	}
	if de.Cause != v1alpha1.ReasonRegistryNotConfigured {
		t.Errorf("cause = %q, want the controller's reason", de.Cause)
	}
	if de.Code != delivery.ErrApplyFailed {
		t.Errorf("code = %q", de.Code)
	}
}

// A healthy environment settles with nothing: the absence of an error is the
// answer, and inventing one for a successful deployment would make every
// caller's error check useless.
func TestSettledErrorOfAHealthyEnvironmentIsNil(t *testing.T) {
	st := healthyEnvironment("hello", "development", "1-abcdef01")
	if err := settledError(st, deliveryState(st, false)); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
