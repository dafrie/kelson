package model

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The effective-config walk is a second reading of the same two documents, so
// the test that matters is not "does it produce plausible rows" but **does it
// agree with [resolve], value for value, on every fixture that exercises the
// precedence rules**. agreesWithResolve below is that assertion, and every case
// in this file runs it: a chain that drifts from resolve.go fails here by
// setting name rather than in a screenshot six months later.

// agreesWithResolve checks every setting the walk reported against the value
// [resolve] carries for it, and checks the other direction too — a value
// resolution has that the walk reported nothing about is a hole, not a pass.
//
// It takes the documents as well as the resolution because a shadow is a claim
// resolution cannot check: the losing value is precisely what the merge threw
// away, so the only thing that can confirm it is the document it was read from
// (see [checkShadows]).
func agreesWithResolve(t *testing.T, p *Project, e *Environment, r *Resolved, ef *Effective) {
	t.Helper()

	if ef.Project != r.Project || ef.Environment != r.Environment.Name {
		t.Errorf("addressed %s/%s, resolution is %s/%s", ef.Project, ef.Environment, r.Project, r.Environment.Name)
	}

	// P4, the environment-scoped chain.
	if got, want := settingValue(t, ef.Settings, GroupEnvironment, SettingPolicyAgents), string(r.Environment.Policy.Agents); got != want {
		t.Errorf("policy.agents = %q, resolution carries %q", got, want)
	}
	if len(r.Environment.Policy.Require) > 0 {
		if got, want := settingValue(t, ef.Settings, GroupEnvironment, SettingPolicyRequire), strings.Join(r.Environment.Policy.Require, ", "); got != want {
			t.Errorf("policy.require = %q, resolution carries %q", got, want)
		}
	} else if find(ef.Settings, GroupEnvironment, SettingPolicyRequire) != nil {
		t.Errorf("policy.require reported %q where resolution requires nothing", settingValue(t, ef.Settings, GroupEnvironment, SettingPolicyRequire))
	}
	if got, want := settingValue(t, ef.Settings, GroupEnvironment, SettingSecretsBackend), string(r.Environment.Secrets.Backend); got != want {
		t.Errorf("secrets.backend = %q, resolution carries %q", got, want)
	}
	if r.Environment.Secrets.Backend == SecretsExternalSecrets {
		if got, want := settingValue(t, ef.Settings, GroupEnvironment, SettingSecretsRefresh), r.Environment.Secrets.RefreshInterval; got != want {
			t.Errorf("secrets.refreshInterval = %q, resolution carries %q", got, want)
		}
	}
	if r.Environment.Secrets.Backend == SecretsSOPS {
		if got, want := settingValue(t, ef.Settings, GroupEnvironment, SettingSecretsAgeKey), r.Environment.Secrets.AgeKeySecret; got != want {
			t.Errorf("secrets.ageKeySecret = %q, resolution carries %q", got, want)
		}
	}

	// Every component resolution produced must be in the table, once.
	for _, rc := range r.Components {
		ec := ef.ComponentNamed(rc.Name)
		if ec == nil {
			t.Errorf("component %q resolved but is absent from the effective table", rc.Name)
			continue
		}
		agreesOnWorkload(t, r, rc, ec)
	}
	for _, rd := range r.DataServices {
		ec := ef.ComponentNamed(rd.Name)
		if ec == nil {
			t.Errorf("data component %q resolved but is absent from the effective table", rd.Name)
			continue
		}
		if got, want := settingValue(t, ec.Settings, GroupData, SettingPreset), string(rd.Preset); got != want {
			t.Errorf("%s preset = %q, resolution carries %q", rd.Name, got, want)
		}
	}
	for _, rc := range r.Charts {
		ec := ef.ComponentNamed(rc.Name)
		if ec == nil {
			t.Errorf("chart %q resolved but is absent from the effective table", rc.Name)
			continue
		}
		if len(ec.Settings) > 0 {
			t.Errorf("chart %q reported %d settings; no precedence rule reaches a chart", rc.Name, len(ec.Settings))
		}
	}
	if got, want := len(ef.Components), len(r.Components)+len(r.DataServices)+len(r.Charts); got != want {
		t.Errorf("effective table has %d components, resolution has %d", got, want)
	}

	// Every SetAt must be structurally complete: a level that names a document
	// must name one, and a level in a document must point at a field. Every
	// shadow must be a value the document it names actually holds.
	for _, s := range ef.Settings {
		checkSetAt(t, "environment", s)
		checkShadows(t, "environment", p, e, s)
	}
	for _, ec := range ef.Components {
		for _, s := range ec.Settings {
			checkSetAt(t, ec.Name, s)
			checkShadows(t, ec.Name, p, e, s)
		}
	}
}

