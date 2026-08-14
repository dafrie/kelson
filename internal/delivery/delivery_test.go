package delivery

import (
	"errors"
	"strings"
	"testing"
)

// The Registry/Adapter/Capabilities tests that lived here went with the seam
// they tested (ADR-0028 decision 9): there is one delivery path now, so there
// is nothing to select between and no capability to negotiate. What remains is
// the error taxonomy, which every plane still speaks.

func TestConflictErrorIsLoud(t *testing.T) {
	err := Conflict("Project/checkout", "$.spec", "concurrent edit detected", "re-read the latest spec and reapply")
	var de Error
	if !errors.As(err, &de) {
		t.Fatalf("expected delivery.Error, got %T", err)
	}
	if de.Code != ErrConflicted {
		t.Fatalf("got code %q", de.Code)
	}
	if !AsConflict(err) {
		t.Fatal("AsConflict should report true")
	}
	if de.DocsURL == "" {
		t.Fatal("structured error must carry a docs URL")
	}
}

// A gated capability must be recognisable as "not yet" rather than "it failed",
// and must name where the work is tracked. That is the whole contract the
// deleted verbs lean on: an agent branching on the code stops instead of
// retrying, and a human reading the message knows when the answer changes.
func TestNotImplementedNamesItsTrackingIssue(t *testing.T) {
	err := NotImplemented("deploy", "kelson cannot apply a rendered set", "#224")
	var de Error
	if !errors.As(err, &de) {
		t.Fatalf("expected delivery.Error, got %T", err)
	}
	if de.Code != ErrNotImplemented {
		t.Fatalf("got code %q, want %q", de.Code, ErrNotImplemented)
	}
	if !AsNotImplemented(err) {
		t.Fatal("AsNotImplemented should report true")
	}
	if AsApplyFailed(err) || AsUnsupported(err) {
		t.Fatal("a not-implemented refusal must not be mistaken for a failure or a capability mismatch")
	}
	if !strings.Contains(de.Remediation, "#224") {
		t.Errorf("the remediation must name the tracking issue, got: %s", de.Remediation)
	}
	if de.DocsURL == "" {
		t.Fatal("structured error must carry a docs URL")
	}
}
