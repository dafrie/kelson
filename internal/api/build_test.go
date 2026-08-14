package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
)

// BuildService is assembly, like the other handlers: resolve the spec, resolve
// the strategy and the ref through the shared plan (internal/build/plan.go),
// and hand one build.Request to the plane. The driver and the executor are
// tested where they live — this plane may not import client-go — so these tests
// drive the RPC through the BuildConnector seam and assert the wiring: what
// reaches the builder, what comes back on the stream, and which refusals are
// which.

// --- fixtures ---------------------------------------------------------------

// A Project that builds from source, with the strategy named. `auto` is the
// interesting other case and has its own test: a server has no checkout, so it
// cannot be detected here at all.
const buildProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: shop
spec:
  source:
    git: https://github.com/acme/shop.git
    ref: main
  build:
    strategy: dockerfile
  components:
    - name: web
      port: 8080
`

const buildEnvironmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: shop
`

const (
	buildCommit    = "0123456789abcdef0123456789abcdef01234567"
	buildDigest    = "sha256:aaaabbbbccccddddeeeeffff00001111aaaabbbbccccddddeeeeffff00001111"
	buildRepo      = "ghcr.io/acme/shop"
	buildReference = buildRepo + "@" + buildDigest
)

// --- fakes ------------------------------------------------------------------

type fakeBuilder struct {
	mu     sync.Mutex
	reqs   []build.Request
	logs   string
	result build.Result
	err    error
	// onReturn runs as Build returns, however it returns. It is deferred on
	// purpose: the interesting case is the one where writing the log *fails*
	// because the caller went away, and a hook that only ran on the happy path
	// would make that case look like a hang.
	onReturn func()
}

func (f *fakeBuilder) Name() string { return "dockerfile" }

func (f *fakeBuilder) Build(_ context.Context, req build.Request, w io.Writer) (build.Result, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	logs, res, err, hook := f.logs, f.result, f.err, f.onReturn
	f.mu.Unlock()

	if hook != nil {
		defer hook()
	}
	if w != nil && logs != "" {
		if _, werr := io.WriteString(w, logs); werr != nil {
			return build.Result{}, werr
		}
	}
	if err != nil {
		return build.Result{}, err
	}
	if res.Reference == "" {
		res = build.Result{Reference: buildReference, Digest: buildDigest, Tag: req.Tag}
	}
	return res, nil
}

func (f *fakeBuilder) lastRequest(t *testing.T) build.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("the builder was never called")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeBuilder) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// fakeRevisions stands in for the delivery plane's git ls-remote.
type fakeRevisions struct {
	mu    sync.Mutex
	asked [][2]string
	hash  string
	err   error
}

func (f *fakeRevisions) Resolve(_ context.Context, repo, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, [2]string{repo, ref})
	if f.err != nil {
		return "", f.err
	}
	if f.hash == "" {
		return buildCommit, nil
	}
	return f.hash, nil
}

func (f *fakeRevisions) calls() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.asked...)
}

// buildPlaneFor serves the given fakes and records the target the handler
// derived from the request and the spec.
func buildPlaneFor(builder *fakeBuilder, revisions RevisionResolver, seen *BuildTarget) BuildConnector {
	return func(_ context.Context, t BuildTarget) (*BuildPlane, error) {
		if seen != nil {
			*seen = t
		}
		return &BuildPlane{Builder: builder, Revisions: revisions}, nil
	}
}

func buildSpec() *kelsonv1alpha1.SpecRef {
	return inlineSpec(buildProjectDoc, map[string]string{"production": buildEnvironmentDoc})
}

// collected is one Build stream, read to the end.
type collected struct {
	started  *kelsonv1alpha1.BuildResponse_Started
	finished *kelsonv1alpha1.BuildResponse_Finished
	chunks   [][]byte
	order    []string
}

func (c collected) logs() string {
	var b bytes.Buffer
	for _, chunk := range c.chunks {
		b.Write(chunk)
	}
	return b.String()
}

func collectBuild(t *testing.T, c clients, req *kelsonv1alpha1.BuildRequest) (collected, error) {
	t.Helper()
	stream, err := c.builds.Build(context.Background(), connect.NewRequest(req))
	if err != nil {
		return collected{}, err
	}
	defer func() { _ = stream.Close() }()

	var out collected
	for stream.Receive() {
		switch event := stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.BuildResponse_Started_:
			out.started = event.Started
			out.order = append(out.order, "started")
		case *kelsonv1alpha1.BuildResponse_Log_:
			out.chunks = append(out.chunks, event.Log.GetChunk())
			out.order = append(out.order, "log")
		case *kelsonv1alpha1.BuildResponse_Finished_:
			out.finished = event.Finished
			out.order = append(out.order, "finished")
		}
	}
	return out, stream.Err()
}