// checkShadows is the agreement harness for what lost (#268). Four claims per
// entry, each of which fails the moment the walk invents one:
//
//   - the value is what the shadowed document holds at that field — read back
//     out of the decoded documents here, not asked of the walk again;
//   - the entry is structurally complete, exactly as the winner's own answer is;
//   - the winner is not in its own shadow list;
//   - the list is outermost first, so a consumer can render the chain in one
//     direction without sorting it.
func checkShadows(t *testing.T, where string, p *Project, e *Environment, s EffectiveSetting) {
	t.Helper()
	last := -1
	for i, sh := range s.Shadowed {
		checkSetAt(t, where, EffectiveSetting{Name: s.Name + " (shadowed)", SetAt: sh.SetAt})
		if sh.SetAt.Level == SetAtBuiltIn {
			t.Errorf("%s/%s: a built-in shadows something; it is the outermost answer and displaces nothing", where, s.Name)
			continue
		}
		if sh.SetAt == s.SetAt {
			t.Errorf("%s/%s: the winning block %+v is in its own shadow list", where, s.Name, s.SetAt)
		}
		if got := documentValue(t, p, e, sh.SetAt); got.String() != sh.Value.String() {
			t.Errorf("%s/%s: shadow %d claims %s at %s, the document holds %s",
				where, s.Name, i, sh.Value.String(), sh.SetAt.Field, got.String())
		}
		if rank := levelRank(sh.SetAt.Level); rank <= last {
			t.Errorf("%s/%s: shadow %d is at %q, which is not outside the entry before it",
				where, s.Name, i, sh.SetAt.Level)
		} else {
			last = rank
		}
	}
}

// levelRank orders the levels the way the merge applies them, outermost first.
// It exists only for the ordering assertion; nothing in the model branches on
// it, and a consumer reads the list's order rather than recomputing this.
func levelRank(level SetAtLevel) int {
	switch level {
	case SetAtBuiltIn:
		return 0
	case SetAtProject:
		return 1
	case SetAtComponent:
		return 2
	case SetAtEnvironment:
		return 3
	case SetAtEnvironmentComponent:
		return 4
	}
	return -1
}

// documentValue reads what a document actually holds at one provenance answer's
// own field. It walks the decoded documents rather than asking the walk again,
// which is what makes the shadow assertion an agreement rather than a mirror:
// the claim under test is "this block writes this value here", and the only way
// to check it is to go and look.
func documentValue(t *testing.T, p *Project, e *Environment, at SetAt) EnvValue {
	t.Helper()
	path, ok := strings.CutPrefix(at.Field, "$.spec.")
	if !ok {
		t.Fatalf("field %q is not a path into a spec", at.Field)
	}
	if rest, index, ok := componentPath(path); ok {
		switch at.Document {
		case KindProject:
			if index >= len(p.Spec.Components) {
				t.Fatalf("field %q points past the project's components", at.Field)
			}
			return componentValue(t, p.Spec.Components[index], rest, at.Field)
		case KindEnvironment:
			if index >= len(e.Spec.Components) {
				t.Fatalf("field %q points past the environment's overrides", at.Field)
			}
			return overrideValue(t, e.Spec.Components[index], rest, at.Field)
		}
	}
	switch at.Document {
	case KindProject:
		return projectValue(t, p, path, at.Field)
	case KindEnvironment:
		return environmentValue(t, e, path, at.Field)
	}
	t.Fatalf("field %q names document %q", at.Field, at.Document)
	return EnvValue{}
}

// componentPath splits `components[3].env.LOG_LEVEL` into the index and what
// follows it.
func componentPath(path string) (string, int, bool) {
	rest, ok := strings.CutPrefix(path, "components[")
	if !ok {
		return "", 0, false
	}
	digits, rest, ok := strings.Cut(rest, "].")
	if !ok {
		return "", 0, false
	}
	index, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}
	return rest, index, true
}

func componentValue(t *testing.T, c Component, path, field string) EnvValue {
	t.Helper()
	if key, ok := strings.CutPrefix(path, "env."); ok {
		return c.Env[key]
	}
	if suffix, ok := strings.CutPrefix(path, settingResourcesSuffix); ok {
		return EnvValue{Literal: documentQuantity(c.Resources, suffix)}
	}
	switch path {
	case SettingImage:
		return EnvValue{Literal: c.Image}
	case SettingCommand:
		return EnvValue{Literal: strings.Join(c.Command, " ")}
	case SettingDomains:
		return EnvValue{Literal: strings.Join(c.Domains, ", ")}
	case SettingPreset:
		return EnvValue{Literal: string(c.Preset)}
	case SettingReplicas:
		if c.Replicas == nil {
			return EnvValue{}
		}
		return EnvValue{Literal: replicaText(*c.Replicas)}
	}
	t.Fatalf("no reading for component field %q", field)
	return EnvValue{}
}

func overrideValue(t *testing.T, ov ComponentOverride, path, field string) EnvValue {
	t.Helper()
	if key, ok := strings.CutPrefix(path, "env."); ok {
		return ov.Env[key]
	}
	if suffix, ok := strings.CutPrefix(path, settingResourcesSuffix); ok {
		return EnvValue{Literal: documentQuantity(ov.Resources, suffix)}
	}
	switch path {
	case SettingImage:
		return EnvValue{Literal: ov.Image}
	case SettingPreset:
		return EnvValue{Literal: string(ov.Preset)}
	case SettingReplicas:
		if ov.Replicas == nil {
			return EnvValue{}
		}
		return EnvValue{Literal: replicaText(*ov.Replicas)}
	case SettingAutoDeploy:
		if ov.AutoDeploy == nil {
			return EnvValue{}
		}
		return EnvValue{Literal: boolText(*ov.AutoDeploy)}
	}
	t.Fatalf("no reading for override field %q", field)
	return EnvValue{}
}

