package model

import (
	"slices"
	"strings"
	"testing"
)

func TestValidProject(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  source:
    git: https://github.com/acme/checkout
  build:
    strategy: auto
  env:
    LOG_LEVEL: info
  components:
    - name: web
      port: 8080
      health: /healthz
      domains: [checkout.acme.com]
      replicas: {min: 2, max: 10}
    - name: worker
      command: ["bundle", "exec", "sidekiq"]
`))
	if len(errs) != 0 {
		t.Fatalf("expected valid project, got:\n%v", errs)
	}
	p, ok := docs[0].(*Project)
	if !ok {
		t.Fatalf("expected *Project, got %T", docs[0])
	}
	if got := p.Spec.Components[0].EffectiveKind(); got != ComponentService {
		t.Fatalf("web workload = %q, want %q", got, ComponentService)
	}
	if got := p.Spec.Components[1].EffectiveKind(); got != ComponentWorker {
		t.Fatalf("worker workload = %q, want %q", got, ComponentWorker)
	}
}

// TestMultipleErrorsCollected is the core of #28: five distinct problems in
// one document, all five reported, none hiding behind another.
func TestMultipleErrorsCollected(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: bad
spec:
  image: ghcr.io/acme/x:1
  typo_field: true
  env:
    DATABASE_URL: postgres://app:s3cret@db.internal:5432/shop
    CACHE_URL:
      from: {service: cache, key: uri}
  components:
    - name: web
      port: 70000
    - name: web
      schedule: "0 * * * *"
      port: 8080
`))
	want := []Code{
		ErrUnknownField,      // typo_field
		ErrSecretLiteral,     // DATABASE_URL embeds a password
		ErrUnknownService,    // cache is never declared
		ErrOutOfRange,        // port 70000
		ErrDuplicateName,     // two applications named web
		ErrMutuallyExclusive, // schedule + port on the same application
	}
	codes := errs.Codes()
	for _, w := range want {
		if !slices.Contains(codes, w) {
			t.Errorf("missing code %s in:\n%v", w, errs)
		}
	}
	if len(errs) < len(want) {
		t.Errorf("want at least %d errors, got %d:\n%v", len(want), len(errs), errs)
	}
	for _, e := range errs {
		if e.Field == "" || e.Field == "$" || e.Remediation == "" || e.DocsURL == "" || e.Resource == "" {
			t.Errorf("error missing structure: %+v", e)
		}
	}
}

