package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/explain"
	"github.com/dafrie/kelson/internal/observation"
)

// ExplainService served: "why is this component degraded?" answered with
// structured causes rather than a log dump (issue #77, ADR-0023).
//
// # The handler is assembly, and that is the point
//
// Every judgement in the answer belongs to a plane that already owns it: the
// verdicts are the observation plane's, the log window is the log engine's.
// This handler resolves them and hands them to internal/explain, which is where
// the causal machinery lives so the CLI and the MCP surface can compose the
// same capability instead of each growing a diagnosis of their own.
//
// # It degrades the way a diagnosis must, and it still degrades today
//
// Everything beyond the verdicts is additive: a missing environment status
// reader, a server started without a log engine, manifests this RPC does not
// fetch — each costs a correlation and arrives in the response's `notes`,
// never as a transport error. A tool an agent calls when something is already
// broken must not itself break because a second source is unavailable.
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) deleted the delivery adapters
// that used to report the phase and the rendered-manifest store that used to
// back a revision's env-var diff, and issue #224 tracked both as a gap. The
// spine closed the first two thirds of it: `Environment.status.phase`,
// `status.revision` and the bounded `status.history` mirror
// (internal/controlstore's EnvironmentStore) are what this handler reads
// below, so "what is deployed now" and "when did it last change" are answered
// from the same source `kelson status` and `kelson history` read. What is
// still missing is the recorded manifests themselves — the registry holds
// them (ADR-0028 decision 4) and this RPC does not fetch them (the same
// choice History makes, in deploy.go) — so a cause's IntroducedBy and a
// container-level env-var diff still cost a note, and the change correlation
// below states images at the whole-environment grain the history mirror
// carries rather than per container.
func (s *Server) Explain(ctx context.Context, req *connect.Request[kelsonv1alpha1.ExplainRequest]) (*connect.Response[kelsonv1alpha1.ExplainResponse], error) {
	msg := req.Msg
	out, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), msg.GetImage(), msg.GetProfile())
	if err != nil {
		return nil, failRequest(err)
	}
	set, err := manifestSet(out)
	if err != nil {
		return nil, fail(connect.CodeInternal, err)
	}
	t := target(out)
	plane, err := s.plane(ctx, t)
	if err != nil {
		return nil, failRequest(err)
	}

	verdicts, err := observeWorkloads(ctx, plane, set, t.Namespace)
	if err != nil {
		return nil, failRequest(err)
	}

	in := explain.Input{
		Project:     set.Project,
		Environment: set.Environment,
		Namespace:   t.Namespace,
		Verdicts:    verdicts,
		Logs:        s.explainLogs(),
	}
	// st is read once and used twice: to fill in.Status/in.History below, and
	// again after Explain runs to complete the change correlation with what
	// the history mirror carries that delivery.Entry cannot (the resolved
	// images). note == "" means st is authoritative, including a zero value
	// for "no Environment resource yet" — which is not a degradation, it is
	// the honest state of an environment nothing has been deployed to, and
	// Explain's own empty-history note says so without this handler repeating
	// it.
	st, note := s.explainEnvironmentState(ctx, t.Project, t.Environment)
	if note != "" {
		in.Notes = append(in.Notes, note)
	} else {
		in.Status = deliveryStatus(st)
		in.History = historyEntries(st)
		in.Notes = append(in.Notes, revisionNotes(st)...)
	}

	explanation := explain.Explain(ctx, in)
	if note == "" {
		completeRecentChange(explanation.RecentChange, st)
	}
	return connect.NewResponse(wireExplanation(explanation)), nil
}

// explainEnvironmentState reads the status the revision correlation is built
// from, or says why there is not one to read. A "not found" Environment is
// not a note: it means nothing has been deployed through kelson here, and
// [explain.Explain] already states that once in.History is empty — a second
// note saying the same thing would just repeat it.
func (s *Server) explainEnvironmentState(ctx context.Context, project, environment string) (controlstore.EnvironmentState, string) {
	if s.environments == nil {
		return controlstore.EnvironmentState{},
			"no Environment status reader is configured, so status.revision and status.history could not be read " +
				"and no revision was correlated"
	}
	st, err := s.environments.Get(ctx, project, environment)
	if err != nil {
		if controlstore.AsNotFound(err) {
			return controlstore.EnvironmentState{}, ""
		}
		return controlstore.EnvironmentState{},
			fmt.Sprintf("the Environment status could not be read (%s), so no revision was correlated", err)
	}
	return st, ""
}

// deliveryStatus projects the Environment's status onto the vocabulary
// [explain.Input.Status] reads: the delivery phase, and the revision actually
// serving — which, while a rollback is pinned (ADR-0028 decision 5), is not
// the newest entry in status.history.
func deliveryStatus(st controlstore.EnvironmentState) delivery.Status {
	status := delivery.Status{
		Phase:    delivery.Phase(st.Phase),
		Revision: st.Revision,
	}
	if ready, ok := st.Ready(); ok && !ready.True() {
		// Relayed verbatim, the same choice deliveryState makes in
		// environment.go: it is the controller's own account of what went
		// wrong, and it is what lets internal/explain's reconciler causes
		// fire on real evidence instead of never firing at all.
		status.Cause = ready.Message
	}
	return status
}

