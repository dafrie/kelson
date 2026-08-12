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