func TestPortRange(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - name: web
      port: 70000
`))
	found := false
	for _, e := range errs {
		if e.Code == ErrOutOfRange {
			found = true
			if e.Field != "$.spec.components[0].port" {
				t.Errorf("field = %q, want $.spec.components[0].port", e.Field)
			}
			if !strings.Contains(e.Message, "1-65535") {
				t.Errorf("message should state the range, got %q", e.Message)
			}
		}
	}
	if !found {
		t.Fatalf("port 70000 not rejected:\n%v", errs)
	}
}

// TestSecretLiteralRejected covers ADR-0009: a plaintext value for a
// secret-shaped variable is an error that names the field and the fix.
func TestSecretLiteralRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"URL with password", `    DATABASE_URL: postgres://u:pw@host/db`},
		{"secret-shaped name", `    API_KEY: "0123456789abcdef0123456789abcdef"`},
		{"token", `    STRIPE_SECRET_TOKEN: sk_live_abcdef`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  env:
` + tc.env + `
  components:
    - name: web
      port: 8080
`))
			var secret *Error
			for i := range errs {
				if errs[i].Code == ErrSecretLiteral {
					secret = &errs[i]
				}
			}
			if secret == nil {
				t.Fatalf("secret literal not rejected:\n%v", errs)
			}
			// The remediation may only name things that work today. It used to
			// send authors to a `kelson secret set` that does not exist (#142),
			// and while #141 gated bindings it could only offer the overlay
			// escape hatch. A binding renders end to end since #89 and a secret
			// reference since ADR-0018, so all three are legitimate answers and
			// all three must be named — the reference first, because it is the
			// one that covers every credential rather than a managed service's.
			if strings.Contains(secret.Remediation, "kelson secret set") {
				t.Errorf("remediation references the nonexistent `kelson secret set` command, got %q", secret.Remediation)
			}
			if !strings.Contains(secret.Remediation, "overlay") {
				t.Errorf("remediation should name a fix that works today, got %q", secret.Remediation)
			}
			if !strings.Contains(secret.Remediation, "{from:") {
				t.Errorf("remediation should name the service binding now that it renders, got %q", secret.Remediation)
			}
			if !strings.Contains(secret.Remediation, "{secret:") {
				t.Errorf("remediation should name the secret reference now that it renders, got %q", secret.Remediation)
			}
		})
	}
}

// TestSecretReferenceIsNotASecretViolation: the same variable through from: is
// well-formed under ADR-0009 — it carries a reference, not a value — and since
// #89 it also renders, so it is simply valid. While #141 gated bindings this
// test asserted the opposite (a not-implemented error, deliberately *not* a
// secret/literal one); what survives is the part that always mattered: a
// reference is never mistaken for a credential written into the spec.
func TestSecretReferenceIsNotASecretViolation(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  env:
    DATABASE_URL:
      from: {service: db, key: uri}
    LOG_LEVEL: info
  components:
    - {name: db, kind: postgres}
    - {name: web, port: 8080}
`))
	if slices.Contains(errs.Codes(), ErrSecretLiteral) {
		t.Errorf("a binding carries a reference, not a value: it must never be a secret/literal, got:\n%v", errs)
	}
	if len(errs) > 0 {
		t.Fatalf("a declared service and a binding to it are valid since #89, got:\n%v", errs)
	}
}

func TestLineAndColumn(t *testing.T) {
	var line int
	src := `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: pos
spec:
  image: i:1
  components:
    - name: web
      port: 70000
`
	line = 9
	_, errs := DecodeDocuments([]byte(src))
	var portErr *Error
	for i := range errs {
		if errs[i].Code == ErrOutOfRange {
			portErr = &errs[i]
		}
	}
	if portErr == nil {
		t.Fatalf("expected port error, got:\n%v", errs)
	}
	if portErr.Line != line {
		t.Errorf("line = %d, want %d (the port: key)", portErr.Line, line)
	}
	if portErr.Column == 0 {
		t.Errorf("column not set: %+v", portErr)
	}
}

func TestUnknownFieldReportedWithPosition(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  replicaz: 3
  components:
    - {name: web, port: 8080}
`))
	var uf *Error
	for i := range errs {
		if errs[i].Code == ErrUnknownField {
			uf = &errs[i]
		}
	}
	if uf == nil {
		t.Fatalf("unknown field not reported:\n%v", errs)
	}
	if uf.Field != "$.spec.replicaz" {
		t.Errorf("field = %q, want $.spec.replicaz", uf.Field)
	}
	if uf.Line != 6 {
		t.Errorf("line = %d, want 6", uf.Line)
	}
	if !strings.Contains(uf.Remediation, "components") {
		t.Errorf("remediation should list valid fields, got %q", uf.Remediation)
	}
}

// TestServicePlanFieldRejected covers #146: the pre-rename field name `plan`
// is not a silent alias for `preset` — it is an unknown field like any typo.
func TestServicePlanFieldRejected(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - {name: db, kind: postgres, plan: shared}
    - {name: web, port: 8080}
`))
	var uf *Error
	for i := range errs {
		if errs[i].Code == ErrUnknownField {
			uf = &errs[i]
		}
	}
	if uf == nil {
		t.Fatalf("old field name %q must be rejected as unknown, got:\n%v", "plan", errs)
	}
	if uf.Field != "$.spec.components[0].plan" {
		t.Errorf("field = %q, want $.spec.components[0].plan", uf.Field)
	}
	if !strings.Contains(uf.Remediation, "preset") {
		t.Errorf("remediation should point at the current field name, got %q", uf.Remediation)
	}
}

