package api

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/observation"
)

// The Watch tests here run over a real HTTP server and the generated Connect
// client, like the other handler tests: what they prove that broker_test.go
// cannot is that the stream framing, the oneof and the cursor survive the
// round trip.
//
// The environment document declares no routing. The broker renders a stored
// spec with no ClusterProfile — exactly what a client calling Status without
// one gets — and since #140 a declared domain with no Gateway API is a render
// error, so a spec with domains would be a test about #140 rather than about
// events.
const watchEnvironmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development
spec:
  project: hello
`

// seededStore is a spec store holding one project with one environment.
func seededStore(t *testing.T) *fakeSpecStore {
	t.Helper()
	store := newFakeSpecStore()
	_, err := store.Put(context.Background(), "hello", controlstore.Documents{
		Project:      []byte(projectDoc),
		Environments: map[string][]byte{"development": []byte(watchEnvironmentDoc)},
	}, controlstore.PutOptions{})
	if err != nil {
		t.Fatalf("seeding the spec store: %v", err)
	}
	return store
}

// watchOptions is a server whose broker polls fast enough for a test, over the
// given health evaluator.
//
// It used to walk an adapter through a list of delivery statuses, because the
// broker diffed on the phase as well as on health. ADR-0028 deleted the source
// of the phase (internal/api/events.go says so at the observer), so health is
// what a watcher sees change.
func watchOptions(store *fakeSpecStore, health observation.Evaluator) Options {
	connector, _ := connectorFor(health)
	return Options{
		Specs:         store,
		Delivery:      connector,
		WatchInterval: 2 * time.Millisecond,
	}
}

func watchRequest(cursor string, types ...kelsonv1alpha1.EventType) *kelsonv1alpha1.WatchRequest {
	return &kelsonv1alpha1.WatchRequest{
		Scopes: []*kelsonv1alpha1.WatchRequest_Scope{{Project: "hello", Environment: "development"}},
		Types:  types,
		Cursor: cursor,
	}
}

// nextEvent reads one message, failing on a resync or a closed stream.
func nextEvent(t *testing.T, stream *connect.ServerStreamForClient[kelsonv1alpha1.WatchResponse]) *kelsonv1alpha1.WatchResponse_Event {
	t.Helper()
	if !stream.Receive() {
		t.Fatalf("stream ended before an event arrived: %v", stream.Err())
	}
	msg := stream.Msg()
	if r := msg.GetResync(); r != nil {
		t.Fatalf("unexpected resync: %s", r.GetReason())
	}
	ev := msg.GetEvent()
	if ev == nil {
		t.Fatal("a WatchResponse carried neither an event nor a resync")
	}
	return ev
}

// TestWatchStreamsHealthChange is the #76 smoke test: a change observed by the
// broker arrives on the wire as a typed event with a cursor.
//
// It watched a *phase* change until ADR-0028 deleted the source of the phase;
// health is what the broker can still see move, and the wire mechanics being
// asserted — typing, cursors, timestamps — are the same either way.
func TestWatchStreamsHealthChange(t *testing.T) {
	c := serve(t, watchOptions(seededStore(t), &steppingEvaluator{}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.events.Watch(ctx, connect.NewRequest(watchRequest("")))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the test is done with the stream

	ev := nextEvent(t, stream)
	if ev.GetProject() != "hello" || ev.GetEnvironment() != "development" {
		t.Errorf("event addressed %s/%s", ev.GetProject(), ev.GetEnvironment())
	}
	hc := ev.GetHealthChange()
	if hc == nil {
		t.Fatalf("event payload = %T, want a health change", ev.GetPayload())
	}
	if hc.GetCode() != "crash-loop-back-off" || hc.GetHealthy() {
		t.Errorf("health change = %+v, want the crash-loop verdict", hc)
	}
	if hc.GetPreviousCode() != "healthy" {
		t.Errorf("previous code = %q, want healthy", hc.GetPreviousCode())
	}
	if ev.GetCursor() == "" {
		t.Error("an event carried no cursor, so nothing can resume after it")
	}
	if ev.GetAtUnixMs() == 0 {
		t.Error("an event carried no timestamp")
	}
}

// TestWatchResumesFromCursor: a second watch handed the first one's cursor
// replays what it missed, in order, before anything new.
func TestWatchResumesFromCursor(t *testing.T) {
	c := serve(t, watchOptions(seededStore(t), &cyclingEvaluator{}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := c.events.Watch(ctx, connect.NewRequest(watchRequest("")))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer first.Close() //nolint:errcheck // the test is done with the stream

	var cursor string
	var codes []string
	for i := 0; i < 3; i++ {
		ev := nextEvent(t, first)
		if i == 0 {
			cursor = ev.GetCursor()
		} else {
			codes = append(codes, ev.GetHealthChange().GetCode())
		}
	}

	resumed, err := c.events.Watch(ctx, connect.NewRequest(watchRequest(cursor)))
	if err != nil {
		t.Fatalf("Watch(cursor): %v", err)
	}
	defer resumed.Close() //nolint:errcheck // the test is done with the stream

	for i, want := range codes {
		got := nextEvent(t, resumed).GetHealthChange().GetCode()
		if got != want {
			t.Fatalf("replayed event %d = %q, want %q (the replay is out of order or incomplete)", i, got, want)
		}
	}
}

// TestWatchStaleCursorResyncs: a cursor this instance did not mint gets a
// Resync as the FIRST message — the client must relist before it believes
// anything that follows.
func TestWatchStaleCursorResyncs(t *testing.T) {
	c := serve(t, watchOptions(seededStore(t), &steppingEvaluator{}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.events.Watch(ctx, connect.NewRequest(watchRequest("someotherserver.42")))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the test is done with the stream

	if !stream.Receive() {
		t.Fatalf("stream ended before the resync: %v", stream.Err())
	}
	resync := stream.Msg().GetResync()
	if resync == nil {
		t.Fatalf("first message = %T, want a resync", stream.Msg().GetBody())
	}
	if resync.GetReason() == "" {
		t.Error("the resync named no reason")
	}
	// And the stream keeps going from now: relisting is the client's job, not
	// reconnecting.
	if hc := nextEvent(t, stream).GetHealthChange(); hc == nil {
		t.Errorf("after the resync the stream carried %T", nextEvent(t, stream).GetPayload())
	}
}

// TestWatchFiltersByType: a client that asked only for health changes gets
// health changes.
//
// The complement — that a status transition is dropped — cannot be exercised
// while nothing produces one (ADR-0028; the phase returns with issue #224). The
// filter itself is unchanged and the type is still on the wire, so what this
// still proves is that the filter does not drop the type it was asked for,
// which is the half that would break a client.
func TestWatchFiltersByType(t *testing.T) {
	store := seededStore(t)
	connector, _ := connectorFor(&steppingEvaluator{})
	c := serve(t, Options{Specs: store, Delivery: connector, WatchInterval: 2 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.events.Watch(ctx, connect.NewRequest(
		watchRequest("", kelsonv1alpha1.EventType_EVENT_TYPE_HEALTH_CHANGE)))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the test is done with the stream

	ev := nextEvent(t, stream)
	hc := ev.GetHealthChange()
	if hc == nil {
		t.Fatalf("event payload = %T, want a health change: the type filter let a transition through", ev.GetPayload())
	}
	if hc.GetCode() != "crash-loop-back-off" || hc.GetHealthy() {
		t.Errorf("health change = %+v, want the crash-loop verdict", hc)
	}
	if hc.GetPreviousCode() != "healthy" {
		t.Errorf("previous code = %q, want healthy", hc.GetPreviousCode())
	}
}

// TestWatchScopeResolution: an empty scope list is every stored project, a
// project with no environment named is all of its environments, and both are
// resolved once at the start of the watch.
func TestWatchScopeResolution(t *testing.T) {
	store := newFakeSpecStore()
	ctx := context.Background()
	for _, project := range []string{"hello", "checkout"} {
		if _, err := store.Put(ctx, project, controlstore.Documents{
			Project: []byte(projectDoc),
			Environments: map[string][]byte{
				"development": []byte(watchEnvironmentDoc),
				"production":  []byte(watchEnvironmentDoc),
			},
		}, controlstore.PutOptions{}); err != nil {
			t.Fatalf("seeding %s: %v", project, err)
		}
	}
	s := New(Options{Specs: store})

	all, err := s.watchScopes(ctx, nil)
	if err != nil {
		t.Fatalf("watchScopes(nil): %v", err)
	}
	want := []Scope{
		{Project: "checkout", Environment: "development"},
		{Project: "checkout", Environment: "production"},
		{Project: "hello", Environment: "development"},
		{Project: "hello", Environment: "production"},
	}
	if len(all) != len(want) {
		t.Fatalf("scopes = %+v, want every stored (project, environment)", all)
	}
	for i, sc := range want {
		if all[i] != sc {
			t.Fatalf("scopes = %+v, want %+v", all, want)
		}
	}

	wide, err := s.watchScopes(ctx, []*kelsonv1alpha1.WatchRequest_Scope{{Project: "hello"}})
	if err != nil {
		t.Fatalf("watchScopes(project only): %v", err)
	}
	if len(wide) != 2 || wide[0].Environment != "development" || wide[1].Environment != "production" {
		t.Errorf("scopes = %+v, want both of hello's environments", wide)
	}

	if _, err := s.watchScopes(ctx, []*kelsonv1alpha1.WatchRequest_Scope{{Environment: "development"}}); err == nil {
		t.Error("a scope naming no project was accepted; it addresses nothing")
	}
}

// TestWatchStopsPollingWhenTheLastClientLeaves: the stream is what keeps a poll
// loop alive, so a server nobody watches costs nothing.
func TestWatchStopsPollingWhenTheLastClientLeaves(t *testing.T) {
	srv := New(watchOptions(seededStore(t), &steppingEvaluator{}))
	c := serveServer(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.events.Watch(ctx, connect.NewRequest(watchRequest("")))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	nextEvent(t, stream)
	if n := srv.events.activePollers(); n != 1 {
		t.Fatalf("pollers = %d while a client is watching, want 1", n)
	}

	cancel()
	_ = stream.Close()
	deadline := time.After(5 * time.Second)
	for srv.events.activePollers() != 0 {
		select {
		case <-deadline:
			t.Fatal("the poller outlived its last watcher")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestWatchUnimplementedWithoutSeams: a partially-wired server says what is
// missing rather than streaming nothing.
func TestWatchUnimplementedWithoutSeams(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore()})
	stream, err := c.events.Watch(context.Background(), connect.NewRequest(watchRequest("")))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the test is done with the stream
	if stream.Receive() {
		t.Fatal("a server with no delivery plane streamed an event")
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeUnimplemented {
		t.Errorf("code = %v, want Unimplemented (err %v)", code, stream.Err())
	}
}
