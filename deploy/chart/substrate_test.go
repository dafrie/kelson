package chart

import (
	"reflect"
	"strings"
	"testing"
)

// The ensure-substrate hook (owner decision 2026-08-16): installing kelson
// installs the delivery substrate, because ADR-0028 makes Flux the only
// reconciliation path and a cluster without it deploys nothing.
//
// The Job is the only place in this chart holding a grant wide enough to
// install an operator, so these tests are less about the Job rendering than
// about the fence around it: it exists only when asked for, it is hook-scoped
// and hook-deleted in every direction, it names the resources it touches rather
// than wildcarding them, and the standing controller grant does not move an inch
// because it is there.
//
// They need `helm` and skip without it, like every other render test here.

// substrateValues turns the controller on, which is what the hook is gated on.
func substrateValues(extra ...string) []string {
	out := append([]string{}, authValues...)
	out = append(out, "--set", "controller.enabled=true")
	return append(out, extra...)
}

// substrateDocs returns the documents the hook renders, by kind.
func substrateDocs(t *testing.T, extra ...string) map[string]map[string]any {
	t.Helper()
	docs := decodeDocs(t, helmTemplate(t, substrateValues(extra...)...))
	out := map[string]map[string]any{}
	for _, doc := range docs {
		if nameOf(doc) == "kelson-ensure-substrate" {
			kind, _ := doc["kind"].(string)
			out[kind] = doc
		}
	}
	return out
}

