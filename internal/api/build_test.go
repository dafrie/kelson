package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview"
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

// ReportBuild is the CI hand-off (ADR-0034 decision 3): judge the report in
// full, decide whether kelson owns this project's images, and — for a change
// request — render and publish the preview through the publisher seam.
//
// Everything behind that seam is tested where it lives (internal/preview
// renders, packages and pushes; internal/delivery/flux stamps the reconcile
// annotation). What these tests hold the handler to is the decision and the
// wiring: which environments it chose, what it handed the publisher, what it
// answered, and which of the four refusals it gave.

const reportSHA = "9f0a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2"

// reportProjectDoc is a Project on the `ci` posture with two components, so a
// report can name one it declares and one it does not.
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
  build:
    by: ci
`

// reportEnvironmentDoc is an environment with no previews block. It exists
// because a report is guarded by agent policy (policy.go files ReportBuild
// under `deploy`), and policy is a property of the environment — so a stored
// project with no environments is a spec whose policy cannot be read, which is
// its own refusal.
const reportEnvironmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
`

// reportPreviewEnvDoc previews the project's own source repository, which is
// what makes it a target of a report about that repository.
func reportPreviewEnvDoc(name, repo string) []byte {
	return []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: ` + name + `}
spec:
  project: checkout
  delivery:
    mode: flux
    git: {repo: "git@github.com:acme/deploy.git", path: checkout/` + name + `}
  previews:
    provider: github
    repo: ` + repo + `
    secretRef: github-auth
    artifacts: {repository: "oci://ghcr.io/acme/checkout-previews"}
