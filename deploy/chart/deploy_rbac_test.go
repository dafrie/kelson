package chart

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The deploy ClusterRole's contract (the opt-in cluster-scoped grant in
// templates/clusterrole-deploy.yaml).
//
// The detect ClusterRole is policed by comparing it against a manifest that is
// its own source of truth (deploy/rbac/detect-clusterrole.yaml, chart_test.go).
// The deploy grant has no such twin — there is no standalone manifest for it,
// because unlike detection it is never needed without the chart: the CLI
// applies under the operator's own kubeconfig, and the server only ever arrives
// by Helm. So its drift guard is against the thing it actually has to track:
// the set of kinds internal/renderer can emit.
//
// That set is derived here from the golden corpus rather than from a list
// written twice. A new rendered kind arrives with a golden file — the renderer's
// tests make sure of that — and this test fails until the ClusterRole covers it.
//
// The honest limit, and it is stated in the template too: the golden corpus is
// the *observed* set. `spec.overlays` can emit any kind at all
// (internal/renderer/overlay.go), so covering everything in the corpus is not
// the same as covering everything a user can apply.

const deployRoleTemplatePath = "kelson/templates/clusterrole-deploy.yaml"

// goldenGlob finds the renderer's golden files from this package's directory.
// Reaching across planes is unusual and deliberate: the RBAC and the renderer's
// output are one contract split over two directories, and the only way to keep
// them together is for something to read both.
const goldenGlob = "../../internal/renderer/testdata/render/*/expected.golden"

// TestDeployRoleCoversEveryRenderedKind is the drift guard. Every
// (apiGroup, resource) the renderer is known to emit must appear in the deploy
// ClusterRole with the verbs an apply and a prune need.
func TestDeployRoleCoversEveryRenderedKind(t *testing.T) {
	rules := readTemplatedClusterRole(t, deployRoleTemplatePath).Rules
	if len(rules) == 0 {
		t.Fatalf("%s has no rules — the check would pass vacuously", deployRoleTemplatePath)
	}

	// What an applied kind needs, and why, is spelled out in the template. The
	// short version: patch+create is the server-side apply, get is the status
	// read-back, list+delete is the prune.
	wantVerbs := []string{"get", "list", "create", "patch", "delete"}

	kinds := renderedKinds(t)
	// Logged rather than asserted against a number: the point is that a reader
	// of `go test -v` can see the derived set, and a hard-coded count would be
	// one more thing to update for every new fixture without saying anything.
	for _, k := range kinds {
		t.Logf("renderer emits %s (apiGroup %q)", k.resource, k.group)
	}

	for _, kind := range kinds {
		group, resource := kind.group, kind.resource
		// Namespaces are the one rendered kind that deliberately gets no
		// `delete`: the adapter never prunes one and the server has no RPC that
		// removes one. Assert the rest.
		verbs := wantVerbs
		if group == "" && resource == "namespaces" {
			verbs = []string{"get", "list", "create", "patch"}
		}
		for _, verb := range verbs {
			if !granted(rules, group, resource, verb) {
				t.Errorf("the deploy ClusterRole does not grant %q on %s (apiGroup %q), "+
					"but %s renders it.\n"+
					"Add it to %s — a kind the renderer emits and RBAC does not cover is a deploy that "+
					"fails with `forbidden` on the user's first click.",
					verb, resource, group, kind.source, deployRoleTemplatePath)
			}
		}
	}
}

// TestDeployRoleGrantsNamespacePatch pins the specific verb the reported bug
// named, separately from the derived sweep above. The whole template exists
// because a namespaced Role cannot grant it, so it deserves a test that says so
// by name rather than only as one row of a table.
func TestDeployRoleGrantsNamespacePatch(t *testing.T) {
	rules := readTemplatedClusterRole(t, deployRoleTemplatePath).Rules
	for _, verb := range []string{"get", "list", "create", "patch"} {
		if !granted(rules, "", "namespaces", verb) {
			t.Errorf("the deploy ClusterRole does not grant %q on namespaces; "+
				"internal/delivery/direct.stampNamespaceOwnership and the apply that follows it need all four", verb)
		}
	}
	// Not granted, and the template says why: pruning excludes namespaces and
	// `kelson uninstall` runs from the CLI under a human's kubeconfig.
	if granted(rules, "", "namespaces", "delete") {
		t.Error("the deploy ClusterRole grants delete on namespaces. " +
			"Nothing the server does deletes one, and deleting a namespace cascades to resources " +
			"kelson never created (#59).")
	}
}

