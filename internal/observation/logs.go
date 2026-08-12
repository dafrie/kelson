package observation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LogSource fetches a container's recent logs. It is the seam that keeps log
// access out of the health classifier: the typed clientset (client-go) can
// read logs where the dynamic client cannot, but a caller with no log access
// must still get a verdict — so logs are best-effort, never a prerequisite.
//
// The interface is deliberately tiny so a test can fake it and so the direct
// and Git adapters can each provide their own source later.
type LogSource interface {
	// ContainerLogs returns the most recent lines of the container's stdout.
	ContainerLogs(ctx context.Context, namespace, pod, container string) (string, error)
}

// LogSourceFunc adapts a plain function to LogSource.
type LogSourceFunc func(ctx context.Context, namespace, pod, container string) (string, error)

// ContainerLogs implements LogSource.
func (f LogSourceFunc) ContainerLogs(ctx context.Context, namespace, pod, container string) (string, error) {
	return f(ctx, namespace, pod, container)
}

// DefaultMaxLogLines is the tail a ClientGoLogSource fetches when the probe
// sets no limit. 50 lines is enough to diagnose a crash without drowning the
// verdict in a busy container's output.
const DefaultMaxLogLines = 50

// maxLogsBytes caps a single container's fetched logs so a chatty container
// cannot balloon a verdict indefinitely.
const maxLogsBytes = 1 << 16 // 64 KiB

// ClientGoLogSource reads logs through the typed clientset. This is the one
// place observation needs the typed client: the dynamic client used for the
// health signal cannot fetch pod logs.
type ClientGoLogSource struct {
	// Client is the typed clientset, injected the way adapters take clients so
	// tests can drive a fake without a cluster.
	Client kubernetes.Interface
	// MaxLines tails at most this many lines (0 means DefaultMaxLogLines).
	MaxLines int
}

var _ LogSource = ClientGoLogSource{}

// ContainerLogs implements LogSource. It returns an error only when the
// clientset itself fails; the verdict path records it on the Container and
// carries on.
func (s ClientGoLogSource) ContainerLogs(ctx context.Context, namespace, pod, container string) (string, error) {
	if s.Client == nil {
		return "", errors.New("observation: ClientGoLogSource has no client")
	}
	max := s.MaxLines
	if max <= 0 {
		max = DefaultMaxLogLines
	}
	tail := int64(max)
	req := s.Client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
	})
	rd, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("observation: opening logs for %s/%s/%s: %w", namespace, pod, container, err)
	}
	defer func() { _ = rd.Close() }()
	var b strings.Builder
	_, err = io.Copy(&b, io.LimitReader(rd, maxLogsBytes))
	if err != nil {
		return "", fmt.Errorf("observation: reading logs for %s/%s/%s: %w", namespace, pod, container, err)
	}
	return b.String(), nil
}

// Line is one structured log line: the fields an agent branches on without
// parsing prose (issue #54). It is the structured-output mode of the log query
// engine, and the same principle as diff.Risk and the detection Reason in
// internal/build/detect — the timestamp, pod and container are first-class, not
// tokens to be scraped back out of a rendered string.
//
// seq is the line's position within its own container's stream, the final
// tiebreak in the merge ordering so equal timestamps from the same container
// never reorder. It is internal to the merge and never serialized.
type Line struct {
	Timestamp time.Time `json:"timestamp"`
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Message   string    `json:"message"`
	seq       int
}

// ContainerRef names one pod's one container.
type ContainerRef struct {
	Namespace string
	Pod       string
	Container string
}

// FetchOptions controls how a single container's log lines are fetched. What
// the API server can do (tail, since-time, per-line timestamps, follow) is
// pushed down; the remaining bounding and merging is the query engine's job.
type FetchOptions struct {
	// Tail fetches at most this many of the container's most recent lines
	// (0 means no server-side tail).
	Tail int
	// Since fetches only lines at or after this time (nil means all).
	Since *time.Time
	// Timestamps requests a per-line timestamp so the engine can order and
	// filter by time. Without it, lines carry a zero Timestamp.
	Timestamps bool
	// Follow streams lines live instead of returning a finite set.
	Follow bool
}

