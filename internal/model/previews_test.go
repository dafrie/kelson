package model

import (
	"strings"
	"testing"
)

// Validation and resolution of Environment.spec.previews (ADR-0017).
//
// The delivery-mode gate is deliberately not tested here: previews render in
// Flux mode only, and that refusal lives in the pure renderer for the same
// reason the Helm one does — the mode is spec data, so an Environment document
// stays valid on its own terms against every mode it will ever meet.

const previewsProject = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:1
  components:
    - {name: web, port: 8080}
`

// previewsEnv wraps a previews block in the smallest valid flux Environment.
func previewsEnv(block string) string {
	return `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: checkout
  delivery:
    mode: flux
    git: {repo: "git@github.com:acme/deploy.git"}
  previews:
` + block
}

const validPreviews = `    provider: github
    repo: https://github.com/acme/checkout
    secretRef: github-auth
    artifacts:
      repository: oci://ghcr.io/acme/checkout-previews
`

func decodePreviews(t *testing.T, block string) Errors {
	t.Helper()
	docs, errs := DecodeDocuments([]byte(previewsProject + "---\n" + previewsEnv(block)))
	var p *Project
	var e *Environment
	for _, d := range docs {
		switch v := d.(type) {
		case *Project:
			p = v
		case *Environment:
			e = v
		}
	}
	if p == nil || e == nil {
		return errs
	}
	return append(errs, ValidateSet(p, e)...)
}

// findErr returns the first error for a field path, or nil.
func findErr(errs Errors, field string) *Error {
	for i := range errs {
		if errs[i].Field == field {
			return &errs[i]
		}
	}
	return nil
}

func TestPreviewsValid(t *testing.T) {
	if errs := decodePreviews(t, validPreviews); len(errs) > 0 {
		t.Fatalf("the minimum viable previews block must validate:\n%v", errs)
	}
}

// TestPreviewsRequiredFields: every one of the four required fields is reported
// on its own path, in one pass — validation never fails fast (issue #28).
func TestPreviewsRequiredFields(t *testing.T) {
	errs := decodePreviews(t, "    interval: 10m\n")
	for _, field := range []string{
		"$.spec.previews.provider",
		"$.spec.previews.repo",
		"$.spec.previews.secretRef",
		"$.spec.previews.artifacts.repository",
	} {
		e := findErr(errs, field)
		if e == nil {
			t.Errorf("%s must be required, got:\n%v", field, errs)
			continue
		}
		if e.Code != ErrMissingRequired {
			t.Errorf("%s: code = %q, want %q", field, e.Code, ErrMissingRequired)
		}
		if e.Remediation == "" || e.Line == 0 {
			t.Errorf("%s must carry a remediation and a source line: %+v", field, e)
		}
	}
}

func TestPreviewsProviderEnum(t *testing.T) {
	errs := decodePreviews(t, `    provider: bitbucket
    repo: https://bitbucket.org/acme/checkout
    secretRef: forge-auth
    artifacts: {repository: "oci://ghcr.io/acme/p"}
`)
	e := findErr(errs, "$.spec.previews.provider")
	if e == nil || e.Code != ErrInvalidEnum {
		t.Fatalf("an unknown provider must be %s, got:\n%v", ErrInvalidEnum, errs)
	}
	// The enum is two wide on purpose (ADR-0017), so the remediation lists both
	// rather than pointing at a doc page.
	for _, want := range []string{"github", "gitlab"} {
		if !strings.Contains(e.Remediation, want) {
			t.Errorf("remediation must list %q: %s", want, e.Remediation)
		}
	}
}

// TestPreviewsRepoIsAnHTTPURL. The source repository is reached over the
// forge's HTTP API, so an SSH remote — the shape delivery.git.repo takes — is
// the wrong string in the right-looking field.
func TestPreviewsRepoIsAnHTTPURL(t *testing.T) {
	for _, repo := range []string{"git@github.com:acme/checkout.git", "acme/checkout", "oci://ghcr.io/acme/checkout"} {
		errs := decodePreviews(t, `    provider: github
    repo: "`+repo+`"
    secretRef: github-auth
    artifacts: {repository: "oci://ghcr.io/acme/p"}
`)
		e := findErr(errs, "$.spec.previews.repo")
		if e == nil || e.Code != ErrInvalidFormat {
			t.Errorf("repo %q must be %s, got:\n%v", repo, ErrInvalidFormat, errs)
			continue
		}
		if !strings.Contains(e.Remediation, "delivery.git.repo") {
			t.Errorf("the remediation must say which repository this is not: %s", e.Remediation)
		}
	}
}

// TestPreviewsArtifactsRepositoryIsUntagged: the tag is chosen per pull
// request, so one written here is either ignored or pins every preview to the
// same manifests (ADR-0017 decision 2).
func TestPreviewsArtifactsRepositoryIsUntagged(t *testing.T) {
	for _, repo := range []string{
		"oci://ghcr.io/acme/checkout-previews:latest",
		"oci://ghcr.io/acme/checkout-previews@sha256:abc",
	} {
		errs := decodePreviews(t, `    provider: github
    repo: https://github.com/acme/checkout
    secretRef: github-auth
    artifacts: {repository: "`+repo+`"}
