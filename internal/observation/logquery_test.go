package observation

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ksfake "k8s.io/client-go/kubernetes/fake"
)

func t0() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func mkLine(pod, cont, msg string, ts time.Time) Line {
	return Line{Timestamp: ts, Pod: pod, Container: cont, Message: msg}
}

// fakePod builds a pod carrying the provenance application label.
func fakePod(name string, containers ...string) *corev1.Pod {
	cs := make([]corev1.Container, 0, len(containers))
	for _, c := range containers {
		cs = append(cs, corev1.Container{Name: c})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{appLabel: testApp}},
		Spec:       corev1.PodSpec{Containers: cs},
	}
}

// fakeStream is a StreamSource served from memory: no cluster, no network.
type fakeStream struct {
	mu    sync.Mutex
	fetch map[ContainerRef][]Line
}

func newFakeStream() *fakeStream { return &fakeStream{fetch: map[ContainerRef][]Line{}} }

func (f *fakeStream) add(ref ContainerRef, lines []Line) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetch[ref] = append(f.fetch[ref], lines...)
}

func (f *fakeStream) get(ref ContainerRef) []Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Line(nil), f.fetch[ref]...)
}

// FetchLines mimics the API server: it applies since and tail to the stored
// lines.
func (f *fakeStream) FetchLines(_ context.Context, ref ContainerRef, opts FetchOptions) ([]Line, error) {
	ls := f.get(ref)
	out := make([]Line, 0, len(ls))
	for _, l := range ls {
		if opts.Since != nil && !l.Timestamp.IsZero() && l.Timestamp.Before(*opts.Since) {
			continue
		}
		out = append(out, l)
	}
	out = fixSeq(out)
	if opts.Tail > 0 && len(out) > opts.Tail {
		out = out[len(out)-opts.Tail:]
	}
	return out, nil
}

