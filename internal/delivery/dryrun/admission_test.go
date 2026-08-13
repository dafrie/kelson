package dryrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
)

// Admission rejections and the preview's honesty about its own reach (#45),
// exercised end to end through the L2 engine rather than at the classifier: a
// denial has to arrive as a blocked diff, and a webhook the dry-run cannot
// reach has to arrive as a coverage gap that blocks nothing.

// checkoutDeployment is the manifest these tests submit: the live Deployment
// with a bumped image, so there is a real change to report whatever the
// admission verdict turns out to be.
func checkoutDeployment(t *testing.T, image string) delivery.Manifest {
	t.Helper()
	return manifest(t, "apps/v1", "Deployment", "checkout", tNS,
		map[string]any{"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "checkout"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": image}},
				},
			},
		}})
}

// TestPreviewWebhookDenialIsABlocker is issue #45 for the webhook half: a
// validating webhook kelson has never heard of denies the dry-run, and the
// preview turns it into a blocked diff naming the webhook and quoting the
// server — not a generic apply failure, and not an error that toasts the
// preview.
func TestPreviewWebhookDenialIsABlocker(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	const detail = "every container must set a memory limit"
	c.handle = func(*unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "checkout",
			fmt.Errorf(`admission webhook "limits.example.com" denied the request: %s`, detail))
	}
	e := newEngine(t, c)

	out, err := e.Preview(context.Background(), set(checkoutDeployment(t, "nginx:1.21")))
	if err != nil {
		t.Fatalf("a webhook denial must be a finding, not a preview error: %v", err)
	}
	if !diff.Blocked(out) {
		t.Fatalf("preview is not blocked; violations=%+v unvalidated=%+v", out.Violations, out.Unvalidated)
	}
	if len(out.Violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", out.Violations)
	}
	v := out.Violations[0]
	if v.Code != diff.CodeWebhookDenied {
		t.Errorf("code = %q, want %q", v.Code, diff.CodeWebhookDenied)
	}
	if v.Engine != "validating-webhook" || v.Policy != "limits.example.com" {
		t.Errorf("violation = %+v, want the webhook named as the target", v)
	}
	if v.Message != detail {
		t.Errorf("message = %q, want the server's own words %q", v.Message, detail)
	}
	if !strings.Contains(v.Remediation, "limits.example.com") {
		t.Errorf("remediation must name the object to inspect: %q", v.Remediation)
	}
	if len(out.Unvalidated) != 0 {
		t.Errorf("a denial is a verdict, not a coverage gap: %+v", out.Unvalidated)
	}
	if out.Summary.MaxRisk != diff.RiskDisruptive {
		t.Errorf("maxRisk = %q, want disruptive (the write would be rejected)", out.Summary.MaxRisk)
	}
}

// TestPreviewDryRunUnsupportedWebhookIsUnvalidated is the honesty boundary: a
// webhook whose side effects keep it out of dry-run makes the API server refuse
// the request. Nothing rejected the Deployment, so this is a coverage gap — not
// a violation and not a blocker — and the rendered change is still reported,
// because what is missing is the admission verdict, not the diff.
func TestPreviewDryRunUnsupportedWebhookIsUnvalidated(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	c.handle = func(*unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return nil, apierrors.NewBadRequest(`admission webhook "sidecar.example.com" does not support dry run`)
	}
	e := newEngine(t, c)

	out, err := e.Preview(context.Background(), set(checkoutDeployment(t, "nginx:1.21")))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(out.Violations) != 0 {
		t.Fatalf("a webhook that never saw the object cannot have rejected it: %+v", out.Violations)
	}
	if len(out.Unvalidated) != 1 {
		t.Fatalf("unvalidated = %+v, want exactly one coverage gap", out.Unvalidated)
	}
	u := out.Unvalidated[0]
	if u.Reason != diff.ReasonDryRunUnsupported {
		t.Errorf("reason = %q, want %q", u.Reason, diff.ReasonDryRunUnsupported)
	}
	if u.Blocking() || diff.Blocked(out) {
		t.Error("a coverage gap must not block: nothing rejected anything")
	}
	if !strings.Contains(u.Message, "sidecar.example.com") {
		t.Errorf("message must name the webhook: %q", u.Message)
	}
	r := findResource(t, out, "Deployment")
	if r.Risk == diff.RiskDisruptive {
		t.Errorf("risk = %q; an unreachable webhook is not a rejected write", r.Risk)
	}
	var bumped bool
	for _, f := range r.Fields {
		if strings.Contains(f.Path, "image") {
			bumped = true
		}
	}
	if !bumped {
		t.Errorf("the rendered change must still be reported: %+v", r.Fields)
	}
}

