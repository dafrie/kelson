package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/observation"
)

// QueryLogs runs one bounded query.
//
// The wire message maps 1:1 onto observation.Query and the engine does the real
// checking: its validate/requireBound rules are the definition of "bounded", so
// duplicating them here would create a second, drifting copy of the invariant.
// Everything the engine rejects is the caller's request being wrong, hence
// CodeInvalidArgument (issue #54).
func (s *Server) QueryLogs(ctx context.Context, req *connect.Request[kelsonv1alpha1.QueryLogsRequest]) (*connect.Response[kelsonv1alpha1.QueryLogsResponse], error) {
	if s.logs == nil {
		return nil, unimplemented("log queries")
	}
	msg := req.Msg
	query := observation.Query{
		Namespace: msg.GetSelector().GetNamespace(),
		// LogSelector.application is the v1alpha1 wire name for the component
		// (ADR-0032 renamed the vocabulary and the label, not the wire field).
		Component:  msg.GetSelector().GetApplication(),
		Containers: msg.GetSelector().GetContainers(),
		Tail:       int(msg.GetTail()),
		Since:      unixMillis(msg.GetSinceUnixMs()),
		Until:      unixMillis(msg.GetUntilUnixMs()),
		Around:     wireAround(msg.GetAround()),
		Match:      wireMatch(msg.GetMatch()),
	}
	result, err := s.logs.Query(ctx, query)
	if err != nil {
		return nil, fail(connect.CodeInvalidArgument, err)
	}
	lines := make([]*kelsonv1alpha1.LogLine, 0, len(result.Lines))
	for _, l := range result.Lines {
		lines = append(lines, wireLine(l))
	}
	return connect.NewResponse(&kelsonv1alpha1.QueryLogsResponse{Lines: lines}), nil
}

// FollowLogs streams lines live.
//
// Loss is reported, never hidden: the engine counts what it dropped under
// backpressure, and a dropped event goes out whenever that counter has moved
// since the last line, so a client reads a gap as a gap rather than believing
// the workload went quiet.
func (s *Server) FollowLogs(ctx context.Context, req *connect.Request[kelsonv1alpha1.FollowLogsRequest], stream *connect.ServerStream[kelsonv1alpha1.FollowLogsResponse]) error {
	if s.logs == nil {
		return unimplemented("log following")
	}
	msg := req.Msg
	query := observation.Query{
		Namespace: msg.GetSelector().GetNamespace(),
		// LogSelector.application is the v1alpha1 wire name for the component
		// (ADR-0032 renamed the vocabulary and the label, not the wire field).
		Component:  msg.GetSelector().GetApplication(),
		Containers: msg.GetSelector().GetContainers(),
		Since:      unixMillis(msg.GetSinceUnixMs()),
		Match:      wireMatch(msg.GetMatch()),
		Backlog:    int(msg.GetBacklog()),
	}
	lines, follow, err := s.logs.Follow(ctx, query)
	if err != nil {
		return fail(connect.CodeInvalidArgument, err)
	}

	dropped := 0
	for line := range lines {
		if err := stream.Send(&kelsonv1alpha1.FollowLogsResponse{
			Event: &kelsonv1alpha1.FollowLogsResponse_Line{Line: wireLine(line)},
		}); err != nil {
			return err
		}
		if n := follow.Dropped(); n > dropped {
			dropped = n
			if err := stream.Send(&kelsonv1alpha1.FollowLogsResponse{
				Event: &kelsonv1alpha1.FollowLogsResponse_Dropped{Dropped: int64(n)},
			}); err != nil {
				return err
			}
		}
	}
	// A stream that ends having dropped its final lines must still say so; the
	// per-line check above cannot report loss that happened after the last line
	// it forwarded.
	if n := follow.Dropped(); n > dropped {
		return stream.Send(&kelsonv1alpha1.FollowLogsResponse{
			Event: &kelsonv1alpha1.FollowLogsResponse_Dropped{Dropped: int64(n)},
		})
	}
	return nil
}

func wireLine(l observation.Line) *kelsonv1alpha1.LogLine {
	line := &kelsonv1alpha1.LogLine{
		Pod:       l.Pod,
		Container: l.Container,
		Message:   l.Message,
	}
	// A zero timestamp means the line carried none the engine could parse; the
	// schema says 0 for exactly that, so it must not become the Unix epoch.
	if !l.Timestamp.IsZero() {
		line.TimestampUnixMs = l.Timestamp.UnixMilli()
	}
	return line
}

func wireAround(a *kelsonv1alpha1.LogAround) *observation.Around {
	if a == nil {
		return nil
	}
	return &observation.Around{
		Lines:         int(a.GetLines()),
		Time:          unixMillis(a.GetTimeUnixMs()),
		AtTermination: a.GetAtTermination(),
	}
}

func wireMatch(m *kelsonv1alpha1.LogMatch) *observation.Match {
	if m == nil {
		return nil
	}
	return &observation.Match{Substring: m.GetSubstring(), Regex: m.GetRegex()}
}

// unixMillis converts a wire instant. Zero is "unset" throughout the schema,
// not midnight 1970 — a query bounded at the epoch is not a query anyone means.
func unixMillis(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}
