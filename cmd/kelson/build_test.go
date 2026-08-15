package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/detect"
	"github.com/dafrie/kelson/internal/model"
)

// `kelson build` is assembly (issues #48, #51): load the spec, resolve the
// strategy and the revision, derive the destination, and hand one
// build.Request to the plane. The driver and the executor are tested where
// they live — the command plane's lint allow-list keeps client-go and go-git
// out of cmd — so these tests drive the command through the buildConnector
// seam and assert the wiring: what reaches the builder, what reaches the
// resolver, what prints, and what the exit code is.

// --- fakes ------------------------------------------------------------------

type fakeBuilder struct {
	mu     sync.Mutex
	reqs   []build.Request
	logs   string
	result build.Result
	err    error
}

func (f *fakeBuilder) Name() string { return "dockerfile" }

func (f *fakeBuilder) Build(_ context.Context, req build.Request, w io.Writer) (build.Result, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	logs, res, err := f.logs, f.result, f.err
	f.mu.Unlock()

	if w != nil && logs != "" {
		if _, werr := io.WriteString(w, logs); werr != nil {
			return build.Result{}, werr
		}
	}
	if err != nil {
		return build.Result{}, err
	}
	if res.Reference == "" {
		res = build.Result{Reference: testRepository + "@" + testBuildDigest, Digest: testBuildDigest, Tag: req.Tag}
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

// fakeResolver stands in for the delivery plane's git ls-remote.
type fakeResolver struct {
	mu    sync.Mutex
	calls [][2]string
	hash  string
	err   error
}

func (f *fakeResolver) Resolve(_ context.Context, repo, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{repo, ref})
	if f.err != nil {
		return "", f.err
	}
	if f.hash == "" {
		return testCommit, nil
	}
	return f.hash, nil
}

func (f *fakeResolver) resolved() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.calls...)
}

const (
	testSourceGit    = "https://github.com/acme/shop.git"
	testRepository   = "ghcr.io/acme/shop"
	testCommit       = "0123456789abcdef0123456789abcdef01234567"
	testBuildDigest  = "sha256:aaaabbbbccccddddeeeeffff00001111aaaabbbbccccddddeeeeffff00001111"
	testPinnedResult = testRepository + "@" + testBuildDigest
)

// --- harness ----------------------------------------------------------------

// buildPlaneFor returns a connector serving the given fakes and records the
// target the command derived from its flags and spec.
func buildPlaneFor(builder *fakeBuilder, resolver *fakeResolver, seen *buildTarget) buildConnector {
	return func(t buildTarget) (*buildPlane, error) {
		if seen != nil {
			*seen = t
		}
		return &buildPlane{builder: builder, revisions: resolver}, nil
	}
}

func rootWithBuildPlane(connect buildConnector) *cobra.Command {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "build" {
			root.RemoveCommand(c)
		}
	}
	root.AddCommand(newBuildCmdFactory(connect))
	return root
}

func runBuildCmd(t *testing.T, connect buildConnector, args ...string) (stdout, stderr string, code int, msg string) {
	t.Helper()
	cmd := rootWithBuildPlane(connect)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), errBuf.String(), code, msg
}

// buildSpecOptions tunes the spec fixture: the build stanza and whether the
// Project has a source at all.
type buildSpecOptions struct {
	strategy   string
	dockerfile string
	ref        string
	noSource   bool
	image      string
}

