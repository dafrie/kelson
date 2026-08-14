package buildkit

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/model"
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

// Push-credential wiring. buildctl authenticates a registry push through the
// Docker CLI's config file, resolved from $DOCKER_CONFIG (falling back to
// ~/.docker) — it is a docker auth provider, not a Kubernetes one, so an
// imagePullSecret on the pod would do nothing for a *push*. So the
// dockerconfigjson Secret is projected as a file the build container reads:
// the Secret's .dockerconfigjson key becomes config.json in dockerConfigDir,
// and DOCKER_CONFIG points buildctl at that directory.
const (
	// dockerConfigDir sits under the rootless image's home (uid 1000), which
	// is the same home buildkit's state directory uses.
	dockerConfigDir = "/home/user/.docker"
	// dockerConfigJSONKey is the fixed key of a kubernetes.io/dockerconfigjson
	// Secret, and dockerConfigFile is what the docker config loader looks for.
	dockerConfigJSONKey = ".dockerconfigjson"
	dockerConfigFile    = "config.json"
	// pushSecretVolume names the projected credential volume.
	pushSecretVolume = "push-secret"
	// pushSecretMode is 0400: the credential is readable by the build user and
	// nobody else. Kubernetes takes the mode as a decimal int32.
	pushSecretMode int32 = 0o400
)

// Names helpers inherited from the delivery plane, mirroring renderer
// provenance (docs/architecture.md).
const (
	labelProject     = "kelson.dev/project"
	labelEnvironment = "kelson.dev/environment"
	labelApplication = "kelson.dev/application"
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
	// The destination is annotated rather than left to be read back out of the
	// buildctl command line: it is what the executor turns into
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
// an image to push to, a namespace to run in, a well-formed timeout, mountable
// secrets, and no credential smuggled in as a build argument.
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
	if err := validateSecrets(c.Secrets); err != nil {
		return err
	}
	if err := registry.ValidateInsecure(c.InsecureRegistries); err != nil {
		return err
	}
	return validateArgs(req.Args)
}

