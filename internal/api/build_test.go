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
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
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
	if req.Component != "" {
		t.Errorf("Component = %q, want empty: one build serves the whole Project", req.Component)
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

// --- ReportBuild ---------------------------------------------------------------

// ReportBuild judges the report in full and then refuses it (ADR-0034 decision
// 3). The two answers are different codes on purpose, and these tests are what
// keep them apart: a request kelson would have acted on gets Unimplemented, and
// one it would not gets InvalidArgument naming the field.

const reportSHA = "9f0a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2"

// reportProjectDoc is a Project with two components, so a report can name one
// it declares and one it does not.
const reportProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:v1
  components:
    - name: web
      kind: service
      port: 8080
    - name: worker
      kind: worker
  source:
    git: https://github.com/acme/checkout
    ref: main
`

// reportEnvironmentDoc exists because a report is guarded by agent policy
// (policy.go files ReportBuild under `deploy`), and policy is a property of the
// environment — so a stored project with no environments is a spec whose policy
// cannot be read, which is its own refusal.
const reportEnvironmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
`

func reportServer(t *testing.T) clients {
	t.Helper()
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project:      []byte(reportProjectDoc),
		Environments: map[string][]byte{"production": []byte(reportEnvironmentDoc)},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	return serve(t, Options{Specs: specs})
}

func reportBuild(t *testing.T, c clients, req *kelsonv1alpha1.ReportBuildRequest) error {
	t.Helper()
	_, err := c.builds.ReportBuild(t.Context(), connect.NewRequest(req))
	return err
}

func pinnedImage() string {
	return "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("a", 64)
}

// A well-formed report reaches the gate, and the gate is honest about what is
// missing: Unimplemented tells an agent to stop, where a cheerful `accepted:
// true` with nothing triggered would tell it to carry on.
func TestReportBuildIsGatedForAWellFormedReport(t *testing.T) {
	err := reportBuild(t, reportServer(t), &kelsonv1alpha1.ReportBuildRequest{
		Project: "checkout",
		Sha:     reportSHA,
		Ref:     "refs/heads/main",
		Images:  map[string]string{"web": pinnedImage()},
	})
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("a well-formed report = %v (code %s), want unimplemented", err, connect.CodeOf(err))
	}
	if !hasCode(detailCodes(err), string(delivery.ErrNotImplemented)) {
		t.Errorf("the refusal does not carry %s: %v", delivery.ErrNotImplemented, detailCodes(err))
	}
	// The slot names where the answer changes, which is what makes a gated
	// capability findable rather than a dead end.
	if !strings.Contains(err.Error(), "ADR-0034") {
		t.Errorf("the refusal does not name the pipeline it is waiting for: %v", err)
	}
}

// A pipeline author must learn what is wrong with their report *now*, not on
// the day the slot is filled.
func TestReportBuildValidatesBeforeItRefuses(t *testing.T) {
	cases := []struct {
		name    string
		req     *kelsonv1alpha1.ReportBuildRequest
		code    string
		wantsIn string
	}{{
		name:    "an abbreviated commit",
		req:     &kelsonv1alpha1.ReportBuildRequest{Project: "checkout", Sha: "9f0a1b2", Images: map[string]string{"web": pinnedImage()}},
		code:    ErrReportShaInvalid,
		wantsIn: "40-character",
	}, {
		name:    "no commit at all",
		req:     &kelsonv1alpha1.ReportBuildRequest{Project: "checkout", Images: map[string]string{"web": pinnedImage()}},
		code:    ErrReportShaInvalid,
		wantsIn: "join key",
	}, {
		name:    "no images",
		req:     &kelsonv1alpha1.ReportBuildRequest{Project: "checkout", Sha: reportSHA},
		code:    ErrReportNoImages,
		wantsIn: "names no images",
	}, {
		// The one guarantee the build plane exists to provide, and the one path
		// where the image comes from outside (#51, ADR-0010).
		name: "an image pinned by tag",
		req: &kelsonv1alpha1.ReportBuildRequest{Project: "checkout", Sha: reportSHA,
			Images: map[string]string{"web": "ghcr.io/acme/checkout-web:latest"}},
		code:    ErrReportImageNotPinned,
		wantsIn: "not pinned by digest",
	}, {
		name: "an empty image reference",
		req: &kelsonv1alpha1.ReportBuildRequest{Project: "checkout", Sha: reportSHA,
			Images: map[string]string{"web": ""}},
		code:    ErrReportImageNotPinned,
		wantsIn: "empty image reference",
	}, {
		// The proto asks for this by name: a key the Project does not declare
		// is an error naming it, not a silent drop.
		name: "a component the Project does not declare",
		req: &kelsonv1alpha1.ReportBuildRequest{Project: "checkout", Sha: reportSHA,
			Images: map[string]string{"api": pinnedImage()}},
		code:    ErrReportUnknownComponent,
		wantsIn: "declares no component",
	}}

	c := reportServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := reportBuild(t, c, tc.req)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("err = %v (code %s), want invalid-argument", err, connect.CodeOf(err))
			}
			if !hasCode(detailCodes(err), tc.code) {
				t.Errorf("the refusal carries %v, want %s", detailCodes(err), tc.code)
			}
			if !strings.Contains(err.Error(), tc.wantsIn) {
				t.Errorf("the refusal does not say %q: %v", tc.wantsIn, err)
			}
		})
	}
}