func writeBuildSpec(t *testing.T, opts buildSpecOptions) string {
	t.Helper()
	// A Project with spec.source but no spec.build fails model validation with
	// semantic/no-image-source, so a source-bearing fixture always carries a
	// build stanza; "auto" is the strategy the schema defaults to anyway.
	if !opts.noSource && opts.strategy == "" {
		opts.strategy = "auto"
	}
	var b strings.Builder
	b.WriteString("apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata:\n  name: shop\n\nspec:\n")
	if !opts.noSource {
		b.WriteString("  source:\n    git: " + testSourceGit + "\n")
		if opts.ref != "" {
			b.WriteString("    ref: " + opts.ref + "\n")
		}
	}
	if opts.strategy != "" || opts.dockerfile != "" {
		b.WriteString("  build:\n")
		if opts.strategy != "" {
			b.WriteString("    strategy: " + opts.strategy + "\n")
		}
		if opts.dockerfile != "" {
			b.WriteString("    dockerfile: " + opts.dockerfile + "\n")
		}
	}
	if opts.image != "" {
		b.WriteString("  image: " + opts.image + "\n")
	}
	b.WriteString("\n  components:\n    - name: web\n      port: 8080\n    - name: worker\n")
	b.WriteString("---\napiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: production\n\nspec:\n  project: shop\n")

	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkoutWith writes a local source tree holding the given files, which is
// what -C points at for strategy detection.
func checkoutWith(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("# fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// --- happy path -------------------------------------------------------------

// TestBuildPrintsThePinnedReferenceLast is the composition contract: the build
// log streams to stdout and the final line is the digest-pinned reference, so
// `kelson build ... | tail -1` feeds `kelson deploy --image`.
func TestBuildPrintsThePinnedReferenceLast(t *testing.T) {
	builder := &fakeBuilder{logs: "#1 [internal] load build definition\n#8 exporting to image\n"}
	resolver := &fakeResolver{}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	stdout, stderr, code, msg := runBuildCmd(t, buildPlaneFor(builder, resolver, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitOK {
		t.Fatalf("exit %d: %s\nstderr: %s", code, msg, stderr)
	}

	if !strings.Contains(stdout, "exporting to image") {
		t.Errorf("the build log must stream to stdout, got:\n%s", stdout)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if last := lines[len(lines)-1]; last != testPinnedResult {
		t.Errorf("last stdout line = %q, want the pinned reference %q", last, testPinnedResult)
	}
	if !strings.Contains(stderr, "deploy it: kelson deploy -f "+spec+" --image "+testPinnedResult) {
		t.Errorf("stderr must carry a runnable deploy hint, got:\n%s", stderr)
	}
	// The hint must not land on stdout, or `tail -1` picks it up instead.
	if strings.Contains(stdout, "deploy it:") {
		t.Errorf("the deploy hint must not pollute stdout:\n%s", stdout)
	}
}

// The destination is derived, not configured: repository from the registry
// prefix plus the project name, tag from project and revision, and one build
// for the whole Project (model rule P3 shares its image with every component
// that names none).
func TestBuildDerivesDestinationAndRequest(t *testing.T) {
	builder := &fakeBuilder{}
	resolver := &fakeResolver{}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile", dockerfile: "deploy/Dockerfile"})

	var target buildTarget
	_, stderr, code, msg := runBuildCmd(t, buildPlaneFor(builder, resolver, &target),
		"build", "-f", spec, "--env", "production", "--registry", "ghcr.io/acme",
		"--push-secret", "ghcr-push", "--namespace", "builds")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}

	req := builder.lastRequest(t)
	if req.Image != testRepository {
		t.Errorf("Image = %q, want %q", req.Image, testRepository)
	}
	if req.Tag != "shop-shop-0123456789ab" {
		t.Errorf("Tag = %q, want the project/project/short-revision convention", req.Tag)
	}
	if req.Revision != testCommit || req.SourceRef != testCommit {
		t.Errorf("Revision/SourceRef = %q/%q, want the resolved commit %q", req.Revision, req.SourceRef, testCommit)
	}
	if req.SourceGit != testSourceGit {
		t.Errorf("SourceGit = %q, want %q", req.SourceGit, testSourceGit)
	}
	if req.Dockerfile != "deploy/Dockerfile" {
		t.Errorf("Dockerfile = %q, want the spec's build.dockerfile", req.Dockerfile)
	}
	if req.Project != "shop" || req.Environment != "production" {
		t.Errorf("Project/Environment = %q/%q", req.Project, req.Environment)
	}
	if req.Component != "" {
		t.Errorf("Component = %q, want empty: one build serves the whole Project", req.Component)
	}

	if target.namespace != "builds" || target.pushSecret != "ghcr-push" {
		t.Errorf("build target = %+v, want the flags to reach the plane", target)
	}
	if !strings.Contains(stderr, testRepository+":shop-shop-0123456789ab") {
		t.Errorf("the plan should name the destination, got:\n%s", stderr)
	}
}

// Without --namespace the build runs in the environment's namespace, which is
// where a push secret for that environment would already live.
func TestBuildDefaultsToTheEnvironmentNamespace(t *testing.T) {
	var target buildTarget
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})
	_, _, code, msg := runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, &fakeResolver{}, &target),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if target.namespace != "shop-production" {
		t.Errorf("namespace = %q, want the resolved environment namespace shop-production", target.namespace)
	}
}

func TestBuildReadsTheRegistryFromTheEnvironment(t *testing.T) {
	t.Setenv(registryEnv, "registry.internal:5000/team")
	builder := &fakeBuilder{}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil), "build", "-f", spec)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if got := builder.lastRequest(t).Image; got != "registry.internal:5000/team/shop" {
		t.Errorf("Image = %q, want the destination derived from %s", got, registryEnv)
	}
}

