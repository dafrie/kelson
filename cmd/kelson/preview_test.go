package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/preview"
)

// `kelson preview` is assembly (issue #11, ADR-0017 stage 2): load the spec,
// render for the change request, package, and hand one artifact to the pusher.
// The render is tested in internal/preview and the registry conversation in
// internal/preview/push_test.go against a fake registry, so these tests drive
// the command through its seams and assert the wiring: what reaches the pusher,
// what the render rung does instead, what prints, and what is refused.

// --- fakes ------------------------------------------------------------------

type fakePusher struct {
	mu        sync.Mutex
	artifacts []preview.Artifact
	creds     []registry.Credential
	insecure  bool
	err       error
}

func (f *fakePusher) Push(_ context.Context, a preview.Artifact) (string, error) {
	f.mu.Lock()
	f.artifacts = append(f.artifacts, a)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return a.Reference(), nil
}

func (f *fakePusher) connector() pusherConnector {
	return func(cred registry.Credential, insecure bool) artifactPusher {
		f.mu.Lock()
		f.creds = append(f.creds, cred)
		f.insecure = insecure
		f.mu.Unlock()
		return f
	}
}

func (f *fakePusher) pushed(t *testing.T) preview.Artifact {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.artifacts) == 0 {
		t.Fatal("nothing was ever pushed")
	}
	return f.artifacts[len(f.artifacts)-1]
}

func (f *fakePusher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.artifacts)
}

// fakeSecretResolver stands in for the cluster read behind --registry-secret.
type fakeSecretResolver struct {
	mu   sync.Mutex
	refs []registry.SecretRef
	cred registry.Credential
	err  error
}

func (f *fakeSecretResolver) Resolve(_ context.Context, ref registry.SecretRef) (registry.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref)
	return f.cred, f.err
}

func (f *fakeSecretResolver) lastRef(t *testing.T) registry.SecretRef {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refs) == 0 {
		t.Fatal("the resolver was never called")
	}
	return f.refs[len(f.refs)-1]
}

// --- harness ----------------------------------------------------------------

const (
	testHeadSHA        = "0123456789abcdef0123456789abcdef01234567"
	testPreviewArchive = "ghcr.io/acme/shop-previews"
)

func rootWithPreviewPlane(t *testing.T, push pusherConnector, resolver registry.Resolver) *cobra.Command {
	t.Helper()
	// Neither rung may read the developer's own docker login, so the default
	// credential source is pointed at an empty directory for the whole test.
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "preview" {
			root.RemoveCommand(c)
		}
	}
	connect := func(string) (registry.Resolver, error) { return resolver, nil }
	if resolver == nil {
		connect = nil
	}
	root.AddCommand(newPreviewCmdFactory(connect, push))
	return root
}

func runPreviewCmd(t *testing.T, push pusherConnector, resolver registry.Resolver, args ...string) (stdout, stderr string, code int, msg string) {
	t.Helper()
	cmd := rootWithPreviewPlane(t, push, resolver)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), errBuf.String(), code, msg
}

type previewSpecOptions struct {
	noPreviews bool
	repository string
}

func writePreviewSpec(t *testing.T, opts previewSpecOptions) string {
	t.Helper()
	if opts.repository == "" {
		opts.repository = "oci://" + testPreviewArchive
	}
	var b strings.Builder
	b.WriteString("apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata:\n  name: shop\nspec:\n")
	b.WriteString("  image: ghcr.io/acme/shop:1.0.0\n")
	b.WriteString("  components:\n    - name: web\n      port: 8080\n")
	b.WriteString("---\napiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: staging\nspec:\n")
	b.WriteString("  project: shop\n  namespace: shop-staging\n")
	if !opts.noPreviews {
		b.WriteString("  previews:\n    provider: github\n    repo: https://github.com/acme/shop\n")
		b.WriteString("    secretRef: github-auth\n    artifacts:\n      repository: " + opts.repository + "\n")
	}
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func publishArgs(spec string, extra ...string) []string {
	return append([]string{
		"preview", "publish", "-f", spec, "--env", "staging",
		"--pr", "412", "--sha", testHeadSHA,
	}, extra...)
}

// --- publish ----------------------------------------------------------------

// TestPublishHandsTheRenderedArtifactToThePusher is the wiring in one
// assertion: what leaves this command is the change request's manifests,
// addressed to the spec's repository under the head commit.
func TestPublishHandsTheRenderedArtifactToThePusher(t *testing.T) {
	push := &fakePusher{}
	spec := writePreviewSpec(t, previewSpecOptions{})
	stdout, stderr, code, msg := runPreviewCmd(t, push.connector(), nil, publishArgs(spec)...)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, msg, stderr)
	}

	a := push.pushed(t)
	if a.Repository != testPreviewArchive {
		t.Errorf("pushed to %q, want %q", a.Repository, testPreviewArchive)
	}
	if a.Tag != testHeadSHA {
		t.Errorf("tagged %q, want the head commit %s", a.Tag, testHeadSHA)
	}
	if len(a.Files) == 0 {
		t.Error("the artifact is empty")
	}

	// The last line of stdout is the pinned reference and nothing else, so the
	// command composes the way `kelson build` does.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if got := lines[len(lines)-1]; got != a.Reference() {
		t.Errorf("stdout ends with %q, want the digest-pinned reference %q", got, a.Reference())
	}
	// The plan is context for a human and belongs on stderr.
	if !strings.Contains(stderr, "shop-staging-pr412") {
		t.Errorf("the plan does not name the preview namespace:\n%s", stderr)
	}
}

