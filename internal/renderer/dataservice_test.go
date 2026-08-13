package renderer

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// Data-service rendering (issue #89). The shapes themselves are pinned by the
// golden fixtures under testdata/render; these tests pin the decisions a
// fixture cannot show — the refusals, the capability tri-state, the ordering
// and the secret-key mapping.

func renderErrorCode(t *testing.T, err error) string {
	t.Helper()
	errs, ok := err.(Errors)
	if !ok {
		t.Fatalf("expected renderer.Errors, got %#v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("expected exactly one error, got %v", errs)
	}
	return errs[0].Code
}

// TestServicesRenderBeforeApplications: a workload must not be applied ahead of
// the resource that produces the credentials it references.
func TestServicesRenderBeforeApplications(t *testing.T) {
	ms, err := Render(boundFixture(model.PresetHASmall), cnpgProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := kinds(ms)
	if got[0] != "Namespace/checkout-prod" {
		t.Fatalf("Namespace must lead the set, got %q", got[0])
	}
	if got[1] != "Cluster/checkout-production-db" {
		t.Fatalf("the service CR must follow the Namespace, got %v", got)
	}
	for _, m := range ms[2:] {
		if m.Kind == "Cluster" || m.Kind == "Database" {
			t.Fatalf("a service resource sorted after an application resource: %v", got)
		}
	}
}

// TestDedicatedPresetSizing pins the sizing table of docs/data-services.md to
// the rendered output, so a silent edit of one of the numbers fails here as
// well as in the goldens.
func TestDedicatedPresetSizing(t *testing.T) {
	for _, tc := range []struct {
		preset   model.ServicePreset
		contains []string
		absent   []string
	}{
		{
			preset:   model.PresetSmall,
			contains: []string{"instances: 1", "size: 5Gi", "cpu: 500m", "memory: 1Gi"},
			absent:   []string{"minSyncReplicas", "maxSyncReplicas"},
		},
		{
			preset:   model.PresetHASmall,
			contains: []string{"instances: 3", "minSyncReplicas: 1", "maxSyncReplicas: 1", "size: 5Gi", "memory: 1Gi"},
		},
		{
			preset:   model.PresetHAMedium,
			contains: []string{"instances: 3", "minSyncReplicas: 1", "size: 20Gi", "cpu: \"2\"", "memory: 4Gi"},
		},
	} {
		t.Run(string(tc.preset), func(t *testing.T) {
			ms, err := Render(boundFixture(tc.preset), cnpgProfile(), nil)
			if err != nil {
				t.Fatalf("Render failed: %v", err)
			}
			out, err := Encode(ms)
			if err != nil {
				t.Fatalf("Encode failed: %v", err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(string(out), want) {
					t.Errorf("rendered Cluster missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(string(out), unwanted) {
					t.Errorf("a single-instance cluster must not claim synchronous replication (%q):\n%s", unwanted, out)
				}
			}
		})
	}
}

// TestNotImplementedServices: what the model accepts and the renderer does not
// render yet must say so, and say where it is tracked. `shared` (#93) is a
// deferral rather than something never built: it rendered a Database CR until
// the owner decided dedicated-per-component is the model for now (2026-08-13)
// — see git history for the removed rendering path.
func TestNotImplementedServices(t *testing.T) {
	for _, tc := range []struct {
		name  string
		svc   model.ResolvedDataService
		issue string
	}{
		{"valkey", model.ResolvedDataService{Name: "cache", Kind: model.ComponentValkey, Preset: model.PresetSmall}, "#98"},
		{"branch", model.ResolvedDataService{Name: "db", Kind: model.ComponentPostgres, Preset: model.PresetBranch}, "#99"},
		{"shared", model.ResolvedDataService{Name: "db", Kind: model.ComponentPostgres, Preset: model.PresetShared}, "#93"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolvedFixture()
			resolved.DataServices = []model.ResolvedDataService{tc.svc}
			_, err := Render(resolved, cnpgProfile(), nil)
			if err == nil {
				t.Fatalf("%s must not render", tc.name)
			}
			if code := renderErrorCode(t, err); code != ErrServiceNotImplemented {
				t.Fatalf("code = %q, want %q", code, ErrServiceNotImplemented)
			}
			if !strings.Contains(err.Error(), tc.issue) {
				t.Errorf("error must name %s: %v", tc.issue, err)
			}
		})
	}
}

// TestPresetCapabilityTriState is the load-bearing decision of #89: only a
// definite No refuses. Unknown means nobody could look, and refusing on it
// would turn every hand-written profile into a refusal (issue #144).
func TestPresetCapabilityTriState(t *testing.T) {
	yes := cnpgProfile()

	no := gatewayProfile() // CloudNativePG absent: checked, and found wanting.

	tooOld := gatewayProfile()
	tooOld.CloudNativePG = &clusterprofile.CloudNativePG{Version: "1.24.0", CRDs: []string{"clusters"}}

	unreadable := gatewayProfile()
	unreadable.CloudNativePG = &clusterprofile.CloudNativePG{} // installed, version unknown

	gapped := gatewayProfile()
	gapped.Incomplete = []clusterprofile.Gap{{
		Field:  "cnpg",
		Reason: "forbidden: needs get on deployments in cnpg-system",
	}}

	for _, tc := range []struct {
		name    string
		profile clusterprofile.ClusterProfile
		preset  model.ServicePreset
		wantErr string // "" means it must render
	}{
		// shared is not exercised here: it is refused before the capability check
		// even runs (TestNotImplementedServices), because the preset itself is
		// deferred (#93) regardless of what the cluster can do. The capability
		// judgement it used to demonstrate — a preset needing more than the
		// baseline Cluster capability — is still pinned directly in
		// internal/clusterprofile/postgres.
		{"supported", yes, model.PresetHASmall, ""},
		{"no cnpg at all", no, model.PresetSmall, ErrPostgresUnsupported},
		{"old enough for dedicated", tooOld, model.PresetSmall, ""},
		{"version unreadable renders", unreadable, model.PresetSmall, ""},
		{"detection gap renders", gapped, model.PresetSmall, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolvedFixture()
			resolved.DataServices = []model.ResolvedDataService{{Name: "db", Kind: model.ComponentPostgres, Preset: tc.preset}}
			_, err := Render(resolved, tc.profile, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("an Unknown or Yes verdict must render, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a No verdict must refuse")
			}
			if code := renderErrorCode(t, err); code != tc.wantErr {
				t.Fatalf("code = %q, want %q", code, tc.wantErr)
			}
		})
	}
}

// TestBindingKeyMapping: kelson's key names are the spec's contract, CNPG's are
// the operator's, and `database` is the one that differs.
func TestBindingKeyMapping(t *testing.T) {
	resolved := boundFixture(model.PresetSmall)
	resolved.Components[0].Env = map[string]model.EnvValue{}
	for _, key := range model.ServiceKeys[model.ComponentPostgres] {
		resolved.Components[0].Env["PG_"+strings.ToUpper(key)] = model.EnvValue{
			From: &model.ServiceBinding{Service: "db", Key: key},
		}
	}
	ms, err := Render(resolved, cnpgProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	out, err := Encode(ms)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"key: uri", "key: host", "key: port", "key: dbname", "key: username", "key: password"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in rendered env:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "key: database") {
		t.Errorf("kelson's `database` key must map onto CNPG's `dbname`:\n%s", out)
	}
}

// TestBindingErrors: every way a binding can fail names the fix.
func TestBindingErrors(t *testing.T) {
	t.Run("unknown service", func(t *testing.T) {
		resolved := resolvedFixture()
		resolved.Components[0].Env["DATABASE_URL"] = model.EnvValue{
			From: &model.ServiceBinding{Service: "nope", Key: "uri"},
		}
		_, err := Render(resolved, cnpgProfile(), nil)
		if err == nil {
			t.Fatalf("binding to an undeclared service must fail")
		}
		if code := renderErrorCode(t, err); code != ErrBindingUnknownService {
			t.Fatalf("code = %q, want %q", code, ErrBindingUnknownService)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		resolved := boundFixture(model.PresetSmall)
		resolved.Components[0].Env["DATABASE_URL"] = model.EnvValue{
			From: &model.ServiceBinding{Service: "db", Key: "jdbc-uri"},
		}
		_, err := Render(resolved, cnpgProfile(), nil)
		if err == nil {
			t.Fatalf("an unmapped key must fail")
		}
		errs, ok := err.(Errors)
		if !ok || len(errs) != 1 {
			t.Fatalf("expected one structured error, got %#v", err)
		}
		if errs[0].Code != ErrBindingUnknownKey {
			t.Fatalf("code = %q, want %q", errs[0].Code, ErrBindingUnknownKey)
		}
		// The remediation must list the keys that do exist, in a stable order.
		if !strings.Contains(errs[0].Remediation, "database, host, password, port, uri, username") {
			t.Errorf("remediation must list the real keys, sorted: %q", errs[0].Remediation)
		}
	})

	t.Run("application is named", func(t *testing.T) {
		resolved := resolvedFixture()
		resolved.Components[1].Env = map[string]model.EnvValue{
			"DATABASE_URL": {From: &model.ServiceBinding{Service: "nope", Key: "uri"}},
		}
		_, err := Render(resolved, cnpgProfile(), nil)
		errs, ok := err.(Errors)
		if !ok || len(errs) != 1 {
			t.Fatalf("expected one structured error, got %#v", err)
		}
		if errs[0].Application != "worker" {
			t.Errorf("error must name the application at fault, got %q", errs[0].Application)
		}
	})
}

// TestServiceNameLengthRefused: CNPG derives <cluster>-superuser and friends
// from this name, and they have to fit in a DNS label.
func TestServiceNameLengthRefused(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Project = strings.Repeat("a", 40)
	resolved.DataServices = []model.ResolvedDataService{{Name: "db", Kind: model.ComponentPostgres, Preset: model.PresetSmall}}
	_, err := Render(resolved, cnpgProfile(), nil)
	if err == nil {
		t.Fatalf("an over-long derived name must be refused")
	}
	if code := renderErrorCode(t, err); code != ErrServiceName {
		t.Fatalf("code = %q, want %q", code, ErrServiceName)
	}
}

// TestServiceHashIsLocal: a database's spec-hash must not churn when an
// unrelated part of the spec changes, or every edit looks like a database edit.
func TestServiceHashIsLocal(t *testing.T) {
	hashOf := func(r *model.Resolved) string {
		ms, err := Render(r, cnpgProfile(), nil)
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		for _, m := range ms {
			if m.Kind != "Cluster" {
				continue
			}
			body, err := m.YAML()
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(body), "\n") {
				if strings.Contains(line, "spec-hash") {
					return strings.TrimSpace(line)
				}
			}
		}
		t.Fatalf("no Cluster rendered")
		return ""
	}

	before := hashOf(boundFixture(model.PresetSmall))

	changed := boundFixture(model.PresetSmall)
	changed.Components[0].Image = "ghcr.io/acme/checkout:9.9.9"
	if got := hashOf(changed); got != before {
		t.Errorf("an application image change moved the database spec-hash:\n%s\n%s", before, got)
	}

	represet := boundFixture(model.PresetHASmall)
	if got := hashOf(represet); got == before {
		t.Errorf("a preset change must move the database spec-hash")
	}
}
