package buildkit

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/build"
)

const (
	testProject = "shop"
	testEnv     = "production"
	testApp     = "checkout"
	testNS      = "shop-production"
	testImage   = "ghcr.io/acme/checkout"
)

func workloadFor(t *testing.T, req build.Request, cfg Config) *workload {
	t.Helper()
	if cfg.Namespace == "" {
		cfg.Namespace = testNS
	}
	raw, err := cfg.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	var out workload
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode rendered workload: %v\n%s", err, raw)
	}
	return &out
}

func baseRequest() build.Request {
	return build.Request{
		Project:     testProject,
		Environment: testEnv,
		Application: testApp,
		Image:       testImage,
		Tag:         "deadbeefabcd1234",
		Revision:    "deadbeefabcd1234cafe0000",
	}
}

func buildContainer(w *workload) container {
	if len(w.Spec.Template.Spec.Containers) != 1 {
		return container{}
	}
	return w.Spec.Template.Spec.Containers[0]
}

// TestWorkloadBuildIsNotPrivileged is the multi-tenancy acceptance criterion
// for #48: a build must not require a privileged container. It asserts the
// generated Job's security context is non-root and carries no privileged flag
// and no CAP_SYS_ADMIN — on the parsed spec and, defensively, on the raw bytes
// so a future renderer change cannot drift past the check.
func TestWorkloadBuildIsNotPrivileged(t *testing.T) {
	req := baseRequest()
	raw, err := Config{Namespace: testNS}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}

	// 1. On the parsed spec.
	w := workloadFor(t, req, Config{Namespace: testNS})
	sc := buildContainer(w).SecurityContext
	if sc == nil {
		t.Fatal("build container must declare a securityContext")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Fatal("build container must not be privileged")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatal("build container must set runAsNonRoot: true")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != defaultRunAsUser {
		t.Fatalf("rootless buildkit runs as uid %d, got %v", defaultRunAsUser, sc.RunAsUser)
	}
	if sc.Capabilities == nil {
		t.Fatal("build container must drop capabilities")
	}
	for _, c := range sc.Capabilities.Add {
		if c == "CAP_SYS_ADMIN" || c == "SYS_ADMIN" || c == "ALL" {
			t.Fatalf("build container must not add %q", c)
		}
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("build container must set allowPrivilegeEscalation: false")
	}

	// 2. Defensively, on the raw bytes: none of the privileged shapes may
	// appear anywhere in the rendered manifest.
	text := string(raw)
	for _, forbidden := range []string{"privileged: true", "privileged:true", "CAP_SYS_ADMIN", "SYS_ADMIN"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("rendered build manifest must not contain %q\n%s", forbidden, text)
		}
	}
	if !strings.Contains(text, "runAsNonRoot: true") {
		t.Fatalf("rendered build manifest must contain runAsNonRoot: true\n%s", text)
	}
}

// TestWorkloadUsesRootlessImage asserts the image and env pick up the rootless
// servant: the default is a *-rootless buildkit image with BUILDKIT_ROOTLESS.
func TestWorkloadUsesRootlessImage(t *testing.T) {
	w := workloadFor(t, baseRequest(), Config{Namespace: testNS})
	ctr := buildContainer(w)
	if !strings.Contains(ctr.Image, "rootless") {
		t.Fatalf("default buildkit image %q must be a rootless variant", ctr.Image)
	}
	if ctr.Image != DefaultBuildkitImage {
		t.Fatalf("image = %q, want default %q", ctr.Image, DefaultBuildkitImage)
	}
	found := false
	for _, e := range ctr.Env {
		if e.Name == "BUILDKIT_ROOTLESS" && e.Value == "true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected BUILDKIT_ROOTLESS=true, got %v", ctr.Env)
	}
}

// TestWorkloadBuildkitCommand verifies the driver leverages args, target and
// platforms through buildctl, never through a kelson DSL (ADR-0010), and that
// the rootless daemon is started with the no-process-sandbox flag.
func TestWorkloadBuildkitCommand(t *testing.T) {
	req := baseRequest()
	req.Args = map[string]string{"BUILDKIT_INLINE_CACHE": "1", "Z": "last"}
	req.Target = "runtime"
	req.Platforms = []string{"linux/amd64", "linux/arm64"}
	req.Dockerfile = "deploy/Dockerfile.prod"

	raw, err := Config{Namespace: testNS}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	cmd := buildContainer(workloadFor(t, req, Config{Namespace: testNS})).Command[2]

	for _, want := range []string{
		"--oci-worker-no-process-sandbox",
		"--frontend dockerfile.v0",
		"--local context=/workspace",
		"--opt filename=deploy/Dockerfile.prod",
		"--opt target=runtime",
		" --opt platform=linux/amd64,linux/arm64",
		"--build-arg BUILDKIT_INLINE_CACHE=1", // sorted for stable manifests
		"--build-arg Z=last",
		"--output type=image,image-format=oci,name=ghcr.io/acme/checkout:deadbeefabcd1234,push=true",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("buildctl command must contain %q\ncommand:\n%s", want, cmd)
		}
	}
	if strings.Contains(string(raw), "strategy") || strings.Contains(cmd, "dockerfile.v0:") {
		t.Error("no buildkit DSL from the kelson spec may enter the command")
	}
}

