package buildpacks

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/registry"
)

// DefaultBuilder is the zero-config builder image the generated Job runs when
// Config.BuilderImage is empty. It is a Paketo Jammy Base builder.
//
// Why Jammy Base over the alternatives:
//
//   - Multi-language coverage out of the box. builder-jammy-base ships the
//     buildpacks for exactly the acceptance set of this issue — Node
//     (package.json), Python (requirements.txt / pyproject.toml), Go (go.mod)
//     and Ruby (Gemfile) — with no per-language configuration.
//   - A base (not full) stack keeps the image smaller than builder-jammy-full,
//     and a base stack is what makes run-image patching via rebase meaningful:
//     the OS layer stays thin, so a base-image CVE is fixed by swapping the
//     run image rather than by rebuilding application layers.
//   - Paketo provides matching builder/run image pairs
//     (builder-jammy-base + run-jammy-base) that are rebuilt and patched on a
//     published cadence, which is the governance justification in ADR-0010 for
//     delegating to a well-governed upstream.
//
// It is still operator/driver configuration (Config.BuilderImage), selectable
// per application through kelson's own config surface — never a builder DSL in
// the spec (ADR-0010).
const DefaultBuilder = "paketobuildpacks/builder-jammy-base"

// DefaultRunImage is the base image built applications are stacked onto, and
// what rebase replaces. It matches DefaultBuilder's stack.
const DefaultRunImage = "paketobuildpacks/run-jammy-base"

// Default rootless username/group the builder image runs the lifecycle as.
const (
	defaultRunAsUser  = 1000
	defaultRunAsGroup = 1000
)

// Names helpers inherited from the delivery plane, mirroring renderer
// provenance (docs/architecture.md) and buildkit's.
const (
	labelProject     = "kelson.dev/project"
	labelEnvironment = "kelson.dev/environment"
	labelApplication = "kelson.dev/application"
	labelStrategy    = "kelson.dev/build-strategy"
	annRevision      = "kelson.dev/revision"
)

// cacheSuffix marks a cache repository derived from (never shared with) its
// image repository (ADR-0011). appending it must be injective over repository
// names so two different applications can never derive the same cache repo.
const cacheSuffix = "-cache"

// withDefaults fills unset parts of the config: the builder and run images and
// a zero timeout and the recommended cache mode.
func (c Config) withDefaults() Config {
	if c.BuilderImage == "" {
		c.BuilderImage = DefaultBuilder
	}
	if c.RunImage == "" {
		c.RunImage = DefaultRunImage
	}
	if c.CacheMode == "" {
		c.CacheMode = CacheModeMin
	}
	return c
}

// Workload is the pure render step: (Request, Config) → the YAML of a
// buildpacks build Job. Same inputs, same bytes. It is what the non-privileged
// acceptance test asserts on, and it never touches a cluster.
func (c Config) Workload(req build.Request) ([]byte, error) {
	if err := validate(req, c); err != nil {
		return nil, err
	}
	cfg := c.withDefaults()

	cacheRef, err := CacheRef(req.Image)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		labelProject:     req.Project,
		labelEnvironment: req.Environment,
		labelStrategy:    StrategyName,
	}
	annotations := map[string]string{}
	if req.Application != "" {
		labels[labelApplication] = req.Application
	}
	if req.Revision != "" {
		annotations[annRevision] = req.Revision
	}

	name := jobName(req)
	ctr := podContainer(req, cfg, cacheRef)

	podSpec := podSpec{
		ServiceAccountName: cfg.ServiceAccount,
		RestartPolicy:      "Never",
		Containers:         []container{ctr},
		Volumes:            baseVolumes(),
	}

	job := workload{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: metadata{
			Name:        name,
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: jobSpec{
			BackoffLimit:          intPtr(0),
			ActiveDeadlineSeconds: secondsPtr(cfg.Timeout),
			Template: podTemplate{
				Metadata: podTemplateMetadata{Labels: labels},
				Spec:     podSpec,
			},
		},
	}

	return yaml.Marshal(job)
}

