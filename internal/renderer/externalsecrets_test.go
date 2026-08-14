package renderer

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// esProfile is gatewayProfile plus an external-secrets installation with the
// stores the case under test needs. Namespaced stores are given the fixture's
// own namespace unless a case overrides it — a SecretStore elsewhere is not a
// candidate, and several tests turn on exactly that.
func esProfile(namespaced []clusterprofile.SecretStore, cluster []string) clusterprofile.ClusterProfile {
	p := gatewayProfile()
	p.ExternalSecrets = &clusterprofile.ExternalSecrets{
		Version:             "0.20.0",
		SecretStores:        namespaced,
		ClusterSecretStores: cluster,
	}
	return p
}

// esFixture is secretRefFixture on the externalSecrets backend, with the
// defaults resolution would have applied already filled in.
func esFixture(store string) *model.Resolved {
	r := secretRefFixture()
	r.Environment.Secrets = model.SecretBackend{
		Backend:         model.SecretsExternalSecrets,
		Store:           store,
		RefreshInterval: model.DefaultSecretRefreshInterval,
	}
	return r
}

// renderErrors renders and demands a structured failure, returning it.
func renderErrors(t *testing.T, r *model.Resolved, p clusterprofile.ClusterProfile) Errors {
	t.Helper()
	_, err := Render(r, p, nil)
	if err == nil {
		t.Fatalf("render must fail")
	}
	errs, ok := err.(Errors)
	if !ok {
		t.Fatalf("expected structured render errors, got %T: %v", err, err)
	}
	return errs
}

