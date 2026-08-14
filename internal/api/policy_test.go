package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
)

// The enforcement tests of issue #75 (ADR-0025). Like #74's, every one of them
// goes through the real HTTP gate, the real interceptor, the real handlers and
// the generated clients: the claim is that a modified client cannot get past
// this, and a claim about a boundary tested from inside the boundary is not a
// claim about anything.

// policySpec is a stored project with one environment per posture, so a test
// picks the environment whose rule it is about rather than restating a spec.
func policySpec() (projectDoc []byte, envs map[string][]byte) {
	project := []byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - {name: web, port: 8080}
    - {name: worker}
`)
	env := func(name, policy string) []byte {
		return []byte(fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: %s}
spec:
  project: shop
%s`, name, policy))
	}
	return project, map[string][]byte{
		// No policy block at all: the documented default, which narrows nothing.
		"development": env("development", ""),
		"production":  env("production", "  policy:\n    agents: propose-only\n"),
		"staging":     env("staging", "  policy:\n    require: [dry-run]\n"),
		"capped":      env("capped", "  policy:\n    maxReplicas: 2\n    protect: [worker]\n"),
		"partial":     env("partial", "  policy:\n    forbid: [secret-set, rollback]\n"),
	}
}

// policyServer is the #74 harness with the policy spec already stored, so every
// test below acts on an environment whose policy the server actually holds.
func policyServer(t *testing.T, opts Options) *gatedServer {
	t.Helper()
	store := newFakeSpecStore()
	opts.Specs = store
	project, envs := policySpec()
	if _, err := store.Put(t.Context(), "shop",
		controlstore.Documents{Project: project, Environments: envs}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the policy spec: %v", err)
	}
	return newGatedServer(t, opts)
}

// mutate is a driver for one mutating RPC: it runs the operation against
// project/environment and returns whatever the server answered. Streaming RPCs
// are driven to their first answer, which is where a refusal arrives.
type mutate func(t *testing.T, c clients, project, environment string) error

