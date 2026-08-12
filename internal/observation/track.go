package observation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultInterval is the poll cadence a Tracker uses when Config.Interval is
// zero. Fast enough to catch a crash "within seconds" (acceptance #53) without
// hammering the API server.
const DefaultInterval = time.Second

// DefaultTimeout is the wall-clock budget a Tracker waits for a workload to
// settle before reporting Stuck. Zero-config callers get this.
const DefaultTimeout = 5 * time.Minute

// Tracker polls a Probe until a workload reaches a decisive verdict: healthy,
// a concrete failure, or — when it only ever saw wait states — a timeout that
// is Stuck. The distinction is the point: stuck is NOT a failure, and this is
// the component that guarantees it is never reported as one.
//
// It composes with the state machine later: an adapter whose health gate is a
// Tracker feeds the verdict through the Applied -> Healthy / Applied ->
// Degraded transition, and a stuck verdict keeps the phase Applied instead.
// Evaluator computes the health verdict of a workload. Probe implements it; the
// interface exists so a Tracker is testable with a fake that needs no cluster.
type Evaluator interface {
	Evaluate(ctx context.Context, namespace, name string) (Verdict, error)
}

// Tracker polls an Evaluator until a workload reaches a decisive verdict:
// healthy, a concrete failure, or — when it only ever saw wait states — a
// timeout that is Stuck. The distinction is the point: stuck is NOT a failure,
// and this is the component that guarantees it is never reported as one.
//
// It composes with the state machine later: an adapter whose health gate is a
// Tracker feeds the verdict through the Applied -> Healthy / Applied ->
// Degraded transition, and a stuck verdict keeps the phase Applied instead.
type Tracker struct {
	eval     Evaluator
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
}

// TrackerConfig configures a Tracker.
type TrackerConfig struct {
	// Evaluator computes verdicts. Required; a Probe satisfies it.
	Evaluator Evaluator
	// Interval is the poll cadence; 0 means DefaultInterval.
	Interval time.Duration
	// Timeout is the budget for the workload to settle; 0 means DefaultTimeout.
	Timeout time.Duration
	// Now is the clock, injectable so timeouts are deterministic in tests.
	// Zero means time.Now.
	Now func() time.Time
}

// NewTracker validates the config and returns a Tracker.
func NewTracker(cfg TrackerConfig) (*Tracker, error) {
	if cfg.Evaluator == nil {
		return nil, errors.New("observation: an evaluator is required")
	}
	t := &Tracker{eval: cfg.Evaluator, interval: cfg.Interval, timeout: cfg.Timeout, now: cfg.Now}
	if t.now == nil {
		t.now = time.Now
	}
	if t.interval <= 0 {
		t.interval = DefaultInterval
	}
	if t.timeout <= 0 {
		t.timeout = DefaultTimeout
	}
	if t.timeout < t.interval {
		return nil, fmt.Errorf("observation: timeout %s must not be shorter than interval %s", t.timeout, t.interval)
	}
	return t, nil
}

// Wait polls until the workload settles or the timeout expires. The returned
// Verdict is Healthy when green, carries a failure code when definitively
// broken, and has Stuck=true with a wait code (progressing/not-scheduled) when
// the timeout expired without either. Only context cancellation returns an
// error; a timeout is a legitimate Stuck answer, not an error.
func (t *Tracker) Wait(ctx context.Context, namespace, name string) (Verdict, error) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	deadline := t.now().Add(t.timeout)
	last := Verdict{Code: CodeProgressing, Reason: "no signal yet", Resource: "Deployment/" + namespace + "/" + name}

	for {
		v, err := t.eval.Evaluate(ctx, namespace, name)
		if err != nil {
			return Verdict{}, err
		}
		if v.Healthy || IsFailure(v.Code) {
			return v, nil
		}
		last = v

		if t.now().After(deadline) {
			last.Stuck = true
			if last.Code == CodeHealthy {
				last.Code = CodeProgressing
			}
			return last, nil
		}

		select {
		case <-ctx.Done():
			return Verdict{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