func TestBuildRequiresADestinationRegistry(t *testing.T) {
	t.Setenv(registryEnv, "")
	builder := &fakeBuilder{}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil), "build", "-f", spec)
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "--registry") {
		t.Errorf("the error should name the flag that fixes it, got: %s", msg)
	}
	if builder.calls() != 0 {
		t.Error("no build may be submitted without a destination")
	}
}

// --- strategy resolution ----------------------------------------------------

// auto detection needs a tree; -C supplies one, and a Dockerfile in it selects
// the dockerfile strategy (ADR-0010 rule 2).
func TestBuildAutoDetectsFromALocalCheckout(t *testing.T) {
	builder := &fakeBuilder{}
	spec := writeBuildSpec(t, buildSpecOptions{})
	checkout := checkoutWith(t, "Dockerfile", "go.mod")

	_, stderr, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme", "-C", checkout)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if !strings.Contains(stderr, "found Dockerfile") {
		t.Errorf("the plan must report why the strategy was chosen, got:\n%s", stderr)
	}
	if builder.calls() != 1 {
		t.Errorf("expected one build, got %d", builder.calls())
	}
}

// Without -C there is no tree to detect from, and the CLI says so instead of
// guessing. In-cluster detection is future work (#50).
func TestBuildAutoWithoutACheckoutIsAClearRefusal(t *testing.T) {
	builder := &fakeBuilder{}
	spec := writeBuildSpec(t, buildSpecOptions{})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	for _, want := range []string{build.ReasonDetectionNeedsSource, "-C", "spec.build.strategy", "#50"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should mention %q, got: %s", want, msg)
		}
	}
	if builder.calls() != 0 {
		t.Error("nothing may be built when the strategy could not be resolved")
	}
}

// The resolved strategy selects the driver (#49, ADR-0010), so it has to reach
// the connector: a Dockerfile-less repository resolves to buildpacks and the
// plane is asked for the buildpacks driver, explicitly or by detection. Which
// driver the connector then builds is buildDriver's, tested below without a
// cluster.
func TestBuildStrategyReachesTheConnector(t *testing.T) {
	cases := []struct {
		name string
		spec buildSpecOptions
		// checkout, when set, is a file that makes a local tree detectable.
		checkout string
		want     detect.Strategy
	}{
		{
			name: "explicit buildpacks needs no checkout",
			spec: buildSpecOptions{strategy: "buildpacks"},
			want: detect.StrategyBuildpacks,
		},
		{
			name:     "a language signal with no Dockerfile detects buildpacks",
			spec:     buildSpecOptions{},
			checkout: "go.mod",
			want:     detect.StrategyBuildpacks,
		},
		{
			name:     "a Dockerfile still takes precedence",
			spec:     buildSpecOptions{},
			checkout: "Dockerfile",
			want:     detect.StrategyDockerfile,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := &fakeBuilder{}
			var target buildTarget
			spec := writeBuildSpec(t, tc.spec)
			args := []string{"build", "-f", spec, "--registry", "ghcr.io/acme"}
			if tc.checkout != "" {
				args = append(args, "-C", checkoutWith(t, tc.checkout))
			}

			_, stderr, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, &target), args...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, msg)
			}
			if target.strategy != tc.want {
				t.Errorf("the connector was asked for %q, want %q", target.strategy, tc.want)
			}
			if !strings.Contains(stderr, "strategy    strategy "+string(tc.want)) {
				t.Errorf("the plan must name the chosen strategy, got:\n%s", stderr)
			}
			if builder.calls() != 1 {
				t.Errorf("the build ran %d times, want 1", builder.calls())
			}
		})
	}
}