// validate enforces the invariants a build Job needs before it is rendered:
// an image to push to, a namespace to run in, a well-formed timeout, and a
// recognised cache mode.
func validate(req build.Request, c Config) error {
	if req.Image == "" {
		return fmt.Errorf("buildpacks: Workload needs a destination image (Request.Image)")
	}
	if c.Namespace == "" {
		return fmt.Errorf("buildpacks: Workload needs a namespace (Config.Namespace)")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("buildpacks: negative build timeout %s", time.Duration(c.Timeout))
	}
	if c.CacheMode != "" && c.CacheMode != CacheModeMin && c.CacheMode != CacheModeMax {
		return fmt.Errorf("buildpacks: unknown cache mode %q", c.CacheMode)
	}
	return nil
}

// CacheRef derives the registry cache reference for a build from the identity
// that owns the image repository, per ADR-0011: cache is scoped per
// (project, application) and shared across environments of one application but
// never across projects. Because an application's image repository already
// encodes its owning project and application, deriving the cache from that
// repository (and not from any global/shared name) gives the isolation
// property by construction: it is a pure, injective function of the
// repository, so two different applications can never derive the same cache
// reference. See TestCacheReferenceIsolationAcrossProjects.
//
// The cache lives in a sibling repository named "<image-repo>-cache" so it is
// visible next to the images it builds, carries the same access control, and
// evicts under the same registry retention as the images themselves
// (ADR-0011). Tags and digests are dropped: the cache repo is distinguished by
// name, not by tag, so it is never mistaken for a deployable image.
func CacheRef(image string) (string, error) {
	r, err := registry.Parse(image)
	if err != nil {
		return "", err
	}
	r.Tag = ""
	r.Digest = ""
	r.Repository += cacheSuffix
	return r.String(), nil
}

// jobName derives a stable, DNS-1123-safe name for the Job from the request so
// the same build reuses the same identity and logs can be correlated.
func jobName(req build.Request) string {
	return sanitizeName(strings.Join(
		[]string{"build", req.Project, req.Application, shortRev(req.Revision)}, "-"))
}

// shortRev keeps the name short for the revision part of the Job name.
func shortRev(rev string) string {
	rev = strings.TrimPrefix(rev, "sha256:")
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}

// sanitizeName makes a Kubernetes-compliant object name: lowercase
// alphanumeric and '-', no leading/trailing '-', at most 63 chars.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastDash := true
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if len(out) > 63 {
		out = out[:63]
	}
	if out == "" {
		return "build"
	}
	return out
}

// podContainer assembles the build container: the builder image running the
// lifecycle rootless, with the source mounted, a per-application registry
// cache, and a non-root security context.
func podContainer(req build.Request, cfg Config, cacheRef string) container {
	volumeMounts := []volumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "layers", MountPath: "/layers"},
		{Name: "launch-cache", MountPath: "/launch-cache"},
		{Name: "tmp", MountPath: "/tmp"},
	}

	ctr := container{
		Name:         "buildpack",
		Image:        cfg.BuilderImage,
		Command:      []string{"sh", "-c", buildCommand(req, cfg, cacheRef)},
		VolumeMounts: volumeMounts,
		SecurityContext: &securityContext{
			RunAsNonRoot:             boolPtr(true),
			RunAsUser:                int64Ptr(defaultRunAsUser),
			RunAsGroup:               int64Ptr(defaultRunAsGroup),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities:             &capabilities{Drop: []string{"ALL"}},
		},
		Resources: resourceReqs(cfg.Resources),
	}
	return ctr
}

// buildCommand is the build itself: the lifecycle creator detects the app's
// language from /workspace, picks the matching buildpacks, builds, and pushes
// to the destination with a per-application registry cache. Detection is the
// lifecycle's job and its choices surface in creator's output, which the
// caller streams back (ADR-0010); this driver only supplies the parameters.
//
// Flags: the destination is <image>:<tag> (or bare <image>), the cache image is
// the ADR-0011 per-application reference, and extra buildpacks registered via
// Config.Buildpacks are passed through verbatim so users can add buildpacks
// without forking the builder. Registry credentials for the push/cache are
// injected out-of-band by the cluster seam, never embedded here. The exact
// lifecycle invocation is what the end-to-end harness (#86) validates against
// a real registry; no unit test here can prove it runs.
func buildCommand(req build.Request, cfg Config, cacheRef string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")

	dest := req.Image
	if req.Tag != "" {
		dest += ":" + req.Tag
	}

	b.WriteString("exec /cnb/lifecycle/creator")
	fmt.Fprintf(&b, " -app /workspace")
	fmt.Fprintf(&b, " -layers /layers")
	fmt.Fprintf(&b, " -launch-cache /launch-cache")
	fmt.Fprintf(&b, " -cache-image %s", cacheRef)
	fmt.Fprintf(&b, " -report /tmp/report.toml")
	fmt.Fprintf(&b, " -run-image %s", cfg.RunImage)
	fmt.Fprintf(&b, " -process-type web")
	fmt.Fprintf(&b, " -image %s", dest)
	// Extra buildpacks, in the caller's order: registration order is
	// meaningful to the lifecycle, so we preserve it rather than sort.
	for _, bp := range cfg.Buildpacks {
		fmt.Fprintf(&b, " -buildpack %s", bp)
	}
	return b.String()
}

