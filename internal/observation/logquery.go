package observation

// The log query engine (issue #54, the buildable half). This is the Go-level
// API that a later transport — the ConnectRPC schema of #69 — will wrap: a
// query addresses an application (never a pod), returns bounded, structured,
// deterministically-ordered lines, and can follow live. It deliberately carries
// no HTTP or protobuf types anywhere, so a CLI, the UI and the MCP server can
// each bind to it without reshaping it.
//
// Bounded is the default and the easy path: every query needs a bound (Tail,
// a time range, or an Around descriptor), and only the explicit Follow mode is
// unbounded. That is the "last N lines around this failure is a first-class
// query" the design note asks for.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultBacklog bounds how many lines Follow buffers for a slow consumer
// before the chosen overflow policy applies.
const DefaultBacklog = 1000

// DefaultAroundLines is the number of context lines Around returns when the
// caller does not say.
const DefaultAroundLines = 200

// filterScanFactor is how many raw lines per container are fetched for each
// requested Tail line when a Match is set, so a filtered query still has room
// to find the last N matching lines rather than the last N raw lines. The
// result stays bounded: the caller's Tail is the final bound, and the scan is
// a constant multiple of it.
const filterScanFactor = 8

// appLabel is the provenance label kelson stamps on a workload's pods. It is
// how a query addresses an application rather than a pod.
const appLabel = "kelson.dev/application"

// OverflowPolicy is the explicit, non-silent behaviour when Follow's backlog is
// full. Dropping lines is never silent: the caller reads a gap from
// Stream.Dropped.
type OverflowPolicy int

const (
	// OverflowDrop drops new lines while the backlog is full and counts them,
	// so the caller can detect exactly how much was lost. This is the default:
	// bounded and loss-aware, which is what a context window needs.
	OverflowDrop OverflowPolicy = iota
	// OverflowBlock blocks the producer until the consumer drains, so no line
	// is ever dropped. It trades latency for completeness and surfaces no gap
	// because there is none.
	OverflowBlock
)

// Around bounds a query to a window of context around a failure. Both modes
// stay bounded: they only operate on the fetched, already-bounded lines.
type Around struct {
	// Lines is the number of context lines to return; 0 means DefaultAroundLines.
	Lines int
	// Time, when set, returns the Lines lines nearest this instant (half before
	// and half after where the fetched history allows).
	Time *time.Time
	// AtTermination, when set, returns the last Lines lines of the stream — the
	// context right before a crashing container terminated, the CrashLoopBackOff
	// diagnosis.
	AtTermination bool
}

// Match is a server-side line predicate. Filtering happens before the final
// tail is applied, so a filtered query stays bounded and returns the last N
// *matching* lines.
type Match struct {
	// Substring keeps only lines containing it (case-sensitive).
	Substring string
	// Regex keeps only lines the compiled expression matches; it takes
	// precedence over Substring when both are set.
	Regex string
}

// Query is one log query against an application's pods. It is bounded by
// default; a query with no bound at all is a validation error, so unbounded
// reading is only ever the deliberate Follow mode.
type Query struct {
	// Namespace and Application select the workload's pods. Required.
	Namespace   string
	Application string
	// Containers restricts the query to these container names; empty means all
	// containers in every selected pod.
	Containers []string

	// Tail returns the last N lines of the merged output, after filtering.
	// This is the direct "last N lines" form of the design note.
	Tail int
	// Since and Until bound the time range (inclusive). Either may be nil.
	Since *time.Time
	Until *time.Time
	// Around returns a window of context around a failure. Mutually exclusive
	// with Tail.
	Around *Around

	// Match filters lines server-side so a bounded result stays bounded.
	Match *Match

	// Backlog bounds how many lines Follow buffers for a slow consumer.
	// 0 means DefaultBacklog.
	Backlog int
	// OnOverflow chooses the slow-consumer behaviour; 0 means OverflowDrop.
	OnOverflow OverflowPolicy
}

// Result is the outcome of a bounded query: the structured lines plus a
// plain-text rendering of the same output.
type Result struct {
	Lines []Line
}

// Text renders the bounded result as plain text, one line per entry in the
// form "timestamp pod/container: message", skipping nothing.
func (r Result) Text() string {
	var b strings.Builder
	for _, l := range r.Lines {
		ts := "-"
		if !l.Timestamp.IsZero() {
			ts = l.Timestamp.Format(time.RFC3339Nano)
		}
		fmt.Fprintf(&b, "%s %s/%s: %s\n", ts, l.Pod, l.Container, l.Message)
	}
	return b.String()
}