// TestApplicationsFieldRejected is the same rule for ADR-0014's rename: the
// two lists the leaf used to be written as are unknown fields now, not silent
// aliases, and the remediation names `components` because the decoder lists the
// document's real field set. A stored spec that still says `applications:`
// therefore fails at the first validation with a fix in the message, which is
// the only migration path a pre-alpha rename gets.
func TestApplicationsFieldRejected(t *testing.T) {
	for _, old := range []string{"applications", "services"} {
		t.Run(old, func(t *testing.T) {
			_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  ` + old + `:
    - {name: web, port: 8080}
  components:
    - {name: api, port: 9090}
`))
			var uf *Error
			for i := range errs {
				if errs[i].Code == ErrUnknownField && errs[i].Field == "$.spec."+old {
					uf = &errs[i]
				}
			}
			if uf == nil {
				t.Fatalf("%q must be rejected as unknown after ADR-0014, got:\n%v", old, errs)
			}
			if !strings.Contains(uf.Remediation, "components") {
				t.Errorf("remediation must name the field that replaced it, got %q", uf.Remediation)
			}
			if uf.Line == 0 {
				t.Errorf("unknown-field error must carry a source line: %+v", uf)
			}
		})
	}
}

// TestEnvironmentOverrideListsRejected is the Environment half: an environment
// that still overrides `applications:` or `services:` fails the same way.
func TestEnvironmentOverrideListsRejected(t *testing.T) {
	for _, old := range []string{"applications", "services"} {
		t.Run(old, func(t *testing.T) {
			_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: p
  ` + old + `:
    - {name: web, preset: small}
`))
			found := false
			for _, e := range errs {
				if e.Code == ErrUnknownField && e.Field == "$.spec."+old && strings.Contains(e.Remediation, "components") {
					found = true
				}
			}
			if !found {
				t.Errorf("Environment spec.%s must be an unknown field naming components, got:\n%v", old, errs)
			}
		})
	}
}

// TestIngressClassFieldRejected is the same rule for #140: kelson renders
// Gateway API only, so `ingressClass` is gone from the model and a spec that
// still carries it fails as an unknown field. Silently ignoring it would route
// nothing while looking configured.
func TestIngressClassFieldRejected(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  routing:
    domainSuffix: acme.com
    ingressClass: nginx
`))
	var uf *Error
	for i := range errs {
		if errs[i].Code == ErrUnknownField && strings.Contains(errs[i].Field, "ingressClass") {
			uf = &errs[i]
		}
	}
	if uf == nil {
		t.Fatalf("ingressClass must be rejected as an unknown field, got:\n%v", errs)
	}
	if uf.Field != "$.spec.routing.ingressClass" {
		t.Errorf("field = %q, want $.spec.routing.ingressClass", uf.Field)
	}
	if uf.Line != 8 {
		t.Errorf("line = %d, want 8 (the ingressClass: key)", uf.Line)
	}
	if !strings.Contains(uf.Remediation, "gatewayClass") {
		t.Errorf("remediation should point at gatewayClass, got %q", uf.Remediation)
	}
}

func TestEnvironmentCrossReferences(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - {name: db, kind: postgres}
    - {name: web, port: 8080}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  delivery:
    mode: github
  components:
    - {name: warehouse, preset: small}
    - name: web
      env:
        PG_URL:
          from: {service: db, key: tls}
    - name: api
      replicas: {min: 2}
`))
	if !slices.Contains(errs.Codes(), ErrInvalidEnum) {
		t.Fatalf("delivery mode github must be caught at decode, got:\n%v", errs)
	}
	// Decode is otherwise clean: any remaining gate (issue #141) is expected,
	// everything else would be a shape complaint this document should not
	// produce.
	for _, e := range errs {
		if e.Code != ErrInvalidEnum && e.Code != ErrNotImplemented {
			t.Fatalf("decode must be otherwise clean, got:\n%v", errs)
		}
	}
	p := docs[0].(*Project)
	e := docs[1].(*Environment)

	envErrs := ValidateEnvironment(e, p)
	want := []Code{
		ErrInvalidEnum,       // delivery mode github
		ErrUnknownServiceKey, // tls is not a postgres key
		ErrUnknownComponent,  // warehouse and api are not in the project
	}
	codes := envErrs.Codes()
	for _, w := range want {
		if !slices.Contains(codes, w) {
			t.Errorf("missing code %s in:\n%v", w, envErrs)
		}
	}
}