`)
}

// --- fakes ------------------------------------------------------------------

// fakePublisher stands in for internal/preview's Publisher. It records the
// options it was handed, which is the only way from this side of the fence to
// assert that the reported image pins reached the render.
type fakePublisher struct {
	mu    sync.Mutex
	calls []preview.Options
	// errs maps an environment name to the failure that environment's publish
	// answers with, so a partial-failure test can fail exactly one of two.
	errs map[string]error
	// reference overrides what the registry reports it now serves. The push is
	// where a resolved credential exists in this path, so it is where the
	// redaction test plants its sentinel.
	reference string
}

func (f *fakePublisher) Publish(_ context.Context, opts preview.Options) (*preview.Published, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, opts)
	if err := f.errs[opts.Environment.Metadata.Name]; err != nil {
		return nil, err
	}
	// The namespaces are the naming scheme's, spelled out rather than derived,
	// so a change to the scheme fails a test that says what it changed.
	name := opts.Project.Metadata.Name + "-" + opts.Environment.Metadata.Name
	reference := f.reference
	if reference == "" {
		reference = "ghcr.io/acme/checkout-previews@sha256:" + strings.Repeat("f", 64)
	}
	return &preview.Published{
		Set: &preview.Set{
			Project:         opts.Project.Metadata.Name,
			Environment:     opts.Environment.Metadata.Name,
			PR:              opts.PR,
			SHA:             opts.SHA,
			Namespace:       name + "-pr" + opts.PR,
			ParentNamespace: name,
			Repository:      "ghcr.io/acme/checkout-previews",
			Tag:             opts.SHA,
			// The hostnames a real render would rewrite for this change
			// request, spelled out rather than derived for the reason the
			// namespaces are: a change to naming.Host must fail a test that
			// says what it changed.
			Hosts: []string{"web-pr" + opts.PR + "." + opts.Environment.Metadata.Name + ".acme.run"},
		},
		Reference: reference,
	}, nil
}

func (f *fakePublisher) only(t *testing.T) preview.Options {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("the publisher was called %d times, want exactly once", len(f.calls))
	}
	return f.calls[0]
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakePoker records which ResourceSetInputProvider was asked to look now.
type fakePoker struct {
	mu    sync.Mutex
	poked []string
	err   error
}

func (f *fakePoker) Poke(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poked = append(f.poked, namespace+"/"+name)
	return f.err
}

func (f *fakePoker) objects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.poked)
}

// fakeOutcomes records what the handler wrote back to the forge: the commit
// statuses and the upserted change-request comments. externalURL stands in for
// the binary's --external-url, so a test can drive both link shapes without a
// server.
type fakeOutcomes struct {
	mu          sync.Mutex
	wrote       []CommitStatus
	comments    []PreviewComment
	err         error
	commentErr  error
	externalURL string
}

func (f *fakeOutcomes) ReportCommitStatus(_ context.Context, s CommitStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrote = append(f.wrote, s)
	return f.err
}

func (f *fakeOutcomes) UpsertPreviewComment(_ context.Context, c PreviewComment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, c)
	return f.commentErr
}

func (f *fakeOutcomes) Link(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.externalURL == "" || path == "" {
		return ""
	}
	return f.externalURL + path
}

func (f *fakeOutcomes) statuses() []CommitStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.wrote)
}

func (f *fakeOutcomes) written() []PreviewComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments)
}

// --- harness ----------------------------------------------------------------

// reportPlane is one server with every trigger seam faked, so a test can assert
// what the handler did as well as what it answered.
type reportPlane struct {
	clients
	publisher *fakePublisher
	poker     *fakePoker
	outcomes  *fakeOutcomes
	// specs is the store itself, because the tracking half of a report writes
	// its whole outcome there (autodeploy.go): the pins go into the environment
	// document and the controller renders from it, so what the trigger did is
	// read back out of the store rather than off a publisher.
	specs *fakeSpecStore
}

// reportServerWith stores one project and serves it with the trigger seams
// wired. envs is keyed by environment name, as the spec store holds them.
func reportServerWith(t *testing.T, project string, envs map[string][]byte) reportPlane {
	t.Helper()
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project:      []byte(project),
		Environments: envs,
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	plane := reportPlane{
		publisher: &fakePublisher{errs: map[string]error{}},
		poker:     &fakePoker{},
		outcomes:  &fakeOutcomes{externalURL: "https://kelson.acme.com"},
		specs:     specs,
	}
	plane.clients = serve(t, Options{
		Specs:    specs,
		Publish:  plane.publisher,
		Poke:     plane.poker,
		Outcomes: plane.outcomes,
	})
	return plane
}

// reportServer is the validation harness: the `ci` project and one environment
// with no previews, which is enough to judge a report and not enough to publish
// one.
func reportServer(t *testing.T) clients {
	t.Helper()
	return reportServerWith(t, reportProjectDoc,
		map[string][]byte{"production": []byte(reportEnvironmentDoc)}).clients
}

// previewReportServer is the publishing harness: the same project with an
// environment that previews its source repository.
func previewReportServer(t *testing.T) reportPlane {
	t.Helper()
	return reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
	})
}

func reportBuild(t *testing.T, c clients, req *kelsonv1alpha1.ReportBuildRequest) error {
	t.Helper()
	_, err := c.builds.ReportBuild(t.Context(), connect.NewRequest(req))
	return err
}

// report drives a successful report and returns the answer.
func report(t *testing.T, c clients, req *kelsonv1alpha1.ReportBuildRequest) *kelsonv1alpha1.ReportBuildResponse {
	t.Helper()
	res, err := c.builds.ReportBuild(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ReportBuild: %v", err)
	}
	return res.Msg
}

func pinnedImage() string {
	return "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("a", 64)
}

// previewReport is the report a pipeline sends for a pull request build.
func previewReport() *kelsonv1alpha1.ReportBuildRequest {
	return &kelsonv1alpha1.ReportBuildRequest{
		Project: "checkout",
		Sha:     reportSHA,
		Ref:     "refs/pull/412/head",
		Pr:      412,
		Images:  map[string]string{"web": pinnedImage()},
	}
}

// --- the live half: change requests ----------------------------------------

// The whole hand-off in one test: a report for a pull request renders that pull
// request's preview with the reported digests, publishes it, and asks
// flux-operator to look now instead of at previews.interval.
func TestReportBuildPublishesTheChangeRequestPreview(t *testing.T) {
	p := previewReportServer(t)
	res := report(t, p.clients, previewReport())

	if !res.GetAccepted() {
		t.Errorf("accepted = false for a report kelson acted on: %s", res.GetMessage())
	}
	if got := res.GetTriggered(); len(got) != 1 || got[0] != "checkout-staging-pr412" {
		t.Errorf("triggered = %v, want the preview's own identifier", got)
	}

	opts := p.publisher.only(t)
	if opts.PR != "412" || opts.SHA != reportSHA {
		t.Errorf("the publisher was asked for pr %q at %q, want 412 at %s", opts.PR, opts.SHA, reportSHA)
	}
	if opts.Environment.Metadata.Name != "staging" || opts.Project.Metadata.Name != "checkout" {
		t.Errorf("the publisher was handed %s/%s", opts.Project.Metadata.Name, opts.Environment.Metadata.Name)
	}
	// The reported pins are the point of the hand-off: the preview must run the
	// image CI built for this commit, not the one the spec resolves.
	if got := opts.Images["web"]; got != pinnedImage() {
		t.Errorf("the publisher was handed image %q for web, want the reported digest %s", got, pinnedImage())
	}
	if got := p.poker.objects(); len(got) != 1 || got[0] != "checkout-staging/checkout-staging-previews" {
		t.Errorf("poked %v, want the environment's ResourceSetInputProvider", got)
	}
}

// The message says why `triggered` is what it is, which is the field's whole
// job: "a pipeline whose report silently did nothing is the failure mode this
// field exists to prevent".
func TestReportBuildMessageNamesWhatItPublished(t *testing.T) {
	p := previewReportServer(t)
	res := report(t, p.clients, previewReport())
	for _, want := range []string{"412", "checkout-staging-pr412"} {
		if !strings.Contains(res.GetMessage(), want) {
			t.Errorf("the message does not mention %q: %s", want, res.GetMessage())
		}
	}
}

// A report for a project whose environments preview nothing is an ordinary
// answer, not a failure — and it says what is missing rather than saying
// nothing.
func TestReportBuildWithNoPreviewsSaysWhatWouldHaveMatched(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{"production": []byte(reportEnvironmentDoc)})
	res := report(t, p.clients, previewReport())

	if !res.GetAccepted() || len(res.GetTriggered()) != 0 {
		t.Fatalf("accepted=%v triggered=%v, want an accepted report that triggered nothing",
			res.GetAccepted(), res.GetTriggered())
	}
	if !strings.Contains(res.GetMessage(), "spec.previews") {
		t.Errorf("the message does not name the field that would have matched: %s", res.GetMessage())
	}
	if p.publisher.count() != 0 {
		t.Error("something was published for a project with no previews")
	}
}

// `previews.repo` is the source repository and ADR-0017 decision 1 defaults
// between it and everything else not at all, so an environment previewing some
// other repository is not a target of this report — and the answer says which
// one it previews.
func TestReportBuildSkipsEnvironmentsPreviewingAnotherRepository(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/other"),
	})
	res := report(t, p.clients, previewReport())

	if len(res.GetTriggered()) != 0 || p.publisher.count() != 0 {
		t.Fatalf("published into an environment previewing another repository: %v", res.GetTriggered())
	}
	if !strings.Contains(res.GetMessage(), "acme/other") {
		t.Errorf("the message does not name the repository that environment previews: %s", res.GetMessage())
	}
}

// The same repository written two ways is one repository: a spec that spells
// its source `git@github.com:acme/checkout.git` still matches previews of
// `https://github.com/acme/checkout`.
func TestReportBuildMatchesRepositoriesAcrossSpellings(t *testing.T) {
	project := strings.Replace(reportProjectDoc,
		"git: https://github.com/acme/checkout", `git: "git@github.com:acme/Checkout.git"`, 1)
	p := reportServerWith(t, project, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
	})
	if got := report(t, p.clients, previewReport()).GetTriggered(); len(got) != 1 {
		t.Errorf("triggered = %v, want the preview published for the same repository written two ways", got)
	}
}

