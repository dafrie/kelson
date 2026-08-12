package dryrun

import (
	"errors"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dafrie/kelson/internal/diff"
)

// This file turns a dry-run rejection from the API server into structured
// diff.PolicyViolation entries (#45).
//
// The three policy engines kelson targets format their rejections differently,
// and none of them is a stable protocol — each is prose nested in a
// metav1.Status. The recognizers below match the shapes we have observed and,
// where a shape is fragile, the regex is best-effort with a fallback that still
// surfaces the engine's own full message. This is deliberately heuristic, and
// we never drop a rejection on the floor: if no recognizer matches, a
// synthesized violation carries the full Status message so the developer still
// sees the engine's wording.
//
// An important distinction (issue #43): an enforce-mode rejection is an
// authorization-style 403 from the admission webhook, which is
// indistinguishable from a plain RBAC 403 at the HTTP level. We classify by
// message shape — a recognized engine signature means "the policy rejected it",
// anything else Forbidden/Unauthorized means "kelson lacks permission" and
// degrades to L1 rather than being reported as a policy failure.

// classification is the outcome of inspecting one rejected dry-run apply.
type classification struct {
	// permission is true when the failure is an RBAC/authorization problem
	// rather than a policy or validation rejection — the L1-degradation trigger.
	permission bool
	// violations are the structured findings; enforce-mode rejections are
	// blockers, audit-mode findings (from policy reports) are warnings.
	violations []diff.PolicyViolation
	// policyRejected is true when a policy engine vetoed the request.
	policyRejected bool
}

// classification is built from a rejection error for a single resource.
func classify(ref ResourceRef, err error) classification {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		// No structured status (network error etc.) — not a policy signal.
		return classification{}
	}
	st := status.Status()

	if apierrors.IsNotFound(err) {
		// Handled by the caller for prerequisite ordering.
		return classification{}
	}

	// Admission webhook signatures. These must be checked before the
	// generic forbidden branch, because an enforce-mode webhook returns 403.
	if v, ok := kyvernoClassify(ref, st); ok {
		return v
	}
	if v, ok := gatekeeperClassify(ref, st); ok {
		return v
	}
	if v, ok := vapClassify(ref, st); ok {
		return v
	}

	// Built-in API server validation (immutable fields, schema violations).
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return apiserverClassify(ref, st)
	}

	// A Forbidden/Unauthorized that carried no engine signature is RBAC: kelson
	// (or its impersonated identity) lacks permission to dry-run this resource.
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return classification{permission: true}
	}

	return classification{}
}

// --- engine recognizers -----------------------------------------------------

// kyvernoClassify recognizes Kyverno rejecting a request in enforce mode.
//
// Kyverno blocks a request with a 403 (or 400) whose message is either the
// modern multi-policy summary —
//
//	resource [ns/name] was blocked due to the following policies
//	<policy>:
//	  <rule>: <message>
//
// — or an older single-line webhook form:
//
//	Admission webhook "validate.kyverno.svc-fail" denied the request: ...
func kyvernoClassify(ref ResourceRef, st metav1.Status) (classification, bool) {
	msg := st.Message
	if !strings.Contains(strings.ToLower(msg), "kyverno") &&
		!strings.Contains(msg, "blocked due to the following policies") {
		return classification{}, false
	}

	var out classification
	out.policyRejected = true
	out.violations = append(out.violations, kyvernoBlockedPolicies(msg)...)
	for i := range out.violations {
		// The multi-policy parser emits per-policy entries without the resource
		// identity; stamp it here so every violation points at what was blocked
		// (issue #45) rather than only the fallback doing so.
		out.violations[i].Resource = ref.String()
	}
	if len(out.violations) == 0 {
		out.violations = append(out.violations, diff.PolicyViolation{
			Engine:      "kyverno",
			Policy:      "unknown",
			Resource:    ref.String(),
			Message:     msg,
			Enforcement: diff.EnforcementEnforce,
		})
	}
	return out, true
}

