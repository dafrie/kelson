package statemachine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
)

// TestChanSourceDrivesEngine covers the adapter shape that already multiplexes
// its watches into one channel.
func TestChanSourceDrivesEngine(t *testing.T) {
	in := make(chan delivery.Status)
	engine, err := statemachine.New(statemachine.Config{
		Target:  target(),
		Source:  statemachine.Chan(in),
		Timeout: progressTimeout,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	go func() {
		for _, p := range []delivery.Phase{
			delivery.PhaseCommitted, delivery.PhaseReconciling,
			delivery.PhaseApplied, delivery.PhaseHealthy,
		} {
			in <- delivery.Status{Phase: p, Revision: targetRev}
		}
		close(in)
	}()

	final, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Answer() != statemachine.AnswerLive {
		t.Fatalf("answer = %s, want live", final.Answer())
	}
}

// TestPollFallback: adapters with no watch can still feed the engine, and the
// poll interval lives in the source rather than in the engine.
func TestPollFallback(t *testing.T) {
	var calls atomic.Int64
	obs := statemachine.ObserverFunc(func(context.Context) (delivery.Status, error) {
		phase := delivery.PhaseCommitted
		if calls.Add(1) >= 3 {
			phase = delivery.PhaseHealthy
		}
		return delivery.Status{Phase: phase, Revision: targetRev}, nil
	})

	engine, err := statemachine.New(statemachine.Config{
		Target:  target(),
		Source:  statemachine.Poll(obs, time.Millisecond),
		Timeout: progressTimeout,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	final, err := run(t, engine)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %s, want Healthy", final.Phase)
	}
	if calls.Load() < 3 {
		t.Fatalf("observer called %d times, want at least 3", calls.Load())
	}
}

func TestPollSurfacesObserverErrors(t *testing.T) {
	boom := errors.New("api server unreachable")
	src := statemachine.Poll(statemachine.ObserverFunc(func(context.Context) (delivery.Status, error) {
		return delivery.Status{}, boom
	}), time.Millisecond)

	engine, err := statemachine.New(statemachine.Config{Target: target(), Source: src, Timeout: progressTimeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := run(t, engine); !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want the observer error", err)
	}
}

func TestPollRejectsNonPositiveInterval(t *testing.T) {
	src := statemachine.Poll(statemachine.ObserverFunc(func(context.Context) (delivery.Status, error) {
		return delivery.Status{}, nil
	}), 0)
	engine, err := statemachine.New(statemachine.Config{Target: target(), Source: src, Timeout: progressTimeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := run(t, engine); err == nil {
		t.Fatal("a non-positive poll interval must fail loudly, not panic or spin")
	}
}

// TestSendHonoursCancellation: a source must never wedge past cancellation.
func TestSendHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan delivery.Status) // nobody reading
	if err := statemachine.Send(ctx, out, delivery.Status{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send = %v, want context.Canceled", err)
	}
}