// validate checks that a query has an explicit bound or follow, and that its
// options are consistent. This is what makes "bounded is the default".
func (q Query) validate() error {
	if q.Namespace == "" || q.Application == "" {
		return errors.New("observation: a log query needs a namespace and application")
	}
	if q.Around != nil && q.Tail > 0 {
		return errors.New("observation: a log query cannot set both Tail and Around")
	}
	return nil
}

// requireBound enforces the design invariant that a pull query is bounded.
// Following is the explicit, deliberate unbounded path and does not call this.
func (q Query) requireBound() error {
	if q.Tail > 0 || q.Since != nil || q.Around != nil {
		return nil
	}
	return errors.New("observation: a log query must be bounded — set Tail, a Since/Until time range, or Around (or follow instead)")
}

// compiledMatch is a prepared Match, validated and freed from the caller's
// strings so the hot path never re-compiles.
type compiledMatch struct {
	sub string
	re  *regexp.Regexp
}

func compileMatch(m *Match) (*compiledMatch, error) {
	if m == nil {
		return nil, nil
	}
	cm := &compiledMatch{sub: m.Substring}
	if m.Regex != "" {
		re, err := regexp.Compile(m.Regex)
		if err != nil {
			return nil, fmt.Errorf("observation: invalid log filter regex %q: %w", m.Regex, err)
		}
		cm.re = re
	}
	return cm, nil
}

func (c *compiledMatch) matches(msg string) bool {
	if c == nil {
		return true
	}
	if c.re != nil {
		return c.re.MatchString(msg)
	}
	if c.sub != "" {
		return strings.Contains(msg, c.sub)
	}
	return true
}

// LogQuery executes log queries against an application's pods. It takes an
// injected typed clientset for pod enumeration and an optional StreamSource for
// log fetching, so a test drives it with a fake clientset and a fake source and
// no cluster is ever touched.
type LogQuery struct {
	client kubernetes.Interface
	source StreamSource
}

// LogQueryConfig configures a LogQuery.
type LogQueryConfig struct {
	// Client enumerates the application's pods. Required; the typed clientset
	// fake serves tests.
	Client kubernetes.Interface
	// Source fetches logs. When nil, a ClientGoLogSource over Client is used.
	Source StreamSource
}

// NewLogQuery validates the config and returns a LogQuery.
func NewLogQuery(cfg LogQueryConfig) (*LogQuery, error) {
	if cfg.Client == nil {
		return nil, errors.New("observation: a clientset is required to enumerate pods")
	}
	src := cfg.Source
	if src == nil {
		src = ClientGoLogSource{Client: cfg.Client}
	}
	return &LogQuery{client: cfg.Client, source: src}, nil
}

// refs enumerates the containers this query addresses, in deterministic order:
// pods by name, containers in spec order. This is the only place pods are
// turned into a concrete set, and it never ranges over a map.
func (q *LogQuery) refs(ctx context.Context, qry Query) ([]ContainerRef, error) {
	list, err := q.client.CoreV1().Pods(qry.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: appLabel + "=" + qry.Application,
	})
	if err != nil {
		return nil, fmt.Errorf("observation: listing pods for %s/%s: %w", qry.Namespace, qry.Application, err)
	}
	pods := list.Items
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })

	var refs []ContainerRef
	for i := range pods {
		if pods[i].Labels[appLabel] != qry.Application {
			continue
		}
		for _, c := range pods[i].Spec.Containers {
			if len(qry.Containers) > 0 && !containsStr(qry.Containers, c.Name) {
				continue
			}
			refs = append(refs, ContainerRef{Namespace: qry.Namespace, Pod: pods[i].Name, Container: c.Name})
		}
	}
	return refs, nil
}