// historyEntries projects the bounded history mirror onto [delivery.Entry],
// reordered so index 0 is the revision [controlstore.EnvironmentState.Running]
// names rather than unconditionally the newest published one. The two
// coincide except while a rollback is pinned, and internal/explain's own
// change correlation always reads index 0 as "what changed" — so this is what
// keeps that correlation about the revision the verdicts above are verdicts
// of, rather than about a newer build that was never live.
func historyEntries(st controlstore.EnvironmentState) []delivery.Entry {
	if len(st.History) == 0 {
		return nil
	}
	ordered := st.History
	if running, ok := st.Running(); ok && running.Revision != st.History[0].Revision {
		ordered = make([]controlstore.Revision, 0, len(st.History))
		ordered = append(ordered, running)
		for _, r := range st.History {
			if r.Revision != running.Revision {
				ordered = append(ordered, r)
			}
		}
	}
	entries := make([]delivery.Entry, 0, len(ordered))
	for _, r := range ordered {
		entries = append(entries, delivery.Entry{
			Revision:    r.Revision,
			SpecHash:    r.SpecHash,
			CommittedAt: committedAt(r.Timestamp),
			// The wire has no slot for outcome, digest or images beyond this
			// string (deploy.go's History RPC hits the same wall and makes
			// the same choice, in revisionSummary).
			Message: revisionSummary(r, r.Revision == st.Revision),
		})
	}
	return entries
}

// revisionNotes states what the correlation should be read with caution
// about: a status that has not caught up with the spec, and a rollback that
// makes "what changed" and "what is serving" two different revisions.
func revisionNotes(st controlstore.EnvironmentState) []string {
	var notes []string
	if st.Generation > 0 && st.ObservedGeneration < st.Generation {
		notes = append(notes, fmt.Sprintf(
			"the status describes generation %d but the spec is now at generation %d: the controller has not "+
				"caught up, so the revision and history below may not reflect the latest spec",
			st.ObservedGeneration, st.Generation))
	}
	running, ok := st.Running()
	if !ok {
		return notes
	}
	if head, headOK := st.HeadRevision(); headOK && head.Revision != running.Revision {
		msg := fmt.Sprintf("revision %s is serving, not %s", running.Revision, head.Revision)
		if st.RollbackRevision != "" {
			msg += fmt.Sprintf(": a rollback is pinned (kelson.dev/rollback-to: %s)", st.RollbackRevision)
		} else {
			msg += ": a rollback is pinned"
		}
		msg += fmt.Sprintf("; %s is the most recently published revision and is not live, so it is not what "+
			"caused what follows", head.Revision)
		notes = append(notes, msg)
	}
	return notes
}

// completeRecentChange fills in what [explain.Explain] leaves unset without a
// [explain.ManifestFn]: the previous revision, and the one grain of "what
// changed" the history mirror carries without the recorded manifests — the
// resolved images, compared as a whole-environment list rather than
// internal/explain's per-container diff.
//
// It is a projection performed here, not a change to internal/explain: the
// source it reads, [controlstore.Revision.Images], is not part of
// [delivery.Entry], and that package's own ManifestFn contract is what a
// registry-backed manifest fetch will satisfy when issue #38 is built —
// adding a second, narrower path into that package would be the redesign the
// task explicitly avoided.
func completeRecentChange(change *explain.Change, st controlstore.EnvironmentState) {
	if change == nil {
		return
	}
	previous, ok := st.PreviousRevision()
	if !ok {
		change.Summary = change.Revision.Revision +
			" is the first recorded revision of this environment: there is nothing to compare it against"
		return
	}
	change.Previous = &explain.RevisionRef{
		Revision:    previous.Revision,
		CommittedAt: committedAt(previous.Timestamp),
		Message:     revisionSummary(previous, false),
	}
	running, ok := st.Running()
	if !ok {
		running = previous // unreachable in practice: PreviousRevision found an entry, so History is non-empty.
	}
	change.Summary = imageChangeSummary(previous, running)
}

// maxImageChangesShown bounds how many images one summary names, the same cap
// internal/explain's own change lists use ([explain.MaxChanges]): evidence for
// a diagnosis, not a changelog.
const maxImageChangesShown = explain.MaxChanges

// imageChangeSummary states what moved between two revisions' recorded
// images. It cannot say which container each belongs to — that needs the
// recorded manifests (issue #38) — so it says images, not containers, and
// says so rather than implying a diff it did not make.
func imageChangeSummary(previous, running controlstore.Revision) string {
	added, removed := imageDelta(previous.Images, running.Images)
	if len(added) == 0 && len(removed) == 0 {
		return fmt.Sprintf("%s → %s: the recorded images are unchanged (a per-container or environment-variable "+
			"diff needs the recorded manifests, which this server does not fetch)", previous.Revision, running.Revision)
	}
	var parts []string
	if len(removed) > 0 {
		parts = append(parts, "removed "+joinBounded(removed, maxImageChangesShown))
	}
	if len(added) > 0 {
		parts = append(parts, "added "+joinBounded(added, maxImageChangesShown))
	}
	return fmt.Sprintf("%s → %s: %s (from the history mirror's recorded images, not a per-container diff — the "+
		"recorded manifests are not fetched here)", previous.Revision, running.Revision, strings.Join(parts, "; "))
}

