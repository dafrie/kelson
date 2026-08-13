//go:build e2e

package e2e

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// projectSelector is the label every resource kelson renders carries
// (internal/renderer/renderer.go). It is the handle an uninstall would use, so
// it is the handle this test proves is exact.
const projectSelector = "kelson.dev/project=kelson-e2e"

// deletableTypes are the namespaced kinds an uninstall would sweep. It is
// deliberately wider than what this fixture renders: the point of the test is
// that nothing outside the rendered set is caught, so the net has to be cast
// wider than the set.
const deletableTypes = "deployments,statefulsets,daemonsets,cronjobs,jobs,services,serviceaccounts,configmaps,secrets,persistentvolumeclaims"

// TestDeleteByLabelIsExactlyTheRenderedSet is the harness-level statement of
// the non-destructive uninstall claim (issue #59), scoped to what can be proven
// without an uninstall command — there is none in cmd/kelson yet, and building
// one is #59's feature work, not this harness's.
//
// What it proves:
//
//  1. Every resource kelson put in the namespace carries the project label, and
//     the set the label selects is EXACTLY the set the renderer produced —
//     nothing extra was labelled, nothing rendered was left unlabelled.
//  2. Deleting by that selector removes exactly that set and leaves everything
//     else in the namespace untouched: an unlabelled bystander, a resource
//     labelled for a different project, and the namespace's own furniture.
//  3. The Namespace survives, and still carries kelson.dev/namespace-ownership.
//     That annotation is the reason deleting the namespace is not the uninstall
//     path: it records that kelson DECLARED the namespace, not that it created
//     it (internal/renderer/namespace.go), and a namespace delete would take
//     the bystanders with it.
func TestDeleteByLabelIsExactlyTheRenderedSet(t *testing.T) {
	h := newHarness(t, "kelson-e2e-additivity")
	const env = "additivity"

	spec := h.copyFixture("spec.yaml")

	t.Log("== deploy the set ==")
	h.kelsonOK("deploy", "-f", spec, "--env", env, "--history", h.history, "--timeout", deployTimeout, "--yes")
	h.waitForRollout("web", baseImage, rolloutTimeout)

	t.Log("== plant bystanders kelson must not touch ==")
	bystanders := h.plantBystanders()

	t.Log("== the labelled set is exactly the rendered set ==")
	rendered := h.renderedNamespacedSet(spec, env)
	labelled := h.listKeys(deletableTypes, projectSelector)
	if !sameSet(rendered, labelled) {
		t.Fatalf("the label selector %s does not select exactly what kelson rendered\n  rendered: %s\n  labelled: %s\n"+
			"(a rendered resource missing from the labelled set would survive an uninstall; an extra one would be deleted by it)",
			projectSelector, sortedKeys(rendered), sortedKeys(labelled))
	}

	t.Log("== delete by selector ==")
	h.kubectlOK("-n", h.namespace, "delete", deletableTypes, "-l", projectSelector, "--wait=true")

	h.waitFor("every labelled resource to be gone", 2*time.Minute, func() (bool, string) {
		left := h.listKeys(deletableTypes, projectSelector)
		return len(left) == 0, "still present: " + sortedKeys(left)
	})

	t.Log("== everything else survived ==")
	for _, b := range bystanders {
		res := h.kubectl("-n", h.namespace, "get", b)
		if res.code != 0 {
			t.Errorf("%s did not survive the label-selector delete (exit %d)\n%s\n"+
				"this is the additivity claim failing: kelson's delete handle reached something kelson did not create",
				b, res.code, res.combined())
		}
	}

	// Read the two facts separately rather than through one escaped jsonpath
	// expression: the annotation key contains dots, and an escaping mistake
	// would read as "the annotation is gone".
	phase := strings.TrimSpace(h.kubectlOK("get", "namespace", h.namespace, "-o", "jsonpath={.status.phase}").stdout)
	if phase != "Active" {
		t.Errorf("namespace %s is %q after the delete, want Active — a namespace delete would have taken the bystanders with it",
			h.namespace, phase)
	}
	annotations := h.kubectlOK("get", "namespace", h.namespace, "-o", "jsonpath={.metadata.annotations}").stdout
	if !strings.Contains(annotations, `"kelson.dev/namespace-ownership":"declared"`) {
		t.Errorf("namespace %s does not carry kelson.dev/namespace-ownership=declared; that annotation is what tells an "+
			"uninstall the namespace is declared, not owned\nannotations: %s", h.namespace, strings.TrimSpace(annotations))
	}
}

// plantBystanders creates the resources that must survive: one with no kelson
// labels at all, and one labelled for a different project — the case a
// selector that dropped the project name would get wrong. It returns the
// "kind/name" arguments to check them with, including the namespace's own
// furniture, which no test created and nothing may delete.
func (h *harness) plantBystanders() []string {
	h.t.Helper()
	// Bystanders survive by design, including across runs — this test deletes
	// only what carries the project label. Clearing them first keeps a second
	// run on the same cluster from failing on AlreadyExists.
	h.kubectlOK("-n", h.namespace, "delete", "configmap", "bystander", "other-project", "--ignore-not-found")
	h.kubectlOK("-n", h.namespace, "create", "configmap", "bystander", "--from-literal=owner=not-kelson")
	h.kubectlOK("-n", h.namespace, "create", "configmap", "other-project", "--from-literal=owner=another-team")
	h.kubectlOK("-n", h.namespace, "label", "configmap", "other-project",
		"kelson.dev/project=some-other-project", "kelson.dev/environment=additivity", "--overwrite")
	return []string{
		"configmap/bystander",
		"configmap/other-project",
		// Furniture Kubernetes puts in every namespace. If an uninstall handle
		// ever swept these, the namespace would be left unusable.
		"configmap/kube-root-ca.crt",
		"serviceaccount/default",
	}
}

// renderedNamespacedSet asks the CLI what it renders and reduces it to the
// "Kind/name" set the cluster is compared against. The Namespace is excluded:
// it is cluster-scoped, and it is precisely the resource an uninstall may not
// delete on the strength of a label.
func (h *harness) renderedNamespacedSet(spec, env string) map[string]bool {
	h.t.Helper()
	res := h.kelsonOK("render", "-f", spec, "--env", env)

	set := map[string]bool{}
	dec := yaml.NewDecoder(strings.NewReader(res.stdout))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.t.Fatalf("parsing `kelson render` output: %v\n%s", err, res.stdout)
		}
		if doc.Kind == "" || doc.Kind == "Namespace" {
			continue
		}
		if doc.Metadata.Namespace != h.namespace {
			h.t.Fatalf("rendered %s/%s targets namespace %q, but the harness is watching %q",
				doc.Kind, doc.Metadata.Name, doc.Metadata.Namespace, h.namespace)
		}
		set[doc.Kind+"/"+doc.Metadata.Name] = true
	}
	if len(set) == 0 {
		h.t.Fatalf("`kelson render` produced no namespaced resources\n%s", res.stdout)
	}
	return set
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