func projectValue(t *testing.T, p *Project, path, field string) EnvValue {
	t.Helper()
	if key, ok := strings.CutPrefix(path, "env."); ok {
		return p.Spec.Env[key]
	}
	if path == SettingImage {
		return EnvValue{Literal: p.Spec.Image}
	}
	d := p.Spec.Defaults
	if rest, ok := strings.CutPrefix(path, "defaults.policy."); ok && d != nil && d.Policy != nil {
		return EnvValue{Literal: policyField(t, *d.Policy, rest, field)}
	}
	if rest, ok := strings.CutPrefix(path, "defaults.secrets."); ok && d != nil && d.Secrets != nil {
		return EnvValue{Literal: secretsField(t, *d.Secrets, rest, field)}
	}
	t.Fatalf("no reading for project field %q", field)
	return EnvValue{}
}

func environmentValue(t *testing.T, e *Environment, path, field string) EnvValue {
	t.Helper()
	if rest, ok := strings.CutPrefix(path, "policy."); ok && e.Spec.Policy != nil {
		return EnvValue{Literal: policyField(t, *e.Spec.Policy, rest, field)}
	}
	if rest, ok := strings.CutPrefix(path, "secrets."); ok && e.Spec.Secrets != nil {
		return EnvValue{Literal: secretsField(t, *e.Spec.Secrets, rest, field)}
	}
	switch path {
	case SettingAutoDeploy:
		if e.Spec.AutoDeploy == nil {
			return EnvValue{}
		}
		return EnvValue{Literal: boolText(*e.Spec.AutoDeploy)}
	case "routing.domainSuffix":
		if e.Spec.Routing == nil {
			return EnvValue{}
		}
		return EnvValue{Literal: e.Spec.Routing.DomainSuffix}
	}
	t.Fatalf("no reading for environment field %q", field)
	return EnvValue{}
}

func policyField(t *testing.T, policy Policy, name, field string) string {
	t.Helper()
	switch name {
	case "agents":
		return string(policy.Agents)
	case "require":
		return strings.Join(policy.Require, ", ")
	}
	t.Fatalf("no reading for policy field %q", field)
	return ""
}

func secretsField(t *testing.T, secrets SecretBackend, name, field string) string {
	t.Helper()
	switch name {
	case "backend":
		return string(secrets.Backend)
	case "store":
		return secrets.Store
	case "refreshInterval":
		return secrets.RefreshInterval
	case "ageKeySecret":
		return secrets.AgeKeySecret
	}
	t.Fatalf("no reading for secrets field %q", field)
	return ""
}

// documentQuantity reads a resources leaf out of the document, spelled the way
// the row's own field spells it. It is written out here rather than shared with
// the walk on purpose: a shared accessor would agree with itself.
func documentQuantity(res *Resources, suffix string) string {
	if res == nil {
		return ""
	}
	switch suffix {
	case ".requests.cpu":
		if res.Requests != nil {
			return res.Requests.CPU
		}
	case ".requests.memory":
		if res.Requests != nil {
			return res.Requests.Memory
		}
	case ".limits.cpu":
		if res.Limits != nil {
			return res.Limits.CPU
		}
	case ".limits.memory":
		if res.Limits != nil {
			return res.Limits.Memory
		}
	}
	return ""
}

func agreesOnWorkload(t *testing.T, r *Resolved, rc ResolvedComponent, ec *EffectiveComponent) {
	t.Helper()

	// P1: the same keys and the same values, in both directions.
	seen := map[string]bool{}
	for _, s := range ec.Settings {
		if s.Group != GroupEnv {
			continue
		}
		seen[s.Name] = true
		want, ok := rc.Env[s.Name]
		if !ok {
			t.Errorf("%s: env %s is in the table and not in the resolution", rc.Name, s.Name)
			continue
		}
		if s.Value.String() != want.String() {
			t.Errorf("%s: env %s = %s, resolution carries %s", rc.Name, s.Name, s.Value.String(), want.String())
		}
	}
	for k := range rc.Env {
		if !seen[k] {
			t.Errorf("%s: env %s resolved but is absent from the table", rc.Name, k)
		}
	}

	// P3: an image is reported exactly when a document names one. A component
	// built from source and awaiting its build carries the sentinel in the
	// resolution and nothing here, which is the one deliberate disagreement.
	image := ec.Setting(GroupWorkload, SettingImage)
	switch {
	case image != nil && image.Value.Literal != rc.Image:
		t.Errorf("%s: image = %q, resolution carries %q", rc.Name, image.Value.Literal, rc.Image)
	case image == nil && rc.Image != "" && rc.Image != ImageUnresolved:
		t.Errorf("%s: resolution carries image %q and the table reports none", rc.Name, rc.Image)
	}

	command := ec.Setting(GroupWorkload, SettingCommand)
	switch {
	case command != nil && command.Value.Literal != strings.Join(rc.Command, " "):
		t.Errorf("%s: command = %q, resolution carries %v", rc.Name, command.Value.Literal, rc.Command)
	case command == nil && len(rc.Command) > 0:
		t.Errorf("%s: resolution carries command %v and the table reports none", rc.Name, rc.Command)
	}

	// P2.
	if got, want := settingValue(t, ec.Settings, GroupWorkload, SettingReplicas), replicaText(rc.Replicas); got != want {
		t.Errorf("%s: replicas = %q, resolution carries %q", rc.Name, got, want)
	}
	agreesOnResources(t, rc, ec)

	// Domain defaulting.
	domains := ec.Setting(GroupWorkload, SettingDomains)
	switch {
	case domains != nil && domains.Value.Literal != strings.Join(rc.Domains, ", "):
		t.Errorf("%s: domains = %q, resolution carries %v", rc.Name, domains.Value.Literal, rc.Domains)
	case domains == nil && len(rc.Domains) > 0:
		t.Errorf("%s: resolution carries domains %v and the table reports none", rc.Name, rc.Domains)
	}

	// ADR-0036 decision 1.
	if got, want := settingValue(t, ec.Settings, GroupWorkload, SettingAutoDeploy), boolText(r.AutoDeploys(rc.Name)); got != want {
		t.Errorf("%s: autoDeploy = %q, resolution carries %q", rc.Name, got, want)
	}
}

