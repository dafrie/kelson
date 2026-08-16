package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
)

// Per-environment agent policy (issue #75, ADR-0025).
//
// # What this layer is, next to #74's
//
// The scope check of ADR-0024 answers "may this *credential* reach this
// (project, environment, operation class)". This one answers "may *any* agent
// do this to this environment", and it is read from the environment's own spec
// rather than from the credential. The two compose in that order: scope first
// in the interceptor, policy second, here. A credential that passes the scope
// check and then meets `agents: propose-only` is refused, and a credential that
// fails the scope check never gets far enough to be told about policy.
//
// # Why the check is in the handlers and not in the interceptor
//
// Policy lives on the stored Environment, and three of the five rules need
// something only the handler has: the *resolved* spec (maxReplicas, protect),
// the render that L1 already performed (require: dry-run), and — for a request
// carrying inline documents — the project name, which is inside the YAML and
// which ADR-0024 deliberately refuses to parse in the interceptor.
//
// The interceptor's own protection against forgetting is a table, and so is
// this one: [agentOperations] names every mutating RPC, a coverage test pins it
// against the scope table's `mutate` rows, and the enforcement test in
// policy_test.go drives every operation in it through the real HTTP stack as a
// propose-only agent. A mutating RPC that lands without a policy check fails
// those tests by name.
//
// # The stored spec decides, never the request
//
// [Server.guard] resolves the policy for (project, environment) out of the
// **stored** spec, whatever the request carried. An agent that presents inline
// documents declaring `policy: {agents: allow}` for an environment the server
// holds as `propose-only` is refused: it named the environment, and naming an
// environment is asking about the one kelson stores. A modified client
// therefore changes nothing — there is no field in any request that policy is
// read from.
//
// # Who it applies to
//
// Agent principals, and anonymous ones. Not humans: this is agent policy, and
// #84's password path is a person. Anonymous gets the agent's strictness
// because on a server with no gate kelson cannot tell one from the other, and a
// guardrail that evaporates when authentication is off is not a guardrail.

// Policy refusal codes. They ride the same wire shape as every other structured
// error (ADR-0013 §2) so an agent branches on the code rather than on prose,
// and each names the rule that refused.
//
// The prefix is `agent-policy/` and not `policy/` on purpose: internal/diff
// already owns `policy/*` for admission-control findings (issue #45), and two
// taxonomies under one prefix would make "the policy refused it" ambiguous
// exactly where an agent is trying to decide what to do next.
const (
	// ErrPolicyProposeOnly is a live mutation in an environment whose policy is
	// `agents: propose-only`.
	ErrPolicyProposeOnly = "agent-policy/propose-only"
	// ErrPolicyForbidden is an operation named in the environment's
	// `policy.forbid`.
	ErrPolicyForbidden = "agent-policy/forbidden-operation"
	// ErrPolicyMaxReplicas is a deploy whose resolved spec exceeds
	// `policy.maxReplicas`.
	ErrPolicyMaxReplicas = "agent-policy/max-replicas"
	// ErrPolicyProtected is a deploy that would remove or scale to zero a
	// component named in `policy.protect`.
	ErrPolicyProtected = "agent-policy/protected-resource"
	// ErrPolicyDryRunRequired is a deploy into an environment that requires a
	// dry-run, where kelson could not run one or where the one it ran said the
	// change would be rejected.
	ErrPolicyDryRunRequired = "agent-policy/dry-run-required"
	// ErrPolicyUnaddressed is a mutation that does not name the project and
	// environment it acts on. Policy is per environment, so a request that
	// cannot say which environment it changes cannot be checked against one.
	ErrPolicyUnaddressed = "agent-policy/unaddressed"
	// ErrPolicyUnreadable is a stored spec the server could not read or decode
	// while deciding. It fails closed: an unreadable policy is not an absent
	// one.
	ErrPolicyUnreadable = "agent-policy/unreadable"
)

const policyDocsBase = "https://kelson.dev/server/policy"

// policyError is one policy refusal. Field carries the spec path of the rule
// that refused — `$.spec.policy.maxReplicas` — so the error names the rule, the
// value and the limit rather than only saying no.
type policyError struct {
	Code        string
	Resource    string
	Field       string
	Message     string
	Remediation string
}

