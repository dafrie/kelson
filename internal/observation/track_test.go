package observation

import (
	"context"
	"testing"
	"time"
)

// fakeEval is a scriptable Evaluator for tracker tests.
type fakeEval func(ctx context.Context, namespace, name string) (Verdict, error)

func (f fakeEval) Evaluate(ctx context.Context, namespace, name string) (Verdict, error) {
	return f(ctx, namespace, name)
}

// TestTimeoutProducesStuckNotFailure is the timeout behaviour the issue calls
// out: a workload that makes no progress within its budget reports a Stuck
// state, and Stuck is a distinct state from failed — it must never carry a
// failure code.
func TestTimeoutProducesStuckNotFailure(t *testing.T) {
	eval := fakeEval(func(context.Context, string, string) (Verdict, error) {
		return Verdict{
			Healthy:  false,
			Code:     CodeProgressing,
			Reason:   "containers creating",
			Resource: "Deployment/prod/web",
		}, nil
	})

	tr, err := NewTracker(TrackerConfig{Evaluator: eval, Interval: 1 * time.Millisecond, Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := tr.Wait(ctx, testNS, testApp)
	if err != nil {
		t.Fatalf("a timeout is a Stuck answer, not an error: %v", err)
	}
	if !v.Stuck {
		t.Fatal("expected the verdict to be marked Stuck after the timeout")
	}
	if v.Healthy {
		t.Fatal("a stuck verdict must not be healthy")
	}
	if IsFailure(v.Code) {
		t.Fatalf("stuck must not be reported as a failure: Code = %q", v.Code)
	}
}

func TestWaitReturnsHealthyImmediately(t *testing.T) {
	eval := fakeEval(func(context.Context, string, string) (Verdict, error) {
		return Verdict{Healthy: true, Code: CodeHealthy, Resource: "Deployment/prod/web"}, nil
	})
	tr, err := NewTracker(TrackerConfig{Evaluator: eval, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	v, err := tr.Wait(context.Background(), testNS, testApp)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Healthy || v.Stuck {
		t.Fatalf("expected an immediate healthy verdict, got %+v", v)
	}
}

func TestWaitReturnsFailureImmediately(t *testing.T) {
	eval := fakeEval(func(context.Context, string, string) (Verdict, error) {
		return Verdict{Healthy: false, Code: CodeImagePullBackOff, Resource: "Deployment/prod/web"}, nil
	})
	tr, err := NewTracker(TrackerConfig{Evaluator: eval, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	v, err := tr.Wait(context.Background(), testNS, testApp)
	if err != nil {
		t.Fatal(err)
	}
	if v.Healthy || v.Stuck {
		t.Fatalf("expected an immediate failure verdict, got %+v", v)
	}
	if v.Code != CodeImagePullBackOff {
		t.Fatalf("Code = %q, want image-pull-back-off", v.Code)
	}
}

func TestWaitReturnsEvaluatorError(t *testing.T) {
	eval := fakeEval(func(context.Context, string, string) (Verdict, error) {
		return Verdict{}, context.Canceled
	})
	tr, err := NewTracker(TrackerConfig{Evaluator: eval, Interval: time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Wait(context.Background(), testNS, testApp)
	if err == nil {
		t.Fatal("expected the evaluator error to propagate")
	}
}
