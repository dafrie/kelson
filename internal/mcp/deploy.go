package mcp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

const deployDescription = `Deploy a stored project's environment, or preview what deploying it would do.

MUTATES THE CLUSTER when dry_run="none": kelson writes the environment's spec to the control plane and kelson-controller takes it from there — publishing a new artifact, pointing Flux at it, and applying it. This tool streams that until it settles or the timeout expires.

dry_run:
  "render" (default) — validate and render only; returns what would be applied and the kinds and sizes of the manifests. Writes nothing.
  "server"           — ask the Kubernetes API server itself (server-side dry-run, including admission and policy). Writes nothing; needs a reachable cluster.
  "none"             — writes the spec and deploys it for real.

A real deploy answers with a settled outcome — SETTLED healthy, SETTLED degraded, or the stream ending with no settled event because the timeout expired first. A deploy that settles unhealthy is a real outcome, not a failed call: read it and act on it, do not retry blindly.

Preconditions: the project must be stored (put_spec) and declare the environment. Supply image when the spec builds from source and you want a specific tag.

Cost: one server call, held open by dry_run="none" until the deployment settles or the request's own budget runs out.

Pass reason to say why you are deploying. It is recorded in kelson's audit trail beside the action, which is what makes the attempt reviewable afterwards by someone who was not here.`

type deployInput struct {
	Project        string `json:"project" jsonschema:"the stored project name"`
	Environment    string `json:"environment" jsonschema:"the environment to deploy, e.g. production"`
	Image          string `json:"image,omitempty" jsonschema:"image reference to deploy; resolves a spec that builds from source"`
	DryRun         string `json:"dry_run,omitempty" jsonschema:"one of render (default, previews and applies nothing), server (Kubernetes server-side dry-run), none (actually deploy)"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"reuse the key from a previous attempt so a retry is the same deployment, not a second one"`
	Reason         string `json:"reason,omitempty" jsonschema:"why you are deploying, in one sentence; recorded in the audit trail beside the action"`
}

func deployTool(c *clients) tool {
	def := mutatingTool("deploy", "Deploy an environment", deployDescription, true)
	return tool{
		def:  def,
		rpcs: []rpc{rpcDeploy},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in deployInput) (*mcpsdk.CallToolResult, any, error) {
				return c.deployEnvironment(ctx, in)
			})
		},
	}
}

// deployEnvironment consumes the Deploy stream to its end and answers with the
// outcome.
//
// The stream is the API's shape and not a tool's: an agent cannot subscribe to
// one, and the transitions in the middle are only worth the context they cost
// as a trail behind the settled answer. So the events are collected, the
// terminal one is the headline, and the phases are the engine's own words —
// this tool decides nothing about what "stuck" or "Rejected" means (issue #37).
func (c *clients) deployEnvironment(ctx context.Context, in deployInput) (*mcpsdk.CallToolResult, any, error) {
	dryRun, err := parseDryRun(in.DryRun)
	if err != nil {
		return nil, nil, err
	}
	key := in.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}

	stream, err := c.deploy.Deploy(ctx, reasoned(connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec:           specRef(in.Project),
		Environment:    in.Environment,
		Image:          in.Image,
		DryRun:         dryRun,
		IdempotencyKey: key,
	}), in.Reason))
	if err != nil {
		return nil, nil, c.fail(rpcDeploy, err)
	}
	defer func() { _ = stream.Close() }()

	var (
		proposed    *kelsonv1alpha1.DeployResponse_Proposed
		committed   *kelsonv1alpha1.DeployResponse_Committed
		transitions []*kelsonv1alpha1.DeployResponse_Transition
		settled     *kelsonv1alpha1.DeployResponse_Settled
	)
	for stream.Receive() {
		switch event := stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.DeployResponse_Proposed_:
			proposed = event.Proposed
		case *kelsonv1alpha1.DeployResponse_Committed_:
			committed = event.Committed
		case *kelsonv1alpha1.DeployResponse_Transition_:
			transitions = append(transitions, event.Transition)
		case *kelsonv1alpha1.DeployResponse_Settled_:
			settled = event.Settled
		}
	}
	if err := stream.Err(); err != nil {
		return nil, nil, c.fail(rpcDeploy, err)
	}

	var r report
	r.addf("%s", deployHeadline(in, dryRun, settled))
	r.addf("idempotency_key: %s (pass it back to retry this exact deployment)", key)

	if proposed != nil {
		r.section("PROPOSED")
		r.addf("  mode       %s", orDash(proposed.GetMode()))
		r.addf("  resources  %d", proposed.GetResources())
		writeManifests(&r, proposed.GetManifests())
	}
	if committed != nil {
		r.section("COMMITTED")
		r.addf("  revision   %s", orDash(committed.GetRevision()))
		r.addf("  adapter    %s", orDash(committed.GetAdapter()))
	}
	writeTransitions(&r, transitions)
	if settled != nil {
		writeSettled(&r, settled)
	}
	if settled == nil && dryRun == kelsonv1alpha1.DryRun_DRY_RUN_NONE {
		r.section("OUTCOME")
		r.addf("  the stream ended without a settled event; the deployment's outcome is unknown. " +
			"Call diagnose_component to read the environment's current state.")
	}
	return text(&r)
}

