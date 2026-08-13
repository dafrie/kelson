package dryrun

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dafrie/kelson/internal/diff"
)

// Audit-mode findings (issue #45).
//
// An enforce-mode policy vetoes the request at dry-run time, and the rejection
// is caught by parse.go. An audit-mode policy does NOT veto the request — the
// dry-run succeeds — so its finding only exists in the engine's *policy report*
// (Kyverno writes a PolicyReport / ClusterPolicyReport CR). Reading those
// reports turns audit-mode findings into warnings rather than blockers, which
// is exactly the enforce-vs-audit distinction issue #45 demands.
//
// Only Kyverno ships a report shape we recognise today; the reader is tolerant
// (a cluster without Kyverno simply yields no audit findings) and structured so
// the Gatekeeper ConstraintPodStatus / VAP forbidden reasons can be added the
// same way.

// policyReportGVRs are the Kyverno report kinds.
var policyReportGVRs = []schema.GroupVersionResource{
	{Group: "kyverno.io", Version: "v1alpha2", Resource: "policyreports"},
	{Group: "kyverno.io", Version: "v1alpha2", Resource: "clusterpolicyreports"},
}

// readAuditReports reads audit-mode policy findings from the cluster's policy
// report CRs. It never fails the preview: a cluster without the CRDs (no
// Kyverno) returns nil.
func (d *DryRun) readAuditReports(ctx context.Context) []diff.PolicyViolation {
	var out []diff.PolicyViolation
	for _, gvr := range policyReportGVRs {
		list, err := d.client.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) {
			continue
		}
		if err != nil {
			// A broken list (RBAC, transient) must not fail the preview; audit
			// finding discovery is best-effort.
			continue
		}
		for i := range list.Items {
			out = append(out, policyReportViolations(&list.Items[i])...)
		}
	}
	return out
}

// policyReportViolations extracts failing results from one Kyverno report.
// A report names the policy, rule, affected resource and the engine's own
// message; every finding is EnforcementAudit because the request was not
// vetoed.
func policyReportViolations(report *unstructured.Unstructured) []diff.PolicyViolation {
	results, found, _ := unstructured.NestedSlice(report.Object, "results")
	if !found {
		return nil
	}
	var out []diff.PolicyViolation
	for _, raw := range results {
		r, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := r["result"].(string); s == "pass" {
			continue
		}
		policy, _ := r["policy"].(string)
		rule, _ := r["rule"].(string)
		if policy == "" {
			continue
		}
		msg, _ := r["message"].(string)
		resource := reportResource(r)
		out = append(out, diff.PolicyViolation{
			Code:        diff.CodeAuditFinding,
			Engine:      "kyverno",
			Policy:      policy,
			Rule:        rule,
			Resource:    resource,
			Message:     msg,
			Enforcement: diff.EnforcementAudit,
			Remediation: kyvernoRemediation(policy),
		})
	}
	return out
}

// reportResource extracts kind/namespace/name for the first affected resource.
func reportResource(r map[string]any) string {
	res, _ := r["resources"].([]any)
	if len(res) == 0 {
		return ""
	}
	ref, ok := res[0].(map[string]any)
	if !ok {
		return ""
	}
	kind, _ := ref["kind"].(string)
	ns, _ := ref["namespace"].(string)
	name, _ := ref["name"].(string)
	if ns != "" {
		return kind + "/" + ns + "/" + name
	}
	return kind + "/" + name
}