func (e policyError) Error() string {
	return fmt.Sprintf("%s [%s] %s: %s", e.Resource, e.Code, e.Message, e.Remediation)
}

func (e policyError) wire() *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        e.Code,
		Resource:    e.Resource,
		Field:       e.Field,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     policyDocsBase + "/" + e.Code,
	}
}

// refused is what every policy failure becomes: permission-denied, the same
// code the scope refusals of #74 use. One code for "you may not" keeps an
// agent's branch simple — retry never helps, a human or a spec change does.
func refused(e policyError) error { return fail(connect.CodePermissionDenied, e) }

// agentOperations is the policy gate table: every mutating RPC and the
// operation name `policy.forbid` knows it by.
//
// It is exhaustive over the `mutate` rows of the scope table
// (TestEveryMutatingMethodHasAPolicyOperation), which is what makes a new
// mutating RPC a test failure rather than an unguarded route.
var agentOperations = map[string]model.AgentOperation{
	kelsonv1alpha1connect.DeployServiceDeployProcedure:   model.AgentOpDeploy,
	kelsonv1alpha1connect.DeployServiceRollbackProcedure: model.AgentOpRollback,
	kelsonv1alpha1connect.DeployServicePromoteProcedure:  model.AgentOpPromote,
	kelsonv1alpha1connect.BuildServiceBuildProcedure:     model.AgentOpBuild,
	// ReportBuild is filed under `deploy` and not under `build`, which is the
	// one place this table maps two RPCs to one word (ADR-0034 decision 3).
	//
	// The name would suggest `build`, and `build` is precisely wrong: it is the
	// operation propose-only exempts, because a build changes no environment.
	// A report does. It records images for a commit and triggers the
	// server-side render→publish for the previews and `autoDeploy` environments
	// that commit feeds — a deploy that CI starts rather than a human. Filing it
	// under `build` would let a propose-only environment be deployed into by a
	// pipeline, which is the exact hole this gate table exists to close, so it
	// is filed under the word that describes its effect.
	//
	// Whether "CI may report, but an agent may not deploy by hand" deserves a
	// word of its own is a model-plane decision (model.AgentOperations is the
	// vocabulary of record) and belongs to whoever builds the pipeline. Until
	// then the coarse mapping fails closed in both directions: `forbid:
	// [deploy]` stops the report, and so does propose-only.
	kelsonv1alpha1connect.BuildServiceReportBuildProcedure:   model.AgentOpDeploy,
	kelsonv1alpha1connect.SecretServiceSetSecretProcedure:    model.AgentOpSecretSet,
	kelsonv1alpha1connect.SecretServiceDeleteSecretProcedure: model.AgentOpSecretDelete,
	// UnsetSecret is the third RPC filed under a word another one owns (#269),
	// and it is the easiest of the three: removing a key and removing the
	// Secret that holds it are the same act at different granularity, and
	// `forbid: [secret-delete]` is how an operator says agents may not take
	// credentials away here. A separate word would be a second thing to
	// remember to forbid, and forgetting it would be silent.
	kelsonv1alpha1connect.SecretServiceUnsetSecretProcedure: model.AgentOpSecretDelete,
	kelsonv1alpha1connect.SpecServicePutSpecProcedure:       model.AgentOpSpecWrite,
	kelsonv1alpha1connect.SpecServiceDeleteSpecProcedure:    model.AgentOpSpecDelete,
	// ProposeSpec is the second entry mapping onto a word another RPC already
	// owns, and the argument runs the opposite way to ReportBuild's (#248,
	// ADR-0033 decision 3).
	//
	// A proposal changes no environment. It writes nothing to kelson's store,
	// applies nothing to a cluster, and its whole effect is a branch and a pull
	// request in the user's repository that a human must read and merge. By
	// effect alone it is closer to `build` — an artifact somewhere else that
	// changes nothing until somebody acts on it — than to `spec-write`.
	//
	// It is filed under `spec-write` anyway, and the reason is `forbid`. The
	// subject of a proposal is the authored document: it is the same bytes
	// PutSpec would have stored, offered by a different route. An operator who
	// writes `forbid: [spec-write]` has said agents may not change this
	// project's documents, and a route that let them do it by pull request
	// instead would make that rule advisory — the exact hole this gate table
	// exists to close, so the coarse mapping fails closed.
	//
	// `propose-only` is the half that must *not* refuse it, and
	// [Server.guardProposal] is where that is spelled out.
	kelsonv1alpha1connect.SpecServiceProposeSpecProcedure: model.AgentOpSpecWrite,
}