// Partial failure: publish what can be published, and name what could not with
// the cause the publisher gave.
func TestReportBuildPublishesWhatItCanAndNamesWhatFailed(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		"canary":  reportPreviewEnvDoc("canary", "https://github.com/acme/checkout"),
	})
	p.publisher.errs["canary"] = errors.New("403 Forbidden pushing to ghcr.io/acme/checkout-previews")
	res := report(t, p.clients, previewReport())

	if got := res.GetTriggered(); len(got) != 1 || got[0] != "checkout-staging-pr412" {
		t.Fatalf("triggered = %v, want only the environment that published", got)
	}
	if !res.GetAccepted() {
		t.Error("accepted = false; a publish that failed is not a report kelson declined to act on")
	}
	for _, want := range []string{"canary", "403 Forbidden"} {
		if !strings.Contains(res.GetMessage(), want) {
			t.Errorf("the message does not carry %q: %s", want, res.GetMessage())
		}
	}
}

// …and when nothing could be published at all, the RPC fails. `accepted: true`
// with nothing triggered where something was meant to be is precisely the green
// pipeline that published nothing.
func TestReportBuildFailsWhenNothingCouldBePublished(t *testing.T) {
	p := previewReportServer(t)
	p.publisher.errs["staging"] = errors.New("connection refused")

	err := reportBuild(t, p.clients, previewReport())
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v (code %s), want failed-precondition", err, connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the failure does not carry the publisher's own cause: %v", err)
	}
}

// A spec with overlays cannot be rendered server-side — overlay paths resolve
// against the files they were authored beside — and publishing an artifact
// silently missing them would come up wrong rather than not come up.
func TestReportBuildRefusesToPublishASpecWithOverlays(t *testing.T) {
	env := append(reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		[]byte("  overlays:\n    - patch: ./patches/staging.yaml\n")...)
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{"staging": env})

	err := reportBuild(t, p.clients, previewReport())
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v (code %s), want failed-precondition", err, connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "overlays") {
		t.Errorf("the refusal does not name overlays: %v", err)
	}
	if p.publisher.count() != 0 {
		t.Error("a spec with overlays was published anyway")
	}
}

// A poke is never load-bearing: losing one costs previews.interval of latency
// and never correctness, so it is reported and not fatal.
func TestReportBuildSurvivesAFailedPoke(t *testing.T) {
	p := previewReportServer(t)
	p.poker.err = errors.New(`resourcesetinputproviders.fluxcd.controlplane.io "checkout-staging-previews" not found`)
	res := report(t, p.clients, previewReport())

	if len(res.GetTriggered()) != 1 {
		t.Fatalf("triggered = %v; a failed poke unpublished the preview", res.GetTriggered())
	}
	if !strings.Contains(res.GetMessage(), "previews.interval") {
		t.Errorf("the message does not say what the lost poke costs: %s", res.GetMessage())
	}
}

// A server with no publisher refuses rather than accepting and dropping the
// report, which is the rule this method has always kept.
func TestReportBuildWithoutAPublisherIsUnimplemented(t *testing.T) {
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project: []byte(reportProjectDoc),
		Environments: map[string][]byte{
			"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	err := reportBuild(t, serve(t, Options{Specs: specs}), previewReport())
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v (code %s), want unimplemented", err, connect.CodeOf(err))
	}
}