`)
		e := findErr(errs, "$.spec.previews.artifacts.repository")
		if e == nil || e.Code != ErrInvalidFormat {
			t.Errorf("repository %q must be %s, got:\n%v", repo, ErrInvalidFormat, errs)
		}
	}

	errs := decodePreviews(t, `    provider: github
    repo: https://github.com/acme/checkout
    secretRef: github-auth
    artifacts: {repository: "https://ghcr.io/acme/checkout-previews"}
`)
	if e := findErr(errs, "$.spec.previews.artifacts.repository"); e == nil || e.Code != ErrInvalidFormat {
		t.Errorf("the artifact repository must be an oci:// URL, got:\n%v", errs)
	}

	// A colon before the first path segment is a registry port, not a tag —
	// an in-cluster registry on :5000 is a legitimate previews target.
	errs = decodePreviews(t, `    provider: github
    repo: https://github.com/acme/checkout
    secretRef: github-auth
    artifacts: {repository: "oci://registry.internal:5000/acme/checkout-previews"}
`)
	if e := findErr(errs, "$.spec.previews.artifacts.repository"); e != nil {
		t.Errorf("a registry port must not be read as a tag, got:\n%v", errs)
	}
}

func TestPreviewsBranchFiltersMustCompile(t *testing.T) {
	errs := decodePreviews(t, validPreviews+`    filter:
      includeBranch: "^feat/(.*"
      excludeBranch: "*bad"
`)
	for _, field := range []string{
		"$.spec.previews.filter.includeBranch",
		"$.spec.previews.filter.excludeBranch",
	} {
		e := findErr(errs, field)
		if e == nil || e.Code != ErrInvalidFormat {
			t.Errorf("%s must be %s, got:\n%v", field, ErrInvalidFormat, errs)
		}
	}

	if errs := decodePreviews(t, validPreviews+`    filter:
      includeBranch: "^feat/.*"
      excludeBranch: "^wip/"
`); len(errs) > 0 {
		t.Errorf("valid expressions must pass:\n%v", errs)
	}
}

func TestPreviewsLimitRange(t *testing.T) {
	for _, limit := range []string{"0", "-1", "10001"} {
		errs := decodePreviews(t, validPreviews+"    filter: {limit: "+limit+"}\n")
		e := findErr(errs, "$.spec.previews.filter.limit")
		if e == nil || e.Code != ErrOutOfRange {
			t.Errorf("limit %s must be %s, got:\n%v", limit, ErrOutOfRange, errs)
			continue
		}
		// The range is stated, and so is the default the author gets for free.
		if !strings.Contains(e.Remediation, "10000") || !strings.Contains(e.Remediation, "10") {
			t.Errorf("remediation must state the range and the default: %s", e.Remediation)
		}
	}
	if errs := decodePreviews(t, validPreviews+"    filter: {limit: 10000}\n"); len(errs) > 0 {
		t.Errorf("the maximum must be accepted:\n%v", errs)
	}
}

// TestPreviewsNegatedLabelsBelongToSkip. A filter selects change requests; a
// skip pauses updates to the ones already selected. flux-operator reads `!` in
// the second and not in the first, so a negated filter label is a silent no-op
// and kelson refuses it.
func TestPreviewsNegatedLabelsBelongToSkip(t *testing.T) {
	errs := decodePreviews(t, validPreviews+`    filter:
      labels: ["!ci/passed"]
`)
	e := findErr(errs, "$.spec.previews.filter.labels[0]")
	if e == nil || e.Code != ErrInvalidFormat {
		t.Fatalf("a negated filter label must be %s, got:\n%v", ErrInvalidFormat, errs)
	}
	if !strings.Contains(e.Remediation, "previews.skip.labels") {
		t.Errorf("the remediation must name where a negated label belongs: %s", e.Remediation)
	}

	if errs := decodePreviews(t, validPreviews+`    skip:
      labels: ["!ci/passed", "deploy/pause"]
`); len(errs) > 0 {
		t.Errorf("a negated skip label is the documented way to gate on CI:\n%v", errs)
	}
}

func TestPreviewsEmptyLabelRejected(t *testing.T) {
	errs := decodePreviews(t, validPreviews+`    filter:
      labels: ["deploy/preview", ""]