// proposeOnlyExempt lists the operations `agents: propose-only` does *not*
// refuse. Building an image changes no environment: it produces an artifact in
// a registry, and the deploy that would put it in front of users is refused by
// the same policy a line above. An operator who wants the build stopped as well
// writes `forbid: [build]`, which is the rule that says so out loud.
func proposeOnlyExempt(op model.AgentOperation) bool {
	return op == model.AgentOpBuild
}

// persists reports whether a dry-run rung actually changes anything, which is
// what decides whether policy has anything to refuse.
//
// RENDER and SERVER both leave the world as they found it, and they are exactly
// what a propose-only agent is told to send instead of an apply — refusing them
// would refuse the proposal along with the change. Rollback is the one handler
// that does not use this helper: its SERVER rung applies (deploy.go), so there
// only RENDER is exempt, and reading that difference off one shared predicate
// would be reading it wrong.
func persists(d kelsonv1alpha1.DryRun) bool {
	return d != kelsonv1alpha1.DryRun_DRY_RUN_RENDER && d != kelsonv1alpha1.DryRun_DRY_RUN_SERVER
}

// agentGuard is one policy decision in progress: the environment's policy, and
// whether it applies to this caller at all. A guard for a human principal is
// inert, and every method on it is a no-op — so a handler that guards
// unconditionally does not have to know who is calling.
type agentGuard struct {
	active      bool
	policy      model.Policy
	project     string
	environment string
	// proposal marks a guard for an operation that proposes a change rather
	// than applying one, which `propose-only` may not refuse
	// ([Server.guardProposal]).
	proposal bool
}

// resource is how a policy error names what it refused.
func (g agentGuard) resource() string {
	return "environment/" + g.project + "/" + g.environment
}

// escalation is the escalation path every refusal carries (ADR-0025 §6). There
// is no approval queue in v0: the error *is* the pointer, and it names both
// ways out — a human doing it, or the rule changing.
func (g agentGuard) escalation(what string) string {
	return fmt.Sprintf("escalate: ask a human to %s (`kelson %s` against %s/%s with their own credential), "+
		"or have one relax the rule on the stored Environment (spec.policy on %s/%s) and store it with `kelson apply`. "+
		"kelson has no approval queue: this refusal is the escalation path",
		what, what, g.project, g.environment, g.project, g.environment)
}

// policyApplies reports whether this principal is subject to agent policy.
func policyApplies(p Principal) bool {
	return p.Type != PrincipalHuman
}

// guard resolves the policy for one (project, environment) out of the stored
// spec and applies the two rules that need nothing else: `propose-only` and
// `forbid`. The returned guard carries the policy for the checks a handler runs
// later, once it has rendered.
//
// project and environment are the *resolved* names — what the request actually
// acts on, after an inline document has been decoded — so an inline spec is
// checked against the stored environment it names.
func (s *Server) guard(ctx context.Context, op model.AgentOperation, project, environment string) (agentGuard, error) {
	g := agentGuard{project: project, environment: environment}
	if !policyApplies(principalOf(ctx)) {
		return g, nil
	}
	g.active = true

	if project == "" || environment == "" {
		return g, refused(policyError{
			Code:     ErrPolicyUnaddressed,
			Resource: "operation/" + string(op),
			Message: fmt.Sprintf("%s does not name the project and environment it acts on, "+
				"and agent policy is a property of the environment", op),
			Remediation: "address the request to a stored project and name the environment " +
				"(`spec: {project: <name>}` and `environment: <name>`), or run it as a human principal",
		})
	}

	policy, err := s.storedPolicy(ctx, project, environment)
	if err != nil {
		return g, err
	}
	g.policy = policy

	if policy.Forbids(op) {
		return g, refused(policyError{
			Code:     ErrPolicyForbidden,
			Resource: g.resource(),
			Field:    "$.spec.policy.forbid",
			Message: fmt.Sprintf("%s/%s forbids the %q operation for agents (forbid: [%s])",
				project, environment, op, joinOperations(policy.Forbid)),
			Remediation: g.escalation(string(op)),
		})
	}
	if !policy.AllowsUnsupervised() && !proposeOnlyExempt(op) {
		return g, refused(policyError{
			Code:     ErrPolicyProposeOnly,
			Resource: g.resource(),
			Field:    "$.spec.policy.agents",
			Message: fmt.Sprintf("%s/%s is propose-only for agents: %s changes live state and no agent may do that here unsupervised",
				project, environment, op),
			Remediation: "propose instead of applying: re-send this request with dry_run=RENDER for the manifests, " +
				"or call RenderService.Diff, and hand a human the result to review. " + g.escalation(string(op)),
		})
	}
	return g, nil
}