// --- tests ------------------------------------------------------------------

// The happy path is the contract: Started first with settled facts, the build's
// own output in the middle, Finished last with a digest-pinned reference. The
// request that reached the builder is asserted too, because everything the
// stream reports is derived from it.
func TestBuildStreamsStartedLogsAndFinished(t *testing.T) {
	builder := &fakeBuilder{logs: "#1 [internal] load build definition\n#8 pushing manifest\n"}
	revisions := &fakeRevisions{}
	var target BuildTarget
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, revisions, &target),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme", PushSecret: "ghcr-push"},
	})

	got, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got.order[0] != "started" {
		t.Errorf("first event = %q, want started", got.order[0])
	}
	if last := got.order[len(got.order)-1]; last != "finished" {
		t.Errorf("last event = %q, want finished", last)
	}
	if strings.Count(strings.Join(got.order, " "), "started") != 1 {
		t.Errorf("want exactly one Started, got %v", got.order)
	}

	if got.started.GetStrategy() != "dockerfile" {
		t.Errorf("strategy = %q, want dockerfile", got.started.GetStrategy())
	}
	if got.started.GetImageRepository() != buildRepo {
		t.Errorf("image_repository = %q, want %q", got.started.GetImageRepository(), buildRepo)
	}
	if got.started.GetRevision() != buildCommit {
		t.Errorf("revision = %q, want the resolved commit %q", got.started.GetRevision(), buildCommit)
	}
	// The tag is the shared derivation's, not a second opinion about it.
	if want := build.DestinationTag("shop", buildCommit); got.started.GetTag() != want {
		t.Errorf("tag = %q, want %q", got.started.GetTag(), want)
	}

	if got.logs() != builder.logs {
		t.Errorf("streamed log = %q, want %q", got.logs(), builder.logs)
	}
	if got.finished.GetReference() != buildReference {
		t.Errorf("reference = %q, want %q", got.finished.GetReference(), buildReference)
	}
	if got.finished.GetDigest() != buildDigest {
		t.Errorf("digest = %q, want %q", got.finished.GetDigest(), buildDigest)
	}

	req := builder.lastRequest(t)
	if req.SourceGit != "https://github.com/acme/shop.git" || req.SourceRef != buildCommit {
		t.Errorf("the builder got source %s at %s", req.SourceGit, req.SourceRef)
	}
	if req.Application != "" {
		t.Errorf("Application = %q, want empty: one build serves the whole Project", req.Application)
	}
	// The namespace defaults to the environment's, exactly like the CLI's.
	if target.Namespace != "shop-production" {
		t.Errorf("build namespace = %q, want shop-production", target.Namespace)
	}
	if target.PushSecret != "ghcr-push" {
		t.Errorf("push secret = %q, want the server default", target.PushSecret)
	}
}

// The ref is resolved once, before anything else happens, so the tag, the
// Started event and the tree the build pod checks out are the same string. A
// ref that is already a commit costs no lookup at all.
func TestBuildResolvesTheRefOnce(t *testing.T) {
	t.Run("request ref beats the spec's", func(t *testing.T) {
		revisions := &fakeRevisions{}
		c := serve(t, Options{
			Build:         buildPlaneFor(&fakeBuilder{}, revisions, nil),
			BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
		})

		if _, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
			Spec: buildSpec(), Environment: "production", Ref: "v1.4.0",
		}); err != nil {
			t.Fatalf("Build: %v", err)
		}
		asked := revisions.calls()
		if len(asked) != 1 {
			t.Fatalf("the resolver was asked %d times, want exactly once", len(asked))
		}
		if asked[0][1] != "v1.4.0" {
			t.Errorf("resolved %q, want the request's ref v1.4.0", asked[0][1])
		}
	})

	t.Run("a commit needs no lookup", func(t *testing.T) {
		revisions := &fakeRevisions{}
		builder := &fakeBuilder{}
		c := serve(t, Options{
			Build:         buildPlaneFor(builder, revisions, nil),
			BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
		})

		upper := strings.ToUpper(buildCommit)
		if _, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
			Spec: buildSpec(), Environment: "production", Ref: upper,
		}); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if n := len(revisions.calls()); n != 0 {
			t.Errorf("the resolver was asked %d times for a full commit", n)
		}
		if got := builder.lastRequest(t).Revision; got != buildCommit {
			t.Errorf("revision = %q, want the lowercased commit %q", got, buildCommit)
		}
	})
}

