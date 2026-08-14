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
//  1. the namespace records only the renderer's "declared" ownership, which is
//     NOT licence to delete it — so the namespace survives (the applier that
//     recorded authorship is deleted, ADR-0028; it returns with issue #224);
//  2. without --yes and with no terminal, it deletes nothing and exits
//     non-zero;
//  3. with --yes it previews, deletes, and reports per object;
//  4. every rendered object is gone and the namespace is not;
//  5. a second run reports nothing to do and exits 0.
//
// The bystanders are planted here too, and they must survive: the sweep is by
// label and the namespace stays, so nothing but the labelled set may go.
func TestUninstallRemovesExactlyWhatKelsonDeployed(t *testing.T) {
	h := newHarness(t, "kelson-e2e-uninstall")
	const env = "uninstall"

	spec := h.copyFixture("spec.yaml")

	t.Log("== put the rendered set in the cluster ==")
	h.applyRendered(spec, env)
	h.waitForRollout("web", baseImage, rolloutTimeout)

	rendered := h.renderedNamespacedSet(spec, env)
	if len(rendered) == 0 {
		t.Fatal("the fixture rendered nothing namespaced, so this test would prove nothing")
	}

	t.Log("== the namespace records only that kelson declared it ==")
	// "created" is the ONLY value uninstall accepts as licence to delete a
	// namespace, and it was written by the applier ADR-0028 deleted. Nothing
	// upgrades the renderer's "declared" today, so the namespace survives the
	// uninstall — asserted below. The authorship half returns with the
	// controller (issue #224), and this assertion goes back to "created" then.
	h.assertNamespaceOwnership("declared")

	t.Log("== plant bystanders kelson must not sweep ==")
	bystanders := h.plantBystanders()

	t.Log("== without --yes and with no terminal, it deletes nothing ==")
	before := h.listKeys(deletableTypes, projectSelector)
	refused := h.kelson("uninstall", "--project", uninstallProject, "--env", env)
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
	res := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--yes")
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

	t.Log("== the namespace stays, because nothing recorded that kelson created it ==")
	// This is the conservative half of the rule and it is the half that matters:
	// with no authorship record, uninstall refuses to delete the namespace
	// rather than assuming. A namespace delete takes everything inside it, so
	// "kelson is not sure" must mean "kelson leaves it".
	phase := strings.TrimSpace(h.kubectlOK("get", "namespace", h.namespace, "-o", "jsonpath={.status.phase}").stdout)
	if phase != "Active" {
		t.Fatalf("namespace %s is %q, want Active: with ownership %q recorded, uninstall must leave it",
			h.namespace, phase, "declared")
	}

	t.Log("== a second run reports nothing to do ==")
	second := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--yes")
	if !strings.Contains(second.stdout, "nothing to do") {
		t.Errorf("the second uninstall does not report an empty plan; a repeated uninstall must be a clean no-op\n%s",
			second.combined())
	}
}

// TestUninstallLeavesAnAdoptedNamespaceAndItsBystanders is the other half of the
// namespace rule, and the one carrying the non-destructive claim.
//
// The namespace here is created by the test. Uninstall must delete everything
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

	t.Log("== put the rendered set into the pre-existing namespace ==")
	h.applyRendered(spec, env)
	h.waitForRollout("web", baseImage, rolloutTimeout)
	h.assertNamespaceOwnership("declared")

	t.Log("== plant bystanders kelson must not touch ==")
	bystanders := h.plantBystanders()

	t.Log("== uninstall ==")
	res := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--yes")
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

	t.Log("== a second run reports nothing to do ==")
	second := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--yes")
	if !strings.Contains(second.stdout, "nothing to do") {
		t.Errorf("the second uninstall does not report an empty plan\n%s", second.combined())
	}
}