// TestExternalSecretsRendersOnePerSecret: one ExternalSecret per Secret *name*,
// carrying one `data` entry per key something in the environment reads, and the
// workload's own reference unchanged.
//
// The count is the assertion that matters. Two variables reading two keys of
// one Secret are one remote secret with two properties; rendering two
// ExternalSecrets against one target would have them fight over the Secret they
// both claim to own.
func TestExternalSecretsRendersOnePerSecret(t *testing.T) {
	r := esFixture("vault-backend")
	r.Components[0].Env["SESSION_PEPPER"] = model.EnvValue{
		Secret: &model.SecretRef{Name: "payments", Key: "session-pepper"},
	}
	r.Components[1].Env = map[string]model.EnvValue{
		"SMTP_PASSWORD": {Secret: &model.SecretRef{Name: "mail-relay", Key: "password"}},
	}

	ms, err := Render(r, esProfile(nil, []string{"vault-backend"}), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	var names []string
	for _, m := range ms {
		if m.Kind == "ExternalSecret" {
			names = append(names, m.Name)
			if m.APIVersion != externalSecretAPIVersion {
				t.Errorf("apiVersion = %q, want %q", m.APIVersion, externalSecretAPIVersion)
			}
			if m.Namespace != r.Environment.Namespace {
				t.Errorf("%s namespace = %q, want the environment's", m.Name, m.Namespace)
			}
		}
	}
	if want := []string{"mail-relay", "payments"}; !equalStrings(names, want) {
		t.Fatalf("ExternalSecrets = %v, want %v (one per Secret name, sorted)", names, want)
	}

	out := encodeOrFail(t, ms)
	// Both keys of `payments` on the one resource, addressed as name/property.
	for _, want := range []string{
		"property: stripe-api-key",
		"property: session-pepper",
		"kind: ClusterSecretStore",
		"name: vault-backend",
		"refreshInterval: 1h",
		"creationPolicy: Owner",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}
	// The workload half of ADR-0018 is untouched: the backend changes what
	// populates the Secret, never how a variable reads it.
	if !strings.Contains(out, "secretKeyRef:") {
		t.Errorf("references must still render as secretKeyRef:\n%s", out)
	}
}

// TestExternalSecretsOrderedBeforeWorkloads: a rendered set expresses
// sequencing only through order (issue #89), so the resources that populate a
// Secret must precede everything that reads one.
func TestExternalSecretsOrderedBeforeWorkloads(t *testing.T) {
	ms, err := Render(esFixture("vault-backend"), esProfile(nil, []string{"vault-backend"}), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	lastES, firstWorkload := -1, len(ms)
	for i, m := range ms {
		switch m.Kind {
		case "ExternalSecret":
			lastES = i
		case "Deployment", "CronJob":
			if i < firstWorkload {
				firstWorkload = i
			}
		}
	}
	if lastES == -1 {
		t.Fatalf("no ExternalSecret rendered")
	}
	if lastES > firstWorkload {
		t.Errorf("ExternalSecret at %d follows the first workload at %d", lastES, firstWorkload)
	}
	if ms[0].Kind != "Namespace" {
		t.Errorf("the Namespace must still lead the set, got %s", ms[0].Kind)
	}
}

// TestExternalSecretsCarryNoValue: the guarantee of issue #82 restated for this
// backend. An ExternalSecret carries addresses — a store, a remote key, a
// property — and there is no field on it a value could be placed in.
func TestExternalSecretsCarryNoValue(t *testing.T) {
	ms, err := Render(esFixture("vault-backend"), esProfile(nil, []string{"vault-backend"}), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind == "Secret" {
			t.Fatalf("the renderer emitted a Secret (%s)", m.Name)
		}
	}
	out := encodeOrFail(t, ms)
	for _, forbidden := range []string{"stringData", "\ndata:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("rendered output contains %q, which is where a value would live:\n%s", forbidden, out)
		}
	}
}

// TestExternalSecretsSkipsOperatorGeneratedSecrets: a `{from: {service, key}}`
// binding names a Secret the component's own operator writes, so no
// ExternalSecret is rendered for it — two writers for one Secret is a fight,
// not a redundancy. A data component's `auth:` is the opposite case and does
// get one: nothing else populates it under this backend.
func TestExternalSecretsSkipsOperatorGeneratedSecrets(t *testing.T) {
	r := esFixture("vault-backend")
	r.DataServices = []model.ResolvedDataService{{
		Name:   "cache",
		Kind:   model.ComponentValkey,
		Preset: model.PresetSmall,
		Auth:   &model.SecretRef{Name: "cache-auth", Key: "password"},
	}}
	r.Components[0].Env["CACHE_URL"] = model.EnvValue{
		From: &model.ServiceBinding{Service: "cache", Key: "uri"},
	}
	p := esProfile(nil, []string{"vault-backend"})
	p.Valkey = &clusterprofile.ValkeyOperator{
		Version: "0.5.0",
		CRDs:    []string{"valkeyclusters", "valkeynodes"},
	}

	ms, err := Render(r, p, nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	var names []string
	for _, m := range ms {
		if m.Kind == "ExternalSecret" {
			names = append(names, m.Name)
		}
	}
	if want := []string{"cache-auth", "payments"}; !equalStrings(names, want) {
		t.Fatalf("ExternalSecrets = %v, want %v — auth yes, the operator's own credentials no", names, want)
	}
}

// TestExternalSecretsRequiresTheOperator is the capability gate: the backend
// delegates everything to a controller, and only a definite No refuses. A Gap
// on the field means the probe could not look, and an Unknown must never be
// reported as a finding (issue #144).
func TestExternalSecretsRequiresTheOperator(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		errs := renderErrors(t, esFixture("vault-backend"), gatewayProfile())
		if errs[0].Code != ErrExternalSecretsNotInstalled {
			t.Fatalf("code = %q, want %q", errs[0].Code, ErrExternalSecretsNotInstalled)
		}
		if !strings.Contains(errs[0].Remediation, "backend: cluster") {
			t.Errorf("remediation must name the backend that needs no operator, got %q", errs[0].Remediation)
		}
	})
	t.Run("unreadable renders", func(t *testing.T) {
		p := gatewayProfile()
		p.Incomplete = []clusterprofile.Gap{{
			Field:  "externalSecrets",
			Reason: "rbac: get /apis denied",
		}}
		// A store cannot be resolved either, so this still fails — but on the
		// store, not on the operator. What matters is that "we could not look"
		// never becomes "it is not installed".
		errs := renderErrors(t, esFixture("vault-backend"), p)
		if errs[0].Code == ErrExternalSecretsNotInstalled {
			t.Fatalf("a gapped profile must not read as a definite absence: %v", errs[0])
		}
	})
}

