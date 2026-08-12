package observation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
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
