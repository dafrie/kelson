// Package statemachine owns the deployment state machine (issue #37): the
// piece that answers "is my change live?" for delivery modes where kelson does
// NOT own the apply step.
//
// # Why this is not a spinner
//
// In Git modes kelson commits and a foreign reconciler (Flux, Argo) applies.
// Between "committed" and "healthy" three very different things can happen, and
// conflating them is the single worst failure mode of a PaaS layered on GitOps:
//
//  1. the reconciler has not picked the commit up yet — keep waiting, and after
//     a timeout report a stuck state naming why (the classic cause: kelson
//     wrote to a path nothing is watching);
//  2. the reconciler picked it up and refused it — Rejected, with the cause;
//  3. it is live and wrong — Degraded, with health detail.
//
// The engine keeps those apart by construction: see State.Answer.
//
// # Shape
//
// The engine is driven by an event stream (a Source, see source.go), never by
// polling: adapters push observations as their watches fire. The only timer in
// the engine is the progress timeout. Observations are correlated to the target
// revision through kelson.dev/revision and kelson.dev/spec-hash provenance, so
// a reconciler still reporting on the previous revision reads as "not picked up
// yet" rather than as progress.
//
// Adapters report observations; this package owns the transitions. An
// observation that implies an impossible transition is a programming error in
// the adapter and is surfaced loudly (InvalidTransitionError), never ignored.
package statemachine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dafrie/kelson/internal/delivery"
)

// Provenance keys the engine correlates on (docs/architecture.md). Adapters
// carry the observed values in delivery.Status.Revision and, when the observed
// object exposes it, in Status.Detail under these keys.
const (
	AnnotationRevision = "kelson.dev/revision"
	AnnotationSpecHash = "kelson.dev/spec-hash"
)

// DefaultTimeout is the progress timeout used when Config.Timeout is zero: the
// wall-clock budget for ONE phase change, not for the whole rollout.
const DefaultTimeout = 5 * time.Minute

// DefaultComponent names the responsible component in engine-generated causes
// when the caller does not set Config.Component.
const DefaultComponent = "reconciler"

// deliveryDocsBase mirrors the unexported base in internal/delivery/errors.go.
// Kept in sync by the shared error-code vocabulary, not by import, so this
// package stays a leaf of the delivery plane.
const deliveryDocsBase = "https://kelson.dev/delivery/errors"

// ErrSourceStopped is returned by Run when the status stream ends before the
// deployment settles. A watch that closes early is a delivery failure, not a
// result: the answer is unknown, and unknown must not read as healthy.
var ErrSourceStopped = errors.New("statemachine: status source stopped before the deployment settled")

// transitions is the allowed-transition table.
//
//	Proposed ──► Committed ──► Reconciling ──► Applied ──► Healthy
//	    │            │              │              │          │
//	    └──► Rejected◄┘             ├──► Rejected  └──► Degraded ◄┘
//	                                └──► Degraded ──► Healthy / Applied / Reconciling
//
// Three rules generate it:
//
//   - forward only. Skips are legal because an observation may be the first one
//     to arrive after several things happened (a reconciler with health checks
//     reports Ready=True in one step, and a polling source can miss phases
//     entirely). Going backwards is not: a revision cannot become uncommitted.
//   - nothing precedes the commit. Proposed can only be Committed or Rejected;
//     a revision that was never committed cannot be reconciling or live, and an
//     adapter claiming otherwise has lost track of which revision it is on.
//   - rejection is pre-apply. Once Applied, a reconciler refusing the change is
//     a contradiction; that situation is Degraded.
//
// Self transitions are always legal (a repeated observation of the same phase
// is normal, it just is not progress). Rejected is terminal; Degraded is not,
// because it can recover.
var transitions = map[delivery.Phase][]delivery.Phase{
	delivery.PhaseProposed:    {delivery.PhaseCommitted, delivery.PhaseRejected},
	delivery.PhaseCommitted:   {delivery.PhaseReconciling, delivery.PhaseApplied, delivery.PhaseHealthy, delivery.PhaseDegraded, delivery.PhaseRejected},
	delivery.PhaseReconciling: {delivery.PhaseApplied, delivery.PhaseHealthy, delivery.PhaseDegraded, delivery.PhaseRejected},
	delivery.PhaseApplied:     {delivery.PhaseHealthy, delivery.PhaseDegraded},
	delivery.PhaseHealthy:     {delivery.PhaseDegraded},
	delivery.PhaseDegraded:    {delivery.PhaseHealthy, delivery.PhaseApplied, delivery.PhaseReconciling},
	delivery.PhaseRejected:    {},
}

