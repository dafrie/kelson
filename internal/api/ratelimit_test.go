package api

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestABucketRefillsOverTime: the budget is a rate, not a quota. An agent that
// backed off must get its requests back, or "retry after backing off" would be
// advice that never works.
func TestABucketRefillsOverTime(t *testing.T) {
	l := newLimiter()
	now := testNow

	// 60/minute is one token a second, and a burst of two is the whole start.
	for i := range 2 {
		if !l.allow("deploybot", 60, 2, now) {
			t.Fatalf("token %d of the burst was not available at the start", i+1)
		}
	}
	if l.allow("deploybot", 60, 2, now) {
		t.Fatal("a third request inside the same instant was allowed")
	}
	if !l.allow("deploybot", 60, 2, now.Add(time.Second)) {
		t.Error("a second later the bucket had not refilled by one token")
	}

	// And it does not refill past the burst, whatever the wait.
	for i := range 2 {
		if !l.allow("deploybot", 60, 2, now.Add(time.Hour)) {
			t.Errorf("the bucket did not hold its full burst after an hour (token %d)", i+1)
		}
	}
	if l.allow("deploybot", 60, 2, now.Add(time.Hour)) {
		t.Error("an hour's wait accumulated more than the burst")
	}
}

// TestABucketIgnoresAClockThatWentBackwards: an NTP step must not mint tokens,
// and must not permanently wedge the identity either.
func TestABucketIgnoresAClockThatWentBackwards(t *testing.T) {
	l := newLimiter()
	if !l.allow("deploybot", 60, 1, testNow) {
		t.Fatal("the first request was refused")
	}
	if l.allow("deploybot", 60, 1, testNow.Add(-time.Hour)) {
		t.Error("a clock that jumped backwards refilled the bucket")
	}
	if !l.allow("deploybot", 60, 1, testNow.Add(time.Second)) {
		t.Error("the identity stayed wedged after the clock recovered")
	}
}

// TestANonPositiveBudgetIsUnlimited: a stored budget of zero means the object
// was written by something that did not set one, and refusing every request
// would turn a decoding accident into a total outage for that identity.
func TestANonPositiveBudgetIsUnlimited(t *testing.T) {
	l := newLimiter()
	for i := range 1000 {
		if !l.allow("deploybot", 0, 0, testNow) {
			t.Fatalf("request %d was refused under a zero budget", i+1)
		}
	}
}

// TestTheLimiterIsSafeUnderConcurrency: every request goes through it, and
// kelson-server serves them in parallel.
func TestTheLimiterIsSafeUnderConcurrency(t *testing.T) {
	l := newLimiter()
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 32 {
				l.allow("agent", 6000, 100, testNow.Add(time.Duration(j)*time.Millisecond))
			}
			_ = i
		}()
	}
	wg.Wait()
}

// TestIdleBucketsAreSwept: an identity that stopped calling should stop costing
// memory. The sweep is size-triggered, so the test fills past the threshold.
func TestIdleBucketsAreSwept(t *testing.T) {
	l := newLimiter()
	for i := range bucketSweepAt {
		l.allow("agent-"+strconv.Itoa(i), 60, 1, testNow)
	}
	// One more request, far enough in the future that every bucket above is
	// idle, triggers the sweep as the new bucket is created.
	l.allow("latecomer", 60, 1, testNow.Add(2*bucketIdleTTL))

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) != 1 {
		t.Errorf("after the sweep %d buckets remain, want only the live one", len(l.buckets))
	}
}
