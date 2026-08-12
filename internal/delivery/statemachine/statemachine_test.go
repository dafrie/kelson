package statemachine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
)

const (
	targetRev  = "9f1c2ab"
	staleRev   = "1111111"
	targetHash = "sha256:deadbeef"
)

// progressTimeout is generous: tests that must NOT time out use it, so a slow
// -race run cannot turn a happy path into a stuck verdict.
const progressTimeout = 30 * time.Second

// stuckTimeout is short: tests that MUST time out only ever wait this long.
const stuckTimeout = 30 * time.Millisecond

// fakeSource scripts a watch without a cluster: it pushes statuses in order,
// then either ends the stream, holds it open until cancellation (the "nothing
// more will happen" case the timeout exists for), or fails.
type fakeSource struct {
	statuses []delivery.Status
	hold     bool
	err      error
}

func (f *fakeSource) Watch(ctx context.Context, out chan<- delivery.Status) error {
	for _, st := range f.statuses {
		if err := statemachine.Send(ctx, out, st); err != nil {
			return err
		}
	}
	if f.err != nil {
		return f.err
	}
	if f.hold {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func target() statemachine.Target {
	return statemachine.Target{
		Project:     "checkout",
		Environment: "production",
		Revision:    targetRev,
		SpecHash:    targetHash,
	}
}

func status(p delivery.Phase, cause string, detail map[string]string) delivery.Status {
	return delivery.Status{Phase: p, Revision: targetRev, Cause: cause, Detail: detail}
}

// recorder collects the phases the engine announced, for asserting the path
// taken and not just the destination.
type recorder struct {
	mu     sync.Mutex
	phases []delivery.Phase
}

func (r *recorder) on(s statemachine.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases = append(r.phases, s.Phase)
}

func (r *recorder) seen() []delivery.Phase {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery.Phase(nil), r.phases...)
}

func newEngine(t *testing.T, cfg statemachine.Config) *statemachine.Engine {
	t.Helper()
	if cfg.Target.Revision == "" {
		cfg.Target = target()
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = progressTimeout
	}
	e, err := statemachine.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func run(t *testing.T, e *statemachine.Engine) (statemachine.State, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.Run(ctx)
}

// TestHappyPath walks Proposed -> Committed -> Reconciling -> Applied ->
// Healthy off a fake source reporting progressing statuses.
func TestHappyPath(t *testing.T) {
	rec := &recorder{}
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		status(delivery.PhaseReconciling, "", nil),
		status(delivery.PhaseApplied, "", map[string]string{"observedGeneration": "7"}),
		status(delivery.PhaseHealthy, "", map[string]string{"ready": "3/3"}),
		// Nothing after Healthy should be read: the machine settles there.
		status(delivery.PhaseDegraded, "should never be observed", nil),
	}, hold: true}

	var engine *statemachine.Engine
	cfg := statemachine.Config{
		Source:    src,
		Component: "flux",
		OnState: func(s statemachine.State) {
			rec.on(s)
			// Reading the snapshot from inside the callback must not
			// deadlock: callbacks run outside the engine lock.
			_ = engine.State()
		},
	}
	engine = newEngine(t, cfg)

	final, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %s, want Healthy", final.Phase)
	}
	if final.Answer() != statemachine.AnswerLive {
		t.Fatalf("answer = %s, want live", final.Answer())
	}
	if final.Stuck {
		t.Fatal("a healthy deployment must not be stuck")
	}
	if !final.Cause.IsZero() {
		t.Fatalf("success transitions carry no cause, got %q", final.Cause)
	}
	if final.Detail["ready"] != "3/3" {
		t.Fatalf("detail not carried through: %v", final.Detail)
	}
	if err := final.Err(); err != nil {
		t.Fatalf("healthy state must not produce an error, got %v", err)
	}

	want := []delivery.Phase{
		delivery.PhaseCommitted, delivery.PhaseReconciling,
		delivery.PhaseApplied, delivery.PhaseHealthy,
	}
	got := rec.seen()
	if len(got) != len(want) {
		t.Fatalf("observed phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("observed phases = %v, want %v", got, want)
		}
	}
}

