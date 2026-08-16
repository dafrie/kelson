package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/model"
)

// The effective-config fixture writes the same key at three scopes, so the
// answer to "which one is in force" is different for each component and cannot
// be right by accident.
const (
	effectiveProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  image: ghcr.io/acme/checkout:1
  env:
    LOG_LEVEL: info
    REGION: eu-central
    DATABASE_URL: {from: {service: db, key: uri}}
  components:
    - {name: db, kind: postgres, preset: small}
    - name: web
      port: 8080
      env:
        LOG_LEVEL: debug
        STRIPE_KEY: {secret: checkout-stripe, key: secretKey}
      replicas: {min: 2, max: 4}
    - name: worker
      image: ghcr.io/acme/checkout-worker:1
`

	effectiveProductionDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  routing:
    domainSuffix: acme.run
  secrets:
    backend: sops
    ageRecipients: [age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p]
  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:1111111111111111111111111111111111111111111111111111111111111111
      env:
        LOG_LEVEL: warn
    - name: db
      preset: ha-small
`
)

func storedEffectiveSpec(t *testing.T) clients {
	t.Helper()
	store := newFakeSpecStore()
	if _, err := store.Put(context.Background(), "checkout", controlstore.Documents{
		Project:      []byte(effectiveProjectDoc),
		Environments: map[string][]byte{"production": []byte(effectiveProductionDoc)},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	return serve(t, Options{Specs: store})
}

func settingOf(t *testing.T, settings []*kelsonv1alpha1.EffectiveSetting, group kelsonv1alpha1.SettingGroup, name string) *kelsonv1alpha1.EffectiveSetting {
	t.Helper()
	for _, s := range settings {
		if s.GetGroup() == group && s.GetName() == name {
			return s
		}
	}
	t.Fatalf("no %s setting named %s", group, name)
	return nil
}

// TestGetEffectiveConfigNamesTheScopeThatWon is the acceptance criterion: three
// documents write LOG_LEVEL and the answer says which one is in force, for each
// component, with the path into the document it came from.
func TestGetEffectiveConfigNamesTheScopeThatWon(t *testing.T) {
	c := storedEffectiveSpec(t)

	got, err := c.spec.GetEffectiveConfig(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project: "checkout", Environment: "production",
	}))
	if err != nil {
		t.Fatalf("GetEffectiveConfig: %v", err)
	}
	if errs := got.Msg.GetErrors(); len(errs) > 0 {
		t.Fatalf("a valid spec reported errors: %v", errs)
	}
	config := got.Msg.GetConfig()
	if config.GetProject() != "checkout" || config.GetEnvironment() != "production" {
		t.Fatalf("answered for %s/%s", config.GetProject(), config.GetEnvironment())
	}
	if len(config.GetComponents()) != 3 {
		t.Fatalf("components = %d, want 3 (db, web, worker)", len(config.GetComponents()))
	}

	byName := map[string]*kelsonv1alpha1.EffectiveComponent{}
	for _, comp := range config.GetComponents() {
		byName[comp.GetName()] = comp
	}

	web := byName["web"]
	if web == nil {
		t.Fatal("web is absent")
	}
	logLevel := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV, "LOG_LEVEL")
	if logLevel.GetValue().GetLiteral() != "warn" {
		t.Errorf("web LOG_LEVEL = %q, want warn", logLevel.GetValue().GetLiteral())
	}
	at := logLevel.GetSetAt()
	if at.GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT_COMPONENT {
		t.Errorf("web LOG_LEVEL level = %v, want the environment's override", at.GetLevel())
	}
	if at.GetEnvironment() != "production" || at.GetComponent() != "web" || at.GetDocument() != "Environment" {
		t.Errorf("web LOG_LEVEL set-at = %+v, want production's override of web", at)
	}
	if at.GetField() != "$.spec.components[0].env.LOG_LEVEL" {
		t.Errorf("web LOG_LEVEL field = %q", at.GetField())
	}

	// The worker takes the same key from the project: no inner scope names it.
	worker := byName["worker"]
	if worker == nil {
		t.Fatal("worker is absent")
	}
	workerLevel := settingOf(t, worker.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV, "LOG_LEVEL")
	if workerLevel.GetValue().GetLiteral() != "info" {
		t.Errorf("worker LOG_LEVEL = %q, want info", workerLevel.GetValue().GetLiteral())
	}
	if workerLevel.GetSetAt().GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_PROJECT {
		t.Errorf("worker LOG_LEVEL level = %v, want the project", workerLevel.GetSetAt().GetLevel())
	}

	// P3, three ways: an environment pin, a component image, the project's.
	webImage := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_WORKLOAD, model.SettingImage)
	if !strings.HasPrefix(webImage.GetValue().GetLiteral(), "ghcr.io/acme/checkout@sha256:") {
		t.Errorf("web image = %q, want the environment's pin", webImage.GetValue().GetLiteral())
	}
	if webImage.GetSetAt().GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT_COMPONENT {
		t.Errorf("web image level = %v, want the environment's override", webImage.GetSetAt().GetLevel())
	}
	workerImage := settingOf(t, worker.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_WORKLOAD, model.SettingImage)
	if workerImage.GetSetAt().GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_COMPONENT {
		t.Errorf("worker image level = %v, want the component", workerImage.GetSetAt().GetLevel())
	}

	// P2: the component's replicas survive, and the derived host belongs to the
	// environment's routing block.
	replicas := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_WORKLOAD, model.SettingReplicas)
	if replicas.GetValue().GetLiteral() != "2–4" {
		t.Errorf("web replicas = %q, want 2–4", replicas.GetValue().GetLiteral())
	}
	domains := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_WORKLOAD, model.SettingDomains)
	if domains.GetValue().GetLiteral() != "web.acme.run" {
		t.Errorf("web domains = %q, want the derived host", domains.GetValue().GetLiteral())
	}
	if domains.GetSetAt().GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT {
		t.Errorf("derived host level = %v, want the environment", domains.GetSetAt().GetLevel())
	}

	// P5, and P4 on the envelope.
	db := byName["db"]
	if db == nil {
		t.Fatal("db is absent")
	}
	preset := settingOf(t, db.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_DATA, model.SettingPreset)
	if preset.GetValue().GetLiteral() != "ha-small" {
		t.Errorf("db preset = %q, want ha-small", preset.GetValue().GetLiteral())
	}
	backend := settingOf(t, config.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENVIRONMENT, model.SettingSecretsBackend)
	if backend.GetValue().GetLiteral() != "sops" {
		t.Errorf("secrets.backend = %q, want sops", backend.GetValue().GetLiteral())
	}
	agents := settingOf(t, config.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENVIRONMENT, model.SettingPolicyAgents)
	if agents.GetSetAt().GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_BUILT_IN {
		t.Errorf("policy.agents level = %v, want the built-in", agents.GetSetAt().GetLevel())
	}
	if agents.GetSetAt().GetDocument() != "" || agents.GetSetAt().GetField() != "" {
		t.Errorf("a built-in default named a document: %+v", agents.GetSetAt())
	}
}

