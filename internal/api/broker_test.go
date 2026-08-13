package api

import (
	"context"
	"sync"
	"testing"
	"time"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// The broker is the concurrent part of this package, so the tests avoid the
// two things that make concurrency tests lie: they never sleep to "let it
// settle", and they never assert an absence by waiting. "No event was emitted
// for that change" is asserted by emitting a *later* change and checking that
// the first event to arrive is the later one — which is a fact about ordering,
// not about timing.

var (
	devScope     = Scope{Project: "hello", Environment: "development"}
	stagingScope = Scope{Project: "hello", Environment: "staging"}
)

// script is a scripted ObserveFunc: each scope has a queue of snapshots and the
// last one repeats forever, exactly like fakeAdapter's statuses.
type script struct {
	mu    sync.Mutex
	snaps map[Scope][]Snapshot
}

func newScript() *script { return &script{snaps: map[Scope][]Snapshot{}} }

func (s *script) set(sc Scope, snaps ...Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps[sc] = snaps
}

func (s *script) observe(_ context.Context, sc Scope) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.snaps[sc]
	if len(list) == 0 {
		return Snapshot{}, nil
	}
	if len(list) > 1 {
		s.snaps[sc] = list[1:]
	}
	return list[0], nil
}

func phase(p string) Snapshot { return Snapshot{Phase: p} }

// drain collects n events, failing the test on a resync or a stall.
func drain(t *testing.T, w *watcher, n int) []event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var got []event
	for len(got) < n {
		select {
		case <-w.notify:
			reason, batch := w.take()
			if reason != "" {
				t.Fatalf("unexpected resync %q after %d events", reason, len(got))
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatalf("timed out with %d of %d events", len(got), n)
		}
	}
	return got
}

// awaitResync waits for the watcher to be owed one.
func awaitResync(t *testing.T, w *watcher) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-w.notify:
			reason, batch := w.take()
			if reason != "" {
				return reason
			}
			t.Fatalf("got %d events, want a resync", len(batch))
		case <-deadline:
			t.Fatal("timed out waiting for a resync")
		}
	}
}

// TestBrokerEmitsTransitionAndHealthChange: the two event types the taxonomy
// has today, from one observation to the next.
func TestBrokerEmitsTransitionAndHealthChange(t *testing.T) {
	s := newScript()
	s.set(devScope,
		Snapshot{Phase: "Reconciling", Revision: "rev-1", Health: []HealthState{
			{Resource: "Deployment/dev/web", Code: "progressing"},
		}},
		Snapshot{Phase: "Healthy", Revision: "rev-1", Cause: "3/3 replicas ready", Health: []HealthState{
			{Resource: "Deployment/dev/web", Code: "healthy", Healthy: true, Message: "Deployment/dev/web healthy"},
		}},
	)
	b := newBroker(s.observe, time.Millisecond, 32)
	w := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(w)

	got := drain(t, w, 2)
	tr := got[0]
	if tr.typ != kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION {
		t.Fatalf("first event type = %v, want a status transition", tr.typ)
	}
	if tr.scope != devScope {
		t.Errorf("scope = %+v, want %+v", tr.scope, devScope)
	}
	if tr.transition.Phase != "Healthy" || tr.transition.PreviousPhase != "Reconciling" {
		t.Errorf("transition = %+v, want Reconciling -> Healthy", tr.transition)
	}
	if tr.transition.Revision != "rev-1" || tr.transition.Cause != "3/3 replicas ready" {
		t.Errorf("transition = %+v, want the revision and cause carried", tr.transition)
	}

	hc := got[1]
	if hc.typ != kelsonv1alpha1.EventType_EVENT_TYPE_HEALTH_CHANGE {
		t.Fatalf("second event type = %v, want a health change", hc.typ)
	}
	if hc.health.Resource != "Deployment/dev/web" || hc.health.Code != "healthy" || hc.health.PreviousCode != "progressing" {
		t.Errorf("health = %+v, want progressing -> healthy on the web deployment", hc.health)
	}
	if !hc.health.Healthy {
		t.Error("health change did not carry the healthy verdict")
	}
	// The cursors are monotonic within one instance, which is what makes a
	// resume point meaningful.
	if tr.seq >= hc.seq {
		t.Errorf("sequences %d, %d are not increasing", tr.seq, hc.seq)
	}
}