// policyDrivers is the call-site half of the policy gate table. Every operation
// in [agentOperations] must have one, and TestProposeOnlyRefusesEveryMutation
// drives all of them: a mutating handler that forgot to guard shows up here as
// a passing request, by name.
var policyDrivers = map[model.AgentOperation]mutate{
	model.AgentOpDeploy: deployTo,
	model.AgentOpRollback: func(t *testing.T, c clients, project, environment string) error {
		stream, err := c.deploy.Rollback(t.Context(), connect.NewRequest(&kelsonv1alpha1.RollbackRequest{
			Spec:        specRefFor(project),
			Environment: environment,
		}))
		if err != nil {
			return err
		}
		defer stream.Close() //nolint:errcheck // the test only wants the first error
		for stream.Receive() {
		}
		return stream.Err()
	},
	model.AgentOpPromote: func(t *testing.T, c clients, project, environment string) error {
		_, err := c.deploy.Promote(t.Context(), connect.NewRequest(&kelsonv1alpha1.PromoteRequest{
			Project:         project,
			FromEnvironment: "development",
			ToEnvironment:   environment,
		}))
		return err
	},
	model.AgentOpBuild: func(t *testing.T, c clients, project, environment string) error {
		stream, err := c.builds.Build(t.Context(), connect.NewRequest(&kelsonv1alpha1.BuildRequest{
			Spec:        specRefFor(project),
			Environment: environment,
		}))
		if err != nil {
			return err
		}
		defer stream.Close() //nolint:errcheck // the test only wants the first error
		for stream.Receive() {
		}
		return stream.Err()
	},
	model.AgentOpSecretSet: func(t *testing.T, c clients, project, environment string) error {
		_, err := c.secrets.SetSecret(t.Context(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
			Target: &kelsonv1alpha1.SecretTarget{Project: project, Environment: environment},
			Name:   "api-credentials",
			Values: map[string]string{"token": "s3cret"},
		}))
		return err
	},
	model.AgentOpSecretDelete: func(t *testing.T, c clients, project, environment string) error {
		_, err := c.secrets.DeleteSecret(t.Context(), connect.NewRequest(&kelsonv1alpha1.DeleteSecretRequest{
			Target: &kelsonv1alpha1.SecretTarget{Project: project, Environment: environment},
			Name:   "api-credentials",
		}))
		return err
	},
	model.AgentOpSpecWrite: func(t *testing.T, c clients, project, environment string) error {
		doc, envs := policySpec()
		// The spec an agent would like to store: the same documents, with the
		// environment's policy rewritten to let it through.
		envs[environment] = []byte(fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: %s}
spec:
  project: %s
  policy:
    agents: allow
`, environment, project))
		_, err := c.spec.PutSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
			Documents: &kelsonv1alpha1.SpecDocuments{Project: doc, Environments: envs},
			Force:     true,
		}))
		return err
	},
	model.AgentOpSpecDelete: func(t *testing.T, c clients, project, _ string) error {
		_, err := c.spec.DeleteSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.DeleteSpecRequest{
			Project: project,
			Force:   true,
		}))
		return err
	},
}

// --- the gate table -----------------------------------------------------------

// TestEveryMutatingMethodHasAPolicyOperation is the #141 gate-table discipline
// applied to policy: an RPC the scope table calls a mutation, with no operation
// name here, is an RPC no `forbid:` entry can name and no propose-only
// environment can refuse.
func TestEveryMutatingMethodHasAPolicyOperation(t *testing.T) {
	for procedure, row := range rpcScopes {
		op, named := agentOperations[procedure]
		switch {
		case row.Operation == controlstore.OpMutate && !named:
			t.Errorf(`%s is a mutating RPC with no entry in agentOperations (internal/api/policy.go).

Add one naming the operation `+"`policy.forbid`"+` knows it by, add the constant to
model.AgentOperations() if it is new, and guard the handler with s.guard — otherwise
this route is one no per-environment policy can refuse (issue #75).`, procedure)
		case row.Operation != controlstore.OpMutate && named:
			t.Errorf("%s is in agentOperations as %q but the scope table does not call it a mutation; "+
				"policy refuses mutations, so the two tables must agree", procedure, op)
		}
	}
	for _, op := range agentOperations {
		if _, ok := policyDrivers[op]; !ok {
			t.Errorf("operation %q has no driver in policyDrivers, so no test proves its handler is guarded", op)
		}
	}
	// And the model's vocabulary is exactly what the server enforces: a word the
	// spec accepts and nothing reads is the silence #141 exists to prevent.
	enforced := map[model.AgentOperation]bool{}
	for _, op := range agentOperations {
		enforced[op] = true
	}
	for _, op := range model.AgentOperations() {
		if !enforced[op] {
			t.Errorf("model accepts `forbid: [%s]` but no RPC maps to it: the spec would accept a word nothing enforces", op)
		}
	}
}

// --- the acceptance criterion --------------------------------------------------

// TestProposeOnlyRefusesEveryMutation is issue #75's acceptance criterion: an
// agent in a propose-only environment cannot mutate live state by ANY route.
// Every mutating RPC in the schema, one agent, one environment.
//
// Build is the documented exception and is asserted as such rather than
// skipped: it changes no environment, and `forbid: [build]` is the rule that
// stops it (covered by TestForbidNamesTheOperationItRefuses).
func TestProposeOnlyRefusesEveryMutation(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	for op, drive := range policyDrivers {
		t.Run(string(op), func(t *testing.T) {
			err := drive(t, agent, "shop", prodEnv)
			if op == model.AgentOpBuild {
				if hasCode(detailCodes(err), ErrPolicyProposeOnly) {
					t.Errorf("propose-only refused a build; a build changes no environment (ADR-0025)")
				}
				return
			}
			if authCode(err) != connect.CodePermissionDenied {
				t.Fatalf("%s in a propose-only environment = %v (code %s), want permission-denied",
					op, err, authCode(err))
			}
			if !hasCode(detailCodes(err), ErrPolicyProposeOnly) {
				t.Fatalf("the refusal does not carry %s: %v", ErrPolicyProposeOnly, detailCodes(err))
			}
			assertEscalates(t, err)
		})
	}
}