// InvalidTransitionError reports a transition the state machine forbids. It is
// a programming error in whatever produced the observation, so it travels as a
// hard error rather than being clamped to something plausible.
type InvalidTransitionError struct {
	From   delivery.Phase
	To     delivery.Phase
	Reason string
}

func (e *InvalidTransitionError) Error() string {
	msg := fmt.Sprintf("statemachine: invalid transition %s -> %s", e.From, e.To)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// Known reports whether p is one of the seven delivery phases.
func Known(p delivery.Phase) bool {
	_, ok := transitions[p]
	return ok
}

// Validate reports whether from -> to is a legal transition, returning an
// *InvalidTransitionError if it is not.
func Validate(from, to delivery.Phase) error {
	if !Known(from) {
		return &InvalidTransitionError{From: from, To: to, Reason: "unknown source phase"}
	}
	if !Known(to) {
		return &InvalidTransitionError{From: from, To: to, Reason: "unknown target phase"}
	}
	if from == to {
		return nil
	}
	for _, allowed := range transitions[from] {
		if allowed == to {
			return nil
		}
	}
	reason := "not an allowed transition"
	if len(transitions[from]) == 0 {
		reason = string(from) + " is terminal"
	}
	return &InvalidTransitionError{From: from, To: to, Reason: reason}
}

// Can reports whether from -> to is legal.
func Can(from, to delivery.Phase) bool { return Validate(from, to) == nil }

// Must panics on an illegal transition. Use it where the caller controls both
// phases and a violation could only be a bug; use Validate for phases that
// come from an adapter observation.
func Must(from, to delivery.Phase) {
	if err := Validate(from, to); err != nil {
		panic(err)
	}
}

// Terminal reports whether a phase settles the question for this revision.
// Degraded is deliberately NOT terminal: it can recover.
func Terminal(p delivery.Phase) bool {
	return p == delivery.PhaseHealthy || p == delivery.PhaseRejected
}

// Cause names the component responsible for a failure and the reason, so no
// failure ever surfaces as a bare phase. Every failure transition carries one.
type Cause struct {
	// Component is the responsible component: "flux", "argocd", "kubernetes",
	// "kelson".
	Component string
	// Reason is a short machine-readable token ("NotPickedUp", "NotReady").
	Reason string
	// Message is the human-readable detail.
	Message string
}

// IsZero reports whether the cause carries nothing.
func (c Cause) IsZero() bool { return c.Component == "" && c.Reason == "" && c.Message == "" }

// String renders "flux: NotReady: Kustomization ./apps is not ready".
func (c Cause) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{c.Component, c.Reason, c.Message} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ": ")
}

// Answer is the user-facing classification of a State: the three answers that
// must never be confused, plus the two non-failure ones.
type Answer string

const (
	// AnswerWaiting: committed, nothing has picked it up yet. Keep waiting.
	AnswerWaiting Answer = "waiting"
	// AnswerProgressing: something is actively working on this revision.
	AnswerProgressing Answer = "progressing"
	// AnswerLive: the change is live and healthy.
	AnswerLive Answer = "live"
	// AnswerStuck: no progress within the timeout. The cause names what we
	// were waiting for — typically a path nothing watches.
	AnswerStuck Answer = "stuck"
	// AnswerRejected: the reconciler processed the change and refused it.
	AnswerRejected Answer = "rejected"
	// AnswerDegraded: the change is applied but unhealthy.
	AnswerDegraded Answer = "degraded"
)

// Target is the revision the engine is asking about. Adapters build one from
// the ManifestSet they delivered.
type Target struct {
	Project     string
	Environment string
	Revision    string
	SpecHash    string
}

// TargetFromSet derives the correlation target from a delivered manifest set.
func TargetFromSet(set delivery.ManifestSet) Target {
	return Target{
		Project:     set.Project,
		Environment: set.Environment,
		Revision:    set.Revision,
		SpecHash:    set.SpecHash,
	}
}

// String renders "checkout/production@abc1234" for error resources.
func (t Target) String() string {
	loc := t.Project
	if t.Environment != "" {
		loc += "/" + t.Environment
	}
	if t.Revision != "" {
		loc += "@" + t.Revision
	}
	return loc
}

// ObservedRevision extracts the revision an observation is about, preferring
// the explicit field and falling back to kelson.dev/revision provenance.
func ObservedRevision(st delivery.Status) string {
	if st.Revision != "" {
		return st.Revision
	}
	return st.Detail[AnnotationRevision]
}

