package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/preview"
	"github.com/dafrie/kelson/internal/renderer"
)

// `kelson preview` publishes the per-pull-request artifacts ADR-0017's
// ResourceSet consumes (issue #11, stage 2).
//
// # Why this is a CLI verb and not a server RPC
//
// The publisher runs in the application repository's CI, on pull request
// events. That is where the checkout of the pull request ref exists, where the
// image was just built, and where the registry credential already is. A
// server-side publisher would need a forge webhook, a checkout of somebody
// else's repository and a credential kelson does not hold — and a server that
// polled forges would duplicate the ResourceSetInputProvider already running in
// the cluster. ADR-0017's Stage 2 section records the alternatives.
//
// # The two rungs
//
// `preview render` is the offline rung: the same render, printed. `preview
// publish` packages and pushes it. That ladder is the same one `kelson diff`
// and `kelson deploy` climb — the debuggable rung takes the same flags as the
// real one and needs no credential — and it exists because "what would this
// pull request actually deploy?" is a question worth answering without a
// registry.
func newPreviewCmd() *cobra.Command {
	return newPreviewCmdFactory(connectRegistrySecrets, connectPusher)
}

func newPreviewCmdFactory(connect registrySecretConnector, push pusherConnector) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Render and publish the manifests for one pull request's preview",
		Long: "Preview renders a project for one open change request — into its own namespace,\n" +
			"<project>-<environment>-pr<number> — and publishes the result as a Flux OCI artifact tagged with\n" +
			"the change request's head commit.\n\n" +
			"The environment's ResourceSet does the rest: flux-operator finds the change request, creates an\n" +
			"OCIRepository pinned to that same commit, and applies what this published. Previews are\n" +
			"declared by spec.previews on the Environment and are available in Flux mode only.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newPreviewPublishCmd(connect, push), newPreviewRenderCmd())
	return cmd
}

// previewOptions is what both rungs take. The two commands share every input
// but the destination, because a render that took different inputs from the
// publish it rehearses would rehearse the wrong thing.
type previewOptions struct {
	specInput
	pr      string
	sha     string
	output  string
	connect registrySecretConnector
	push    pusherConnector

	// registrySecret names a kubernetes.io/dockerconfigjson Secret to resolve
	// the push credential from, for a runner that has cluster access but no
	// docker login. Empty means the local docker config.
	registrySecret    string
	registryNamespace string
	dockerConfig      string
	insecure          bool
	timeout           time.Duration
}

// defaultPublishTimeout bounds the push. It is a handful of small blobs over
// HTTPS, so a minute is already generous; a build's half hour would only turn a
// hung registry into a hung CI job.
const defaultPublishTimeout = 2 * time.Minute

func newPreviewPublishCmd(connect registrySecretConnector, push pusherConnector) *cobra.Command {
	opts := &previewOptions{connect: connect, push: push}
	cmd := &cobra.Command{
		Use:   "publish -f spec.yaml --env <name> --pr <number> --sha <commit>",
		Short: "Render one pull request's manifests and push them as an OCI artifact",
		Long: "Publish renders the environment for one change request and pushes the manifests to\n" +
			"spec.previews.artifacts.repository, tagged with the head commit — which is exactly the tag the\n" +
			"rendered OCIRepository pins, so the preview updates the moment the artifact lands.\n\n" +
			"It composes with `kelson build`: build the image first and pass the digest-pinned reference to\n" +
			"--image, so the preview runs the code the pull request actually contains.\n\n" +
			"The push credential comes from the runner's docker login by default, or from a cluster Secret\n" +
			"with --registry-secret.",
		Example: "  kelson preview publish -f project.yaml -f staging.yaml --env staging --pr 412 --sha $HEAD_SHA --image ghcr.io/acme/shop@sha256:abc\n" +
			"  kelson preview publish -f spec.yaml --env staging --pr 412 --sha $HEAD_SHA --registry-secret ghcr-push",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPreviewPublish(cmd, opts)
		},
	}
	previewFlags(cmd, opts)
	f := cmd.Flags()
	f.StringVar(&opts.registrySecret, "registry-secret", "", "name of a kubernetes.io/dockerconfigjson Secret to resolve the push credential from (default: the runner's docker login)")
	f.StringVar(&opts.registryNamespace, "registry-secret-namespace", "", "namespace holding --registry-secret (default: the parent environment's namespace)")
	f.StringVar(&opts.dockerConfig, "docker-config", "", "path to a docker config.json holding the push credential (default: $DOCKER_CONFIG/config.json, then ~/.docker/config.json)")
	f.BoolVar(&opts.insecure, "insecure-registry", false, "push over plain HTTP (implied for localhost registries)")
	f.DurationVar(&opts.timeout, "timeout", defaultPublishTimeout, "budget for the push")
	return cmd
}