func agreesOnResources(t *testing.T, rc ResolvedComponent, ec *EffectiveComponent) {
	t.Helper()
	want := map[string]string{}
	if rc.Resources != nil {
		if req := rc.Resources.Requests; req != nil {
			want[settingRequestsCPU], want[settingRequestsMemory] = req.CPU, req.Memory
		}
		if lim := rc.Resources.Limits; lim != nil {
			want[settingLimitsCPU], want[settingLimitsMemory] = lim.CPU, lim.Memory
		}
	}
	for name, value := range want {
		got := ec.Setting(GroupWorkload, name)
		if value == "" {
			if got != nil {
				t.Errorf("%s: %s reported as %q where resolution sets none", rc.Name, name, got.Value.Literal)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: resolution carries %s = %q and the table reports none", rc.Name, name, value)
			continue
		}
		if got.Value.Literal != value {
			t.Errorf("%s: %s = %q, resolution carries %q", rc.Name, name, got.Value.Literal, value)
		}
	}
	for _, s := range ec.Settings {
		if !strings.HasPrefix(s.Name, "resources.") {
			continue
		}
		if _, ok := want[s.Name]; !ok {
			t.Errorf("%s: %s is in the table and not in the resolution", rc.Name, s.Name)
		}
	}
}

// checkSetAt is the structural contract of a provenance answer: a consumer has
// to be able to build a sentence from it without guessing.
func checkSetAt(t *testing.T, where string, s EffectiveSetting) {
	t.Helper()
	at := s.SetAt
	switch at.Level {
	case SetAtBuiltIn:
		if at.Document != "" || at.Field != "" {
			t.Errorf("%s/%s: a built-in default names document %q and field %q; it is in no document",
				where, s.Name, at.Document, at.Field)
		}
	case SetAtProject:
		if at.Document != KindProject || at.Field == "" {
			t.Errorf("%s/%s: project level says document %q field %q", where, s.Name, at.Document, at.Field)
		}
	case SetAtComponent:
		if at.Document != KindProject || at.Component == "" || at.Field == "" {
			t.Errorf("%s/%s: component level says document %q component %q field %q",
				where, s.Name, at.Document, at.Component, at.Field)
		}
	case SetAtEnvironment:
		if at.Document != KindEnvironment || at.Environment == "" || at.Field == "" {
			t.Errorf("%s/%s: environment level says document %q environment %q field %q",
				where, s.Name, at.Document, at.Environment, at.Field)
		}
	case SetAtEnvironmentComponent:
		if at.Document != KindEnvironment || at.Environment == "" || at.Component == "" || at.Field == "" {
			t.Errorf("%s/%s: environment-component level says document %q environment %q component %q field %q",
				where, s.Name, at.Document, at.Environment, at.Component, at.Field)
		}
	default:
		t.Errorf("%s/%s: unknown set-at level %q", where, s.Name, at.Level)
	}
	if strings.Contains(at.Field, "[-1]") {
		t.Errorf("%s/%s: field %q points at an override that does not exist", where, s.Name, at.Field)
	}
}

// find looks a setting up by group and name, never by name alone: an
// environment variable and a workload setting may share a name, and the group
// is what tells them apart (see [SettingGroup]).
func find(settings []EffectiveSetting, group SettingGroup, name string) *EffectiveSetting {
	for i := range settings {
		if settings[i].Group == group && settings[i].Name == name {
			return &settings[i]
		}
	}
	return nil
}

func settingValue(t *testing.T, settings []EffectiveSetting, group SettingGroup, name string) string {
	t.Helper()
	s := find(settings, group, name)
	if s == nil {
		t.Errorf("no %s setting named %s; the table has %v", group, name, settingNames(settings))
		return ""
	}
	return s.Value.Literal
}

func settingNames(settings []EffectiveSetting) []string {
	out := make([]string, 0, len(settings))
	for _, s := range settings {
		out = append(out, s.Name)
	}
	return out
}

func setAtOf(t *testing.T, settings []EffectiveSetting, group SettingGroup, name string) SetAt {
	t.Helper()
	s := find(settings, group, name)
	if s == nil {
		t.Fatalf("no %s setting named %s; the table has %v", group, name, settingNames(settings))
	}
	return s.SetAt
}

func effectiveOf(t *testing.T, p *Project, e *Environment) (*Resolved, *Effective) {
	t.Helper()
	return resolved(t, p, e), effectiveConfig(p, e)
}

// TestEffectiveAgreesWithResolveOnEveryPrecedenceFixture runs the whole
// precedence corpus through both readings at once. precedenceProject exercises
// P1–P6 in one document, and each environment below turns a different rule's
// innermost scope on.
func TestEffectiveAgreesWithResolveOnEveryPrecedenceFixture(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"P1 environment override of an env key", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  routing: {domainSuffix: staging.example.com}
  components:
    - name: web
      env:
        LOG_LEVEL: trace
`},
		{"P2 replicas replaced whole", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  routing: {domainSuffix: staging.example.com}
  components:
    - name: web
      replicas: {min: 5}
`},
		{"P2 resources replaced whole", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  components:
    - name: web
      resources:
        requests: {cpu: 500m, memory: 512Mi}
        limits: {cpu: "2", memory: 1Gi}
`},
		{"P3 environment image pin", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  components:
    - name: web
      image: ghcr.io/acme/shop@sha256:1111111111111111111111111111111111111111111111111111111111111111
`},
		{"P4 project defaults, nothing on the environment", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
`},
		{"P4 environment block taken whole", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  policy:
    agents: propose-only
  secrets:
    backend: sops
`},
		{"P5 data preset override", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  components:
    - {name: db, preset: ha-small}
`},
		{"P6 overlays, which the table does not claim", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  overlays:
    - patch: ./k8s/staging.yaml
`},
		{"autoDeploy at both levels", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  autoDeploy: true
  components:
    - name: worker
      autoDeploy: false
`},
		{"externalSecrets, whose interval resolution defaults", `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  secrets:
    backend: externalSecrets
    store: vault
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, e := loadPairUnvalidated(t, precedenceProject, tc.env)
			r, ef := effectiveOf(t, p, e)
			agreesWithResolve(t, p, e, r, ef)
		})
	}
}

