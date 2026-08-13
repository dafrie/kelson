package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/observation"
)

// TestQueryLogs: the wire message maps 1:1 onto observation.Query, and a line
// that carried no parseable timestamp stays 0 rather than becoming the epoch.
func TestQueryLogs(t *testing.T) {
	at := time.Date(2026, 8, 13, 9, 30, 0, 0, time.UTC)
	engine := &fakeLogEngine{result: observation.Result{Lines: []observation.Line{
		{Timestamp: at, Pod: "web-1", Container: "web", Message: "listening on :8080"},
		{Pod: "web-1", Container: "web", Message: "no timestamp here"},
	}}}
	c := serve(t, Options{Logs: engine})

	since := at.Add(-time.Hour)
	res, err := c.logs.QueryLogs(context.Background(), connect.NewRequest(&kelsonv1alpha1.QueryLogsRequest{
		Selector: &kelsonv1alpha1.LogSelector{
			Namespace:   "hello-development",
			Application: "web",
			Containers:  []string{"web"},
		},
		Tail:        50,
		SinceUnixMs: since.UnixMilli(),
		Match:       &kelsonv1alpha1.LogMatch{Substring: "listening"},
	}))
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}

	q := engine.query
	if q.Namespace != "hello-development" || q.Application != "web" {
		t.Errorf("selector did not reach the engine: %+v", q)
	}
	if q.Tail != 50 {
		t.Errorf("tail = %d, want 50", q.Tail)
	}
	if q.Since == nil || !q.Since.Equal(since) {
		t.Errorf("since = %v, want %v", q.Since, since)
	}
	if q.Until != nil {
		t.Errorf("until = %v, want nil: 0 on the wire is unset, not the epoch", q.Until)
	}
	if q.Match == nil || q.Match.Substring != "listening" {
		t.Errorf("match = %+v", q.Match)
	}

	lines := res.Msg.GetLines()
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	if lines[0].GetTimestampUnixMs() != at.UnixMilli() {
		t.Errorf("timestamp = %d, want %d", lines[0].GetTimestampUnixMs(), at.UnixMilli())
	}
	if lines[1].GetTimestampUnixMs() != 0 {
		t.Errorf("an undated line reported %d, want 0", lines[1].GetTimestampUnixMs())
	}
	if lines[0].GetPod() != "web-1" || lines[0].GetContainer() != "web" {
		t.Errorf("line = %+v", lines[0])
	}
}

