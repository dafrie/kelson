package model

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// precedenceProject exercises every precedence rule at once: P1 (env merge),
// P2 (replicas/resources), P3 (image), P4 (delivery/policy/secrets chain),
// P5 (service presets), P6 (overlays).
//
// It deliberately keeps the fields issue #141 gates — services, bindings and a
// policy default. Precedence over them is still real behaviour the resolver
// implements, and M7/M8/M9 land by deleting a gate row, not by rebuilding
// resolution. So these cases load through loadPairUnvalidated and call the
// unexported resolve; the gate itself is covered in coverage_test.go.
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
  services:
    - {name: db, type: postgres, preset: shared}
    - {name: cache, type: valkey}
  applications:
    - name: web
      port: 8080
      env:
        LOG_LEVEL: debug          # application beats project (P1)
      replicas: {min: 2, max: 4}
      resources:
        requests: {cpu: 100m}
    - name: worker
      image: ghcr.io/acme/shop-worker:2   # application beats project (P3)
  defaults:
    deliveryMode: flux
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
  applications:
    - name: web
      env:
        LOG_LEVEL: trace          # environment override beats application (P1)
`)
	r := resolve(p, e)
	web := r.Applications[0]
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
  applications:
    - name: web
      replicas: {min: 5}          # replaces the application's {2,4} whole (P2)
`)
	r := resolve(p, e)
	web := r.Applications[0]
	if web.Replicas != (Replicas{Min: 5}) {
		t.Errorf("replicas = %+v, want {Min:5} — environment replaces whole, no deep merge", web.Replicas)
	}
	if web.Resources == nil || web.Resources.Requests.CPU != "100m" {
		t.Errorf("resources must survive from the application when the environment does not override them: %+v", web.Resources)
	}
	if !slices.Equal(web.Domains, []string{"web.staging.example.com"}) {
		t.Errorf("default host derived from domainSuffix, got %v", web.Domains)
	}
	worker := r.Applications[1]
	if worker.Image != "ghcr.io/acme/shop-worker:2" {
		t.Errorf("worker image = %q, want application override (P3)", worker.Image)
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
  applications:
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
	if !slices.Equal(r.Applications[0].Domains, []string{"shop.example.com"}) {
		t.Errorf("explicit domains must win over the suffix, got %v", r.Applications[0].Domains)
	}
}

func TestResolveDeliveryPolicySecretsP4(t *testing.T) {
	// Staging carries only its git target: the mode comes from the Project
	// default, policy from the same default, secrets from the built-in.
	p, staging := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  delivery:
    git: {repo: git@github.com:acme/deploy.git, path: shop/staging}
`)
	r := resolve(p, staging)
	if r.Environment.Mode != DeliveryFlux {
		t.Errorf("mode = %q, want flux from project default", r.Environment.Mode)
	}
	if r.Environment.Delivery.Git == nil || r.Environment.Delivery.Git.Path != "shop/staging" {
		t.Errorf("git target must survive: %+v", r.Environment.Delivery.Git)
	}
	if r.Environment.Policy.Agents != AgentsAllow || len(r.Environment.Policy.Require) != 1 {
		t.Errorf("policy = %+v, want allow + dry-run from project default", r.Environment.Policy)
	}
	if r.Environment.Secrets.Backend != SecretsCluster {
		t.Errorf("secrets.backend = %q, want cluster built-in", r.Environment.Secrets.Backend)
	}
}

func TestResolvedFluxWithoutGitFails(t *testing.T) {
	// The effective mode (here, the Project default) requires a git target
	// even though the Environment itself never set delivery.mode.
	// A gate-free project, so the only error this can produce is the one under
	// test (issue #141 would otherwise add noise from precedenceProject).
	p, staging := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  applications:
    - {name: web, port: 8080}
  defaults:
    deliveryMode: flux
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
`)
	if _, errs := Resolve(p, staging); !slices.Contains(errs.Codes(), ErrGitTargetMissing) {
		t.Errorf("effective flux mode without git must fail with semantic/git-target-missing, got %v", errs)
	}
}

func TestResolveEnvironmentOverridesDefaultsP4(t *testing.T) {
	p, prod := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  delivery:
    mode: argocd
    git: {repo: git@github.com:acme/deploy.git, path: shop/prod}
  policy:
    agents: propose-only        # taken whole; the project default does not merge in (P4)
  secrets:
    backend: sops
`)
	r := resolve(p, prod)
	if r.Environment.Mode != DeliveryArgoCD {
		t.Errorf("mode = %q, want argocd (environment beats project default)", r.Environment.Mode)
	}
	if r.Environment.Delivery.Git == nil || r.Environment.Delivery.Git.Path != "shop/prod" {
		t.Errorf("git target = %+v", r.Environment.Delivery.Git)
	}
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
  services:
    - {name: db, preset: ha-small}
`)
	r := resolve(p, e)
	if r.Services[0].Preset != PresetHASmall {
		t.Errorf("db preset = %q, want ha-small (P5)", r.Services[0].Preset)
	}
	if r.Services[1].Preset != PresetShared {
		t.Errorf("cache preset = %q, want shared (project value survives; preset default is shared)", r.Services[1].Preset)
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
	r := resolve(p, e)
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
  applications:
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
	if r.Environment.Mode != DeliveryDirect {
		t.Errorf("built-in delivery default = %q, want direct", r.Environment.Mode)
	}
	if r.Environment.Policy.Agents != AgentsProposeOnly {
		t.Errorf("built-in agents default = %q, want propose-only", r.Environment.Policy.Agents)
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
	if r.Applications[1].Kind != WorkloadCron {
		t.Errorf("kind = %q, want cron", r.Applications[1].Kind)
	}
}

func TestResolveRejectsInvalid(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: hello}
spec:
  image: i:1
  applications:
    - {name: web, port: 8080}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: dev}
spec:
  project: hello
  applications:
    - {name: ghost, replicas: {min: 2}}
`)
	if _, errs := Resolve(p, e); !slices.Contains(errs.Codes(), ErrUnknownApplication) {
		t.Errorf("resolving an invalid pair must fail with ref/unknown-application, got %v", errs)
	}
}