// buildDriver is the whole of the strategy → driver mapping, and it is where a
// wrong answer would be worst: building a Dockerfile-less repository with
// BuildKit fails deep in buildctl with a message about a missing file, which
// says nothing about the strategy that was chosen.
func TestBuildDriverForStrategy(t *testing.T) {
	cases := map[detect.Strategy]string{
		detect.StrategyDockerfile: "dockerfile",
		detect.StrategyBuildpacks: "buildpacks",
	}
	for strategy, want := range cases {
		driver, err := buildDriver(buildTarget{strategy: strategy, namespace: "shop-production"}, nopExecutor{})
		if err != nil {
			t.Fatalf("buildDriver(%q): %v", strategy, err)
		}
		if driver.Name() != want {
			t.Errorf("buildDriver(%q) built the %q driver", strategy, driver.Name())
		}
	}
	if _, err := buildDriver(buildTarget{strategy: "railpack"}, nopExecutor{}); err == nil {
		t.Error("a strategy with no driver must be refused, not silently built with another")
	}
	if _, err := buildDriver(buildTarget{strategy: detect.StrategyNone}, nopExecutor{}); err == nil {
		t.Error("`none` means build nothing and must not produce a driver")
	}
}

// A local registry has no TLS, so the hosts that may be reached over plain
// HTTP are named — by flag or by the same environment variable kelson-server
// reads — and reach the driver through the target. Nothing else is affected,
// and a malformed entry is refused before a build Job exists.
func TestInsecureRegistriesReachTheTarget(t *testing.T) {
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	var target buildTarget
	_, _, code, msg := runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, &fakeResolver{}, &target),
		"build", "-f", spec, "--registry", "localhost:5000",
		"--insecure-registries", "localhost:5000, registry.internal:5000")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if strings.Join(target.insecureRegistries, "|") != "localhost:5000|registry.internal:5000" {
		t.Errorf("insecureRegistries = %v", target.insecureRegistries)
	}

	// Nothing named, nothing insecure.
	target = buildTarget{}
	if _, _, code, msg = runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, &fakeResolver{}, &target),
		"build", "-f", spec, "--registry", "ghcr.io/acme"); code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if target.insecureRegistries != nil {
		t.Errorf("no registry was named insecure, got %v", target.insecureRegistries)
	}

	// The environment supplies the default, as $KELSON_REGISTRY does.
	t.Setenv(insecureRegistriesEnv, "localhost:5000")
	target = buildTarget{}
	if _, _, code, msg = runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, &fakeResolver{}, &target),
		"build", "-f", spec, "--registry", "localhost:5000"); code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if len(target.insecureRegistries) != 1 || target.insecureRegistries[0] != "localhost:5000" {
		t.Errorf("the environment variable must supply the default, got %v", target.insecureRegistries)
	}

	// A malformed entry is a typo, and the message names the flag.
	if _, _, code, msg = runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme",
		"--insecure-registries", "http://localhost:5000"); code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "--insecure-registries") {
		t.Errorf("the refusal should name the flag: %s", msg)
	}
}

// nopExecutor satisfies the cluster seam both drivers take. buildDriver only
// constructs them, so it never runs.
type nopExecutor struct{}

func (nopExecutor) Submit(context.Context, []byte) (string, error) { return "", nil }
func (nopExecutor) Wait(context.Context, string, io.Writer) (build.Result, error) {
	return build.Result{}, nil
}

func TestBuildStrategyNoneIsNothingToBuild(t *testing.T) {
	builder := &fakeBuilder{}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "none", image: "ghcr.io/acme/shop:1.0.0"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, build.ReasonNothingToBuild) {
		t.Errorf("want the %s reason, got: %s", build.ReasonNothingToBuild, msg)
	}
	if builder.calls() != 0 {
		t.Error("build.strategy: none must not submit a build")
	}
}

func TestBuildWithoutASourceIsRefused(t *testing.T) {
	builder := &fakeBuilder{}
	spec := writeBuildSpec(t, buildSpecOptions{noSource: true, image: "ghcr.io/acme/shop:1.0.0"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, build.ReasonNoSource) {
		t.Errorf("want the %s reason, got: %s", build.ReasonNoSource, msg)
	}
}

func TestBuildRejectsAMissingCheckoutDirectory(t *testing.T) {
	spec := writeBuildSpec(t, buildSpecOptions{})
	missing := filepath.Join(t.TempDir(), "not-here")

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme", "-C", missing)
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "local source checkout") {
		t.Errorf("a mistyped -C should be reported as such, got: %s", msg)
	}
}

