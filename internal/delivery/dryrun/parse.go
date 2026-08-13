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
// Four rejection shapes are recognized, in order of specificity: Kyverno,
// Gatekeeper/OPA, the built-in ValidatingAdmissionPolicy, and any other
// validating webhook — which is the generic form the first three also travel
// in, so it is matched last and only on the webhook-denial shape itself. A
// fifth shape is not a rejection at all: the API server refusing to run a
// webhook under dry-run, which is a hole in the preview's coverage rather than
// a verdict about the object.
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
	// dryRunUnsupported is true when the request failed not because anything
	// rejected the object but because an admission webhook cannot be run for a
	// dry-run request at all. Nothing was checked, so this is a coverage gap
	// (diff.Unvalidated), never a violation (#45).
	dryRunUnsupported bool
	// webhook is the webhook named by a dryRunUnsupported failure.
	webhook string
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

	// "This webhook cannot run under dry-run" comes back as a 400 and would
	// otherwise be read as an API-server validation failure — a rejection that
	// never happened, reported as a blocker. It is checked first because its
	// shape is the most specific one here.
	if c, ok := dryRunUnsupportedClassify(st); ok {
		return c
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
	// Any other validating webhook. Last of the admission recognizers, so a
	// Kyverno or Gatekeeper denial is attributed to its engine rather than to
	// the webhook endpoint that carried it.
	if v, ok := webhookClassify(ref, st); ok {
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

// dryRunUnsupportedClassify recognizes the API server refusing to run a
// validating webhook for a dry-run request:
//
//	admission webhook "x.y.z" does not support dry run
//
// The API server emits this for a webhook whose sideEffects is neither None nor
// NoneOnDryRun. It is not a rejection of the object — that webhook never saw
// it — so it must not become a violation. It is the preview saying out loud
// that its coverage has a hole (#45).
func dryRunUnsupportedClassify(st metav1.Status) (classification, bool) {
	m := dryRunUnsupportedPattern.FindStringSubmatch(st.Message)
	if m == nil {
		return classification{}, false
	}
	return classification{dryRunUnsupported: true, webhook: m[1]}, true
}

var dryRunUnsupportedPattern = regexp.MustCompile(`(?i)admission webhook "([^"]+)" does not support dry run`)

// webhookClassify recognizes any validating webhook denying the request:
//
//	admission webhook "x.y.z" denied the request: <message>
//
// This is the shape every ValidatingWebhookConfiguration produces, whatever
// runs behind it. Naming the webhook is not fabrication — it is the server's
// own word for who said no — so the developer gets an object to go and read
// even when kelson has never heard of the engine behind it (#45). The match is
// deliberately narrow: the quoted name and the "denied the request" phrase both
// have to be there, so prose that merely mentions a webhook stays unattributed.
func webhookClassify(ref ResourceRef, st metav1.Status) (classification, bool) {
	m := webhookDeniedPattern.FindStringSubmatch(st.Message)
	if m == nil {
		return classification{}, false
	}
	name := m[1]
	detail := strings.TrimSpace(m[2])
	if detail == "" {
		detail = st.Message
	}
	return classification{
		policyRejected: true,
		violations: []diff.PolicyViolation{{
			Code:        diff.CodeWebhookDenied,
			Engine:      "validating-webhook",
			Policy:      name,
			Resource:    ref.String(),
			Message:     detail,
			Enforcement: diff.EnforcementEnforce,
			Remediation: webhookRemediation(name),
		}},
	}, true
}

// The (?s) makes a multi-line denial body one detail rather than a first line.
var webhookDeniedPattern = regexp.MustCompile(`(?is)admission webhook "([^"]+)" denied the request:\s*(.*)`)

// webhookRemediation names the object that registers a webhook. The webhook
// name is a key inside a ValidatingWebhookConfiguration, not the name of one,
// so the remediation points at the list and the name to look for rather than
// pretending to know a resource name it never saw.
func webhookRemediation(name string) string {
	return "inspect the webhook: kubectl get validatingwebhookconfigurations -o yaml, and find the webhook named " + name
}

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
			Code:        diff.CodeWebhookDenied,
			Engine:      "kyverno",
			Policy:      "unknown",
			Resource:    ref.String(),
			Message:     msg,
			Enforcement: diff.EnforcementEnforce,
			Remediation: kyvernoRemediation("unknown"),
		})
	}
	return out, true
}