// TestWorkloadMultiPlatformBuilds verifies the platform list reaches buildctl.
func TestWorkloadMultiPlatformBuilds(t *testing.T) {
	req := baseRequest()
	req.Platforms = []string{"linux/amd64", "linux/arm64"}
	cmd := buildContainer(workloadFor(t, req, Config{Namespace: testNS})).Command[2]
	if !strings.Contains(cmd, "--opt platform=linux/amd64,linux/arm64") {
		t.Errorf("platforms must reach buildctl, got:\n%s", cmd)
	}
}

// TestWorkloadResourceLimitsAndTimeout verifies the operator knobs land in the
// manifest: resource limits/requests and the active-deadline timeout.
func TestWorkloadResourceLimitsAndTimeout(t *testing.T) {
	cfg := Config{
		Namespace: testNS,
		Resources: ResourceRequirements{
			RequestsCPU:    "250m",
			RequestsMemory: "256Mi",
			LimitsCPU:      "2",
			LimitsMemory:   "2Gi",
		},
		Timeout: Duration(30 * time.Minute),
	}
	w := workloadFor(t, baseRequest(), cfg)

	dl := w.Spec.ActiveDeadlineSeconds
	if dl == nil || *dl != 1800 {
		t.Fatalf("activeDeadlineSeconds = %v, want 1800", dl)
	}

	ctr := buildContainer(w)
	switch {
	case ctr.Resources.Requests.CPU != "250m":
		t.Errorf("requests.cpu = %q", ctr.Resources.Requests.CPU)
	case ctr.Resources.Requests.Memory != "256Mi":
		t.Errorf("requests.memory = %q", ctr.Resources.Requests.Memory)
	case ctr.Resources.Limits.CPU != "2":
		t.Errorf("limits.cpu = %q", ctr.Resources.Limits.CPU)
	case ctr.Resources.Limits.Memory != "2Gi":
		t.Errorf("limits.memory = %q", ctr.Resources.Limits.Memory)
	}
}

// TestWorkloadNoTimeoutLeavesDeadlineUnset: a zero timeout means build as long
// as it takes; there is no activeDeadlineSeconds.
func TestWorkloadNoTimeoutLeavesDeadlineUnset(t *testing.T) {
	w := workloadFor(t, baseRequest(), Config{Namespace: testNS})
	if w.Spec.ActiveDeadlineSeconds != nil {
		t.Fatalf("expected no activeDeadlineSeconds, got %v", *w.Spec.ActiveDeadlineSeconds)
	}
}

// TestWorkloadRequiresImageAndNamespace covers validation: the render step
// refuses to emit a Job with no destination image or no namespace.
func TestWorkloadRequiresImageAndNamespace(t *testing.T) {
	req := baseRequest()
	req.Image = ""
	if _, err := (Config{Namespace: testNS}).Workload(req); err == nil {
		t.Fatal("a build with no destination image must be rejected")
	}
	req = baseRequest()
	if _, err := (Config{}).Workload(req); err == nil {
		t.Fatal("a build with no namespace must be rejected")
	}
	if _, err := (Config{Namespace: testNS, Timeout: Duration(-1 * time.Second)}).Workload(baseRequest()); err == nil {
		t.Fatal("a negative timeout must be rejected")
	}
}

// TestWorkloadJobNameIsDNS1123Safe guards the name mangling: the Job name is
// a legal Kubernetes object name derived from project/application/revision.
func TestWorkloadJobNameIsDNS1123Safe(t *testing.T) {
	req := baseRequest()
	name := workloadFor(t, req, Config{Namespace: testNS}).Metadata.Name
	if !isDNS1123(name) {
		t.Fatalf("Job name %q is not DNS-1123 safe", name)
	}
	if !strings.HasPrefix(name, "build-shop-checkout-") {
		t.Fatalf("Job name %q does not derive from project/application", name)
	}
	if !strings.Contains(name, "deadbeef") {
		t.Fatalf("Job name %q should carry a short source revision", name)
	}
}

func isDNS1123(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return false
		}
		if r == '-' && (i == 0 || i == len(s)-1) {
			return false
		}
	}
	return true
}

