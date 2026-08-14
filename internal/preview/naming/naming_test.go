package naming_test

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/preview/naming"
)

// The naming scheme is the contract ADR-0017 stage 2 turned from a convention
// into shared code, so these tests are about the numbers in the ADR: what the
// cap is, what it reserves it for, and that the templated form and the concrete
// form are the same function.

func TestPreviewNameIsBaseAndChangeRequest(t *testing.T) {
	got := naming.Preview("checkout", "staging", "412")
	if want := "checkout-staging-pr412"; got != want {
		t.Fatalf("Preview() = %q, want %q", got, want)
	}
	if got := naming.Lifecycle("checkout", "staging"); got != "checkout-staging-previews" {
		t.Errorf("Lifecycle() = %q, want checkout-staging-previews", got)
	}
}

// TestPreviewTemplateSubstitutesToPreview is the property the renderer and the
// publisher both depend on: what flux-operator templates and what kelson
// publishes into are the same string once the input is filled in.
func TestPreviewTemplateSubstitutesToPreview(t *testing.T) {
	tmpl := naming.PreviewTemplate("checkout", "staging")
	if !strings.Contains(tmpl, naming.InputID) {
		t.Fatalf("PreviewTemplate() = %q, which carries no %s hole", tmpl, naming.InputID)
	}
	substituted := strings.ReplaceAll(tmpl, naming.InputID, "412")
	if want := naming.Preview("checkout", "staging", "412"); substituted != want {
		t.Errorf("substituting the template gave %q, want %q", substituted, want)
	}
}

// TestMaxBaseReservesSixDigits pins the arithmetic ADR-0017 decision 3 states:
// 63 is the DNS-1123 label limit, "-pr" plus six digits is what a preview adds,
// and a preview of the longest permitted base must still fit.
func TestMaxBaseReservesSixDigits(t *testing.T) {
	if naming.MaxBase != 54 {
		t.Fatalf("MaxBase = %d, want 54 (63 - len(\"-pr\") - 6)", naming.MaxBase)
	}
	base := strings.Repeat("a", naming.MaxBase)
	longest := base + "-" + naming.Infix + strings.Repeat("9", naming.IDDigits)
	if len(longest) != 63 {
		t.Errorf("the longest preview name is %d characters, want 63: %s", len(longest), longest)
	}
	if naming.BaseTooLong(base) {
		t.Errorf("a base of exactly MaxBase must be accepted")
	}
	if !naming.BaseTooLong(base + "a") {
		t.Errorf("a base one over MaxBase must be refused")
	}
}

func TestValidateID(t *testing.T) {
	for _, ok := range []string{"1", "412", "999999"} {
		if err := naming.ValidateID(ok); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", ok, err)
		}
	}
	// Seven digits is the ADR's named assumption failing: it would render a
	// namespace one character too long, so it is refused at the flag.
	for _, bad := range []string{"", "0", "0412", "1000000", "12a", "-1", " 412"} {
		if err := naming.ValidateID(bad); err == nil {
			t.Errorf("ValidateID(%q) = nil, want a refusal", bad)
		}
	}
}

// TestValidateSHARefusesAbbreviations is the quiet failure this validation
// exists to make loud: an abbreviated commit tags the artifact somewhere the
// rendered OCIRepository — which pins the full sha flux-operator read from the
// forge — will never look.
func TestValidateSHARefusesAbbreviations(t *testing.T) {
	full := strings.Repeat("ab", 20)
	if err := naming.ValidateSHA(full); err != nil {
		t.Errorf("ValidateSHA(sha1) = %v, want nil", err)
	}
	if err := naming.ValidateSHA(strings.Repeat("ab", 32)); err != nil {
		t.Errorf("ValidateSHA(sha256) = %v, want nil", err)
	}
	for _, bad := range []string{"", "abc1234", full[:39], full + "a", strings.Repeat("z", 40)} {
		if err := naming.ValidateSHA(bad); err == nil {
			t.Errorf("ValidateSHA(%q) = nil, want a refusal", bad)
		}
	}
}

func TestTagNormalisesCase(t *testing.T) {
	sha := strings.Repeat("AB", 20)
	if got := naming.Tag(sha); got != strings.ToLower(sha) {
		t.Errorf("Tag(%q) = %q, want it lowercased: a tag is compared byte for byte", sha, got)
	}
}

// TestHostSuffixesTheFirstLabel pins the wildcard property: only the first
// label changes, so `*.staging.acme.run` still covers every preview.
func TestHostSuffixesTheFirstLabel(t *testing.T) {
	cases := map[string]string{
		"web.staging.acme.run": "web-pr412.staging.acme.run",
		"api.acme.com":         "api-pr412.acme.com",
		"web":                  "web-pr412",
	}
	for in, want := range cases {
		if got := naming.Host(in, "412"); got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
	// Two change requests must never collide, which is the whole reason the
	// rewrite happens at all.
	if naming.Host("web.staging.acme.run", "412") == naming.Host("web.staging.acme.run", "413") {
		t.Error("two change requests derived the same hostname")
	}
}

func TestRevisionNamesBothTheChangeRequestAndTheCommit(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	got := naming.Revision("412", sha)
	if !strings.Contains(got, "412") || !strings.Contains(got, sha) {
		t.Errorf("Revision() = %q, want it to name change request 412 and commit %s", got, sha)
	}
}