// Idempotency at this seam: a replayed report asks for the same publish and
// gets the same answer. That the *artifact* is byte-identical is internal/
// preview's property and tested there; what matters here is that the handler
// adds no second effect and no second answer of its own.
func TestReportBuildReplayIsTheSameReport(t *testing.T) {
	p := previewReportServer(t)
	req := previewReport()
	req.IdempotencyKey = "gha-run-9912"

	first := report(t, p.clients, req)
	second := report(t, p.clients, req)
	if first.GetMessage() != second.GetMessage() || !slices.Equal(first.GetTriggered(), second.GetTriggered()) {
		t.Fatalf("a replayed report answered differently:\n %v %s\n %v %s",
			first.GetTriggered(), first.GetMessage(), second.GetTriggered(), second.GetMessage())
	}
	p.publisher.mu.Lock()
	defer p.publisher.mu.Unlock()
	a, b := p.publisher.calls[0], p.publisher.calls[1]
	if a.PR != b.PR || a.SHA != b.SHA || !maps.Equal(a.Images, b.Images) {
		t.Error("the two publishes were asked for different inputs, so they would not deduplicate in the registry")
	}
}

// --- the commit status (ADR-0034 decision 5) --------------------------------

// A successful publish is written back onto the commit, under a context stable
// enough for a branch-protection rule to name, and linking the preview's own
// page rather than the project it belongs to.
func TestReportBuildWritesTheCommitStatus(t *testing.T) {
	p := previewReportServer(t)
	report(t, p.clients, previewReport())

	wrote := p.outcomes.statuses()
	if len(wrote) != 1 {
		t.Fatalf("wrote %d statuses, want one for the repository the previews are about", len(wrote))
	}
	got := wrote[0]
	if got.Repo != "https://github.com/acme/checkout" || got.SHA != reportSHA {
		t.Errorf("status is about %s@%s, want the previewed repository at the reported commit", got.Repo, got.SHA)
	}
	if got.State != "success" || got.Context != PreviewStatusContext {
		t.Errorf("status = %s under %q, want success under %s", got.State, got.Context, PreviewStatusContext)
	}
	// The route the UI serves (ui/src/App.tsx: projects/:project/:env/previews/:pr),
	// with the change request's own number as its id — the same value
	// Preview.id carries on the wire.
	if want := "/projects/checkout/staging/previews/412"; got.Path != want {
		t.Errorf("status links to %q, want the preview detail page %q", got.Path, want)
	}
}

// Two environments previewing one repository still write one status — a forge
// keys them by context — and it links a page that exists, deterministically the
// first environment in name order rather than whichever published first.
func TestReportBuildStatusLinksOnePreviewPageForTwoEnvironments(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		"canary":  reportPreviewEnvDoc("canary", "https://github.com/acme/checkout"),
	})
	report(t, p.clients, previewReport())

	wrote := p.outcomes.statuses()
	if len(wrote) != 1 {
		t.Fatalf("wrote %d statuses, want one for the one repository both environments preview", len(wrote))
	}
	if want := "/projects/checkout/canary/previews/412"; wrote[0].Path != want {
		t.Errorf("status links to %q, want %q", wrote[0].Path, want)
	}
}

// A repository the spec previews but nothing published into has no preview page
// to link, so its check falls back to the project rather than to a page that
// would report nothing.
func TestReportBuildStatusFallsBackToTheProjectWhenNothingPublished(t *testing.T) {
	project := strings.Replace(reportProjectDoc,
		"  source:\n    git: https://github.com/acme/checkout\n    ref: main\n",
		"  sources:\n    - name: app\n      git: https://github.com/acme/checkout\n      ref: main\n"+
			"    - name: docs\n      git: https://github.com/acme/checkout-docs\n      ref: main\n", 1)
	p := reportServerWith(t, project, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		"docs":    reportPreviewEnvDoc("docs", "https://github.com/acme/checkout-docs"),
	})
	p.publisher.errs["docs"] = errors.New("403 Forbidden")
	report(t, p.clients, previewReport())

	paths := map[string]string{}
	for _, got := range p.outcomes.statuses() {
		paths[got.Repo] = got.Path
	}
	if want := "/projects/checkout/staging/previews/412"; paths["https://github.com/acme/checkout"] != want {
		t.Errorf("the published repository's check links to %q, want %q", paths["https://github.com/acme/checkout"], want)
	}
	if want := "/projects/checkout"; paths["https://github.com/acme/checkout-docs"] != want {
		t.Errorf("the repository nothing published into links to %q, want the project page %q",
			paths["https://github.com/acme/checkout-docs"], want)
	}
}

// A partial publish is not a success, and the check says so.
func TestReportBuildStatusIsFailureWhenAnEnvironmentFailed(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		"canary":  reportPreviewEnvDoc("canary", "https://github.com/acme/checkout"),
	})
	p.publisher.errs["canary"] = errors.New("403 Forbidden")
	report(t, p.clients, previewReport())

	wrote := p.outcomes.statuses()
	if len(wrote) != 1 || wrote[0].State != "failure" {
		t.Fatalf("statuses = %+v, want one failure", wrote)
	}
}