// TestPublishResolvesTheRegistrySecretInTheParentNamespace: the preview's own
// namespace does not exist yet — creating it is what the artifact is for — so
// the Secret is looked for where the environment already runs.
func TestPublishResolvesTheRegistrySecretInTheParentNamespace(t *testing.T) {
	push := &fakePusher{}
	resolver := &fakeSecretResolver{cred: registry.Credential{Username: "robot", Password: "s3cret"}}
	spec := writePreviewSpec(t, previewSpecOptions{})

	_, stderr, code, msg := runPreviewCmd(t, push.connector(), resolver,
		publishArgs(spec, "--registry-secret", "ghcr-push")...)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, msg, stderr)
	}

	ref := resolver.lastRef(t)
	if ref.Name != "ghcr-push" {
		t.Errorf("secret name = %q", ref.Name)
	}
	if ref.Namespace != "shop-staging" {
		t.Errorf("secret namespace = %q, want the environment's own namespace", ref.Namespace)
	}
	if ref.Registry != "ghcr.io" {
		t.Errorf("secret registry = %q, want the artifact repository's host", ref.Registry)
	}

	push.mu.Lock()
	defer push.mu.Unlock()
	if len(push.creds) == 0 || push.creds[0].Username != "robot" {
		t.Errorf("the resolved credential did not reach the pusher: %v", push.creds)
	}
	// A plan line that printed the password would put it in a CI log.
	if strings.Contains(stderr, "s3cret") {
		t.Errorf("the plan echoes the credential:\n%s", stderr)
	}
}

func TestPublishReportsAFailedPush(t *testing.T) {
	push := &fakePusher{err: errors.New("registry said no")}
	spec := writePreviewSpec(t, previewSpecOptions{})
	_, _, code, msg := runPreviewCmd(t, push.connector(), nil, publishArgs(spec)...)
	if code != exitErr {
		t.Fatalf("exit %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "registry said no") {
		t.Errorf("the failure is not reported: %s", msg)
	}
}

// TestPublishRefusals: each refusal names its reason and is an ordinary
// exit-1 error, and none of them reaches the pusher.
func TestPublishRefusals(t *testing.T) {
	cases := []struct {
		name string
		spec previewSpecOptions
		args []string
		want string
	}{
		{
			name: "environment without previews",
			spec: previewSpecOptions{noPreviews: true},
			want: "spec.previews",
		},
		{
			name: "malformed change request",
			args: []string{"--pr", "abc"},
			want: preview.ReasonInvalidChangeRequest,
		},
		{
			name: "abbreviated commit",
			args: []string{"--sha", testHeadSHA[:8]},
			want: preview.ReasonInvalidSHA,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			push := &fakePusher{}
			spec := writePreviewSpec(t, tc.spec)
			// The overriding flag has to come after the defaults, which is what
			// publishArgs' variadic tail is for.
			_, _, code, msg := runPreviewCmd(t, push.connector(), nil, publishArgs(spec, tc.args...)...)
			if code != exitErr {
				t.Fatalf("exit %d, want %d (%s)", code, exitErr, msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("the refusal does not mention %q: %s", tc.want, msg)
			}
			if push.calls() != 0 {
				t.Error("a refused publish still pushed something")
			}
		})
	}
}

// --- render -----------------------------------------------------------------

// TestPreviewRenderWritesWithoutPushing is the dry rung: the same manifests,
// no artifact, no credential, no registry.
func TestPreviewRenderWritesWithoutPushing(t *testing.T) {
	push := &fakePusher{}
	spec := writePreviewSpec(t, previewSpecOptions{})
	stdout, stderr, code, msg := runPreviewCmd(t, push.connector(), nil,
		"preview", "render", "-f", spec, "--env", "staging", "--pr", "412", "--sha", testHeadSHA)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if push.calls() != 0 {
		t.Error("the render rung pushed an artifact")
	}
	if !strings.Contains(stdout, "namespace: shop-staging-pr412") {
		t.Errorf("the manifests are not rendered for the preview namespace:\n%s", stdout)
	}
	if strings.Contains(stdout, "kind: ResourceSet") {
		t.Error("the preview render carries the parent's preview lifecycle")
	}
	// Where it would go is invisible in the manifests, so it is printed beside
	// them.
	if !strings.Contains(stderr, testPreviewArchive+":"+testHeadSHA) {
		t.Errorf("the destination is not reported:\n%s", stderr)
	}
}

func TestPreviewRenderWritesADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	spec := writePreviewSpec(t, previewSpecOptions{})
	_, _, code, msg := runPreviewCmd(t, (&fakePusher{}).connector(), nil,
		"preview", "render", "-f", spec, "--env", "staging", "--pr", "412", "--sha", testHeadSHA, "-o", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the render rung wrote no files")
	}
}

// TestPreviewAcceptsEitherEnvironmentSpelling: a CI workflow reaches for
// --environment and every other kelson command spells it --env.
func TestPreviewAcceptsEitherEnvironmentSpelling(t *testing.T) {
	spec := writePreviewSpec(t, previewSpecOptions{})
	_, _, code, msg := runPreviewCmd(t, (&fakePusher{}).connector(), nil,
		"preview", "render", "-f", spec, "--environment", "staging", "--pr", "412", "--sha", testHeadSHA)
	if code != 0 {
		t.Fatalf("--environment was not accepted: exit %d: %s", code, msg)
	}
}
