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

const rollbackDescription = `NOT AVAILABLE. Rolling back an environment is refused while kelson's delivery spine is rebuilt.

MUTATES THE CLUSTER when execute=true, in principle. In practice it CHANGES NOTHING in every mode, including execute=false: every call is refused with the code delivery/not-implemented, naming issue #224. Do not retry it and do not work around it.

Why the preview is refused too, and not answered as "no findings": the preview lists what a rollback cannot revert — data written, a volume resized, an external side effect — and it was computed by comparing two recorded revisions. The store that kept them is deleted (ADR-0027), so an empty preview would report "nothing to worry about" for a question nothing looked at. That is the exact failure the preview exists to prevent.

What to do instead: diagnose_application to find out what is wrong, and tell the human that the environment needs a rollback a person must perform.`

type rollbackInput struct {
	Project        string `json:"project" jsonschema:"the stored project name"`
	Environment    string `json:"environment" jsonschema:"the environment to roll back, e.g. production"`
	ToRevision     string `json:"to_revision,omitempty" jsonschema:"the revision to restore; omitted restores the revision before the current one"`
	Execute        bool   `json:"execute,omitempty" jsonschema:"false (default) previews only; true applies the rollback"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"reuse the key from a previous attempt so a retry is the same rollback, not a second one"`
	Reason         string `json:"reason,omitempty" jsonschema:"why you are rolling back, in one sentence; recorded in the audit trail beside the action"`
}

func rollbackTool(c *clients) tool {
	def := mutatingTool("rollback", "Roll back an environment", rollbackDescription, true)
	return tool{
		def:  def,
		rpcs: []rpc{rpcRollback},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in rollbackInput) (*mcpsdk.CallToolResult, any, error) {
				return c.rollbackEnvironment(ctx, in)
			})
		},
	}
}

// rollbackEnvironment consumes the Rollback stream. The preview event always
// comes first, whether or not the rollback executes, which is the API's
// contract and this tool's headline (issues #38, #55).
func (c *clients) rollbackEnvironment(ctx context.Context, in rollbackInput) (*mcpsdk.CallToolResult, any, error) {
	dryRun := kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	if in.Execute {
		dryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	}
	key := in.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}

	stream, err := c.deploy.Rollback(ctx, reasoned(connect.NewRequest(&kelsonv1alpha1.RollbackRequest{
		Spec:           specRef(in.Project),
		Environment:    in.Environment,
		ToRevision:     in.ToRevision,
		DryRun:         dryRun,
		IdempotencyKey: key,
	}), in.Reason))
	if err != nil {
		return nil, nil, c.fail(rpcRollback, err)
	}
	defer func() { _ = stream.Close() }()

	var (
		preview   *kelsonv1alpha1.RollbackResponse_Preview
		committed *kelsonv1alpha1.RollbackResponse_Committed
		settled   *kelsonv1alpha1.RollbackResponse_Settled
	)
	for stream.Receive() {
		switch event := stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.RollbackResponse_Preview_:
			preview = event.Preview
		case *kelsonv1alpha1.RollbackResponse_Committed_:
			committed = event.Committed
		case *kelsonv1alpha1.RollbackResponse_Settled_:
			settled = event.Settled
		}
	}
	if err := stream.Err(); err != nil {
		return nil, nil, c.fail(rpcRollback, err)
	}

	var r report
	target := in.ToRevision
	if preview != nil {
		target = preview.GetToRevision()
	}
	if in.Execute {
		r.addf("rollback %s/%s to revision %s: EXECUTED.", in.Project, in.Environment, orDash(target))
	} else {
		r.addf("rollback %s/%s to revision %s: PREVIEW ONLY, nothing was applied. Set execute=true to apply it.",
			in.Project, in.Environment, orDash(target))
	}
	r.addf("idempotency_key: %s (pass it back to retry this exact rollback)", key)

	writeFindings(&r, preview)
	writeDiffSummary(&r, preview)

	if committed != nil {
		r.section("RESULT")
		r.addf("  restored revision %s, recorded as revision %s", committed.GetRestoredRevision(), committed.GetAsRevision())
	}
	if settled != nil && settled.GetError() != nil {
		r.section("ERROR")
		r.wireError("  ", settled.GetError())
	}
	return text(&r)
}

// writeFindings lists what the rollback cannot revert, unrecoverable first.
//
// An absent warning must never read as "nothing to warn about": a preview event
// with no findings says so explicitly, and a delivery mode whose recorded
// history kelson cannot read says that instead of showing an empty list.
func writeFindings(r *report, preview *kelsonv1alpha1.RollbackResponse_Preview) {
	if preview == nil {
		r.section("FINDINGS")
		r.addf("  the server sent no preview; treat this rollback as unassessed.")
		return
	}
	findings := preview.GetFindings()
	unrecoverable := 0
	for _, f := range findings {
		if f.GetUnrecoverable() {
			unrecoverable++
		}
	}
	r.section(fmt.Sprintf("FINDINGS (%d, %d unrecoverable)", len(findings), unrecoverable))
	if len(findings) == 0 {
		r.addf("  none: the server found nothing this rollback cannot revert.")
		return
	}
	shown, dropped := limit(findings, maxFindings)
	for _, f := range shown {
		marker := "recoverable  "
		if f.GetUnrecoverable() {
			marker = "UNRECOVERABLE"
		}
		r.addf("  %s %s %s", marker, pad(f.GetResource(), 40), f.GetPath())
		r.addf("      cause: %s — %s", f.GetCause(), f.GetMessage())
	}
	r.truncated(dropped, "findings")
}

// writeDiffSummary reports the counts, never the diff. The encoded diff is the
// one canonical representation the CLI and the UI also read (internal/diff), so
// it is decoded with that package's own type rather than re-described here.
func writeDiffSummary(r *report, preview *kelsonv1alpha1.RollbackResponse_Preview) {
	if preview == nil {
		return
	}
	r.section("DIFF")
	encoded := preview.GetDiffJson()
	if len(encoded) == 0 {
		r.addf("  none: this delivery mode records no manifests kelson can compare, so the change set is unknown.")
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
	if len(d.Violations) > 0 {
		r.addf("  %d policy violation(s) reported by the preview", len(d.Violations))
	}
}
