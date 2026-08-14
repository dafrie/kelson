package buildpacks

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/build"
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

// DefaultGitImage clones the source, the same image and for the same reason as
// buildkit's: small, needs no privileges, and the clone runs as the same
// non-root user as the build.
const DefaultGitImage = "alpine/git:latest"

// Default rootless username/group the builder image runs the lifecycle as.
const (
	defaultRunAsUser  = 1000
	defaultRunAsGroup = 1000
)

// buildContainerName is the container running the lifecycle. It is the one the
// executor streams logs from, which it reads off the rendered Job rather than
// knowing by name.
const buildContainerName = "buildpack"

// reportPath is where the lifecycle writes its report (`-report`). The build
// script reads the pushed digest back out of it, which is the only place the
// creator states it as data rather than as prose.
const reportPath = "/tmp/report.toml"

// Names helpers inherited from the delivery plane, mirroring renderer
// provenance (docs/architecture.md) and buildkit's.
const (
	labelProject     = "kelson.dev/project"
	labelEnvironment = "kelson.dev/environment"
	labelApplication = "kelson.dev/application"
	labelStrategy    = "kelson.dev/build-strategy"
)

// Push-credential wiring, identical to buildkit's and for the same reason: the
// lifecycle authenticates a push through a Docker config file resolved from
// $DOCKER_CONFIG, not through a Kubernetes imagePullSecret, so the
// dockerconfigjson Secret is projected as a file the build container reads.
const (
	// dockerConfigDir is under the builder image's home (the cnb user, uid
	// 1000).
	dockerConfigDir = "/home/cnb/.docker"
	// dockerConfigJSONKey is the fixed key of a kubernetes.io/dockerconfigjson
	// Secret, and dockerConfigFile is what the docker config loader looks for.
	dockerConfigJSONKey = ".dockerconfigjson"
	dockerConfigFile    = "config.json"
	// pushSecretVolume names the projected credential volume.
	pushSecretVolume = "push-secret"
	// pushSecretMode is 0444: the credential is read-only, and readable by the
	// non-root build user, which a Secret volume's files are not by default —
	// they are owned by root, so a 0400 projection would be unreadable by the
	// uid the lifecycle runs as.
	pushSecretMode int32 = 0o444
)

