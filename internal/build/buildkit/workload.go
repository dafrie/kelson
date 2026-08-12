package buildkit

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/build"
)

// DefaultBuildkitImage is the rootless buildkit image the generated Job runs.
// It is deliberately *-rootless: running a privileged builder is rejected for
// multi-tenancy (issue #48).
const DefaultBuildkitImage = "moby/buildkit:v0.20.0-rootless"

// DefaultGitImage clones the source. Alpine's git image is small and needs no
// privileges; the clone runs as the same non-root user as the build.
const DefaultGitImage = "alpine/git:latest"

// Default rootless service account username/group the buildkit image runs as.
const (
	defaultRunAsUser  = 1000
	defaultRunAsGroup = 1000
)

// Names helpers inherited from the delivery plane, mirroring renderer
// provenance (docs/architecture.md).
const (
	labelProject     = "kelson.dev/project"
	labelEnvironment = "kelson.dev/environment"
	labelApplication = "kelson.dev/application"
	annRevision      = "kelson.dev/revision"
)

// withDefaults fills unset parts of the config: the buildkit image and a zero
// timeout. It does not require a namespace; that is validated when a workload
// is actually rendered.
func (c Config) withDefaults() Config {
	if c.BuildkitImage == "" {
		c.BuildkitImage = DefaultBuildkitImage
	}
	if c.GitImage == "" {
		c.GitImage = DefaultGitImage
	}
	return c
}

// Workload is the pure render step: (Request, Config) → the YAML of a build
// Job. Same inputs, same bytes. It is what the non-privileged acceptance test
// asserts on, and it never touches a cluster.
func (c Config) Workload(req build.Request) ([]byte, error) {
	if err := validate(req, c); err != nil {
		return nil, err
	}
	cfg := c.withDefaults()

	labels := map[string]string{
		labelProject:     req.Project,
		labelEnvironment: req.Environment,
	}
	annotations := map[string]string{}
	if req.Application != "" {
		labels[labelApplication] = req.Application
	}
	if req.Revision != "" {
		annotations[annRevision] = req.Revision
	}

	name := jobName(req)
	ctr := podContainer(req, cfg)

	podSpec := podSpec{
		ServiceAccountName: cfg.ServiceAccount,
		RestartPolicy:      "Never",
		InitContainers:     sourceInitContainers(req, cfg),
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
// an image to push to, a namespace to run in, and a well-formed timeout.
func validate(req build.Request, c Config) error {
	if req.Image == "" {
		return fmt.Errorf("buildkit: Workload needs a destination image (Request.Image)")
	}
	if c.Namespace == "" {
		return fmt.Errorf("buildkit: Workload needs a namespace (Config.Namespace)")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("buildkit: negative build timeout %s", time.Duration(c.Timeout))
	}
	return nil
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

// podContainer assembles the build container: the rootless buildkit image,
// the rootless flags, and its non-root security context.
func podContainer(req build.Request, cfg Config) container {
	volumeMounts := []volumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "buildkit-state", MountPath: "/home/user/.local/share/buildkit"},
		{Name: "tmp", MountPath: "/tmp"},
	}

	ctr := container{
		Name:         "buildkit",
		Image:        cfg.BuildkitImage,
		Command:      []string{"sh", "-c", buildCommand(req)},
		VolumeMounts: volumeMounts,
		Env:          []envVar{{Name: "BUILDKIT_ROOTLESS", Value: "true"}},
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

// buildCommand is the rootless build itself: buildkitd daemon in the
// background, then buildctl builds from the mounted context and pushes by
// digest. Args, target and platforms are all leveraged through buildctl, never
// through a kelson DSL (ADR-0010).
func buildCommand(req build.Request) string {
	const sock = "/run/user/1000/buildkit/buildkitd.sock"

	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	fmt.Fprintf(&b, "buildkitd --oci-worker-no-process-sandbox --oci-worker-snapshotter=native --addr unix://%s &\n", sock)
	fmt.Fprintf(&b, "until buildctl --addr unix://%s debug workers >/dev/null 2>&1; do sleep 1; done\n", sock)

	b.WriteString("exec buildctl --addr unix://")
	b.WriteString(sock)
	b.WriteString(" build --frontend dockerfile.v0")
	ctxPath := contextPath(req)
	fmt.Fprintf(&b, " --local context=%s", ctxPath)
	if req.Dockerfile != "" && req.Dockerfile != "Dockerfile" {
		fmt.Fprintf(&b, " --local dockerfile=%s --opt filename=%s", ctxPath, req.Dockerfile)
	} else {
		fmt.Fprintf(&b, " --local dockerfile=%s", ctxPath)
	}
	if req.Target != "" {
		fmt.Fprintf(&b, " --opt target=%s", req.Target)
	}
	platforms := req.Platforms
	if len(platforms) > 0 {
		fmt.Fprintf(&b, " --opt platform=%s", strings.Join(platforms, ","))
	}
	if len(req.Args) > 0 {
		// Sorted for deterministic output: build args are never secrets, so
		// sorting them for stable manifests costs nothing.
		keys := make([]string, 0, len(req.Args))
		for k := range req.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, " --build-arg %s=%s", k, req.Args[k])
		}
	}
	if req.Tag == "" {
		fmt.Fprintf(&b, " --output type=image,image-format=oci,name=%s,push=true", req.Image)
	} else {
		fmt.Fprintf(&b, " --output type=image,image-format=oci,name=%s:%s,push=true", req.Image, req.Tag)
	}
	return b.String()
}

// --- typed manifest shapes -------------------------------------------------

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
	InitContainers     []container `yaml:"initContainers,omitempty"`
	Containers         []container `yaml:"containers"`
	Volumes            []volume    `yaml:"volumes"`
}