// TestGetEffectiveConfigCarriesReferencesNotValues: the two mapping arms of an
// env value reach the wire as references. There is no value field on either
// message, so this is a check that the right arm is chosen — a reference
// flattened into a literal would print a Secret's name where a reader expects a
// credential's shape.
func TestGetEffectiveConfigCarriesReferencesNotValues(t *testing.T) {
	c := storedEffectiveSpec(t)
	got, err := c.spec.GetEffectiveConfig(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project: "checkout", Environment: "production", Component: "web",
	}))
	if err != nil {
		t.Fatalf("GetEffectiveConfig: %v", err)
	}
	if len(got.Msg.GetConfig().GetComponents()) != 1 {
		t.Fatalf("a component filter returned %d components", len(got.Msg.GetConfig().GetComponents()))
	}
	web := got.Msg.GetConfig().GetComponents()[0]

	stripe := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV, "STRIPE_KEY")
	ref := stripe.GetValue().GetSecret()
	if ref == nil {
		t.Fatalf("STRIPE_KEY = %+v, want a secret reference", stripe.GetValue())
	}
	if ref.GetSecret() != "checkout-stripe" || ref.GetKey() != "secretKey" {
		t.Errorf("STRIPE_KEY reference = %+v", ref)
	}
	if stripe.GetValue().GetLiteral() != "" {
		t.Errorf("a secret reference also carried a literal %q", stripe.GetValue().GetLiteral())
	}

	url := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV, "DATABASE_URL")
	binding := url.GetValue().GetBinding()
	if binding == nil || binding.GetService() != "db" || binding.GetKey() != "uri" {
		t.Errorf("DATABASE_URL = %+v, want the data-service binding", url.GetValue())
	}
}