// TestEffectiveNamesTheScopeThatWonP1 is the table's whole reason for existing:
// three scopes write LOG_LEVEL and the reader is told which one is in force,
// while a key only the Project sets keeps the Project as its answer.
func TestEffectiveNamesTheScopeThatWonP1(t *testing.T) {
	p, e := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  components:
    - name: web
      env:
        LOG_LEVEL: trace
`)
	r, ef := effectiveOf(t, p, e)
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	at := setAtOf(t, web.Settings, GroupEnv, "LOG_LEVEL")
	if at.Level != SetAtEnvironmentComponent || at.Environment != "staging" || at.Component != "web" {
		t.Errorf("LOG_LEVEL set-at = %+v, want staging's override of web", at)
	}
	if at.Field != "$.spec.components[0].env.LOG_LEVEL" {
		t.Errorf("LOG_LEVEL field = %q, want the override's own path", at.Field)
	}
	if got := setAtOf(t, web.Settings, GroupEnv, "REGION").Level; got != SetAtProject {
		t.Errorf("REGION set-at = %q, want the project — nothing inner names it", got)
	}

	// The component wins over the project for a key only those two set: the
	// worker has no override at all and reads the project's value.
	worker := ef.ComponentNamed("worker")
	if worker == nil {
		t.Fatal("worker is absent")
	}
	if got := setAtOf(t, worker.Settings, GroupEnv, "LOG_LEVEL").Level; got != SetAtProject {
		t.Errorf("worker LOG_LEVEL set-at = %q, want the project — only web overrides it", got)
	}
	if got := setAtOf(t, worker.Settings, GroupWorkload, SettingImage).Level; got != SetAtComponent {
		t.Errorf("worker image set-at = %q, want the component (P3)", got)
	}
}

// shadowsOf renders one setting's shadow chain as `value@field` strings, in the
// order it carries them, so a test can state the whole chain in one line.
func shadowsOf(t *testing.T, settings []EffectiveSetting, group SettingGroup, name string) []string {
	t.Helper()
	s := find(settings, group, name)
	if s == nil {
		t.Fatalf("no %s setting named %s; the table has %v", group, name, settingNames(settings))
	}
	out := make([]string, 0, len(s.Shadowed))
	for _, sh := range s.Shadowed {
		out = append(out, fmt.Sprintf("%s@%s", sh.Value.String(), sh.SetAt.Field))
	}
	return out
}

// TestEffectiveShadowsTheChainTheWinnerReplaced is #268 item 1: the same three
// scopes as the test above, now reported as a chain rather than as a winner
// with an unexplained value. The order is the merge's own — outermost first —
// so a consumer draws the losers in the direction they were applied and the
// winner is what follows the list.
func TestEffectiveShadowsTheChainTheWinnerReplaced(t *testing.T) {
	p, e := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  components:
    - name: web
      env:
        LOG_LEVEL: trace
`)
	r, ef := effectiveOf(t, p, e)
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	got := shadowsOf(t, web.Settings, GroupEnv, "LOG_LEVEL")
	want := []string{
		"info@$.spec.env.LOG_LEVEL",
		"debug@$.spec.components[2].env.LOG_LEVEL",
	}
	if !slices.Equal(got, want) {
		t.Errorf("LOG_LEVEL shadows = %v, want the project then the component", got)
	}

	// A key one scope set is a value with no chain behind it, and a key an
	// inner scope never mentions has none either — a shadow list is not a list
	// of scopes, it is a list of values that were replaced.
	if got := shadowsOf(t, web.Settings, GroupEnv, "REGION"); len(got) != 0 {
		t.Errorf("REGION shadows %v; only the project ever names it", got)
	}
	worker := ef.ComponentNamed("worker")
	if worker == nil {
		t.Fatal("worker is absent")
	}
	if got := shadowsOf(t, worker.Settings, GroupEnv, "LOG_LEVEL"); len(got) != 0 {
		t.Errorf("the worker's LOG_LEVEL shadows %v; only web overrides it", got)
	}
	// P3 on the worker: the component's image beat the project's, so the
	// project's is what it replaced.
	if got, want := shadowsOf(t, worker.Settings, GroupWorkload, SettingImage),
		[]string{"ghcr.io/acme/shop:2@$.spec.image"}; !slices.Equal(got, want) {
		t.Errorf("the worker's image shadows %v, want the project's image", got)
	}
}