// Absence degrades to nothing, silently: a write-back is a courtesy of the
// integration, not a delivery dependency.
func TestReportBuildPublishesWithoutAStatusReporter(t *testing.T) {
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project: []byte(reportProjectDoc),
		Environments: map[string][]byte{
			"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	publisher := &fakePublisher{errs: map[string]error{}}
	c := serve(t, Options{Specs: specs, Publish: publisher})
	if got := report(t, c, previewReport()).GetTriggered(); len(got) != 1 {
		t.Errorf("triggered = %v; a server with no status reporter published nothing", got)
	}
}

// A reporter that was asked and failed is different from one that is absent:
// something is configured and broken, and the report says so without pretending
// the publish did not happen.
func TestReportBuildNamesAFailedStatusWriteBack(t *testing.T) {
	p := previewReportServer(t)
	p.outcomes.err = errors.New("404 Not Found (statuses:write not granted)")
	res := report(t, p.clients, previewReport())

	if len(res.GetTriggered()) != 1 {
		t.Fatalf("triggered = %v; a failed status write-back unpublished the preview", res.GetTriggered())
	}
	if !strings.Contains(res.GetMessage(), "statuses:write") {
		t.Errorf("the message does not carry the forge's own refusal: %s", res.GetMessage())
	}
}

// --- the preview comment (ADR-0034 decision 5) ------------------------------

// The comment ADR-0034 asks for: one per preview, upserted, carrying the hosts,
// what was published and where the live answer is.
func TestReportBuildUpsertsThePreviewComment(t *testing.T) {
	p := previewReportServer(t)
	report(t, p.clients, previewReport())

	written := p.outcomes.written()
	if len(written) != 1 {
		t.Fatalf("wrote %d comments, want one for the one preview published", len(written))
	}
	got := written[0]
	if got.Repo != "https://github.com/acme/checkout" || got.FullName != "acme/checkout" || got.PR != 412 {
		t.Errorf("comment is on %s (%s) #%d, want the previewed repository's change request 412",
			got.Repo, got.FullName, got.PR)
	}
	if want := "<!-- kelson:preview:checkout-staging -->"; got.Marker != want {
		t.Errorf("marker = %q, want %q", got.Marker, want)
	}
	if !strings.Contains(got.Body, got.Marker) {
		t.Errorf("the body lost its marker, so the next publish would post a second comment:\n%s", got.Body)
	}
	for _, want := range []string{
		"[web-pr412.staging.acme.run](https://web-pr412.staging.acme.run)", // the hosts, as links
		reportSHA,                         // the commit it was published for
		"ghcr.io/acme/checkout-previews@", // the revision the registry now serves
		"checkout-staging-pr412",          // the namespace it lands in
		"Phase at publish: **published**", // what kelson knows, and no more
		"https://kelson.acme.com/projects/checkout/staging/previews/412", // the detail page
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("the comment body does not carry %q:\n%s", want, got.Body)
		}
	}
}

// A server with no external URL has no absolute link to give, and says where to
// look in relative terms rather than guessing an origin — the rule the check's
// own missing target_url keeps.
func TestPreviewCommentWithoutAnExternalURLSaysWhereToLook(t *testing.T) {
	p := previewReportServer(t)
	p.outcomes.externalURL = ""
	report(t, p.clients, previewReport())

	written := p.outcomes.written()
	if len(written) != 1 {
		t.Fatalf("wrote %d comments, want one", len(written))
	}
	body := written[0].Body
	if strings.Contains(body, "http://") || strings.Contains(body, "https://kelson") {
		t.Errorf("the comment invented an origin for this server:\n%s", body)
	}
	for _, want := range []string{"no external URL", "project checkout", "environment staging", "preview 412"} {
		if !strings.Contains(body, want) {
			t.Errorf("the comment does not say %q:\n%s", want, body)
		}
	}
}

// Two environments previewing one pull request keep two comments, not one they
// take turns overwriting: the marker names the environment.
func TestReportBuildKeepsOneCommentPerEnvironment(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		"canary":  reportPreviewEnvDoc("canary", "https://github.com/acme/checkout"),
	})
	report(t, p.clients, previewReport())

	markers := map[string]bool{}
	for _, c := range p.outcomes.written() {
		markers[c.Marker] = true
	}
	for _, want := range []string{"<!-- kelson:preview:checkout-staging -->", "<!-- kelson:preview:checkout-canary -->"} {
		if !markers[want] {
			t.Errorf("no comment carries %q; the two environments would fight over one comment: %v", want, markers)
		}
	}
	if len(markers) != 2 {
		t.Errorf("markers = %v, want one per environment", markers)
	}
}

// An environment that published nothing has nothing to say on the change
// request, and says nothing.
func TestReportBuildCommentsOnlyForWhatPublished(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		"canary":  reportPreviewEnvDoc("canary", "https://github.com/acme/checkout"),
	})
	p.publisher.errs["canary"] = errors.New("403 Forbidden")
	report(t, p.clients, previewReport())

	written := p.outcomes.written()
	if len(written) != 1 || !strings.Contains(written[0].Marker, "staging") {
		t.Fatalf("comments = %+v, want only the environment that published", written)
	}
}

