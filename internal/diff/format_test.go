package diff_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/diff"
)

func sampleDiff() *diff.Diff {
	return &diff.Diff{
		Level:       diff.LevelRendered,
		Project:     "checkout",
		Environment: "production",
		Resources: []diff.ResourceDiff{
			{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "web",
				Namespace:  "checkout-prod",
				Op:         diff.OpModified,
				Risk:       diff.RiskRestart,
				Fields: []diff.FieldDiff{
					{
						Path:   "spec.template.spec.containers[0].env[LOG_LEVEL].value",
						Before: "info",
						After:  "debug",
						Origin: diff.OriginSpec,
						Risk:   diff.RiskRestart,
					},
				},
			},
			{
				APIVersion: "networking.k8s.io/v1",
				Kind:       "NetworkPolicy",
				Name:       "web-ingress",
				Namespace:  "checkout-prod",
				Op:         diff.OpAdded,
				Risk:       diff.RiskAdditive,
			},
		},
		Summary: diff.Summary{Added: 1, Modified: 1, Removed: 0, Restarting: []string{"web"}, MaxRisk: diff.RiskRestart},
	}
}

// TestEncodeJSONRoundTrip: the JSON encoder is machine-readable — it must
// encode the full Diff and decode back losslessly.
func TestEncodeJSONRoundTrip(t *testing.T) {
	orig := sampleDiff()
	b, err := diff.EncodeJSON(orig)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("EncodeJSON produced invalid JSON: %s", b)
	}
	var got diff.Diff
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Level != orig.Level || got.Project != orig.Project || got.Environment != orig.Environment {
		t.Fatalf("headers not round-tripped: %+v", got)
	}
	if len(got.Resources) != len(orig.Resources) {
		t.Fatalf("resource count = %d, want %d", len(got.Resources), len(orig.Resources))
	}
	if len(got.Resources[0].Fields) != 1 || got.Resources[0].Fields[0].Path != "spec.template.spec.containers[0].env[LOG_LEVEL].value" {
		t.Fatalf("field not round-tripped: %+v", got.Resources[0].Fields)
	}
	if got.Summary.MaxRisk != diff.RiskRestart || got.Summary.Restarting[0] != "web" {
		t.Fatalf("summary not round-tripped: %+v", got.Summary)
	}
}

// TestEncodeJSONDeterministic: diffing the same data N times (here the same
// prebuilt Diff) must produce identical bytes.
func TestEncodeJSONDeterministic(t *testing.T) {
	first, err := diff.EncodeJSON(sampleDiff())
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	for i := 0; i < 25; i++ {
		got, err := diff.EncodeJSON(sampleDiff())
		if err != nil {
			t.Fatalf("EncodeJSON run %d: %v", i, err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("EncodeJSON run %d differed from first (determinism violated)", i)
		}
	}
}

// TestWritePlainHasNoANSI: the terminal renderer degrades to plain text when
// colour is disabled — no escape sequences leak into the byte stream.
func TestWritePlainHasNoANSI(t *testing.T) {
	var buf bytes.Buffer
	if err := diff.Write(&buf, sampleDiff(), false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("\x1b[")) {
		t.Fatalf("plain output must not contain ANSI escapes:\n%q", buf.String())
	}
	// Determinism across runs.
	second := buf.String()
	for i := 0; i < 5; i++ {
		var again bytes.Buffer
		_ = diff.Write(&again, sampleDiff(), false)
		if again.String() != second {
			t.Fatalf("plain render not deterministic")
		}
	}
}

// TestWriteColoredHasANSI: with a colour flag the risk is highlighted; the
// bytes are still deterministic.
func TestWriteColoredHasANSI(t *testing.T) {
	var buf bytes.Buffer
	if err := diff.Write(&buf, sampleDiff(), true); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("\x1b[")) {
		t.Fatalf("colored render must contain ANSI escapes")
	}
	var again bytes.Buffer
	_ = diff.Write(&again, sampleDiff(), true)
	if again.String() != buf.String() {
		t.Fatalf("colored render not deterministic")
	}
}

// TestDefaultColorNoColorEnv: NO_COLOR forces plain text regression (issue #44
// "degrade to plain text when NO_COLOR is set").
func TestDefaultColorNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	if diff.DefaultColor(&buf) {
		t.Fatalf("DefaultColor must be false when NO_COLOR is set")
	}
}

// TestDefaultColorNonTerminal: a non-char-device writer is not a TTY, so the
// renderer must degrade to plain text.
func TestDefaultColorNonTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	if diff.DefaultColor(&buf) {
		t.Fatalf("DefaultColor must be false for a non-terminal writer")
	}
	// The dummy is cleared after; confirm we did not accidentally return true.
	if os.Getenv("NO_COLOR") != "" {
		t.Fatalf("NO_COLOR should be cleared")
	}
}

// serverDiff is an L2 result carrying both a policy rejection and a resource
// the preview could not evaluate.
func serverDiff() *diff.Diff {
	d := sampleDiff()
	d.Level = diff.LevelServer
	d.Violations = []diff.PolicyViolation{
		{
			Engine:      "kyverno",
			Policy:      "require-run-as-nonroot",
			Rule:        "check-securitycontext",
			Resource:    "Deployment/web",
			Path:        "spec.template.spec.securityContext.runAsNonRoot",
			Message:     "runAsNonRoot must be set to true",
			Enforcement: diff.EnforcementEnforce,
		},
		{
			Engine:      "kyverno",
			Policy:      "require-labels",
			Resource:    "Deployment/web",
			Message:     "missing recommended label app.kubernetes.io/name",
			Enforcement: diff.EnforcementAudit,
		},
	}
	d.Unvalidated = []diff.Unvalidated{
		{Resource: "Widget/thing", Requires: "CustomResourceDefinition for Widget.example.com", InBatch: true},
	}
	return d
}

// TestWriteRendersViolations: a policy rejection is the whole point of L2
// (#45) — the terminal renderer must not swallow it, and must label enforce
// and audit distinctly so a warning is never read as a blocker.
func TestWriteRendersViolations(t *testing.T) {
	var buf bytes.Buffer
	if err := diff.Write(&buf, serverDiff(), false); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"BLOCKED", "kyverno/require-run-as-nonroot", "check-securitycontext",
		"runAsNonRoot must be set to true",
		"warning", "kyverno/require-labels",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestWriteRendersUnvalidated: a preview that could not check a resource must
// say so, otherwise an incomplete preview reads as a clean one (#43).
func TestWriteRendersUnvalidated(t *testing.T) {
	var buf bytes.Buffer
	if err := diff.Write(&buf, serverDiff(), false); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "not validated: Widget/thing") {
		t.Errorf("unvalidated resource not reported\n%s", out)
	}
	if !strings.Contains(out, "CustomResourceDefinition for Widget.example.com") {
		t.Errorf("missing prerequisite not named\n%s", out)
	}
}

// TestUnvalidatedIsNotAViolation guards the contract distinction itself: the
// two channels stay separate through a JSON round trip, so an agent branching
// on Violations never sees an ordering artifact.
func TestUnvalidatedIsNotAViolation(t *testing.T) {
	b, err := diff.EncodeJSON(serverDiff())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got diff.Diff
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Unvalidated) != 1 || !got.Unvalidated[0].InBatch {
		t.Fatalf("unvalidated lost in round trip: %+v", got.Unvalidated)
	}
	for _, v := range got.Violations {
		if strings.Contains(v.Resource, "Widget") {
			t.Errorf("unvalidated resource leaked into Violations: %+v", v)
		}
	}
}
