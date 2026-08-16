package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// precedenceProject exercises every precedence rule at once: P1 (env merge),
// P2 (replicas/resources), P3 (image), P4 (policy/secrets chain),
// P5 (service presets), P6 (overlays).
//
// It deliberately keeps fields issue #141 gated at one time or another — a
// policy default and a secret backend. Precedence over them was real behaviour
// the resolver implemented before anything consumed them, which is why landing
// a milestone has twice been a matter of deleting a gate row rather than
// rebuilding resolution (#89 for data services, ADR-0025 for `policy:`). These
// cases load through loadPairUnvalidated and call the unexported resolve; the
// gate itself is covered in coverage_test.go.
const precedenceProject = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  env:
    LOG_LEVEL: info
    REGION: eu-central
    DATABASE_URL:
      from: {service: db, key: uri}
  components:
    - {name: db, kind: postgres, preset: shared}
    - {name: cache, kind: valkey}
    - name: web
      port: 8080
      env:
        LOG_LEVEL: debug          # component beats project (P1)
      replicas: {min: 2, max: 4}
      resources:
        requests: {cpu: 100m}
    - name: worker
      image: ghcr.io/acme/shop-worker:2   # component beats project (P3)
  defaults:
    policy:
      agents: allow
      require: [dry-run]
  overlays:
    - manifest: ./k8s/base.yaml
`

func loadPair(t *testing.T, projSrc, envSrc string) (*Project, *Environment) {
	t.Helper()
	docs, errs := DecodeDocuments([]byte(projSrc + "\n---\n" + envSrc))
	if len(errs) != 0 {
		t.Fatalf("decode errors: %v", errs)
	}
	return docs[0].(*Project), docs[1].(*Environment)
}

// loadPairUnvalidated decodes a pair without validating it, so a spec carrying
// fields gated by issue #141 can still reach the resolver. Only precedence
// tests use it; anything asserting on validation must go through
// DecodeDocuments.
func loadPairUnvalidated(t *testing.T, projSrc, envSrc string) (*Project, *Environment) {
	t.Helper()
	p := new(Project)
	if err := yaml.Unmarshal([]byte(projSrc), p); err != nil {
		t.Fatalf("decoding project: %v", err)
	}
	e := new(Environment)
	if err := yaml.Unmarshal([]byte(envSrc), e); err != nil {
		t.Fatalf("decoding environment: %v", err)
	}
	return p, e
}

// resolved runs the resolver with an empty global source tier, which is what a
// precedence test means: the bindings under test are the Project's own, and the
// GitSources an instance offers (ADR-0035 decision 2) are somebody else's
// input. A binding refusal fails the test rather than being asserted away —
// these documents are supposed to resolve.
func resolved(t *testing.T, p *Project, e *Environment) *Resolved {
	t.Helper()
	r, errs := resolve(p, e, nil)
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}
	return r
}

func TestResolveMergesEnvironmentVariablesP1(t *testing.T) {
	p, e := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  routing: {domainSuffix: staging.example.com}
  delivery:
    mode: flux
    git: {repo: r}
  components:
    - name: web
      env:
        LOG_LEVEL: trace          # environment override beats component (P1)
`)
	r := resolved(t, p, e)
	web := r.Components[0]
	if web.Env["LOG_LEVEL"].Literal != "trace" {
		t.Errorf("LOG_LEVEL = %q, want trace (environment override wins)", web.Env["LOG_LEVEL"].Literal)
	}
	if web.Env["REGION"].Literal != "eu-central" {
		t.Errorf("REGION = %q, want eu-central (project survives)", web.Env["REGION"].Literal)
	}
	if web.Env["DATABASE_URL"].From == nil || web.Env["DATABASE_URL"].From.Service != "db" {
		t.Errorf("DATABASE_URL binding lost: %+v", web.Env["DATABASE_URL"])
	}
}