`)
	if e := findErr(errs, "$.spec.previews.filter.labels[1]"); e == nil || e.Code != ErrInvalidFormat {
		t.Fatalf("an empty label must be %s, got:\n%v", ErrInvalidFormat, errs)
	}
}

func TestPreviewsIntervalIsADuration(t *testing.T) {
	for _, interval := range []string{"10", "1d", "10 m", "0.5h"} {
		errs := decodePreviews(t, validPreviews+"    interval: \""+interval+"\"\n")
		if e := findErr(errs, "$.spec.previews.interval"); e == nil || e.Code != ErrInvalidFormat {
			t.Errorf("interval %q must be %s, got:\n%v", interval, ErrInvalidFormat, errs)
		}
	}
	for _, interval := range []string{"30s", "5m", "1h", "1h30m"} {
		if errs := decodePreviews(t, validPreviews+"    interval: "+interval+"\n"); len(errs) > 0 {
			t.Errorf("interval %q must validate:\n%v", interval, errs)
		}
	}
}

func TestPreviewsSecretRefIsANameNotAToken(t *testing.T) {
	errs := decodePreviews(t, `    provider: github
    repo: https://github.com/acme/checkout
    secretRef: "ghp_AAAAAAAAAAAAAAAAAAAA"
    artifacts: {repository: "oci://ghcr.io/acme/p"}
`)
	// A pasted token is not a DNS-1123 label, which is the check that catches
	// it. The spec carries a Secret name and never a value (ADR-0009, #79).
	if e := findErr(errs, "$.spec.previews.secretRef"); e == nil || e.Code != ErrInvalidFormat {
		t.Fatalf("a pasted token must not pass as a Secret name, got:\n%v", errs)
	}
}

// TestPreviewsUnknownFieldRejected: the previews block is held to the same
// closed-schema rule as everything else, so a misspelt field is an error rather
// than a setting that does nothing.
func TestPreviewsUnknownFieldRejected(t *testing.T) {
	errs := decodePreviews(t, validPreviews+"    ttl: 72h\n")
	found := false
	for _, e := range errs {
		if e.Code == ErrUnknownField && strings.Contains(e.Field, "ttl") {
			found = true
		}
	}
	if !found {
		// TTL is real future work and deliberately absent (ADR-0017): accepting
		// the field would promise an expiry that never happens.
		t.Fatalf("an unknown previews field must be %s, got:\n%v", ErrUnknownField, errs)
	}
}

// TestResolvePreviewsDefaults: the renderer never has to know what an unset
// interval or an unset limit means, because resolution has already decided.
func TestResolvePreviewsDefaults(t *testing.T) {
	p := &Previews{
		Provider:  PreviewGitHub,
		Repo:      "https://github.com/acme/checkout",
		SecretRef: "github-auth",
		Artifacts: PreviewArtifacts{Repository: "oci://ghcr.io/acme/p"},
	}
	got := resolvePreviews(p)
	if got.Interval != PreviewDefaultInterval {
		t.Errorf("interval = %q, want %q", got.Interval, PreviewDefaultInterval)
	}
	if got.Filter.Limit != PreviewDefaultLimit {
		t.Errorf("limit = %d, want %d — the ceiling is a cost control and is never inherited from flux-operator",
			got.Filter.Limit, PreviewDefaultLimit)
	}
	if got.Skip != nil {
		t.Errorf("skip = %v, want nil", got.Skip)
	}

	p.Interval = "5m"
	three := 3
	p.Filter = &PreviewFilter{Limit: &three, Labels: []string{"deploy/preview"}}
	p.Skip = &PreviewSkip{Labels: []string{"!ci/passed"}}
	got = resolvePreviews(p)
	if got.Interval != "5m" || got.Filter.Limit != 3 {
		t.Errorf("an explicit interval and limit must win: %+v", got)
	}
	if len(got.Skip) != 1 || got.Skip[0] != "!ci/passed" {
		t.Errorf("skip labels must survive resolution: %v", got.Skip)
	}

	if resolvePreviews(nil) != nil {
		t.Errorf("no previews block resolves to nil, not to an empty one")
	}
}

// TestResolvePreviewsAbsent: an environment without previews resolves to nil,
// which is what the renderer branches on.
func TestResolvePreviewsAbsent(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(previewsProject + `---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: checkout
`))
	if len(errs) > 0 {
		t.Fatalf("fixture does not validate:\n%v", errs)
	}
	r, rerrs := Resolve(docs[0].(*Project), docs[1].(*Environment))
	if len(rerrs) > 0 {
		t.Fatalf("resolve:\n%v", rerrs)
	}
	if r.Environment.Previews != nil {
		t.Errorf("previews = %+v, want nil", r.Environment.Previews)
	}
}