// TestDeployRoleCoversTheEnvRoleReads: applying is only the first wall. The
// namespace the web UI deploys into is created by that apply, so the reads
// behind the first status verdict, the first log stream and the first secret
// write have no namespaced Role either. They are duplicated from rbac.yaml's
// `-env` Role on purpose (see the template) and this pins the duplication.
func TestDeployRoleCoversTheEnvRoleReads(t *testing.T) {
	rules := readTemplatedClusterRole(t, deployRoleTemplatePath).Rules
	for _, want := range []struct {
		group, resource string
		verbs           []string
		why             string
	}{
		{"", "secrets", []string{"get", "list", "create", "patch", "delete"}, "internal/secret, behind SecretService"},
		{"", "pods", []string{"get", "list"}, "internal/observation's status probe"},
		{"", "pods/log", []string{"get"}, "the log streams the API and the web UI serve"},
		{"apps", "deployments", []string{"get", "list"}, "internal/observation's deployment read"},
		{"batch", "jobs", []string{"get", "create", "delete"}, "internal/delivery/kube's build executor"},
		{"fluxcd.controlplane.io", "resourcesets", []string{"get", "list"}, "PreviewService.ListPreviews"},
		{"fluxcd.controlplane.io", "resourcesetinputproviders", []string{"get", "list"}, "PreviewService.ListPreviews"},
		{"source.toolkit.fluxcd.io", "ocirepositories", []string{"get", "list"}, "PreviewService.ListPreviews"},
		{"kustomize.toolkit.fluxcd.io", "kustomizations", []string{"get", "list"}, "PreviewService.ListPreviews"},
		{"gateway.networking.k8s.io", "httproutes", []string{"get", "list"}, "the preview hostnames a namespaced Role could never reach"},
	} {
		for _, verb := range want.verbs {
			if !granted(rules, want.group, want.resource, verb) {
				t.Errorf("the deploy ClusterRole does not grant %q on %s (apiGroup %q), needed by %s",
					verb, want.resource, want.group, want.why)
			}
		}
	}
}

// TestDeployRoleHasNoWildcards holds the template to its own claim. A `*`
// anywhere would make every other test here decorative.
func TestDeployRoleHasNoWildcards(t *testing.T) {
	role := readTemplatedClusterRole(t, deployRoleTemplatePath)
	for i, r := range role.Rules {
		for _, field := range [][]string{r.APIGroups, r.Resources, r.Verbs} {
			for _, v := range field {
				if v == "*" {
					t.Errorf("rule %d contains a wildcard. The deploy grant is broad enough already: "+
						"every apiGroup, resource and verb is named so that reading the ClusterRole tells you "+
						"what the server can do.", i)
				}
			}
		}
	}
	// `update` and `watch` are absent by design — the delivery plane server-side
	// applies and polls, and a verb nothing calls is a verb not to grant.
	for i, r := range role.Rules {
		for _, v := range r.Verbs {
			if v == "update" || v == "deletecollection" {
				t.Errorf("rule %d grants %q. Nothing in internal/delivery does a read-modify-write PUT "+
					"or deletes by selector; if that changed, change this test and say why in the template.", i, v)
			}
		}
	}
}

// TestValuesDefaultDeployClusterRoleOff pins the posture. Turning this on is a
// decision the operator makes; a chart that made it for them would be granting
// itself cluster-wide write on `helm install` with no one having asked.
func TestValuesDefaultDeployClusterRoleOff(t *testing.T) {
	var values struct {
		RBAC struct {
			Create                  bool `yaml:"create"`
			CreateDetectClusterRole bool `yaml:"createDetectClusterRole"`
			CreateDeployClusterRole bool `yaml:"createDeployClusterRole"`
		} `yaml:"rbac"`
	}
	unmarshalFile(t, chartValuesPath, &values)

	if values.RBAC.CreateDeployClusterRole {
		t.Error("values.yaml defaults rbac.createDeployClusterRole to true. " +
			"The grant is cluster-wide write; it has to be opted into out loud (#84).")
	}
	if !values.RBAC.Create || !values.RBAC.CreateDetectClusterRole {
		t.Error("the existing rbac defaults changed; this test exists to notice that the deploy value " +
			"was added without disturbing them")
	}
}