func TestResolveReplicasAndDomainP2(t *testing.T) {
	p, e := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  routing: {domainSuffix: staging.example.com}
  delivery:
    mode: flux
    git: {repo: r}
  components:
    - name: web
      replicas: {min: 5}          # replaces the component's {2,4} whole (P2)
`)
	r := resolved(t, p, e)
	web := r.Components[0]
	if web.Replicas != (Replicas{Min: 5}) {
		t.Errorf("replicas = %+v, want {Min:5} — environment replaces whole, no deep merge", web.Replicas)
	}
	if web.Resources == nil || web.Resources.Requests.CPU != "100m" {
		t.Errorf("resources must survive from the component when the environment does not override them: %+v", web.Resources)
	}
	if !slices.Equal(web.Domains, []string{"web.staging.example.com"}) {
		t.Errorf("default host derived from domainSuffix, got %v", web.Domains)
	}
	worker := r.Components[1]
	if worker.Image != "ghcr.io/acme/shop-worker:2" {
		t.Errorf("worker image = %q, want component override (P3)", worker.Image)
	}
	if worker.Replicas != (Replicas{Min: 1}) {
		t.Errorf("default replicas = %+v, want {Min:1}", worker.Replicas)
	}
}

func TestResolveExplicitDomainsWin(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - {name: web, port: 8080, domains: [shop.example.com]}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  routing: {domainSuffix: staging.example.com}
`)
	r, _ := Resolve(p, e)
	if !slices.Equal(r.Components[0].Domains, []string{"shop.example.com"}) {
		t.Errorf("explicit domains must win over the suffix, got %v", r.Components[0].Domains)
	}
}

func TestResolvePolicySecretsP4(t *testing.T) {
	// Staging says nothing of its own: policy comes from the Project default,
	// secrets from the built-in. (The chain used to start with a delivery mode;
	// there is one delivery path now — ADR-0028.)
	p, staging := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
`)
	r := resolved(t, p, staging)
	if r.Environment.Policy.Agents != AgentsAllow || len(r.Environment.Policy.Require) != 1 {
		t.Errorf("policy = %+v, want allow + dry-run from project default", r.Environment.Policy)
	}
	if r.Environment.Secrets.Backend != SecretsCluster {
		t.Errorf("secrets.backend = %q, want cluster built-in", r.Environment.Secrets.Backend)
	}
}

// TestRetiredDeliveryBlockIsRefusedWithItsStory: an Environment written against
// the old vocabulary decodes to a strict schema/unknown-field, and the
// remediation says the block is gone rather than offering a spelling list
// (ADR-0028 decision 9, internal/model/decode.go).
func TestRetiredDeliveryBlockIsRefusedWithItsStory(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  delivery:
    mode: flux
    git: {repo: git@github.com:acme/deploy.git}
`))
	var got *Error
	for i := range errs {
		if errs[i].Code == ErrUnknownField && errs[i].Field == "$.spec.delivery" {
			got = &errs[i]
		}
	}
	if got == nil {
		t.Fatalf("a delivery block must be refused as %s, got:\n%v", ErrUnknownField, errs)
	}
	// The remediation is the whole migration and it names no design record: an
	// author who wrote a correct thing that has since been removed needs the
	// instruction, not the citation (#260).
	if !strings.Contains(got.Remediation, "delete") ||
		!strings.Contains(got.Remediation, "nothing about the deployment changes") ||
		strings.Contains(got.Remediation, "ADR-") {
		t.Errorf("the remediation must say the block is gone and safe to delete, got %q", got.Remediation)
	}
	if got.Line != 6 {
		t.Errorf("line = %d, want 6 (the delivery: key)", got.Line)
	}
}