func (f *fakeStream) FollowLines(ctx context.Context, ref ContainerRef, _ FetchOptions) (<-chan Line, error) {
	ls := f.get(ref)
	ch := make(chan Line)
	go func() {
		defer close(ch)
		for i, l := range ls {
			l.seq = i
			select {
			case ch <- l:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// fixSeq renumbers the within-container order so tests can build lines without
// fussing over the internal seq field.
func fixSeq(ls []Line) []Line {
	for i := range ls {
		ls[i].seq = i
	}
	return ls
}

func newTestQuery(objs []runtime.Object, src *fakeStream) *LogQuery {
	q, err := NewLogQuery(LogQueryConfig{Client: ksfake.NewSimpleClientset(objs...), Source: src})
	if err != nil {
		panic(err)
	}
	return q
}

func messages(ls []Line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Message
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- validation: bounded is the default --------------------------------

func TestQueryRequiresBound(t *testing.T) {
	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, newFakeStream())
	_, err := q.Query(context.Background(), Query{Namespace: testNS, Application: testApp})
	if err == nil {
		t.Fatal("an unbounded query must be rejected; unbounded must be deliberate (Follow)")
	}
	if !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("error should name bounding, got %q", err)
	}
}

func TestQueryRejectsAroundAndTailTogether(t *testing.T) {
	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, newFakeStream())
	_, err := q.Query(context.Background(), Query{
		Namespace: testNS, Application: testApp, Tail: 5,
		Around: &Around{Lines: 5, AtTermination: true},
	})
	if err == nil {
		t.Fatal("Tail and Around together must be rejected")
	}
}

// --- tail bounding -----------------------------------------------------

func TestTailBounded(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	var lines []Line
	for i := 0; i < 100; i++ {
		lines = append(lines, mkLine("web-0", "web", "msg-"+itoa(i), t0().Add(time.Duration(i)*time.Second)))
	}
	src.add(ref, fixSeq(lines))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	res, err := q.Query(context.Background(), Query{Namespace: testNS, Application: testApp, Tail: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(res.Lines))
	}
	want := []string{"msg-90", "msg-91", "msg-92", "msg-93", "msg-94", "msg-95", "msg-96", "msg-97", "msg-98", "msg-99"}
	if !eqStrings(messages(res.Lines), want) {
		t.Fatalf("tail mismatch: %v", messages(res.Lines))
	}
	for i, l := range res.Lines {
		if l.Pod != "web-0" || l.Container != "web" {
			t.Fatalf("line %d lost its origin: %+v", i, l)
		}
	}
}

// --- time range --------------------------------------------------------

func TestTimeRange(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	var lines []Line
	for i := 0; i < 10; i++ {
		lines = append(lines, mkLine("web-0", "web", "m"+itoa(i), t0().Add(time.Duration(i)*time.Second)))
	}
	src.add(ref, fixSeq(lines))

	since := t0().Add(2 * time.Second)
	until := t0().Add(5 * time.Second)
	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	res, err := q.Query(context.Background(), Query{
		Namespace: testNS, Application: testApp, Since: &since, Until: &until,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m2", "m3", "m4", "m5"}
	if !eqStrings(messages(res.Lines), want) {
		t.Fatalf("time range mismatch: %v", messages(res.Lines))
	}
}

// --- around a failure --------------------------------------------------

func TestAroundTermination(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	var lines []Line
	for i := 0; i < 50; i++ {
		lines = append(lines, mkLine("web-0", "web", "m"+itoa(i), t0().Add(time.Duration(i)*time.Second)))
	}
	src.add(ref, fixSeq(lines))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	res, err := q.Query(context.Background(), Query{
		Namespace: testNS, Application: testApp, Around: &Around{Lines: 5, AtTermination: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m45", "m46", "m47", "m48", "m49"}
	if !eqStrings(messages(res.Lines), want) {
		t.Fatalf("around-termination mismatch: %v", messages(res.Lines))
	}
}

func TestAroundTime(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	var lines []Line
	for i := 0; i < 10; i++ {
		lines = append(lines, mkLine("web-0", "web", "m"+itoa(i), t0().Add(time.Duration(i)*time.Second)))
	}
	src.add(ref, fixSeq(lines))

	anchor := t0().Add(4500 * time.Millisecond)
	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	res, err := q.Query(context.Background(), Query{
		Namespace: testNS, Application: testApp, Around: &Around{Lines: 3, Time: &anchor},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Around operates on a bounded recent window (2*Lines = 6 most recent lines:
	// m4..m9). The three nearest to t0+4.5s within that window are m4, m5, m6.
	want := []string{"m4", "m5", "m6"}
	if !eqStrings(messages(res.Lines), want) {
		t.Fatalf("around-time mismatch: %v", messages(res.Lines))
	}
}

// --- multi-replica merge ordering & tiebreak ---------------------------

func TestMultiReplicaMergeOrderedByTimestamp(t *testing.T) {
	src := newFakeStream()
	src.add(ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}, fixSeq([]Line{
		mkLine("web-0", "web", "a", t0().Add(time.Second)),
		mkLine("web-0", "web", "c", t0().Add(3*time.Second)),
	}))
	src.add(ContainerRef{Namespace: testNS, Pod: "web-1", Container: "web"}, fixSeq([]Line{
		mkLine("web-1", "web", "b", t0().Add(2*time.Second)),
		mkLine("web-1", "web", "d", t0().Add(2500*time.Millisecond)),
	}))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web"), fakePod("web-1", "web")}, src)
	res, err := q.Query(context.Background(), Query{Namespace: testNS, Application: testApp, Tail: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "d", "c"}
	if !eqStrings(messages(res.Lines), want) {
		t.Fatalf("merge order mismatch: %v", messages(res.Lines))
	}
	for i, l := range res.Lines {
		_ = l
		_ = i
	}
}

func TestEqualTimestampTiebreakByPodName(t *testing.T) {
	src := newFakeStream()
	src.add(ContainerRef{Namespace: testNS, Pod: "web-9", Container: "web"}, fixSeq([]Line{
		mkLine("web-9", "web", "z9", t0()),
	}))
	src.add(ContainerRef{Namespace: testNS, Pod: "web-1", Container: "web"}, fixSeq([]Line{
		mkLine("web-1", "web", "z1", t0()),
	}))

	q := newTestQuery([]runtime.Object{fakePod("web-9", "web"), fakePod("web-1", "web")}, src)
	res, err := q.Query(context.Background(), Query{Namespace: testNS, Application: testApp, Tail: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"z1", "z9"}
	if !eqStrings(messages(res.Lines), want) {
		t.Fatalf("equal-timestamp tiebreak mismatch: %v", messages(res.Lines))
	}
}

func TestEqualTimestampWithinContainerKeepsOrder(t *testing.T) {
	src := newFakeStream()
	src.add(ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}, fixSeq([]Line{
		mkLine("web-0", "web", "first", t0()),
		mkLine("web-0", "web", "second", t0()),
	}))
	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	res, err := q.Query(context.Background(), Query{Namespace: testNS, Application: testApp, Tail: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(messages(res.Lines), []string{"first", "second"}) {
		t.Fatalf("within-container equal-timestamp order lost: %v", messages(res.Lines))
	}
}

// TestMergeDeterministicAcrossRuns feeds the same lines in a different
// insertion order and asserts the merged output is byte-identical. No map
// iteration may leak into the result ordering.
func TestMergeDeterministicAcrossRuns(t *testing.T) {
	build := func(order int) string {
		src := newFakeStream()
		refA := ContainerRef{Namespace: testNS, Pod: "web-a", Container: "web"}
		refB := ContainerRef{Namespace: testNS, Pod: "web-b", Container: "web"}
		var la, lb []Line
		base := t0()
		for i := 0; i < 20; i++ {
			la = append(la, mkLine("web-a", "web", "a"+itoa(i), base.Add(time.Duration(i)*time.Second)))
		}
		for i := 0; i < 20; i++ {
			lb = append(lb, mkLine("web-b", "web", "b"+itoa(i), base.Add(time.Duration(i)*time.Second+500*time.Millisecond)))
		}
		if order == 0 {
			src.add(refA, fixSeq(la))
			src.add(refB, fixSeq(lb))
		} else {
			src.add(refB, fixSeq(lb))
			src.add(refA, fixSeq(la))
		}
		q := newTestQuery([]runtime.Object{fakePod("web-a", "web"), fakePod("web-b", "web")}, src)
		res, err := q.Query(context.Background(), Query{Namespace: testNS, Application: testApp, Tail: 100})
		if err != nil {
			t.Fatal(err)
		}
		return res.Text()
	}
	if build(0) != build(1) {
		t.Fatal("merged output is not deterministic across runs/insertion order")
	}
}

// --- filtering ---------------------------------------------------------

func TestFilterSubstringThenTail(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	var lines []Line
	for i := 0; i < 20; i++ {
		msg := "info-" + itoa(i)
		if i%2 == 0 {
			msg = "ERR " + itoa(i)
		}
		lines = append(lines, mkLine("web-0", "web", msg, t0().Add(time.Duration(i)*time.Second)))
	}
	src.add(ref, fixSeq(lines))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	res, err := q.Query(context.Background(), Query{
		Namespace: testNS, Application: testApp, Tail: 3, Match: &Match{Substring: "ERR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ERR 14", "ERR 16", "ERR 18"}
	if !eqStrings(messages(res.Lines), want) {
		t.Fatalf("filtered tail mismatch: %v", messages(res.Lines))
	}
}

func TestFilterRegex(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	src.add(ref, fixSeq([]Line{
		mkLine("web-0", "web", "status 500", t0()),
		mkLine("web-0", "web", "status 200", t0().Add(time.Second)),
		mkLine("web-0", "web", "ok", t0().Add(2*time.Second)),
	}))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	res, err := q.Query(context.Background(), Query{
		Namespace: testNS, Application: testApp, Tail: 100, Match: &Match{Regex: `status 5\d\d`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(messages(res.Lines), []string{"status 500"}) {
		t.Fatalf("regex filter mismatch: %v", messages(res.Lines))
	}
}

func TestInvalidRegexIsError(t *testing.T) {
	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, newFakeStream())
	_, err := q.Query(context.Background(), Query{
		Namespace: testNS, Application: testApp, Tail: 10, Match: &Match{Regex: "("},
	})
	if err == nil {
		t.Fatal("an invalid filter regex must be an error, not silent")
	}
}

// --- follow & context cancellation -------------------------------------

func TestFollowDeliversLinesAndCloses(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	src.add(ref, fixSeq([]Line{
		mkLine("web-0", "web", "one", t0()),
		mkLine("web-0", "web", "two", t0().Add(time.Second)),
		mkLine("web-0", "web", "three", t0().Add(2*time.Second)),
	}))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	// Default backlog, deliberately. This test's premise is a consumer that
	// keeps up, and a backlog of 2 for three lines does not express that — it
	// makes "nothing was dropped" depend on whether the reader is scheduled
	// between sends, which is a coin flip that lands differently on a loaded
	// CI runner. Overflow behaviour has its own tests that force the overflow
	// rather than racing for it.
	ch, st, err := q.Follow(context.Background(), Query{Namespace: testNS, Application: testApp})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	timeout := time.After(3 * time.Second)
	for len(got) < 3 {
		select {
		case l, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed early after %d lines: %v", len(got), got)
			}
			got = append(got, l.Message)
		case <-timeout:
			t.Fatalf("timed out waiting for follow lines; got %v", got)
		}
	}
	if _, ok := <-ch; ok {
		t.Fatal("follow channel should be closed after all lines")
	}
	if st.Dropped() != 0 {
		t.Fatalf("Dropped = %d, want 0 when the consumer keeps up", st.Dropped())
	}
	if !eqStrings(got, []string{"one", "two", "three"}) {
		t.Fatalf("follow order mismatch: %v", got)
	}
}

// blockingStream never emits and never closes, so a follow on it blocks until
// the context is cancelled.
type blockingStream struct{}

func (b *blockingStream) FollowLines(ctx context.Context, _ ContainerRef, _ FetchOptions) (<-chan Line, error) {
	ch := make(chan Line)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func (b *blockingStream) FetchLines(_ context.Context, _ ContainerRef, _ FetchOptions) ([]Line, error) {
	return nil, nil
}

func TestFollowContextCancellationCloses(t *testing.T) {
	client := ksfake.NewSimpleClientset(fakePod("web-0", "web"))
	q, err := NewLogQuery(LogQueryConfig{Client: client, Source: &blockingStream{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, _, err := q.Follow(ctx, Query{Namespace: testNS, Application: testApp, Backlog: 2})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("follow channel should close on context cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow channel did not close after context cancellation")
	}
}

// --- backpressure: the caller can detect a gap -------------------------

func TestBackpressureDropCountsDroppedLines(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	const total = 100
	var lines []Line
	for i := 0; i < total; i++ {
		lines = append(lines, mkLine("web-0", "web", "l"+itoa(i), t0().Add(time.Duration(i)*time.Millisecond)))
	}
	src.add(ref, fixSeq(lines))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	ch, st, err := q.Follow(context.Background(), Query{
		Namespace: testNS, Application: testApp, Backlog: 4, OnOverflow: OverflowDrop,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Do not drain: the merge overflows the backlog of 4 and drops the rest.
	time.Sleep(100 * time.Millisecond)
	if st.Dropped() == 0 {
		t.Fatal("Dropped = 0: a slow consumer must detect a loss, not have it silently swallowed")
	}
	// Drain whatever is buffered; there must be at most backlog lines held.
	drained := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto done
			}
			drained++
		case <-time.After(200 * time.Millisecond):
			goto done
		}
	}
done:
	if drained > 4 {
		t.Fatalf("held %d lines with a backlog of 4", drained)
	}
	if st.Dropped() == 0 {
		t.Fatal("expected a counted drop in drop mode")
	}
}

func TestBackpressureBlockLosesNothing(t *testing.T) {
	src := newFakeStream()
	ref := ContainerRef{Namespace: testNS, Pod: "web-0", Container: "web"}
	const total = 200
	var lines []Line
	for i := 0; i < total; i++ {
		lines = append(lines, mkLine("web-0", "web", "l"+itoa(i), t0().Add(time.Duration(i)*time.Nanosecond)))
	}
	src.add(ref, fixSeq(lines))

	q := newTestQuery([]runtime.Object{fakePod("web-0", "web")}, src)
	ch, st, err := q.Follow(context.Background(), Query{
		Namespace: testNS, Application: testApp, Backlog: 2, OnOverflow: OverflowBlock,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < total {
		select {
		case l, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d/%d lines", len(got), total)
			}
			got = append(got, l.Message)
			// Slow consumer: occasionally pause so the backlog genuinely fills.
			if len(got)%50 == 0 {
				time.Sleep(2 * time.Millisecond)
			}
		case <-timeout:
			t.Fatalf("timed out with %d/%d lines", len(got), total)
		}
	}
	if _, ok := <-ch; ok {
		t.Fatal("follow channel should be closed after all lines")
	}
	if st.Dropped() != 0 {
		t.Fatalf("Dropped = %d: block mode must not drop any line", st.Dropped())
	}
	if len(got) != total {
		t.Fatalf("got %d lines, want %d (block mode loses nothing)", len(got), total)
	}
}

// --- plain-text rendering ----------------------------------------------

func TestResultText(t *testing.T) {
	res := Result{Lines: fixSeq([]Line{
		mkLine("web-0", "web", "hello", t0()),
	})}
	text := res.Text()
	if !strings.Contains(text, "web-0/web: hello") {
		t.Fatalf("text rendering missing origin/message: %q", text)
	}
	if !strings.Contains(text, "2023") {
		t.Fatalf("text rendering missing timestamp: %q", text)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}