// TestEffectiveShadowsAnEmptyOverride is the case the whole feature is least
// useful without: docs/model.md unsets a project variable for one environment
// by overriding it to the empty string, and the winner is then a value that
// says nothing about itself. The shadow is the only thing on the row that can
// say what was unset.
func TestEffectiveShadowsAnEmptyOverride(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  env:
    FEATURE_X: enabled
  components:
    - {name: web, port: 8080}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  components:
    - name: web
      env:
        FEATURE_X: ""
`)
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	feature := find(web.Settings, GroupEnv, "FEATURE_X")
	if feature == nil {
		t.Fatal("FEATURE_X is absent")
	}
	if feature.Value.Literal != "" || feature.SetAt.Level != SetAtEnvironmentComponent {
		t.Errorf("FEATURE_X = %+v, want the empty override production wrote", feature)
	}
	if got, want := shadowsOf(t, web.Settings, GroupEnv, "FEATURE_X"),
		[]string{"enabled@$.spec.env.FEATURE_X"}; !slices.Equal(got, want) {
		t.Errorf("FEATURE_X shadows %v, want the project value it unset", got)
	}
}

// TestEffectiveShadowedReferencesStayReferences: a shadowed `{secret, key}` is
// still a reference to a Secret kelson never reads, so it travels as one. There
// is no value to flatten it into, in either direction of the chain.
func TestEffectiveShadowedReferencesStayReferences(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: i:1
  env:
    STRIPE_KEY: {secret: checkout-stripe-test, key: secretKey}
    DATABASE_URL: {from: {service: db, key: uri}}
  components:
    - {name: db, kind: postgres, preset: small}
    - name: web
      port: 8080
      env:
        DATABASE_URL: postgres://localhost/dev
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  components:
    - name: web
      env:
        STRIPE_KEY: {secret: checkout-stripe-live, key: secretKey}
`)
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	stripe := find(web.Settings, GroupEnv, "STRIPE_KEY")
	if stripe == nil || len(stripe.Shadowed) != 1 {
		t.Fatalf("STRIPE_KEY = %+v, want one shadowed reference", stripe)
	}
	was := stripe.Shadowed[0].Value
	if was.Secret == nil || was.Secret.Name != "checkout-stripe-test" || was.Secret.Key != "secretKey" {
		t.Errorf("the shadowed STRIPE_KEY = %+v, want the test Secret's reference", was)
	}
	if was.Literal != "" {
		t.Errorf("a shadowed secret reference carried a literal %q", was.Literal)
	}
	// And the other direction: a literal that displaced a binding shadows the
	// binding as a binding.
	url := find(web.Settings, GroupEnv, "DATABASE_URL")
	if url == nil || len(url.Shadowed) != 1 {
		t.Fatalf("DATABASE_URL = %+v, want one shadowed binding", url)
	}
	if from := url.Shadowed[0].Value.From; from == nil || from.Service != "db" || from.Key != "uri" {
		t.Errorf("the shadowed DATABASE_URL = %+v, want the data-service binding", url.Shadowed[0].Value)
	}
}

// TestEffectiveResourcesShadowTheBlockTheyReplaced: P2 replaces the block
// whole, so the shadow is what the displaced block held **at that row's own
// field** — and a quantity the winning block does not set has no row here at
// all, exactly as it has no value in the container. The table does not invent a
// row to hang a lost quantity on, because a row whose winner does not exist is
// a setting kelson never merged.
func TestEffectiveResourcesShadowTheBlockTheyReplaced(t *testing.T) {
	p, e := loadPairUnvalidated(t, precedenceProject, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  components:
    - name: web
      replicas: {min: 5}
      resources:
        requests: {cpu: 500m, memory: 512Mi}
`)
	r, ef := effectiveOf(t, p, e)
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	if got, want := shadowsOf(t, web.Settings, GroupWorkload, settingRequestsCPU),
		[]string{"100m@$.spec.components[2].resources.requests.cpu"}; !slices.Equal(got, want) {
		t.Errorf("requests.cpu shadows %v, want the component's own quantity", got)
	}
	// The component's block says nothing about memory, so the environment's
	// memory request replaced nothing.
	if got := shadowsOf(t, web.Settings, GroupWorkload, settingRequestsMemory); len(got) != 0 {
		t.Errorf("requests.memory shadows %v; the component's block sets none", got)
	}
	// Replicas are replaced whole and reported whole, so they shadow whole.
	if got, want := shadowsOf(t, web.Settings, GroupWorkload, SettingReplicas),
		[]string{"2–4@$.spec.components[2].replicas"}; !slices.Equal(got, want) {
		t.Errorf("replicas shadows %v, want the component's own range", got)
	}
}

// TestEffectiveKeepsSecretsAsReferences: an env value that is a reference stays
// one all the way out. There is no value in the spec, there is none in the
// resolution, and there must be none on this table — the row carries the Secret
// and the key and nothing else (ADR-0009, ADR-0018).
func TestEffectiveKeepsSecretsAsReferences(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:1
  env:
    DATABASE_URL: {from: {service: db, key: uri}}
  components:
    - {name: db, kind: postgres, preset: small}
    - name: web
      port: 8080
      env:
        STRIPE_KEY: {secret: checkout-stripe, key: secretKey}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
`)
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	stripe := find(web.Settings, GroupEnv, "STRIPE_KEY")
	if stripe == nil {
		t.Fatal("STRIPE_KEY is absent")
	}
	if stripe.Value.Secret == nil || stripe.Value.Secret.Name != "checkout-stripe" || stripe.Value.Secret.Key != "secretKey" {
		t.Errorf("STRIPE_KEY = %+v, want the {secret, key} reference", stripe.Value)
	}
	if stripe.Value.Literal != "" {
		t.Errorf("a secret reference carried a literal %q", stripe.Value.Literal)
	}
	url := find(web.Settings, GroupEnv, "DATABASE_URL")
	if url == nil || url.Value.From == nil || url.Value.From.Service != "db" {
		t.Fatalf("DATABASE_URL = %+v, want the data-service binding", url)
	}
	if url.Value.Literal != "" {
		t.Errorf("a binding carried a literal %q", url.Value.Literal)
	}
}

