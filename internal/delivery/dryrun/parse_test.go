package dryrun

import (
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/dafrie/kelson/internal/diff"
)

// These tests pin the recognisers that turn a dry-run rejection into a
// diff.PolicyViolation (issue #45). Every policy engine nest its detail inside
// a metav1.Status message in its own prose shape, none of which is a stable
// protocol — so the assertions are over verbatim message strings as the engines
// actually write them, plus deliberately malformed shapes to prove we never
// fabricate a policy name we did not find.

const tTarget = "Deployment/shop-prod/checkout"

// rejected builds an error whose status Message is exactly msg, mirroring an
// enforce-mode admission webhook (403) without apierrors.NewForbidden's
// "forbidden: " prefix — these tests pin the parser on the engine's own
// wording, not on how the API server frames it.
func rejected(msg string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Reason:  metav1.StatusReasonForbidden,
		Message: msg,
	}}
}

func TestClassifyKyverno(t *testing.T) {
	cases := []struct {
		name   string
		msg    string
		policy string
		rule   string
		detail string
		found  bool
	}{
		{
			name: "modern multi-policy summary",
			msg: fmt.Sprintf(
				"resource [%s] was blocked due to the following policies\n\n"+
					"require-labels:\n"+
					"  require-owner: validation error: rule require-owner failed at path /metadata/labels/owner/",
				tTarget),
			policy: "require-labels",
			rule:   "require-owner",
			detail: "validation error: rule require-owner failed at path /metadata/labels/owner/",
			found:  true,
		},
		{
			name: "two policies blocked at once",
			msg: fmt.Sprintf(
				"resource [%s] was blocked due to the following policies\n\n"+
					"disallow-latest-tag:\n"+
					"  require-image-tag: validation error: rule require-image-tag failed at path /spec/containers/\n"+
					"require-readiness:\n"+
					"  require-probe: validation error: rule require-probe failed at path /spec/containers/",
				tTarget),
			found: true,
		},
		{
			name:   "legacy webhook deny line",
			msg:    "Admission webhook \"validate.kyverno.svc-fail\" denied the request: validation error: rule check-owner failed at path /metadata/labels/owner/",
			policy: "unknown",
			rule:   "",
			detail: "Admission webhook \"validate.kyverno.svc-fail\" denied the request: validation error: rule check-owner failed at path /metadata/labels/owner/",
			found:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classify(ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}, rejected(tc.msg))
			if !c.policyRejected {
				t.Fatalf("policyRejected = false, want true")
			}
			if c.permission {
				t.Fatalf("permission = true, a policy rejection must not be read as RBAC")
			}
			if len(c.violations) == 0 {
				t.Fatalf("no violations parsed from:\n%s", tc.msg)
			}
			for _, v := range c.violations {
				if v.Engine != "kyverno" {
					t.Errorf("engine = %q, want kyverno", v.Engine)
				}
				if v.Enforcement != diff.EnforcementEnforce {
					t.Errorf("enforcement = %q, want enforce (a veto is a blocker)", v.Enforcement)
				}
				if v.Policy == "" || v.Policy == "dryrun-rejected" {
					t.Errorf("policy %q is fabricated", v.Policy)
				}
			}
			if tc.found {
				first := c.violations[0]
				if tc.policy != "" && first.Policy != tc.policy {
					t.Errorf("policy = %q, want %q", first.Policy, tc.policy)
				}
				if tc.rule != "" && first.Rule != tc.rule {
					t.Errorf("rule = %q, want %q", first.Rule, tc.rule)
				}
				if tc.detail != "" && first.Message != tc.detail {
					t.Errorf("message = %q, want %q", first.Message, tc.detail)
				}
			}
		})
	}
}

func TestClassifyGatekeeper(t *testing.T) {
	cases := []struct {
		name   string
		msg    string
		policy string
		detail string
	}{
		{
			name:   "bracketed denied-by form",
			msg:    "admission webhook \"validation.gatekeeper.sh\" denied the request: [denied by require-owner] you must set an owner label",
			policy: "require-owner",
			detail: "you must set an owner label",
		},
		{
			// The kind.denied form carries no bracketed constraint, so the
			// extracted detail is the full message; the constraint name still
			// comes from the "<constraint> <kind>.denied:" head.
			name:   "kind.denied form",
			msg:    "admission webhook \"validation.gatekeeper.sh\" denied the request: require-owner K8sRequiredLabels.denied: you must set an owner label",
			policy: "require-owner",
			detail: "admission webhook \"validation.gatekeeper.sh\" denied the request: require-owner K8sRequiredLabels.denied: you must set an owner label",
		},
		{
			// A bare "denied by <constraint>" with no gatekeeper hostname is
			// still recognisable as a constraint rejection, because the "denied
			// by" phrase is specific enough to the protocol to match on its own.
			name:   "constraint without hostname",
			msg:    "admission webhook denied the request: [denied by require-owner] missing owner label",
			policy: "require-owner",
			detail: "missing owner label",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classify(ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}, rejected(tc.msg))
			if !c.policyRejected {
				t.Fatalf("policyRejected = false, want true")
			}
			if len(c.violations) != 1 {
				t.Fatalf("violations = %+v, want exactly one", c.violations)
			}
			v := c.violations[0]
			if v.Engine != "gatekeeper" {
				t.Errorf("engine = %q, want gatekeeper", v.Engine)
			}
			if v.Enforcement != diff.EnforcementEnforce {
				t.Errorf("enforcement = %q, want enforce", v.Enforcement)
			}
			if tc.policy != "" && v.Policy != tc.policy {
				t.Errorf("policy = %q, want %q", v.Policy, tc.policy)
			}
			if tc.detail != "" && v.Message != tc.detail {
				t.Errorf("message = %q, want %q", v.Message, tc.detail)
			}
		})
	}
}