// TestGetEffectiveConfigCarriesTheShadowedChain: the fixture writes LOG_LEVEL
// at all three scopes, so the winner reaches the wire with the two values it
// replaced, outermost first, each carrying the block it was written in (#268).
func TestGetEffectiveConfigCarriesTheShadowedChain(t *testing.T) {
	c := storedEffectiveSpec(t)
	got, err := c.spec.GetEffectiveConfig(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project: "checkout", Environment: "production", Component: "web",
	}))
	if err != nil {
		t.Fatalf("GetEffectiveConfig: %v", err)
	}
	web := got.Msg.GetConfig().GetComponents()[0]

	logLevel := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV, "LOG_LEVEL")
	shadowed := logLevel.GetShadowed()
	if len(shadowed) != 2 {
		t.Fatalf("LOG_LEVEL carries %d shadowed values, want the project's and the component's", len(shadowed))
	}
	if v, at := shadowed[0].GetValue().GetLiteral(), shadowed[0].GetSetAt(); v != "info" ||
		at.GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_PROJECT || at.GetField() != "$.spec.env.LOG_LEVEL" {
		t.Errorf("the outermost shadow is %q at %+v, want the project's info", v, at)
	}
	if v, at := shadowed[1].GetValue().GetLiteral(), shadowed[1].GetSetAt(); v != "debug" ||
		at.GetLevel() != kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_COMPONENT || at.GetField() != "$.spec.components[1].env.LOG_LEVEL" {
		t.Errorf("the inner shadow is %q at %+v, want the component's debug", v, at)
	}

	// A key one document names has no chain behind it, and neither does a row
	// nobody overrode.
	region := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV, "REGION")
	if len(region.GetShadowed()) != 0 {
		t.Errorf("REGION carries %d shadowed values; only the project names it", len(region.GetShadowed()))
	}

	// A shadowed reference stays a reference: there is no value behind a
	// `{secret, key}` in either half of the chain.
	stripe := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV, "STRIPE_KEY")
	if len(stripe.GetShadowed()) != 0 {
		t.Fatalf("STRIPE_KEY carries %d shadowed values; only the component names it", len(stripe.GetShadowed()))
	}

	// P3: the environment's pin displaced the project's image.
	image := settingOf(t, web.GetSettings(), kelsonv1alpha1.SettingGroup_SETTING_GROUP_WORKLOAD, model.SettingImage)
	if len(image.GetShadowed()) != 1 {
		t.Fatalf("the image carries %d shadowed values, want the project's", len(image.GetShadowed()))
	}
	if v := image.GetShadowed()[0].GetValue().GetLiteral(); v != "ghcr.io/acme/checkout:1" {
		t.Errorf("the shadowed image = %q, want the project's", v)
	}
}