// webhookConfig builds a ValidatingWebhookConfiguration for the coverage tests.
func webhookConfig(name string, hooks ...map[string]any) *unstructured.Unstructured {
	items := make([]any, 0, len(hooks))
	for _, h := range hooks {
		items = append(items, h)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingWebhookConfiguration",
		"metadata":   map[string]any{"name": name},
		"webhooks":   items,
	}}
}

func webhook(name, sideEffects, failurePolicy string, groups, resources []string) map[string]any {
	return map[string]any{
		"name":          name,
		"sideEffects":   sideEffects,
		"failurePolicy": failurePolicy,
		"rules": []any{map[string]any{
			"apiGroups":   anySlice(groups),
			"apiVersions": []any{"*"},
			"resources":   anySlice(resources),
			"operations":  []any{"CREATE", "UPDATE"},
		}},
	}
}

func anySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// TestPreviewReportsWebhooksTheDryRunCannotReach: a dry-run runs the cluster's
// validating webhooks, but not one whose sideEffects keep it out — and with
// failurePolicy Ignore the API server skips it in silence, so the dry-run
// succeeds and the preview would otherwise read as a complete verdict. The
// preview says what it could not check, and stays quiet about webhooks that do
// run under dry-run and about webhooks that could not match this batch at all.
func TestPreviewReportsWebhooksTheDryRunCannotReach(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	c.seedLive(webhookConfig("platform-guards",
		webhook("quota.example.com", "Some", "Ignore", []string{"apps"}, []string{"deployments"}),
		webhook("labels.example.com", "None", "Fail", []string{"apps"}, []string{"deployments"}),
	))
	c.seedLive(webhookConfig("secret-guards",
		webhook("secrets.example.com", "Some", "Fail", []string{""}, []string{"secrets"}),
	))
	e := newEngine(t, c)

	out, err := e.Preview(context.Background(), set(checkoutDeployment(t, "nginx:1.21")))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(out.Unvalidated) != 1 {
		t.Fatalf("unvalidated = %+v, want only the unreachable webhook that matches this batch", out.Unvalidated)
	}
	u := out.Unvalidated[0]
	if u.Reason != diff.ReasonWebhookExcludesDryRun {
		t.Errorf("reason = %q, want %q", u.Reason, diff.ReasonWebhookExcludesDryRun)
	}
	if !strings.Contains(u.Resource, "platform-guards") || !strings.Contains(u.Resource, "quota.example.com") {
		t.Errorf("the gap must name the configuration and the webhook: %q", u.Resource)
	}
	if !strings.Contains(u.Message, "Ignore") {
		t.Errorf("message must say the dry-run is skipped silently: %q", u.Message)
	}
	if diff.Blocked(out) {
		t.Error("a coverage gap must not block a pipeline: nothing was rejected")
	}
	if len(out.Violations) != 0 {
		t.Errorf("a webhook that was never called has found nothing: %+v", out.Violations)
	}
}

// TestPreviewIgnoresWebhooksItCannotBeAffectedBy keeps the coverage report from
// becoming noise: a cluster full of webhooks for kinds this batch does not
// touch produces no gaps at all.
func TestPreviewIgnoresWebhooksItCannotBeAffectedBy(t *testing.T) {
	c := newCluster()
	c.seedLive(liveDeployment())
	c.seedLive(webhookConfig("unrelated",
		webhook("cronjobs.example.com", "Unknown", "Fail", []string{"batch"}, []string{"cronjobs"}),
	))
	e := newEngine(t, c)

	out, err := e.Preview(context.Background(), set(checkoutDeployment(t, "nginx:1.21")))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(out.Unvalidated) != 0 {
		t.Fatalf("unvalidated = %+v, want none: no webhook here can see a Deployment", out.Unvalidated)
	}
}

// TestCoverageReadsAreGranted keeps the shipped ClusterRole and the reads this
// package issues from drifting apart. The coverage probe fails silently by
// design, so a missing grant would not break a test — it would quietly turn the
// honesty check off, which is the failure mode worth pinning.
func TestCoverageReadsAreGranted(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "rbac", "detect-clusterrole.yaml")
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var role struct {
		Rules []struct {
			APIGroups []string `yaml:"apiGroups"`
			Resources []string `yaml:"resources"`
			Verbs     []string `yaml:"verbs"`
		} `yaml:"rules"`
	}
	if err := sigsyaml.Unmarshal(data, &role); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	want := map[string]string{
		validatingWebhookGVR.Resource: validatingWebhookGVR.Group,
		"policyreports":               policyReportGVRs[0].Group,
		"clusterpolicyreports":        policyReportGVRs[1].Group,
	}
	for resource, group := range want {
		var granted bool
		for _, r := range role.Rules {
			if !slices.Contains(r.APIGroups, group) || !slices.Contains(r.Resources, resource) {
				continue
			}
			if slices.Contains(r.Verbs, "list") {
				granted = true
			}
		}
		if !granted {
			t.Errorf("the preview lists %s.%s but the ClusterRole does not grant it", resource, group)
		}
	}
}
