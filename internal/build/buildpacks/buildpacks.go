// Package buildpacks drives Cloud Native Buildpacks builds through the
// lifecycle running in the target cluster (issue #49). When no Dockerfile is
// present, buildpacks is the zero-config default (ADR-0010); the detection
// that decides *when* it is chosen lives in internal/build/detect, and this
// package implements the build itself.
//
// # The shape of a build
//
// Mirrors internal/build/buildkit exactly. Everything on the "what should run"
// side of this driver is a pure function: (build.Request, Config) → a
// Kubernetes workload, with no cluster, daemon or network. Config.Workload
// returns the manifest; a driver's only side-effecting work is handing that
// manifest to an injected Cluster and streaming the logs back (Build).
//
// # What this driver chooses, and what it does not
//
// The lifecycle does buildpack detection and language coverage: it reads the
// source tree at /workspace, picks the buildpacks that match the app's
// language, and reports what it chose in its output. This package only gives
// the lifecycle the source and the parameters (builder, run image, cache,
// destination), then returns whatever it pushed. That boundary is deliberate
// (ADR-0010): kelson does not re-implement detection, and no buildpack DSL
// enters the spec.
//
// # Builder images and the kelson config surface
//
// The builder, run image, cache and extra buildpacks are operator/driver
// configuration on Config — the same status as a delivery adapter's client or
// buildkit's image pick — never strategy fields in the kelson spec (ADR-0010).
// "Configure buildpacks" means setting Config here, not teaching the spec a
// builder DSL.
//
// # Rootless, provably
//
// Multi-tenancy rules out privileged builders (issue #48). Like buildkit, the
// generated workload runs the builder image with a non-root security context
// and no privileged capabilities; TestWorkloadLifecycleIsNotPrivileged is the
// acceptance test for that, not an intention.
//
// # The cluster seam
//
// Executing a build needs a Kubernetes/BuildKit client, and rebase needs an
// OCI manifest client. Both are pulled out behind interfaces (Cluster,
// Rebaser) so every test drives a fake — no test here needs a cluster, a
// daemon, or a network. The concrete Kubernetes- and registry-backed
// implementations cannot live in this package: internal/build sits on the main
// lint allow-list, which forbids client-go and the registry SDKs (.golangci.yml),
// so the caller provides them the same way delivery.direct takes its client.
package buildpacks

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/registry"
)

// StrategyName is the builder strategy id for buildpacks (ADR-0010). It is
// what build.Builder.Name returns and what a strategy registry keys on.
const StrategyName = "buildpacks"

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

// Rebaser patches a built image onto a new run image without rebuilding from
// source (issue #49, ADR-0010). It is injectable because it needs an OCI
// registry client, which this plane's lint allow-list forbids; the concrete
// implementation lives with the caller, the same way Cluster does.
type Rebaser interface {
	// Rebase rewrites ref (a digest-pinned application image built by this
	// driver) onto newRunImage and returns the new digest-pinned reference.
	Rebase(ctx context.Context, ref, newRunImage string) (string, error)
}

// Config is everything Workload needs beyond build.Request to render a
// buildpacks build Job, plus the operator knobs for caching and custom
// buildpacks. None of it enters the kelson spec (ADR-0010) — it is
// operator/concrete driver configuration.
type Config struct {
	// Namespace the build Job runs in. Empty is not valid and is rejected by
	// Workload.
	Namespace string
	// BuilderImage is the build image running the lifecycle, e.g.
	// paketobuildpacks/builder-jammy-base (the zero-config default; see
	// DefaultBuilder in workload.go). Empty selects DefaultBuilder.
	BuilderImage string
	// RunImage is the base image app layers are stacked onto and that rebase
	// replaces. Empty selects DefaultRunImage.
	RunImage string
	// ServiceAccount the Job runs as, when the caller wants a non-default SA.
	ServiceAccount string
	// Resources applied to the build container. May be zero.
	Resources ResourceRequirements
	// Timeout bounds the whole build; "" means no deadline. Non-empty values
	// must be a valid time.Duration.
	Timeout Duration
	// Buildpacks are extra buildpacks to add without forking the builder
	// (issue #49 custom registration). Each is a lifecycle buildpack URI or
	// reference passed straight through to the lifecycle. Empty uses the
	// builder's bundled buildpacks.
	Buildpacks []string
	// CacheMode selects the registry cache export mode (ADR-0011). Empty
	// defaults to CacheModeMin, the recommended default.
	CacheMode CacheMode
}