// TestUninstallLeavesACreatedNamespaceAnotherProjectMovedInto is the third
// namespace case, and the one issue #215 was filed for.
//
// This namespace is kelson's own: the deploy records
// kelson.dev/namespace-ownership=created, which used to be the entire licence to
// delete it. But `spec.namespace` is overridable, so a second deployment can be
// pointed at a namespace that already exists — and then "kelson created it" is a
// fact about the past that says nothing about who is in it now. Deleting it on
// that evidence alone evicts the other project's workloads and its data.
//
// So the uninstall must delete everything it labelled, leave the namespace
// standing, and name the deployment it is standing for.
func TestUninstallLeavesACreatedNamespaceAnotherProjectMovedInto(t *testing.T) {
	h := newHarness(t, "kelson-e2e-shared")
	const env = "shared"

	// This scenario leaves its namespace behind on purpose, so a second run
	// against the same cluster would deploy into a namespace that already
	// exists, record `adopted`, and prove the adopted case again instead of this
	// one. Clearing it first is what keeps the scenario about the CREATED case.
	h.kubectlOK("delete", "namespace", h.namespace, "--ignore-not-found", "--wait=true")

	spec := h.copyFixture("spec.yaml")

	t.Log("== put the rendered set in the cluster ==")
	h.applyRendered(spec, env)
	h.waitForRollout("web", baseImage, rolloutTimeout)

	t.Log("== record that kelson created the namespace ==")
	// The applier that stamped `created` at apply time went with the direct
	// plane (ADR-0028; it returns with issue #224), and the renderer alone only
	// claims `declared`. The annotation is an input to this test, not its
	// subject: what is under test is what uninstall does when it holds the
	// created licence AND somebody else is living in the namespace, so the
	// licence is written here rather than waited for.
	h.kubectlOK("annotate", "namespace", h.namespace,
		"kelson.dev/namespace-ownership=created", "--overwrite")
	h.assertNamespaceOwnership("created")

	t.Log("== a second project is deployed into the same namespace ==")
	tenant := h.plantForeignTenant("grocery", "production")
	bystanders := h.plantBystanders()

	t.Log("== uninstall ==")
	res := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--yes")
	for _, want := range []string{"stays", "left behind", "grocery/production"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the preview does not say %q; the namespace survives and nothing names the deployment it "+
				"survives for\n%s", want, res.combined())
		}
	}
	h.assertNotDeleted(res, append(bystanders, tenant))

	h.waitFor("every labelled resource of this environment to be gone", uninstallTimeout, func() (bool, string) {
		left := h.listKeys(deletableTypes, projectSelector)
		return len(left) == 0, "still present: " + sortedKeys(left)
	})

	t.Log("== the namespace and the other project's resource survived ==")
	phase := strings.TrimSpace(h.kubectlOK("get", "namespace", h.namespace, "-o", "jsonpath={.status.phase}").stdout)
	if phase != "Active" {
		t.Fatalf("namespace %s is %q after uninstalling one of the two deployments in it, want Active — the other "+
			"project's workloads and data went with it", h.namespace, phase)
	}
	for _, survivor := range append(bystanders, tenant) {
		if got := h.kubectl("-n", h.namespace, "get", survivor); got.code != 0 {
			t.Errorf("%s did not survive `kelson uninstall` (exit %d)\n%s\nkelson deleted something that is not its to delete",
				survivor, got.code, got.combined())
		}
	}

	t.Log("== a second run has nothing to do, and still says why the namespace stays ==")
	second := h.kelsonOK("uninstall", "--project", uninstallProject, "--env", env, "--yes")
	if !strings.Contains(second.stdout, "nothing to do") {
		t.Errorf("the second uninstall does not report an empty plan\n%s", second.combined())
	}
	if !strings.Contains(second.stdout, "grocery/production") {
		t.Errorf("the second uninstall stops naming the deployment the namespace is kept for; the reason has to "+
			"outlive the resources\n%s", second.combined())
	}
}

// plantForeignTenant creates a resource carrying kelson's FULL provenance for a
// different (project, environment), which is what a second deployment sharing
// this namespace looks like to the cluster.
//
// It is a different fixture from plantBystanders' `other-project`, and the
// difference is the point: that one carries a project label but NOT
// app.kubernetes.io/managed-by=kelson, so it is somebody's label rather than a
// kelson deployment, and a namespace kelson created still goes when it is the
// only thing in the way (TestUninstallRemovesExactlyWhatKelsonDeployed).
func (h *harness) plantForeignTenant(project, environment string) string {
	h.t.Helper()
	h.kubectlOK("-n", h.namespace, "delete", "configmap", "other-tenant", "--ignore-not-found")
	h.kubectlOK("-n", h.namespace, "create", "configmap", "other-tenant", "--from-literal=owner="+project)
	h.kubectlOK("-n", h.namespace, "label", "configmap", "other-tenant",
		"app.kubernetes.io/managed-by=kelson",
		"kelson.dev/project="+project,
		"kelson.dev/environment="+environment,
		"--overwrite")
	return "configmap/other-tenant"
}

// assertNamespaceOwnership reads the namespace-ownership annotation. It reads
// the whole annotation map rather than one escaped
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
