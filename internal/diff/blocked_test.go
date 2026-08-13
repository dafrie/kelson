package diff_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/diff"
)

// The blocked predicate is the CI contract (`kelson diff` exit 3, the Diff
// RPC's exit_semantics 3) in one place, so these tests are the contract: a
// verdict blocks, a gap in the preview's own coverage does not, and both are
// still reported (#45, #46).

func TestBlockedByEnforcedViolation(t *testing.T) {
	d := &diff.Diff{Violations: []diff.PolicyViolation{{
		Code:        diff.CodeWebhookDenied,
		Engine:      "validating-webhook",
		Policy:      "limits.example.com",
		Message:     "memory limit required",
		Enforcement: diff.EnforcementEnforce,
	}}}
	if !diff.Blocked(d) {
		t.Fatal("an enforce-mode denial must block")
	}
}

func TestNotBlockedByAuditFinding(t *testing.T) {
	d := &diff.Diff{Violations: []diff.PolicyViolation{{
		Code:        diff.CodeAuditFinding,
		Engine:      "kyverno",
		Policy:      "require-labels",
		Enforcement: diff.EnforcementAudit,
	}}}
	if diff.Blocked(d) {
		t.Fatal("an audit-mode warning must never block")
	}
}

// TestUnvalidatedBlockingByReason: "we asked and the answer was no" blocks;
// "we could not ask" does not. Failing a pipeline on a webhook the dry-run
// never reached would punish a coverage gap as if it were a verdict.
func TestUnvalidatedBlockingByReason(t *testing.T) {
	cases := []struct {
		name  string
		entry diff.Unvalidated
		block bool
	}{
		{"missing prerequisite, created in this batch", diff.Unvalidated{Reason: diff.ReasonMissingPrerequisite, InBatch: true}, false},
		{"missing prerequisite, genuinely absent", diff.Unvalidated{Reason: diff.ReasonMissingPrerequisite}, true},
		{"rejected in a shape we cannot attribute", diff.Unvalidated{Reason: diff.ReasonUnattributedRejection}, true},
		{"webhook refused the dry-run", diff.Unvalidated{Reason: diff.ReasonDryRunUnsupported}, false},
		{"webhook the dry-run does not reach", diff.Unvalidated{Reason: diff.ReasonWebhookExcludesDryRun}, false},
		{"no reason recorded, absent prerequisite", diff.Unvalidated{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.Blocking(); got != tc.block {
				t.Errorf("Blocking() = %v, want %v", got, tc.block)
			}
			if got := diff.Blocked(&diff.Diff{Unvalidated: []diff.Unvalidated{tc.entry}}); got != tc.block {
				t.Errorf("Blocked() = %v, want %v", got, tc.block)
			}
		})
	}
}

// TestCoverageGapIsReportedNotSwallowed: not blocking is not the same as not
// mentioning. A gap has to reach both readers — the terminal report and the
// JSON an agent parses — or an incomplete preview reads as a clean one.
func TestCoverageGapIsReportedNotSwallowed(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelServer,
		Unvalidated: []diff.Unvalidated{{
			Resource: "ValidatingWebhookConfiguration/platform-guards webhook quota.example.com",
			Reason:   diff.ReasonWebhookExcludesDryRun,
			Message:  "admission webhook quota.example.com declares sideEffects: Some",
		}},
	}

	var buf bytes.Buffer
	if err := diff.Write(&buf, d, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	text := buf.String()
	if !strings.Contains(text, "quota.example.com") {
		t.Errorf("the terminal report must name the webhook it could not reach:\n%s", text)
	}
	if strings.Contains(text, "apply would fail") {
		t.Errorf("a coverage gap must not claim the apply would fail:\n%s", text)
	}

	b, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got diff.Diff
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Unvalidated) != 1 || got.Unvalidated[0].Reason != diff.ReasonWebhookExcludesDryRun {
		t.Fatalf("the reason must survive the wire for an agent to branch on: %+v", got.Unvalidated)
	}
	if len(got.Violations) != 0 {
		t.Errorf("a gap must never arrive as a violation: %+v", got.Violations)
	}
}

// TestViolationCarriesCodeAndRemediation: the structured-error shape the rest
// of kelson promises (code, target, message, remediation) has to survive
// encoding, because it is what an agent acts on instead of parsing prose.
func TestViolationCarriesCodeAndRemediation(t *testing.T) {
	d := &diff.Diff{Violations: []diff.PolicyViolation{{
		Code:        diff.CodeAdmissionPolicyDenied,
		Engine:      "validating-admission-policy",
		Policy:      "require-limits",
		Rule:        "require-limits-all",
		Resource:    "Deployment/checkout",
		Message:     "containers must set memory limits",
		Enforcement: diff.EnforcementEnforce,
		Remediation: "inspect the policy: kubectl get validatingadmissionpolicy require-limits -o yaml",
	}}}
	b, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got diff.Diff
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v := got.Violations[0]
	if v.Code != diff.CodeAdmissionPolicyDenied {
		t.Errorf("code = %q, want %q", v.Code, diff.CodeAdmissionPolicyDenied)
	}
	if v.Remediation == "" {
		t.Error("remediation lost in the round trip")
	}

	var buf bytes.Buffer
	if err := diff.Write(&buf, d, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(buf.String(), "kubectl get validatingadmissionpolicy require-limits") {
		t.Errorf("the human report must say which object to go and read:\n%s", buf.String())
	}
}