// TestNotPickedUpBecomesStuck is answer (a): the commit landed and nothing ever
// reconciled it. It must reach a stuck state naming the cause, not spin.
func TestNotPickedUpBecomesStuck(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
	}, hold: true}

	engine := newEngine(t, statemachine.Config{Source: src, Component: "flux", Timeout: stuckTimeout})
	final, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseCommitted {
		t.Fatalf("phase = %s, want Committed (the phase it wedged in IS the diagnosis)", final.Phase)
	}
	if !final.Stuck {
		t.Fatal("want Stuck after the progress timeout")
	}
	if final.Answer() != statemachine.AnswerStuck {
		t.Fatalf("answer = %s, want stuck", final.Answer())
	}
	if final.Cause.Component != "flux" || final.Cause.Reason != "NotPickedUp" {
		t.Fatalf("cause must name the responsible component and reason, got %+v", final.Cause)
	}
	if !strings.Contains(final.Cause.Message, "watches the path") {
		t.Fatalf("stuck-in-Committed must point at the wiring, got %q", final.Cause.Message)
	}

	// Stuck-before-pickup is the delivery/not-watched failure of issue #34.
	var de delivery.Error
	if !errors.As(final.Err(), &de) {
		t.Fatalf("Err() = %v, want a delivery.Error", final.Err())
	}
	if de.Code != delivery.ErrNotWatched {
		t.Fatalf("code = %s, want %s", de.Code, delivery.ErrNotWatched)
	}
	if de.Cause == "" || de.Remediation == "" || de.DocsURL == "" {
		t.Fatalf("structured error must be actionable, got %+v", de)
	}
}

// TestStaleObservationIsNotProgress is the correlation guarantee: a reconciler
// happily reporting on the PREVIOUS revision must read as "not picked up",
// never as progress on ours.
func TestStaleObservationIsNotProgress(t *testing.T) {
	stale := delivery.Status{
		Phase:    delivery.PhaseHealthy,
		Revision: staleRev,
		Detail:   map[string]string{"ready": "3/3"},
	}
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		stale,
		stale,
	}, hold: true}

	engine := newEngine(t, statemachine.Config{Source: src, Component: "flux", Timeout: stuckTimeout})
	final, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseCommitted || !final.Stuck {
		t.Fatalf("stale Healthy for another revision must not settle us: %+v", final)
	}
	if final.Stale != 2 {
		t.Fatalf("stale count = %d, want 2", final.Stale)
	}
	if final.ObservedRevision != staleRev {
		t.Fatalf("observed revision = %q, want %q", final.ObservedRevision, staleRev)
	}
	if !strings.Contains(final.Cause.Message, staleRev) {
		t.Fatalf("cause should report what the reconciler is still on, got %q", final.Cause.Message)
	}
}

// TestRejectedNamesTheCause is answer (b): processed and refused.
func TestRejectedNamesTheCause(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		status(delivery.PhaseReconciling, "", nil),
		status(delivery.PhaseRejected,
			"flux: Kustomization ./apps/checkout build failed: accumulating resources: missing kustomization.yaml",
			map[string]string{"reason": "BuildFailed"}),
	}, hold: true}

	engine := newEngine(t, statemachine.Config{Source: src, Component: "flux"})
	final, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseRejected {
		t.Fatalf("phase = %s, want Rejected", final.Phase)
	}
	if final.Answer() != statemachine.AnswerRejected {
		t.Fatalf("answer = %s, want rejected", final.Answer())
	}
	if final.Stuck {
		t.Fatal("Rejected is an answer, not a timeout")
	}
	if final.Cause.Component != "flux" || final.Cause.Reason != "BuildFailed" {
		t.Fatalf("cause = %+v, want component flux / reason BuildFailed", final.Cause)
	}
	if strings.HasPrefix(final.Cause.Message, "flux:") {
		t.Fatalf("component prefix should not stutter: %q", final.Cause)
	}
	if !strings.Contains(final.Cause.Message, "missing kustomization.yaml") {
		t.Fatalf("cause must keep the adapter's reason, got %q", final.Cause.Message)
	}

	var de delivery.Error
	if !errors.As(final.Err(), &de) || de.Code != delivery.ErrApplyFailed {
		t.Fatalf("Err() = %v, want %s", final.Err(), delivery.ErrApplyFailed)
	}
}