// Query runs a bounded query and returns the merged, filtered, ordered lines.
func (q *LogQuery) Query(ctx context.Context, qry Query) (Result, error) {
	if err := qry.validate(); err != nil {
		return Result{}, err
	}
	if err := qry.requireBound(); err != nil {
		return Result{}, err
	}
	match, err := compileMatch(qry.Match)
	if err != nil {
		return Result{}, err
	}
	refs, err := q.refs(ctx, qry)
	if err != nil {
		return Result{}, err
	}

	per := perContainerFetchOptions(qry)
	var all []Line
	for _, r := range refs {
		ls, err := q.source.FetchLines(ctx, r, per)
		if err != nil {
			return Result{}, fmt.Errorf("observation: fetching logs for %s/%s/%s: %w", r.Namespace, r.Pod, r.Container, err)
		}
		all = append(all, ls...)
	}

	ordered := mergeLines(all)
	filtered := ordered[:0:0]
	for _, l := range ordered {
		if !inRange(l, qry.Since, qry.Until) {
			continue
		}
		if !match.matches(l.Message) {
			continue
		}
		filtered = append(filtered, l)
	}

	if qry.Around != nil {
		filtered = selectAround(filtered, qry.Around)
	}
	if qry.Tail > 0 && len(filtered) > qry.Tail {
		filtered = filtered[len(filtered)-qry.Tail:]
	}
	return Result{Lines: filtered}, nil
}

// Follow streams a query's lines live. It returns a channel the caller drains
// until it closes, plus a Stream the caller uses to detect loss. Follow is the
// deliberate unbounded path.
func (q *LogQuery) Follow(ctx context.Context, qry Query) (<-chan Line, *Stream, error) {
	if err := qry.validate(); err != nil {
		return nil, nil, err
	}
	match, err := compileMatch(qry.Match)
	if err != nil {
		return nil, nil, err
	}
	refs, err := q.refs(ctx, qry)
	if err != nil {
		return nil, nil, err
	}

	srcs := make([]<-chan Line, 0, len(refs))
	for _, r := range refs {
		ch, err := q.source.FollowLines(ctx, r, FetchOptions{Timestamps: true, Since: qry.Since})
		if err != nil {
			return nil, nil, fmt.Errorf("observation: following logs for %s/%s/%s: %w", r.Namespace, r.Pod, r.Container, err)
		}
		srcs = append(srcs, ch)
	}

	s := &Stream{
		ctx:    ctx,
		out:    make(chan Line, backlog(qry.Backlog)),
		policy: qry.OnOverflow,
		match:  match,
		until:  qry.Until,
	}
	go s.merge(srcs)
	return s.out, s, nil
}

func backlog(n int) int {
	if n <= 0 {
		return DefaultBacklog
	}
	return n
}

// Stream is the handle a Follow consumer keeps. It owns the output channel and
// the loss counter.
type Stream struct {
	ctx     context.Context
	out     chan Line
	policy  OverflowPolicy
	match   *compiledMatch
	until   *time.Time
	dropped atomic.Int64
}

// Dropped returns how many lines were lost to a full backlog under
// OverflowDrop. It is the explicit, non-silent report of loss: a caller that
// sees Dropped > 0 knows the stream has a gap. Safe to call from any goroutine.
func (s *Stream) Dropped() int {
	return int(s.dropped.Load())
}

// emit forwards one line to the consumer, applying the filter and the chosen
// overflow policy. It returns false when the context is done and the stream
// should stop.
func (s *Stream) emit(l Line) bool {
	if !s.match.matches(l.Message) {
		return true
	}
	if s.until != nil && !l.Timestamp.IsZero() && l.Timestamp.After(*s.until) {
		return false
	}
	select {
	case s.out <- l:
		return true
	default:
	}
	switch s.policy {
	case OverflowBlock:
		select {
		case s.out <- l:
			return true
		case <-s.ctx.Done():
			return false
		}
	default: // OverflowDrop
		s.dropped.Add(1)
		return s.ctx.Err() == nil
	}
}

// merge is the k-way live merge over the per-container streams. Each stream
// keeps at most one buffered head; the globally earliest head by the
// deterministic order is emitted next, so within-container order is preserved
// and equal timestamps never reorder across runs. When no head is ready it
// blocks on the first live source rather than spinning.
func (s *Stream) merge(srcs []<-chan Line) {
	defer close(s.out)
	if len(srcs) == 0 {
		return
	}

	type state struct {
		ch      <-chan Line
		closed  bool
		hasHead bool
		head    Line
	}
	st := make([]state, len(srcs))
	for i := range srcs {
		st[i].ch = srcs[i]
	}
	open := len(srcs)

	pull := func(i int) {
		if st[i].closed || st[i].hasHead {
			return
		}
		select {
		case l, ok := <-st[i].ch:
			if !ok {
				st[i].closed = true
				open--
			} else {
				st[i].head = l
				st[i].hasHead = true
			}
		default:
		}
	}

	for open > 0 {
		// Drain whatever is immediately ready, emitting the earliest head.
		for {
			for i := range st {
				pull(i)
			}
			best := -1
			for i := range st {
				if !st[i].hasHead {
					continue
				}
				if best < 0 || less(st[i].head, st[best].head) {
					best = i
				}
			}
			if best < 0 {
				break
			}
			if !s.emit(st[best].head) {
				return
			}
			st[best].hasHead = false
		}
		if open == 0 {
			return
		}
		// Nothing ready; block on the first live, head-less source.
		waited := false
		for i := range st {
			if st[i].closed || st[i].hasHead {
				continue
			}
			select {
			case <-s.ctx.Done():
				return
			case l, ok := <-st[i].ch:
				if !ok {
					st[i].closed = true
					open--
				} else {
					st[i].head = l
					st[i].hasHead = true
				}
				waited = true
			}
			if !waited {
				continue
			}
			break
		}
	}
}