func TestResolveEnvironmentOverridesDefaultsP4(t *testing.T) {
	p, prod := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  policy:
    agents: propose-only        # taken whole; the project default does not merge in (P4)
  secrets:
    backend: sops
`)
	r := resolved(t, p, prod)
	if r.Environment.Policy.Agents != AgentsProposeOnly {
		t.Errorf("agents = %q, want propose-only", r.Environment.Policy.Agents)
	}
	if len(r.Environment.Policy.Require) != 0 {
		t.Errorf("environment policy replaces the project default whole: require must be empty, got %v", r.Environment.Policy.Require)
	}
	if r.Environment.Secrets.Backend != SecretsSOPS {
		t.Errorf("secrets.backend = %q, want sops", r.Environment.Secrets.Backend)
	}
}

func TestResolveServicePresetOverrideP5(t *testing.T) {
	p, e := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  delivery:
    git: {repo: git@github.com:acme/deploy.git}
  components:
    - {name: db, preset: ha-small}
`)
	r := resolved(t, p, e)
	if r.DataServices[0].Preset != PresetHASmall {
		t.Errorf("db preset = %q, want ha-small (P5)", r.DataServices[0].Preset)
	}
	if r.DataServices[1].Preset != PresetShared {
		t.Errorf("cache preset = %q, want shared (project value survives; preset default is shared)", r.DataServices[1].Preset)
	}
}

func TestResolveOverlaysOrderP6(t *testing.T) {
	p, e := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  delivery:
    git: {repo: git@github.com:acme/deploy.git}
  overlays:
    - patch: ./k8s/staging.yaml
`)
	r := resolved(t, p, e)
	if len(r.Overlays) != 2 || r.Overlays[0].Manifest != "./k8s/base.yaml" || r.Overlays[1].Patch != "./k8s/staging.yaml" {
		t.Errorf("overlays must concatenate project-first: %+v", r.Overlays)
	}
}

func TestResolveBuiltInDefaults(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: hello}
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
    - {name: nightly, schedule: "0 3 * * *"}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: dev}
spec:
  project: hello
`)
	r, errs := Resolve(p, e)
	if len(errs) != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	// An environment that says nothing about agents narrows nothing: the
	// credential an operator issued is the grant, and every guardrail in
	// `policy:` is opt-in (ADR-0025 §3).
	if r.Environment.Policy.Agents != AgentsAllow {
		t.Errorf("built-in agents default = %q, want allow", r.Environment.Policy.Agents)
	}
	if r.Environment.Secrets.Backend != SecretsCluster {
		t.Errorf("built-in secrets default = %q, want cluster", r.Environment.Secrets.Backend)
	}
	if r.Environment.Namespace != "hello-dev" {
		t.Errorf("default namespace = %q, want hello-dev", r.Environment.Namespace)
	}
	if !r.Environment.Routing.TLS {
		t.Errorf("TLS must default to true")
	}
	if r.Components[1].Kind != ComponentCron {
		t.Errorf("kind = %q, want cron", r.Components[1].Kind)
	}
}

// TestResolveSourceOnlyProjectYieldsImageUnresolved covers issue #170: a
// Project with spec.source and no spec.build is built from source under
// ADR-0010's auto default, the same as one with an explicit `build: {strategy:
// auto}`. It must validate and resolve with ImageUnresolved, not fail.
func TestResolveSourceOnlyProjectYieldsImageUnresolved(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: hello}
spec:
  source: {git: https://github.com/acme/hello}
  components:
    - {name: web, port: 8080}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: dev}
spec:
  project: hello
`)
	r, errs := Resolve(p, e)
	if len(errs) != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if got := r.Components[0].Image; got != ImageUnresolved {
		t.Errorf("image = %q, want %q (built from source, awaiting build)", got, ImageUnresolved)
	}
}

// TestResolveCarriesTheProjectSource: the git URL and the named connection
// reach the resolved spec, so a plane holding only a Resolved can resolve the
// connection the way the server does (ADR-0033 decision 4). Before this, the
// preview materializer could match by host and nothing else — the override
// existed in the spec and stopped at the server.
func TestResolveCarriesTheProjectSource(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: hello}
spec:
  source:
    git: https://github.com/acme/hello
    ref: main
    connection: acme-github
  components:
    - {name: web, port: 8080}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: dev}
spec:
  project: hello
`)
	r, errs := Resolve(p, e)
	if len(errs) != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if r.Source == nil {
		t.Fatal("a project with a source resolved to none")
	}
	if r.Source.Git != "https://github.com/acme/hello" || r.Source.Connection != "acme-github" {
		t.Errorf("source = %+v, want the project's git URL and connection", *r.Source)
	}
}