// --- push credentials (#51) -------------------------------------------------

func findVolume(w *workload, name string) (volume, bool) {
	for _, v := range w.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return volume{}, false
}

func findMount(c container, name string) (volumeMount, bool) {
	for _, m := range c.VolumeMounts {
		if m.Name == name {
			return m, true
		}
	}
	return volumeMount{}, false
}

func envValue(c container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestWorkloadPushSecretIsProjectedForBuildctl is the push-credential
// acceptance test (#51): a named dockerconfigjson Secret is projected as
// config.json and DOCKER_CONFIG points buildctl at it. buildctl authenticates
// a push through the docker config file, not through an imagePullSecret, so
// this file mount is the mechanism rather than a convenience.
func TestWorkloadPushSecretIsProjectedForBuildctl(t *testing.T) {
	cfg := Config{Namespace: testNS, PushSecret: "ghcr-push"}
	w := workloadFor(t, baseRequest(), cfg)

	vol, ok := findVolume(w, pushSecretVolume)
	if !ok {
		t.Fatalf("expected a %q volume, got %+v", pushSecretVolume, w.Spec.Template.Spec.Volumes)
	}
	if vol.Secret == nil {
		t.Fatal("the push credential must be projected from a Secret, not an emptyDir")
	}
	if vol.EmptyDir != nil {
		t.Fatal("the push-secret volume must not also declare an emptyDir")
	}
	if vol.Secret.SecretName != "ghcr-push" {
		t.Fatalf("secretName = %q, want %q", vol.Secret.SecretName, "ghcr-push")
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != pushSecretMode {
		t.Fatalf("defaultMode = %v, want %d (0400)", vol.Secret.DefaultMode, pushSecretMode)
	}
	if len(vol.Secret.Items) != 1 {
		t.Fatalf("expected exactly the dockerconfigjson key to be projected, got %+v", vol.Secret.Items)
	}
	if got := vol.Secret.Items[0]; got.Key != dockerConfigJSONKey || got.Path != dockerConfigFile {
		t.Fatalf("projected item = %+v, want %s -> %s", got, dockerConfigJSONKey, dockerConfigFile)
	}

	ctr := buildContainer(w)
	mount, ok := findMount(ctr, pushSecretVolume)
	if !ok {
		t.Fatalf("build container must mount %q, got %+v", pushSecretVolume, ctr.VolumeMounts)
	}
	if mount.MountPath != dockerConfigDir {
		t.Fatalf("mountPath = %q, want %q", mount.MountPath, dockerConfigDir)
	}
	if !mount.ReadOnly {
		t.Fatal("the projected credential must be mounted read-only")
	}
	if got, ok := envValue(ctr, "DOCKER_CONFIG"); !ok || got != dockerConfigDir {
		t.Fatalf("DOCKER_CONFIG = %q (present %v), want %q — buildctl resolves registry auth from it", got, ok, dockerConfigDir)
	}
}

// TestWorkloadWithoutPushSecretMountsNothing: an unauthenticated push (a local
// registry) is a legitimate configuration, and it must not fabricate a Secret
// reference the cluster does not have.
func TestWorkloadWithoutPushSecretMountsNothing(t *testing.T) {
	w := workloadFor(t, baseRequest(), Config{Namespace: testNS})

	if _, ok := findVolume(w, pushSecretVolume); ok {
		t.Fatal("no push secret configured: the manifest must declare no credential volume")
	}
	for _, v := range w.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			t.Fatalf("unexpected Secret volume %q in a build with no push secret", v.Name)
		}
		if v.EmptyDir == nil {
			t.Fatalf("volume %q must still be an emptyDir", v.Name)
		}
	}
	ctr := buildContainer(w)
	if _, ok := findMount(ctr, pushSecretVolume); ok {
		t.Fatal("no push secret configured: the build container must mount none")
	}
	if got, ok := envValue(ctr, "DOCKER_CONFIG"); ok {
		t.Fatalf("DOCKER_CONFIG must be unset without a push secret, got %q", got)
	}
}

// The credential is a reference, never a value (ADR-0009): the rendered
// manifest may name the Secret and nothing more. And the credential path must
// not cost the build its rootless posture.
func TestWorkloadPushSecretStaysAReferenceAndUnprivileged(t *testing.T) {
	cfg := Config{Namespace: testNS, PushSecret: "ghcr-push"}
	raw, err := cfg.Workload(baseRequest())
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	text := string(raw)
	for _, forbidden := range []string{"password", "username", "auths", "privileged: true", "CAP_SYS_ADMIN"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("rendered manifest must not contain %q\n%s", forbidden, text)
		}
	}

	sc := buildContainer(workloadFor(t, baseRequest(), cfg)).SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatal("mounting a push credential must not change the rootless security context")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("mounting a push credential must not allow privilege escalation")
	}
}

