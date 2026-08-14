package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/diff"
)

const promoteDescription = `Pin one environment to the images another environment is currently running, without rebuilding anything.

MUTATES THE STORED SPEC when execute=true. The default is execute=false, which writes nothing.

Promotion in kelson is a spec edit and nothing else: the images the source environment's latest deployed revision runs are written as the target environment's per-component image pins. Nothing is built, nothing is deployed, and no promotion record is kept — the change is one image line per component in the target Environment document, and the deployment history records it the way it records any other spec change.

The images come from the delivery history, not from the source environment's spec: a promotion moves what ran, not what was intended. A component whose image the recorded revision does not carry is reported as skipped with a reason, never guessed.

The answer always includes the resulting diff of the target environment, whether or not it executed, so there is no second call needed to find out what the promotion changes.

Preconditions: the project must be stored, both environments must be declared by it, and the source environment must have a deployed revision (promote/nothing-deployed otherwise). This does NOT deploy — call deploy on the target environment afterwards to make the pins live.

Cost: one server call. Every call carries an idempotency key, returned in the answer; pass it back on a retry so the retry is the same write rather than a second one.`

type promoteInput struct {
	Project         string   `json:"project" jsonschema:"the stored project name"`
	FromEnvironment string   `json:"from_environment" jsonschema:"the environment whose deployed images are promoted, e.g. staging"`
	ToEnvironment   string   `json:"to_environment" jsonschema:"the environment whose image pins are written, e.g. production"`
	Components      []string `json:"components,omitempty" jsonschema:"restrict the promotion to these components; omitted promotes every workload component"`
	Execute         bool     `json:"execute,omitempty" jsonschema:"false (default) previews only; true writes the pins to the stored spec"`
	Version         string   `json:"version,omitempty" jsonschema:"the spec version read before promoting; a mismatch fails with store/version-conflict instead of overwriting"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty" jsonschema:"reuse the key from a previous attempt so a retry is the same write, not a second one"`
	Reason          string   `json:"reason,omitempty" jsonschema:"why you are promoting, in one sentence; recorded in the audit trail beside the action"`
}

func promoteTool(c *clients) tool {
	// Not destructive: a promotion writes a spec field and takes nothing away
	// from what is currently serving. What it changes only reaches the cluster
	// when someone deploys, and `deploy` is the tool that carries that warning.
	def := mutatingTool("promote_component", "Promote an environment", promoteDescription, false)
	return tool{
		def:  def,
		rpcs: []rpc{rpcPromote},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in promoteInput) (*mcpsdk.CallToolResult, any, error) {
				return c.promoteComponent(ctx, in)
			})
		},
	}
}

// promoteComponent composes the Promote RPC. Dry run is first-class and is
// the default: the preview and the write return the same answer shape, so an
// agent reads what would happen in exactly the form it will read what did.
func (c *clients) promoteComponent(ctx context.Context, in promoteInput) (*mcpsdk.CallToolResult, any, error) {
	dryRun := kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	if in.Execute {
		dryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	}
	key := in.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}

	res, err := c.deploy.Promote(ctx, reasoned(connect.NewRequest(&kelsonv1alpha1.PromoteRequest{
		Project:         in.Project,
		FromEnvironment: in.FromEnvironment,
		ToEnvironment:   in.ToEnvironment,
		Components:      in.Components,
		DryRun:          dryRun,
		Version:         in.Version,
		IdempotencyKey:  key,
	}), in.Reason))
	if err != nil {
		return nil, nil, c.fail(rpcPromote, err)
	}
	msg := res.Msg

	var r report
	r.addf("%s", promoteHeadline(in, msg))
	r.addf("idempotency_key: %s (pass it back to retry this exact promotion)", key)
	if revision := msg.GetFromRevision(); revision != "" {
		r.addf("source revision: %s of %s/%s — the deployed truth these images came from",
			revision, in.Project, in.FromEnvironment)
	}

	writePromotedComponents(&r, msg.GetComponents())
	writePromotionDiff(&r, msg)
	if errs := msg.GetErrors(); len(errs) > 0 {
		shown, dropped := limit(errs, maxSpecErrors)
		r.section("ERRORS")
		for _, e := range shown {
			r.wireError("  ", e)
		}
		r.truncated(dropped, "errors")
		r.addf("  nothing was written: a promotion whose result does not validate or render is refused.")
	}
	if in.Execute && len(msg.GetErrors()) == 0 {
		r.section("NEXT")
		r.addf("  the pins are stored but not live. Call deploy for %s/%s to apply them.", in.Project, in.ToEnvironment)
	}
	return text(&r)
}