// CacheMode controls how much of a build the registry cache retains
// (ADR-0011).
type CacheMode string

const (
	// CacheModeMin caches only the final image layers, so intermediates it
	// contains are no more sensitive than the image itself (ADR-0011 default).
	CacheModeMin CacheMode = "min"
	// CacheModeMax also caches intermediate layers, which can contain
	// build-time file contents and source that never reach the final image; a
	// max cache is therefore as sensitive as the source tree (ADR-0011).
	CacheModeMax CacheMode = "max"
)

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
	// Rebaser performs rebase onto a new run image. Optional: only Rebase
	// needs it.
	Rebaser Rebaser
}

// Driver is the build.Builder for the buildpacks strategy (ADR-0010). It
// renders the build Job purely, submits it through Cluster, and streams the
// result; Rebase patches a run image through Rebaser.
type Driver struct {
	name    string
	cluster Cluster
	rebaser Rebaser
	cfg     Config
}

var _ build.Builder = (*Driver)(nil)

// New returns a Driver. A Cluster is required; Config gets its defaults.
func New(opts Options) (*Driver, error) {
	if opts.Cluster == nil {
		return nil, errors.New("buildpacks: a Cluster is required")
	}
	return &Driver{
		name:    StrategyName,
		cluster: opts.Cluster,
		rebaser: opts.Rebaser,
		cfg:     opts.Config.withDefaults(),
	}, nil
}

// Name implements build.Builder: the buildpacks strategy id (ADR-0010).
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
func (d *Driver) Build(ctx context.Context, req build.Request, w io.Writer) (build.Result, error) {
	manifest, err := d.cfg.Workload(req)
	if err != nil {
		return build.Result{}, err
	}
	name, err := d.cluster.Submit(ctx, manifest)
	if err != nil {
		return build.Result{}, err
	}
	return d.cluster.Wait(ctx, name, w)
}

// Rebase patches the run image of a previously built application onto
// newRunImage and returns the new digest-pinned reference, without rebuilding
// application source (issue #49, ADR-0010).
//
// Honest limit: rebase replaces the compatible run-image layers underneath a
// built application. It is the fast fleet-wide path for base-image CVEs, but
// it does NOT rebuild application dependencies — a transitive dependency
// vulnerability in the app's own layers still needs a full rebuild. Rebase is
// a run-image patch, not a CVE solution for application code.
//
// Both ref and newRunImage must be digest-pinned: rebase mutates a specific
// built image, and mutating a mutable tag would be non-reproducible. It
// requires the injected Rebaser; without one it fails closed.
func (d *Driver) Rebase(ctx context.Context, ref, newRunImage string) (string, error) {
	if err := requirePinned(ref, "ref"); err != nil {
		return "", err
	}
	if err := requirePinned(newRunImage, "new run image"); err != nil {
		return "", err
	}
	if d.rebaser == nil {
		return "", errors.New("buildpacks: Rebase needs an injected Rebaser (Options.Rebaser)")
	}
	return d.rebaser.Rebase(ctx, ref, newRunImage)
}

// requirePinned ensures an image reference is pinned by digest, so a rebase
// can never silently retarget a mutable tag. Fail closed: anything unpinned is
// rejected.
func requirePinned(ref, what string) error {
	if ref == "" {
		return errors.New("buildpacks: " + what + " must be set for a rebase")
	}
	r, err := registry.Parse(ref)
	if err != nil {
		return errors.New("buildpacks: invalid " + what + ": " + err.Error())
	}
	if r.Digest == "" {
		return errors.New("buildpacks: " + what + " must be pinned by digest before a rebase")
	}
	return nil
}