// withDefaults fills unset parts of the config: the builder, run and git
// images and a zero timeout.
func (c Config) withDefaults() Config {
	if c.BuilderImage == "" {
		c.BuilderImage = DefaultBuilder
	}
	if c.RunImage == "" {
		c.RunImage = DefaultRunImage
	}
	if c.GitImage == "" {
		c.GitImage = DefaultGitImage
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

	labels := map[string]string{
		labelProject:     req.Project,
		labelEnvironment: req.Environment,
		labelStrategy:    StrategyName,
	}
	// The destination is annotated rather than left to be read back out of the
	// lifecycle's command line: it is what the executor turns into
	// Result.Reference (build.AnnotationImage).
	annotations := map[string]string{build.AnnotationImage: req.Image}
	if req.Application != "" {
		labels[labelApplication] = req.Application
	}
	if req.Tag != "" {
		annotations[build.AnnotationTag] = req.Tag
	}
	if req.Revision != "" {
		annotations[build.AnnotationRevision] = req.Revision
	}

	name := jobName(req)
	ctr := podContainer(req, cfg)

	podSpec := podSpec{
		ServiceAccountName: cfg.ServiceAccount,
		RestartPolicy:      "Never",
		InitContainers:     sourceInitContainers(req, cfg),
		Containers:         []container{ctr},
		Volumes:            volumes(cfg),
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
		return fmt.Errorf("buildpacks: Workload needs a destination image (Request.Image)")
	}
	if c.Namespace == "" {
		return fmt.Errorf("buildpacks: Workload needs a namespace (Config.Namespace)")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("buildpacks: negative build timeout %s", time.Duration(c.Timeout))
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

// podContainer assembles the build container: the builder image running the
// lifecycle rootless, with the source mounted and a non-root security context.
func podContainer(req build.Request, cfg Config) container {
	volumeMounts := []volumeMount{
		{Name: "workspace", MountPath: build.Workspace},
		{Name: "layers", MountPath: "/layers"},
		{Name: "launch-cache", MountPath: "/launch-cache"},
		{Name: "tmp", MountPath: "/tmp"},
	}
	var env []envVar
	if cfg.PushSecret != "" {
		volumeMounts = append(volumeMounts, volumeMount{
			Name:      pushSecretVolume,
			MountPath: dockerConfigDir,
			ReadOnly:  true,
		})
		env = append(env, envVar{Name: "DOCKER_CONFIG", Value: dockerConfigDir})
	}

	ctr := container{
		Name:         buildContainerName,
		Image:        cfg.BuilderImage,
		Command:      []string{"sh", "-c", buildCommand(req, cfg)},
		Env:          env,
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
// language from the workspace, picks the matching buildpacks, builds, and
// pushes to the destination. Detection is the lifecycle's job and its choices
// surface in creator's output, which the caller streams back (ADR-0010); this
// driver only supplies the parameters.
//
// There is no cross-build cache, so every build is cold (issue #52). The
// -launch-cache below is local to the pod and dies with it — it is the
// lifecycle's own scratch space, not a cache that survives a build.
//
// Flags: the destination is <image>:<tag> (or bare <image>), and extra
// buildpacks registered via Config.Buildpacks are passed through verbatim so
// users can add buildpacks without forking the builder. Registry credentials
// for the push are projected as a file and picked up through $DOCKER_CONFIG,
// never embedded here. The exact lifecycle invocation is what the end-to-end
// harness (#86) validates against a real registry; no unit test here can prove
// it runs.
//
// The creator is not `exec`ed, unlike buildctl, because one thing has to
// happen after it: see reportDigest.
func buildCommand(req build.Request, cfg Config) string {
	var b strings.Builder
	// Not `set -o pipefail`: the builder image is Ubuntu, whose /bin/sh is
	// dash, and dash exits on `set -o pipefail` before the build even starts.
	b.WriteString("set -eu\n")

	dest := req.Image
	if req.Tag != "" {
		dest += ":" + req.Tag
	}

	b.WriteString("/cnb/lifecycle/creator")
	fmt.Fprintf(&b, " -app %s", build.ContextPath(req))
	fmt.Fprintf(&b, " -layers /layers")
	fmt.Fprintf(&b, " -launch-cache /launch-cache")
	fmt.Fprintf(&b, " -report %s", reportPath)
	fmt.Fprintf(&b, " -run-image %s", cfg.RunImage)
	fmt.Fprintf(&b, " -process-type web")
	fmt.Fprintf(&b, " -image %s", dest)
	// Extra buildpacks, in the caller's order: registration order is
	// meaningful to the lifecycle, so we preserve it rather than sort.
	for _, bp := range cfg.Buildpacks {
		fmt.Fprintf(&b, " -buildpack %s", bp)
	}
	b.WriteString("\n")
	reportDigest(&b, req)
	return b.String()
}

// reportDigest prints the pushed image as `<repository>@sha256:…` on the last
// line of the build's output.
//
// The executor recovers what a build produced by reading its log, and the
// lifecycle does not print the pushed reference in that form: it announces
// `*** Images (<id>):` in prose and states the digest as data only in
// report.toml, which nothing outside the pod can read once the Job is deleted.
// So the digest is lifted out of the report and echoed in the one shape the
// executor's parser accepts — the same `@sha256:` shape buildctl's push line
// happens to have, which is what lets one executor serve both drivers.
//
// A build whose report carries no digest fails here rather than succeeding
// with nothing to deploy: it means the creator exported somewhere other than a
// registry, and a "successful" build with no reference would be discovered at
// deploy time instead.
func reportDigest(b *strings.Builder, req build.Request) {
	fmt.Fprintf(b, "digest=$(sed -n 's/^[[:space:]]*digest[[:space:]]*=[[:space:]]*\"\\(sha256:[0-9a-f]\\{64\\}\\)\".*/\\1/p' %s | head -n 1)\n", reportPath)
	fmt.Fprintf(b, "if [ -z \"${digest}\" ]; then echo \"kelson: the lifecycle report at %s carries no image digest\" >&2; exit 1; fi\n", reportPath)
	fmt.Fprintf(b, "echo \"kelson: pushed %s@${digest}\"\n", req.Image)
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
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

type volume struct {
	Name     string          `yaml:"name"`
	EmptyDir *emptyDirVolume `yaml:"emptyDir,omitempty"`
	Secret   *secretVolume   `yaml:"secret,omitempty"`
}

type emptyDirVolume struct{}

// secretVolume projects a Secret as files. Only the name of the Secret enters
// the manifest — the value stays in the cluster (ADR-0009).
type secretVolume struct {
	SecretName  string      `yaml:"secretName"`
	DefaultMode *int32      `yaml:"defaultMode,omitempty"`
	Items       []keyToPath `yaml:"items,omitempty"`
}

type keyToPath struct {
	Key  string `yaml:"key"`
	Path string `yaml:"path"`
}

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

// volumes are what the build Job needs: the source workspace (populated by the
// clone init container), writable layers, launch-cache and tmp that the
// rootless builder requires without a privileged node, and — when the caller
// named one — the projected push credential.
func volumes(cfg Config) []volume {
	vols := []volume{
		{Name: "workspace", EmptyDir: &emptyDirVolume{}},
		{Name: "layers", EmptyDir: &emptyDirVolume{}},
		{Name: "launch-cache", EmptyDir: &emptyDirVolume{}},
		{Name: "tmp", EmptyDir: &emptyDirVolume{}},
	}
	if cfg.PushSecret != "" {
		mode := pushSecretMode
		vols = append(vols, volume{
			Name: pushSecretVolume,
			Secret: &secretVolume{
				SecretName:  cfg.PushSecret,
				DefaultMode: &mode,
				// Only the dockerconfigjson key is projected, renamed to the
				// file name the docker config loader expects. Projecting the
				// whole Secret would put whatever else it carries next to it.
				Items: []keyToPath{{Key: dockerConfigJSONKey, Path: dockerConfigFile}},
			},
		})
	}
	return vols
}

// sourceInitContainers clones the application source into the workspace before
// the lifecycle starts. The lifecycle has no clone of its own: it reads a tree
// that is already there, so without this the build detects an empty directory
// and fails with no buildpack matching, which says nothing about the cause.
//
// It returns nil when no source is configured, which is the case a caller that
// populates the workspace itself relies on.
func sourceInitContainers(req build.Request, cfg Config) []container {
	if req.SourceGit == "" {
		return nil
	}
	return []container{{
		Name:         "clone",
		Image:        cfg.GitImage,
		Command:      []string{"sh", "-c", build.CloneScript(req)},
		VolumeMounts: []volumeMount{{Name: "workspace", MountPath: build.Workspace}},
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
