package detect

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// Acceptance criterion 1 (issue #56) is that "detection requires read-only
// permissions and does not need cluster-admin". These tests enforce that from
// the manifest itself rather than asserting it in prose: the ClusterRole at
// deploy/rbac/detect-clusterrole.yaml must grant no verb outside {get, list,
// watch}, and must grant every cluster-scoped list the probe issues.

// TestDetectClusterRoleReadOnly fails if any verb outside {get, list, watch}
// appears, or if the "*" verb does. A write verb or a "*" would break the
// read-only contract and silently let detection drift toward cluster-admin.
func TestDetectClusterRoleReadOnly(t *testing.T) {
	role := readDetectClusterRole(t)
	if role.Kind != "ClusterRole" {
		t.Fatalf("expected kind ClusterRole, got %q", role.Kind)
	}
	if len(role.Rules) == 0 {
		t.Fatal("RBAC has no rules — nothing would grant the probe its reads")
	}
	allowed := map[string]bool{"get": true, "list": true, "watch": true}
	for i, r := range role.Rules {
		for _, v := range r.Verbs {
			if !allowed[v] {
				t.Errorf("rule %d uses verb %q — detection must be read-only (get/list only)", i, v)
			}
			if v == "*" {
				t.Errorf("rule %d uses the '*' verb, which is forbidden by the read-only contract", i)
			}
		}
	}
}

// TestDetectClusterRoleCoversListedResources keeps the RBAC the required
// minimum by checking every cluster-scoped list the probe issues is granted.
func TestDetectClusterRoleCoversListedResources(t *testing.T) {
	role := readDetectClusterRole(t)
	needed := []string{"nodes", "ingressclasses", "storageclasses", "volumesnapshotclasses", "gatewayclasses", "clusterissuers", "clustersecretstores", "secretstores"}
	got := map[string]bool{}
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			got[res] = true
		}
	}
	for _, res := range needed {
		if !got[res] {
			t.Errorf("detection lists %q but the ClusterRole does not grant it", res)
		}
	}
}

type detectClusterRole struct {
	Kind  string `yaml:"kind"`
	Rules []struct {
		Resources       []string `yaml:"resources"`
		NonResourceURLs []string `yaml:"nonResourceURLs"`
		Verbs           []string `yaml:"verbs"`
	} `yaml:"rules"`
}

func readDetectClusterRole(t *testing.T) detectClusterRole {
	t.Helper()
	path := filepath.Join("..", "..", "..", "deploy", "rbac", "detect-clusterrole.yaml")
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var role detectClusterRole
	if err := yaml.Unmarshal(data, &role); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return role
}
