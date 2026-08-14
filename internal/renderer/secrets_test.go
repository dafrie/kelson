package renderer

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
)

// secretRefFixture is the standard fixture with one secret reference on the
// web component, alongside the plain LOG_LEVEL it already carries.
func secretRefFixture() *model.Resolved {
	r := resolvedFixture()
	r.Components[0].Env["STRIPE_API_KEY"] = model.EnvValue{
		Secret: &model.SecretRef{Name: "payments", Key: "stripe-api-key"},
	}
	return r
}

// TestSecretReferenceRendersSecretKeyRef is the shape half of issue #82: a
// reference reaches the manifest as a reference.
func TestSecretReferenceRendersSecretKeyRef(t *testing.T) {
	ms, err := Render(secretRefFixture(), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	out := encodeOrFail(t, ms)
	for _, want := range []string{
		"name: STRIPE_API_KEY",
		"secretKeyRef:",
		"name: payments",
		"key: stripe-api-key",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}
	// The plain value beside it is untouched: a reference changes how one
	// variable is emitted and nothing else.
	if !strings.Contains(out, "value: info") {
		t.Errorf("plain env value should still render as value:\n%s", out)
	}
}

// TestRenderedOutputCarriesNoSecretValue is the guarantee of issue #82 stated
// as a test: nothing kelson renders contains a secret's value, and no path
// through the renderer can emit a Secret at all.
//
// It is asserted structurally rather than by scanning for credential-shaped
// strings, because that is what the guarantee actually is (ADR-0018): a
// reference carries a name and a key, and there is no field on any resource the
// renderer writes that could hold `data:` or `stringData:`.
func TestRenderedOutputCarriesNoSecretValue(t *testing.T) {
	r := secretRefFixture()
	r.Components[1].Env = map[string]model.EnvValue{
		"SMTP_PASSWORD": {Secret: &model.SecretRef{Name: "mail-relay", Key: "password"}},
	}
	r.Components[2].Env = map[string]model.EnvValue{
		"REPORT_TOKEN": {Secret: &model.SecretRef{Name: "reports", Key: "token"}},
	}
	ms, err := Render(r, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind == "Secret" {
			t.Fatalf("the renderer emitted a Secret (%s); there is no spec surface that should produce one", m.Name)
		}
	}
	out := encodeOrFail(t, ms)
	for _, forbidden := range []string{"stringData", "\ndata:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("rendered output contains %q, which is where a secret value would live:\n%s", forbidden, out)
		}
	}
	// Every reference that went in came out as a reference.
	if got := strings.Count(out, "secretKeyRef:"); got != 3 {
		t.Errorf("expected 3 secretKeyRefs, got %d:\n%s", got, out)
	}
}

// TestSecretBackendGate: only `cluster` renders. The other two backends are a
// structured refusal naming the issue that implements them, rather than a
// cluster-shaped render against a Secret nothing would populate.
func TestSecretBackendGate(t *testing.T) {
	for _, tc := range []struct {
		backend   model.SecretBackendType
		wantIssue string
	}{
		{model.SecretsExternalSecrets, "#80"},
		{model.SecretsSOPS, "#81"},
	} {
		t.Run(string(tc.backend), func(t *testing.T) {
			r := secretRefFixture()
			r.Environment.Secrets = model.SecretBackend{Backend: tc.backend}
			_, err := Render(r, gatewayProfile(), nil)
			if err == nil {
				t.Fatalf("backend %q must not render", tc.backend)
			}
			errs, ok := err.(Errors)
			if !ok || len(errs) != 1 {
				t.Fatalf("expected one structured render error, got %T: %v", err, err)
			}
			e := errs[0]
			if e.Code != ErrSecretBackendUnsupported {
				t.Errorf("code = %q, want %q", e.Code, ErrSecretBackendUnsupported)
			}
			if !strings.Contains(e.Remediation, tc.wantIssue) {
				t.Errorf("remediation must name where the work is tracked (%s), got %q", tc.wantIssue, e.Remediation)
			}
			if !strings.Contains(e.Remediation, "backend: cluster") {
				t.Errorf("remediation must name the backend that works today, got %q", e.Remediation)
			}
		})
	}
}

// TestSecretBackendClusterRenders pins the other half: the v0 backend, and an
// unset one, render exactly as they did before the gate existed.
func TestSecretBackendClusterRenders(t *testing.T) {
	for _, backend := range []model.SecretBackendType{"", model.SecretsCluster} {
		r := secretRefFixture()
		r.Environment.Secrets = model.SecretBackend{Backend: backend}
		if _, err := Render(r, gatewayProfile(), nil); err != nil {
			t.Errorf("backend %q must render: %v", backend, err)
		}
	}
}

func encodeOrFail(t *testing.T, ms []Manifest) string {
	t.Helper()
	out, err := Encode(ms)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	return string(out)
}