// TestRejectedWithoutCauseStillNamesOne: an unexplained failure is unactionable,
// so the engine fills in the responsible component itself.
func TestRejectedWithoutCauseStillNamesOne(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		status(delivery.PhaseRejected, "", nil),
	}, hold: true}

	final, err := run(t, newEngine(t, statemachine.Config{Source: src, Component: "argocd"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Cause.IsZero() || final.Cause.Component != "argocd" {
		t.Fatalf("every failure transition must carry a cause, got %+v", final.Cause)
	}
}

// TestDegradedSurfacesHealthDetail is answer (c): live and wrong.
func TestDegradedSurfacesHealthDetail(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		status(delivery.PhaseReconciling, "", nil),
		status(delivery.PhaseApplied, "", nil),
		status(delivery.PhaseDegraded,
			"Deployment checkout: 1/3 replicas available: CrashLoopBackOff",
			map[string]string{"reason": "ProgressDeadlineExceeded", "ready": "1/3"}),
	}, hold: true}

	engine := newEngine(t, statemachine.Config{Source: src, Component: "kubernetes", Timeout: stuckTimeout})
	final, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded", final.Phase)
	}
	// It timed out waiting for recovery, but "degraded" is the answer the user
	// needs — it must not be reported as "stuck waiting for pickup".
	if !final.Stuck {
		t.Fatal("want the timeout to have fired")
	}
	if final.Answer() != statemachine.AnswerDegraded {
		t.Fatalf("answer = %s, want degraded", final.Answer())
	}
	if final.Detail["ready"] != "1/3" {
		t.Fatalf("health detail must survive: %v", final.Detail)
	}
	if !strings.Contains(final.Cause.Message, "CrashLoopBackOff") {
		t.Fatalf("cause must carry health detail, got %q", final.Cause.Message)
	}
	if final.Cause.Reason != "ProgressDeadlineExceeded" {
		t.Fatalf("reason = %q", final.Cause.Reason)
	}
}

// TestDegradedRecovers: Degraded is not terminal, and recovery clears stuck.
func TestDegradedRecovers(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		status(delivery.PhaseApplied, "", nil),
		status(delivery.PhaseDegraded, "1/3 replicas available", nil),
		status(delivery.PhaseHealthy, "", map[string]string{"ready": "3/3"}),
	}, hold: true}

	final, err := run(t, newEngine(t, statemachine.Config{Source: src, Component: "kubernetes"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseHealthy || final.Stuck {
		t.Fatalf("want a clean Healthy after recovery, got %+v", final)
	}
	if !final.Cause.IsZero() {
		t.Fatalf("recovery must clear the cause, got %q", final.Cause)
	}
}

// TestInvalidTransitionIsRejected: an adapter reporting an impossible jump is a
// programming error and must be loud.
func TestInvalidTransitionIsRejected(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseHealthy, "", nil), // Proposed -> Healthy
	}, hold: true}

	final, err := run(t, newEngine(t, statemachine.Config{Source: src}))
	var ite *statemachine.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("Run error = %v, want *InvalidTransitionError", err)
	}
	if ite.From != delivery.PhaseProposed || ite.To != delivery.PhaseHealthy {
		t.Fatalf("error = %+v", ite)
	}
	if final.Phase != delivery.PhaseProposed {
		t.Fatalf("state must not move on an invalid transition, got %s", final.Phase)
	}
}

func TestValidateTable(t *testing.T) {
	legal := [][2]delivery.Phase{
		{delivery.PhaseProposed, delivery.PhaseCommitted},
		{delivery.PhaseProposed, delivery.PhaseRejected},
		{delivery.PhaseCommitted, delivery.PhaseReconciling},
		{delivery.PhaseCommitted, delivery.PhaseApplied},
		{delivery.PhaseCommitted, delivery.PhaseHealthy},  // a polling source may see only the end
		{delivery.PhaseCommitted, delivery.PhaseDegraded}, // ... including a bad end
		{delivery.PhaseCommitted, delivery.PhaseRejected},
		{delivery.PhaseReconciling, delivery.PhaseApplied},
		{delivery.PhaseReconciling, delivery.PhaseHealthy},
		{delivery.PhaseReconciling, delivery.PhaseRejected},
		{delivery.PhaseReconciling, delivery.PhaseDegraded},
		{delivery.PhaseApplied, delivery.PhaseHealthy},
		{delivery.PhaseApplied, delivery.PhaseDegraded},
		{delivery.PhaseHealthy, delivery.PhaseDegraded},
		{delivery.PhaseDegraded, delivery.PhaseHealthy},
		{delivery.PhaseDegraded, delivery.PhaseReconciling},
		{delivery.PhaseCommitted, delivery.PhaseCommitted}, // self: legal, not progress
		{delivery.PhaseRejected, delivery.PhaseRejected},
	}
	for _, tc := range legal {
		if err := statemachine.Validate(tc[0], tc[1]); err != nil {
			t.Errorf("Validate(%s, %s) = %v, want nil", tc[0], tc[1], err)
		}
	}

	illegal := [][2]delivery.Phase{
		{delivery.PhaseProposed, delivery.PhaseHealthy},
		{delivery.PhaseProposed, delivery.PhaseReconciling},
		{delivery.PhaseCommitted, delivery.PhaseProposed},
		{delivery.PhaseHealthy, delivery.PhaseCommitted},
		{delivery.PhaseApplied, delivery.PhaseRejected}, // already applied: too late to refuse
		{delivery.PhaseRejected, delivery.PhaseHealthy}, // terminal
		{delivery.PhaseRejected, delivery.PhaseCommitted},
		{delivery.PhaseHealthy, "Wat"},
		{"Wat", delivery.PhaseHealthy},
	}
	for _, tc := range illegal {
		if err := statemachine.Validate(tc[0], tc[1]); err == nil {
			t.Errorf("Validate(%s, %s) = nil, want an error", tc[0], tc[1])
		}
		if statemachine.Can(tc[0], tc[1]) {
			t.Errorf("Can(%s, %s) = true, want false", tc[0], tc[1])
		}
	}

	if !statemachine.Terminal(delivery.PhaseHealthy) || !statemachine.Terminal(delivery.PhaseRejected) {
		t.Error("Healthy and Rejected settle the question")
	}
	if statemachine.Terminal(delivery.PhaseDegraded) {
		t.Error("Degraded must stay open: it can recover")
	}
}