// Matches reports whether an observation is about this target. Correlation is
// what turns "the reconciler is reporting on the PREVIOUS revision" into "not
// picked up yet" instead of into false progress:
//
//   - a revision on the observation must equal the target revision;
//   - with no revision, kelson.dev/spec-hash provenance is the fallback;
//   - an observation with no provenance at all cannot be correlated and is
//     dropped, because attributing it to this revision would be a guess.
func (t Target) Matches(st delivery.Status) bool {
	if rev := ObservedRevision(st); rev != "" {
		return rev == t.Revision
	}
	if hash := st.Detail[AnnotationSpecHash]; hash != "" && t.SpecHash != "" {
		return hash == t.SpecHash
	}
	return false
}

// State is the engine's view of one revision at one point in time.
type State struct {
	// Target is the revision this state is about.
	Target Target
	// Phase is the current state-machine phase.
	Phase delivery.Phase
	// Stuck is set when the progress timeout expired without a phase change.
	// It is orthogonal to Phase: a stuck deployment is wedged in whatever
	// phase it reached, and which phase that is IS the diagnosis.
	Stuck bool
	// Cause names the responsible component and reason. Set on every failure
	// transition and on every stuck state; zero otherwise.
	Cause Cause
	// Detail is the adapter's opaque detail from the last correlated
	// observation (health counts, observedGeneration, ...).
	Detail map[string]string
	// ObservedRevision is the revision named by the last observation the
	// engine saw, correlated or not. When the engine is stuck in Committed
	// this is what the reconciler is still sitting on.
	ObservedRevision string
	// Stale counts observations that named a different revision than the
	// target. A rising count with no phase change is the signature of a
	// reconciler that is healthy but pointed somewhere else.
	Stale int
	// Since is when the engine entered Phase; UpdatedAt is the last change of
	// any kind.
	Since     time.Time
	UpdatedAt time.Time
}

// Answer classifies the state for a caller that has to say one sentence to a
// human. The failure answers are checked first: a Degraded deployment that
// then times out is still "degraded", not "stuck waiting".
func (s State) Answer() Answer {
	switch s.Phase {
	case delivery.PhaseRejected:
		return AnswerRejected
	case delivery.PhaseDegraded:
		return AnswerDegraded
	case delivery.PhaseHealthy:
		return AnswerLive
	}
	if s.Stuck {
		return AnswerStuck
	}
	if s.Phase == delivery.PhaseReconciling || s.Phase == delivery.PhaseApplied {
		return AnswerProgressing
	}
	return AnswerWaiting
}

// Status projects the state back onto the delivery-plane Status shape, so
// adapters can return the engine's verdict from Adapter.Status.
func (s State) Status() delivery.Status {
	return delivery.Status{
		Phase:    s.Phase,
		Revision: s.Target.Revision,
		Cause:    s.Cause.String(),
		Detail:   maps.Clone(s.Detail),
	}
}

// Err returns the structured delivery error for a settled failure, or nil when
// the state is not a failure. Stuck-before-pickup maps to delivery/not-watched
// (issue #34): the overwhelmingly common cause is a path nothing reconciles.
func (s State) Err() error {
	var code delivery.Code
	var msg, remediation string
	switch {
	case s.Stuck && (s.Phase == delivery.PhaseProposed || s.Phase == delivery.PhaseCommitted):
		code = delivery.ErrNotWatched
		msg = "revision was committed but never picked up"
		remediation = "check that the reconciler watches the path kelson writes to, and that its source is syncing"
	case s.Phase == delivery.PhaseRejected:
		code = delivery.ErrApplyFailed
		msg = "the reconciler rejected this revision"
		remediation = "fix the cause and redeploy; the change is not live"
	case s.Phase == delivery.PhaseDegraded:
		code = delivery.ErrApplyFailed
		msg = "the revision is applied but unhealthy"
		remediation = "inspect the workload health detail; the change IS live"
	case s.Stuck:
		code = delivery.ErrApplyFailed
		msg = "the revision made no progress before the timeout"
		remediation = "inspect the reconciler for the named component"
	default:
		return nil
	}
	return delivery.Error{
		Code:        code,
		Resource:    s.Target.String(),
		Message:     msg,
		Remediation: remediation,
		DocsURL:     deliveryDocsBase + "/" + string(code),
		Cause:       s.Cause.String(),
	}
}

// String renders a one-line summary for the CLI.
func (s State) String() string {
	out := fmt.Sprintf("%s (%s)", s.Phase, s.Answer())
	if !s.Cause.IsZero() {
		out += ": " + s.Cause.String()
	}
	return out
}