// The reverse of the component check is not an error: a component the Project
// declares and the report omits keeps whatever the spec resolves for it, so a
// partial report is a partial pin rather than a broken render.
func TestReportBuildAcceptsAPartialReport(t *testing.T) {
	err := reportBuild(t, reportServer(t), &kelsonv1alpha1.ReportBuildRequest{
		Project: "checkout",
		Sha:     reportSHA,
		Images:  map[string]string{"web": pinnedImage()},
	})
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("a report naming one of two components = %v (code %s), want unimplemented", err, connect.CodeOf(err))
	}
}

func TestReportBuildRefusesAnUnknownProject(t *testing.T) {
	err := reportBuild(t, reportServer(t), &kelsonv1alpha1.ReportBuildRequest{
		Project: "nowhere",
		Sha:     reportSHA,
		Images:  map[string]string{"web": pinnedImage()},
	})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("a report for a project that does not exist = %v (code %s), want not-found", err, connect.CodeOf(err))
	}
}

// A report with two bad images always names the same one first: a refusal that
// varied with Go's map order is one nobody can fix twice the same way.
func TestReportBuildRefusalIsDeterministic(t *testing.T) {
	c := reportServer(t)
	req := &kelsonv1alpha1.ReportBuildRequest{Project: "checkout", Sha: reportSHA, Images: map[string]string{
		"web":    "ghcr.io/acme/checkout-web:latest",
		"worker": "ghcr.io/acme/checkout-worker:latest",
	}}
	first := reportBuild(t, c, req).Error()
	for range 20 {
		if got := reportBuild(t, c, req).Error(); got != first {
			t.Fatalf("the refusal changed between identical requests:\n %s\n %s", first, got)
		}
	}
	if !strings.Contains(first, `"web"`) {
		t.Errorf("the refusal reads %q, want the alphabetically first component named", first)
	}
}

// A report is a deploy that CI starts, so a propose-only environment refuses
// it — policy.go files ReportBuild under `deploy` and explains at length why
// `build` would be precisely wrong. This is the test that makes that comment
// true: the report names no environment, so every stored environment of the
// project gets a say, and production's propose-only is the one that answers.
func TestReportBuildIsRefusedByProposeOnly(t *testing.T) {
	g := policyServer(t, Options{})
	agent := g.as(g.mint(t, "ci", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	err := reportBuild(t, agent, &kelsonv1alpha1.ReportBuildRequest{
		Project: "shop",
		Sha:     reportSHA,
		Images:  map[string]string{"web": pinnedImage()},
	})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a report into a propose-only project = %v (code %s), want permission-denied",
			err, connect.CodeOf(err))
	}
	if !hasCode(detailCodes(err), ErrPolicyProposeOnly) {
		t.Errorf("the refusal carries %v, want %s", detailCodes(err), ErrPolicyProposeOnly)
	}
}

// And a human is never refused by agent policy, which is the other half of
// ADR-0025's criterion: the report reaches the gate and is refused for the
// honest reason instead.
func TestReportBuildFromAHumanReachesTheGate(t *testing.T) {
	g := policyServer(t, Options{})
	err := reportBuild(t, g.as(testPassword), &kelsonv1alpha1.ReportBuildRequest{
		Project: "shop",
		Sha:     reportSHA,
		Images:  map[string]string{"web": pinnedImage()},
	})
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("a human's report = %v (code %s), want unimplemented", err, connect.CodeOf(err))
	}
}