func TestClassifyValidatingAdmissionPolicy(t *testing.T) {
	msg := "ValidatingAdmissionPolicy 'require-owner' with binding 'require-owner-binding' denied request: object must set spec.owner"
	c := classify(ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}, rejected(msg))
	if !c.policyRejected {
		t.Fatalf("policyRejected = false, want true")
	}
	if len(c.violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", c.violations)
	}
	v := c.violations[0]
	if v.Engine != "validating-admission-policy" {
		t.Errorf("engine = %q, want validating-admission-policy", v.Engine)
	}
	if v.Policy != "require-owner" {
		t.Errorf("policy = %q, want require-owner", v.Policy)
	}
	if v.Rule != "require-owner-binding" {
		t.Errorf("rule = %q, want require-owner-binding", v.Rule)
	}
	if v.Message != "object must set spec.owner" {
		t.Errorf("message = %q, want the detail after 'denied request'", v.Message)
	}
	if v.Enforcement != diff.EnforcementEnforce {
		t.Errorf("enforcement = %q, want enforce", v.Enforcement)
	}
	if v.Code != diff.CodeAdmissionPolicyDenied {
		t.Errorf("code = %q, want %q", v.Code, diff.CodeAdmissionPolicyDenied)
	}
	if !strings.Contains(v.Remediation, "validatingadmissionpolicy require-owner") ||
		!strings.Contains(v.Remediation, "require-owner-binding") {
		t.Errorf("remediation must name the policy and the binding: %q", v.Remediation)
	}
}

// TestClassifyValidatingAdmissionPolicyDetails: the API server wraps the
// denial in its own framing and repeats the policy's message in
// status.details.causes. The policy and binding names exist only in the
// sentence, so they are parsed from there; the causes are a fallback for the
// detail, never a source of names.
func TestClassifyValidatingAdmissionPolicyDetails(t *testing.T) {
	ref := ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}

	// Double-quoted variant, and a detail that lives only in the causes.
	err := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Reason:  metav1.StatusReasonInvalid,
		Message: `deployments.apps "checkout" is forbidden: ValidatingAdmissionPolicy "require-limits" with binding "require-limits-all" denied request`,
		Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{
			{Type: metav1.CauseType("FieldValueInvalid"), Message: "containers must set memory limits"},
		}},
	}}
	c := classify(ref, err)
	if len(c.violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", c.violations)
	}
	v := c.violations[0]
	if v.Policy != "require-limits" || v.Rule != "require-limits-all" {
		t.Errorf("policy/binding = %q/%q, want require-limits/require-limits-all", v.Policy, v.Rule)
	}
	if v.Message != "containers must set memory limits" {
		t.Errorf("message = %q, want the cause the API server carried", v.Message)
	}
}

func TestClassifyAPIServerValidation(t *testing.T) {
	err := apierrors.NewInvalid(
		schema.GroupKind{Group: "apps", Kind: "Deployment"}, "checkout",
		field.ErrorList{
			field.Invalid(field.NewPath("spec", "selector"), "map[app:other]", "field is immutable"),
			field.Invalid(field.NewPath("spec", "template", "spec", "containers", "index(0)", "image"), "x", "invalid image"),
		})
	c := classify(ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}, err)
	if !c.policyRejected {
		t.Fatalf("policyRejected = false, want true (a validation rejection is a blocker)")
	}
	if len(c.violations) != 2 {
		t.Fatalf("violations = %+v, want the two cause paths", c.violations)
	}
	if c.violations[0].Path != "spec.selector" || c.violations[0].Policy != "field-is-immutable" {
		t.Errorf("first violation = %+v, want spec.selector / field-is-immutable", c.violations[0])
	}
	if c.violations[0].Engine != "kubernetes" {
		t.Errorf("engine = %q, want kubernetes (the API server, not a policy engine)", c.violations[0].Engine)
	}
}

