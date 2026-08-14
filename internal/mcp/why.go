package mcp

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// The WHY section of diagnose_application: the server's structured causes
// (issue #77, ADR-0023).
//
// # This is composition, not diagnosis
//
// The causes, their confidences, their evidence and the revision correlation
// are ExplainService's. Nothing here decides anything: the section renders what
// the server produced, in the order the server produced it, exactly as the
// WORKLOADS section relays verdicts and the STATUS section relays a phase.
//
// That is the whole point of #77 relative to #73. Before it, the closest thing
// kelson had to a causal answer was this tool's own arrangement of facts, which
// meant the CLI and the UI could not have it and no other client could check
// it. The machinery now lives in internal/explain behind an RPC, and this
// surface composes it — ADR-0008's rule, applied to the capability it was
// written for.
//
// # Bounded by the server, and stated by it
//
// Explain enforces its own caps (6 causes, 4 evidence items each, 8 log lines
// per excerpt, 12 KiB in total) and lists everything it cut in `truncated`.
// This section therefore re-caps nothing except the evidence lines it indents:
// a second, quieter set of limits here would be limits nobody can see.
//
// # Additive, like every other section
//
// A server too old to serve ExplainService, or one whose delivery plane cannot
// answer, degrades to one line. The rest of the diagnosis is unaffected: the
// sections below this one are the ones that existed before the capability did,
// and they still answer without it.

// maxWhyEvidenceLines caps the lines of one evidence excerpt this section
// prints. The server has already bounded the excerpt; this is the indent
// budget, so a multi-line log block cannot push the sections below it out of
// view.
const maxWhyEvidenceLines = 8

func (c *clients) reportWhy(ctx context.Context, r *report, in diagnoseApplicationInput) {
	r.section("WHY (the server's causes, with confidence and evidence)")
	if c.explain == nil {
		r.addf("  unavailable — this build has no explain client")
		return
	}
	res, err := c.explain.Explain(ctx, connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        specRef(in.Project),
		Environment: in.Environment,
	}))
	if err != nil {
		r.addf("  unavailable — %s", connectMessage(err))
		return
	}
	msg := res.Msg

	if summary := msg.GetSummary(); summary != "" {
		r.addf("  %s", summary)
	}
	if len(msg.GetCauses()) == 0 {
		r.addf("  no cause found: nothing in the delivery status, the workload verdicts or the recent change explains a failure")
	}
	for i, cause := range msg.GetCauses() {
		writeCause(r, i+1, cause)
	}
	writeRecentChange(r, msg.GetRecentChange())
	writeWhyList(r, "  could not be read:", msg.GetNotes())
	writeWhyList(r, "  truncated:", msg.GetTruncated())
}

// writeCause renders one cause. The confidence leads the line because it is
// what a reader must weigh the sentence by: a medium cause states a correlation
// and must not be acted on as though it were a fact.
func writeCause(r *report, n int, cause *kelsonv1alpha1.ExplainCause) {
	r.addf("  %d. [%s] %s", n, cause.GetConfidence(), cause.GetCode())
	if resource := cause.GetResource(); resource != "" {
		r.addf("     resource: %s", resource)
	}
	r.addf("     %s", cause.GetMessage())
	if ref := cause.GetIntroducedBy(); ref != nil {
		r.addf("     introduced by: %s", revisionLine(ref))
	}
	for _, ev := range cause.GetEvidence() {
		writeEvidence(r, ev)
	}
	if fix := cause.GetRemediation(); fix != "" {
		r.addf("     fix: %s", fix)
	}
}

func writeEvidence(r *report, ev *kelsonv1alpha1.ExplainEvidence) {
	header := "     " + ev.GetKind()
	if source := ev.GetSource(); source != "" {
		header += " (" + source + ")"
	}
	lines := strings.Split(ev.GetDetail(), "\n")
	if len(lines) == 1 {
		r.addf("%s: %s", header, lines[0])
		return
	}
	r.addf("%s:", header)
	shown, dropped := limit(lines, maxWhyEvidenceLines)
	for _, line := range shown {
		r.addf("       | %s", line)
	}
	if dropped > 0 {
		r.addf("       "+truncation, dropped, "evidence lines")
	}
}

// writeRecentChange prints what the last revision changed. It is the
// correlation half of #77 and belongs next to the causes: "what is broken" and
// "what changed" are one answer, and an agent that has to call History and diff
// two revisions itself is paying context for a correlation the server made.
func writeRecentChange(r *report, change *kelsonv1alpha1.RecentChange) {
	if change == nil {
		return
	}
	r.addf("  recent change: %s", change.GetSummary())
	for _, ic := range change.GetImages() {
		r.addf("    image  %s/%s  %s → %s", ic.GetWorkload(), ic.GetContainer(), orDash(ic.GetBefore()), orDash(ic.GetAfter()))
	}
	for _, ec := range change.GetEnv() {
		r.addf("    env    %s/%s  %s %s %s", ec.GetWorkload(), ec.GetContainer(), ec.GetName(), ec.GetKind(), envDelta(ec))
	}
}

func envDelta(ec *kelsonv1alpha1.EnvChange) string {
	before, after := ec.GetBefore(), ec.GetAfter()
	switch {
	case before != "" && after != "":
		return before + " → " + after
	case before != "":
		return "(was " + before + ")"
	case after != "":
		return "(now " + after + ")"
	default:
		return ""
	}
}

func writeWhyList(r *report, title string, items []string) {
	if len(items) == 0 {
		return
	}
	r.addf("%s", title)
	for _, item := range items {
		r.addf("    - %s", item)
	}
}

func revisionLine(ref *kelsonv1alpha1.RevisionRef) string {
	parts := []string{ref.GetRevision()}
	for _, field := range []string{ref.GetCommittedAt(), ref.GetMessage()} {
		if field != "" {
			parts = append(parts, field)
		}
	}
	if author := ref.GetAuthor(); author != "" {
		parts = append(parts, "by "+author)
	}
	return strings.Join(parts, "  ")
}