// Config configures an Engine.
type Config struct {
	// Target is the revision being tracked. Target.Revision is required.
	Target Target
	// Source feeds observations. Required.
	Source Source
	// Timeout is the PROGRESS timeout: the budget for one phase change, reset
	// on every phase change. Zero means DefaultTimeout.
	Timeout time.Duration
	// Component names the reconciler responsible for progress, used in
	// engine-generated causes ("flux", "argocd", "kelson"). Zero means
	// DefaultComponent.
	Component string
	// OnState, if set, is called on every state change (including the stuck
	// transition), outside the engine lock, from the Run goroutine.
	OnState func(State)
	// Now is the clock, injectable for tests. Zero means time.Now.
	Now func() time.Time
}

// Engine drives one revision through the state machine. One Engine tracks one
// revision: a new deployment gets a new Engine starting from Proposed.
type Engine struct {
	source    Source
	timeout   time.Duration
	component string
	onState   func(State)
	now       func() time.Time

	running atomic.Bool
	mu      sync.Mutex
	state   State
}

// New validates the config and returns an Engine parked in Proposed.
func New(cfg Config) (*Engine, error) {
	if cfg.Source == nil {
		return nil, errors.New("statemachine: Config.Source is required")
	}
	if cfg.Target.Revision == "" {
		return nil, errors.New("statemachine: Config.Target.Revision is required for correlation")
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("statemachine: Config.Timeout must not be negative, got %s", cfg.Timeout)
	}
	e := &Engine{
		source:    cfg.Source,
		timeout:   cfg.Timeout,
		component: cfg.Component,
		onState:   cfg.OnState,
		now:       cfg.Now,
	}
	if e.timeout == 0 {
		e.timeout = DefaultTimeout
	}
	if e.component == "" {
		e.component = DefaultComponent
	}
	if e.now == nil {
		e.now = time.Now
	}
	start := e.now()
	e.state = State{
		Target:    cfg.Target,
		Phase:     delivery.PhaseProposed,
		Since:     start,
		UpdatedAt: start,
	}
	return e, nil
}

// State returns a snapshot, safe to call from any goroutine while Run is
// executing (that is how a UI reads progress).
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return snapshot(e.state)
}

func snapshot(s State) State {
	s.Detail = maps.Clone(s.Detail)
	return s
}

// Run drives the machine off the status stream until the question is answered:
// the state settles (Healthy or Rejected), or the progress timeout expires and
// the state goes stuck with a cause. It returns the final state.
//
// The returned error is non-nil only for failures of the machinery itself — a
// source that failed or stopped early, an observation implying an illegal
// transition, ctx cancellation. A Rejected, Degraded or stuck deployment is a
// legitimate ANSWER, reported through the returned State (see State.Err for
// the structured delivery error).
//
// Run may be called again after it returns (to keep watching a Degraded
// revision, say), but not concurrently with itself: two watches feeding one
// machine would interleave into a fiction.
func (e *Engine) Run(ctx context.Context) (State, error) {
	if !e.running.CompareAndSwap(false, true) {
		return e.State(), errors.New("statemachine: Run is already in progress for this engine")
	}
	defer e.running.Store(false)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	obs := make(chan delivery.Status)
	watchErr := make(chan error, 1)
	go func() { watchErr <- e.source.Watch(ctx, obs) }()

	timer := time.NewTimer(e.timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return e.State(), ctx.Err()

		case err := <-watchErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				return e.State(), fmt.Errorf("statemachine: status source failed: %w", err)
			}
			// A clean stop with no verdict is still no verdict.
			return e.State(), ErrSourceStopped

		case st, ok := <-obs:
			if !ok {
				return e.State(), ErrSourceStopped
			}
			progressed, err := e.observe(st)
			if err != nil {
				return e.State(), err
			}
			settled := e.State()
			if Terminal(settled.Phase) {
				return settled, nil
			}
			if progressed {
				resetTimer(timer, e.timeout)
			}

		case <-timer.C:
			return e.MarkStuck(), nil
		}
	}
}