// TestAHumanIsNeverRestrictedByAgentPolicy is the other half of the criterion.
// The same requests, the same environment, the password path: policy is agent
// policy, and a rule that quietly governed people would be a different feature.
func TestAHumanIsNeverRestrictedByAgentPolicy(t *testing.T) {
	g := policyServer(t, Options{})
	human := g.as(testPassword)

	for op, drive := range policyDrivers {
		t.Run(string(op), func(t *testing.T) {
			err := drive(t, human, "shop", prodEnv)
			if authCode(err) == connect.CodePermissionDenied {
				t.Errorf("a human was refused %s in a propose-only environment: %v", op, err)
			}
			for _, code := range detailCodes(err) {
				if strings.HasPrefix(code, "agent-policy/") {
					t.Errorf("a human received an agent-policy refusal (%s): %v", code, err)
				}
			}
		})
	}
}

// TestPolicyAbsentAllowsEverything pins the documented default. The credential
// an operator issued is the grant (ADR-0024 §3); an environment whose spec says
// nothing about agents narrows nothing, and this is what would break loudly if
// that ever changed.
func TestPolicyAbsentAllowsEverything(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	for op, drive := range policyDrivers {
		if op == model.AgentOpSpecDelete || op == model.AgentOpSpecWrite {
			// Both reach every stored environment of the project, production
			// included, so they are governed by production's policy rather than
			// development's. TestSpecWritesCannotRelaxTheRuleThatBindsThem is
			// where they belong.
			continue
		}
		t.Run(string(op), func(t *testing.T) {
			err := drive(t, agent, "shop", devEnv)
			for _, code := range detailCodes(err) {
				if strings.HasPrefix(code, "agent-policy/") {
					t.Errorf("an environment with no policy refused %s with %s: %v", op, code, err)
				}
			}
		})
	}
}

// --- the individual rules ------------------------------------------------------

// TestForbidNamesTheOperationItRefuses: `forbid:` refuses exactly what it names
// and nothing else, which is also what pins each handler to the right operation
// constant — a handler guarding with the wrong name would refuse the wrong RPC.
func TestForbidNamesTheOperationItRefuses(t *testing.T) {
	for op, drive := range policyDrivers {
		t.Run(string(op), func(t *testing.T) {
			g := forbiddingServer(t, op)
			agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
				Operations: []controlstore.Operation{controlstore.OpMutate},
			}))

			err := drive(t, agent, "shop", "forbidden")
			if authCode(err) != connect.CodePermissionDenied {
				t.Fatalf("%s with forbid: [%s] = %v (code %s), want permission-denied", op, op, err, authCode(err))
			}
			if !hasCode(detailCodes(err), ErrPolicyForbidden) {
				t.Fatalf("the refusal does not carry %s: %v", ErrPolicyForbidden, detailCodes(err))
			}
			if !strings.Contains(errText(err), string(op)) {
				t.Errorf("the refusal does not name the operation %q it refused: %v", op, errText(err))
			}
		})
	}
}