// The same silent degradation the status keeps: no reporter wired, nothing
// written, nothing said, and the preview is published either way.
func TestReportBuildPublishesWithoutACommenter(t *testing.T) {
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project: []byte(reportProjectDoc),
		Environments: map[string][]byte{
			"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	c := serve(t, Options{Specs: specs, Publish: &fakePublisher{errs: map[string]error{}}})
	res := report(t, c, previewReport())
	if len(res.GetTriggered()) != 1 {
		t.Errorf("triggered = %v; a server with no commenter published nothing", res.GetTriggered())
	}
	if strings.Contains(res.GetMessage(), "comment") {
		t.Errorf("an absent commenter was reported as a problem: %s", res.GetMessage())
	}
}

// …and the one that is not silent: a comment kelson was asked to write and
// could not. `pull_requests:write` is a permission an installation may not have
// been granted (ADR-0034's "users who install with less get silently fewer
// features"), and that is exactly the case an operator has to be able to see.
func TestReportBuildNamesAFailedComment(t *testing.T) {
	p := previewReportServer(t)
	p.outcomes.commentErr = errors.New("403 Forbidden (pull_requests:write not granted)")
	res := report(t, p.clients, previewReport())

	if len(res.GetTriggered()) != 1 {
		t.Fatalf("triggered = %v; a failed comment unpublished the preview", res.GetTriggered())
	}
	for _, want := range []string{"staging", "pull_requests:write"} {
		if !strings.Contains(res.GetMessage(), want) {
			t.Errorf("the message does not carry %q: %s", want, res.GetMessage())
		}
	}
}

// --- build.by (ADR-0034 decision 3) -----------------------------------------

// A project kelson builds for itself declines the report rather than publishing
// over its own build plane. It is `accepted: false` and not an error code,
// because that is the meaning the schema pins to the field.
func TestReportBuildDeclinesAKelsonBuiltProject(t *testing.T) {
	for _, tc := range []struct {
		name, by, says string
	}{
		{name: "explicit", by: "  build:\n    by: kelson\n", says: "spec.build.by: kelson"},
		{name: "defaulted for a project with a source", by: "", says: "defaults to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := strings.Replace(reportProjectDoc, "  build:\n    by: ci\n", tc.by, 1)
			p := reportServerWith(t, project, map[string][]byte{
				"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
			})
			res := report(t, p.clients, previewReport())

			if res.GetAccepted() || len(res.GetTriggered()) != 0 {
				t.Fatalf("accepted=%v triggered=%v, want a declined report", res.GetAccepted(), res.GetTriggered())
			}
			if !strings.Contains(res.GetMessage(), tc.says) || !strings.Contains(res.GetMessage(), "by: ci") {
				t.Errorf("the message does not name the field and its remedy: %s", res.GetMessage())
			}
			if p.publisher.count() != 0 {
				t.Error("a declined report published something anyway")
			}
		})
	}
}

// A project kelson cannot build has no build plane to publish over, so the
// default is read as ADR-0034 wrote it — "`by: kelson` (default for projects
// with `source:`)" — rather than as "empty means kelson, always". Declining
// here would state a reason that is false.
func TestReportBuildAcceptsAProjectKelsonCannotBuild(t *testing.T) {
	for _, tc := range []struct{ name, project string }{
		{name: "no source at all", project: strings.NewReplacer(
			"  build:\n    by: ci\n", "",
			"  source:\n    git: https://github.com/acme/checkout\n    ref: main\n", "").Replace(reportProjectDoc)},
		{name: "strategy none", project: strings.Replace(reportProjectDoc,
			"  build:\n    by: ci\n", "  build:\n    strategy: none\n", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := reportServerWith(t, tc.project, map[string][]byte{
				"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
			})
			res := report(t, p.clients, previewReport())
			if !res.GetAccepted() || len(res.GetTriggered()) != 1 {
				t.Fatalf("accepted=%v triggered=%v message=%s, want the report acted on",
					res.GetAccepted(), res.GetTriggered(), res.GetMessage())
			}
		})
	}
}

// --- the tracking half -------------------------------------------------------