// resetTimer restarts the progress budget. Draining is required because the
// timer may have fired between the last select and here.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// observe folds one observation into the state, reporting whether it was
// progress (a phase change). Only progress resets the timeout: a reconciler
// that repeats "still Committed" forever is exactly the wedge we must detect.
func (e *Engine) observe(st delivery.Status) (bool, error) {
	e.mu.Lock()
	prev := e.state
	next := prev
	next.UpdatedAt = e.now()

	if rev := ObservedRevision(st); rev != "" {
		next.ObservedRevision = rev
	}
	if !e.state.Target.Matches(st) {
		// Not our revision: no phase movement, but the observation is the
		// evidence for the eventual stuck cause.
		next.Stale++
		e.state = next
		e.mu.Unlock()
		e.notify(next)
		return false, nil
	}

	if err := Validate(prev.Phase, st.Phase); err != nil {
		e.mu.Unlock()
		return false, err
	}

	progressed := st.Phase != prev.Phase
	next.Phase = st.Phase
	if progressed {
		next.Since = next.UpdatedAt
		// Progress clears a previous stuck verdict: something moved after all.
		next.Stuck = false
	}
	next.Detail = maps.Clone(st.Detail)
	next.Cause = e.causeFor(st)
	e.state = next
	e.mu.Unlock()

	e.notify(next)
	return progressed, nil
}

// causeFor derives the Cause for an observed status. Failure phases must carry
// one even when the adapter forgot: an unexplained failure is a bug report we
// cannot act on.
func (e *Engine) causeFor(st delivery.Status) Cause {
	if st.Cause == "" {
		switch st.Phase {
		case delivery.PhaseRejected:
			return Cause{Component: e.component, Reason: "Rejected", Message: "the reconciler refused this revision but reported no reason"}
		case delivery.PhaseDegraded:
			return Cause{Component: e.component, Reason: "Unhealthy", Message: "the revision is applied but unhealthy; the adapter reported no health detail"}
		default:
			return Cause{}
		}
	}
	msg := st.Cause
	// Adapters commonly prefix their own name ("flux: ..."); do not stutter.
	if trimmed, ok := strings.CutPrefix(msg, e.component+":"); ok {
		msg = strings.TrimSpace(trimmed)
	}
	return Cause{Component: e.component, Reason: st.Detail["reason"], Message: msg}
}

// MarkStuck records the timeout verdict. The phase the machine is wedged in
// determines the cause, which is the whole point of keeping the three answers
// apart: stuck-in-Committed is a wiring problem, stuck-in-Degraded is a
// workload problem, and they get different sentences.
//
// Run calls it when the progress timer expires. Callers with a wall-clock
// budget of their own (kelson deploy's --timeout cancels the ctx) call it
// after Run returns on that deadline: both budgets expire together when
// nothing progresses, and the verdict must not depend on which timer the
// scheduler serviced first.
func (e *Engine) MarkStuck() State {
	e.mu.Lock()
	s := e.state
	s.Stuck = true
	s.UpdatedAt = e.now()
	waited := s.UpdatedAt.Sub(s.Since).Round(time.Millisecond)

	switch s.Phase {
	case delivery.PhaseProposed:
		s.Cause = Cause{
			Component: "kelson",
			Reason:    "NotCommitted",
			Message:   fmt.Sprintf("revision %s was never committed (no commit observed within %s)", s.Target.Revision, waited),
		}
	case delivery.PhaseCommitted:
		msg := fmt.Sprintf("revision %s was committed but %s has not picked it up within %s",
			s.Target.Revision, e.component, waited)
		if s.ObservedRevision != "" && s.ObservedRevision != s.Target.Revision {
			msg += fmt.Sprintf("; it is still reporting revision %s", s.ObservedRevision)
		}
		msg += " — check that the reconciler watches the path kelson writes to"
		s.Cause = Cause{Component: e.component, Reason: "NotPickedUp", Message: msg}
	case delivery.PhaseReconciling:
		s.Cause = Cause{
			Component: e.component,
			Reason:    "StalledReconciling",
			Message:   fmt.Sprintf("%s has been reconciling revision %s for %s without progress", e.component, s.Target.Revision, waited),
		}
	case delivery.PhaseApplied:
		s.Cause = Cause{
			Component: e.component,
			Reason:    "HealthUnknown",
			Message:   fmt.Sprintf("revision %s was applied but never reported healthy within %s", s.Target.Revision, waited),
		}
	default:
		// Degraded (the only non-terminal phase left) already carries the
		// adapter's health cause; the timeout adds nothing but the flag.
		if s.Cause.IsZero() {
			s.Cause = Cause{
				Component: e.component,
				Reason:    "NoProgress",
				Message:   fmt.Sprintf("revision %s made no progress from %s within %s", s.Target.Revision, s.Phase, waited),
			}
		}
	}
	e.state = s
	e.mu.Unlock()

	out := snapshot(s)
	e.notify(out)
	return out
}

// notify calls the observer callback outside the lock, so a callback may read
// State() without deadlocking.
func (e *Engine) notify(s State) {
	if e.onState != nil {
		e.onState(snapshot(s))
	}
}