// TestClassifyValidatingWebhook covers the shape every
// ValidatingWebhookConfiguration produces, whatever engine sits behind it. The
// webhook's name is the API server's own word for who said no, so it is
// reported as the finding's target — an engine kelson has never heard of still
// yields a policy object the developer can go and read (#45).
func TestClassifyValidatingWebhook(t *testing.T) {
	ref := ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}
	cases := []struct {
		name    string
		msg     string
		webhook string
		detail  string
	}{
		{
			name:    "lowercase form",
			msg:     `admission webhook "policy.example.com" denied the request: every container must set resource limits`,
			webhook: "policy.example.com",
			detail:  "every container must set resource limits",
		},
		{
			name:    "capitalised form, as the API server frames a 403",
			msg:     `deployments.apps "checkout" is forbidden: Admission webhook "vpolicy.acme.io" denied the request: image registry acme.io/ is required`,
			webhook: "vpolicy.acme.io",
			detail:  "image registry acme.io/ is required",
		},
		{
			name:    "multi-line detail is kept whole",
			msg:     "admission webhook \"guard.example.com\" denied the request: two problems:\n  - no owner label\n  - no runAsNonRoot",
			webhook: "guard.example.com",
			detail:  "two problems:\n  - no owner label\n  - no runAsNonRoot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classify(ref, rejected(tc.msg))
			if !c.policyRejected {
				t.Fatalf("policyRejected = false; a webhook denial is a rejection")
			}
			if c.permission {
				t.Fatalf("permission = true; a webhook denial is not RBAC")
			}
			if len(c.violations) != 1 {
				t.Fatalf("violations = %+v, want exactly one", c.violations)
			}
			v := c.violations[0]
			if v.Engine != "validating-webhook" {
				t.Errorf("engine = %q, want validating-webhook", v.Engine)
			}
			if v.Policy != tc.webhook {
				t.Errorf("policy = %q, want the webhook name %q", v.Policy, tc.webhook)
			}
			if v.Message != tc.detail {
				t.Errorf("message = %q, want the server's own words %q", v.Message, tc.detail)
			}
			if v.Code != diff.CodeWebhookDenied {
				t.Errorf("code = %q, want %q", v.Code, diff.CodeWebhookDenied)
			}
			if v.Enforcement != diff.EnforcementEnforce {
				t.Errorf("enforcement = %q, want enforce", v.Enforcement)
			}
			if v.Resource != ref.String() {
				t.Errorf("resource = %q, want the rejected resource %q", v.Resource, ref.String())
			}
			if !strings.Contains(v.Remediation, "validatingwebhookconfigurations") ||
				!strings.Contains(v.Remediation, tc.webhook) {
				t.Errorf("remediation must name the object to inspect and the webhook: %q", v.Remediation)
			}
		})
	}
}

// TestClassifyDryRunUnsupportedIsNotAViolation: a webhook whose sideEffects
// forbid dry-run makes the API server fail the request with a 400 that looks
// like validation but is nothing of the sort — nobody rejected the object, it
// was never shown to that webhook. Reporting it as a violation would invent a
// verdict; it is a coverage gap (#45).
func TestClassifyDryRunUnsupportedIsNotAViolation(t *testing.T) {
	ref := ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}
	err := apierrors.NewBadRequest(`admission webhook "sidecar.example.com" does not support dry run`)
	c := classify(ref, err)
	if !c.dryRunUnsupported {
		t.Fatalf("dryRunUnsupported = false for %q", err)
	}
	if c.policyRejected {
		t.Error("policyRejected = true; nothing rejected this resource")
	}
	if len(c.violations) != 0 {
		t.Errorf("violations = %+v, want none — the webhook never saw the object", c.violations)
	}
	if c.webhook != "sidecar.example.com" {
		t.Errorf("webhook = %q, want sidecar.example.com", c.webhook)
	}
}

func TestClassifyMalformedOrUnrecognized(t *testing.T) {
	ref := ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", Namespace: "shop-prod"}

	// Prose that mentions a webhook, or a denial, without the shape either
	// recogniser needs — plus a generic conflict. The parsers must stay silent
	// rather than name a webhook or a policy they did not actually read.
	cases := []struct {
		name string
		err  error
	}{
		{"webhook call failed, nothing denied", rejected(`Internal error occurred: failed calling webhook "guard.example.com": context deadline exceeded`)},
		{"prose about a denial", rejected("the request was denied by the platform team's admission webhook; ask #platform")},
		{"conflict", apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "checkout", fmt.Errorf("object has been modified"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classify(ref, tc.err)
			if c.policyRejected {
				t.Errorf("policyRejected = true for an unattributable message")
			}
			for _, v := range c.violations {
				if v.Policy == "dryrun-rejected" || v.Policy == "unknown" {
					t.Errorf("fabricated a policy name %q for an unattributable rejection", v.Policy)
				}
			}
		})
	}
}
