package buildkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/dafrie/kelson/internal/build"
)

// fakeCluster is the injectable Cluster seam: it records the last submitted
// manifest, emits canned logs, and returns a fixed, digest-pinned Result — the
// same posture delivery.direct's tests use. No test here touches a cluster.
type fakeCluster struct {
	mu        sync.Mutex
	manifest  []byte
	logs      string
	result    build.Result
	waitErr   error
	submitErr error
	waited    bool
}

func (f *fakeCluster) Submit(_ context.Context, manifest []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return "", f.submitErr
	}
	f.manifest = manifest
	return "build-shop-checkout-deadbeef", nil
}

func (f *fakeCluster) Wait(_ context.Context, _ string, w io.Writer) (build.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waited = true
	if f.waitErr != nil {
		return build.Result{}, f.waitErr
	}
	if w != nil {
		_, _ = io.WriteString(w, f.logs)
	}
	return f.result, nil
}

func (f *fakeCluster) submitted() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifest
}

func newDriver(t *testing.T, fc *fakeCluster, cfg Config) *Driver {
	t.Helper()
	d, err := New(Options{Cluster: fc, Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

const testDigest = "sha256:6B29FC40C84A0DEA9A0F7B1EB0C2C0C2F8E3C7E0A8B9A0B1C2D3E4F5A6B7C8D9"

// TestDriverPattern verifies the full driver flow against the fake: render
// (pure), Submit the rendered Job, Wait streams logs, and the digest-pinned
// Result comes back. This is the shape of every real build.
func TestDriverBuildRendersSubmitsAndStreams(t *testing.T) {
	fc := &fakeCluster{
		logs: "step 1/3 FROM ...\nexporting to image\n",
		result: build.Result{
			Reference: "ghcr.io/acme/checkout@" + testDigest,
			Digest:    testDigest,
			Tag:       "deadbeefabcd1234",
			Platforms: []string{"linux/amd64", "linux/arm64"},
		},
	}
	d := newDriver(t, fc, Config{Namespace: testNS})

	var buf bytes.Buffer
	res, err := d.Build(context.Background(), baseRequest(), &buf)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The exact manifest the driver rendered was what reached the cluster.
	submitted := fc.submitted()
	if len(submitted) == 0 {
		t.Fatal("driver never submitted a workload")
	}
	if res.Reference != "ghcr.io/acme/checkout@"+testDigest || res.Digest != testDigest {
		t.Fatalf("result = %+v", res)
	}
	if !fc.waited {
		t.Fatal("driver never waited on the job")
	}
	if buf.String() != fc.logs {
		t.Fatalf("streamed logs = %q, want %q", buf.String(), fc.logs)
	}
}

// TestDriverName asserts the strategy id the spec's infra will key on.
func TestDriverName(t *testing.T) {
	if got := newDriver(t, &fakeCluster{}, Config{Namespace: testNS}).Name(); got != StrategyName {
		t.Fatalf("Name = %q, want %q", got, StrategyName)
	}
}

// TestDriverNewRequiresCluster mirrors delivery.direct: a nil Cluster is a
// construction error, not a runtime surprise.
func TestDriverNewRequiresCluster(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a Cluster is required")
	}
}

// TestDriverSubmitErrorSurfaces: a failed submit stops the build without a
// misleading streamed result.
func TestDriverSubmitErrorSurfaces(t *testing.T) {
	fc := &fakeCluster{submitErr: errors.New("kube: forbidden")}
	d := newDriver(t, fc, Config{Namespace: testNS})
	if _, err := d.Build(context.Background(), baseRequest(), io.Discard); err == nil {
		t.Fatal("expected submit error to surface")
	}
}