// TestGetEffectiveConfigRefusesAComponentTheProjectDoesNotDeclare: an empty
// table would be indistinguishable from a chart's, which is legitimately empty,
// so a filter that names nothing is the caller's argument being wrong.
func TestGetEffectiveConfigRefusesAComponentTheProjectDoesNotDeclare(t *testing.T) {
	c := storedEffectiveSpec(t)
	_, err := c.spec.GetEffectiveConfig(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project: "checkout", Environment: "production", Component: "ghost",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "web") {
		t.Errorf("the refusal must name what is declared, got %v", err)
	}
}

// TestGetEffectiveConfigRefusesAnUnknownEnvironment: naming an environment the
// spec does not declare is an argument error, and the message lists the ones it
// does — the same rule --env follows.
func TestGetEffectiveConfigRefusesAnUnknownEnvironment(t *testing.T) {
	c := storedEffectiveSpec(t)
	_, err := c.spec.GetEffectiveConfig(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project: "checkout", Environment: "staging",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("the refusal must list the declared environments, got %v", err)
	}
}

// TestGetEffectiveConfigReportsAnInvalidSpecInline: a stored spec that no
// longer validates — written before a rule existed, or applied to the cluster
// by somebody's Flux — is an answer about the spec, not a transport failure, so
// it lands in `errors` with the RPC succeeding and no config.
func TestGetEffectiveConfigReportsAnInvalidSpecInline(t *testing.T) {
	store := newFakeSpecStore()
	if _, err := store.Put(context.Background(), "checkout", controlstore.Documents{
		Project: []byte(effectiveProjectDoc),
		Environments: map[string][]byte{"production": []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components:
    - {name: ghost, replicas: {min: 2}}
`)},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	c := serve(t, Options{Specs: store})

	got, err := c.spec.GetEffectiveConfig(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project: "checkout", Environment: "production",
	}))
	if err != nil {
		t.Fatalf("an invalid spec must be an answer, not a transport failure: %v", err)
	}
	if got.Msg.GetConfig() != nil {
		t.Errorf("a spec that does not validate produced a table: %+v", got.Msg.GetConfig())
	}
	errs := got.Msg.GetErrors()
	if len(errs) == 0 {
		t.Fatal("no errors reported for an invalid spec")
	}
	found := false
	for _, e := range errs {
		if e.GetCode() == string(model.ErrUnknownComponent) {
			found = true
		}
	}
	if !found {
		t.Errorf("errors = %v, want ref/unknown-component", errs)
	}
}

// TestGetEffectiveConfigNeedsAStore: a server built without the spec store says
// so rather than answering for a project it cannot read.
func TestGetEffectiveConfigNeedsAStore(t *testing.T) {
	c := serve(t, Options{})
	_, err := c.spec.GetEffectiveConfig(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project: "checkout", Environment: "production",
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented (err %v)", connect.CodeOf(err), err)
	}
}

// TestEverySetAtLevelReachesTheWire is the enum's coverage gate. wireSetAtLevel
// is an exhaustive map and its failure mode is silent — an unmapped level
// reaches a client as UNSPECIFIED, which is a provenance answer that says
// nothing — so every level the model defines is checked to map onto something
// else.
func TestEverySetAtLevelReachesTheWire(t *testing.T) {
	levels := []model.SetAtLevel{
		model.SetAtBuiltIn, model.SetAtProject, model.SetAtComponent,
		model.SetAtEnvironment, model.SetAtEnvironmentComponent,
	}
	seen := map[kelsonv1alpha1.SetAtLevel]bool{}
	for _, level := range levels {
		wire := wireSetAtLevel(level)
		if wire == kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_UNSPECIFIED {
			t.Errorf("%q has no wire value", level)
		}
		if seen[wire] {
			t.Errorf("%q shares a wire value with an earlier level", level)
		}
		seen[wire] = true
	}
	groups := []model.SettingGroup{model.GroupEnv, model.GroupWorkload, model.GroupData, model.GroupEnvironment}
	for _, group := range groups {
		if wireSettingGroup(group) == kelsonv1alpha1.SettingGroup_SETTING_GROUP_UNSPECIFIED {
			t.Errorf("%q has no wire value", group)
		}
	}
}