// TestBrokerDedupes: an observation identical to the last is not news.
//
// The assertion is ordering, not timing: the scope is observed as A, A, A, then
// B, and the first event to arrive must be the A -> B transition. An A -> A
// event would have arrived first and failed this.
func TestBrokerDedupes(t *testing.T) {
	s := newScript()
	s.set(devScope,
		Snapshot{Phase: "Healthy", Revision: "rev-1", Health: []HealthState{{Resource: "Deployment/dev/web", Code: "healthy", Healthy: true}}},
		Snapshot{Phase: "Healthy", Revision: "rev-1", Health: []HealthState{{Resource: "Deployment/dev/web", Code: "healthy", Healthy: true}}},
		// Same verdict, different prose: the message is carried, never
		// compared, so this is still not an event.
		Snapshot{Phase: "Healthy", Revision: "rev-1", Health: []HealthState{{Resource: "Deployment/dev/web", Code: "healthy", Healthy: true, Message: "3/3 ready"}}},
		Snapshot{Phase: "Degraded", Revision: "rev-1", Health: []HealthState{{Resource: "Deployment/dev/web", Code: "crash-loop-back-off"}}},
	)
	b := newBroker(s.observe, time.Millisecond, 32)
	w := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(w)

	got := drain(t, w, 1)
	if got[0].transition.Phase != "Degraded" {
		t.Fatalf("first event = %+v, want the Healthy -> Degraded transition: an unchanged observation was emitted", got[0])
	}
	if got[0].transition.PreviousPhase != "Healthy" {
		t.Errorf("previous phase = %q, want Healthy", got[0].transition.PreviousPhase)
	}
}

// TestBrokerFansOutToEveryWatcher: one poller, many readers. Each watcher gets
// its own copy — a slow one must not be able to affect what a fast one sees.
func TestBrokerFansOutToEveryWatcher(t *testing.T) {
	s := newScript()
	s.set(devScope, phase("Reconciling"), phase("Healthy"))
	b := newBroker(s.observe, time.Millisecond, 32)

	first := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(first)
	second := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(second)

	if n := b.activePollers(); n != 1 {
		t.Fatalf("pollers = %d, want 1 shared by both watchers", n)
	}
	for _, w := range []*watcher{first, second} {
		got := drain(t, w, 1)
		if got[0].transition.Phase != "Healthy" {
			t.Errorf("watcher saw %+v, want the Healthy transition", got[0].transition)
		}
	}
}

// TestBrokerPollerRefcount: the last watcher of a scope leaving stops its poll
// loop, and unsubscribe does not return until it has.
func TestBrokerPollerRefcount(t *testing.T) {
	s := newScript()
	s.set(devScope, phase("Healthy"))
	s.set(stagingScope, phase("Healthy"))
	b := newBroker(s.observe, time.Millisecond, 32)

	first := b.subscribe([]Scope{devScope, stagingScope}, nil, "")
	if n := b.activePollers(); n != 2 {
		t.Fatalf("pollers = %d, want one per scope", n)
	}
	second := b.subscribe([]Scope{devScope}, nil, "")
	if n := b.activePollers(); n != 2 {
		t.Fatalf("pollers = %d after a second watcher joined development, want 2", n)
	}

	b.unsubscribe(first)
	if n := b.activePollers(); n != 1 {
		t.Fatalf("pollers = %d, want only development's — staging lost its last watcher", n)
	}
	b.unsubscribe(second)
	if n := b.activePollers(); n != 0 {
		t.Fatalf("pollers = %d after the last watcher left, want 0", n)
	}
}