func annotationsOf(doc map[string]any) map[string]any {
	meta, _ := doc["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	return ann
}

// TestSubstrateHookRendersByDefault: the default is on, and "on" means all four
// objects — a Job with no identity, or an identity with no binding, is a hook
// that fails at its first apply.
func TestSubstrateHookRendersByDefault(t *testing.T) {
	docs := substrateDocs(t)
	for _, kind := range []string{"Job", "ServiceAccount", "ClusterRole", "ClusterRoleBinding"} {
		if docs[kind] == nil {
			t.Errorf("controller.enabled=true rendered no %s for the substrate hook; installing kelson must "+
				"install the substrate it cannot reconcile without (ADR-0028)", kind)
		}
	}
	if t.Failed() {
		return
	}

	spec, _ := docs["Job"]["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	containers := anySlice(podSpec["containers"])
	if len(containers) != 1 {
		t.Fatalf("the hook Job has %d containers, want 1", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	args := stringsOf(container["args"])
	if !contains(args, "--ensure-substrate") {
		t.Errorf("the hook Job does not run --ensure-substrate; its args are %v", args)
	}
	// The bounce. The controller detects Flux once at start-up, so a hook that
	// installed Flux and did not restart it would leave every environment
	// reporting FluxNotInstalled under a green release.
	if !contains(args, "--substrate-restart-deployment=kelson-system/kelson-controller") {
		t.Errorf("the hook Job does not name the controller Deployment to restart. The controller's Flux "+
			"detection is one-shot at start-up, so without the bounce convergence depends on which pod won "+
			"a race; args are %v", args)
	}
	if podSpec["serviceAccountName"] != "kelson-ensure-substrate" {
		t.Errorf("the hook Job runs as %v, not its own hook-scoped ServiceAccount", podSpec["serviceAccountName"])
	}
	// The controller image, which is how the mode reaches the cluster without
	// publishing a second image or pinning a second tag.
	if got, want := container["image"], "ghcr.io/dafrie/kelson-controller:v0.1.0"; got != want {
		t.Errorf("the hook Job runs image %v, want the controller image %q", got, want)
	}
	if spec["backoffLimit"] != 0 {
		t.Errorf("the hook Job's backoffLimit is %v, want 0: every apply is a server-side apply and "+
			"`helm upgrade` is the retry, so a blind re-run only doubles the wait before somebody reads "+
			"the failure", spec["backoffLimit"])
	}
}

// TestSubstrateHookIsHookScoped is the containment claim in mechanical form.
// Every object carries a hook annotation AND a delete policy, so nothing here
// outlives the Job: the ServiceAccount and its grant go whether the hook
// succeeds or fails, and only a failed Job is kept, because it is the only
// record of why.
func TestSubstrateHookIsHookScoped(t *testing.T) {
	docs := substrateDocs(t)
	if len(docs) == 0 {
		t.Fatal("the substrate hook rendered nothing to check")
	}
	for kind, doc := range docs {
		ann := annotationsOf(doc)
		if ann == nil {
			t.Errorf("the substrate %s carries no annotations, so it is an ordinary standing object", kind)
			continue
		}
		if ann["helm.sh/hook"] != "post-install,post-upgrade" {
			t.Errorf("the substrate %s has helm.sh/hook = %v, want post-install,post-upgrade",
				kind, ann["helm.sh/hook"])
		}
		policy, _ := ann["helm.sh/hook-delete-policy"].(string)
		if !strings.Contains(policy, "before-hook-creation") || !strings.Contains(policy, "hook-succeeded") {
			t.Errorf("the substrate %s has delete policy %q; a hook object that survives its hook is a "+
				"standing grant nobody asked for", kind, policy)
		}
		if kind == "Job" {
			continue
		}
		if !strings.Contains(policy, "hook-failed") {
			t.Errorf("the substrate %s is not deleted when the hook fails (%q). An installer identity left "+
				"standing after a failure is exactly the grant this hook exists to keep temporary", kind, policy)
		}
	}
}

// TestSubstrateHookGrantIsNamedAndBounded: the widest grant in this chart, held
// to naming what it touches.
//
// The rules are derived from what the two Flux rows actually apply
// (internal/delivery/install), so the assertions follow that derivation: the
// kinds are named, no wildcard appears anywhere, and the verbs stop at what a
// server-side apply with per-object provenance needs. `delete` is worth a test
// of its own — an installer that could delete a CRD could delete every custom
// resource in the cluster with it, and removal is `kelson uninstall
// --component`, run by a person under their own credentials.
func TestSubstrateHookGrantIsNamedAndBounded(t *testing.T) {
	doc := substrateDocs(t)["ClusterRole"]
	if doc == nil {
		t.Fatal("no substrate ClusterRole was rendered")
	}
	var rules []policyRule
	remarshal(t, doc["rules"], &rules)

	for _, want := range []struct{ group, resource string }{
		{"apiextensions.k8s.io", "customresourcedefinitions"},
		{"", "namespaces"},
		{"apps", "deployments"},
		{"rbac.authorization.k8s.io", "clusterroles"},
		{"fluxcd.controlplane.io", "fluxinstances"},
	} {
		for _, verb := range []string{"get", "create", "patch"} {
			if !granted(rules, want.group, want.resource, verb) {
				t.Errorf("the substrate ClusterRole does not grant %q on %s (apiGroup %q); the install fails "+
					"partway and the release reports it:\n%s", verb, want.resource, want.group, mustYAML(t, rules))
			}
		}
	}
	// `escalate`/`bind` are unavoidable — Flux's controllers hold permissions no
	// enumerable subset of this grant could cover — and they are the reason the
	// lifetime bound above matters. Pinned so their removal is a decision rather
	// than an accident that surfaces as a failed install on somebody's cluster.
	for _, verb := range []string{"escalate", "bind"} {
		if !granted(rules, "rbac.authorization.k8s.io", "clusterroles", verb) {
			t.Errorf("the substrate ClusterRole does not grant %q on clusterroles. Kubernetes forbids an "+
				"installer from creating RBAC wider than its own without it, so installing any reconciler "+
				"fails:\n%s", verb, mustYAML(t, rules))
		}
	}

	for _, r := range rules {
		if contains(r.APIGroups, "*") || contains(r.Resources, "*") || contains(r.Verbs, "*") {
			t.Errorf("the substrate ClusterRole wildcards %v/%v/%v. It is derived from what the pinned Flux "+
				"manifests contain, and a wildcard is the opposite of that derivation:\n%s",
				r.APIGroups, r.Resources, r.Verbs, mustYAML(t, rules))
		}
		for _, verb := range r.Verbs {
			switch verb {
			case "get", "create", "patch", "escalate", "bind":
			default:
				t.Errorf("the substrate ClusterRole grants %q on %v. This hook installs: it reads each object "+
					"to decide created-versus-adopted and applies it, and nothing else:\n%s",
					verb, r.Resources, mustYAML(t, rules))
			}
		}
	}
}

// TestSubstrateHookAbsentWhenDisabled: both gates, and completely.
//
// substrate.autoInstall=false is the off-switch for a cluster whose Flux is
// somebody else's pipeline; controller.enabled=false means nothing in this
// release reconciles at all, so installing a reconciler for it would be a grant
// taken for no purpose. Either way the ServiceAccount and the ClusterRole must
// go with the Job — an off-switch that left the grant behind would be the worst
// of both.
func TestSubstrateHookAbsentWhenDisabled(t *testing.T) {
	for _, tc := range []struct{ name, flag string }{
		{"autoInstall off", "substrate.autoInstall=false"},
		{"controller off", "controller.enabled=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := decodeDocs(t, helmTemplate(t, substrateValues("--set", tc.flag)...))
			for _, doc := range docs {
				if strings.Contains(nameOf(doc), "ensure-substrate") {
					t.Errorf("%s still rendered %v/%s; the off-switch has to be complete, grant included",
						tc.flag, doc["kind"], nameOf(doc))
				}
			}
		})
	}
}

// TestSubstrateHookLeavesTheControllerGrantAlone is the diff-shaped guard the
// whole design rests on: the standing controller ClusterRole and Deployment
// must be identical with the hook on and off.
//
// The temptation this catches is real and would be invisible — folding the
// install grant into the controller's own role "so the hook can reuse it" turns
// a Job-lifetime capability into a permanent one, and nothing else in this
// suite would notice.
func TestSubstrateHookLeavesTheControllerGrantAlone(t *testing.T) {
	with := controllerRules(t)
	without := controllerRules(t, "--set", "substrate.autoInstall=false")
	if !reflect.DeepEqual(with, without) {
		t.Errorf("the controller ClusterRole changes with substrate.autoInstall. The install grant belongs to "+
			"a Job deleted with the hook, and the running controller must gain nothing from it.\n"+
			"with:\n%s\nwithout:\n%s", mustYAML(t, with), mustYAML(t, without))
	}
	// And the Deployment: the hook restarts the controller, it does not
	// reconfigure it.
	if a, b := controllerArgs(t), controllerArgs(t, "--set", "substrate.autoInstall=false"); !reflect.DeepEqual(a, b) {
		t.Errorf("the controller Deployment's args change with substrate.autoInstall:\nwith:    %v\nwithout: %v", a, b)
	}
}

func controllerRules(t *testing.T, extra ...string) []policyRule {
	t.Helper()
	docs := decodeDocs(t, helmTemplate(t, substrateValues(extra...)...))
	var rules []policyRule
	for _, doc := range docs {
		if doc["kind"] == "ClusterRole" && nameOf(doc) == "kelson-controller" {
			remarshal(t, doc["rules"], &rules)
		}
	}
	if rules == nil {
		t.Fatal("no kelson-controller ClusterRole was rendered")
	}
	return rules
}

func controllerArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	docs := decodeDocs(t, helmTemplate(t, substrateValues(extra...)...))
	for _, doc := range docs {
		if doc["kind"] != "Deployment" || nameOf(doc) != "kelson-controller" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers := anySlice(podSpec["containers"])
		if len(containers) != 1 {
			t.Fatalf("the controller Deployment has %d containers, want 1", len(containers))
		}
		container, _ := containers[0].(map[string]any)
		return stringsOf(container["args"])
	}
	t.Fatal("no kelson-controller Deployment was rendered")
	return nil
}