// A project deploying a pre-built image has no source, and the field must stay
// nil so it marshals to nothing: a struct here rather than a pointer would have
// moved the spec hash of every image-only environment in existence.
func TestResolveWithoutASourceCarriesNone(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: hello}
spec:
  image: ghcr.io/acme/hello:1
  components:
    - {name: web, port: 8080}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: dev}
spec:
  project: hello
`)
	r, errs := Resolve(p, e)
	if len(errs) != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if r.Source != nil {
		t.Errorf("source = %+v, want nil", *r.Source)
	}
	// The hash is taken over this encoding, so "marshals to nothing" is the
	// property that kept the artifact tag of every image-only environment where
	// it was. A struct here rather than a pointer would have moved all of them.
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(encoded), `"source"`) {
		t.Errorf("a project with no source encoded one: %s", encoded)
	}
}

// TestResolveEnvironmentImagePinWinsP3 is the promotion primitive of ADR-0016:
// the Environment's pin is the innermost scope of rule P3, so it beats a
// component image and the Project's alike. Promoting is writing this field.
func TestResolveEnvironmentImagePinWinsP3(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - {name: web, port: 8080}
    - {name: worker, image: ghcr.io/acme/shop-worker:2}
    - {name: cron, schedule: "0 3 * * *"}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  components:
    - name: web
      image: ghcr.io/acme/shop@sha256:1111111111111111111111111111111111111111111111111111111111111111
    - name: worker
      image: ghcr.io/acme/shop-worker@sha256:2222222222222222222222222222222222222222222222222222222222222222
`)
	r, errs := Resolve(p, e)
	if len(errs) != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	want := map[string]string{
		"web":    "ghcr.io/acme/shop@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"worker": "ghcr.io/acme/shop-worker@sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"cron":   "ghcr.io/acme/shop:2",
	}
	for _, c := range r.Components {
		if c.Image != want[c.Name] {
			t.Errorf("%s image = %q, want %q", c.Name, c.Image, want[c.Name])
		}
	}
}

// TestResolveImagePinSatisfiesBuiltFromSource is the promotion shape: one
// environment pins the digest a build produced and resolves to it, its
// unpinned sibling still resolves to the sentinel and fails to render (#136).
func TestResolveImagePinSatisfiesBuiltFromSource(t *testing.T) {
	const project = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  source: {git: https://github.com/acme/checkout}
  build: {strategy: dockerfile}
  components:
    - {name: web, port: 8080}
`
	const pinned = "ghcr.io/acme/checkout@sha256:3333333333333333333333333333333333333333333333333333333333333333"

	p, prod := loadPair(t, project, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  components:
    - name: web
      image: `+pinned+`
`)
	r, errs := Resolve(p, prod)
	if len(errs) != 0 {
		t.Fatalf("resolve pinned: %v", errs)
	}
	if got := r.Components[0].Image; got != pinned {
		t.Errorf("pinned image = %q, want %q — a pin resolves a built-from-source component the way --image does", got, pinned)
	}

	_, staging := loadPair(t, project, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: checkout
`)
	r, errs = Resolve(p, staging)
	if len(errs) != 0 {
		t.Fatalf("resolve unpinned: %v", errs)
	}
	if got := r.Components[0].Image; got != ImageUnresolved {
		t.Errorf("unpinned image = %q, want %q — only the environment that was promoted to is pinned", got, ImageUnresolved)
	}
}

func TestResolveRejectsInvalid(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: hello}
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: dev}
spec:
  project: hello
  components:
    - {name: ghost, replicas: {min: 2}}
`)
	if _, errs := Resolve(p, e); !slices.Contains(errs.Codes(), ErrUnknownComponent) {
		t.Errorf("resolving an invalid pair must fail with ref/unknown-component, got %v", errs)
	}
}
