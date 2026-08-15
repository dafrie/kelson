package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/buildkit"
	"github.com/dafrie/kelson/internal/build/buildpacks"
	"github.com/dafrie/kelson/internal/build/detect"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/gitref"
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
		Short: "Build the project's source into an image with an in-cluster build job",
		Long: "Build clones the source its components are bound to in the cluster, builds it rootlessly and\n" +
			"pushes the result, streaming the build log as it happens.\n\n" +
			"A component builds from the source it names, or from the project's default when it names none\n" +
			"(ADR-0035). One build clones one repository and pushes one image, so a project whose components\n" +
			"build from different repositories is refused by name rather than built by half.\n\n" +
			"Strategy selection follows ADR-0010: an explicit spec.build.strategy wins, otherwise a\n" +
			"Dockerfile selects BuildKit and its absence selects Cloud Native Buildpacks. Detecting the\n" +
			"strategy needs to see the source tree, which the CLI can only do for a local checkout — pass -C\n" +
			"for auto-detection, or name the strategy in the spec.\n\n" +
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
	f.StringVar(&opts.insecureRegistries, "insecure-registries", "",
		"comma-separated registry hosts served over plain HTTP, e.g. localhost:5000 (default: $"+insecureRegistriesEnv+"); only the listed hosts are affected")
	f.StringVar(&opts.pushSecret, "push-secret", "", "name of an existing kubernetes.io/dockerconfigjson Secret in the build namespace that authenticates the push")
	f.StringVar(&opts.ref, "ref", "", "git branch, tag or commit to build (default: the bound source's ref, else the repository's default branch)")
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

// insecureRegistriesEnv supplies --insecure-registries for the same reason and
// with the same variable kelson-server reads: which registries have no TLS is
// a property of the environment, not of one invocation, and a local cluster's
// registry is the same one for every build against it.
const insecureRegistriesEnv = "KELSON_INSECURE_REGISTRIES"

