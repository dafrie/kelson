package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// The event broker behind EventService.Watch (issue #76).
//
// # Watching is polling, once, on everyone's behalf
//
// kelson has no cluster watch of its own: the delivery adapters answer
// Status and the observation plane answers Evaluate, both by asking. So the
// broker asks — one poll loop per watched (project, environment), shared by
// every watcher of that scope and refcounted, so ten browser tabs on the apps
// list cost one loop per environment rather than ten. It calls exactly what
// DeployService.Status calls, through the same DeliveryConnector and health
// evaluator seams: an event and a Status response can never disagree, because
// one is the diff of the other.
//
// # A cursor is a promise the server can keep
//
// Events live in a bounded in-memory ring with monotonic sequence numbers, and
// a cursor is that sequence plus a nonce minted at startup. Inside the window,
// resumption is exact. Outside it — evicted, or from a previous process — the
// server sends Resync rather than pretending. That asymmetry is deliberate and
// is the same contract Kubernetes offers for "resourceVersion too old": a
// client that believes it saw everything when it did not is worse off than one
// told to relist. Fake durability here would cost a database and still be a
// lie after a restart.

// DefaultWatchInterval is how often each watched scope is re-observed.
//
// Five seconds is chosen against what the poll costs and what it buys: one
// render plus one adapter Status plus one health evaluation per scope, against
// a UI that should feel live. Faster would multiply cluster reads for changes
// nobody can perceive; slower and a deploy landing would look like a hang. The
// deploy stream keeps its own 2s poll (DefaultPollInterval) because a watched
// deployment is an operation in progress, not a background subscription.
const DefaultWatchInterval = 5 * time.Second

// watchRingSize is how many recent events the resume window holds.
//
// 256 is a window, not a log: it is what a client that reconnects within a few
// seconds needs, sized so a burst across many scopes still covers a browser
// refresh. Making it large enough to matter would be an attempt at durability,
// which is what Resync exists to avoid pretending to (see the package note
// above). It doubles as the per-watcher queue bound: a consumer that has
// fallen a whole window behind is exactly a consumer the window cannot serve,
// so the same number decides both.
const watchRingSize = 256

// watchObserveTimeout bounds one observation. Without it an adapter that never
// answers would wedge its poller — and with it a shared poller — for as long
// as the watch lived. It is generous relative to the interval on purpose: a
// slow cluster should delay events, not drop the scope.
const watchObserveTimeout = 30 * time.Second

// Resync reasons. They are prose for a human reading a log; a client branches
// on the Resync message being present, never on this text.
const (
	resyncUnknownCursor = "the cursor was not minted by this server instance (it restarted, or the cursor is not one of ours): relist with Status and keep reading"
	resyncEvicted       = "the cursor is older than the retained event window: relist with Status and keep reading"
	resyncSlowConsumer  = "this stream fell a full event window behind: relist with Status and keep reading"
)

// Scope is one watched (project, environment). It is the unit a poller covers
// and the unit a watcher filters on, because it is the unit that has a
// delivery phase: a project is not a deployable thing, an environment is
// (docs/model.md).
type Scope struct {
	Project     string
	Environment string
}

// HealthState is one workload's observation verdict as the broker remembers
// it: the fields a change is judged on, plus the message that travels with it.
type HealthState struct {
	Resource string
	Code     string
	Healthy  bool
	Message  string
}

// Snapshot is one observation of a scope — what DeployService.Status would
// have answered at that instant, reduced to the fields events are diffed on.
type Snapshot struct {
	Phase    string
	Revision string
	Cause    string
	Health   []HealthState
}

// ObserveFunc observes one scope. [Server.observeScope] is the production
// implementation; it runs the same pipeline Status runs.
type ObserveFunc func(ctx context.Context, scope Scope) (Snapshot, error)

// transition and health are the two payload shapes, kept as plain values so an
// event is immutable and copyable. The wire message is built per watcher at
// send time rather than shared: two streams marshalling one protobuf value
// concurrently would be a data race for no gain, since building it is cheap.
type transition struct {
	Phase         string
	PreviousPhase string
	Revision      string
	Cause         string
}

type health struct {
	Resource     string
	Code         string
	PreviousCode string
	Healthy      bool
	Message      string
}

