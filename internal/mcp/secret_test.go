package mcp

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// set_secret is the one tool that receives a credential (issue #116), so the
// assertion running through this file is the one that matters most: the value
// reached the server and appeared in no tool answer.

// toolSecretValue is the value every test here writes.
const toolSecretValue = "sk-live-TOOL-SENTINEL-6b3d"

// secretServer stages a SecretService that records what it was sent and answers
// the way the real one does: keys back, never values.
func secretServer() (*fakeServer, *[]*kelsonv1alpha1.SetSecretRequest) {
	var seen []*kelsonv1alpha1.SetSecretRequest
	fake := &fakeServer{}
	fake.setSecret = func(req *kelsonv1alpha1.SetSecretRequest) (*kelsonv1alpha1.SetSecretResponse, error) {
		seen = append(seen, req)
		written := make([]string, 0, len(req.GetValues()))
		for k := range req.GetValues() {
			written = append(written, k)
		}
		dry := req.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_NONE
		summary := &kelsonv1alpha1.SecretSummary{
			Name:      req.GetName(),
			Namespace: req.GetTarget().GetProject() + "-" + req.GetTarget().GetEnvironment(),
		}
		if !dry {
			summary.Keys = append(append([]string{}, written...), "already-there")
		}
		return &kelsonv1alpha1.SetSecretResponse{Secret: summary, WrittenKeys: written, DryRun: dry}, nil
	}
	return fake, &seen
}

// TestSetSecretPreviewsByDefault: an agent that calls the tool without asking
// for a write gets a validation and nothing else. Every mutating tool on this
// surface defaults to the preview (ADR-0008 §3), and a credential is the last
// thing that should be the exception.
func TestSetSecretPreviewsByDefault(t *testing.T) {
	fake, seen := secretServer()
	h := start(t, fake)

	out := h.call(t, "set_secret", map[string]any{
		"project":     "checkout",
		"environment": "production",
		"name":        "payments",
		"values":      map[string]any{"api-key": toolSecretValue},
	})
	if len(*seen) != 1 {
		t.Fatalf("the server saw %d calls, want 1", len(*seen))
	}
	if got := (*seen)[0].GetDryRun(); got != kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		t.Errorf("dry_run = %v, want RENDER by default", got)
	}
	if got := (*seen)[0].GetValues()["api-key"]; got != toolSecretValue {
		t.Errorf("the value did not reach the server: %q", got)
	}
	for _, want := range []string{"VALIDATED ONLY", "nothing was written", "execute=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer should say %q:\n%s", want, out)
		}
	}
	assertNoToolSecret(t, out)
}

// TestSetSecretExecutesAndShowsTheReferenceForm: the write, plus the one thing
// an agent needs next — how to spell the reference in the spec.
func TestSetSecretExecutesAndShowsTheReferenceForm(t *testing.T) {
	fake, seen := secretServer()
	h := start(t, fake)

	out := h.call(t, "set_secret", map[string]any{
		"project":     "checkout",
		"environment": "production",
		"name":        "payments",
		"values":      map[string]any{"api-key": toolSecretValue},
		"execute":     true,
	})
	if got := (*seen)[0].GetDryRun(); got != kelsonv1alpha1.DryRun_DRY_RUN_NONE {
		t.Errorf("dry_run = %v, want NONE with execute=true", got)
	}
	for _, want := range []string{
		"WRITTEN",
		"checkout-production",
		"{ secret: payments, key: api-key }",
		"already-there",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer should contain %q:\n%s", want, out)
		}
	}
	assertNoToolSecret(t, out)
}

// TestSetSecretPassesTheNamespaceOverride.
func TestSetSecretPassesTheNamespaceOverride(t *testing.T) {
	fake, seen := secretServer()
	h := start(t, fake)

	h.call(t, "set_secret", map[string]any{
		"project":     "checkout",
		"environment": "production",
		"namespace":   "shop-live",
		"name":        "payments",
		"values":      map[string]any{"api-key": toolSecretValue},
		"execute":     true,
	})
	if got := (*seen)[0].GetTarget().GetNamespace(); got != "shop-live" {
		t.Errorf("namespace = %q, want the override", got)
	}
}

// TestSetSecretRelaysTheServersRefusalWithoutTheValue: a `secret/*` refusal
// reaches the agent with its code and remediation intact, and the value the
// call carried is in none of it.
func TestSetSecretRelaysTheServersRefusalWithoutTheValue(t *testing.T) {
	fake := &fakeServer{}
	fake.setSecret = func(*kelsonv1alpha1.SetSecretRequest) (*kelsonv1alpha1.SetSecretResponse, error) {
		cerr := connect.NewError(connect.CodeFailedPrecondition,
			errorf("Secret \"payments\" exists but is not managed by kelson"))
		detail, derr := connect.NewErrorDetail(&kelsonv1alpha1.Error{
			Code:        "secret/not-managed",
			Resource:    "Secret/checkout-production/payments",
			Message:     "not managed by kelson",
			Remediation: "use a different name, or adopt it deliberately",
		})
		if derr == nil {
			cerr.AddDetail(detail)
		}
		return nil, cerr
	}
	h := start(t, fake)

	out := h.callErr(t, "set_secret", map[string]any{
		"project":     "checkout",
		"environment": "production",
		"name":        "payments",
		"values":      map[string]any{"api-key": toolSecretValue},
		"execute":     true,
	})
	for _, want := range []string{"secret/not-managed", "adopt it deliberately"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal should carry %q:\n%s", want, out)
		}
	}
	assertNoToolSecret(t, out)
}