// forbiddingServer stores a spec whose one environment forbids exactly op.
func forbiddingServer(t *testing.T, op model.AgentOperation) *gatedServer {
	t.Helper()
	store := newFakeSpecStore()
	project, _ := policySpec()
	env := []byte(fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: forbidden}
spec:
  project: shop
  policy:
    forbid: [%s]
`, op))
	// `development` exists because a promotion needs a source environment to
	// read from; the rule under test is on the target.
	_, envs := policySpec()
	if _, err := store.Put(t.Context(), "shop", controlstore.Documents{
		Project:      project,
		Environments: map[string][]byte{"forbidden": env, "development": envs["development"]},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing: %v", err)
	}
	return newGatedServer(t, Options{Specs: store})
}

// TestMaxReplicasIsCheckedAgainstTheResolvedSpec: the ceiling applies to what
// would actually run, and the refusal names the rule, the value and the limit.
func TestMaxReplicasIsCheckedAgainstTheResolvedSpec(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	// The stored spec runs one replica; the request pins nine through an
	// environment override, which is exactly the shape a limit has to catch.
	err := deployInline(t, agent, "capped", "  components:\n    - {name: web, replicas: {min: 9}}\n")
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("nine replicas under maxReplicas: 2 = %v (code %s), want permission-denied", err, authCode(err))
	}
	if !hasCode(detailCodes(err), ErrPolicyMaxReplicas) {
		t.Fatalf("the refusal does not carry %s: %v", ErrPolicyMaxReplicas, detailCodes(err))
	}
	text := errText(err)
	for _, want := range []string{"web", "9", "2"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal must name the component, the value and the limit; %q is missing from: %s", want, text)
		}
	}

	// Two replicas is the limit itself, and a limit is a ceiling rather than a
	// forbidden value.
	if err := deployInline(t, agent, "capped", "  components:\n    - {name: web, replicas: {min: 2}}\n"); hasCode(detailCodes(err), ErrPolicyMaxReplicas) {
		t.Errorf("the ceiling refused the ceiling: %v", err)
	}
}

// TestProtectedComponentsCannotBeRemovedOrScaledToZero: the two ways an agent
// takes a database away.
func TestProtectedComponentsCannotBeRemovedOrScaledToZero(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	// Scaled to zero. The environment's own stored policy is what protects
	// `worker`; the inline document only supplies the scale-down.
	scaledToZero := "  components:\n    - {name: worker, replicas: {min: 0}}\n"
	err := deployInlineWithSpec(t, agent, "capped", scaledToZero, false)
	if !hasCode(detailCodes(err), ErrPolicyProtected) {
		t.Errorf("scaling a protected component to zero was not refused with %s: %v", ErrPolicyProtected, detailCodes(err))
	}

	// Removed: the same environment, an inline project that no longer declares
	// the protected database at all.
	err = deployInlineWithSpec(t, agent, "capped", "", true)
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("dropping a protected component = %v (code %s), want permission-denied", err, authCode(err))
	}
	if !hasCode(detailCodes(err), ErrPolicyProtected) {
		t.Fatalf("the refusal does not carry %s: %v", ErrPolicyProtected, detailCodes(err))
	}
	if !strings.Contains(errText(err), "worker") {
		t.Errorf("the refusal must name the protected component: %s", errText(err))
	}
}

// TestRequireDryRunIsRunByTheServer: `require: [dry-run]` is satisfied by the
// server running one, and a server that cannot run one refuses rather than
// waving the deploy through. There is no request field that could claim a
// dry-run happened, which is the point.
func TestRequireDryRunIsRunByTheServer(t *testing.T) {
	connector, _ := connectorFor(nil)

	// No preview seam: the requirement cannot be satisfied, so the deploy is
	// refused with the rule named.
	g := policyServer(t, Options{Delivery: connector})
	agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))
	err := deployTo(t, agent, "shop", "staging")
	if !hasCode(detailCodes(err), ErrPolicyDryRunRequired) {
		t.Fatalf("a deploy into require: [dry-run] with no dry-run engine was not refused with %s: %v",
			ErrPolicyDryRunRequired, detailCodes(err))
	}
	assertEscalates(t, err)
	// And the refusal is the policy's, not the apply gate's: an unsatisfiable
	// requirement is a statement about this agent that must be reached before
	// the "kelson cannot apply yet" one (#224), or an agent would be told the
	// wrong thing about a rule that still governs it.
	if hasCode(detailCodes(err), string(delivery.ErrNotImplemented)) {
		t.Fatal("the apply gate answered ahead of the policy refusal")
	}

	// A dry-run that reports the change would be rejected is the same refusal:
	// the requirement is a *passing* dry-run.
	blocked := policyServer(t, Options{Delivery: connector, Preview: previewConnector(&fakePreview{diff: blockedDiff()})})
	agent = blocked.as(blocked.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))
	if err := deployTo(t, agent, "shop", "staging"); !hasCode(detailCodes(err), ErrPolicyDryRunRequired) {
		t.Errorf("a blocked dry-run did not refuse the deploy: %v", detailCodes(err))
	}

	// A dry-run that passes lets the deploy through to the delivery plane.
	ok := policyServer(t, Options{Delivery: connector, Preview: previewConnector(&fakePreview{diff: cleanDiff()})})
	agent = ok.as(ok.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))
	if err := deployTo(t, agent, "shop", "staging"); hasCode(detailCodes(err), ErrPolicyDryRunRequired) {
		t.Errorf("a passing dry-run still refused the deploy: %v", err)
	}
}

// --- the routes around the rules -----------------------------------------------

// TestInlineSpecGetsTheStoredPolicy is the "cannot be bypassed by a modified
// client" half of the acceptance criterion, in its sharpest form: the agent
// sends documents that say the environment allows it, and the environment the
// server stores says otherwise. The stored one wins.
func TestInlineSpecGetsTheStoredPolicy(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "wide", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	err := deployInline(t, agent, prodEnv, "  policy:\n    agents: allow\n")
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("an inline spec claiming `agents: allow` = %v (code %s), want permission-denied", err, authCode(err))
	}
	if !hasCode(detailCodes(err), ErrPolicyProposeOnly) {
		t.Errorf("the refusal does not carry %s: %v", ErrPolicyProposeOnly, detailCodes(err))
	}
}

// TestSpecWritesCannotRelaxTheRuleThatBindsThem: the escalation an agent must
// not have. Rewriting production's document to `agents: allow` is itself a
// mutation of production, checked against the policy the store holds now.
func TestSpecWritesCannotRelaxTheRuleThatBindsThem(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "wide", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	err := policyDrivers[model.AgentOpSpecWrite](t, agent, "shop", prodEnv)
	if !hasCode(detailCodes(err), ErrPolicyProposeOnly) {
		t.Fatalf("an agent rewrote a propose-only environment's spec: %v", detailCodes(err))
	}

	// And the store still holds the original: nothing was written.
	res, err := g.as(testPassword).spec.GetSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "shop"}))
	if err != nil {
		t.Fatalf("GetSpec: %v", err)
	}
	if got := string(res.Msg.GetSpec().GetDocuments().GetEnvironments()[prodEnv]); !strings.Contains(got, "propose-only") {
		t.Errorf("the stored production environment no longer says propose-only:\n%s", got)
	}
}

// TestASpecWriteCannotDeleteAnEnvironmentByOmittingIt: PutSpec replaces the
// project's whole document set, so leaving an environment out of the request
// deletes it. A check scoped to the environments the message names would miss
// exactly that — the quietest route to removing a propose-only environment.
func TestASpecWriteCannotDeleteAnEnvironmentByOmittingIt(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "wide", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	project, envs := policySpec()
	delete(envs, prodEnv) // the whole attack: say nothing about production

	_, err := agent.spec.PutSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: &kelsonv1alpha1.SpecDocuments{Project: project, Environments: envs},
		Force:     true,
	}))
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("a spec write omitting a propose-only environment = %v (code %s), want permission-denied", err, authCode(err))
	}
	if !hasCode(detailCodes(err), ErrPolicyProposeOnly) {
		t.Errorf("the refusal does not carry %s: %v", ErrPolicyProposeOnly, detailCodes(err))
	}
}

// TestDryRunIsNeverRefused: a propose-only agent must be able to produce the
// proposal, or `propose-only` would mean "do nothing" rather than "propose".
func TestDryRunIsNeverRefused(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	stream, err := agent.deploy.Deploy(t.Context(), connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec:        specRefFor("shop"),
		Environment: prodEnv,
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the assertion is on the first event
	var manifests int
	for stream.Receive() {
		manifests += len(stream.Msg().GetProposed().GetManifests())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("a propose-only agent could not render its own proposal: %v", err)
	}
	if manifests == 0 {
		t.Error("the proposal carried no manifests, so there is nothing for a human to review")
	}
}

// TestAnUnreadablePolicyFailsClosed: a spec store that errs is not an absent
// policy. The mutation is refused rather than allowed on the strength of a
// question kelson could not ask.
func TestAnUnreadablePolicyFailsClosed(t *testing.T) {
	g := newGatedServer(t, Options{Specs: brokenSpecStore{}})
	agent := g.as(g.mint(t, "deploybot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	// An inline spec, so the render needs no store read and the only question
	// the store is asked is the policy one.
	err := deployInline(t, agent, prodEnv, "")
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("a mutation with an unreadable policy = %v (code %s), want permission-denied", err, authCode(err))
	}
	if !hasCode(detailCodes(err), ErrPolicyUnreadable) {
		t.Errorf("the refusal does not carry %s: %v", ErrPolicyUnreadable, detailCodes(err))
	}
}

// TestAMutationMustNameItsEnvironment: policy is per environment, so a mutation
// that cannot say which environment it changes cannot be checked against one
// and is refused rather than assumed into the nearest match.
//
// Two layers, and the outer one answers first today: a secret addressed to a
// bare namespace is refused by internal/secret's own target rule before the
// guard sees it, which is why the HTTP half of this test asserts only that the
// route is closed. The guard's own refusal is asserted directly, because it is
// the layer that has to hold when a future RPC addresses something less
// strictly than SecretTarget does.
func TestAMutationMustNameItsEnvironment(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "wide", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	_, err := agent.secrets.SetSecret(t.Context(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: &kelsonv1alpha1.SecretTarget{Namespace: "shop-production"},
		Name:   "api-credentials",
		Values: map[string]string{"token": "s3cret"},
	}))
	if err == nil {
		t.Fatal("a namespace-addressed secret write by an agent was accepted; it names no environment, so no policy could have been applied to it")
	}

	// The guard itself, on an agent-strict (anonymous) context.
	server := New(Options{Specs: newFakeSpecStore()})
	_, err = server.guard(t.Context(), model.AgentOpSecretSet, "shop", "")
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("guarding a mutation with no environment = %v (code %s), want permission-denied", err, authCode(err))
	}
	if !hasCode(detailCodes(err), ErrPolicyUnaddressed) {
		t.Errorf("the refusal does not carry %s: %v", ErrPolicyUnaddressed, detailCodes(err))
	}

	// And a human is still not subject to it.
	if _, err := server.guard(WithPrincipal(t.Context(), Principal{Type: PrincipalHuman}),
		model.AgentOpSecretSet, "shop", ""); err != nil {
		t.Errorf("a human was refused by the guard: %v", err)
	}
}

// TestAnonymousGetsAgentStrictness: on a server with no gate kelson cannot tell
// a person from an agent, so it applies the stricter reading rather than the
// one that would make the guardrail evaporate when authentication is off.
func TestAnonymousGetsAgentStrictness(t *testing.T) {
	store := newFakeSpecStore()
	project, envs := policySpec()
	if _, err := store.Put(t.Context(), "shop",
		controlstore.Documents{Project: project, Environments: envs}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing: %v", err)
	}
	c := serve(t, Options{Specs: store})

	if err := deployTo(t, c, "shop", prodEnv); !hasCode(detailCodes(err), ErrPolicyProposeOnly) {
		t.Errorf("an anonymous caller was not held to the environment's agent policy: %v", detailCodes(err))
	}
}

// --- helpers -------------------------------------------------------------------

// deployInline deploys documents the caller composes, so a test can send a spec
// that disagrees with the stored one.
func deployInline(t *testing.T, c clients, environment, envBody string) error {
	t.Helper()
	return deployInlineWithSpec(t, c, environment, envBody, false)
}

// deployInlineWithSpec is deployInline with control over the project document:
// dropProtected removes the protected component, which is how the removal half
// of `protect:` is exercised.
func deployInlineWithSpec(t *testing.T, c clients, environment, envBody string, dropProtected bool) error {
	t.Helper()
	project := []byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - {name: web, port: 8080}
    - {name: worker}
`)
	if dropProtected {
		project = []byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - {name: web, port: 8080}
`)
	}
	env := []byte(fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: %s}
spec:
  project: shop
%s`, environment, envBody))

	stream, err := c.deploy.Deploy(t.Context(), connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec: &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Documents{
			Documents: &kelsonv1alpha1.SpecDocuments{
				Project:      project,
				Environments: map[string][]byte{environment: env},
			},
		}},
		Environment: environment,
	}))
	if err != nil {
		return err
	}
	defer stream.Close() //nolint:errcheck // the test only wants the first error
	for stream.Receive() {
	}
	return stream.Err()
}