type buildOptions struct {
	files              []string
	env                string
	registry           string
	insecureRegistries string
	pushSecret         string
	ref                string
	sourceDir          string
	kubeconfig         string
	namespace          string
	timeout            time.Duration
	connect            buildConnector
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

	// Which repository this build clones is the components' answer, not the
	// Project's (ADR-0035 decision 4), and the refusals — nothing bound, or
	// bound to several — come from the plan this command shares with
	// BuildService so the two cannot name them differently.
	binding, err := build.SourceToBuild(project.Metadata.Name, resolved)
	if err != nil {
		return build.Result{}, err
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
	plane, err := connectBuildPlane(opts, namespace, detection.Strategy)
	if err != nil {
		return build.Result{}, err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()

	// One ls-remote, against the bound source's repository and its own ref;
	// --ref overrides it for this build alone.
	revision, err := resolveRevision(ctx, plane.revisions, binding.Source.Git, binding.SourceRef(opts.ref))
	if err != nil {
		return build.Result{}, err
	}

	req := build.Request{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		// Component is deliberately empty: one build serves every component
		// bound to this source (see destinationTag), so naming one of them here
		// would put a false label on the Job and on the image.
		SourceGit:        binding.Source.Git,
		SourceRef:        revision,
		SourceName:       binding.Source.Name,
		SourceConnection: binding.Source.Connection,
		Dockerfile:       build.DockerfilePath(project.Spec.Build),
		Image:            image,
		Tag:              build.DestinationTag(project.Metadata.Name, revision),
		Revision:         revision,
	}

	// The plan is context for a human watching the build, so it goes to stderr
	// alongside the deploy hint and leaves stdout to the log and the reference.
	//
	// The source line names the binding and not merely the URL: with sources
	// declared and bound by name, "which repository is this cloning" and "why
	// that one" are two questions, and the second is answered by the source's
	// name and the components that asked for it.
	plan := &printer{w: cmd.ErrOrStderr()}
	plan.printf("strategy    %s\n", detection.Message)
	plan.printf("source      %s %s at %s (%s)\n", binding.Source.Name, binding.Source.Git, revision,
		componentsPhrase(binding.Components))
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

// componentsPhrase names the components one build serves, which is what makes
// the shared clone visible: two components bound to one source are one build,
// and the plan says so rather than leaving a reader to infer it from a single
// Job appearing where they expected two.
func componentsPhrase(components []string) string {
	if len(components) == 1 {
		return "component " + components[0]
	}
	return "components " + strings.Join(components, ", ")
}

func registryPrefix(opts *buildOptions) string {
	if opts.registry != "" {
		return opts.registry
	}
	return os.Getenv(registryEnv)
}

// insecureRegistries is the parsed --insecure-registries list, falling back to
// the environment. Parsing here rather than in the connector means a typo is
// reported before a build Job is created, and it is registry.ParseInsecure
// rather than a local strings.Split so `kelson build` and kelson-server cannot
// read the same variable two ways.
func insecureRegistries(opts *buildOptions) ([]string, error) {
	list := opts.insecureRegistries
	if list == "" {
		list = os.Getenv(insecureRegistriesEnv)
	}
	hosts, err := registry.ParseInsecure(list)
	if err != nil {
		return nil, fmt.Errorf("--insecure-registries: %w", err)
	}
	return hosts, nil
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

// resolveBuildStrategy is the CLI's half of strategy resolution: open the
// checkout -C names, then apply ADR-0010's precedence, which is
// build.ResolveStrategy and shared with the API server (internal/build/plan.go).
//
// Only the opening is the CLI's own, and only because only the CLI has a local
// filesystem to open. Everything the user is told — which strategy, and which
// of the three refusals — comes from the shared function, so `kelson build` and
// BuildService cannot disagree about what a spec selects.
func resolveBuildStrategy(spec *model.Build, sourceDir string) (detect.Detection, error) {
	tree, err := sourceTree(sourceDir)
	if err != nil {
		return detect.Detection{}, err
	}
	return build.ResolveStrategy(spec, tree)
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

// resolveRevision turns whatever --ref (or spec.source.ref) says into the
// commit that was actually built.
//
// A ref that is already a commit is used as written and costs no network call.
// Anything else is resolved once, here, so the tag, Request.Revision and the
// commit the build pod checks out all name the same thing — a branch resolved
// separately by each of them would let them disagree.
func resolveRevision(ctx context.Context, resolver revisionResolver, repo, ref string) (string, error) {
	if build.IsCommit(ref) {
		return build.NormalizeCommit(ref), nil
	}
	if resolver == nil {
		return "", fmt.Errorf("git revision resolution is unavailable in this build; pass --ref with a full 40-character commit")
	}
	return resolver.Resolve(ctx, repo, ref)
}

// --- the build plane seam ---------------------------------------------------

// buildTarget is what the build command resolved from its flags and spec:
// which strategy, which cluster, which namespace, which credential, and how
// long it may run.
type buildTarget struct {
	// strategy is what ResolveStrategy decided, and it selects the driver:
	// dockerfile builds with BuildKit, buildpacks with the CNB lifecycle
	// (ADR-0010). It travels to the connector rather than being decided there,
	// because the decision is the shared plan's (internal/build/plan.go) and
	// the two callers must not be able to make it differently.
	strategy   detect.Strategy
	kubeconfig string
	namespace  string
	pushSecret string
	// insecureRegistries are hosts served over plain HTTP. It reaches both
	// drivers, which mark exactly these and nothing else.
	insecureRegistries []string
	timeout            time.Duration
}

// revisionResolver answers "what commit does this ref name?" against a remote
// repository. It is an interface because the answer needs the git libraries,
// which the command plane's lint allow-list forbids (.golangci.yml) — the
// implementation is internal/gitref and tests pass a fake.
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

// connectBuild is the production connector: one cluster connection, the driver
// the resolved strategy selects over the Kubernetes build executor, and a
// remote ref resolver sharing the delivery credential.
//
// One executor serves both drivers. It satisfies each one's Cluster seam
// structurally, and nothing in it is strategy-specific since it reads the
// destination off the Job's annotations rather than a builder's argv
// (internal/delivery/kube/build_executor.go).
func connectBuild(t buildTarget) (*buildPlane, error) {
	cluster, err := kube.Connect(t.kubeconfig)
	if err != nil {
		return nil, err
	}
	driver, err := buildDriver(t, kube.NewBuildExecutor(cluster.Typed))
	if err != nil {
		return nil, err
	}
	return &buildPlane{
		builder: driver,
		// KELSON_GIT_TOKEN reads the *source* repository. It was named for the
		// deployment repository the git writer pushed to, and that writer is
		// gone (ADR-0028); a build reading a private source is the one job the
		// credential still has.
		revisions: gitref.RemoteResolver{Auth: sourceAuth()},
	}, nil
}

// buildExecutor is what both drivers need from the cluster: submit a rendered
// build Job and stream it to completion. It is declared here so the driver
// pick below is one function taking one seam, rather than two nearly identical
// constructions — kube.BuildExecutor satisfies buildkit.Cluster and
// buildpacks.Cluster structurally, and this names that fact.
type buildExecutor interface {
	Submit(ctx context.Context, manifest []byte) (string, error)
	Wait(ctx context.Context, name string, w io.Writer) (build.Result, error)
}

// buildDriver constructs the driver the resolved strategy selects (ADR-0010).
//
// A strategy this build cannot run is an error here rather than a silent
// fallback to BuildKit: building a Dockerfile-less repository with the
// Dockerfile driver fails deep inside buildctl with a message about a missing
// file, which tells the user nothing about the strategy that was actually
// chosen.
func buildDriver(t buildTarget, cluster buildExecutor) (build.Builder, error) {
	switch t.strategy {
	case detect.StrategyDockerfile:
		return buildkit.New(buildkit.Options{
			Cluster: cluster,
			Config: buildkit.Config{
				Namespace:          t.namespace,
				PushSecret:         t.pushSecret,
				InsecureRegistries: t.insecureRegistries,
				Timeout:            buildkit.Duration(t.timeout),
			},
		})
	case detect.StrategyBuildpacks:
		// No Rebaser is injected: `kelson build` has no rebase command, and a
		// Rebaser needs an OCI client this plane may not import. Rebase fails
		// closed without one rather than pretending to patch a run image
		// (internal/build/buildpacks: Driver.Rebase).
		return buildpacks.New(buildpacks.Options{
			Cluster: cluster,
			Config: buildpacks.Config{
				Namespace:          t.namespace,
				PushSecret:         t.pushSecret,
				InsecureRegistries: t.insecureRegistries,
				Timeout:            buildpacks.Duration(t.timeout),
			},
		})
	default:
		return nil, fmt.Errorf("no build driver for strategy %q", t.strategy)
	}
}

func connectBuildPlane(opts *buildOptions, namespace string, strategy detect.Strategy) (*buildPlane, error) {
	if opts.connect == nil {
		return nil, fmt.Errorf("the build plane is unavailable in this build")
	}
	insecure, err := insecureRegistries(opts)
	if err != nil {
		return nil, err
	}
	plane, err := opts.connect(buildTarget{
		strategy:           strategy,
		kubeconfig:         opts.kubeconfig,
		namespace:          namespace,
		pushSecret:         opts.pushSecret,
		insecureRegistries: insecure,
		timeout:            opts.timeout,
	})
	if err != nil {
		return nil, err
	}
	if plane == nil || plane.builder == nil {
		return nil, fmt.Errorf("the build plane is unavailable in this build")
	}
	return plane, nil
}

// sourceAuth reads the source-repository credential from the environment. A
// missing token is anonymous, which is correct for local paths and public
// remotes and fails loudly at ls-remote time for anything else.
func sourceAuth() gitref.Auth {
	if token := strings.TrimSpace(os.Getenv("KELSON_GIT_TOKEN")); token != "" {
		return gitref.Token{Token: token}
	}
	return gitref.Anonymous{}
}
