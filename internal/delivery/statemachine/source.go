package statemachine

import (
	"context"
	"fmt"
	"time"

	"github.com/dafrie/kelson/internal/delivery"
)

// Source is the adapter side of the state machine: it feeds observations to
// the engine. This is the ONLY thing an adapter has to implement to plug into
// the state machine, which is what keeps the engine adapter-agnostic and
// unit-testable without a cluster.
//
// Watch blocks, pushing one observation per change onto out, until ctx is done
// or the underlying watch fails. It must:
//
//   - be watch-driven, not polling — the engine owns the only timer (use Poll
//     explicitly if a backend really has no watch);
//   - stamp each Status with the revision it observed (Status.Revision or
//     kelson.dev/revision in Detail) so the engine can correlate it — an
//     observation about the previous revision is meaningful, and dropping it
//     would erase the evidence that nothing has picked the new one up;
//   - carry a Cause on every failure phase, naming the responsible component
//     and the reason;
//   - return ctx.Err() when ctx is done, and a real error when the watch
//     breaks (the engine surfaces both rather than reporting a false verdict).
//
// Use Send to push, so a blocked engine cannot wedge the watch past
// cancellation.
type Source interface {
	Watch(ctx context.Context, out chan<- delivery.Status) error
}

// SourceFunc adapts a plain function to Source.
type SourceFunc func(ctx context.Context, out chan<- delivery.Status) error

// Watch implements Source.
func (f SourceFunc) Watch(ctx context.Context, out chan<- delivery.Status) error {
	return f(ctx, out)
}

// Send pushes one observation, honouring cancellation. Adapter authors should
// use it instead of a bare channel send.
func Send(ctx context.Context, out chan<- delivery.Status, st delivery.Status) error {
	select {
	case out <- st:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Chan returns a Source that forwards an existing channel of observations —
// for adapters that already multiplex their watches into one stream. Closing
// in ends the stream, which the engine reports as ErrSourceStopped unless the
// deployment already settled.
func Chan(in <-chan delivery.Status) Source {
	return SourceFunc(func(ctx context.Context, out chan<- delivery.Status) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case st, ok := <-in:
				if !ok {
					return nil
				}
				if err := Send(ctx, out, st); err != nil {
					return err
				}
			}
		}
	})
}

// Observer is the fallback shape for a backend with no watch: one point-in-time
// observation of the delivery target.
type Observer interface {
	Observe(ctx context.Context) (delivery.Status, error)
}

// ObserverFunc adapts a plain function to Observer.
type ObserverFunc func(ctx context.Context) (delivery.Status, error)

// Observe implements Observer.
func (f ObserverFunc) Observe(ctx context.Context) (delivery.Status, error) { return f(ctx) }

// Poll turns an Observer into a Source by sampling every interval, starting
// immediately.
//
// Prefer a real watch. Polling is a deliberate fallback for backends that
// expose no event stream, and it is confined HERE rather than in the engine:
// the engine's only timer stays the progress timeout, so poll latency can
// never be mistaken for lack of progress.
func Poll(o Observer, interval time.Duration) Source {
	return SourceFunc(func(ctx context.Context, out chan<- delivery.Status) error {
		if interval <= 0 {
			return fmt.Errorf("statemachine: Poll interval must be positive, got %s", interval)
		}
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			st, err := o.Observe(ctx)
			if err != nil {
				return err
			}
			if err := Send(ctx, out, st); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
	})
}