// event is one published change. Nothing mutates one after publish assigns its
// sequence: the ring holds values, watchers queue values, and the wire message
// is derived.
type event struct {
	seq        uint64
	at         time.Time
	scope      Scope
	typ        kelsonv1alpha1.EventType
	transition transition
	health     health
}

// wire projects the event onto the schema, stamping the cursor that resumes
// after it.
func (e event) wire(cursor string) *kelsonv1alpha1.WatchResponse_Event {
	out := &kelsonv1alpha1.WatchResponse_Event{
		Cursor:      cursor,
		AtUnixMs:    e.at.UnixMilli(),
		Project:     e.scope.Project,
		Environment: e.scope.Environment,
	}
	switch e.typ {
	case kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION:
		out.Payload = &kelsonv1alpha1.WatchResponse_Event_StatusTransition{
			StatusTransition: &kelsonv1alpha1.WatchResponse_StatusTransition{
				Phase:         e.transition.Phase,
				PreviousPhase: e.transition.PreviousPhase,
				Revision:      e.transition.Revision,
				Cause:         e.transition.Cause,
			},
		}
	case kelsonv1alpha1.EventType_EVENT_TYPE_HEALTH_CHANGE:
		out.Payload = &kelsonv1alpha1.WatchResponse_Event_HealthChange{
			HealthChange: &kelsonv1alpha1.WatchResponse_HealthChange{
				Resource:     e.health.Resource,
				Code:         e.health.Code,
				PreviousCode: e.health.PreviousCode,
				Healthy:      e.health.Healthy,
				Message:      e.health.Message,
			},
		}
	case kelsonv1alpha1.EventType_EVENT_TYPE_UNSPECIFIED:
	}
	return out
}

// watcher is one live Watch stream's mailbox.
//
// It owns its own queue rather than sharing the ring's cursor arithmetic: the
// publisher must never block on a slow reader, and it must never silently drop
// either. A queue that reaches the window bound is emptied and the watcher is
// owed a Resync — cheap, and honest about the gap.
type watcher struct {
	scopes map[Scope]bool
	types  map[kelsonv1alpha1.EventType]bool // empty = every type
	limit  int                               // queue bound; the broker's ring size

	mu     sync.Mutex
	queue  []event
	resync string
	notify chan struct{}
}

func newWatcher(scopes []Scope, types []kelsonv1alpha1.EventType, limit int) *watcher {
	w := &watcher{
		scopes: make(map[Scope]bool, len(scopes)),
		types:  make(map[kelsonv1alpha1.EventType]bool, len(types)),
		limit:  limit,
		notify: make(chan struct{}, 1),
	}
	for _, sc := range scopes {
		w.scopes[sc] = true
	}
	for _, t := range types {
		// UNSPECIFIED is not a type anything is published with; treating it as
		// a filter entry would silently select nothing.
		if t != kelsonv1alpha1.EventType_EVENT_TYPE_UNSPECIFIED {
			w.types[t] = true
		}
	}
	return w
}

// wants reports whether this watcher subscribed to the event.
func (w *watcher) wants(ev event) bool {
	if !w.scopes[ev.scope] {
		return false
	}
	return len(w.types) == 0 || w.types[ev.typ]
}

// push queues an event, or converts an overrun into a pending Resync. Called
// with the broker's lock held; it takes only the watcher's own.
func (w *watcher) push(ev event) {
	w.mu.Lock()
	switch {
	case w.resync != "":
		// Already owed a resync. Queueing behind it would suggest the gap has
		// an end the server can name, and it does not.
	case len(w.queue) >= w.limit:
		w.queue = nil
		w.resync = resyncSlowConsumer
	default:
		w.queue = append(w.queue, ev)
	}
	w.mu.Unlock()
	w.signal()
}

// owe records a Resync the watcher must be told about before anything else.
func (w *watcher) owe(reason string) {
	w.mu.Lock()
	w.queue = nil
	w.resync = reason
	w.mu.Unlock()
	w.signal()
}

// replay seeds the queue from the ring. It skips the overrun check: the ring
// is the queue bound, so a full replay cannot exceed it.
func (w *watcher) replay(events []event) {
	w.mu.Lock()
	for _, ev := range events {
		if w.wants(ev) {
			w.queue = append(w.queue, ev)
		}
	}
	pending := len(w.queue) > 0
	w.mu.Unlock()
	if pending {
		w.signal()
	}
}