// --- typed manifest shapes (mirrors buildkit/workload.go) -----------------

type workload struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   metadata `yaml:"metadata"`
	Spec       jobSpec  `yaml:"spec"`
}

type metadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type jobSpec struct {
	BackoffLimit          *int        `yaml:"backoffLimit"`
	ActiveDeadlineSeconds *int64      `yaml:"activeDeadlineSeconds,omitempty"`
	Template              podTemplate `yaml:"template"`
}

type podTemplate struct {
	Metadata podTemplateMetadata `yaml:"metadata"`
	Spec     podSpec             `yaml:"spec"`
}

type podTemplateMetadata struct {
	Labels map[string]string `yaml:"labels,omitempty"`
}

type podSpec struct {
	ServiceAccountName string      `yaml:"serviceAccountName,omitempty"`
	RestartPolicy      string      `yaml:"restartPolicy"`
	Containers         []container `yaml:"containers"`
	Volumes            []volume    `yaml:"volumes"`
}

type container struct {
	Name            string           `yaml:"name"`
	Image           string           `yaml:"image"`
	Command         []string         `yaml:"command"`
	VolumeMounts    []volumeMount    `yaml:"volumeMounts"`
	SecurityContext *securityContext `yaml:"securityContext"`
	Resources       resources        `yaml:"resources"`
}

type volumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

type volume struct {
	Name     string          `yaml:"name"`
	EmptyDir *emptyDirVolume `yaml:"emptyDir"`
}

type emptyDirVolume struct{}

type securityContext struct {
	RunAsNonRoot             *bool         `yaml:"runAsNonRoot"`
	RunAsUser                *int64        `yaml:"runAsUser,omitempty"`
	RunAsGroup               *int64        `yaml:"runAsGroup,omitempty"`
	AllowPrivilegeEscalation *bool         `yaml:"allowPrivilegeEscalation"`
	Privileged               *bool         `yaml:"privileged,omitempty"`
	Capabilities             *capabilities `yaml:"capabilities,omitempty"`
}

type capabilities struct {
	Add  []string `yaml:"add,omitempty"`
	Drop []string `yaml:"drop,omitempty"`
}

type resources struct {
	Requests resourceList `yaml:"requests,omitempty"`
	Limits   resourceList `yaml:"limits,omitempty"`
}

type resourceList struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

// baseVolumes the build Job always needs: the source workspace (populated by
// the Cluster on Submit), and writable layers, launch-cache and tmp that the
// rootless builder requires without a privileged node.
func baseVolumes() []volume {
	return []volume{
		{Name: "workspace", EmptyDir: &emptyDirVolume{}},
		{Name: "layers", EmptyDir: &emptyDirVolume{}},
		{Name: "launch-cache", EmptyDir: &emptyDirVolume{}},
		{Name: "tmp", EmptyDir: &emptyDirVolume{}},
	}
}

func resourceReqs(r ResourceRequirements) resources {
	out := resources{}
	if r.RequestsCPU != "" {
		out.Requests.CPU = r.RequestsCPU
	}
	if r.RequestsMemory != "" {
		out.Requests.Memory = r.RequestsMemory
	}
	if r.LimitsCPU != "" {
		out.Limits.CPU = r.LimitsCPU
	}
	if r.LimitsMemory != "" {
		out.Limits.Memory = r.LimitsMemory
	}
	return out
}

func intPtr(i int) *int       { return &i }
func int64Ptr(i int64) *int64 { return &i }
func boolPtr(b bool) *bool    { return &b }
func secondsPtr(d Duration) *int64 {
	if d == 0 {
		return nil
	}
	s := int64(time.Duration(d) / time.Second)
	if s == 0 {
		s = 1
	}
	return &s
}