// TestEnvironmentImagePinValidation covers the three ways the promotion pin of
// ADR-0016 can be wrong: it names a component the Project does not declare, it
// pins a data component (whose image belongs to its operator), or it is not an
// image reference at all — held to exactly what spec.image is held to.
func TestEnvironmentImagePinValidation(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - {name: db, kind: postgres}
    - {name: web, port: 8080}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  components:
    - {name: db, image: ghcr.io/acme/postgres:16}
    - {name: ghost, image: ghcr.io/acme/ghost:1}
    - {name: web, image: "ghcr.io/acme/shop:2 "}
`))
	if !slices.Contains(errs.Codes(), ErrInvalidFormat) {
		t.Fatalf("a reference with whitespace must fail like any other image field, got:\n%v", errs)
	}
	envErrs := ValidateEnvironment(docs[1].(*Environment), docs[0].(*Project))
	for _, want := range []Code{ErrMutuallyExclusive, ErrUnknownComponent, ErrInvalidFormat} {
		if !slices.Contains(envErrs.Codes(), want) {
			t.Errorf("missing code %s in:\n%v", want, envErrs)
		}
	}
	var pinnedData bool
	for _, e := range envErrs {
		if e.Code == ErrMutuallyExclusive && strings.HasSuffix(e.Field, "].image") {
			pinnedData = true
			if !strings.Contains(e.Remediation, "preset") {
				t.Errorf("pinning a data component must point at preset: %s", e.Remediation)
			}
		}
	}
	if !pinnedData {
		t.Errorf("pinning a data component's image must be rejected, got:\n%v", envErrs)
	}
}

// TestEnvironmentImagePinAccepted: the pin is ordinary on a workload, and
// nothing else in the pair has to change to carry it.
func TestEnvironmentImagePinAccepted(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  source: {git: https://github.com/acme/shop}
  components:
    - {name: web, port: 8080}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  components:
    - name: web
      image: ghcr.io/acme/shop@sha256:4444444444444444444444444444444444444444444444444444444444444444
`))
	if len(errs) != 0 {
		t.Fatalf("a pinned environment must validate, got:\n%v", errs)
	}
	if envErrs := ValidateEnvironment(docs[1].(*Environment), docs[0].(*Project)); len(envErrs) != 0 {
		t.Fatalf("a pinned environment must validate against its project, got:\n%v", envErrs)
	}
}

func TestDeliveryModes(t *testing.T) {
	mk := func(delivery string) *Environment {
		docs, _ := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: e}
spec:
  project: p
` + delivery))
		return docs[0].(*Environment)
	}
	// flux without git is an error
	errs := ValidateEnvironment(mk(`
  delivery: {mode: flux}`), nil)
	if !slices.Contains(errs.Codes(), ErrGitTargetMissing) {
		t.Errorf("flux without git must fail, got %v", errs)
	}
	// direct with git is an error
	errs = ValidateEnvironment(mk(`
  delivery: {mode: direct, git: {repo: r}}`), nil)
	if !slices.Contains(errs.Codes(), ErrMutuallyExclusive) {
		t.Errorf("direct with git must fail, got %v", errs)
	}
	// flux with git is fine
	errs = ValidateEnvironment(mk(`
  delivery: {mode: flux, git: {repo: git@github.com:a/b.git}}`), nil)
	if len(errs) != 0 {
		t.Errorf("flux with git must be valid, got %v", errs)
	}
}

func TestWorkloadDerivationRules(t *testing.T) {
	// schedule + domains is mutually exclusive (cron jobs are not routed).
	docs, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - name: nightly
      schedule: "0 3 * * *"
`))
	if len(errs) != 0 {
		t.Fatalf("cron application must be valid, got %v", errs)
	}
	if got := docs[0].(*Project).Spec.Components[0].EffectiveKind(); got != ComponentCron {
		t.Errorf("workload = %q, want cron", got)
	}

	_, errs = DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - name: bad
      schedule: "0 3 * * *"
      health: /healthz
