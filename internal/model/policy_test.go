package model

import (
	"slices"
	"testing"
)

// The authoring half of issue #75 (ADR-0025): what a `policy:` block may say,
// what it may not, and what an environment that says nothing means. Enforcement
// is internal/api's and is tested there; this file is about the spec.

const policyProject = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - {name: db, kind: postgres, preset: small}
    - {name: web, port: 8080}
`

func policyEnv(t *testing.T, body string) (*Project, *Environment, Errors) {
	t.Helper()
	docs, errs := DecodeDocuments([]byte(policyProject + "\n---\n" +
		"apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata: {name: production}\nspec:\n  project: shop\n" + body))
	if len(docs) != 2 {
		t.Fatalf("decode produced %d documents: %v", len(docs), errs)
	}
	p := docs[0].(*Project)
	e := docs[1].(*Environment)
	return p, e, append(errs, ValidateSet(p, e)...)
}

// TestPolicyBlockValidates: the whole vocabulary of ADR-0025 authors cleanly,
// which is the precondition for every enforcement test in internal/api.
func TestPolicyBlockValidates(t *testing.T) {
	p, e, errs := policyEnv(t, `  policy:
    agents: propose-only
    require: [dry-run]
    maxReplicas: 3
    protect: [db]
    forbid: [secret-set, rollback]
`)
	if len(errs) != 0 {
		t.Fatalf("a fully populated policy must validate, got: %v", errs)
	}
	pol := EffectivePolicy(p, e)
	if pol.AllowsUnsupervised() {
		t.Error("propose-only must not allow unsupervised action")
	}
	if !pol.RequiresDryRun() {
		t.Error("require: [dry-run] must be reported by RequiresDryRun")
	}
	if !pol.Protects("db") || pol.Protects("web") {
		t.Errorf("protect = %v: only db is protected", pol.Protect)
	}
	if !pol.Forbids(AgentOpSecretSet) || !pol.Forbids(AgentOpRollback) || pol.Forbids(AgentOpDeploy) {
		t.Errorf("forbid = %v", pol.Forbid)
	}
	if pol.MaxReplicas == nil || *pol.MaxReplicas != 3 {
		t.Errorf("maxReplicas = %v, want 3", pol.MaxReplicas)
	}
}

// TestPolicyRefusesWhatItCannotEnforce: every field that could silently do
// nothing is refused instead.
func TestPolicyRefusesWhatItCannotEnforce(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		code Code
	}{
		"unknown mode":        {"  policy: {agents: sometimes}\n", ErrInvalidEnum},
		"unknown requirement": {"  policy: {require: [two-approvals]}\n", ErrInvalidEnum},
		"unknown operation":   {"  policy: {forbid: [drop-database]}\n", ErrInvalidEnum},
		"ceiling of zero":     {"  policy: {maxReplicas: 0}\n", ErrOutOfRange},
		"protects a stranger": {"  policy: {protect: [queue]}\n", ErrUnknownComponent},
		"deployers is gated":  {"  policy: {deployers: [platform]}\n", ErrNotImplemented},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, errs := policyEnv(t, tc.body)
			if !slices.Contains(errs.Codes(), tc.code) {
				t.Errorf("want %s, got %v", tc.code, errs)
			}
		})
	}
}

// TestPolicyProtectIsCheckedAgainstTheProject in both places a policy block can
// live. A protected name that matches nothing protects nothing, and the one
// field whose job is to stop an agent removing a database must not fail open on
// a typo.
func TestPolicyProtectIsCheckedAgainstTheProject(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
  defaults:
    policy: {protect: [db]}
`))
	if len(docs) != 1 {
		t.Fatalf("decode: %v", errs)
	}
	errs = append(errs, ValidateSet(docs[0].(*Project))...)
	if !slices.Contains(errs.Codes(), ErrUnknownComponent) {
		t.Errorf("a project default protecting an undeclared component must fail, got %v", errs)
	}
}

// TestEffectivePolicyIsP4: the environment's block wins whole, the project's
// default fills in when the environment has none, and neither means allow.
func TestEffectivePolicyIsP4(t *testing.T) {
	project := &Project{Spec: ProjectSpec{Defaults: &ProjectDefaults{
		Policy: &Policy{Agents: AgentsProposeOnly, Require: []string{PolicyRequireDryRun}},
	}}}

	if pol := EffectivePolicy(project, &Environment{}); pol.Agents != AgentsProposeOnly || !pol.RequiresDryRun() {
		t.Errorf("an environment with no policy inherits the project default whole, got %+v", pol)
	}

	env := &Environment{Spec: EnvironmentSpec{Policy: &Policy{Agents: AgentsAllow}}}
	pol := EffectivePolicy(project, env)
	if pol.Agents != AgentsAllow {
		t.Errorf("agents = %q, want the environment's allow", pol.Agents)
	}
	if pol.RequiresDryRun() {
		t.Error("P4 takes the block whole: the project's require must not merge into the environment's")
	}

	// Nothing anywhere, and a block that leaves `agents` unset, are the same
	// answer: unset restricts nothing (ADR-0025 §3).
	if pol := EffectivePolicy(nil, nil); pol.Agents != AgentsAllow {
		t.Errorf("no policy at all = %q, want allow", pol.Agents)
	}
	partial := &Environment{Spec: EnvironmentSpec{Policy: &Policy{Require: []string{PolicyRequireDryRun}}}}
	if pol := EffectivePolicy(nil, partial); pol.Agents != AgentsAllow || !pol.RequiresDryRun() {
		t.Errorf("a block with agents unset = %+v, want allow plus the dry-run requirement", pol)
	}
}
