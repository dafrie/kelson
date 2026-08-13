package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/buildkit"
	"github.com/dafrie/kelson/internal/build/detect"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/model"
)

// newBuildCmd builds `kelson build` (issues #48, #51): the command that finally
// drives the build plane, which until now had a complete driver, a complete
// executor and no caller at all.
//
// # What it does and does not decide
//
// Everything about *how* to build comes from the spec and ADR-0010's precedence
// (an explicit build.strategy wins, otherwise a Dockerfile decides). Everything
// about *where* the image goes is infrastructure configuration and arrives as
// flags: --registry, --push-secret, --namespace. That split is the same one
// ADR-0010 draws when it forbids strategy configuration in the spec, seen from
// the registry side — the same Project must be buildable against a team's
// ghcr.io and against a kind cluster's localhost:5000.
//
// # Why the plane is built behind a seam
//
// Same reason as `kelson deploy`: the command plane's lint allow-list forbids
// the Kubernetes client libraries (.golangci.yml, the `main` depguard rule) and
// the git libraries, so nothing here may construct a clientset or run
// ls-remote. buildConnector is the deliveryConnector pattern applied to the
// build plane — production passes connectBuild, tests pass fakes, and no test
// of this command needs a cluster or a network.
func newBuildCmd() *cobra.Command { return newBuildCmdFactory(connectBuild) }

func newBuildCmdFactory(connect buildConnector) *cobra.Command {
	opts := &buildOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "build -f spec.yaml --registry <prefix>",
		Short: "Build the project's source into an image with an in-cluster BuildKit job",
		Long: "Build clones the Project's source in the cluster, builds it with rootless BuildKit and pushes\n" +
			"the result, streaming the build log as it happens.\n\n" +
			"Strategy selection follows ADR-0010: an explicit spec.build.strategy wins, otherwise a\n" +
			"Dockerfile decides. Detecting the strategy needs to see the source tree, which the CLI can only\n" +
			"do for a local checkout — pass -C for auto-detection, or name the strategy in the spec.\n\n" +
			"The last line of stdout is the digest-pinned image reference, so it can be fed straight to\n" +
			"`kelson deploy --image`.",
		Example: "  kelson build -f project.yaml --registry ghcr.io/acme --push-secret ghcr-push\n" +
			"  kelson build -f project.yaml -f production.yaml --env production --ref v1.4.0 -C .",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuild(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to build for (optional when the input holds exactly one)")
	f.StringVar(&opts.registry, "registry", "", "destination registry and namespace, e.g. ghcr.io/acme (default: $KELSON_REGISTRY)")
	f.StringVar(&opts.pushSecret, "push-secret", "", "name of an existing kubernetes.io/dockerconfigjson Secret in the build namespace that authenticates the push")
	f.StringVar(&opts.ref, "ref", "", "git branch, tag or commit to build (default: the Project's spec.source.ref, else the default branch)")
	f.StringVarP(&opts.sourceDir, "source-dir", "C", "", "local checkout of the source, used only to detect the build strategy when it is not named in the spec")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.namespace, "namespace", "", "namespace the build Job runs in (default: the environment's namespace)")
	f.DurationVar(&opts.timeout, "timeout", defaultBuildTimeout, "budget for the whole build, including the clone and the push")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

// defaultBuildTimeout is generous next to a deploy's: a cold build with no
// cache pulls a base image and compiles, and killing that at five minutes
// would fail more builds than it saves.
const defaultBuildTimeout = 30 * time.Minute

// registryEnv is the environment variable that supplies --registry, so an
// operator sets the destination once per shell rather than per invocation.
const registryEnv = "KELSON_REGISTRY"

type buildOptions struct {
	files      []string
	env        string
	registry   string
	pushSecret string
	ref        string
	sourceDir  string
	kubeconfig string
	namespace  string
	timeout    time.Duration
	connect    buildConnector
}

func runBuild(cmd *cobra.Command, opts *buildOptions) error {
	res, err := buildImage(cmd, opts)
	if err != nil {
		return err
	}

	// The deploy hint goes to stderr and the reference to stdout, so the last
	// line of stdout is the reference and nothing else — `kelson build ... |
	// tail -1` is the composition this command is built for.
	hint := &printer{w: cmd.ErrOrStderr()}
	hint.printf("deploy it: kelson deploy %s --image %s\n", specArgs(opts), res.Reference)
	if err := hint.err; err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), res.Reference)
	return err
}