// The clone init container has no business reading a registry credential.
func TestWorkloadPushSecretIsNotVisibleToTheCloneStep(t *testing.T) {
	req := baseRequest()
	req.SourceGit = "https://github.com/acme/shop.git"
	w := workloadFor(t, req, Config{Namespace: testNS, PushSecret: "ghcr-push"})

	if len(w.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected one clone init container, got %d", len(w.Spec.Template.Spec.InitContainers))
	}
	clone := w.Spec.Template.Spec.InitContainers[0]
	if _, ok := findMount(clone, pushSecretVolume); ok {
		t.Fatal("the clone init container must not mount the push credential")
	}
	if _, ok := envValue(clone, "DOCKER_CONFIG"); ok {
		t.Fatal("the clone init container must not receive DOCKER_CONFIG")
	}
}

// TestWorkloadAnnotatesTheDestination: the executor turns a build into a
// digest-pinned reference from these annotations, not by re-reading the
// buildctl command line — the two drivers spell their destination differently
// and one executor serves both (build.AnnotationImage).
func TestWorkloadAnnotatesTheDestination(t *testing.T) {
	w := workloadFor(t, baseRequest(), Config{Namespace: testNS})
	if got := w.Metadata.Annotations[build.AnnotationImage]; got != testImage {
		t.Errorf("%s = %q, want %q", build.AnnotationImage, got, testImage)
	}
	if got := w.Metadata.Annotations[build.AnnotationTag]; got != "deadbeefabcd1234" {
		t.Errorf("%s = %q", build.AnnotationTag, got)
	}

	req := baseRequest()
	req.Tag = ""
	if _, ok := workloadFor(t, req, Config{Namespace: testNS}).Metadata.Annotations[build.AnnotationTag]; ok {
		t.Error("an untagged build must not annotate a tag")
	}
}

// TestInsecureRegistriesMarkOnlyTheListedHosts: a plain-HTTP registry is an
// operator decision, per host. buildkitd learns it from a generated config —
// `http = true`, and deliberately not `insecure = true`, which would force
// HTTPS with no fallback (moby/buildkit#5872) — and the exporter is marked
// only when the destination itself was listed.
func TestInsecureRegistriesMarkOnlyTheListedHosts(t *testing.T) {
	req := baseRequest()
	req.Image = "localhost:5000/acme/checkout"
	cfg := Config{Namespace: testNS, InsecureRegistries: []string{"localhost:5000", "registry.internal:5000"}}
	cmd := buildContainer(workloadFor(t, req, cfg)).Command[2]

	for _, want := range []string{
		`[registry."localhost:5000"]`,
		`[registry."registry.internal:5000"]`,
		"http = true",
		"--config " + buildkitConfigPath,
		"registry.insecure=true",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("build command must contain %q\ncommand:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "insecure = true") {
		t.Errorf("`insecure = true` forces HTTPS with no fallback and must not be paired with `http = true`\n%s", cmd)
	}
}

// A destination that was not listed is not marked insecure, even when other
// registries were: the exception is per host, not a mode the build runs in.
func TestInsecureRegistriesLeaveOtherDestinationsAlone(t *testing.T) {
	cfg := Config{Namespace: testNS, InsecureRegistries: []string{"localhost:5000"}}
	cmd := buildContainer(workloadFor(t, baseRequest(), cfg)).Command[2] // pushes to ghcr.io
	if strings.Contains(cmd, "registry.insecure=true") {
		t.Errorf("ghcr.io was not listed and must not be pushed to insecurely\n%s", cmd)
	}
	if !strings.Contains(cmd, `[registry."localhost:5000"]`) {
		t.Errorf("the listed host is still configured for pulls\n%s", cmd)
	}

	plain := buildContainer(workloadFor(t, baseRequest(), Config{Namespace: testNS})).Command[2]
	for _, forbidden := range []string{"registry.insecure=true", "--config", "http = true"} {
		if strings.Contains(plain, forbidden) {
			t.Errorf("with no insecure registries configured the command must not contain %q\n%s", forbidden, plain)
		}
	}
}

// TestInsecureRegistriesAreValidated: an entry with a scheme or a repository
// path would become a buildkitd registry key matching nothing.
func TestInsecureRegistriesAreValidated(t *testing.T) {
	for _, entry := range []string{"http://localhost:5000", "localhost:5000/acme"} {
		cfg := Config{Namespace: testNS, InsecureRegistries: []string{entry}}
		if _, err := cfg.Workload(baseRequest()); err == nil {
			t.Errorf("insecure registry %q must be refused", entry)
		}
	}
}