// A report with no `pr` and a ref no environment follows is accepted and
// triggers nothing. It is the ordinary shape of a report for a feature branch,
// and the schema pins the answer: `accepted: true` with an empty `triggered`,
// and a message saying what would have made it move.
func TestReportBuildAtARefNothingFollows(t *testing.T) {
	p := previewReportServer(t)
	res := report(t, p.clients, &kelsonv1alpha1.ReportBuildRequest{
		Project: "checkout",
		Sha:     reportSHA,
		Ref:     "refs/heads/main",
		Images:  map[string]string{"web": pinnedImage()},
	})
	if !res.GetAccepted() || len(res.GetTriggered()) != 0 {
		t.Fatalf("accepted=%v triggered=%v, want an accepted report that moved nothing",
			res.GetAccepted(), res.GetTriggered())
	}
	if !strings.Contains(res.GetMessage(), "autoDeploy") {
		t.Errorf("the message does not name the opt-in that would have moved it: %s", res.GetMessage())
	}
	if p.publisher.count() != 0 {
		t.Error("a branch report published a preview")
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
// partial report is a partial pin rather than a broken render. The publisher is
// handed exactly the components that were reported and no others.
func TestReportBuildAcceptsAPartialReport(t *testing.T) {
	p := previewReportServer(t)
	req := previewReport()
	req.Images = map[string]string{"web": pinnedImage()}
	report(t, p.clients, req)

	if got := p.publisher.only(t).Images; !maps.Equal(got, req.Images) {
		t.Errorf("the publisher was handed %v, want only the reported component", got)
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
// ADR-0025's criterion: the report reaches the handler and is answered on its
// merits instead. This one carries neither `pr` nor `ref`, so what it reaches is
// the tracking half answering that there is nothing for it to follow.
func TestReportBuildFromAHumanReachesTheGate(t *testing.T) {
	g := policyServer(t, Options{})
	res, err := g.as(testPassword).builds.ReportBuild(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ReportBuildRequest{
			Project: "shop",
			Sha:     reportSHA,
			Images:  map[string]string{"web": pinnedImage()},
		}))
	if err != nil {
		t.Fatalf("a human's report = %v (code %s), want the handler's own answer", err, connect.CodeOf(err))
	}
	if !res.Msg.GetAccepted() || len(res.Msg.GetTriggered()) != 0 {
		t.Fatalf("accepted=%v triggered=%v, want an accepted report with no target",
			res.Msg.GetAccepted(), res.Msg.GetTriggered())
	}
}

// --- per-component sources (ADR-0035) ---------------------------------------

// The Project ADR-0035 exists for: two declared sources, and a component bound
// to the second one. Before this slice it was refused with build/no-source,
// because the build path read `spec.source` and this document has none.
const buildPluralProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: shop
spec:
  sources:
    - name: app
      git: https://github.com/acme/shop.git
      ref: main
    - name: tools
      git: https://gitlab.com/acme/build-tools.git
      ref: v2
      connection: acme-gitlab
  build:
    strategy: dockerfile
  components:
    - name: worker
      source: tools
`

// fakeGitSources is the instance's global tier, in memory.
type fakeGitSources struct {
	sources []model.Source
	err     error
}

func (f fakeGitSources) ListSources(context.Context) ([]model.Source, error) {
	return f.sources, f.err
}

// A project spelling its sources in the plural builds, and it builds the source
// its component is bound to — repository, ref and connection — rather than the
// first one declared.
func TestBuildFollowsTheComponentsSourceBinding(t *testing.T) {
	builder := &fakeBuilder{}
	revisions := &fakeRevisions{}
	var target BuildTarget
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, revisions, &target),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	out, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        inlineSpec(buildPluralProjectDoc, map[string]string{"production": buildEnvironmentDoc}),
		Environment: "production",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.finished == nil {
		t.Fatal("the build produced no Finished event")
	}

	req := builder.lastRequest(t)
	if req.SourceGit != "https://gitlab.com/acme/build-tools.git" {
		t.Errorf("SourceGit = %q, want the bound source's repository", req.SourceGit)
	}
	if req.SourceName != "tools" {
		t.Errorf("SourceName = %q, want tools", req.SourceName)
	}
	if req.SourceConnection != "acme-gitlab" {
		t.Errorf("SourceConnection = %q, want the bound source's connection", req.SourceConnection)
	}
	// The credential the clone will mint and the credential the ref resolver
	// already used have to be the same one, which is why the connection also
	// travels on the target the plane was built from.
	if target.SourceConnection != "acme-gitlab" {
		t.Errorf("target.SourceConnection = %q, want the bound source's connection", target.SourceConnection)
	}
	// And the ls-remote asked about that repository at *its* ref, not the
	// project's first.
	if asked := revisions.calls(); len(asked) != 1 ||
		asked[0] != [2]string{"https://gitlab.com/acme/build-tools.git", "v2"} {
		t.Errorf("resolver calls = %v, want one call for the bound source at v2", asked)
	}
}

// The request's `ref` still overrides, and it overrides the *source's* ref.
func TestBuildRefOverridesTheSourcesRef(t *testing.T) {
	revisions := &fakeRevisions{}
	c := serve(t, Options{
		Build:         buildPlaneFor(&fakeBuilder{}, revisions, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	if _, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        inlineSpec(buildPluralProjectDoc, map[string]string{"production": buildEnvironmentDoc}),
		Environment: "production",
		Ref:         "release/v3",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if asked := revisions.calls(); len(asked) != 1 || asked[0][1] != "release/v3" {
		t.Errorf("resolver calls = %v, want the request's ref", asked)
	}
}

// A component bound to a GitSource the instance offers resolves through the
// global tier the server supplies — the whole reason the build path passes
// globals to the resolver at all.
func TestBuildResolvesAGlobalGitSource(t *testing.T) {
	const doc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: shop
spec:
  build:
    strategy: dockerfile
  components:
    - name: web
      port: 8080
      source: platform
`
	builder := &fakeBuilder{}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
		GitSources: fakeGitSources{sources: []model.Source{{
			Name: "platform", Git: "https://github.com/acme/platform.git", Ref: "release",
		}}},
	})

	if _, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        inlineSpec(doc, map[string]string{"production": buildEnvironmentDoc}),
		Environment: "production",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := builder.lastRequest(t).SourceGit; got != "https://github.com/acme/platform.git" {
		t.Errorf("SourceGit = %q, want the instance's GitSource", got)
	}
}