// validateArgs refuses a build argument whose name says it carries a
// credential (ADR-0009, issue #117).
//
// This is a refusal and not a redaction on purpose. A build arg is baked into
// image history and into the Job's own command line, so there is no output
// surface to clean up afterwards — by the time anything could be redacted the
// value is already in the pushed image, readable by anyone who can pull it.
// The name check is the same heuristic model.SecretShapedName applies to spec
// literals, deliberately shared so the two cannot drift.
func validateArgs(args map[string]string) error {
	names := make([]string, 0, len(args))
	for name := range args {
		if model.SecretShapedName(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return fmt.Errorf("buildkit: build argument %s looks like a credential; build args are recorded in image history and in the Job's command line, "+
		"so they can never carry a secret (ADR-0009). Mount it instead: Config.Secrets names an existing Kubernetes Secret and the Dockerfile reads it "+
		"with RUN --mount=type=secret,id=<id>", strings.Join(names, ", "))
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
	env := []envVar{{Name: "BUILDKIT_ROOTLESS", Value: "true"}}
	if cfg.PushSecret != "" {
		volumeMounts = append(volumeMounts, volumeMount{
			Name:      pushSecretVolume,
			MountPath: dockerConfigDir,
			ReadOnly:  true,
		})
		env = append(env, envVar{Name: "DOCKER_CONFIG", Value: dockerConfigDir})
	}
	volumeMounts = append(volumeMounts, secretVolumeMounts(cfg.Secrets)...)

	ctr := container{
		Name:         "buildkit",
		Image:        cfg.BuildkitImage,
		Command:      []string{"sh", "-c", buildCommand(req, cfg)},
		VolumeMounts: volumeMounts,
		Env:          env,
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
// digest. Args, secrets, target and platforms are all leveraged through
// buildctl, never through a kelson DSL (ADR-0010).
//
// This string ends up in the Job's command line, which `kubectl get job -o yaml`
// prints — so what may appear in it is exactly what may appear in public. Build
// args do (validateArgs refuses the ones that must not); secrets appear only as
// an id and the path of a projected file.
func buildCommand(req build.Request, cfg Config) string {
	const sock = "/run/user/1000/buildkit/buildkitd.sock"

	var b strings.Builder
	// Not `set -o pipefail`: this script is run by the image's /bin/sh, and
	// the shells that stand behind it are not all bash.
	b.WriteString("set -eu\n")
	daemonFlags := insecureRegistryConfig(&b, cfg.InsecureRegistries)
	fmt.Fprintf(&b, "buildkitd --oci-worker-no-process-sandbox --oci-worker-snapshotter=native%s --addr unix://%s &\n", daemonFlags, sock)
	fmt.Fprintf(&b, "until buildctl --addr unix://%s debug workers >/dev/null 2>&1; do sleep 1; done\n", sock)

	b.WriteString("exec buildctl --addr unix://")
	b.WriteString(sock)
	b.WriteString(" build --frontend dockerfile.v0")
	ctxPath := build.ContextPath(req)
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
	secretFlags(&b, cfg.Secrets)
	if len(req.Args) > 0 {
		// Sorted for deterministic output. Build args are never secrets —
		// validateArgs enforces that rather than trusting it — so sorting them
		// for stable manifests costs nothing.
		keys := make([]string, 0, len(req.Args))
		for k := range req.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, " --build-arg %s=%s", k, req.Args[k])
		}
	}
	dest := req.Image
	if req.Tag != "" {
		dest += ":" + req.Tag
	}
	fmt.Fprintf(&b, " --output type=image,image-format=oci,name=%s,push=true", dest)
	if registry.IsInsecure(cfg.InsecureRegistries, req.Image) {
		// The exporter's own knob, which covers a registry serving TLS the
		// build cannot verify. It does not by itself make the push plain HTTP
		// — the resolver decides the scheme, and that is what the buildkitd
		// config above is for — so the two are set together, and only for a
		// destination the operator listed.
		b.WriteString(",registry.insecure=true")
	}
	return b.String()
}

// buildkitConfigPath is where the generated buildkitd registry configuration
// is written. It is under the tmp emptyDir because the rootless image's
// filesystem is not writable elsewhere.
const buildkitConfigPath = "/tmp/buildkitd.toml"

// insecureRegistryConfig writes a buildkitd configuration marking exactly the
// listed registries as plain HTTP, and returns the daemon flag that loads it
// (or "" when nothing is listed).
//
// `http = true` is the key that matters, and it is deliberately not paired
// with `insecure = true` in the same stanza: buildkit's resolver forces HTTPS
// when a registry is marked insecure, with no fallback to HTTP, so writing
// both would break the plain-HTTP registry this exists to reach
// (moby/buildkit#5872). `registry.insecure=true` on the exporter, above, is
// the separate knob for a registry that speaks TLS the build cannot verify.
//
// Hosts are validated before rendering (registry.ValidateInsecure), so nothing
// user-controlled reaches the heredoc as anything but a bare host; the
// delimiter is quoted so the shell performs no expansion inside it either.
func insecureRegistryConfig(b *strings.Builder, hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	fmt.Fprintf(b, "cat > %s <<'KELSON_BUILDKITD_CONFIG'\n", buildkitConfigPath)
	for _, h := range hosts {
		fmt.Fprintf(b, "[registry.%q]\n  http = true\n", h)
	}
	b.WriteString("KELSON_BUILDKITD_CONFIG\n")
	return " --config " + buildkitConfigPath
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
// clone init container), writable buildkit state + tmp that the rootless image
// requires without a privileged node, and — when the caller named one — the
// projected push credential.
func volumes(cfg Config) []volume {
	vols := []volume{
		{Name: "workspace", EmptyDir: &emptyDirVolume{}},
		{Name: "buildkit-state", EmptyDir: &emptyDirVolume{}},
		{Name: "tmp", EmptyDir: &emptyDirVolume{}},
	}
	if cfg.PushSecret != "" {
		mode := pushSecretMode
		vols = append(vols, volume{
			Name: pushSecretVolume,
			Secret: &secretVolume{
				SecretName:  cfg.PushSecret,
				DefaultMode: &mode,
				// Only the dockerconfigjson key is projected, renamed to the file
				// name the docker config loader expects. Projecting the whole
				// Secret would put whatever else it carries next to it.
				Items: []keyToPath{{Key: dockerConfigJSONKey, Path: dockerConfigFile}},
			},
		})
	}
	return append(vols, secretVolumes(cfg.Secrets)...)
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

// The clone script, the workspace path and the context path are
// build.CloneScript, build.Workspace and build.ContextPath: both drivers
// render the same clone, so it lives in the plane rather than once per driver.