// The server's own limit, stated rather than guessed around: `auto` detection
// reads a source tree and a server has none, so the CLI's structured refusal
// travels over the wire with its code intact.
func TestBuildAutoStrategyNeedsASourceTree(t *testing.T) {
	builder := &fakeBuilder{}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	auto := strings.Replace(buildProjectDoc, "    strategy: dockerfile\n", "    strategy: auto\n", 1)
	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        inlineSpec(auto, map[string]string{"production": buildEnvironmentDoc}),
		Environment: "production",
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if got := detailCode(t, err); got != build.ReasonDetectionNeedsSource {
		t.Errorf("detail code = %q, want %q", got, build.ReasonDetectionNeedsSource)
	}
	for _, want := range []string{"spec.build.strategy", "#50"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
	if builder.calls() != 0 {
		t.Error("nothing may be built when the strategy could not be resolved")
	}
}

// The two other refusals, each with its own code so an agent branches rather
// than reading prose.
func TestBuildRefusals(t *testing.T) {
	noSource := strings.Replace(buildProjectDoc,
		"  source:\n    git: https://github.com/acme/shop.git\n    ref: main\n", "  image: ghcr.io/acme/shop:1.0.0\n", 1)
	// `strategy: none` means "deploy spec.image", so the spec has to carry one;
	// without it the model refuses first, with semantic/no-image-source, and
	// the build plane never gets a say.
	none := strings.Replace(buildProjectDoc,
		"    strategy: dockerfile\n", "    strategy: none\n  image: ghcr.io/acme/shop:1.0.0\n", 1)
	cases := []struct {
		name    string
		project string
		code    string
	}{
		{"no source repository", noSource, build.ReasonNoSource},
		{"strategy none is nothing to build", none, build.ReasonNothingToBuild},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := &fakeBuilder{}
			c := serve(t, Options{
				Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
				BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
			})

			_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
				Spec:        inlineSpec(tc.project, map[string]string{"production": buildEnvironmentDoc}),
				Environment: "production",
			})
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
			}
			if got := detailCode(t, err); got != tc.code {
				t.Errorf("detail code = %q, want %q", got, tc.code)
			}
			if builder.calls() != 0 {
				t.Error("a refusal must not reach the build plane")
			}
		})
	}
}

// A server with no --registry and a request that names none has no destination
// at all. That is a precondition of the server, not a malformed request, and
// the message says which of the two fixes applies.
func TestBuildWithoutADestinationIsRefused(t *testing.T) {
	c := serve(t, Options{Build: buildPlaneFor(&fakeBuilder{}, &fakeRevisions{}, nil)})

	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "--registry") {
		t.Errorf("the refusal should name the flag that fixes it: %v", err)
	}
}

// The resolved strategy selects the driver, so it has to reach the connector —
// which is where the concrete drivers are constructed, because this plane may
// not import the Kubernetes client they need (#49, ADR-0010). A spec naming
// buildpacks now builds; it is no longer a refusal.
func TestBuildStrategyReachesTheConnector(t *testing.T) {
	for _, strategy := range []string{"dockerfile", "buildpacks"} {
		t.Run(strategy, func(t *testing.T) {
			project := strings.Replace(buildProjectDoc, "    strategy: dockerfile\n", "    strategy: "+strategy+"\n", 1)
			builder := &fakeBuilder{}
			var target BuildTarget
			c := serve(t, Options{
				Build:         buildPlaneFor(builder, &fakeRevisions{}, &target),
				BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
			})

			got, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
				Spec:        inlineSpec(project, map[string]string{"production": buildEnvironmentDoc}),
				Environment: "production",
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if target.Strategy != strategy {
				t.Errorf("the connector was asked for %q, want %q", target.Strategy, strategy)
			}
			if got.started.GetStrategy() != strategy {
				t.Errorf("Started.strategy = %q, want %q", got.started.GetStrategy(), strategy)
			}
			if builder.calls() != 1 {
				t.Errorf("the build ran %d times, want 1", builder.calls())
			}
		})
	}
}

// The request may push somewhere else: where an image goes is not application
// description, so a caller is allowed an opinion about it (the strategy stays
// the spec's, which is why there is no field for that one).
func TestBuildRequestOverridesTheDestination(t *testing.T) {
	builder := &fakeBuilder{}
	var target BuildTarget
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, &target),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme", PushSecret: "ghcr-push", Namespace: "kelson-builds"},
	})

	if _, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec: buildSpec(), Environment: "production",
		Registry: "localhost:5000", PushSecret: "local-push",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := builder.lastRequest(t).Image; got != "localhost:5000/shop" {
		t.Errorf("image = %q, want localhost:5000/shop", got)
	}
	if target.PushSecret != "local-push" {
		t.Errorf("push secret = %q, want the request's", target.PushSecret)
	}
	// --build-namespace wins over the environment's namespace when set.
	if target.Namespace != "kelson-builds" {
		t.Errorf("namespace = %q, want the configured kelson-builds", target.Namespace)
	}
}