// TestQueryLogsUnbounded: the engine owns the bounding rule and its refusal is
// the caller's request being wrong, not a server failure.
func TestQueryLogsUnbounded(t *testing.T) {
	engine := &fakeLogEngine{err: errors.New("observation: a log query must be bounded — set Tail, a Since/Until time range, or Around (or follow instead)")}
	c := serve(t, Options{Logs: engine})

	_, err := c.logs.QueryLogs(context.Background(), connect.NewRequest(&kelsonv1alpha1.QueryLogsRequest{
		Selector: &kelsonv1alpha1.LogSelector{Namespace: "hello-development", Application: "web"},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
}

// TestQueryLogsAround covers the Around descriptor, the crash-loop diagnosis
// form of the query.
func TestQueryLogsAround(t *testing.T) {
	engine := &fakeLogEngine{}
	c := serve(t, Options{Logs: engine})

	if _, err := c.logs.QueryLogs(context.Background(), connect.NewRequest(&kelsonv1alpha1.QueryLogsRequest{
		Selector: &kelsonv1alpha1.LogSelector{Namespace: "hello-development", Application: "web"},
		Around:   &kelsonv1alpha1.LogAround{Lines: 40, AtTermination: true},
	})); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if engine.query.Around == nil || engine.query.Around.Lines != 40 || !engine.query.Around.AtTermination {
		t.Errorf("around = %+v", engine.query.Around)
	}
}

// TestFollowLogs streams the lines and closes when the source does.
func TestFollowLogs(t *testing.T) {
	engine := &fakeLogEngine{follow: []observation.Line{
		{Pod: "web-1", Container: "web", Message: "one"},
		{Pod: "web-1", Container: "web", Message: "two"},
	}}
	c := serve(t, Options{Logs: engine})

	stream, err := c.logs.FollowLogs(context.Background(), connect.NewRequest(&kelsonv1alpha1.FollowLogsRequest{
		Selector: &kelsonv1alpha1.LogSelector{Namespace: "hello-development", Application: "web"},
		Backlog:  16,
	}))
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	var messages []string
	for stream.Receive() {
		if line := stream.Msg().GetLine(); line != nil {
			messages = append(messages, line.GetMessage())
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(messages) != 2 || messages[0] != "one" || messages[1] != "two" {
		t.Fatalf("messages = %v", messages)
	}
	if engine.query.Backlog != 16 {
		t.Errorf("backlog = %d, want 16", engine.query.Backlog)
	}
}

// TestFollowLogsReportsDrops: loss is reported, never hidden. A client that
// sees no dropped event must be entitled to read a gap as the application
// having gone quiet, so the counter moving has to produce one.
func TestFollowLogsReportsDrops(t *testing.T) {
	engine := &fakeLogEngine{
		follow: []observation.Line{
			{Pod: "web-1", Container: "web", Message: "one"},
			{Pod: "web-1", Container: "web", Message: "two"},
			{Pod: "web-1", Container: "web", Message: "three"},
		},
		// The engine had already dropped 7 lines by the time "one" arrived.
		dropsBefore: map[int]int{0: 7},
	}
	c := serve(t, Options{Logs: engine})

	stream, err := c.logs.FollowLogs(context.Background(), connect.NewRequest(&kelsonv1alpha1.FollowLogsRequest{
		Selector: &kelsonv1alpha1.LogSelector{Namespace: "hello-development", Application: "web"},
	}))
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	var events []string
	var drops []int64
	for stream.Receive() {
		switch e := stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.FollowLogsResponse_Line:
			events = append(events, e.Line.GetMessage())
		case *kelsonv1alpha1.FollowLogsResponse_Dropped:
			events = append(events, "dropped")
			drops = append(drops, e.Dropped)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(drops) != 1 || drops[0] != 7 {
		t.Fatalf("drops = %v, want one cumulative count of 7 (events %v)", drops, events)
	}
	// The report follows the line it was noticed on, so a client can place the
	// gap in the stream rather than only learning a total at the end.
	if len(events) != 4 || events[0] != "one" || events[1] != "dropped" {
		t.Errorf("events = %v, want the drop reported right after the line it was noticed on", events)
	}
}

// TestGetProfile: gaps are data. They travel in the document AND as messages,
// so a client cannot mistake "could not check" for "absent" (#56).
func TestGetProfile(t *testing.T) {
	captured := clusterprofile.ClusterProfile{
		GatewayAPI: &clusterprofile.GatewayAPI{Version: "v1.6.0", Classes: []string{"envoy"}},
		Incomplete: []clusterprofile.Gap{{
			Field:  "certManager.clusterIssuers",
			Reason: "forbidden: needs list on clusterissuers.cert-manager.io",
		}},
	}
	c := serve(t, Options{Profile: fakeProfile(captured)})

	res, err := c.profile.GetProfile(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetProfileRequest{}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}

	// The YAML is the schema of record: it must round-trip back into a profile
	// a later render can consume.
	back, err := clusterprofile.Unmarshal(res.Msg.GetYaml())
	if err != nil {
		t.Fatalf("the served document is not a profile: %v", err)
	}
	if back.GatewayAPI == nil || back.GatewayAPI.Version != "v1.6.0" {
		t.Errorf("gatewayAPI did not round-trip: %+v", back.GatewayAPI)
	}
	if len(back.Incomplete) != 1 {
		t.Errorf("gaps did not survive in the document: %+v", back.Incomplete)
	}

	gaps := res.Msg.GetGaps()
	if len(gaps) != 1 || gaps[0].GetField() != "certManager.clusterIssuers" {
		t.Fatalf("gaps = %+v", gaps)
	}
	if gaps[0].GetReason() == "" {
		t.Error("a gap with no reason is not actionable")
	}
}

// TestGetProfileUnwired: a server with no capture seam says so.
func TestGetProfileUnwired(t *testing.T) {
	c := serve(t, Options{})
	_, err := c.profile.GetProfile(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetProfileRequest{}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented", connect.CodeOf(err))
	}
}

// TestRenderFromCapturedProfile wires the capture seam into a render: the
// profile the server detects is the profile the renderer is told about, which
// is the whole ADR-0001 contract (the renderer never looks one up).
func TestRenderFromCapturedProfile(t *testing.T) {
	captured := clusterprofile.ClusterProfile{
		GatewayAPI: &clusterprofile.GatewayAPI{Version: "v1.6.0", Classes: []string{"envoy"}},
	}
	c := serve(t, Options{Profile: fakeProfile(captured)})

	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     &kelsonv1alpha1.ProfileRef{Profile: &kelsonv1alpha1.ProfileRef_FromCluster{FromCluster: true}},
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Msg.GetErrors()) > 0 {
		t.Fatalf("render against the captured profile reported errors: %v", res.Msg.GetErrors())
	}
	var routes int
	for _, m := range res.Msg.GetManifests() {
		if m.GetKind() == "HTTPRoute" {
			routes++
		}
	}
	if routes == 0 {
		t.Error("the captured Gateway API finding did not reach the render")
	}
}