func (w *watcher) signal() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// take drains everything pending. A non-empty reason is delivered first and
// never accompanies queued events — the resync IS the statement that what
// would have been queued is gone.
func (w *watcher) take() (string, []event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	reason, queue := w.resync, w.queue
	w.resync, w.queue = "", nil
	return reason, queue
}

// poller is one scope's shared poll loop and its reference count.
type poller struct {
	refs   int
	cancel context.CancelFunc
	done   chan struct{}
}

// broker fans scope observations out to watchers.
type broker struct {
	observe  ObserveFunc
	interval time.Duration
	ring     int
	nonce    string
	now      func() time.Time

	mu       sync.Mutex
	seq      uint64
	events   []event // oldest first; at most ring entries
	watchers map[*watcher]struct{}
	pollers  map[Scope]*poller
}

func newBroker(observe ObserveFunc, interval time.Duration, ring int) *broker {
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	if ring <= 0 {
		ring = watchRingSize
	}
	return &broker{
		observe:  observe,
		interval: interval,
		ring:     ring,
		nonce:    instanceNonce(),
		now:      time.Now,
		watchers: map[*watcher]struct{}{},
		pollers:  map[Scope]*poller{},
	}
}

// instanceNonce identifies this process's cursor space. A restart mints a new
// one, which is precisely how a client learns that its cursor cannot be
// honoured — the alternative, reusing sequence numbers across processes, would
// hand back events from a different history under the same name.
func instanceNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it somehow
		// did, a clock-derived nonce still separates this process's cursor
		// space from the last one's, which is all the nonce has to do.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// cursor encodes a sequence. The format is the server's business: the schema
// says opaque and a client that parses one is relying on something that may
// change.
func (b *broker) cursor(seq uint64) string {
	return b.nonce + "." + strconv.FormatUint(seq, 10)
}

