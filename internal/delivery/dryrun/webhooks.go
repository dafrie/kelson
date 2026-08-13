package dryrun

import (
	"context"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dafrie/kelson/internal/diff"
)

// Coverage honesty for the L2 preview (issue #45).
//
// A server-side dry-run runs the cluster's ValidatingAdmissionPolicies and its
// validating webhooks, which is what makes the preview a real admission
// verdict rather than kelson's guess at one. But not every webhook is reached.
// The API server refuses to call a webhook for a dry-run request unless its
// sideEffects is None or NoneOnDryRun, and what happens next is the webhook's
// own failurePolicy: Fail turns the dry-run into an error kelson can see and
// report (parse.go's dry-run-unsupported classification), while Ignore makes
// the API server skip it silently — the dry-run succeeds and the preview looks
// clean, even though a webhook that will run at apply time never saw the
// object.
//
// The silent case is the dangerous one, and it is invisible from the rejection
// alone. It is visible from the cluster's ValidatingWebhookConfigurations, one
// cheap list, so the preview reads them and reports each webhook it knows the
// dry-run does not reach as a diff.Unvalidated coverage gap. That is the
// tri-state discipline applied to the preview's own reach: checked-and-passing,
// checked-and-failing, and could-not-check are three answers, never two.
//
// The read is best-effort. A caller without permission to list webhook
// configurations gets no gap entries rather than a failed preview — the grant
// is in deploy/rbac/detect-clusterrole.yaml.

var validatingWebhookGVR = schema.GroupVersionResource{
	Group:    "admissionregistration.k8s.io",
	Version:  "v1",
	Resource: "validatingwebhookconfigurations",
}

// dryRunSafeSideEffects are the two sideEffects values the API server accepts
// for a dry-run request. Anything else (including an absent value) means the
// webhook is not called.
var dryRunSafeSideEffects = map[string]bool{"None": true, "NoneOnDryRun": true}

// coverageGaps reports the validating webhooks that a dry-run request does not
// reach and that could have matched something in this batch. It never fails the
// preview.
func (d *DryRun) coverageGaps(ctx context.Context, targets []target) []diff.Unvalidated {
	list, err := d.client.Resource(validatingWebhookGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		// No permission, no such API, or a transient failure. Coverage
		// discovery is an extra honesty check, never a precondition — and it
		// stays silent rather than reporting a gap, because "we could not read
		// the webhook configurations" is not evidence that any webhook exists.
		return nil
	}
	var out []diff.Unvalidated
	for i := range list.Items {
		out = append(out, configGaps(&list.Items[i], targets)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

// configGaps inspects one ValidatingWebhookConfiguration.
func configGaps(cfg *unstructured.Unstructured, targets []target) []diff.Unvalidated {
	hooks, found, err := unstructured.NestedSlice(cfg.Object, "webhooks")
	if !found || err != nil {
		return nil
	}
	var out []diff.Unvalidated
	for _, raw := range hooks {
		h, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := h["name"].(string)
		if name == "" {
			continue
		}
		side, _ := h["sideEffects"].(string)
		if dryRunSafeSideEffects[side] {
			continue
		}
		if !webhookMatchesAny(h, targets) {
			continue
		}
		out = append(out, diff.Unvalidated{
			Resource: "ValidatingWebhookConfiguration/" + cfg.GetName() + " webhook " + name,
			InBatch:  false,
			Reason:   diff.ReasonWebhookExcludesDryRun,
			Message:  gapMessage(name, side, failurePolicyOf(h)),
		})
	}
	return out
}

// failurePolicyOf reads a webhook's failurePolicy, defaulting the way the API
// server does.
func failurePolicyOf(h map[string]any) string {
	if p, _ := h["failurePolicy"].(string); p != "" {
		return p
	}
	return "Fail"
}

// gapMessage states what is unknown and why, in the cluster's own terms. It
// distinguishes the two consequences of an unreachable webhook, because they
// are different risks: with failurePolicy Fail the deploy will error out, with
// Ignore it will go through unchecked.
func gapMessage(name, sideEffects, failurePolicy string) string {
	effects := sideEffects
	if effects == "" {
		effects = "unset"
	}
	msg := "admission webhook " + name + " declares sideEffects: " + effects +
		", so the API server does not run it for a dry-run request; preview cannot say whether it would reject this change"
	if failurePolicy == "Ignore" {
		return msg + " (its failurePolicy is Ignore, so the dry-run is skipped silently and the webhook still runs at apply time)"
	}
	return msg + " (its failurePolicy is " + failurePolicy + ", so a dry-run reaching it fails outright)"
}

// webhookMatchesAny reports whether any resource in this batch could be
// intercepted by the webhook, so a cluster full of webhooks for kinds kelson
// never touches produces no noise.
//
// The match is deliberately coarse: group, version and resource, with the
// wildcards the API allows. namespaceSelector and objectSelector are not
// evaluated — they need labels of objects that may not exist yet — so this
// over-reports rather than under-reports, which is the right direction for a
// warning about a gap in coverage.
func webhookMatchesAny(h map[string]any, targets []target) bool {
	rules, found, err := unstructured.NestedSlice(h, "rules")
	if err != nil {
		return false
	}
	if !found || len(rules) == 0 {
		// A webhook with no rules intercepts nothing.
		return false
	}
	for _, raw := range rules {
		r, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !matchesOperations(stringsOf(r["operations"])) {
			continue
		}
		groups := stringsOf(r["apiGroups"])
		versions := stringsOf(r["apiVersions"])
		resources := stringsOf(r["resources"])
		for _, t := range targets {
			gv := t.obj.GroupVersionKind().GroupVersion()
			if matchesOne(groups, gv.Group) &&
				matchesOne(versions, gv.Version) &&
				matchesResource(resources, t.mapping.Resource.Resource) {
				return true
			}
		}
	}
	return false
}

// matchesOperations reports whether a rule covers the operations a server-side
// apply performs — a create or an update, depending on whether the object is
// already there.
func matchesOperations(ops []string) bool {
	for _, op := range ops {
		switch op {
		case "*", "CREATE", "UPDATE":
			return true
		}
	}
	return false
}

func matchesOne(patterns []string, value string) bool {
	for _, p := range patterns {
		if p == "*" || p == value {
			return true
		}
	}
	return false
}

// matchesResource compares against a rule's resource patterns, which may name a
// subresource ("deployments/scale"); kelson only ever applies the main resource.
func matchesResource(patterns []string, resource string) bool {
	for _, p := range patterns {
		if p == "*" || p == "*/*" || p == resource || p == resource+"/*" {
			return true
		}
	}
	return false
}

func stringsOf(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, i := range items {
		if s, ok := i.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