// TestBrokerFiltersByScopeAndType: a watcher receives what it subscribed to and
// nothing else. Both absences are asserted by ordering — the event that must
// arrive is published after the ones that must not.
func TestBrokerFiltersByScopeAndType(t *testing.T) {
	s := newScript()
	s.set(devScope, phase("Healthy"))
	b := newBroker(s.observe, time.Hour, 32)

	scoped := b.subscribe([]Scope{stagingScope}, nil, "")
	defer b.unsubscribe(scoped)
	typed := b.subscribe([]Scope{devScope},
		[]kelsonv1alpha1.EventType{kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION}, "")
	defer b.unsubscribe(typed)

	b.publish([]event{
		{scope: devScope, typ: kelsonv1alpha1.EventType_EVENT_TYPE_HEALTH_CHANGE, health: health{Resource: "Deployment/dev/web", Code: "crash-loop-back-off"}},
		{scope: devScope, typ: kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION, transition: transition{Phase: "Degraded"}},
		{scope: stagingScope, typ: kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION, transition: transition{Phase: "Healthy"}},
	})

	got := drain(t, typed, 1)
	if got[0].typ != kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION {
		t.Errorf("a type-filtered watcher received %v", got[0].typ)
	}
	got = drain(t, scoped, 1)
	if got[0].scope != stagingScope {
		t.Errorf("a scope-filtered watcher received %+v", got[0].scope)
	}
}

// TestBrokerResumesWithinTheWindow: a cursor inside the ring replays everything
// after it, in order, and nothing before it.
func TestBrokerResumesWithinTheWindow(t *testing.T) {
	s := newScript()
	s.set(devScope, phase("Healthy"))
	b := newBroker(s.observe, time.Hour, 8)

	seed := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(seed)
	for _, p := range []string{"Proposed", "Committed", "Reconciling", "Healthy"} {
		b.publish([]event{{scope: devScope, typ: kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION, transition: transition{Phase: p}}})
	}
	published := drain(t, seed, 4)

	resumed := b.subscribe([]Scope{devScope}, nil, b.cursor(published[1].seq))
	defer b.unsubscribe(resumed)
	got := drain(t, resumed, 2)
	if got[0].transition.Phase != "Reconciling" || got[1].transition.Phase != "Healthy" {
		t.Fatalf("replay = %q, %q, want Reconciling then Healthy", got[0].transition.Phase, got[1].transition.Phase)
	}
}

// TestBrokerEvictedCursorResyncs: past the window the server says so rather
// than guessing. Fake durability is the failure this design refuses.
func TestBrokerEvictedCursorResyncs(t *testing.T) {
	s := newScript()
	s.set(devScope, phase("Healthy"))
	b := newBroker(s.observe, time.Hour, 4)

	// Drained as they are published: this watcher is keeping up, so the only
	// thing the window can be too small for is the cursor below.
	seed := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(seed)
	var published []event
	for i := 0; i < 6; i++ {
		b.publish([]event{{scope: devScope, typ: kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION}})
		published = append(published, drain(t, seed, 1)...)
	}

	stale := b.subscribe([]Scope{devScope}, nil, b.cursor(published[0].seq))
	defer b.unsubscribe(stale)
	if reason := awaitResync(t, stale); reason != resyncEvicted {
		t.Errorf("reason = %q, want the eviction reason", reason)
	}
}

// TestBrokerForeignCursorResyncs: a cursor from another server instance — which
// is what a restart looks like — cannot be honoured, and neither can a cursor
// that is not one of ours at all.
func TestBrokerForeignCursorResyncs(t *testing.T) {
	s := newScript()
	s.set(devScope, phase("Healthy"))
	b := newBroker(s.observe, time.Hour, 8)

	for _, cursor := range []string{"0011223344556677.3", "not-a-cursor", ".1"} {
		w := b.subscribe([]Scope{devScope}, nil, cursor)
		if reason := awaitResync(t, w); reason != resyncUnknownCursor {
			t.Errorf("cursor %q: reason = %q, want the unknown-instance reason", cursor, reason)
		}
		b.unsubscribe(w)
	}

	// A cursor ahead of this instance's sequence is not from this history
	// either, however well-formed it looks.
	ahead := b.subscribe([]Scope{devScope}, nil, b.cursor(99))
	defer b.unsubscribe(ahead)
	if reason := awaitResync(t, ahead); reason != resyncEvicted {
		t.Errorf("reason = %q, want the eviction reason for a cursor from the future", reason)
	}
}