func TestMustPanicsOnIllegalTransition(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Must should panic on an illegal transition")
		}
		if _, ok := r.(*statemachine.InvalidTransitionError); !ok {
			t.Fatalf("panic value = %T, want *InvalidTransitionError", r)
		}
	}()
	statemachine.Must(delivery.PhaseRejected, delivery.PhaseHealthy)
}

func TestCorrelation(t *testing.T) {
	tgt := target()
	cases := []struct {
		name string
		st   delivery.Status
		want bool
	}{
		{"explicit revision", delivery.Status{Revision: targetRev}, true},
		{"other revision", delivery.Status{Revision: staleRev}, false},
		{"revision annotation", delivery.Status{Detail: map[string]string{statemachine.AnnotationRevision: targetRev}}, true},
		{"spec-hash fallback", delivery.Status{Detail: map[string]string{statemachine.AnnotationSpecHash: targetHash}}, true},
		{"other spec-hash", delivery.Status{Detail: map[string]string{statemachine.AnnotationSpecHash: "sha256:other"}}, false},
		{"revision wins over spec-hash", delivery.Status{
			Revision: staleRev,
			Detail:   map[string]string{statemachine.AnnotationSpecHash: targetHash},
		}, false},
		{"no provenance", delivery.Status{Phase: delivery.PhaseHealthy}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tgt.Matches(tc.st); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}

	set := delivery.ManifestSet{Project: "checkout", Environment: "production", Revision: targetRev, SpecHash: targetHash}
	if got := statemachine.TargetFromSet(set); got != tgt {
		t.Fatalf("TargetFromSet = %+v, want %+v", got, tgt)
	}
	if got := tgt.String(); got != "checkout/production@"+targetRev {
		t.Fatalf("Target.String = %q", got)
	}
}

func TestUncorrelatedObservationsCannotSettle(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		{Phase: delivery.PhaseHealthy}, // no provenance at all
	}, hold: true}

	final, err := run(t, newEngine(t, statemachine.Config{Source: src, Timeout: stuckTimeout}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseProposed || !final.Stuck {
		t.Fatalf("an uncorrelatable Healthy must not settle anything, got %+v", final)
	}
	if final.Cause.Reason != "NotCommitted" {
		t.Fatalf("cause = %+v", final.Cause)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	src := &fakeSource{}
	cases := []struct {
		name string
		cfg  statemachine.Config
	}{
		{"no source", statemachine.Config{Target: target()}},
		{"no revision", statemachine.Config{Source: src}},
		{"negative timeout", statemachine.Config{Source: src, Target: target(), Timeout: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := statemachine.New(tc.cfg); err == nil {
				t.Fatal("want a config error")
			}
		})
	}

	e, err := statemachine.New(statemachine.Config{Source: src, Target: target()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := e.State()
	if s.Phase != delivery.PhaseProposed {
		t.Fatalf("a new engine starts Proposed, got %s", s.Phase)
	}
	if s.Answer() != statemachine.AnswerWaiting {
		t.Fatalf("answer = %s, want waiting", s.Answer())
	}
	if s.Status().Revision != targetRev || s.Status().Phase != delivery.PhaseProposed {
		t.Fatalf("Status projection = %+v", s.Status())
	}
}

func TestSourceFailureIsSurfaced(t *testing.T) {
	boom := errors.New("watch broke")
	src := &fakeSource{statuses: []delivery.Status{status(delivery.PhaseCommitted, "", nil)}, err: boom}

	final, err := run(t, newEngine(t, statemachine.Config{Source: src}))
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want the source error", err)
	}
	if final.Phase != delivery.PhaseCommitted {
		t.Fatalf("phase = %s, want the last known Committed", final.Phase)
	}
}

func TestSourceStoppingEarlyIsNotAVerdict(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{
		status(delivery.PhaseCommitted, "", nil),
		status(delivery.PhaseReconciling, "", nil),
	}}

	final, err := run(t, newEngine(t, statemachine.Config{Source: src}))
	if !errors.Is(err, statemachine.ErrSourceStopped) {
		t.Fatalf("Run error = %v, want ErrSourceStopped", err)
	}
	if final.Phase != delivery.PhaseReconciling {
		t.Fatalf("phase = %s", final.Phase)
	}
}

func TestRunHonoursCancellation(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{status(delivery.PhaseCommitted, "", nil)}, hold: true}
	engine := newEngine(t, statemachine.Config{Source: src})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var (
		final statemachine.State
		err   error
	)
	go func() {
		defer close(done)
		final, err = engine.Run(ctx)
	}()

	// Wait until the first observation landed, then cancel.
	waitForPhase(t, engine, delivery.PhaseCommitted)
	cancel()
	<-done

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if final.Phase != delivery.PhaseCommitted {
		t.Fatalf("phase = %s", final.Phase)
	}
}

// TestConcurrentRunIsRefused: two watches feeding one machine would interleave
// into a fiction, so the second Run is refused rather than accepted.
func TestConcurrentRunIsRefused(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{status(delivery.PhaseCommitted, "", nil)}, hold: true}
	// A long progress timeout keeps the first Run in flight while we probe.
	engine := newEngine(t, statemachine.Config{Source: src})

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = engine.Run(ctx)
	}()
	waitForPhase(t, engine, delivery.PhaseCommitted)

	_, err := engine.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second Run error = %v, want a refusal", err)
	}

	cancel()
	<-first
}

