//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// uninstallProject is the project testdata/minimal.yaml declares.
//
// `kelson uninstall` is addressed by (project, environment) and not by a spec
// file, deliberately: what to delete is a fact about the CLUSTER, recorded in
// the provenance labels on the live objects. A spec that changed since the
// deploy would misstate the set, and someone uninstalling an environment has
// very often already deleted the spec that created it (cmd/kelson/uninstall.go).
const uninstallProject = "kelson-e2e"

// uninstallTimeout is the budget for the swept resources to actually disappear.
// The command returns when the API server has accepted every delete; a
// Deployment's pods and a namespace's finalizers take a little longer.
const uninstallTimeout = 3 * time.Minute

// TestUninstallRemovesExactlyWhatKelsonDeployed is issue #59's acceptance, run
// as the verb rather than as the property.
//
// TestDeleteByLabelIsExactlyTheRenderedSet proves the property the command is
// built on — the provenance selector selects exactly the rendered set — with
// kubectl as the deleting tool. This proves the command:
//
//  1. a deploy records that kelson created the namespace, which is the only
//     licence uninstall accepts for deleting one;
//  2. without --yes and with no terminal, it deletes nothing and exits
//     non-zero;
//  3. with --yes it previews, deletes, and reports per object;
//  4. every rendered object is gone and the namespace with it;
//  5. a second run reports nothing to do and exits 0.
//
// The bystanders are planted here too, but what they prove in THIS scenario is
// that the sweep did not reach them — they live in the namespace kelson
// created, so they go when it does. That they SURVIVE is the adopted-namespace
// scenario below, which is where the non-destructive claim is actually at risk.
func TestUninstallRemovesExactlyWhatKelsonDeployed(t *testing.T) {
	h := newHarness(t, "kelson-e2e-uninstall")
	const env = "uninstall"

	spec := h.copyFixture("spec.yaml")

	t.Log("== deploy the set ==")
	h.kelsonOK("deploy", "-f", spec, "--env", env, "--history", h.history, "--timeout", deployTimeout, "--yes")
	h.waitForRollout("web", baseImage, rolloutTimeout)

	rendered := h.renderedNamespacedSet(spec, env)
	if len(rendered) == 0 {
		t.Fatal("the fixture rendered nothing namespaced, so this test would prove nothing")
	}

	t.Log("== the namespace records that kelson created it ==")
	// internal/delivery/direct.stampNamespaceOwnership writes this at apply
	// time and it is the ONLY thing uninstall accepts as licence to delete a
	// namespace. Asserting it before the uninstall keeps the namespace
	// assertion below from passing for the wrong reason.
	h.assertNamespaceOwnership("created")

	t.Log("== plant bystanders kelson must not sweep ==")
	bystanders := h.plantBystanders()

	t.Log("== without --yes and with no terminal, it deletes nothing ==")
	before := h.listKeys(deletableTypes, projectSelector)
	refused := h.kelson("uninstall", "--project", uninstallProject, "--env", env, "--history", h.history)
	if refused.code == 0 {
		t.Fatalf("uninstall without --yes exited 0 on a non-terminal stdin; there was nobody to answer\n%s", refused.combined())
	}
	if after := h.listKeys(deletableTypes, projectSelector); !sameSet(before, after) {
		t.Fatalf("a refused uninstall changed the cluster\nbefore: %s\nafter:  %s", sortedKeys(before), sortedKeys(after))
	}
	if !strings.Contains(refused.stdout, "Workloads") {
		t.Errorf("the refusal skipped the preview; --yes skips the question, never the preview\n%s", refused.combined())
	}

	t.Log("== uninstall ==")
	res := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--history", h.history, "--yes")
	for _, want := range []string{"kelson uninstall", "Workloads", "deleted "} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the uninstall output does not contain %q; the preview and the per-object results are the contract\n%s",
				want, res.combined())
		}
	}
	h.assertNotDeleted(res, bystanders)

	t.Log("== every rendered object is gone ==")
	h.waitFor("every labelled resource to be gone", uninstallTimeout, func() (bool, string) {
		left := h.listKeys(deletableTypes, projectSelector)
		return len(left) == 0, "still present: " + sortedKeys(left)
	})

	t.Log("== the namespace is gone, because kelson created it ==")
	h.waitFor("the namespace to be gone", uninstallTimeout, func() (bool, string) {
		got := h.kubectl("get", "namespace", h.namespace, "-o", "jsonpath={.status.phase}")
		if got.code != 0 {
			return true, "the namespace is not there, which is what a deleted namespace looks like"
		}
		return false, "namespace phase " + strings.TrimSpace(got.stdout)
	})

	t.Log("== a second run reports nothing to do ==")
	second := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--history", h.history, "--yes")
	if !strings.Contains(second.stdout, "nothing to do") {
		t.Errorf("the second uninstall does not report an empty plan; a repeated uninstall must be a clean no-op\n%s",
			second.combined())
	}
}

