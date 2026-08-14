package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/explain"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/redact"
)

// ExplainService served: "why is this component degraded?" answered with
// structured causes rather than a log dump (issue #77, ADR-0023).
//
// # The handler is assembly, and that is the point
//
// Every judgement in the answer belongs to a plane that already owns it: the
// phase is the adapter's, the verdicts are the observation plane's, the
// recorded manifests are the history store's, the log window is the log
// engine's. This handler resolves them and hands them to internal/explain,
// which is where the causal machinery lives so the CLI and the MCP surface can
// compose the same capability instead of each growing a diagnosis of their own.
//
// # It degrades the way a diagnosis must
//
// Status is the spine and its failure is the answer. Everything after it is
// additive: an adapter with no history, a plane with no recorded manifests, a
// server started without a log engine — each costs a correlation and arrives in
// the response's `notes`, never as a transport error. A tool an agent calls
// when something is already broken must not itself break because a second
// source is unavailable.
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
	t := target(out, msg.GetMode())
	adapter, plane, err := s.selectAdapter(ctx, t)
	if err != nil {
		return nil, failRequest(err)
	}

	status, err := adapter.Status(ctx, set)
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
		Status:      status,
		Verdicts:    verdicts,
		Logs:        s.explainLogs(),
		Manifests:   explainManifests(plane),
	}
	// History is what makes the change correlation possible, and an adapter
	// that cannot report it yields a note rather than a failure: a mode kelson
	// records no history for, or a store momentarily unreachable, must not take
	// the diagnosis with it. The note is composed here because the reason
	// belongs to this plane — which adapter refused, and why.
	entries, err := adapter.History(ctx, set)
	if err != nil {
		in.Notes = append(in.Notes, fmt.Sprintf("the %s adapter could not report its history (%s), so no change was correlated",
			adapter.Name(), redact.Scrub(err.Error())))
	}
	in.History = entries

	return connect.NewResponse(wireExplanation(explain.Explain(ctx, in))), nil
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

// explainManifests adapts the plane's recorded-history source onto explain's
// seam. A mode that keeps no rendered history kelson can read has none, and the
// explanation says so.
func explainManifests(plane *Plane) explain.ManifestFn {
	if plane == nil || plane.Recorded == nil {
		return nil
	}
	return plane.Recorded.Revision
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