// errText is the whole refusal as one string: the ConnectRPC message plus every
// structured detail, which is where the rule, the value and the limit live.
func errText(err error) string {
	if err == nil {
		return ""
	}
	var out strings.Builder
	out.WriteString(err.Error())
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return out.String()
	}
	for _, detail := range cerr.Details() {
		value, derr := detail.Value()
		if derr != nil {
			continue
		}
		if wire, ok := value.(*kelsonv1alpha1.Error); ok {
			fmt.Fprintf(&out, " | %s %s %s %s", wire.GetCode(), wire.GetField(), wire.GetMessage(), wire.GetRemediation())
		}
	}
	return out.String()
}

// assertEscalates holds every refusal to ADR-0025 §6: the error IS the
// escalation path, so it has to say what a human must do.
func assertEscalates(t *testing.T, err error) {
	t.Helper()
	text := errText(err)
	if !strings.Contains(text, "escalate:") {
		t.Errorf("a policy refusal must carry the escalation path: %s", text)
	}
	if !strings.Contains(text, "spec.policy") {
		t.Errorf("a policy refusal must name the spec field a human would change: %s", text)
	}
}

// blockedDiff is a server-side dry-run verdict that says the change would be
// rejected — an enforce-mode admission policy, the same finding `kelson diff`
// exits 3 on.
func blockedDiff() *diff.Diff {
	return &diff.Diff{
		Level:      diff.LevelServer,
		Resources:  []diff.ResourceDiff{{Kind: "Deployment", Name: "web", Op: diff.OpModified}},
		Violations: []diff.PolicyViolation{{Engine: "kyverno", Policy: "require-limits", Enforcement: diff.EnforcementEnforce}},
		Summary:    diff.Summary{Modified: 1, MaxRisk: diff.RiskDisruptive},
	}
}

// cleanDiff is a dry-run that found nothing to object to.
func cleanDiff() *diff.Diff {
	return &diff.Diff{
		Level:     diff.LevelServer,
		Resources: []diff.ResourceDiff{{Kind: "Deployment", Name: "web", Op: diff.OpModified}},
		Summary:   diff.Summary{Modified: 1, MaxRisk: diff.RiskAdditive},
	}
}

// brokenSpecStore is a spec store whose reads fail for a reason that is not
// "no such spec", which is the case that must fail closed.
type brokenSpecStore struct{}

func (brokenSpecStore) Put(context.Context, string, controlstore.Documents, controlstore.PutOptions) (controlstore.Stored, error) {
	return controlstore.Stored{}, context.DeadlineExceeded
}
func (brokenSpecStore) Get(context.Context, string) (controlstore.Stored, error) {
	return controlstore.Stored{}, context.DeadlineExceeded
}
func (brokenSpecStore) List(context.Context) ([]controlstore.Stored, error) {
	return nil, context.DeadlineExceeded
}
func (brokenSpecStore) Delete(context.Context, string, controlstore.DeleteOptions) error {
	return context.DeadlineExceeded
}