// TestUninstallLeavesAnAdoptedNamespaceAndItsBystanders is the other half of the
// namespace rule, and the one carrying the non-destructive claim.
//
// The namespace here is created by the test, so the deploy records
// kelson.dev/namespace-ownership=adopted. Uninstall must then delete everything
// it labelled and leave the namespace — with the unlabelled bystander, another
// project's labelled object and the namespace's own furniture all intact. A
// namespace delete would have taken all three.
func TestUninstallLeavesAnAdoptedNamespaceAndItsBystanders(t *testing.T) {
	h := newHarness(t, "kelson-e2e-adopted")
	const env = "adopted"

	// Creating the namespace first is what makes this the adopted case. It is
	// idempotent so the scenario can be re-run against the same cluster.
	if got := h.kubectl("get", "namespace", h.namespace); got.code != 0 {
		h.kubectlOK("create", "namespace", h.namespace)
	}

	spec := h.copyFixture("spec.yaml")

	t.Log("== deploy into the pre-existing namespace ==")
	h.kelsonOK("deploy", "-f", spec, "--env", env, "--history", h.history, "--timeout", deployTimeout, "--yes")
	h.waitForRollout("web", baseImage, rolloutTimeout)
	h.assertNamespaceOwnership("adopted")

	t.Log("== plant bystanders kelson must not touch ==")
	bystanders := h.plantBystanders()

	t.Log("== uninstall ==")
	res := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--history", h.history, "--yes")
	if !strings.Contains(res.stdout, "stays") {
		t.Errorf("the preview does not say the namespace stays, or why\n%s", res.combined())
	}
	h.assertNotDeleted(res, bystanders)

	h.waitFor("every labelled resource to be gone", uninstallTimeout, func() (bool, string) {
		left := h.listKeys(deletableTypes, projectSelector)
		return len(left) == 0, "still present: " + sortedKeys(left)
	})

	t.Log("== the namespace and every bystander survived ==")
	phase := strings.TrimSpace(h.kubectlOK("get", "namespace", h.namespace, "-o", "jsonpath={.status.phase}").stdout)
	if phase != "Active" {
		t.Fatalf("namespace %s is %q after uninstalling an adopted environment, want Active — this is the "+
			"non-destructive claim failing at its sharpest point", h.namespace, phase)
	}
	for _, b := range bystanders {
		if got := h.kubectl("-n", h.namespace, "get", b); got.code != 0 {
			t.Errorf("%s did not survive `kelson uninstall` (exit %d)\n%s\nkelson deleted something it did not create",
				b, got.code, got.combined())
		}
	}

	t.Log("== the local rendered history for this environment is gone ==")
	// The journal describes a set that no longer exists. Leaving it would give
	// the next deploy of the same name a revision sequence and a prune baseline
	// inherited from a deployment that is gone (direct.Store.Forget).
	status := h.kelson("status", "-f", spec, "--env", env, "--history", h.history)
	if status.code == 0 && strings.Contains(status.stdout, "rev-") {
		t.Errorf("`kelson status` still reports a recorded revision for an uninstalled environment\n%s", status.combined())
	}

	t.Log("== a second run reports nothing to do ==")
	second := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--history", h.history, "--yes")
	if !strings.Contains(second.stdout, "nothing to do") {
		t.Errorf("the second uninstall does not report an empty plan\n%s", second.combined())
	}
}

// assertNamespaceOwnership reads the annotation the delivery plane records at
// apply time. It reads the whole annotation map rather than one escaped
// jsonpath expression, for the reason additivity_test.go does: the key contains
// dots, and an escaping mistake would read as "the annotation is gone".
func (h *harness) assertNamespaceOwnership(want string) {
	h.t.Helper()
	annotations := h.kubectlOK("get", "namespace", h.namespace, "-o", "jsonpath={.metadata.annotations}").stdout
	if !strings.Contains(annotations, `"kelson.dev/namespace-ownership":"`+want+`"`) {
		h.t.Fatalf("namespace %s does not record ownership %q; that annotation is the only licence uninstall "+
			"accepts for deleting a namespace\nannotations: %s", h.namespace, want, strings.TrimSpace(annotations))
	}
}

// assertNotDeleted holds the per-object report to the additivity claim: no
// bystander may appear on a "deleted" line. The report is the only place a
// wrongly swept bystander is visible before the namespace's own deletion could
// explain it away.
func (h *harness) assertNotDeleted(res result, bystanders []string) {
	h.t.Helper()
	for _, b := range bystanders {
		name := b
		if i := strings.Index(b, "/"); i >= 0 {
			name = b[i+1:]
		}
		for _, line := range strings.Split(res.stdout, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "deleted ") && strings.HasSuffix(trimmed, "/"+name) {
				h.t.Errorf("the uninstall reports deleting %s, which kelson did not create:\n  %s\n%s",
					b, trimmed, res.combined())
			}
		}
	}
}