// kyvernoRemediation names the policy object to read. A Kyverno policy is
// either a cluster-scoped ClusterPolicy or a namespaced Policy and the
// rejection does not say which, so the remediation covers both rather than
// sending the reader to a resource that may not exist.
func kyvernoRemediation(policy string) string {
	if policy == "" || policy == "unknown" {
		return "inspect the cluster's policies: kubectl get clusterpolicy,policy -A"
	}
	return "inspect the policy: kubectl get clusterpolicy " + policy + " -o yaml (or kubectl get policy -A, if it is namespaced)"
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
				Code:        diff.CodeWebhookDenied,
				Engine:      "kyverno",
				Policy:      currentPolicy,
				Rule:        m[1],
				Message:     m[2],
				Enforcement: diff.EnforcementEnforce,
				Remediation: kyvernoRemediation(currentPolicy),
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
//
// The "denied by" trigger requires the bracket Gatekeeper actually writes:
// bare prose that happens to say a request was denied by something is not a
// constraint rejection, and matching it would put a stray word in the Policy
// field (#45's rule against fabricated policy names).
func gatekeeperClassify(ref ResourceRef, st metav1.Status) (classification, bool) {
	msg := st.Message
	if !strings.Contains(msg, "validation.gatekeeper.sh") &&
		!strings.Contains(msg, "[denied by ") &&
		!strings.Contains(strings.ToLower(msg), "constraint") {
		return classification{}, false
	}

	policy := gatekeeperConstraint(msg)
	v := diff.PolicyViolation{
		Code:        diff.CodeWebhookDenied,
		Engine:      "gatekeeper",
		Policy:      policy,
		Resource:    ref.String(),
		Message:     msg,
		Enforcement: diff.EnforcementEnforce,
		Remediation: gatekeeperRemediation(policy),
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

// gatekeeperRemediation points at the constraint. Its kind is a
// ConstraintTemplate-generated CRD the rejection does not always name, so the
// remediation uses the `constraints` category, which resolves whatever kind it
// turned out to be.
func gatekeeperRemediation(constraint string) string {
	if constraint == "" || constraint == "unknown" {
		return "inspect the cluster's constraints: kubectl get constraints -o yaml"
	}
	return "inspect the constraint " + constraint + ": kubectl get constraints -o yaml"
}

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
//
// The API server wraps that sentence in a Forbidden (or the policy's own
// failure reason, which may be Invalid) and repeats the policy's message in
// status.details.causes. The sentence is the only place the policy and binding
// names appear, so it is parsed first; the causes are read as a fallback for
// the detail, never for the names — inventing a policy name from a cause we did
// not recognize is exactly the fabrication #45 rejects.
func vapClassify(ref ResourceRef, st metav1.Status) (classification, bool) {
	msg := st.Message
	if !strings.Contains(strings.ToLower(msg), "validatingadmissionpolicy") {
		return classification{}, false
	}
	policy, binding, detail := vapParts(msg)
	if detail == "" {
		detail = firstCauseMessage(st)
	}
	if detail == "" {
		detail = msg
	}
	return classification{
		policyRejected: true,
		violations: []diff.PolicyViolation{{
			Code:        diff.CodeAdmissionPolicyDenied,
			Engine:      "validating-admission-policy",
			Policy:      policy,
			Rule:        binding,
			Resource:    ref.String(),
			Message:     detail,
			Enforcement: diff.EnforcementEnforce,
			Remediation: vapRemediation(policy, binding),
		}},
	}, true
}

// Both quoting styles are accepted because the sentence is prose, not a
// protocol, and it has been seen with single and double quotes. The trailing
// detail is optional: when the API server carries the policy's message in
// status.details.causes instead, the sentence stops after "denied request".
var vapPattern = regexp.MustCompile(`(?s)ValidatingAdmissionPolicy ['"]([^'"]+)['"] with binding ['"]([^'"]+)['"] denied request:?\s*(.*)`)

// vapParts returns the policy and binding names, and the detail where the
// sentence carried one. An empty detail is left empty for the caller to fill
// from the status causes rather than guessed at here.
func vapParts(msg string) (policy, binding, detail string) {
	if m := vapPattern.FindStringSubmatch(msg); m != nil {
		return m[1], m[2], strings.TrimSpace(m[3])
	}
	return "unknown", "", ""
}

// firstCauseMessage returns the first non-empty cause message a Status carries.
func firstCauseMessage(st metav1.Status) string {
	if st.Details == nil {
		return ""
	}
	for _, c := range st.Details.Causes {
		if c.Message != "" {
			return c.Message
		}
	}
	return ""
}

// vapRemediation names the policy object and, where the server gave it, the
// binding that put the policy in force for this resource — the two objects a
// reader needs to see why this policy applied here at all.
func vapRemediation(policy, binding string) string {
	if policy == "" || policy == "unknown" {
		return "inspect the cluster's policies: kubectl get validatingadmissionpolicy"
	}
	out := "inspect the policy: kubectl get validatingadmissionpolicy " + policy + " -o yaml"
	if binding != "" {
		out += "; it applies here through binding " + binding
	}
	return out
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
			Code:        diff.CodeValidationFailed,
			Engine:      "kubernetes",
			Policy:      "validation-error",
			Resource:    ref.String(),
			Message:     st.Message,
			Enforcement: diff.EnforcementEnforce,
			Remediation: apiserverRemediation("validation-error", ""),
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
			Code:     diff.CodeValidationFailed,
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
			Remediation: apiserverRemediation(pol, cause.Field),
		})
	}
	return out
}

// apiserverRemediation says what to do about the API server's own verdict.
// There is no policy object to read here — naming one would point the reader at
// an admission engine that had no part in this — so the remediation is about
// the field the server named.
func apiserverRemediation(policy, path string) string {
	at := ""
	if path != "" {
		at = " at " + path
	}
	if policy == "field-is-immutable" {
		return "this field" + at + " cannot be changed in place: revert it, or delete and recreate the resource"
	}
	return "the API server rejected this itself, with no admission policy involved: correct the value" + at + " in kelson.yaml"
}
