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
    DATABASE_URL:
      from: {service: db, key: uri}
  services:
    - name: db
      type: postgres
      plan: ha-small
  applications:
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
	if got := p.Spec.Applications[0].Workload(); got != WorkloadService {
		t.Fatalf("web workload = %q, want %q", got, WorkloadService)
	}
	if got := p.Spec.Applications[1].Workload(); got != WorkloadWorker {
		t.Fatalf("worker workload = %q, want %q", got, WorkloadWorker)
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
  applications:
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
  applications:
    - name: web
      port: 70000
`))
	found := false
	for _, e := range errs {
		if e.Code == ErrOutOfRange {
			found = true
			if e.Field != "$.spec.applications[0].port" {
				t.Errorf("field = %q, want $.spec.applications[0].port", e.Field)
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
  applications:
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
			if !strings.Contains(secret.Remediation, "kelson secret set") {
				t.Errorf("remediation should name the fix, got %q", secret.Remediation)
			}
		})
	}
}

// TestSecretReferenceAccepted: the same variable through from: is valid.
func TestSecretReferenceAccepted(t *testing.T) {
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
  services:
    - {name: db, type: postgres}
  applications:
    - {name: web, port: 8080}
`))
	if len(errs) != 0 {
		t.Fatalf("references must be accepted, got:\n%v", errs)
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
  applications:
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
  applications:
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
	if !strings.Contains(uf.Remediation, "applications") {
		t.Errorf("remediation should list valid fields, got %q", uf.Remediation)
	}
}

func TestEnvironmentCrossReferences(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  services:
    - {name: db, type: postgres}
  applications:
    - {name: web, port: 8080}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  delivery:
    mode: github
  services:
    - {name: warehouse, plan: small}
  applications:
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
	shapeOnly := 0
	for _, e := range errs {
		if e.Code != ErrInvalidEnum {
			shapeOnly++
		}
	}
	if shapeOnly != 0 {
		t.Fatalf("decode must be otherwise clean, got:\n%v", errs)
	}
	p := docs[0].(*Project)
	e := docs[1].(*Environment)

	envErrs := ValidateEnvironment(e, p)
	want := []Code{
		ErrInvalidEnum,        // delivery mode github
		ErrUnknownService,     // warehouse not in the project
		ErrUnknownServiceKey,  // tls is not a postgres key
		ErrUnknownApplication, // api not in the project
	}
	codes := envErrs.Codes()
	for _, w := range want {
		if !slices.Contains(codes, w) {
			t.Errorf("missing code %s in:\n%v", w, envErrs)
		}
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
  applications:
    - name: nightly
      schedule: "0 3 * * *"
`))
	if len(errs) != 0 {
		t.Fatalf("cron application must be valid, got %v", errs)
	}
	if got := docs[0].(*Project).Spec.Applications[0].Workload(); got != WorkloadCron {
		t.Errorf("workload = %q, want cron", got)
	}

	_, errs = DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  applications:
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
  applications:
    - name: web
      port: 8080
`))
	if !slices.Contains(errs.Codes(), ErrNoImageSource) {
		t.Errorf("build.strategy none without image must fail, got %v", errs)
	}
}