// StreamSource fetches a container's logs as structured, timestamped Lines. It
// is the grown seam from the plain-string LogSource (which the health verdict
// still uses): the same client-go machinery, but emitting the per-line fields
// the query engine merges. Like LogSource it is deliberately small so a test
// fakes it and the direct and Git adapters can provide their own later.
type StreamSource interface {
	// FetchLines returns the container's matching lines, oldest first, applying
	// opts. It never follows.
	FetchLines(ctx context.Context, ref ContainerRef, opts FetchOptions) ([]Line, error)
	// FollowLines streams the container's lines as they are produced until ctx
	// is cancelled or the stream ends. The returned channel is closed by the
	// implementation when it is done.
	FollowLines(ctx context.Context, ref ContainerRef, opts FetchOptions) (<-chan Line, error)
}

var _ StreamSource = ClientGoLogSource{}

// lineScanBuffer is the buffer budget the log scanner uses. Logs routinely hold
// multi-line JSON and stack traces on a single physical line; the default 64 KB
// scanner cap would truncate them.
const lineScanBuffer = 1 << 20 // 1 MiB

// FetchLines implements StreamSource.
func (s ClientGoLogSource) FetchLines(ctx context.Context, ref ContainerRef, opts FetchOptions) ([]Line, error) {
	rc, err := s.openStream(ctx, ref, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var out []Line
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, lineScanBuffer), lineScanBuffer)
	seq := 0
	for sc.Scan() {
		out = append(out, parseLogLine(ref, sc.Text(), opts.Timestamps, seq))
		seq++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("observation: reading logs for %s/%s/%s: %w", ref.Namespace, ref.Pod, ref.Container, err)
	}
	return out, nil
}

// FollowLines implements StreamSource. The stream is read on a goroutine so a
// caller can consume lines as they arrive; the channel closes on context
// cancellation or when the log stream ends.
func (s ClientGoLogSource) FollowLines(ctx context.Context, ref ContainerRef, opts FetchOptions) (<-chan Line, error) {
	opts.Follow = true
	rc, err := s.openStream(ctx, ref, opts)
	if err != nil {
		return nil, err
	}
	ch := make(chan Line)
	go func() {
		defer close(ch)
		defer func() { _ = rc.Close() }()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, lineScanBuffer), lineScanBuffer)
		seq := 0
		for sc.Scan() {
			select {
			case ch <- parseLogLine(ref, sc.Text(), opts.Timestamps, seq):
				seq++
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// openStream builds and opens the clientset power for a single container's
// logs. Tail, since-time and per-line timestamps are pushed down to the API
// server so a bounded query never streams more from the wire than it needs.
func (s ClientGoLogSource) openStream(ctx context.Context, ref ContainerRef, opts FetchOptions) (io.ReadCloser, error) {
	if s.Client == nil {
		return nil, errors.New("observation: ClientGoLogSource has no client")
	}
	var tail *int64
	if opts.Tail > 0 {
		t := int64(opts.Tail)
		tail = &t
	}
	po := &corev1.PodLogOptions{
		Container:  ref.Container,
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
		TailLines:  tail,
	}
	if opts.Since != nil {
		po.SinceTime = &metav1.Time{Time: *opts.Since}
	}
	rc, err := s.Client.CoreV1().Pods(ref.Namespace).GetLogs(ref.Pod, po).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("observation: opening logs for %s/%s/%s: %w", ref.Namespace, ref.Pod, ref.Container, err)
	}
	return rc, nil
}

// parseLogLine splits one raw log line into a Line. With Timestamps the API
// server prefixes each line with an RFC3339Nano timestamp and a space; the
// timestamp is parsed off and the remainder is the message. A line that does
// not parse (no prefix, or a malformed one) is kept whole with a zero
// timestamp rather than discarded, so a malformed line is never silently lost.
func parseLogLine(ref ContainerRef, raw string, timestamps bool, seq int) Line {
	l := Line{Pod: ref.Pod, Container: ref.Container, Message: raw, seq: seq}
	if !timestamps {
		return l
	}
	if i := strings.IndexByte(raw, ' '); i >= 0 {
		if ts, err := time.Parse(time.RFC3339Nano, raw[:i]); err == nil {
			l.Timestamp = ts
			l.Message = raw[i+1:]
		}
	}
	return l
}