// Without the global tier the same spec is refused by the *resolver*, with the
// code and the remediation ADR-0035 decision 3 specifies — it names both halves
// of the scope it searched, so "I forgot to declare it" and "the instance does
// not offer it" are told apart without a second lookup.
func TestBuildSurfacesAnUnknownSource(t *testing.T) {
	const doc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: shop
spec:
  build:
    strategy: dockerfile
  components:
    - name: web
      port: 8080
      source: platform
`
	builder := &fakeBuilder{}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        inlineSpec(doc, map[string]string{"production": buildEnvironmentDoc}),
		Environment: "production",
	})
	if got := detailCode(t, err); got != string(model.ErrUnknownSource) {
		t.Fatalf("detail code = %q, want %q", got, model.ErrUnknownSource)
	}
	// The remediation is the half that makes the code actionable, and it rides
	// the structured detail rather than the message — so that is where it is
	// asserted, naming both halves of the scope the resolver searched.
	remediation := detailRemediation(t, err)
	for _, want := range []string{"This project declares", "This instance offers"} {
		if !strings.Contains(remediation, want) {
			t.Errorf("the remediation should survive to the wire (%q): %s", want, remediation)
		}
	}
	if builder.calls() != 0 {
		t.Error("a spec that does not resolve must not reach the build plane")
	}
}

// Two components on one source are one build: the clone is shared rather than
// repeated, which is what makes a project cloning several repositories per
// revision different from one cloning several copies of the same.
func TestBuildSharesOneSourceAcrossComponents(t *testing.T) {
	const doc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: shop
spec:
  sources:
    - name: app
      git: https://github.com/acme/shop.git
      ref: main
  build:
    strategy: dockerfile
  components:
    - name: web
      port: 8080
      source: app
    - name: worker
      source: app
`
	builder := &fakeBuilder{}
	revisions := &fakeRevisions{}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, revisions, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	if _, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        inlineSpec(doc, map[string]string{"production": buildEnvironmentDoc}),
		Environment: "production",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if builder.calls() != 1 {
		t.Errorf("the builder was called %d times, want one build for the shared source", builder.calls())
	}
	if len(revisions.calls()) != 1 {
		t.Errorf("ls-remote ran %d times, want once per distinct source", len(revisions.calls()))
	}
	if got := builder.lastRequest(t).Component; got != "" {
		t.Errorf("Component = %q, want empty: one build serves both components", got)
	}
}

// And a project whose components build from *different* repositories is
// refused by name. One build pushes one image and model rule P3 pins one image
// per project, so building half of it would be worse than saying so.
func TestBuildRefusesSeveralSources(t *testing.T) {
	const doc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: shop
spec:
  sources:
    - name: app
      git: https://github.com/acme/shop.git
      ref: main
    - name: tools
      git: https://gitlab.com/acme/build-tools.git
      ref: v2
  build:
    strategy: dockerfile
  components:
    - name: web
      port: 8080
      source: app
    - name: worker
      source: tools
`
	builder := &fakeBuilder{}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        inlineSpec(doc, map[string]string{"production": buildEnvironmentDoc}),
		Environment: "production",
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if got := detailCode(t, err); got != build.ReasonSeveralSources {
		t.Errorf("detail code = %q, want %q", got, build.ReasonSeveralSources)
	}
	if builder.calls() != 0 {
		t.Error("a refusal must not reach the build plane")
	}
}

// A GitSource listing that fails is reported rather than degraded into an empty
// global tier: "this instance offers nothing" is a true sentence about a lookup
// that failed and a false one about the instance.
func TestBuildReportsAFailedGitSourceListing(t *testing.T) {
	builder := &fakeBuilder{}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
		GitSources:    fakeGitSources{err: errors.New("the API server said no")},
	})

	_, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        buildSpec(),
		Environment: "production",
	})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable (err %v)", connect.CodeOf(err), err)
	}
	if builder.calls() != 0 {
		t.Error("nothing may be built when the global tier could not be read")
	}
}

// detailRemediation reads the structured remediation off the first error
// detail. It is separate from detailCode because the two are separate promises:
// the code is what an agent branches on, the remediation is what a human acts
// on, and a refusal that carries one without the other is half a refusal.
func detailRemediation(t *testing.T, err error) string {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("not a connect error: %v", err)
	}
	for _, d := range cerr.Details() {
		msg, verr := d.Value()
		if verr != nil {
			continue
		}
		if wire, ok := msg.(*kelsonv1alpha1.Error); ok {
			return wire.GetRemediation()
		}
	}
	t.Fatalf("no kelson.v1alpha1.Error detail on %v", err)
	return ""
}
