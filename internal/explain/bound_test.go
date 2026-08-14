package explain

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
)

// The bound is the acceptance criterion "output is bounded and usable directly
// in a context window" (issue #77). These tests are the enforcement: a
// pathological input must produce an answer that still fits, and must say that
// it was cut.

// pathological builds the worst input this package can be handed: forty failing
// workloads, each with a megabyte of container output, all of it one enormous
// line, plus a revision that changed a hundred variables and fifty policy
// violations on top.
func pathological() Input {
	noise := strings.Repeat("x", 4096)
	var logs strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&logs, "%d %s DATABASE_URL is not set\n", i, noise)
	}

	var verdicts []observation.Verdict
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("web-%02d", i)
		verdicts = append(verdicts, observation.Verdict{
			Healthy:     false,
			Code:        observation.CodeCrashLoopBackOff,
			Reason:      "CrashLoopBackOff: " + noise,
			Resource:    "Deployment/" + namespace + "/" + name,
			Remediation: noise,
			Containers: []observation.Container{{
				Name: name, Pod: name + "-abc", Code: observation.CodeCrashLoopBackOff,
				Reason: "CrashLoopBackOff " + noise, Logs: logs.String(),
			}},
		})
	}

	var violations []diff.PolicyViolation
	for i := 0; i < 50; i++ {
		violations = append(violations, diff.PolicyViolation{
			Code: diff.CodeWebhookDenied, Engine: "kyverno", Policy: fmt.Sprintf("policy-%02d", i),
			Resource: "Deployment/web", Message: noise, Enforcement: diff.EnforcementEnforce, Remediation: noise,
		})
	}

	before := make([]string, 0, 100)
	after := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		before = append(before, literalEnv(fmt.Sprintf("VAR_%03d", i), noise))
	}
	after = append(after, literalEnv("VAR_000", noise))

	return Input{
		Project: "hello", Environment: "production", Namespace: namespace,
		Status: delivery.Status{
			Phase: delivery.PhaseDegraded, Revision: liveRevision,
			Cause:  "flux: Kustomization apps/hello is not ready (BuildFailed): " + noise,
			Detail: map[string]string{"kustomization": "apps/hello"},
		},
		Verdicts:   verdicts,
		Violations: violations,
		History:    history(),
		Manifests: revisions(map[string][]delivery.Manifest{
			liveRevision: {manifest("web-00", deploymentYAML("web-00", "img:2", after, false))},
			pastRevision: {manifest("web-00", deploymentYAML("web-00", "img:1", before, false))},
		}),
	}
}

// TestPathologicalInputStaysInsideTheBudget is the boundedness acceptance test.
func TestPathologicalInputStaysInsideTheBudget(t *testing.T) {
	e := Explain(context.Background(), pathological())

	if got := e.size(); got > MaxBytes {
		t.Errorf("explanation is %d bytes, over the %d-byte budget", got, MaxBytes)
	}
	if got := len(e.Text()); got > MaxBytes*2 {
		// The rendered text carries the section headers and indentation the
		// payload does not, so it is allowed a factor of the budget — but not
		// an unbounded one.
		t.Errorf("rendered text is %d bytes, unreasonable for a %d-byte payload", got, MaxBytes)
	}
	if len(e.Causes) > MaxCauses {
		t.Errorf("%d causes, cap is %d", len(e.Causes), MaxCauses)
	}
	for _, c := range e.Causes {
		if len(c.Evidence) > MaxEvidence {
			t.Errorf("%s carries %d evidence items, cap is %d", c.Code, len(c.Evidence), MaxEvidence)
		}
		for _, ev := range c.Evidence {
			lines := strings.Split(ev.Detail, "\n")
			if len(lines) > MaxLogLines {
				t.Errorf("%s evidence has %d lines, cap is %d", c.Code, len(lines), MaxLogLines)
			}
			for _, l := range lines {
				if len(l) > MaxLineBytes+len(ellipsis) {
					t.Errorf("%s evidence line is %d bytes, cap is %d", c.Code, len(l), MaxLineBytes)
				}
			}
		}
	}
	if e.RecentChange != nil && len(e.RecentChange.Env) > MaxChanges {
		t.Errorf("%d env changes, cap is %d", len(e.RecentChange.Env), MaxChanges)
	}
	// Every cut is stated. An answer silently truncated is worse than a short
	// one, because a reader draws a wrong conclusion from a correct report.
	if len(e.Truncated) == 0 {
		t.Errorf("nothing was reported as truncated, though the input was pathological")
	}
	// And it is still an answer: the leading cause survived the trimming.
	if len(e.Causes) == 0 {
		t.Fatalf("everything was trimmed away:\n%s", e.Text())
	}
	if e.Summary == "" {
		t.Errorf("the summary must survive any trimming")
	}
}

// TestTruncationIsMarkedInline: a clamped string says so where it was clamped,
// so a reader can tell kelson's cut from the application's own output.
func TestTruncationIsMarkedInline(t *testing.T) {
	long := strings.Repeat("a", MaxLineBytes*3)
	got := clampLine(long, MaxLineBytes)
	if len(got) > MaxLineBytes+len(ellipsis) {
		t.Errorf("clamped to %d bytes, want at most %d", len(got), MaxLineBytes+len(ellipsis))
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("clamped value does not carry the marker: %q", got)
	}
	if short := clampLine("fine", MaxLineBytes); short != "fine" {
		t.Errorf("an in-budget value must be untouched, got %q", short)
	}
}

// TestClampNeverSplitsARune: truncation on a byte boundary inside a multi-byte
// character would produce output no terminal and no JSON encoder can render.
func TestClampNeverSplitsARune(t *testing.T) {
	s := strings.Repeat("é", MaxLineBytes)
	got := clampLine(s, MaxLineBytes)
	trimmed := strings.TrimSuffix(got, ellipsis)
	for _, r := range trimmed {
		if r == '�' {
			t.Fatalf("truncation split a rune: %q", got)
		}
	}
}

// TestExcerptKeepsTheTail: the last thing a container said before it died is
// the diagnosis, so an over-long excerpt loses its head.
func TestExcerptKeepsTheTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	got := clampExcerpt(b.String())
	lines := strings.Split(got, "\n")
	if len(lines) != MaxLogLines {
		t.Fatalf("excerpt has %d lines, want %d", len(lines), MaxLogLines)
	}
	if lines[len(lines)-1] != "line 99" {
		t.Errorf("last line = %q, want the newest line", lines[len(lines)-1])
	}
}
