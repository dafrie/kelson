package buildpacks

import (
	"context"
	"io"
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

// languageSignals mirrors detect's probe surface for the four acceptance
// languages of issue #49. The lifecycle does the actual detection from the
// (unmounted, cluster-populated) workspace; this driver's render must be
// correct and non-privileged for each, identical in shape because detection is
// the lifecycle's job, not a per-language renderer's.
var acceptanceLanguages = []struct {
	name   string
	signal string
}{
	{name: "node", signal: "package.json"},
	{name: "python", signal: "requirements.txt"},
	{name: "go", signal: "go.mod"},
	{name: "ruby", signal: "Gemfile"},
}

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

// TestWorkloadLifecycleIsNotPrivileged is the multi-tenancy acceptance
// criterion for #49 (mirroring buildkit's TestWorkloadBuildIsNotPrivileged):
// a buildpacks build must not require a privileged container. It asserts the
// generated Job's security context is non-root and carries no privileged flag
// and no CAP_SYS_ADMIN — on the parsed spec and, defensively, on the raw bytes
// so a future renderer change cannot drift past the check.
func TestWorkloadLifecycleIsNotPrivileged(t *testing.T) {
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
		t.Fatalf("rootless lifecycle runs as uid %d, got %v", defaultRunAsUser, sc.RunAsUser)
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

// TestWorkloadCoversAcceptanceLanguages renders the workload for each of the
// four acceptance languages of issue #49 (a Node, Python, Go or Ruby repo with
// no Dockerfile) and asserts the render is correct — strategy label, builder,
// run image, destination — and non-privileged.
//
// This can only prove the rendered workload is correct and non-privileged, not
// that it builds and runs; the language detection itself is the lifecycle's
// job and the end-to-end "builds and runs" half of the criterion belongs to
// the e2e harness (#86).
func TestWorkloadCoversAcceptanceLanguages(t *testing.T) {
	for _, lang := range acceptanceLanguages {
		req := baseRequest()
		req.Application = lang.name
		req.Image = "ghcr.io/acme/" + lang.name
		req.Tag = ""
		req.Revision = "deadbeef"

		w := workloadFor(t, req, Config{Namespace: testNS})
		ctr := buildContainer(w)

		if got := w.Metadata.Labels[labelStrategy]; got != StrategyName {
			t.Errorf("%s: strategy label = %q, want %q", lang.name, got, StrategyName)
		}
		if ctr.Image != DefaultBuilder {
			t.Errorf("%s: image = %q, want default builder %q", lang.name, ctr.Image, DefaultBuilder)
		}
		cmd := ctr.Command[2]
		if !strings.Contains(cmd, "-run-image "+DefaultRunImage) {
			t.Errorf("%s: command must carry default run image %q\n%s", lang.name, DefaultRunImage, cmd)
		}
		if !strings.Contains(cmd, "-image ghcr.io/acme/"+lang.name) {
			t.Errorf("%s: command must carry the per-app destination\n%s", lang.name, cmd)
		}

		sc := ctr.SecurityContext
		if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s: workload must be non-root", lang.name)
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.Errorf("%s: workload must not be privileged", lang.name)
		}
	}
}

// TestWorkloadCommand carries the build parameters: builder, run image,
// destination, and extra registered buildpacks — all of
// it driven from Config, never from the kelson spec or a buildpack DSL
// (ADR-0010).
func TestWorkloadCommand(t *testing.T) {
	cfg := Config{
		Namespace:  testNS,
		RunImage:   "ghcr.io/corp/run-jammy",
		Buildpacks: []string{"urn:cnb:builder:paketo-community/rust", "urn:cnb:builder:example/extra"},
	}
	cmd := buildContainer(workloadFor(t, baseRequest(), cfg)).Command[2]

	for _, want := range []string{
		"exec /cnb/lifecycle/creator",
		"-app /workspace",
		"-run-image ghcr.io/corp/run-jammy",
		"-process-type web",
		"-image " + testImage + ":deadbeefabcd1234",
		"-buildpack urn:cnb:builder:paketo-community/rust",
		"-buildpack urn:cnb:builder:example/extra",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("creator command must contain %q\ncommand:\n%s", want, cmd)
		}
	}
}

// TestCustomBuildpacksRegistration verifies users add buildpacks through
// Config.Buildpacks without forking the builder, preserved in registration
// order (order is semantically meaningful to the lifecycle).
func TestCustomBuildpacksRegistration(t *testing.T) {
	cfg := Config{Namespace: testNS}
	cfg.Buildpacks = []string{"urn:cnb:builder:b", "urn:cnb:builder:a"}
	cmd := buildContainer(workloadFor(t, baseRequest(), cfg)).Command[2]

	if !strings.Contains(cmd, "-buildpack urn:cnb:builder:b") {
		t.Errorf("first registered buildpack missing\n%s", cmd)
	}
	if strings.Index(cmd, "urn:cnb:builder:b") > strings.Index(cmd, "urn:cnb:builder:a") {
		t.Errorf("buildpack registration order must be preserved\n%s", cmd)
	}
	if !strings.Contains(cmd, "-buildpack urn:cnb:builder:a") {
		t.Errorf("second registered buildpack missing\n%s", cmd)
	}
}