`))
	if !slices.Contains(errs.Codes(), ErrMutuallyExclusive) {
		t.Errorf("schedule + health must be rejected, got %v", errs)
	}
}

func TestBuildStrategyNone(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  source: {git: https://github.com/a/b}
  build: {strategy: none}
  components:
    - name: web
      port: 8080
`))
	if !slices.Contains(errs.Codes(), ErrNoImageSource) {
		t.Errorf("build.strategy none without image must fail, got %v", errs)
	}
}

// TestSourceOnlyProjectIsBuiltFromSource covers issue #170: validation treated
// a Project as built-from-source only when both spec.source and spec.build
// were present, while model.Resolve already treated a nil spec.build as the
// `auto` strategy (ADR-0010's default). A source-only Project must validate
// without ErrNoImageSource; TestResolveSourceOnlyProjectYieldsImageUnresolved
// in resolve_test.go covers the matching resolve-time behaviour.
func TestSourceOnlyProjectIsBuiltFromSource(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  source: {git: https://github.com/a/b}
  components:
    - name: web
      port: 8080
`))
	if len(errs) != 0 {
		t.Fatalf("source-only project without build must be valid, got %v", errs)
	}
}

// TestNoSourceNoImageStillFails guards the other half of #170's fix: relaxing
// the built-from-source check must not relax semantic/no-image-source when a
// component genuinely has neither an image nor a source to build from.
func TestNoSourceNoImageStillFails(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  components:
    - name: web
      port: 8080
`))
	if !slices.Contains(errs.Codes(), ErrNoImageSource) {
		t.Errorf("no source and no image must fail, got %v", errs)
	}
}

// TestCronFieldRanges covers issue #143: cronFieldRE only checked shape, so a
// schedule like "99 * * * *" passed validation and failed only once applied
// to the cluster as a CronJob. Bounds mirror what Kubernetes' CronJob accepts
// (robfig/cron's standard 5-field parser), field by field, with no
// cross-field check (schedule "0 0 31 2 *" is accepted here even though no
// February has a 31st — that is deliberately out of scope).
func TestCronFieldRanges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule string
		wantErr  bool
	}{
		{"plain schedule", "0 3 * * *", false},
		{"ranges, steps and lists", "*/15 2-4 * * 1-5", false},
		{"no cross-field check", "0 0 31 2 *", false},
		{"month name", "0 0 1 JAN *", false},
		{"day-of-week name", "0 0 * * MON", false},
		{"day-of-week 0 is Sunday", "0 0 * * 0", false},
		{"day-of-week 7 is also Sunday", "0 0 * * 7", false},
		{"minute out of range", "99 * * * *", true},
		{"hour out of range", "* 24 * * *", true},
		{"step of zero", "*/0 * * * *", true},
		{"range end out of range", "5-99 * * * *", true},
		{"day-of-month zero", "0 0 0 * *", true},
		{"day-of-week out of range", "0 0 * * 8", true},
		{"month out of range", "0 0 1 13 *", true},
		{"list member out of range", "0 0 * * 1,2,8", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - name: nightly
      schedule: "` + tc.schedule + `"
`))
			hasRangeErr := slices.Contains(errs.Codes(), ErrOutOfRange)
			if tc.wantErr && !hasRangeErr {
				t.Errorf("schedule %q: want an out-of-range error, got %v", tc.schedule, errs)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("schedule %q: want no errors, got %v", tc.schedule, errs)
			}
		})
	}
}