// --- deriving the renderer's kinds ------------------------------------------

// renderedKind is one (apiGroup, resource) pair the renderer emits, plus the
// golden file it was seen in so a failure names something a reader can open.
type renderedKind struct {
	group    string
	resource string
	source   string
}

// renderedKinds reads every golden file and collects the top-level documents'
// apiVersion/kind pairs.
//
// Only column-zero `apiVersion:` and `kind:` lines count, which is what tells a
// document apart from the kinds that appear *inside* one: a HelmRelease's
// `valuesFrom` names a ConfigMap, a ResourceSet's `inputsFrom` names its input
// provider, and its `resourcesTemplate` is a whole embedded manifest set that
// flux-operator — not kelson — applies. None of those need a grant here.
func renderedKinds(t *testing.T) []renderedKind {
	t.Helper()
	files, err := filepath.Glob(goldenGlob)
	if err != nil {
		t.Fatalf("globbing the renderer's golden files: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no golden files matched %q. If the renderer's testdata moved, this test moved with it — "+
			"a coverage check that finds nothing to cover passes vacuously and is worse than no check.", goldenGlob)
	}

	seen := map[string]renderedKind{}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		var apiVersion string
		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "apiVersion: "):
				apiVersion = strings.TrimSpace(strings.TrimPrefix(line, "apiVersion: "))
			case strings.HasPrefix(line, "kind: "):
				kind := strings.TrimSpace(strings.TrimPrefix(line, "kind: "))
				if apiVersion == "" {
					t.Fatalf("%s: `kind: %s` with no preceding apiVersion; the golden format changed", file, kind)
				}
				group := apiGroupOf(apiVersion)
				rk := renderedKind{group: group, resource: resourceOf(t, kind), source: file}
				seen[group+"/"+rk.resource] = rk
				apiVersion = ""
			}
		}
	}

	out := make([]renderedKind, 0, len(seen))
	for _, k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].group != out[j].group {
			return out[i].group < out[j].group
		}
		return out[i].resource < out[j].resource
	})
	return out
}

// apiGroupOf splits an apiVersion into the RBAC apiGroup. Core is "" and is
// spelled `v1`, which is the one case with no slash in it.
func apiGroupOf(apiVersion string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return group
}

// resourceOf turns a Kind into the resource name RBAC uses.
//
// Kubernetes derives this from the CRD or the built-in type registration and
// there is no general rule, so this handles the two shapes every kind kelson
// renders actually has — `…y` → `…ies` (NetworkPolicy → networkpolicies,
// HelmRepository → helmrepositories) and otherwise a plain `s`. A kind whose
// plural is irregular in some other way (an `…s` or `…x` ending, say) would be
// derived wrong and silently, so the function refuses those out loud rather
// than guessing: the fix is one case here plus the matching rule in the
// ClusterRole.
func resourceOf(t *testing.T, kind string) string {
	t.Helper()
	lower := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(lower, "s"), strings.HasSuffix(lower, "x"),
		strings.HasSuffix(lower, "ch"), strings.HasSuffix(lower, "sh"):
		t.Fatalf("kind %q has a plural this test cannot derive. Add it to resourceOf and to %s.",
			kind, deployRoleTemplatePath)
		return ""
	case strings.HasSuffix(lower, "y"):
		return strings.TrimSuffix(lower, "y") + "ies"
	default:
		return lower + "s"
	}
}

// granted reports whether the rule set allows verb on (group, resource). RBAC
// is a union of rules, so one rule matching all three is enough.
func granted(rules []policyRule, group, resource, verb string) bool {
	for _, r := range rules {
		if !contains(r.APIGroups, group) || !contains(r.Resources, resource) {
			continue
		}
		if contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}