// waitForPhase blocks until the engine reports phase, proving Run is in flight.
func waitForPhase(t *testing.T, e *statemachine.Engine, phase delivery.Phase) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for e.State().Phase != phase {
		select {
		case <-deadline:
			t.Fatalf("engine never reached %s (stuck at %s)", phase, e.State().Phase)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// TestRepeatedObservationsDoNotDeferTheTimeout: a chatty reconciler repeating
// "still Committed" is precisely the wedge case, so only a phase change resets
// the progress budget.
func TestRepeatedObservationsDoNotDeferTheTimeout(t *testing.T) {
	statuses := make([]delivery.Status, 0, 21)
	statuses = append(statuses, status(delivery.PhaseCommitted, "", nil))
	for range 20 {
		statuses = append(statuses, status(delivery.PhaseCommitted, "", nil))
	}
	src := &fakeSource{statuses: statuses, hold: true}

	start := time.Now()
	final, err := run(t, newEngine(t, statemachine.Config{Source: src, Component: "flux", Timeout: stuckTimeout}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !final.Stuck {
		t.Fatal("repeated identical observations must not keep the machine alive")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout was deferred by chatter: %s", elapsed)
	}
}

func TestCauseString(t *testing.T) {
	c := statemachine.Cause{Component: "flux", Reason: "NotReady", Message: "Kustomization ./apps is not ready"}
	if got, want := c.String(), "flux: NotReady: Kustomization ./apps is not ready"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if (statemachine.Cause{Message: "bare"}).String() != "bare" {
		t.Fatal("a bare message renders unchanged")
	}
	if !(statemachine.Cause{}).IsZero() {
		t.Fatal("zero cause")
	}
}

func TestStateString(t *testing.T) {
	src := &fakeSource{statuses: []delivery.Status{status(delivery.PhaseCommitted, "", nil)}, hold: true}
	final, err := run(t, newEngine(t, statemachine.Config{Source: src, Component: "flux", Timeout: stuckTimeout}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(final.String(), "Committed (stuck): flux: NotPickedUp:") {
		t.Fatalf("State.String = %q", final.String())
	}
}