// deployHeadline states first what an agent must not miss: whether anything was
// applied, and how it ended.
func deployHeadline(in deployInput, dryRun kelsonv1alpha1.DryRun, settled *kelsonv1alpha1.DeployResponse_Settled) string {
	switch dryRun {
	case kelsonv1alpha1.DryRun_DRY_RUN_RENDER:
		return fmt.Sprintf("deploy %s/%s dry_run=render: PREVIEW ONLY, nothing was applied.", in.Project, in.Environment)
	case kelsonv1alpha1.DryRun_DRY_RUN_SERVER:
		return fmt.Sprintf("deploy %s/%s dry_run=server: PREVIEW ONLY, nothing was applied; the verdict below is the Kubernetes API server's own.",
			in.Project, in.Environment)
	default:
	}
	if settled == nil {
		return fmt.Sprintf("deploy %s/%s: APPLIED, outcome unknown.", in.Project, in.Environment)
	}
	final := settled.GetFinal()
	headline := fmt.Sprintf("deploy %s/%s: SETTLED %s (%s)", in.Project, in.Environment, final.GetPhase(), final.GetAnswer())
	if final.GetStuck() {
		headline += " — the deployment did not reach a healthy phase within the server's timeout"
	}
	if settled.GetError() != nil {
		headline += " — it settled unhealthy; see ERROR below"
	}
	return headline
}

// writeManifests lists what would be applied, by identity and size. The YAML
// itself is never returned: a rendered set is thousands of lines and an agent
// that needs them has RenderService (ADR-0008 §2).
func writeManifests(r *report, manifests []*kelsonv1alpha1.Manifest) {
	if len(manifests) == 0 {
		return
	}
	shown, dropped := limit(manifests, maxManifests)
	r.section(fmt.Sprintf("MANIFESTS (%d, bodies omitted)", len(manifests)))
	for _, m := range shown {
		name := m.GetKind() + "/" + m.GetName()
		if ns := m.GetNamespace(); ns != "" {
			name = m.GetKind() + "/" + ns + "/" + m.GetName()
		}
		r.addf("  %s %s", pad(name, 48), bytesize(len(m.GetYaml())))
	}
	r.truncated(dropped, "manifests")
}

func writeTransitions(r *report, transitions []*kelsonv1alpha1.DeployResponse_Transition) {
	if len(transitions) == 0 {
		return
	}
	shown, dropped := limit(transitions, maxEvents)
	r.section(fmt.Sprintf("TRANSITIONS (%d)", len(transitions)))
	for _, t := range shown {
		line := fmt.Sprintf("  %s %s", pad(t.GetPhase(), 14), pad(t.GetAnswer(), 14))
		if t.GetStuck() {
			line += "stuck "
		}
		if cause := t.GetCause(); cause != nil {
			line += fmt.Sprintf("%s/%s: %s", cause.GetComponent(), cause.GetReason(), cause.GetMessage())
		}
		r.addf("%s", line)
	}
	r.truncated(dropped, "transitions")
}

func writeSettled(r *report, settled *kelsonv1alpha1.DeployResponse_Settled) {
	final := settled.GetFinal()
	r.section("SETTLED")
	r.addf("  phase      %s", final.GetPhase())
	r.addf("  answer     %s", final.GetAnswer())
	r.addf("  stuck      %t", final.GetStuck())
	r.addf("  revision   %s", orDash(final.GetObservedRevision()))
	r.addf("  since      %s", stamp(final.GetSinceUnixMs()))
	if cause := final.GetCause(); cause != nil {
		r.addf("  cause      %s/%s: %s", cause.GetComponent(), cause.GetReason(), cause.GetMessage())
	}
	if wire := settled.GetError(); wire != nil {
		r.section("ERROR")
		r.wireError("  ", wire)
	}
}

// parseDryRun maps the tool's vocabulary onto the schema's dry-run ladder. The
// default is the safe rung: a tool whose default mutates is a tool an agent
// will eventually fire by accident (ADR-0008 §3, epic #8).
func parseDryRun(value string) (kelsonv1alpha1.DryRun, error) {
	switch value {
	case "", "render":
		return kelsonv1alpha1.DryRun_DRY_RUN_RENDER, nil
	case "server":
		return kelsonv1alpha1.DryRun_DRY_RUN_SERVER, nil
	case "none":
		return kelsonv1alpha1.DryRun_DRY_RUN_NONE, nil
	default:
		return kelsonv1alpha1.DryRun_DRY_RUN_UNSPECIFIED,
			fmt.Errorf("dry_run must be %q, %q or %q; got %q. Omit it to preview without applying anything", "render", "server", "none", value)
	}
}
