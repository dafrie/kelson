// Package chart holds the tests that keep deploy/chart/kelson honest.
//
// There is no Go code in the chart and there never will be — it is Helm
// templates. The tests live here because two of the chart's claims are only
// true as long as something checks them on every commit (issue #58):
//
//   - the detect ClusterRole the chart ships is the same grant as
//     deploy/rbac/detect-clusterrole.yaml, and
//   - the chart refuses to render a server that would serve an unauthenticated
//     API to the cluster.
//
// The first is a plain file comparison and always runs. The second needs the
// `helm` binary and skips without it (helm_test.go).
package chart

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/version"
)

// policyRule is the subset of rbac/v1 PolicyRule the two manifests use. It is
// declared here rather than imported from k8s.io/api because this package sits
// under the `main` depguard rule and does not need a Kubernetes client to
// compare two YAML files.
type policyRule struct {
	APIGroups       []string `yaml:"apiGroups"`
	Resources       []string `yaml:"resources"`
	ResourceNames   []string `yaml:"resourceNames"`
	NonResourceURLs []string `yaml:"nonResourceURLs"`
	Verbs           []string `yaml:"verbs"`
}

type clusterRole struct {
	Kind  string       `yaml:"kind"`
	Rules []policyRule `yaml:"rules"`
}

const (
	deployRBACPath  = "../rbac/detect-clusterrole.yaml"
	chartRBACPath   = "kelson/templates/clusterrole-detect.yaml"
	chartYAMLPath   = "kelson/Chart.yaml"
	chartValuesPath = "kelson/values.yaml"
)

// TestChartDetectRoleMatchesDeployRBAC is the anti-rot check the chart's own
// comment promises. deploy/rbac/detect-clusterrole.yaml is what the CLI docs
// point at and what internal/clusterprofile/detect's tests police; the chart
// carries a copy so a Helm install is a complete install. A copy nobody
// compares is a copy that drifts, and the drift would be silent — detection
// would just start reporting gaps where it used to report facts.
func TestChartDetectRoleMatchesDeployRBAC(t *testing.T) {
	want := readClusterRole(t, deployRBACPath)
	got := readTemplatedClusterRole(t, chartRBACPath)

	if len(want.Rules) == 0 {
		t.Fatalf("%s has no rules — the comparison would pass vacuously", deployRBACPath)
	}
	if !reflect.DeepEqual(want.Rules, got.Rules) {
		t.Errorf("the chart's detect ClusterRole has drifted from %s\n\n%s:\n%s\n%s:\n%s\n\n"+
			"These must stay identical: a `kubectl apply -f deploy/rbac` install and a `helm install` "+
			"have to grant the probe the same reads.",
			deployRBACPath, deployRBACPath, mustYAML(t, want.Rules), chartRBACPath, mustYAML(t, got.Rules))
	}
}

// TestChartDetectRoleIsReadOnly repeats detect's read-only contract against the
// chart's copy directly, so a chart edited without touching deploy/rbac fails
// on the substance and not only on the diff.
func TestChartDetectRoleIsReadOnly(t *testing.T) {
	role := readTemplatedClusterRole(t, chartRBACPath)
	allowed := map[string]bool{"get": true, "list": true, "watch": true}
	for i, r := range role.Rules {
		for _, v := range r.Verbs {
			if !allowed[v] {
				t.Errorf("rule %d grants %q — the detect ClusterRole is read-only by contract (issue #56)", i, v)
			}
		}
	}
}

// TestChartAppVersionTracksVersionPackage keeps Chart.yaml's appVersion equal
// to the version package's default. They describe the same thing — which
// kelson build this is — and a chart whose appVersion lags is a chart whose
// app.kubernetes.io/version label lies about what is running.
func TestChartAppVersionTracksVersionPackage(t *testing.T) {
	var chart struct {
		APIVersion string `yaml:"apiVersion"`
		Name       string `yaml:"name"`
		Version    string `yaml:"version"`
		AppVersion string `yaml:"appVersion"`
	}
	unmarshalFile(t, chartYAMLPath, &chart)

	if chart.APIVersion != "v2" {
		t.Errorf("Chart.yaml apiVersion = %q, want v2", chart.APIVersion)
	}
	if chart.Name != "kelson" {
		t.Errorf("Chart.yaml name = %q, want kelson", chart.Name)
	}
	if chart.AppVersion != version.Version {
		t.Errorf("Chart.yaml appVersion = %q but internal/version.Version = %q.\n"+
			"They name the same build. A release that bumps one bumps the other "+
			"(docs/release-policy.md).", chart.AppVersion, version.Version)
	}
}

// TestValuesLatchNoImageTag pins the deliberate absence of a default image tag.
// `latest` would make a Deployment's identity unknowable, and the appVersion is
// a development placeholder rather than a published image, so the chart asks
// for the tag instead of guessing (values.yaml, and the helm test proves the
// render actually fails without one).
func TestValuesLatchNoImageTag(t *testing.T) {
	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
			Tag        string `yaml:"tag"`
		} `yaml:"image"`
		Auth struct {
			Password string `yaml:"password"`
			Insecure bool   `yaml:"insecure"`
		} `yaml:"auth"`
	}
	unmarshalFile(t, chartValuesPath, &values)

	if values.Image.Tag != "" {
		t.Errorf("values.yaml image.tag = %q, want empty: the chart must not latch a tag", values.Image.Tag)
	}
	if values.Image.Repository == "" {
		t.Error("values.yaml image.repository is empty: the published image is the one default worth having")
	}
	if values.Auth.Password != "" || values.Auth.Insecure {
		t.Error("values.yaml ships an authentication default — the posture must be chosen by the operator, not by the chart")
	}
}

// readClusterRole parses a plain manifest.
func readClusterRole(t *testing.T, rel string) clusterRole {
	t.Helper()
	var role clusterRole
	unmarshalFile(t, rel, &role)
	if role.Kind != "ClusterRole" {
		t.Fatalf("%s: kind = %q, want ClusterRole", rel, role.Kind)
	}
	return role
}

// readTemplatedClusterRole parses the rules out of a Helm template without
// rendering it, so the drift check runs even where `helm` is not installed.
//
// It works because the chart's rules block is deliberately plain YAML: only the
// metadata above it is templated, and the `{{ if }}` wrapper contributes whole
// lines that are dropped here. If someone templates a rule, this stops parsing
// and the test says so rather than quietly comparing less.
func readTemplatedClusterRole(t *testing.T, rel string) clusterRole {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "rules:" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s has no `rules:` line — the chart's detect ClusterRole moved or was renamed", rel)
	}
	kept := make([]string, 0, len(lines)-start)
	for _, line := range lines[start:] {
		if strings.HasPrefix(strings.TrimSpace(line), "{{") {
			continue
		}
		if strings.Contains(line, "{{") {
			t.Fatalf("%s templates a line inside `rules:` (%q). The rules block is compared as literal YAML "+
				"against %s; keep templating out of it.", rel, strings.TrimSpace(line), deployRBACPath)
		}
		kept = append(kept, line)
	}
	var role clusterRole
	if err := yaml.Unmarshal([]byte(strings.Join(kept, "\n")), &role); err != nil {
		t.Fatalf("parsing the rules block of %s: %v", rel, err)
	}
	role.Kind = "ClusterRole"
	return role
}

func unmarshalFile(t *testing.T, rel string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("parsing %s: %v", rel, err)
	}
}

func mustYAML(t *testing.T, v any) string {
	t.Helper()
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling for the failure message: %v", err)
	}
	return string(out)
}
