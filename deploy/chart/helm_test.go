package chart

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests drive the real `helm` binary, because the properties worth
// pinning are properties of a render: that the chart produces valid YAML, that
// it refuses to render an unauthenticated server, and that the ClusterRole it
// actually emits — not the one in the file — is the deploy/rbac grant.
//
// Without helm on PATH they skip with a message naming the install. Skipping is
// right rather than failing: `go test ./...` must pass on a checkout that has
// never heard of Helm, and the file-level drift check in chart_test.go still
// runs there.

const chartDir = "kelson"

// helmBin finds helm on PATH or in the Go bin directory, where
// `go install helm.sh/helm/v3/cmd/helm@latest` puts it.
func helmBin(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("helm"); err == nil {
		return path
	}
	for _, dir := range goBinDirs() {
		candidate := filepath.Join(dir, "helm")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("helm not found on PATH or in the Go bin directory; install it with " +
		"`go install helm.sh/helm/v3/cmd/helm@latest` to run the chart render tests")
	return ""
}

func goBinDirs() []string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		for _, p := range filepath.SplitList(gopath) {
			dirs = append(dirs, filepath.Join(p, "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}

// authValues is the smallest set that renders: a tag, because the chart never
// picks one, and a password source, because the chart never assumes one.
var authValues = []string{
	"--set", "image.tag=v0.1.0",
	"--set", "auth.existingSecret.name=kelson-auth",
}

func helmRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(helmBin(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

func helmTemplate(t *testing.T, extra ...string) string {
	t.Helper()
	args := append([]string{"template", "kelson", chartDir, "--namespace", "kelson-system"}, extra...)
	out, err := helmRun(t, args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// TestHelmLint is the cheapest statement that the chart is a chart.
func TestHelmLint(t *testing.T) {
	args := append([]string{"lint", chartDir}, authValues...)
	if out, err := helmRun(t, args...); err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
}

// TestTemplateRendersValidYAML renders the documented install and checks every
// document parses and carries the uninstall label. The label is the #59 claim
// in mechanical form: `kubectl delete … -l kelson.dev/install=<release>` is only
// a complete uninstall if nothing the chart creates is missing it.
func TestTemplateRendersValidYAML(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, authValues...))
	if len(docs) < 6 {
		t.Fatalf("rendered %d documents, expected the full install (namespace-scoped RBAC, SA, Deployment, Service, ClusterRole+Binding)", len(docs))
	}
	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		name, _ := meta["name"].(string)
		if labels["kelson.dev/install"] != "kelson" {
			t.Errorf("%s/%s has no kelson.dev/install label — it would survive an uninstall by selector (issue #59)", kind, name)
		}
		if labels["app.kubernetes.io/managed-by"] != "Helm" {
			t.Errorf("%s/%s is not labelled app.kubernetes.io/managed-by: Helm", kind, name)
		}
	}
}

// TestTemplateServerFlags pins the flags the values surface produces, including
// the one that is not a value: in-cluster the listen address is always
// 0.0.0.0, because a Service cannot reach loopback.
func TestTemplateServerFlags(t *testing.T) {
	out := helmTemplate(t, append(authValues,
		"--set", "server.registry=ghcr.io/acme",
		"--set", "server.pushSecret=ghcr-push",
		"--set", "server.buildNamespace=kelson-builds",
		"--set", "server.keep=5",
		"--set", "server.insecureRegistries={localhost:5000,registry.internal:5000}",
	)...)
	container := serverContainer(t, decodeDocs(t, out))

	args := stringsOf(container["args"])
	for _, want := range []string{
		"--listen=0.0.0.0:8420",
		"--namespace=kelson-system",
		"--keep=5",
		"--registry=ghcr.io/acme",
		"--push-secret=ghcr-push",
		"--build-namespace=kelson-builds",
		// A list value, one flag: the server splits on commas, and a host that
		// was not listed is never reached over plain HTTP.
		"--insecure-registries=localhost:5000,registry.internal:5000",
	} {
		if !contains(args, want) {
			t.Errorf("rendered args %v are missing %q", args, want)
		}
	}
	if contains(args, "--insecure-bind") {
		t.Error("--insecure-bind was rendered for a release that has a password")
	}
	if !strings.Contains(out, "KELSON_PASSWORD") {
		t.Error("KELSON_PASSWORD is not in the pod spec, so the server would start unauthenticated")
	}
	if strings.Contains(out, "--password") {
		t.Error("the password is passed as a flag: it would be visible in the container's command line")
	}
}

// TestAuthGateRefusesWithoutPassword is the posture check. Serving 0.0.0.0 with
// no authentication has to be a decision somebody made, not a default somebody
// inherited (cmd/kelson-server's checkBind, ADR-0013 §3).
func TestAuthGateRefusesWithoutPassword(t *testing.T) {
	out, err := helmRun(t, "template", "kelson", chartDir, "--namespace", "kelson-system", "--set", "image.tag=v0.1.0")
	if err == nil {
		t.Fatalf("helm template succeeded with no auth values; the chart must refuse:\n%s", out)
	}
	for _, want := range []string{"no password configured", "auth.existingSecret.name", "auth.insecure=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q — it has to say how to get out of it:\n%s", want, out)
		}
	}
}

// TestAuthGateRefusesAmbiguousPasswords: two password sources is a question the
// chart cannot answer, so it asks rather than picking.
func TestAuthGateRefusesAmbiguousPasswords(t *testing.T) {
	out, err := helmRun(t, "template", "kelson", chartDir, "--namespace", "kelson-system",
		"--set", "image.tag=v0.1.0", "--set", "auth.password=hunter2", "--set", "auth.existingSecret.name=kelson-auth")
	if err == nil {
		t.Fatalf("helm template accepted both auth.password and auth.existingSecret:\n%s", out)
	}
	if !strings.Contains(out, "Pick one") {
		t.Errorf("the refusal does not explain the ambiguity:\n%s", out)
	}
}

// TestInsecureRendersInsecureBind: the escape hatch works, and it is loud. The
// server would refuse a non-loopback bind without the flag, so a chart that
// allowed auth.insecure without rendering it would produce a CrashLoopBackOff.
func TestInsecureRendersInsecureBind(t *testing.T) {
	out := helmTemplate(t, "--set", "image.tag=v0.1.0", "--set", "auth.insecure=true")
	container := serverContainer(t, decodeDocs(t, out))
	if !contains(stringsOf(container["args"]), "--insecure-bind") {
		t.Errorf("auth.insecure did not render --insecure-bind; the server would refuse to start:\n%v", container["args"])
	}
	if strings.Contains(out, "KELSON_PASSWORD") {
		t.Error("an unauthenticated release still references a password secret")
	}
}

// TestPasswordLiteralCreatesSecret covers the documented-lesser path: the value
// still reaches the pod as a secretKeyRef, never as a literal env value.
func TestPasswordLiteralCreatesSecret(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, "--set", "image.tag=v0.1.0", "--set", "auth.password=hunter2"))
	var found bool
	for _, doc := range docs {
		if doc["kind"] == "Secret" {
			found = true
		}
	}
	if !found {
		t.Fatal("auth.password did not produce a Secret")
	}
	container := serverContainer(t, docs)
	for _, e := range anySlice(container["env"]) {
		env, _ := e.(map[string]any)
		if env["name"] == "KELSON_PASSWORD" {
			if _, ok := env["value"]; ok {
				t.Error("KELSON_PASSWORD is a literal in the pod spec; it must be a secretKeyRef")
			}
			return
		}
	}
	t.Error("KELSON_PASSWORD is missing from the pod spec")
}

// TestImageTagIsRequired pins the other refusal: no default tag, ever.
func TestImageTagIsRequired(t *testing.T) {
	out, err := helmRun(t, "template", "kelson", chartDir, "--namespace", "kelson-system",
		"--set", "auth.existingSecret.name=kelson-auth")
	if err == nil {
		t.Fatalf("helm template succeeded with no image.tag:\n%s", out)
	}
	if !strings.Contains(out, "image.tag is required") {
		t.Errorf("the refusal does not name image.tag:\n%s", out)
	}
}

// TestRenderedDetectRoleMatchesDeployRBAC repeats the drift check against what
// helm actually emits, so a templating change that alters the rules on the way
// out is caught as well as a file edit.
func TestRenderedDetectRoleMatchesDeployRBAC(t *testing.T) {
	var got []policyRule
	for _, doc := range decodeDocs(t, helmTemplate(t, authValues...)) {
		if doc["kind"] != "ClusterRole" {
			continue
		}
		remarshal(t, doc["rules"], &got)
	}
	if got == nil {
		t.Fatal("no ClusterRole was rendered")
	}
	want := readClusterRole(t, deployRBACPath).Rules
	if !reflect.DeepEqual(want, got) {
		t.Errorf("the rendered ClusterRole differs from %s:\nwant:\n%s\ngot:\n%s",
			deployRBACPath, mustYAML(t, want), mustYAML(t, got))
	}
}

// TestTargetNamespacesGetTheirOwnRole covers how docs/server.md says to bound
// the Secret grant today: name the namespaces the server may serve.
func TestTargetNamespacesGetTheirOwnRole(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "rbac.targetNamespaces={apps-prod,apps-staging}")...))
	seen := map[string]bool{}
	for _, doc := range docs {
		if doc["kind"] != "Role" {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		ns, _ := meta["namespace"].(string)
		seen[ns] = true
	}
	for _, ns := range []string{"kelson-system", "apps-prod", "apps-staging"} {
		if !seen[ns] {
			t.Errorf("no Role was rendered in %q; the server could not serve it", ns)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func decodeDocs(t *testing.T, out string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(out))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the rendered output is not valid YAML: %v\n%s", err, out)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

func serverContainer(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers := anySlice(podSpec["containers"])
		if len(containers) != 1 {
			t.Fatalf("expected one container in the Deployment, got %d", len(containers))
		}
		c, _ := containers[0].(map[string]any)
		return c
	}
	t.Fatal("no Deployment was rendered")
	return nil
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func stringsOf(v any) []string {
	items := anySlice(v)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func remarshal(t *testing.T, from any, into any) {
	t.Helper()
	data, err := yaml.Marshal(from)
	if err != nil {
		t.Fatalf("re-marshalling the rendered rules: %v", err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("re-parsing the rendered rules: %v", err)
	}
}