// imageDelta is a whole-environment set difference, not a per-container
// match: two revisions with the same images in a different order are
// unchanged, and an image string present on both sides never appears in
// either list.
func imageDelta(before, after []string) (added, removed []string) {
	beforeSet := make(map[string]bool, len(before))
	for _, img := range before {
		beforeSet[img] = true
	}
	afterSet := make(map[string]bool, len(after))
	for _, img := range after {
		afterSet[img] = true
	}
	for _, img := range after {
		if !beforeSet[img] {
			added = append(added, img)
		}
	}
	for _, img := range before {
		if !afterSet[img] {
			removed = append(removed, img)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// joinBounded lists items up to max and names the remainder rather than
// growing the summary without bound.
func joinBounded(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s (and %d more)", strings.Join(items[:max], ", "), len(items)-max)
}

// explainLogs adapts the server's log engine onto explain's seam. A server
// started without one returns nil, which explain reports as a missing excerpt
// rather than as an error.
//
// The window asked for is the crash-loop window: the lines before the container
// terminated, which is the same query diagnose_component issues and the one
// observation.Around was built for (issue #54).
func (s *Server) explainLogs() explain.LogFn {
	if s.logs == nil {
		return nil
	}
	return func(ctx context.Context, namespace, component string, lines int) ([]observation.Line, error) {
		res, err := s.logs.Query(ctx, observation.Query{
			Namespace: namespace,
			Component: component,
			Around:    &observation.Around{Lines: lines, AtTermination: true},
		})
		if err != nil {
			return nil, err
		}
		return res.Lines, nil
	}
}

// wireExplanation projects the explanation onto the schema. It is a field-for-
// field projection on purpose: the bounds were enforced by internal/explain
// before this runs, and a projection that summarised anything here would put a
// second, quieter set of rules in the one place a client cannot see them.
func wireExplanation(e explain.Explanation) *kelsonv1alpha1.ExplainResponse {
	out := &kelsonv1alpha1.ExplainResponse{
		Subject: &kelsonv1alpha1.ExplainSubject{
			Project:     e.Subject.Project,
			Environment: e.Subject.Environment,
			Namespace:   e.Subject.Namespace,
			Revision:    e.Subject.Revision,
		},
		Phase:     e.Phase,
		Summary:   e.Summary,
		Notes:     e.Notes,
		Truncated: e.Truncated,
	}
	for _, c := range e.Causes {
		out.Causes = append(out.Causes, wireCause(c))
	}
	out.RecentChange = wireRecentChange(e.RecentChange)
	return out
}

func wireCause(c explain.Cause) *kelsonv1alpha1.ExplainCause {
	out := &kelsonv1alpha1.ExplainCause{
		Code:         string(c.Code),
		Message:      c.Message,
		Confidence:   string(c.Confidence),
		Resource:     c.Resource,
		Remediation:  c.Remediation,
		IntroducedBy: wireRevisionRef(c.IntroducedBy),
	}
	for _, ev := range c.Evidence {
		out.Evidence = append(out.Evidence, &kelsonv1alpha1.ExplainEvidence{
			Kind:   string(ev.Kind),
			Source: ev.Source,
			Detail: ev.Detail,
		})
	}
	return out
}

func wireRecentChange(change *explain.Change) *kelsonv1alpha1.RecentChange {
	if change == nil {
		return nil
	}
	out := &kelsonv1alpha1.RecentChange{
		Revision: wireRevisionRef(&change.Revision),
		Previous: wireRevisionRef(change.Previous),
		Summary:  change.Summary,
	}
	for _, ec := range change.Env {
		out.Env = append(out.Env, &kelsonv1alpha1.EnvChange{
			Workload:  ec.Workload,
			Container: ec.Container,
			Name:      ec.Name,
			Kind:      string(ec.Kind),
			Before:    ec.Before,
			After:     ec.After,
		})
	}
	for _, ic := range change.Images {
		out.Images = append(out.Images, &kelsonv1alpha1.ImageChange{
			Workload:  ic.Workload,
			Container: ic.Container,
			Before:    ic.Before,
			After:     ic.After,
		})
	}
	return out
}

func wireRevisionRef(r *explain.RevisionRef) *kelsonv1alpha1.RevisionRef {
	if r == nil {
		return nil
	}
	return &kelsonv1alpha1.RevisionRef{
		Revision:    r.Revision,
		CommittedAt: r.CommittedAt,
		Message:     r.Message,
		Author:      r.Author,
	}
}