// perContainerFetchOptions derives what to push down to each container for a
// bounded query. It always asks for timestamps and pushes a bounded tail and/or
// since-time so no single container is fetched without bound.
func perContainerFetchOptions(qry Query) FetchOptions {
	opts := FetchOptions{Timestamps: true}
	switch {
	case qry.Around != nil:
		look := qry.Around.Lines
		if look <= 0 {
			look = DefaultAroundLines
		}
		if qry.Around.Time != nil {
			// Time-anchored context wants lines *before* the anchor too, so
			// fetch a deeper bounded lookback.
			opts.Tail = 2 * look
		} else {
			opts.Tail = look
		}
	case qry.Tail > 0:
		opts.Tail = qry.Tail
		if qry.Match != nil {
			opts.Tail = qry.Tail * filterScanFactor
		}
	}
	if qry.Since != nil {
		opts.Since = qry.Since
	}
	return opts
}

// less is the deterministic total order the merge and sort share: timestamp,
// then pod name, then container, then the line's within-container order. Equal
// timestamps therefore always resolve the same way between runs — never by map
// iteration or arrival order.
func less(a, b Line) bool {
	if !a.Timestamp.Equal(b.Timestamp) {
		return a.Timestamp.Before(b.Timestamp)
	}
	if a.Pod != b.Pod {
		return a.Pod < b.Pod
	}
	if a.Container != b.Container {
		return a.Container < b.Container
	}
	return a.seq < b.seq
}

// mergeLines orders a flat set of lines with the deterministic order.
func mergeLines(ls []Line) []Line {
	sort.SliceStable(ls, func(i, j int) bool { return less(ls[i], ls[j]) })
	return ls
}

// inRange applies the time-range bound client-side. Lines without a parseable
// timestamp are kept (they cannot be dated), so no line is silently dropped.
func inRange(l Line, since, until *time.Time) bool {
	if l.Timestamp.IsZero() {
		return true
	}
	if since != nil && l.Timestamp.Before(*since) {
		return false
	}
	if until != nil && l.Timestamp.After(*until) {
		return false
	}
	return true
}

// selectAround reduces the already-bounded, filtered lines to the descriptor's
// context window. AtTermination takes the last N (the pre-termination context);
// a Time anchor takes the N nearest lines, output in their original merge order.
func selectAround(lines []Line, a *Around) []Line {
	n := a.Lines
	if n <= 0 {
		n = DefaultAroundLines
	}
	if a.AtTermination || a.Time == nil {
		if len(lines) <= n {
			return lines
		}
		return lines[len(lines)-n:]
	}
	return aroundTime(lines, *a.Time, n)
}

func aroundTime(lines []Line, t time.Time, n int) []Line {
	type cand struct {
		i int
		d time.Duration
	}
	cands := make([]cand, len(lines))
	for i, l := range lines {
		if l.Timestamp.IsZero() {
			cands[i].d = time.Duration(1) << 62 // maximally far, kept as fallback
		} else {
			cands[i].d = l.Timestamp.Sub(t)
			if cands[i].d < 0 {
				cands[i].d = -cands[i].d
			}
		}
		cands[i].i = i
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].d != cands[j].d {
			return cands[i].d < cands[j].d
		}
		return cands[i].i < cands[j].i
	})
	if len(cands) > n {
		cands = cands[:n]
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].i < cands[j].i })
	out := make([]Line, 0, len(cands))
	for _, c := range cands {
		out = append(out, lines[c.i])
	}
	return out
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