// buildImage is the pipeline: everything from the spec to a pinned reference,
// with none of the presentation. runBuild adds the printing.
//
// The split is not speculative generality — it is where a `kelson deploy
// --build` would attach. That convenience is deliberately not wired: it would
// need --registry, --push-secret, --ref, -C, a build namespace and a second
// timeout on `kelson deploy`, all inert unless --build is passed, plus a
// second connector on a command that already takes one. `kelson build |
// tail -1` into `kelson deploy --image` composes without any of that, so the
// composition is documented (imageFlagUsage, docs/build.md) instead.
func buildImage(cmd *cobra.Command, opts *buildOptions) (build.Result, error) {
	if opts.timeout <= 0 {
		return build.Result{}, fmt.Errorf("--timeout must be positive, got %s", opts.timeout)
	}

	project, environment, err := loadBuildSpec(opts)
	if err != nil {
		return build.Result{}, err
	}
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		return build.Result{}, errs
	}

	source := project.Spec.Source
	if source == nil || strings.TrimSpace(source.Git) == "" {
		return build.Result{}, buildError{
			Reason:      reasonNoSource,
			Message:     fmt.Sprintf("Project %s has no spec.source.git, so there is nothing to build from", project.Metadata.Name),
			Remediation: "add spec.source.git to the Project, or deploy a pre-built image with `kelson deploy --image`",
		}
	}

	detection, err := resolveBuildStrategy(project.Spec.Build, opts.sourceDir)
	if err != nil {
		return build.Result{}, err
	}

	image, err := registry.Repository(registryPrefix(opts), project.Metadata.Name)
	if err != nil {
		return build.Result{}, err
	}

	namespace := opts.namespace
	if namespace == "" {
		namespace = resolved.Environment.Namespace
	}
	plane, err := connectBuildPlane(opts, namespace)
	if err != nil {
		return build.Result{}, err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()

	ref := opts.ref
	if ref == "" {
		ref = source.Ref
	}
	revision, err := resolveRevision(ctx, plane.revisions, source.Git, ref)
	if err != nil {
		return build.Result{}, err
	}

	req := build.Request{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		// Application is deliberately empty: one build serves the whole
		// Project (see destinationTag), so naming one of its applications here
		// would put a false label on the Job and on the image.
		SourceGit:  source.Git,
		SourceRef:  revision,
		Dockerfile: dockerfilePath(project.Spec.Build),
		Image:      image,
		Tag:        destinationTag(project.Metadata.Name, revision),
		Revision:   revision,
	}

	// The plan is context for a human watching the build, so it goes to stderr
	// alongside the deploy hint and leaves stdout to the log and the reference.
	plan := &printer{w: cmd.ErrOrStderr()}
	plan.printf("strategy    %s\n", detection.Message)
	plan.printf("source      %s at %s\n", source.Git, revision)
	plan.printf("destination %s:%s (namespace %s)\n", req.Image, req.Tag, namespace)
	if err := plan.err; err != nil {
		return build.Result{}, err
	}

	// The build log goes to stdout as it arrives; the executor streams the
	// pod's output through this writer while the Job runs.
	res, err := plane.builder.Build(ctx, req, cmd.OutOrStdout())
	if err != nil {
		return build.Result{}, err
	}
	if res.Reference == "" {
		return build.Result{}, fmt.Errorf("the build reported success but returned no image reference")
	}
	if registry.Mutable(res.Reference) {
		// The executor pins by the digest it parsed out of the push; a
		// reference that is not pinned would make the deploy non-reproducible,
		// which is the one guarantee the build exists to provide (#51).
		return build.Result{}, fmt.Errorf("the build returned %s, which is not pinned by digest", res.Reference)
	}
	return res, nil
}

// destinationTag names the human-readable tag pushed alongside the digest.
//
// registry.Tag's convention is <project>-<application>-<revision>, but a
// kelson build is per-Project, not per-Application: the source is project-level
// and model rule P3 gives every application without its own image: the
// Project's image, so one build feeds all of them. There is no single
// application to name. The project name is passed for that slot — redundant
// with the first component, but true; naming the first application instead
// would read as "this image belongs to web", which is exactly what it does not
// mean. Reproducibility comes from the digest either way; this tag is for
// humans reading a registry listing.
func destinationTag(project, revision string) string {
	return registry.Tag(project, project, shortRevision(revision))
}