func newPreviewRenderCmd() *cobra.Command {
	opts := &previewOptions{}
	cmd := &cobra.Command{
		Use:   "render -f spec.yaml --env <name> --pr <number> --sha <commit>",
		Short: "Render one pull request's manifests without publishing them",
		Long: "Preview render is publish without the push: the same manifests, printed instead of packaged.\n\n" +
			"It is the rung to reach for when a preview is not what it should be — the artifact's contents\n" +
			"are exactly these bytes, so anything wrong here is wrong in the cluster and anything right here\n" +
			"is a publishing or a reconciliation problem instead.",
		Example: "  kelson preview render -f spec.yaml --env staging --pr 412 --sha $HEAD_SHA\n" +
			"  kelson preview render -f spec.yaml --env staging --pr 412 --sha $HEAD_SHA -o ./preview",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPreviewRender(cmd, opts)
		},
	}
	previewFlags(cmd, opts)
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "directory to write one YAML file per manifest (default: stdout, multi-document)")
	return cmd
}

// previewFlags registers the inputs both rungs share.
func previewFlags(cmd *cobra.Command, opts *previewOptions) {
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment whose previews this belongs to (optional when the input holds exactly one)")
	f.StringVar(&opts.pr, "pr", "", "change request number, as the forge numbers it")
	f.StringVar(&opts.sha, "sha", "", "head commit of the change request, in full")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig for --profile from-cluster and --registry-secret (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	// A CI workflow spells things out, and `--environment` is what somebody
	// writing one reaches for; every other kelson command spells it `--env`.
	// Both are accepted and only one is documented, which costs a hidden alias
	// and saves a class of workflow typo that fails a job.
	f.StringVar(&opts.env, "environment", "", "alias for --env")
	cobra.CheckErr(f.MarkHidden("environment"))
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	cobra.CheckErr(cmd.MarkFlagRequired("pr"))
	cobra.CheckErr(cmd.MarkFlagRequired("sha"))
}

func runPreviewRender(cmd *cobra.Command, opts *previewOptions) error {
	set, err := renderPreview(opts, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	// Where it would go is printed even though nothing goes there, because the
	// two facts a reader is checking on this rung are "what is in it" and "where
	// would it land" — and the second is invisible in the manifests.
	hint := &printer{w: cmd.ErrOrStderr()}
	hint.printf("namespace  %s\n", set.Namespace)
	hint.printf("artifact   %s:%s\n", set.Repository, set.Tag)
	if err := hint.err; err != nil {
		return err
	}

	if opts.output != "" {
		return writeManifestDir(cmd, opts.output, set.Manifests)
	}
	out, err := renderer.Encode(set.Manifests)
	if err != nil {
		return err
	}
	if _, err := cmd.OutOrStdout().Write(out); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func runPreviewPublish(cmd *cobra.Command, opts *previewOptions) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", opts.timeout)
	}
	if opts.push == nil {
		return fmt.Errorf("the artifact publisher is unavailable in this build")
	}
	set, err := renderPreview(opts, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	artifact, err := preview.Package(set)
	if err != nil {
		return err
	}

	host, err := preview.RegistryHost(set.Repository)
	if err != nil {
		return err
	}
	cred, err := publishCredential(cmd, opts, set.ParentNamespace, host)
	if err != nil {
		return err
	}

	plan := &printer{w: cmd.ErrOrStderr()}
	plan.printf("preview    %s (change request %s at %s)\n", set.Namespace, set.PR, set.SHA)
	plan.printf("resources  %d\n", len(artifact.Files))
	plan.printf("push       %s:%s as %s\n", artifact.Repository, artifact.Tag, cred)
	if err := plan.err; err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	ref, err := opts.push(cred, opts.insecure).Push(ctx, artifact)
	if err != nil {
		return err
	}

	// The reference goes to stdout and nothing else does, so the last line of
	// stdout is the artifact — the same contract `kelson build` keeps for the
	// image it pushed.
	_, err = fmt.Fprintln(cmd.OutOrStdout(), ref)
	return err
}

// renderPreview is the shared half: load the spec files, resolve the profile,
// and render for the change request. Both rungs call it, which is what makes
// `preview render` a rehearsal of `preview publish` rather than a second
// implementation of it.
//
// warn takes the profile's version-skew statements, which is the whole point of
// reporting them here: a preview is where a too-old component should surface,
// not the apply that follows it (issue #57).
func renderPreview(opts *previewOptions, warn io.Writer) (*preview.Set, error) {
	profile, err := resolveProfile(opts.profile, opts.kubeconfig, warn)
	if err != nil {
		return nil, err
	}
	project, environments, specDirs, err := loadSpecFiles(opts.files)
	if err != nil {
		return nil, err
	}
	environment, err := selectEnvironment(environments, opts.env)
	if err != nil {
		return nil, err
	}
	return preview.Render(preview.Options{
		Project:     project,
		Environment: environment,
		PR:          strings.TrimSpace(opts.pr),
		SHA:         strings.TrimSpace(opts.sha),
		Image:       opts.image,
		Profile:     profile,
		Overlays:    overlayResolver(specDirs),
	})
}

// publishCredential resolves the push credential from whichever source the
// flags name: a cluster Secret, or the runner's docker login.
//
// The Secret path goes through the same resolver the build plane uses, which is
// also where the value is registered with internal/redact — so a credential
// that arrives this way is unprintable process-wide before it is ever used.
func publishCredential(cmd *cobra.Command, opts *previewOptions, parentNamespace, host string) (registry.Credential, error) {
	if opts.registrySecret == "" {
		return preview.CredentialFromDockerConfig(opts.dockerConfig, host)
	}
	if opts.connect == nil {
		return registry.Credential{}, fmt.Errorf("resolving --registry-secret needs cluster access, which is unavailable in this build")
	}
	resolver, err := opts.connect(opts.kubeconfig)
	if err != nil {
		return registry.Credential{}, err
	}
	// The parent environment's namespace, not the preview's: the preview's
	// namespace does not exist yet — creating it is what this artifact is for —
	// and the Secret is an operator's, living where the environment's other
	// credentials live.
	namespace := opts.registryNamespace
	if namespace == "" {
		namespace = parentNamespace
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	return resolver.Resolve(ctx, registry.SecretRef{
		Name:      opts.registrySecret,
		Namespace: namespace,
		Registry:  host,
	})
}

// artifactPusher uploads a packaged artifact. It is an interface for the same
// reason buildConnector is one: the production implementation talks to a
// registry, and the wiring this command is responsible for — what gets
// packaged, under which tag, with which credential — is testable without one.
type artifactPusher interface {
	Push(ctx context.Context, a preview.Artifact) (string, error)
}

// pusherConnector builds the pusher for one run. The seam is here rather than
// inside internal/preview because the registry conversation itself is tested
// where it lives, against a fake registry (internal/preview/push_test.go).
type pusherConnector func(cred registry.Credential, insecure bool) artifactPusher

func connectPusher(cred registry.Credential, insecure bool) artifactPusher {
	return &preview.Pusher{Credential: cred, Insecure: insecure}
}

// registrySecretConnector builds a registry credential resolver. It is the seam
// the command tests replace, mirroring buildConnector: the real one needs a
// cluster and none of the wiring under test does.
//
// The name says "registry" because `kelson secret` now has a connector of its
// own (secret.go) and the two reach for different things through the same
// clientset: this one reads a dockerconfigjson Secret to authenticate a push,
// that one writes the Secrets a spec's references point at.
type registrySecretConnector func(kubeconfig string) (registry.Resolver, error)

// connectRegistrySecrets is the production connector.
func connectRegistrySecrets(kubeconfig string) (registry.Resolver, error) {
	cluster, err := kube.Connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kube.NewSecretResolver(cluster.Typed), nil
}