// promoteHeadline states first what an agent must not miss: whether the spec
// changed, and how much of the promotion actually landed.
func promoteHeadline(in promoteInput, msg *kelsonv1alpha1.PromoteResponse) string {
	var pinned, unchanged, skipped int
	for _, c := range msg.GetComponents() {
		switch c.GetStatus() {
		case kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED:
			pinned++
		case kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_UNCHANGED:
			unchanged++
		case kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_SKIPPED:
			skipped++
		default:
		}
	}
	counts := fmt.Sprintf("%d pinned, %d unchanged, %d skipped", pinned, unchanged, skipped)
	if !in.Execute {
		return fmt.Sprintf("promote %s %s→%s: PREVIEW ONLY, nothing was written (%s). Set execute=true to write the pins.",
			in.Project, in.FromEnvironment, in.ToEnvironment, counts)
	}
	if len(msg.GetErrors()) > 0 {
		return fmt.Sprintf("promote %s %s→%s: REFUSED, nothing was written; see ERRORS below.",
			in.Project, in.FromEnvironment, in.ToEnvironment)
	}
	return fmt.Sprintf("promote %s %s→%s: WRITTEN to spec version %s (%s). NOT deployed.",
		in.Project, in.FromEnvironment, in.ToEnvironment, orDash(msg.GetVersion()), counts)
}

// writePromotedComponents lists every component the promotion considered,
// including the ones nothing happened to. A component missing from the list
// would be indistinguishable from one kelson forgot.
func writePromotedComponents(r *report, components []*kelsonv1alpha1.PromotedComponent) {
	if len(components) == 0 {
		r.section("COMPONENTS")
		r.addf("  none: this promotion selected no workload component.")
		return
	}
	shown, dropped := limit(components, maxComponents)
	r.section(fmt.Sprintf("COMPONENTS (%d)", len(components)))
	for _, c := range shown {
		switch c.GetStatus() {
		case kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED:
			r.addf("  %s pinned    %s -> %s", pad(c.GetComponent(), 20), orDash(c.GetFromImage()), c.GetToImage())
		case kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_UNCHANGED:
			r.addf("  %s unchanged  already pinned to %s", pad(c.GetComponent(), 20), c.GetToImage())
		default:
			r.addf("  %s skipped    %s (%s)", pad(c.GetComponent(), 20), c.GetReason(), c.GetCode())
		}
	}
	r.truncated(dropped, "components")
}

// writePromotionDiff reports the counts, never the diff itself — the same rule
// the rollback preview follows, and for the same reason: a rendered diff is
// thousands of lines and an agent that needs them has RenderService.
func writePromotionDiff(r *report, msg *kelsonv1alpha1.PromoteResponse) {
	r.section("DIFF")
	encoded := msg.GetDiffJson()
	if len(encoded) == 0 {
		r.addf("  none: the server returned no diff for this promotion.")
		return
	}
	var d diff.Diff
	if err := json.Unmarshal(encoded, &d); err != nil {
		r.addf("  unreadable: %s", err)
		return
	}
	r.addf("  %d added, %d modified, %d removed (max risk %s)",
		d.Summary.Added, d.Summary.Modified, d.Summary.Removed, d.Summary.MaxRisk)
	if len(d.Summary.Restarting) > 0 {
		shown, dropped := limit(d.Summary.Restarting, maxFindings)
		r.addf("  restarting: %s", joinCapped(shown, dropped))
	}
	if len(d.Summary.Disruptive) > 0 {
		shown, dropped := limit(d.Summary.Disruptive, maxFindings)
		r.addf("  disruptive: %s", joinCapped(shown, dropped))
	}
	r.addf("  exit semantics: %d (0 no changes, 2 changes present, 3 blocked)", msg.GetExitSemantics())
}