// A reference that is not digest-pinned breaks the one guarantee the build
// exists to provide (#51), so it is refused rather than returned.
func TestBuildRefusesAnUnpinnedReference(t *testing.T) {
	builder := &fakeBuilder{result: build.Result{Reference: buildRepo + ":latest"}}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	got, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err %v)", connect.CodeOf(err), err)
	}
	if got.finished != nil {
		t.Error("an unpinned build must not report Finished")
	}
}

// A failing build is a ConnectRPC error, not a fourth event: there is no image,
// so there is nothing a result message could honestly say. What the executor
// wrote before it failed still reaches the caller.
func TestBuildFailureKeepsTheLogAndFailsTheStream(t *testing.T) {
	builder := &fakeBuilder{
		logs: "#5 [build 2/2] RUN go build ./...\n#5 ERROR: exit code 2\n",
		err:  errors.New("kube: build shop-abc failed: BackoffLimitExceeded"),
	}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	got, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if err == nil {
		t.Fatal("a failed build completed the stream cleanly")
	}
	if !strings.Contains(err.Error(), "BackoffLimitExceeded") {
		t.Errorf("the executor's classification was lost: %v", err)
	}
	if got.logs() != builder.logs {
		t.Errorf("the output produced before the failure was lost: %q", got.logs())
	}
	if got.finished != nil {
		t.Error("a failed build must not report Finished")
	}
}

// Output is chunked for transport and nothing else: every chunk is bounded, and
// their concatenation is exactly what the builder wrote. A chunk boundary is
// explicitly not a line boundary, which is why the proto calls them bytes.
func TestBuildChunksLargeOutput(t *testing.T) {
	large := strings.Repeat("x", 3*logChunkSize+17)
	builder := &fakeBuilder{logs: large}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	got, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.chunks) < 4 {
		t.Errorf("%d chunks for %d bytes; the writer is not bounding them", len(got.chunks), len(large))
	}
	for i, chunk := range got.chunks {
		if len(chunk) > logChunkSize {
			t.Fatalf("chunk %d is %d bytes, over the %d cap", i, len(chunk), logChunkSize)
		}
	}
	if got.logs() != large {
		t.Errorf("the concatenated chunks are not the output the builder wrote (%d vs %d bytes)", len(got.logs()), len(large))
	}
}

// A client that goes away stops the server watching. The build goroutine has to
// unwind too — the pump's writer watches the context precisely so a cancelled
// build does not leave the builder blocked on a reader that stopped reading.
//
// The log is deliberately larger than the pump's buffer, so the writer is
// certainly parked on a full channel when the client disappears. Cancellation
// is then the only thing that can free it, which is the property under test
// rather than a race the test happens to win.
func TestBuildAbortUnblocksTheBuilder(t *testing.T) {
	released := make(chan struct{})
	builder := &fakeBuilder{
		logs:     strings.Repeat("y", (logChunkBuffer+4)*logChunkSize),
		onReturn: func() { close(released) },
	}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.builds.Build(ctx, connect.NewRequest(&kelsonv1alpha1.BuildRequest{
		Spec: buildSpec(), Environment: "production",
	}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("no Started event: %v", stream.Err())
	}
	cancel()
	_ = stream.Close()

	// The assertion is that this returns at all: the writer's ctx.Done branch
	// is the only thing that ends a Write blocked on a full channel.
	<-released
}

// A server started without the seam says so rather than panicking, the same way
// every other partially-wired capability does.
func TestBuildWithoutTheSeamIsUnimplemented(t *testing.T) {
	c := serve(t, Options{})

	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented (err %v)", connect.CodeOf(err), err)
	}
}

// A plane that cannot resolve refs must not silently build a moving target. It
// says what to pass instead.
func TestBuildWithoutARevisionResolverAsksForACommit(t *testing.T) {
	c := serve(t, Options{
		Build:         buildPlaneFor(&fakeBuilder{}, nil, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable (err %v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "40-character commit") {
		t.Errorf("the message should say what to pass instead: %v", err)
	}
}

// A connector that cannot reach the cluster is the server's dependency failing,
// not the caller's request being wrong — the distinction that stops an agent
// editing a perfectly good request.
func TestBuildConnectorFailureIsUnavailable(t *testing.T) {
	c := serve(t, Options{
		Build: func(context.Context, BuildTarget) (*BuildPlane, error) {
			return nil, fmt.Errorf("dialing the cluster: connection refused")
		},
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable (err %v)", connect.CodeOf(err), err)
	}
}