// shortRevision keeps the tag readable. The full commit stays in
// Request.Revision and in the Job's kelson.dev/revision annotation, so nothing
// is lost by shortening what humans read.
func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func dockerfilePath(b *model.Build) string {
	if b == nil {
		return ""
	}
	return b.Dockerfile
}

func registryPrefix(opts *buildOptions) string {
	if opts.registry != "" {
		return opts.registry
	}
	return os.Getenv(registryEnv)
}

// specArgs rebuilds the -f/--env part of the command line for the deploy hint,
// so the suggestion is one that actually runs.
func specArgs(opts *buildOptions) string {
	var b strings.Builder
	for _, f := range opts.files {
		fmt.Fprintf(&b, "-f %s ", f)
	}
	if opts.env != "" {
		fmt.Fprintf(&b, "--env %s ", opts.env)
	}
	return strings.TrimSpace(b.String())
}

// loadBuildSpec is the spec-loading half of the build command. It stops short
// of rendering on purpose: a build needs the Project's source and build
// stanzas and the Environment's identity, and nothing a ClusterProfile decides.
// Requiring --profile to run a build would be a prerequisite with no payoff.
func loadBuildSpec(opts *buildOptions) (*model.Project, *model.Environment, error) {
	project, environments, _, err := loadSpecFiles(opts.files)
	if err != nil {
		return nil, nil, err
	}
	environment, err := selectEnvironment(environments, opts.env)
	if err != nil {
		return nil, nil, err
	}
	return project, environment, nil
}

// --- strategy resolution ----------------------------------------------------

// resolveBuildStrategy applies ADR-0010's precedence and reports what the
// build plane can actually run today.
//
// The honest limitation is detection: detect.Detect reads the source tree, and
// for a remote repository the CLI has no tree to read — the tree only exists
// inside the build pod, after the clone. So `auto` needs either a local
// checkout (-C) or an explicit strategy in the spec. Detecting inside the
// cluster, before choosing a driver, is future work; guessing "probably
// Dockerfile" here would be the kind of magic ADR-0010 exists to avoid.
func resolveBuildStrategy(spec *model.Build, sourceDir string) (detect.Detection, error) {
	tree, err := sourceTree(sourceDir)
	if err != nil {
		return detect.Detection{}, err
	}

	strategy := model.BuildAuto
	if spec != nil && spec.Strategy != "" {
		strategy = spec.Strategy
	}
	if strategy == model.BuildAuto && tree == nil {
		return detect.Detection{}, buildError{
			Reason: reasonDetectionNeedsCheckout,
			Message: "the build strategy is `auto` and detecting it needs to read the source tree, " +
				"which is not available for a remote repository before the build pod clones it",
			Remediation: "pass -C <dir> pointing at a local checkout of the source, or set spec.build.strategy " +
				"(`dockerfile` needs no detection); in-cluster detection is tracked by issue #50",
		}
	}

	detection, err := detect.Detect(spec, tree)
	if err != nil {
		return detect.Detection{}, fmt.Errorf("detecting the build strategy in %s: %w", sourceDir, err)
	}

	switch detection.Strategy {
	case detect.StrategyDockerfile:
		return detection, nil

	case detect.StrategyBuildpacks:
		return detection, buildError{
			Reason: reasonStrategyDeferred,
			Message: fmt.Sprintf("%s, but the buildpacks strategy is not implemented in this release",
				detection.Message),
			Remediation: "add a Dockerfile to the source (it takes precedence, ADR-0010) or set " +
				"spec.build.strategy: dockerfile; buildpacks is deferred out of the v0.1 cut and tracked " +
				"by issue #49, and ADR-0010 still makes it the eventual default",
		}

	default: // detect.StrategyNone
		return detection, buildError{
			Reason:      reasonNothingToBuild,
			Message:     "the spec sets build.strategy: none, which means build nothing and deploy spec.image",
			Remediation: "remove build.strategy: none to build from source, or deploy the pre-built image directly",
		}
	}
}

// sourceTree opens the local checkout -C names, if any. A missing or
// non-directory path is a mistyped flag and is reported as one rather than
// silently degrading to "no tree", which would produce the auto-detection
// error and send the user looking in the wrong place.
func sourceTree(dir string) (fs.FS, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the local source checkout: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("-C %s is not a directory; it must point at a checkout of the source", dir)
	}
	return os.DirFS(dir), nil
}

// --- revision resolution ----------------------------------------------------

