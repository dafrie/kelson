// Package buildkit drives Dockerfile builds through BuildKit running in the
// target cluster (issue #48). A Dockerfile present in a repository selects
// this strategy and it takes precedence over the zero-config default
// (ADR-0010).
//
// # The shape of a build
//
// Like the renderer, everything on the "what should run" side of this driver
// is a pure function: (build.Request, Config) → a Kubernetes workload, with no
// cluster, daemon or network. Config.Workload returns the manifest; a driver's
// only side-effecting work is handing that manifest to an injected Cluster and
// streaming the logs back (Build).
//
// # Rootless, provably
//
// Multi-tenancy rules out privileged builders (issue #48: "running privileged
// builders in a shared cluster is not acceptable"). The generated workload
// runs the moby/buildkit *-rootless image with a non-root security context and
// no privileged capabilities; WorkloadBuildIsNotPrivileged is the acceptance
// test for that, not an intention.
//
// # The cluster seam
//
// Executing a build needs a Kubernetes/BuildKit client. That is pulled out
// behind the Cluster interface below so every test drives a fake — no test
// here needs a cluster, a daemon, or a network. The concrete Kubernetes-backed
// implementation cannot live in this package: internal/build sits on the main
// lint allow-list, which forbids the client-go libraries (.golangci.yml), so
// the caller provides a Cluster the same way delivery.direct takes its client.
package buildkit

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/redact"
)

// StrategyName is the builder strategy id for Dockerfile builds (ADR-0010).
// It is what build.Builder.Name returns and what a strategy registry keys on.
const StrategyName = "dockerfile"

// Cluster submits and watches build workloads. It is injectable so the driver
// is testable without a cluster; the Kubernetes-backed implementation lives
// with the caller (this plane cannot import client-go, .golangci.yml).
type Cluster interface {
	// Submit creates the build Job from its manifest and returns its name.
	Submit(ctx context.Context, manifest []byte) (string, error)
	// Wait streams the Job's logs to w until it terminates. Streaming is part
	// of the build contract (issue #5, ADR-0010): logs arrive as produced,
	// not in one blast at the end. w may be nil. The returned Result carries
	// the pushed reference, digest and platforms.
	Wait(ctx context.Context, name string, w io.Writer) (build.Result, error)
}

// Config is everything Workload needs beyond build.Request to render a build
// Job. None of it enters the kelson spec (ADR-0010) — it is operator/concrete
// driver configuration, the same status as a delivery adapter's client.
type Config struct {
	// Namespace the build Job runs in. Empty is not valid for an in-cluster
	// build and is rejected by Workload.
	Namespace string
	// BuildkitImage is the rootless buildkit image, e.g.
	// moby/buildkit:v0.20.0-rootless. Defaults to DefaultBuildkitImage.
	BuildkitImage string
	// GitImage clones the source into the workspace. Defaults to
	// DefaultGitImage. It runs as an unprivileged init container.
	GitImage string
	// ServiceAccount the Job runs as, when the caller wants a non-default SA.
	ServiceAccount string
	// PushSecret is the name of a kubernetes.io/dockerconfigjson Secret in
	// Namespace holding the credential for the destination registry. It is a
	// reference, never a value (ADR-0009): kelson does not create it, does not
	// read it, and never puts a credential in a manifest it renders — the
	// kubelet projects it into the build pod and buildctl picks it up from
	// $DOCKER_CONFIG.
	//
	// Empty means an unauthenticated push, which is correct for a local
	// registry (kind, a cluster-internal registry) and fails at push time for
	// anything that requires auth.
	PushSecret string
	// Secrets are build-time secrets, projected as BuildKit secret mounts and
	// never as build arguments or image layers (ADR-0009, issue #117). Each
	// names an existing Kubernetes Secret in Namespace; see secrets.go for why
	// there is no field here that could carry a value.
	Secrets []SecretMount
	// Resources applied to the build container. May be zero.
	Resources ResourceRequirements
	// Timeout bounds the whole build; "" means no deadline. Non-empty values
	// must be a valid time.Duration.
	Timeout Duration
}

// ResourceRequirements is a thin re-declaration of the Kubernetes resource
// shape, kept here so Workload stays pure (no k8s imports, .golangci.yml).
// Values are the usual millicore / byte-quantity strings.
type ResourceRequirements struct {
	RequestsCPU    string
	RequestsMemory string
	LimitsCPU      string
	LimitsMemory   string
}

// Duration is a time.Duration that also marshals cleanly for callers (it is a
// plain type so Config can live as config without dragging in the time package
// everywhere).
type Duration time.Duration

// Options configures a Driver.
type Options struct {
	// Cluster executes build workloads. Required.
	Cluster Cluster
	// Config supplies the non-request part of the workload.
	Config Config
}

// Driver is the build.Builder for Dockerfile strategy (ADR-0010). It renders
// the build Job purely, submits it through Cluster, and streams the result.
type Driver struct {
	name    string
	cluster Cluster
	cfg     Config
}

var _ build.Builder = (*Driver)(nil)

// New returns a Driver. A Cluster is required; Config gets its defaults.
func New(opts Options) (*Driver, error) {
	if opts.Cluster == nil {
		return nil, errors.New("buildkit: a Cluster is required")
	}
	return &Driver{name: StrategyName, cluster: opts.Cluster, cfg: opts.Config.withDefaults()}, nil
}

// Name implements build.Builder: the Dockerfile strategy id (ADR-0010).
func (d *Driver) Name() string { return d.name }

// Workload renders the build Job for req. It is pure — same inputs, same
// bytes — and is the unit the non-privileged acceptance test checks.
func (d *Driver) Workload(req build.Request) ([]byte, error) {
	return d.cfg.Workload(req)
}

// Build renders the workload, submits it through the injected Cluster, and
// streams its logs to w until completion. Safe to call concurrently for
// different Requests: each Request renders its own Job with no shared mutable
// state.
//
// The log stream passes through the known-value scrubber (issue #117) so a
// credential kelson has resolved cannot reach a build log even if the builder
// echoes it — buildkitd has printed registry auth config in debug modes, and
// "the builder would not do that" is not a property. Everything kelson holds
// only as a reference (the push Secret, Config.Secrets) is not scrubbed here
// because it was never in this process to begin with; the Job carries names and
// paths, and the kubelet does the projection.
//
// kelson does not sniff the rest of the build's output. A Dockerfile that cats
// its own secret into stdout is printing the user's bytes, and guessing which
// of them are credentials would corrupt real output while still missing the
// credential that looks like a word (internal/redact package doc).
func (d *Driver) Build(ctx context.Context, req build.Request, w io.Writer) (build.Result, error) {
	manifest, err := d.cfg.Workload(req)
	if err != nil {
		return build.Result{}, err
	}
	name, err := d.cluster.Submit(ctx, manifest)
	if err != nil {
		return build.Result{}, err
	}
	logs := redact.Registered().Writer(w)
	res, werr := d.cluster.Wait(ctx, name, logs)
	if flusher, ok := logs.(*redact.ScrubWriter); ok {
		if ferr := flusher.Flush(); ferr != nil && werr == nil {
			werr = ferr
		}
	}
	return res, werr
}