// --- revision resolution ----------------------------------------------------

// A moving ref is resolved once, through the seam, so the tag, the recorded
// revision and the commit the pod checks out cannot disagree.
func TestBuildResolvesARefThroughTheSeam(t *testing.T) {
	builder := &fakeBuilder{}
	resolver := &fakeResolver{hash: testCommit}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, resolver, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme", "--ref", "release/v2")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	calls := resolver.resolved()
	if len(calls) != 1 || calls[0] != [2]string{testSourceGit, "release/v2"} {
		t.Fatalf("resolver calls = %v, want one call for (%s, release/v2)", calls, testSourceGit)
	}
	if got := builder.lastRequest(t).Revision; got != testCommit {
		t.Errorf("Revision = %q, want the resolved commit", got)
	}
}

// spec.source.ref is the default when --ref is absent.
func TestBuildFallsBackToTheSpecRef(t *testing.T) {
	resolver := &fakeResolver{}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile", ref: "develop"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, resolver, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if calls := resolver.resolved(); len(calls) != 1 || calls[0][1] != "develop" {
		t.Fatalf("resolver calls = %v, want the spec's source.ref", calls)
	}
}

// A ref that is already a commit needs no remote round trip.
func TestBuildSkipsResolutionForACommit(t *testing.T) {
	builder := &fakeBuilder{}
	resolver := &fakeResolver{}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, resolver, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme", "--ref", strings.ToUpper(testCommit))
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if calls := resolver.resolved(); len(calls) != 0 {
		t.Fatalf("a 40-hex ref must not be resolved remotely, got %v", calls)
	}
	if got := builder.lastRequest(t).Revision; got != testCommit {
		t.Errorf("Revision = %q, want the lowercased commit", got)
	}
}

func TestBuildSurfacesARefResolutionFailure(t *testing.T) {
	builder := &fakeBuilder{}
	resolver := &fakeResolver{err: errors.New("git: no branch named nope")}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, resolver, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme", "--ref", "nope")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "no branch named nope") {
		t.Errorf("the resolver's error should reach the user, got: %s", msg)
	}
	if builder.calls() != 0 {
		t.Error("an unresolved revision must not be built")
	}
}

// --- failures ---------------------------------------------------------------

// The executor already classifies success, failure and timeout; the command's
// job is to exit non-zero and print what it was told, unedited.
func TestBuildFailureExitsNonZeroWithTheClassifiedError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "failed", err: errors.New("kube: build build-shop-abc failed: BackoffLimitExceeded\nbuild output:\nCOPY failed")},
		{name: "timeout", err: errors.New("kube: build build-shop-abc timed out (activeDeadlineSeconds exceeded)")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := &fakeBuilder{err: tc.err}
			spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

			stdout, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
				"build", "-f", spec, "--registry", "ghcr.io/acme")
			if code != exitErr {
				t.Fatalf("exit %d, want %d", code, exitErr)
			}
			if msg != tc.err.Error() {
				t.Errorf("the executor's classified error must reach the user unedited:\ngot:  %s\nwant: %s", msg, tc.err)
			}
			if strings.Contains(stdout, "@sha256:") {
				t.Errorf("a failed build must not print a reference:\n%s", stdout)
			}
		})
	}
}

// A build whose result is not digest-pinned breaks the one guarantee the build
// exists to provide (#51), so it fails rather than printing a mutable tag.
func TestBuildRejectsAnUnpinnedResult(t *testing.T) {
	builder := &fakeBuilder{result: build.Result{Reference: testRepository + ":latest"}}
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "not pinned by digest") {
		t.Errorf("want a pinning failure, got: %s", msg)
	}
}

func TestBuildRejectsANonPositiveTimeout(t *testing.T) {
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})
	_, _, code, msg := runBuildCmd(t, buildPlaneFor(&fakeBuilder{}, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme", "--timeout", "0")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "--timeout") {
		t.Errorf("want a timeout error, got: %s", msg)
	}
}