// commitLength is the length of a full git object id in hex.
const commitLength = 40

// resolveRevision turns whatever --ref (or spec.source.ref) says into the
// commit that was actually built.
//
// A ref that is already a commit is used as written and costs no network call.
// Anything else is resolved once, here, so the tag, Request.Revision and the
// commit the build pod checks out all name the same thing — a branch resolved
// separately by each of them would let them disagree.
func resolveRevision(ctx context.Context, resolver revisionResolver, repo, ref string) (string, error) {
	if isCommit(ref) {
		return strings.ToLower(ref), nil
	}
	if resolver == nil {
		return "", fmt.Errorf("git revision resolution is unavailable in this build; pass --ref with a full 40-character commit")
	}
	return resolver.Resolve(ctx, repo, ref)
}

func isCommit(ref string) bool {
	if len(ref) != commitLength {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !hex {
			return false
		}
	}
	return true
}

// --- the build plane seam ---------------------------------------------------

// buildTarget is what the build command resolved from its flags and spec:
// which cluster, which namespace, which credential, and how long it may run.
type buildTarget struct {
	kubeconfig string
	namespace  string
	pushSecret string
	timeout    time.Duration
}

// revisionResolver answers "what commit does this ref name?" against a remote
// repository. It is an interface because the answer needs the git libraries,
// which the command plane's lint allow-list forbids (.golangci.yml) — the
// implementation lives in the delivery plane and tests pass a fake.
type revisionResolver interface {
	Resolve(ctx context.Context, repo, ref string) (string, error)
}

// buildPlane is the assembled build plane for one command run.
type buildPlane struct {
	builder   build.Builder
	revisions revisionResolver
}

// buildConnector builds the build plane for a target. It is the seam the
// command tests replace, mirroring deliveryConnector: the real one needs a
// cluster, none of the wiring under test does.
type buildConnector func(buildTarget) (*buildPlane, error)

// connectBuild is the production connector: one cluster connection, the
// BuildKit driver over the Kubernetes build executor, and a remote ref
// resolver sharing the delivery credential.
//
// This is the first caller of buildkit.New and kube.NewBuildExecutor outside
// tests — the gap issue #48 exists to close.
func connectBuild(t buildTarget) (*buildPlane, error) {
	cluster, err := kube.Connect(t.kubeconfig)
	if err != nil {
		return nil, err
	}
	driver, err := buildkit.New(buildkit.Options{
		Cluster: kube.NewBuildExecutor(cluster.Typed),
		Config: buildkit.Config{
			Namespace:  t.namespace,
			PushSecret: t.pushSecret,
			Timeout:    buildkit.Duration(t.timeout),
		},
	})
	if err != nil {
		return nil, err
	}
	return &buildPlane{
		builder: driver,
		// The source repository and the deployment repository are commonly the
		// same forge, so the build reuses the delivery credential rather than
		// inventing a second one.
		revisions: git.RemoteResolver{Auth: gitAuth()},
	}, nil
}

func connectBuildPlane(opts *buildOptions, namespace string) (*buildPlane, error) {
	if opts.connect == nil {
		return nil, fmt.Errorf("the build plane is unavailable in this build")
	}
	plane, err := opts.connect(buildTarget{
		kubeconfig: opts.kubeconfig,
		namespace:  namespace,
		pushSecret: opts.pushSecret,
		timeout:    opts.timeout,
	})
	if err != nil {
		return nil, err
	}
	if plane == nil || plane.builder == nil {
		return nil, fmt.Errorf("the build plane is unavailable in this build")
	}
	return plane, nil
}

// --- structured errors ------------------------------------------------------

// Reason codes for the build command's own refusals. They are stable strings
// so an agent branches on the reason instead of matching prose, the same
// contract model.Code and delivery.Code make for their planes.
const (
	reasonNoSource               = "build/no-source"
	reasonNothingToBuild         = "build/nothing-to-build"
	reasonStrategyDeferred       = "build/strategy-not-implemented"
	reasonDetectionNeedsCheckout = "build/detection-needs-source"
)

// buildError is a refusal by the build command itself, as opposed to a failure
// reported by the build plane. It carries the same shape the model and
// delivery taxonomies use: a code, what happened, and the action that fixes it.
type buildError struct {
	Reason      string
	Message     string
	Remediation string
}

func (e buildError) Error() string {
	return fmt.Sprintf("[%s] %s: %s", e.Reason, e.Message, e.Remediation)
}