func splitCursor(s string) (nonce string, seq uint64, ok bool) {
	nonce, rest, found := strings.Cut(s, ".")
	if !found {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return nonce, seq, true
}

// subscribe registers a watcher, resolves its resume point and starts (or
// joins) a poller for each of its scopes. The returned watcher must be handed
// back to unsubscribe.
func (b *broker) subscribe(scopes []Scope, types []kelsonv1alpha1.EventType, cursor string) *watcher {
	w := newWatcher(scopes, types, b.ring)

	// Seeding the queue and registering for new events happen under one lock.
	// Split them and a change published in between would be delivered ahead of
	// the replay it comes after, which is the one thing a resume must not do.
	b.mu.Lock()
	missed, reason := b.resume(cursor)
	switch {
	case reason != "":
		w.owe(reason)
	case len(missed) > 0:
		w.replay(missed)
	}
	b.watchers[w] = struct{}{}
	for _, sc := range scopes {
		b.acquire(sc)
	}
	b.mu.Unlock()
	return w
}

// resume answers what a cursor is owed: the events after it, or a reason it
// cannot be honoured. Called with b.mu held.
func (b *broker) resume(cursor string) ([]event, string) {
	if cursor == "" {
		return nil, ""
	}
	nonce, seq, ok := splitCursor(cursor)
	if !ok || nonce != b.nonce {
		return nil, resyncUnknownCursor
	}
	// The retained window is [seq-len+1, seq]. A cursor at or after
	// seq-len still has every successor retained; anything older lost some.
	// A cursor ahead of the sequence is not from this history either.
	if seq > b.seq || seq < b.seq-uint64(len(b.events)) {
		return nil, resyncEvicted
	}
	missed := make([]event, 0, len(b.events))
	for _, ev := range b.events {
		if ev.seq > seq {
			missed = append(missed, ev)
		}
	}
	return missed, ""
}

// acquire starts a poller for the scope or joins the running one. Called with
// b.mu held.
func (b *broker) acquire(sc Scope) {
	if p, ok := b.pollers[sc]; ok {
		p.refs++
		return
	}
	// The poller outlives the request that started it — the next watcher of
	// this scope inherits it — so it hangs off Background rather than off one
	// client's context.
	ctx, cancel := context.WithCancel(context.Background())
	p := &poller{refs: 1, cancel: cancel, done: make(chan struct{})}
	b.pollers[sc] = p
	go b.poll(ctx, sc, p)
}

// unsubscribe removes a watcher and stops any poller it was the last user of.
// It returns once those pollers have exited, so a caller (and a test) knows
// nothing is left running.
func (b *broker) unsubscribe(w *watcher) {
	b.mu.Lock()
	delete(b.watchers, w)
	var stopped []*poller
	for sc := range w.scopes {
		p, ok := b.pollers[sc]
		if !ok {
			continue
		}
		p.refs--
		if p.refs <= 0 {
			p.cancel()
			delete(b.pollers, sc)
			stopped = append(stopped, p)
		}
	}
	b.mu.Unlock()

	// Waiting outside the lock is required, not tidiness: a poller mid-publish
	// is blocked on b.mu and would never reach its own cancellation.
	for _, p := range stopped {
		<-p.done
	}
}

// poll observes one scope until its context is cancelled, publishing what
// changed.
//
// The first observation only seeds the baseline. A watcher gets the current
// state from its own Status call — that is what the "relist, then watch"
// contract means — so replaying it as an event would announce a change that
// did not happen. A failed observation leaves the baseline untouched for the
// same reason: an unreachable cluster is not a transition, and treating it as
// one would emit a spurious pair of events when it came back.
func (b *broker) poll(ctx context.Context, sc Scope, p *poller) {
	defer close(p.done)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	var last *Snapshot
	for {
		if snap, err := b.observeOnce(ctx, sc); err == nil {
			if last != nil {
				b.publish(diffSnapshot(sc, *last, snap, b.now()))
			}
			last = &snap
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *broker) observeOnce(ctx context.Context, sc Scope) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, watchObserveTimeout)
	defer cancel()
	return b.observe(ctx, sc)
}

// publish assigns sequences, retains the events in the ring and fans them out.
func (b *broker) publish(events []event) {
	if len(events) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range events {
		b.seq++
		ev.seq = b.seq
		b.events = append(b.events, ev)
		if len(b.events) > b.ring {
			// Re-slicing forward is amortised: append reallocates once the
			// backing array is exhausted, copying only the live window.
			b.events = b.events[1:]
		}
		for w := range b.watchers {
			if w.wants(ev) {
				w.push(ev)
			}
		}
	}
}

// diffSnapshot is what "changed" means, and it is the whole taxonomy.
//
// A transition is a change of phase or revision: a redeploy of the same phase
// is still news, and a phase change without one is too. Health changes are
// per-resource and judged on code and healthiness alone — the message is
// carried, never compared, because adapter prose moves for reasons that are
// not changes (replica counts, timestamps) and a stream that fired on those
// would be a heartbeat with extra steps.
//
// A workload that disappears from the render emits nothing: "no longer part of
// this spec" is not a health verdict, and inventing one would put a claim on
// the wire that no probe made.
func diffSnapshot(sc Scope, prev, next Snapshot, at time.Time) []event {
	var out []event
	if prev.Phase != next.Phase || prev.Revision != next.Revision {
		out = append(out, event{
			at:    at,
			scope: sc,
			typ:   kelsonv1alpha1.EventType_EVENT_TYPE_STATUS_TRANSITION,
			transition: transition{
				Phase:         next.Phase,
				PreviousPhase: prev.Phase,
				Revision:      next.Revision,
				Cause:         next.Cause,
			},
		})
	}

	previous := make(map[string]HealthState, len(prev.Health))
	for _, h := range prev.Health {
		previous[h.Resource] = h
	}
	for _, h := range next.Health {
		was, known := previous[h.Resource]
		if known && was.Code == h.Code && was.Healthy == h.Healthy {
			continue
		}
		out = append(out, event{
			at:    at,
			scope: sc,
			typ:   kelsonv1alpha1.EventType_EVENT_TYPE_HEALTH_CHANGE,
			health: health{
				Resource:     h.Resource,
				Code:         h.Code,
				PreviousCode: was.Code,
				Healthy:      h.Healthy,
				Message:      h.Message,
			},
		})
	}
	return out
}

// activePollers reports how many poll loops are running. It exists for the
// tests: "the last watcher left and the goroutine stopped" is a property worth
// asserting directly rather than inferring from a goroutine count.
func (b *broker) activePollers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pollers)
}