// blastRadius applies the two rules that need the resolved spec: the replica
// ceiling and the protected components. It runs on what the request *would*
// deploy, which is the only honest place to ask — a limit checked against the
// authored document rather than the resolved one would miss every override.
func (g agentGuard) blastRadius(resolved *model.Resolved) error {
	if !g.active || resolved == nil {
		return nil
	}
	if limit := g.policy.MaxReplicas; limit != nil {
		for _, c := range resolved.Components {
			count := c.Replicas.Min
			if c.Replicas.Max > count {
				count = c.Replicas.Max
			}
			if count <= *limit {
				continue
			}
			return refused(policyError{
				Code:     ErrPolicyMaxReplicas,
				Resource: g.resource(),
				Field:    "$.spec.policy.maxReplicas",
				Message: fmt.Sprintf("component %q would run %d replicas in %s/%s, and policy caps an agent at %d",
					c.Name, count, g.project, g.environment, *limit),
				Remediation: fmt.Sprintf("lower replicas for %q to %d or fewer, or ask a human to deploy the larger count. %s",
					c.Name, *limit, g.escalation("deploy")),
			})
		}
	}
	for _, name := range g.policy.Protect {
		declared, replicas := findResolved(resolved, name)
		switch {
		case !declared:
			return refused(policyError{
				Code:     ErrPolicyProtected,
				Resource: g.resource(),
				Field:    "$.spec.policy.protect",
				Message: fmt.Sprintf("component %q is protected in %s/%s and this spec no longer declares it, "+
					"so applying it would delete the component", name, g.project, g.environment),
				Remediation: fmt.Sprintf("keep %q in the spec, or ask a human to remove it deliberately. %s",
					name, g.escalation("deploy")),
			})
		case replicas != nil && *replicas == 0:
			return refused(policyError{
				Code:     ErrPolicyProtected,
				Resource: g.resource(),
				Field:    "$.spec.policy.protect",
				Message: fmt.Sprintf("component %q is protected in %s/%s and this spec scales it to zero replicas",
					name, g.project, g.environment),
				Remediation: fmt.Sprintf("leave %q running, or ask a human to scale it down deliberately. %s",
					name, g.escalation("deploy")),
			})
		}
	}
	return nil
}