// TestBrokerSlowConsumerResyncs: a reader that falls a whole window behind gets
// told, rather than silently losing the middle of its stream.
func TestBrokerSlowConsumerResyncs(t *testing.T) {
	s := newScript()
	s.set(devScope, phase("Healthy"))
	b := newBroker(s.observe, time.Hour, 4)

	slow := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(slow)
	for i := 0; i < 5; i++ {
		b.publish([]event{{scope: devScope, typ: kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION}})
	}

	if reason := awaitResync(t, slow); reason != resyncSlowConsumer {
		t.Errorf("reason = %q, want the slow-consumer reason", reason)
	}
	// After the resync the stream continues from now: the watcher is live
	// again rather than wedged owing a resync forever.
	b.publish([]event{{scope: devScope, typ: kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION, transition: transition{Phase: "Healthy"}}})
	if got := drain(t, slow, 1); got[0].transition.Phase != "Healthy" {
		t.Errorf("after a resync the watcher received %+v", got[0].transition)
	}
}

// TestBrokerFailedObservationIsNotATransition: an unreachable cluster is not a
// change. The baseline survives it, so recovery emits the real transition
// rather than a spurious pair.
func TestBrokerFailedObservationIsNotATransition(t *testing.T) {
	var calls int
	observe := func(context.Context, Scope) (Snapshot, error) {
		calls++
		switch calls {
		case 1:
			return phase("Healthy"), nil
		case 2, 3:
			return Snapshot{}, context.DeadlineExceeded
		default:
			return phase("Degraded"), nil
		}
	}
	b := newBroker(observe, time.Millisecond, 32)
	w := b.subscribe([]Scope{devScope}, nil, "")
	defer b.unsubscribe(w)

	got := drain(t, w, 1)
	if got[0].transition.PreviousPhase != "Healthy" || got[0].transition.Phase != "Degraded" {
		t.Fatalf("transition = %+v, want Healthy -> Degraded across the failed observations", got[0].transition)
	}
}

// TestObserveScopeMirrorsStatus: the production observer reads the same two
// seams DeployService.Status reads, and nothing else.
func TestObserveScopeMirrorsStatus(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase:    delivery.PhaseDegraded,
		Revision: "rev-00000007",
		Cause:    "web: 1 of 3 replicas are not ready",
	}}
	connector, _ := connectorFor(adapter, nil, fakeEvaluator{"web": {
		Resource: "Deployment/hello-development/web",
		Code:     observation.CodeCrashLoopBackOff,
		Reason:   "back-off restarting failed container",
	}})
	s := New(Options{Specs: seededStore(t), Delivery: connector})

	snap, err := s.observeScope(context.Background(), devScope)
	if err != nil {
		t.Fatalf("observeScope: %v", err)
	}
	if snap.Phase != string(delivery.PhaseDegraded) || snap.Revision != "rev-00000007" {
		t.Errorf("snapshot = %+v, want the adapter's phase and revision", snap)
	}
	if snap.Cause != "web: 1 of 3 replicas are not ready" {
		t.Errorf("cause = %q", snap.Cause)
	}
	if len(snap.Health) != 1 {
		t.Fatalf("health = %+v, want one verdict for the rendered Deployment", snap.Health)
	}
	if snap.Health[0].Code != string(observation.CodeCrashLoopBackOff) || snap.Health[0].Healthy {
		t.Errorf("verdict = %+v, want the evaluator's crash-loop verdict", snap.Health[0])
	}
}