// TestExternalSecretsStoreResolution covers every way a store name does and
// does not resolve. The refusals are the design: a wrong store is a workload
// reading a credential from somewhere nobody intended, so kelson stops instead
// of picking.
func TestExternalSecretsStoreResolution(t *testing.T) {
	const ns = "checkout-prod"
	here := func(names ...string) []clusterprofile.SecretStore {
		out := make([]clusterprofile.SecretStore, 0, len(names))
		for _, n := range names {
			out = append(out, clusterprofile.SecretStore{Name: n, Namespace: ns})
		}
		return out
	}

	for _, tc := range []struct {
		name       string
		store      string
		namespaced []clusterprofile.SecretStore
		cluster    []string
		wantCode   string
		wantKind   string
		wantName   string
	}{
		{
			name:     "named ClusterSecretStore",
			store:    "vault-backend",
			cluster:  []string{"aws-backend", "vault-backend"},
			wantKind: storeKindCluster,
			wantName: "vault-backend",
		},
		{
			name:       "named SecretStore in this namespace",
			store:      "team-vault",
			namespaced: here("team-vault"),
			cluster:    []string{"vault-backend"},
			wantKind:   storeKindNamespaced,
			wantName:   "team-vault",
		},
		{
			name:       "unnamed with exactly one store",
			namespaced: here("team-vault"),
			wantKind:   storeKindNamespaced,
			wantName:   "team-vault",
		},
		{
			name:     "unnamed with exactly one ClusterSecretStore",
			cluster:  []string{"vault-backend"},
			wantKind: storeKindCluster,
			wantName: "vault-backend",
		},
		{
			name:       "unnamed with several is refused",
			namespaced: here("team-vault"),
			cluster:    []string{"vault-backend"},
			wantCode:   ErrExternalSecretsStoreAmbiguous,
		},
		{
			name:       "a name matching both kinds is refused",
			store:      "vault",
			namespaced: here("vault"),
			cluster:    []string{"vault"},
			wantCode:   ErrExternalSecretsStoreAmbiguous,
		},
		{
			name:     "a name the cluster does not have is refused",
			store:    "vault-backend",
			cluster:  []string{"aws-backend"},
			wantCode: ErrExternalSecretsStoreUnknown,
		},
		{
			name:     "no store at all is refused",
			wantCode: ErrExternalSecretsStoreUnknown,
		},
		{
			// A SecretStore is readable only from its own namespace, which is
			// why the profile records one: a bare name would make this render.
			name:       "a SecretStore in another namespace is not a candidate",
			store:      "team-vault",
			namespaced: []clusterprofile.SecretStore{{Name: "team-vault", Namespace: "other-team"}},
			wantCode:   ErrExternalSecretsStoreUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := esFixture(tc.store)
			p := esProfile(tc.namespaced, tc.cluster)
			if tc.wantCode != "" {
				errs := renderErrors(t, r, p)
				if errs[0].Code != tc.wantCode {
					t.Fatalf("code = %q, want %q (%s)", errs[0].Code, tc.wantCode, errs[0].Message)
				}
				if errs[0].Remediation == "" {
					t.Errorf("a refusal must state the fix: %+v", errs[0])
				}
				return
			}
			ms, err := Render(r, p, nil)
			if err != nil {
				t.Fatalf("Render failed: %v", err)
			}
			out := encodeOrFail(t, ms)
			if !strings.Contains(out, "kind: "+tc.wantKind) || !strings.Contains(out, "name: "+tc.wantName) {
				t.Errorf("want secretStoreRef %s/%s in:\n%s", tc.wantKind, tc.wantName, out)
			}
		})
	}
}

// TestExternalSecretsWithNoReferencesRendersNothing: selecting the backend
// before writing the first reference is not an error, and it must not be —
// otherwise an environment could never adopt the backend ahead of its specs.
// The store is not even resolved, so an environment with no references renders
// against a cluster with no store.
func TestExternalSecretsWithNoReferencesRendersNothing(t *testing.T) {
	r := resolvedFixture()
	r.Environment.Secrets = model.SecretBackend{
		Backend:         model.SecretsExternalSecrets,
		RefreshInterval: model.DefaultSecretRefreshInterval,
	}
	ms, err := Render(r, esProfile(nil, nil), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind == "ExternalSecret" {
			t.Fatalf("nothing references a Secret, so nothing should be synced; got %s", m.Name)
		}
	}
}

// TestExternalSecretsBackendIsAOneFieldMigration is ADR-0018's promise checked
// against ADR-0020's implementation: the same spec, rendered under both
// backends, produces the same workloads and differs only by the resources that
// populate the Secrets.
func TestExternalSecretsBackendIsAOneFieldMigration(t *testing.T) {
	cluster := secretRefFixture()
	cluster.Environment.Secrets = model.SecretBackend{Backend: model.SecretsCluster}
	clusterMs, err := Render(cluster, esProfile(nil, []string{"vault-backend"}), nil)
	if err != nil {
		t.Fatalf("Render (cluster) failed: %v", err)
	}
	esMs, err := Render(esFixture("vault-backend"), esProfile(nil, []string{"vault-backend"}), nil)
	if err != nil {
		t.Fatalf("Render (externalSecrets) failed: %v", err)
	}

	strip := func(ms []Manifest) []string {
		var out []string
		for _, m := range ms {
			if m.Kind == "ExternalSecret" {
				continue
			}
			b, err := m.YAML()
			if err != nil {
				t.Fatalf("YAML: %v", err)
			}
			out = append(out, string(b))
		}
		return out
	}
	if !equalStrings(strip(clusterMs), strip(esMs)) {
		t.Errorf("changing the backend changed something other than the ExternalSecrets; the spec text is\n" +
			"supposed to be backend-agnostic (ADR-0018 §2)")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