// TestWorkloadValidation covers the render-step invariants: it refuses to emit
// a Job with no destination image, no namespace, or a negative timeout.
func TestWorkloadValidation(t *testing.T) {
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
	name := workloadFor(t, baseRequest(), Config{Namespace: testNS}).Metadata.Name
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

// --- driver-level tests with fakes ---------------------------------------

type fakeCluster struct {
	submitted [][]byte
	result    build.Result
}

func (f *fakeCluster) Submit(_ context.Context, manifest []byte) (string, error) {
	f.submitted = append(f.submitted, manifest)
	return "build-shop-checkout-deadbeef", nil
}

func (f *fakeCluster) Wait(_ context.Context, _ string, _ io.Writer) (build.Result, error) {
	return f.result, nil
}

type fakeRebaser struct{ newRef string }

func (f *fakeRebaser) Rebase(_ context.Context, ref, newRunImage string) (string, error) {
	return f.newRef, nil
}

// TestDriverNameAndBuild verifies the driver implements build.Builder with
// name "buildpacks" and that Build submits the rendered manifest and returns
// the cluster's result.
func TestDriverNameAndBuild(t *testing.T) {
	cluster := &fakeCluster{result: build.Result{
		Reference: "ghcr.io/acme/checkout@sha256:" + strings.Repeat("ef", 32),
		Digest:    "sha256:" + strings.Repeat("ef", 32),
		Tag:       "shop-checkout-deadbeef",
	}}
	d, err := New(Options{Cluster: cluster, Config: Config{Namespace: testNS}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Name() != StrategyName {
		t.Fatalf("Name() = %q, want %q", d.Name(), StrategyName)
	}
	if d.Name() != "buildpacks" {
		t.Fatalf("Name() must be the strategy id %q", StrategyName)
	}

	res, err := d.Build(context.Background(), baseRequest(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Reference != cluster.result.Reference {
		t.Fatalf("Build result = %q, want %q", res.Reference, cluster.result.Reference)
	}
	if len(cluster.submitted) != 1 {
		t.Fatalf("cluster must receive exactly one manifest, got %d", len(cluster.submitted))
	}
	if !strings.Contains(string(cluster.submitted[0]), "runAsNonRoot: true") {
		t.Fatal("submitted manifest must be non-root")
	}
}

// TestNewRequiresCluster: the driver fails closed without a Cluster.
func TestNewRequiresCluster(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New must reject a missing Cluster")
	}
}

// TestRebaseProducesExpectedReference exercises the real Rebase API: given a
// digest-pinned built reference and a digest-pinned new run image, it asks the
// injected Rebaser and returns its new reference — a run-image patch across a
// built application without a source rebuild (issue #49).
func TestRebaseProducesExpectedReference(t *testing.T) {
	var (
		built   = "ghcr.io/acme/checkout@sha256:" + strings.Repeat("11", 32)
		newRun  = "ghcr.io/rel/run-jammy@sha256:" + strings.Repeat("22", 32)
		rebased = "ghcr.io/acme/checkout@sha256:" + strings.Repeat("33", 32)
	)
	rb := &fakeRebaser{newRef: rebased}
	d, err := New(Options{
		Cluster: &fakeCluster{},
		Config:  Config{Namespace: testNS},
		Rebaser: rb,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := d.Rebase(context.Background(), built, newRun)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if got != rebased {
		t.Fatalf("Rebase = %q, want %q", got, rebased)
	}
}

// TestRebaseRequiresPinnedReferences: rebase must fail closed when either the
// built reference or the new run image is a mutable tag, so it can never
// silently retarget a non-reproducible image.
func TestRebaseRequiresPinnedReferences(t *testing.T) {
	d, err := New(Options{Cluster: &fakeCluster{}, Config: Config{Namespace: testNS}, Rebaser: &fakeRebaser{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run := "ghcr.io/rel/run-jammy@sha256:" + strings.Repeat("22", 32)

	// Mutable (tag only) built ref.
	if _, err := d.Rebase(context.Background(), "ghcr.io/acme/checkout:latest", run); err == nil {
		t.Fatal("Rebase must reject a mutable built reference")
	}
	// Empty built ref.
	if _, err := d.Rebase(context.Background(), "", run); err == nil {
		t.Fatal("Rebase must reject an empty built reference")
	}
	// Mutable new run image.
	if _, err := d.Rebase(context.Background(), "ghcr.io/acme/checkout@sha256:"+strings.Repeat("11", 32), "ghcr.io/rel/run-jammy:latest"); err == nil {
		t.Fatal("Rebase must reject a mutable new run image")
	}
}

// TestRebaseRequiresRebaser: without an injected Rebaser the operation fails
// closed rather than silently doing nothing.
func TestRebaseRequiresRebaser(t *testing.T) {
	d, err := New(Options{Cluster: &fakeCluster{}, Config: Config{Namespace: testNS}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = d.Rebase(context.Background(),
		"ghcr.io/acme/checkout@sha256:"+strings.Repeat("11", 32),
		"ghcr.io/rel/run-jammy@sha256:"+strings.Repeat("22", 32))
	if err == nil {
		t.Fatal("Rebase must fail closed without an injected Rebaser")
	}
}