// findResolved locates a component by name in a resolved spec and reports its
// effective replica floor. The bool is whether the spec declares it at all; the
// pointer is nil for a component that has no replica count of its own — a data
// service's topology is its preset, and a chart's is the chart's business.
func findResolved(r *model.Resolved, name string) (bool, *int) {
	for _, c := range r.Components {
		if c.Name == name {
			count := c.Replicas.Min
			return true, &count
		}
	}
	for _, d := range r.DataServices {
		if d.Name == name {
			return true, nil
		}
	}
	for _, c := range r.Charts {
		if c.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// requireDryRun is `require: [dry-run]`: kelson runs its own dry-run in this
// request and refuses the deploy if it cannot, or if the dry-run says the
// change would be rejected.
//
// The server runs it rather than believing a claim that one was run. A flag on
// the request saying "I dry-ran this" would be exactly the client-side
// enforcement this issue exists to rule out, so there is no such flag: L1 is
// the render that already succeeded to get here, and L2 is the same preview
// engine `--dry-run=server` uses, called on the same rendered set that is about
// to be applied.
func (s *Server) requireDryRun(ctx context.Context, g agentGuard, profile clusterprofile.ClusterProfile, set delivery.ManifestSet) error {
	if !g.active || !g.policy.RequiresDryRun() {
		return nil
	}
	unsatisfied := func(why, fix string) error {
		return refused(policyError{
			Code:     ErrPolicyDryRunRequired,
			Resource: g.resource(),
			Field:    "$.spec.policy.require",
			Message: fmt.Sprintf("%s/%s requires a passing server-side dry-run before an agent deploy applies, and %s",
				g.project, g.environment, why),
			Remediation: fix + " " + g.escalation("deploy"),
		})
	}
	if s.preview == nil {
		return unsatisfied("this server has no server-side dry-run engine wired",
			"start kelson-server with a cluster connection so it can dry-run, or drop `require: [dry-run]` from the environment.")
	}
	engine, err := s.preview(ctx, profile)
	if err != nil {
		return unsatisfied("the dry-run engine could not be built: "+err.Error(),
			"fix the server's cluster connection and retry.")
	}
	d, err := engine.Preview(ctx, set)
	if err != nil {
		return unsatisfied("the dry-run failed: "+err.Error(), "retry once the cluster answers.")
	}
	if diffExitCode(d) == exitSemanticsBlocked {
		return unsatisfied("it reported the change would be rejected: "+previewSummary(d),
			"fix what the dry-run objects to — `kelson diff --dry-run=server` reports the same finding — and retry.")
	}
	return nil
}

// storedPolicy reads one environment's effective policy out of the spec store.
//
// Three answers, and the difference between them is the whole design:
// a stored environment's policy applies; an environment or project the store
// does not hold has none, which is `allow`; and a store that *errs* is a
// refusal, because an unreadable policy is not an absent one.
func (s *Server) storedPolicy(ctx context.Context, project, environment string) (model.Policy, error) {
	policies, err := s.storedPolicies(ctx, project)
	if err != nil {
		return model.Policy{}, err
	}
	return policies[environment], nil
}

// storedPolicies reads every environment's effective policy for one project. A
// server with no spec store holds no specs and therefore no policy — that is a
// partially wired build, not a bypass: cmd/kelson-server always wires it.
func (s *Server) storedPolicies(ctx context.Context, project string) (map[string]model.Policy, error) {
	if s.specs == nil {
		return nil, nil
	}
	stored, err := s.specs.Get(ctx, project)
	if err != nil {
		if controlstore.AsNotFound(err) {
			return nil, nil
		}
		return nil, refused(policyError{
			Code:     ErrPolicyUnreadable,
			Resource: "spec/" + project,
			Message: fmt.Sprintf("the stored spec for project %q could not be read, so its agent policy is unknown: %v",
				project, err),
			Remediation: "retry once the state store answers; kelson refuses an agent mutation it cannot check rather than assuming there is no policy",
		})
	}
	spec, err := decodeSpec(stored.Documents.Project, stored.Documents.Environments)
	if err != nil {
		return nil, refused(policyError{
			Code:        ErrPolicyUnreadable,
			Resource:    "spec/" + project,
			Message:     fmt.Sprintf("the stored spec for project %q does not decode, so its agent policy is unknown: %v", project, err),
			Remediation: "have a human repair the stored spec (`kelson spec get` then `kelson apply`); an agent mutation is refused until its policy can be read",
		})
	}
	out := make(map[string]model.Policy, len(spec.environments))
	for _, env := range spec.environments {
		out[env.Metadata.Name] = model.EffectivePolicy(spec.project, env)
	}
	return out, nil
}

// guardStored applies `propose-only` and `forbid` to **every** stored
// environment of a project at once, which is what the two spec-store mutations
// need.
//
// Every stored environment, not just the ones the request mentions, and the
// reason is that both writes reach all of them. PutSpec replaces the project's
// whole document set: an environment named in the request is overwritten, an
// environment *missing* from it is deleted, and the Project document every
// environment resolves against is rewritten either way. DeleteSpec removes the
// lot. So a stored environment is affected by both operations whether or not
// its name appears in the message, and scoping this check to the names the
// request happens to carry would let an agent delete a propose-only environment
// by omitting it.
//
// The consequence is deliberate and worth stating: an agent cannot store a spec
// for a project that has any propose-only environment, even to change a
// different one. A spec write is a whole-project write, and pretending it is
// narrower would be pretending about the one operation that can rewrite the
// policy itself.
func (s *Server) guardStored(ctx context.Context, op model.AgentOperation, project string) error {
	return s.guardEveryStored(ctx, op, project, false)
}

// guardProposal is [Server.guardStored] for an RPC that *proposes* a spec
// change to the repository instead of storing one (ProposeSpec, #248).
//
// # `forbid` applies and `propose-only` does not, and the asymmetry is the point
//
// `forbid: [spec-write]` applies unchanged: the operator said agents may not
// change this project's documents, and a pull request changes them by another
// route (see the note beside the ProposeSpec row in [agentOperations]).
//
// `agents: propose-only` must not refuse it, and this is not a convenience. The
// policy's own sentence is "no live mutation without a human", and its
// remediation everywhere else in this file is "propose instead of applying" —
// today that means dry_run=RENDER and a diff somebody pastes into a review by
// hand. ADR-0033's consequences name the gap outright: "`propose-only` agents
// can finally open the pull request they propose." Refusing this call under
// propose-only would refuse the escalation path the same policy tells the agent
// to take, which is the failure `proposeOnlyExempt` was invented to prevent for
// `build` — an operation the policy has no reason to stop, blocked because it
// shares a word with one it does.
//
// It is a separate entry point rather than another member of `proposeOnlyExempt`
// because the exemption there is keyed by *operation* and this operation is
// `spec-write`: exempting the word would exempt PutSpec, which is the one call
// propose-only exists to stop. The distinction is which RPC is running, so the
// handler is what states it.
func (s *Server) guardProposal(ctx context.Context, op model.AgentOperation, project string) error {
	return s.guardEveryStored(ctx, op, project, true)
}

func (s *Server) guardEveryStored(ctx context.Context, op model.AgentOperation, project string, proposal bool) error {
	if !policyApplies(principalOf(ctx)) {
		return nil
	}
	policies, err := s.storedPolicies(ctx, project)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	// Sorted, so a spec that trips two environments' policies always names the
	// same one first: a refusal that varied with Go's map order would be a
	// refusal an agent could not learn from.
	slices.Sort(names)
	for _, name := range names {
		g := agentGuard{active: true, policy: policies[name], project: project, environment: name, proposal: proposal}
		if err := g.refuseWrite(op); err != nil {
			return err
		}
	}
	return nil
}

// refuseWrite is guard's `forbid`/`propose-only` pair for an environment whose
// policy is already in hand. A guard marked as a proposal runs the first rule
// and skips the second — [Server.guardProposal] argues why.
func (g agentGuard) refuseWrite(op model.AgentOperation) error {
	if g.policy.Forbids(op) {
		return refused(policyError{
			Code:     ErrPolicyForbidden,
			Resource: g.resource(),
			Field:    "$.spec.policy.forbid",
			Message: fmt.Sprintf("%s/%s forbids the %q operation for agents (forbid: [%s])",
				g.project, g.environment, op, joinOperations(g.policy.Forbid)),
			Remediation: g.escalation(string(op)),
		})
	}
	if !g.policy.AllowsUnsupervised() && !g.proposal {
		return refused(policyError{
			Code:     ErrPolicyProposeOnly,
			Resource: g.resource(),
			Field:    "$.spec.policy.agents",
			Message: fmt.Sprintf("%s/%s is propose-only for agents, and %s rewrites the desired state of that environment "+
				"— including the policy that says so", g.project, g.environment, op),
			Remediation: "send the change with dry_run=RENDER to have kelson validate and render it without storing it, " +
				"and hand a human the result. " + g.escalation(string(op)),
		})
	}
	return nil
}

func joinOperations(ops []model.AgentOperation) string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, string(op))
	}
	return strings.Join(out, ", ")
}