// TestSetSecretIsDeclaredDestructive: overwriting a key changes what the next
// pod reads and kelson keeps no previous value to restore, which is exactly
// what the destructive hint is for.
func TestSetSecretIsDeclaredDestructive(t *testing.T) {
	for _, tool := range surface(&clients{}) {
		if tool.def.Name != "set_secret" {
			continue
		}
		if tool.def.Annotations.DestructiveHint == nil || !*tool.def.Annotations.DestructiveHint {
			t.Error("set_secret is not marked destructive: there is no undo for an overwritten credential")
		}
		if !strings.Contains(tool.def.Description, "MUTATES") {
			t.Error("set_secret's description does not say it mutates")
		}
		return
	}
	t.Fatal("set_secret is not in the surface")
}

// TestDiagnoseReportsManagedSecretsByKey is the listing half of #116 on this
// surface. It is a section of the diagnosis rather than a tool of its own
// because the failure it explains — a reference to a Secret nobody wrote — is a
// diagnosis, and it must show keys and never values.
func TestDiagnoseReportsManagedSecretsByKey(t *testing.T) {
	fake := &fakeServer{}
	fake.status = func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
		return &kelsonv1alpha1.StatusResponse{Phase: "Live", Namespace: "checkout-production"}, nil
	}
	fake.listSecrets = func(req *kelsonv1alpha1.ListSecretsRequest) (*kelsonv1alpha1.ListSecretsResponse, error) {
		if req.GetTarget().GetProject() != "checkout" || req.GetTarget().GetEnvironment() != "production" {
			t.Errorf("the diagnosis asked about %+v", req.GetTarget())
		}
		return &kelsonv1alpha1.ListSecretsResponse{
			Namespace: "checkout-production",
			Secrets: []*kelsonv1alpha1.SecretSummary{
				{Name: "checkout-db", Namespace: "checkout-production", Keys: []string{"url"}},
				{Name: "payments", Namespace: "checkout-production", Keys: []string{"api-key", "webhook"}},
			},
		}, nil
	}
	h := start(t, fake)

	out := h.call(t, "diagnose_application", map[string]any{"project": "checkout", "environment": "production"})
	for _, want := range []string{"SECRETS", "checkout-db", "url", "payments", "api-key", "webhook"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnosis should report %q:\n%s", want, out)
		}
	}
	assertNoToolSecret(t, out)
}

// TestDiagnoseSaysWhenTheNamespaceHoldsNoManagedSecrets: an empty listing is
// itself a diagnosis when a workload is in CreateContainerConfigError, so it
// must be a stated fact rather than a missing section.
func TestDiagnoseSaysWhenTheNamespaceHoldsNoManagedSecrets(t *testing.T) {
	fake := &fakeServer{}
	fake.status = func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
		return &kelsonv1alpha1.StatusResponse{Phase: "Live", Namespace: "checkout-production"}, nil
	}
	fake.listSecrets = func(*kelsonv1alpha1.ListSecretsRequest) (*kelsonv1alpha1.ListSecretsResponse, error) {
		return &kelsonv1alpha1.ListSecretsResponse{Namespace: "checkout-production"}, nil
	}
	h := start(t, fake)

	out := h.call(t, "diagnose_application", map[string]any{"project": "checkout", "environment": "production"})
	if !strings.Contains(out, "CreateContainerConfigError") || !strings.Contains(out, "set_secret") {
		t.Errorf("an empty listing should name the failure it causes and the tool that fixes it:\n%s", out)
	}
}

// TestDiagnoseSurvivesAServerWithoutTheSecretBackend: every section after
// status is additive, and a server started without a cluster secret backend
// must degrade to a line rather than take the diagnosis with it.
func TestDiagnoseSurvivesAServerWithoutTheSecretBackend(t *testing.T) {
	fake := &fakeServer{}
	fake.status = func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
		return &kelsonv1alpha1.StatusResponse{Phase: "Live", Namespace: "checkout-production"}, nil
	}
	h := start(t, fake)

	out := h.call(t, "diagnose_application", map[string]any{"project": "checkout", "environment": "production"})
	if !strings.Contains(out, "SECRETS") || !strings.Contains(out, "unavailable") {
		t.Errorf("the section should report its own gap:\n%s", out)
	}
	if !strings.Contains(out, "STATUS") {
		t.Errorf("the diagnosis was lost with the section:\n%s", out)
	}
}

// TestNoToolDeletesASecret: DeleteSecret is a real API operation and
// deliberately has no tool. An agent deleting a credential it cannot read and
// kelson cannot restore is not a task this surface offers (ADR-0008); the CLI
// and the API have it, with a confirmation and a human behind it.
func TestNoToolDeletesASecret(t *testing.T) {
	for _, tool := range surface(&clients{}) {
		for _, r := range tool.rpcs {
			if r.method == "DeleteSecret" {
				t.Errorf("tool %q composes %s: deleting an unrecoverable credential is not an agent task", tool.def.Name, r)
			}
		}
	}
}

func assertNoToolSecret(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, toolSecretValue) {
		t.Errorf("the value reached the tool answer:\n%s", out)
	}
}

// errorf keeps the fake's error construction to one line without pulling fmt
// into every test that stages a refusal.
func errorf(message string) error { return &staticError{message} }

type staticError struct{ message string }

func (e *staticError) Error() string { return e.message }