var kyvernoBlockedLine = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_.-]+):\s*$`)
var kyvernoBlockedSub = regexp.MustCompile(`(?m)^\s{2,}([A-Za-z0-9_.-]+):\s+(.+)$`)

// kyvernoBlockedPolicies parses the modern "blocked due to the following
// policies" structure into one violation per policy/rule pair.
func kyvernoBlockedPolicies(msg string) []diff.PolicyViolation {
	var (
		currentPolicy string
		out           []diff.PolicyViolation
	)
	for _, line := range strings.Split(msg, "\n") {
		if m := kyvernoBlockedSub.FindStringSubmatch(line); m != nil && currentPolicy != "" {
			out = append(out, diff.PolicyViolation{
				Engine:      "kyverno",
				Policy:      currentPolicy,
				Rule:        m[1],
				Message:     m[2],
				Enforcement: diff.EnforcementEnforce,
			})
			continue
		}
		if m := kyvernoBlockedLine.FindStringSubmatch(line); m != nil {
			currentPolicy = m[1]
		}
	}
	return out
}

// gatekeeperClassify recognizes Gatekeeper / OPA ConstraintTemplate rejections:
//
//	admission webhook "validation.gatekeeper.sh" denied the request:
//	[denied by <constraint>] <message>
//	<constraint> <Kind.<name>.denied: <message>
func gatekeeperClassify(ref ResourceRef, st metav1.Status) (classification, bool) {
	msg := st.Message
	if !strings.Contains(msg, "validation.gatekeeper.sh") &&
		!strings.Contains(msg, "denied by") &&
		!strings.Contains(strings.ToLower(msg), "constraint") {
		return classification{}, false
	}

	policy := gatekeeperConstraint(msg)
	v := diff.PolicyViolation{
		Engine:      "gatekeeper",
		Policy:      policy,
		Resource:    ref.String(),
		Message:     msg,
		Enforcement: diff.EnforcementEnforce,
	}
	if m := gatekeeperDenied.FindStringSubmatch(msg); m != nil {
		v.Message = m[2]
		if i := strings.Index(m[2], "\n"); i >= 0 {
			v.Message = strings.TrimSpace(m[2][:i])
		}
	}
	return classification{
		policyRejected: true,
		violations:     []diff.PolicyViolation{v},
	}, true
}

var gatekeeperDenied = regexp.MustCompile(`denied the request:\s*\[denied by (\S+)\]\s*(.*)`)

// gatekeeperConstraint extracts the constraint name from either the bracketed
// "[denied by <constraint>]" or the older "<constraint> <kind>.denied:" form.
// The name character class stops at punctuation — `\S+` alone would swallow the
// closing bracket of "[denied by require-owner]" — and the kind.denied form is
// matched anywhere in the line, because the message carries the webhook head
// ("admission webhook ... denied the request:") before the constraint pair.
func gatekeeperConstraint(msg string) string {
	if m := regexp.MustCompile(`denied by ([A-Za-z0-9_.-]+)`).FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	if m := regexp.MustCompile(`(\S+)\s+\S+\.denied:`).FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return "unknown"
}

// vapClassify recognizes Kubernetes ValidatingAdmissionPolicy rejections:
//
//	ValidatingAdmissionPolicy '<policy>' with binding '<binding>' denied request: <message>
func vapClassify(ref ResourceRef, st metav1.Status) (classification, bool) {
	msg := st.Message
	if !strings.Contains(msg, "ValidatingAdmissionPolicy") &&
		!strings.Contains(strings.ToLower(msg), "validatingadmissionpolicy") {
		return classification{}, false
	}
	policy, binding, detail := vapParts(msg)
	return classification{
		policyRejected: true,
		violations: []diff.PolicyViolation{{
			Engine:      "validating-admission-policy",
			Policy:      policy,
			Rule:        binding,
			Resource:    ref.String(),
			Message:     detail,
			Enforcement: diff.EnforcementEnforce,
		}},
	}, true
}

var vapPattern = regexp.MustCompile(`ValidatingAdmissionPolicy '([^']+)' with binding '([^']+)' denied request:\s*(.+)`)

func vapParts(msg string) (policy, binding, detail string) {
	if m := vapPattern.FindStringSubmatch(msg); m != nil {
		return m[1], m[2], m[3]
	}
	return "unknown", "", msg
}

// apiserverClassify handles the API server's own validation: immutable fields
// (the classic "cannot change selector on a Deployment") and schema violations.
// These are not a policy engine but are still preview-time blockers — the write
// would be rejected before it persisted, which is exactly issue #43's second
// acceptance criterion.
func apiserverClassify(ref ResourceRef, st metav1.Status) classification {
	out := classification{policyRejected: true}
	if st.Details == nil || len(st.Details.Causes) == 0 {
		out.violations = append(out.violations, diff.PolicyViolation{
			Engine:      "kubernetes",
			Policy:      "validation-error",
			Resource:    ref.String(),
			Message:     st.Message,
			Enforcement: diff.EnforcementEnforce,
		})
		return out
	}
	seen := map[string]bool{}
	for _, cause := range st.Details.Causes {
		key := cause.Field + "|" + cause.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		pol := "invalid-field"
		if strings.Contains(strings.ToLower(cause.Message), "immutable") {
			pol = "field-is-immutable"
		}
		out.violations = append(out.violations, diff.PolicyViolation{
			Engine:   "kubernetes",
			Policy:   pol,
			Resource: ref.String(),
			Path:     cause.Field,
			// SpecPath is the best available proxy for the field the user wrote:
			// an immutable-field error quotes the rendered path, which maps 1:1
			// back to the kelson.yaml field the renderer produced it from.
			// Where the engine quotes a path we do not recognize we leave it
			// empty rather than invent a mapping (#45).
			Message:     cause.Message,
			Enforcement: diff.EnforcementEnforce,
		})
	}
	return out
}
