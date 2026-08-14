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

// TestSecretBackendGate: all three backends have a mechanism since ADR-0021,
// so the only thing left to refuse by name is a backend that is not one of
// them. The refusal lists what is.
func TestSecretBackendGate(t *testing.T) {
	r := secretRefFixture()
	r.Environment.Secrets = model.SecretBackend{Backend: "vault"}
	_, err := Render(r, gatewayProfile(), nil)
	if err == nil {
		t.Fatalf("an unknown backend must not render")
	}
	errs, ok := err.(Errors)
	if !ok || len(errs) != 1 {
		t.Fatalf("expected one structured render error, got %T: %v", err, err)
	}
	e := errs[0]
	if e.Code != ErrSecretBackendUnsupported {
		t.Errorf("code = %q, want %q", e.Code, ErrSecretBackendUnsupported)
	}
	for _, backend := range []string{"cluster", "externalSecrets", "sops"} {
		if !strings.Contains(e.Remediation, backend) {
			t.Errorf("remediation must list %q, got %q", backend, e.Remediation)
		}
	}
}

// sopsFixture is the reference-carrying fixture switched to the sops backend
// in the delivery mode that backend requires.
func sopsFixture() *model.Resolved {
	r := secretRefFixture()
	r.Environment.Mode = model.DeliveryFlux
	r.Environment.Delivery = model.Delivery{
		Mode: model.DeliveryFlux,
		Git:  &model.GitTarget{Repo: "https://example.test/deploy.git", Branch: "main", Path: "clusters/prod"},
	}
	r.Environment.Secrets = model.SecretBackend{
		Backend:       model.SecretsSOPS,
		AgeRecipients: []string{"age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw"},
		AgeKeySecret:  model.DefaultAgeKeySecret,
	}
	return r
}

// TestSOPSRendersTheClusterShape: the workload half of a sops render is
// byte-identical to the cluster backend's. ADR-0018 promised that switching
// backends is one field on one Environment, ADR-0020's test asserted it for
// externalSecrets, and this is the third and last backend it has to hold for.
func TestSOPSRendersTheClusterShape(t *testing.T) {
	sops := sopsFixture()
	cluster := sopsFixture()
	cluster.Environment.Secrets = model.SecretBackend{Backend: model.SecretsCluster}

	a, err := Render(sops, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("sops render: %v", err)
	}
	b, err := Render(cluster, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("cluster render: %v", err)
	}
	if encodeOrFail(t, a) != encodeOrFail(t, b) {
		t.Errorf("the sops render must differ from the cluster render by nothing:\n%s", encodeOrFail(t, a))
	}
	// And the guarantee that matters most: the backend that puts a Secret in
	// Git still does not put one in the rendered set. The encrypted file is
	// written by `kelson secret set`, outside the renderer, which has no
	// plaintext and no way to acquire one.
	for _, m := range a {
		if m.Kind == "Secret" {
			t.Fatalf("the sops backend must not make the renderer emit a Secret (%s)", m.Name)
		}
	}
}

// TestSOPSRequiresFlux: direct mode has no decryptor, so an encrypted file
// there would stay encrypted and every reference to it would fail at pod
// start. Same gate as charts and previews, decided from spec data alone.
func TestSOPSRequiresFlux(t *testing.T) {
	r := sopsFixture()
	r.Environment.Mode = model.DeliveryDirect
	_, err := Render(r, gatewayProfile(), nil)
	if err == nil {
		t.Fatalf("backend sops must not render in direct mode")
	}
	errs, ok := err.(Errors)
	if !ok || len(errs) != 1 || errs[0].Code != ErrSOPSRequiresFlux {
		t.Fatalf("expected one %s, got %T: %v", ErrSOPSRequiresFlux, err, err)
	}
	if !strings.Contains(errs[0].Remediation, "delivery.mode: flux") {
		t.Errorf("remediation must name the fix, got %q", errs[0].Remediation)
	}
}

// TestSOPSPreviewKustomizationDecrypts: a preview's artifact carries the same
// encrypted Secrets, so the Kustomization flux-operator instantiates per pull
// request needs the same decryption block. This is the only Kustomization
// kelson writes, and it is written through the function that also tells the
// operator what their own Kustomization needs.
func TestSOPSPreviewKustomizationDecrypts(t *testing.T) {
	r := sopsFixture()
	r.Environment.Previews = &model.ResolvedPreviews{
		Provider:  model.PreviewGitHub,
		Repo:      "https://github.com/acme/checkout",
		SecretRef: "forge",
		Interval:  "10m",
		Filter:    model.ResolvedPreviewFilter{Limit: 10},
		Artifacts: model.PreviewArtifacts{Repository: "oci://ghcr.io/acme/previews"},
	}
	if _, err := Render(r, gatewayProfile(), nil); err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	// Asserted against the template itself rather than the encoded ResourceSet:
	// the template is a literal block scalar, so the encoder re-indents it and
	// a substring match on the output would be a test of yaml.v3's indentation.
	template := previewResourcesTemplate(r, r.Environment.Previews)
	want := SOPSDecryptionBlock(model.DefaultAgeKeySecret, "  ")
	if !strings.Contains(template, want) {
		t.Errorf("the preview Kustomization must carry:\n%s\ngot:\n%s", want, template)
	}

	// And under the cluster backend it carries nothing of the sort: a
	// decryption block referencing a Secret nobody created would make every
	// preview fail to build.
	r.Environment.Secrets = model.SecretBackend{Backend: model.SecretsCluster}
	ms, err := Render(r, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if strings.Contains(encodeOrFail(t, ms), "decryption:") {
		t.Errorf("only the sops backend may emit a decryption block")
	}
}

// TestSOPSDecryptionBlockDefaults: an empty key-Secret name is the default
// rather than an empty secretRef, which would be a Kustomization that fails to
// build against a Secret called "".
func TestSOPSDecryptionBlockDefaults(t *testing.T) {
	if got := SOPSDecryptionBlock("", "  "); !strings.Contains(got, "name: "+model.DefaultAgeKeySecret) {
		t.Errorf("an empty name must default, got:\n%s", got)
	}
	if got := SOPSDecryptionBlock("prod-age", ""); got != "decryption:\n  provider: sops\n  secretRef:\n    name: prod-age\n" {
		t.Errorf("unexpected block:\n%s", got)
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