// TestEffectiveReportsNoImageWhenNothingNamesOne: a component awaiting its
// first build resolves to the ImageUnresolved sentinel, which is not a value
// anybody set and must not appear in a table of what runs (#136).
func TestEffectiveReportsNoImageWhenNothingNamesOne(t *testing.T) {
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
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	if r.Components[0].Image != ImageUnresolved {
		t.Fatalf("fixture no longer awaits a build: image = %q", r.Components[0].Image)
	}
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	if s := web.Setting(GroupWorkload, SettingImage); s != nil {
		t.Errorf("image reported as %q; nothing in these documents names one", s.Value.Literal)
	}
	for _, s := range web.Settings {
		if strings.Contains(s.Value.Literal, ImageUnresolved) && s.Name == SettingImage {
			t.Errorf("the sentinel reached the table on %s", s.Name)
		}
	}
}

// TestEffectiveBuiltInDefaultsAreNamedAsSuch: a document that says nothing
// still produces answers, and each says it came from kelson rather than from a
// file the reader could open.
func TestEffectiveBuiltInDefaultsAreNamedAsSuch(t *testing.T) {
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
`)
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	agreesWithResolve(t, p, e, r, ef)

	for _, name := range []string{SettingPolicyAgents, SettingSecretsBackend} {
		if got := setAtOf(t, ef.Settings, GroupEnvironment, name).Level; got != SetAtBuiltIn {
			t.Errorf("%s set-at = %q, want the built-in", name, got)
		}
	}
	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	if got := setAtOf(t, web.Settings, GroupWorkload, SettingReplicas); got.Level != SetAtBuiltIn {
		t.Errorf("replicas set-at = %+v, want the built-in {min: 1}", got)
	}
	if got := settingValue(t, web.Settings, GroupWorkload, SettingReplicas); got != "1" {
		t.Errorf("replicas = %q, want 1", got)
	}
	if got := setAtOf(t, web.Settings, GroupWorkload, SettingAutoDeploy); got.Level != SetAtBuiltIn {
		t.Errorf("autoDeploy set-at = %+v, want the built-in false", got)
	}
	if s := web.Setting(GroupWorkload, SettingDomains); s != nil {
		t.Errorf("domains reported %q with no suffix and no explicit domains", s.Value.Literal)
	}

	// Nothing was overridden anywhere, so no row carries a chain. A shadow list
	// that filled up on a document nobody edited would be a scope list wearing
	// a shadow's clothes (#268).
	for _, s := range append(slices.Clone(ef.Settings), web.Settings...) {
		if len(s.Shadowed) > 0 {
			t.Errorf("%s shadows %+v; these two documents override nothing", s.Name, s.Shadowed)
		}
	}
}

// TestEffectivePolicyBlockIsTakenWhole is the P4 subtlety that surprises
// people, stated as provenance: an environment that writes `policy:` at all
// replaces the project's block, so a field it leaves unset falls to the
// **built-in** rather than back to the project.
func TestEffectivePolicyBlockIsTakenWhole(t *testing.T) {
	p, e := loadPairUnvalidated(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
  defaults:
    policy:
      agents: propose-only
      require: [dry-run]
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  policy:
    require: [dry-run]
`)
	r, ef := effectiveOf(t, p, e)
	agreesWithResolve(t, p, e, r, ef)

	if got := settingValue(t, ef.Settings, GroupEnvironment, SettingPolicyAgents); got != string(AgentsAllow) {
		t.Errorf("policy.agents = %q, want allow — the environment's block replaced the project's", got)
	}
	if got := setAtOf(t, ef.Settings, GroupEnvironment, SettingPolicyAgents).Level; got != SetAtBuiltIn {
		t.Errorf("policy.agents set-at = %q, want the built-in — the project's block is gone", got)
	}
	at := setAtOf(t, ef.Settings, GroupEnvironment, SettingPolicyRequire)
	if at.Level != SetAtEnvironment || at.Field != "$.spec.policy.require" {
		t.Errorf("policy.require set-at = %+v, want the environment's own block", at)
	}

	// The block being taken whole is exactly what a shadow makes legible: the
	// project *did* say propose-only, the environment's block displaced it, and
	// the winner is kelson's default. Without the entry the row reads as though
	// nobody ever asked for propose-only (#268).
	if got, want := shadowsOf(t, ef.Settings, GroupEnvironment, SettingPolicyAgents),
		[]string{"propose-only@$.spec.defaults.policy.agents"}; !slices.Equal(got, want) {
		t.Errorf("policy.agents shadows %v, want the project default the block displaced", got)
	}
	if got, want := shadowsOf(t, ef.Settings, GroupEnvironment, SettingPolicyRequire),
		[]string{"dry-run@$.spec.defaults.policy.require"}; !slices.Equal(got, want) {
		t.Errorf("policy.require shadows %v, want the project's own require", got)
	}
}