type container struct {
	Name            string           `yaml:"name"`
	Image           string           `yaml:"image"`
	Command         []string         `yaml:"command"`
	Env             []envVar         `yaml:"env,omitempty"`
	VolumeMounts    []volumeMount    `yaml:"volumeMounts"`
	SecurityContext *securityContext `yaml:"securityContext"`
	Resources       resources        `yaml:"resources"`
}

type envVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
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
// the Cluster on Submit), and writable buildkit state + tmp that the rootless
// image requires without a privileged node.
func baseVolumes() []volume {
	return []volume{
		{Name: "workspace", EmptyDir: &emptyDirVolume{}},
		{Name: "buildkit-state", EmptyDir: &emptyDirVolume{}},
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

// sourceInitContainers clones the application source into the workspace before
// the build container starts (#48 scope: build context handling from a Git
// source).
//
// Cloning in-pod rather than uploading a context keeps the control plane out
// of the data path: kelson never streams a source tree through itself, and a
// large repository costs the build pod's bandwidth rather than the server's.
//
// It returns nil when no source is configured, which is the case the tests
// covering a pre-populated workspace rely on.
func sourceInitContainers(req build.Request, cfg Config) []container {
	if req.SourceGit == "" {
		return nil
	}
	return []container{{
		Name:         "clone",
		Image:        cfg.GitImage,
		Command:      []string{"sh", "-c", cloneCommand(req)},
		VolumeMounts: []volumeMount{{Name: "workspace", MountPath: "/workspace"}},
		SecurityContext: &securityContext{
			RunAsNonRoot:             boolPtr(true),
			RunAsUser:                int64Ptr(defaultRunAsUser),
			RunAsGroup:               int64Ptr(defaultRunAsGroup),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities:             &capabilities{Drop: []string{"ALL"}},
		},
		Resources: resourceReqs(cfg.Resources),
	}}
}

// cloneCommand fetches exactly one commit where it can.
//
// A ref that names a commit is fetched directly at depth 1, which is both the
// fastest path and the only one that guarantees the build matches Revision. A
// branch or tag is cloned at depth 1 instead; that is a moving target, and the
// comment says so rather than pretending the result is pinned.
func cloneCommand(req build.Request) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("git init -q /workspace\n")
	b.WriteString("cd /workspace\n")
	fmt.Fprintf(&b, "git remote add origin %s\n", shellQuote(req.SourceGit))
	ref := req.SourceRef
	if ref == "" {
		ref = "HEAD"
	}
	fmt.Fprintf(&b, "git fetch --depth 1 origin %s\n", shellQuote(ref))
	b.WriteString("git checkout -q FETCH_HEAD\n")
	return b.String()
}

// shellQuote wraps a value in single quotes for the generated shell command.
// The values here come from the spec, not from a build's own output, but a
// repository URL is still user input reaching a shell — quoting it is the
// difference between a config error and a command injection.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// contextPath is the build context inside the cloned workspace. A monorepo
// clones whole and builds one subdirectory, so ContextDir selects the subtree
// rather than changing what is fetched.
func contextPath(req build.Request) string {
	dir := strings.Trim(req.ContextDir, "/")
	if dir == "" || dir == "." {
		return "/workspace"
	}
	return "/workspace/" + dir
}