func TestBuildReportsAnUnavailablePlane(t *testing.T) {
	spec := writeBuildSpec(t, buildSpecOptions{strategy: "dockerfile"})
	connect := func(buildTarget) (*buildPlane, error) { return nil, errors.New("kube: no usable cluster credentials") }

	_, _, code, msg := runBuildCmd(t, connect, "build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "no usable cluster credentials") {
		t.Errorf("the connector's error should reach the user, got: %s", msg)
	}
}

// --- per-component sources (ADR-0035) ---------------------------------------

const testToolsGit = "https://gitlab.com/acme/build-tools.git"

// writeMultiSourceSpec writes a Project spelling its sources in the plural,
// with each component bound to the source it names.
func writeMultiSourceSpec(t *testing.T, webSource, workerSource string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata:\n  name: shop\n\nspec:\n")
	b.WriteString("  sources:\n")
	b.WriteString("    - name: app\n      git: " + testSourceGit + "\n      ref: main\n")
	b.WriteString("    - name: tools\n      git: " + testToolsGit + "\n      ref: v2\n      connection: acme-gitlab\n")
	b.WriteString("  build:\n    strategy: dockerfile\n")
	b.WriteString("\n  components:\n")
	b.WriteString("    - name: web\n      port: 8080\n      source: " + webSource + "\n")
	b.WriteString("    - name: worker\n      source: " + workerSource + "\n")
	b.WriteString("---\napiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: production\n\nspec:\n  project: shop\n")

	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A plural-spelling project builds — it used to be refused with
// build/no-source — and it builds what its components are bound to: the tools
// repository at the tools ref through the tools connection, not the first
// source declared.
func TestBuildFollowsTheComponentsSourceBinding(t *testing.T) {
	builder := &fakeBuilder{}
	resolver := &fakeResolver{}
	spec := writeMultiSourceSpec(t, "tools", "tools")

	_, stderr, code, msg := runBuildCmd(t, buildPlaneFor(builder, resolver, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, msg)
	}

	req := builder.lastRequest(t)
	if req.SourceGit != testToolsGit {
		t.Errorf("SourceGit = %q, want the bound source %q", req.SourceGit, testToolsGit)
	}
	if req.SourceName != "tools" || req.SourceConnection != "acme-gitlab" {
		t.Errorf("SourceName/SourceConnection = %q/%q, want tools/acme-gitlab", req.SourceName, req.SourceConnection)
	}
	if calls := resolver.resolved(); len(calls) != 1 || calls[0] != [2]string{testToolsGit, "v2"} {
		t.Fatalf("resolver calls = %v, want one call for the bound source at its own ref", calls)
	}
	// The plan a human reads names the binding, not merely the URL: with
	// sources declared and bound by name, "which repository is this cloning"
	// and "why that one" are two questions.
	for _, want := range []string{"source      tools ", testToolsGit, "components web, worker"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the plan should contain %q, got:\n%s", want, stderr)
		}
	}
	if builder.calls() != 1 {
		t.Errorf("the builder was called %d times, want one build for the shared source", builder.calls())
	}
}

// Components bound to different repositories are refused by name rather than
// half-built: one build pushes one image, and rule P3 pins one image per
// project.
func TestBuildRefusesSeveralSources(t *testing.T) {
	builder := &fakeBuilder{}
	spec := writeMultiSourceSpec(t, "app", "tools")

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, build.ReasonSeveralSources) {
		t.Errorf("want the %s reason, got: %s", build.ReasonSeveralSources, msg)
	}
	if builder.calls() != 0 {
		t.Error("a refusal must not reach the build plane")
	}
}

// A component bound to a name nothing declares is the resolver's refusal, and
// it reaches the user as one — the code, the document and the field that names
// it — rather than being flattened into a build-plane failure. The CLI consults
// no global tier, because the instance's GitSources are the server's to read,
// so what it can say about them is that it saw none.
func TestBuildSurfacesAnUnknownSource(t *testing.T) {
	builder := &fakeBuilder{}
	spec := writeMultiSourceSpec(t, "app", "platform")

	_, _, code, msg := runBuildCmd(t, buildPlaneFor(builder, &fakeResolver{}, nil),
		"build", "-f", spec, "--registry", "ghcr.io/acme")
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	for _, want := range []string{string(model.ErrUnknownSource), "platform", "$.spec.components[1].source"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should mention %q, got: %s", want, msg)
		}
	}
	if builder.calls() != 0 {
		t.Error("a spec that does not resolve must not reach the build plane")
	}
}