// TestEffectiveDerivedHostBelongsToTheEnvironment: a component with a port and
// no domains gets `<component>.<suffix>`, and the block a reader edits to
// change it is the environment's routing — not the component, which says
// nothing about hostnames at all.
func TestEffectiveDerivedHostBelongsToTheEnvironment(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
    - {name: api, port: 9090, domains: [api.acme.com]}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
  routing: {domainSuffix: staging.acme.run}
`)
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	if got := settingValue(t, web.Settings, GroupWorkload, SettingDomains); got != "web.staging.acme.run" {
		t.Errorf("web domains = %q, want the derived host", got)
	}
	at := setAtOf(t, web.Settings, GroupWorkload, SettingDomains)
	if at.Level != SetAtEnvironment || at.Field != "$.spec.routing.domainSuffix" {
		t.Errorf("derived host set-at = %+v, want the environment's routing block", at)
	}
	api := ef.ComponentNamed("api")
	if api == nil {
		t.Fatal("api is absent")
	}
	if got := setAtOf(t, api.Settings, GroupWorkload, SettingDomains).Level; got != SetAtComponent {
		t.Errorf("explicit domains set-at = %q, want the component", got)
	}
}

// TestEffectiveChartHasNoSettings: no precedence rule reaches a `kind: helm`
// component, and an environment takes no override on one. The honest table says
// nothing rather than inventing defaults for a chart's values.
func TestEffectiveChartHasNoSettings(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  env:
    LOG_LEVEL: info
  components:
    - {name: web, port: 8080}
    - name: ingress
      kind: helm
      chart: ingress-nginx
      chartVersion: 4.11.3
      source: {repository: https://kubernetes.github.io/ingress-nginx}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
`)
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	agreesWithResolve(t, p, e, r, ef)

	chart := ef.ComponentNamed("ingress")
	if chart == nil {
		t.Fatal("the chart component is absent from the table")
	}
	if chart.Kind != ComponentHelm {
		t.Errorf("kind = %q, want helm", chart.Kind)
	}
	if len(chart.Settings) != 0 {
		t.Errorf("chart carries %v; the project's env must not leak onto it", settingNames(chart.Settings))
	}
}

// TestEffectiveConfigRefusesAnInvalidPair: a pair that does not validate has no
// effective configuration, exactly as it has no resolution.
func TestEffectiveConfigRefusesAnInvalidPair(t *testing.T) {
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
	ef, errs := EffectiveConfig(p, e)
	if ef != nil {
		t.Errorf("an invalid pair produced a table: %+v", ef)
	}
	if !slices.Contains(errs.Codes(), ErrUnknownComponent) {
		t.Errorf("errors = %v, want ref/unknown-component", errs)
	}
}

// TestEffectiveFieldsAreUniqueWithinAComponent: the table is addressed by
// (group, name), and a duplicate row would make one of the two unreachable.
func TestEffectiveFieldsAreUniqueWithinAComponent(t *testing.T) {
	// An environment variable deliberately named after a workload setting: the
	// one case where separating on the name alone would collide.
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - name: web
      port: 8080
      command: ["./serve", "--port", "8080"]
      env:
        image: not-a-reference
        replicas: "7"
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: shop
`)
	ef, errs := EffectiveConfig(p, e)
	if len(errs) > 0 {
		t.Fatalf("EffectiveConfig: %v", errs)
	}
	r, rerrs := Resolve(p, e)
	if len(rerrs) > 0 {
		t.Fatalf("Resolve: %v", rerrs)
	}
	agreesWithResolve(t, p, e, r, ef)

	web := ef.ComponentNamed("web")
	if web == nil {
		t.Fatal("web is absent")
	}
	if got := web.Setting(GroupWorkload, SettingCommand); got == nil || got.Value.Literal != "./serve --port 8080" {
		t.Errorf("command row = %+v, want the component's own argv", got)
	}
	seen := map[string]bool{}
	for _, s := range web.Settings {
		key := fmt.Sprintf("%s/%s", s.Group, s.Name)
		if seen[key] {
			t.Errorf("duplicate setting %s", key)
		}
		seen[key] = true
	}
	if got := web.Setting(GroupWorkload, SettingImage); got == nil || got.Value.Literal != "ghcr.io/acme/shop:2" {
		t.Errorf("the workload image row = %+v, want the project's image", got)
	}
	if got := web.Setting(GroupEnv, "image"); got == nil || got.Value.Literal != "not-a-reference" {
		t.Errorf("the env row named image = %+v, want the authored value", got)
	}
	if got := web.Setting(GroupWorkload, SettingReplicas); got == nil || got.Value.Literal != "1" {
		t.Errorf("the workload replicas row = %+v, want the built-in", got)
	}
}
