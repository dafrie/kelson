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
		if m.Kind == "Cluster" || m.Kind == "Database" || m.Kind == "ValkeyCluster" {
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
//
// The valkey rows are the other half of the rule: `shared` and `branch` are
// members of one shared preset vocabulary (ADR-0007) and two of them describe
// topologies a cache does not have, so they refuse with a cache-specific reason
// rather than rendering the nearest thing that does exist.
func TestNotImplementedServices(t *testing.T) {
	for _, tc := range []struct {
		name  string
		svc   model.ResolvedDataService
		issue string
	}{
		{"postgres branch", model.ResolvedDataService{Name: "db", Kind: model.ComponentPostgres, Preset: model.PresetBranch}, "#99"},
		{"postgres shared", model.ResolvedDataService{Name: "db", Kind: model.ComponentPostgres, Preset: model.PresetShared}, "#93"},
		{"valkey branch", model.ResolvedDataService{Name: "cache", Kind: model.ComponentValkey, Preset: model.PresetBranch}, "#99"},
		{"valkey shared", model.ResolvedDataService{Name: "cache", Kind: model.ComponentValkey, Preset: model.PresetShared}, "#93"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolvedFixture()
			resolved.DataServices = []model.ResolvedDataService{tc.svc}
			_, err := Render(resolved, valkeyProfile(), nil)
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

// TestValkeyPresetSizing pins the cache sizing table of docs/data-services.md to
// the rendered output, so a silent edit of one of the numbers fails here as well
// as in the goldens. maxmemory is checked against the memory limit deliberately:
// the two numbers must never become the same one (docs/data-services.md).
func TestValkeyPresetSizing(t *testing.T) {
	for _, tc := range []struct {
		preset   model.ServicePreset
		contains []string
	}{
		{
			preset:   model.PresetSmall,
			contains: []string{"shards: 1", "replicas: 0", "cpu: 250m", "memory: 512Mi", "maxmemory: 384mb"},
		},
		{
			preset:   model.PresetHASmall,
			contains: []string{"shards: 3", "replicas: 1", "memory: 512Mi", "maxmemory: 384mb"},
		},
		{
			preset:   model.PresetHAMedium,
			contains: []string{"shards: 3", "replicas: 1", "cpu: \"1\"", "memory: 2Gi", "maxmemory: 1536mb"},
		},
	} {
		t.Run(string(tc.preset), func(t *testing.T) {
			ms, err := Render(cacheFixture(tc.preset), valkeyProfile(), nil)
			if err != nil {
				t.Fatalf("Render failed: %v", err)
			}
			out, err := Encode(ms)
			if err != nil {
				t.Fatalf("Encode failed: %v", err)
			}
			for _, want := range append(tc.contains, "maxmemory-policy: allkeys-lru") {
				if !strings.Contains(string(out), want) {
					t.Errorf("rendered ValkeyCluster missing %q:\n%s", want, out)
				}
			}
			// Persistence is what separates a cache from a store, and its
			// absence is a decision (issue #98), not an oversight.
			if strings.Contains(string(out), "persistence:") {
				t.Errorf("a cache preset must render no persistence:\n%s", out)
			}
		})
	}
}

// TestValkeyOperatorAbsentRefuses: `kind: valkey` is a managed type with no
// degraded mode (ADR-0005). Without the operator there is nothing to delegate
// to, and rendering a ValkeyCluster nothing would reconcile is the silent
// success issue #141 exists to prevent.
func TestValkeyOperatorAbsentRefuses(t *testing.T) {
	_, err := Render(cacheFixture(model.PresetSmall), cnpgProfile(), nil)
	if err == nil {
		t.Fatalf("a cluster with no Valkey operator must refuse")
	}
	errs, ok := err.(Errors)
	if !ok || len(errs) != 1 {
		t.Fatalf("expected one structured error, got %#v", err)
	}
	if errs[0].Code != ErrValkeyUnsupported {
		t.Fatalf("code = %q, want %q", errs[0].Code, ErrValkeyUnsupported)
	}
	if !strings.Contains(errs[0].Remediation, "valkey-io/valkey-operator") {
		t.Errorf("the refusal must name the operator to install: %q", errs[0].Remediation)
	}
}

// TestValkeyCapabilityTriState is the postgres tri-state rule applied to the
// other operator: only a definite No refuses, and a partially-served CRD set is
// a definite No because a ValkeyCluster with no ValkeyNode CRD is accepted and
// then never runs a pod.
func TestValkeyCapabilityTriState(t *testing.T) {
	tooOld := cnpgProfile()
	tooOld.Valkey = &clusterprofile.ValkeyOperator{Version: "0.4.0", CRDs: []string{"valkeyclusters", "valkeynodes"}}

	partialCRDs := cnpgProfile()
	partialCRDs.Valkey = &clusterprofile.ValkeyOperator{Version: "0.5.0", CRDs: []string{"valkeyclusters"}}

	unreadable := cnpgProfile()
	unreadable.Valkey = &clusterprofile.ValkeyOperator{} // installed, version unknown, CRDs unread

	gapped := cnpgProfile()
	gapped.Incomplete = []clusterprofile.Gap{{
		Field:  "valkey",
		Reason: "forbidden: needs get,list on deployments.apps",
	}}

	for _, tc := range []struct {
		name    string
		profile clusterprofile.ClusterProfile
		wantErr string // "" means it must render
	}{
		{"supported", valkeyProfile(), ""},
		{"operator too old", tooOld, ErrValkeyUnsupported},
		{"valkeynodes not served", partialCRDs, ErrValkeyUnsupported},
		{"version unreadable renders", unreadable, ""},
		{"detection gap renders", gapped, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(cacheFixture(model.PresetSmall), tc.profile, nil)
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

// TestValkeyBindings: a cache's connection details are plain values, and the one
// key it cannot answer refuses with the reason rather than with a list of
// alternatives that does not contain the answer.
func TestValkeyBindings(t *testing.T) {
	t.Run("connection details render as values", func(t *testing.T) {
		resolved := cacheFixture(model.PresetSmall)
		resolved.Components[0].Env["CACHE_HOST"] = model.EnvValue{From: &model.ServiceBinding{Service: "cache", Key: "host"}}
		resolved.Components[0].Env["CACHE_PORT"] = model.EnvValue{From: &model.ServiceBinding{Service: "cache", Key: "port"}}
		ms, err := Render(resolved, valkeyProfile(), nil)
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		out, err := Encode(ms)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"value: redis://valkey-checkout-production-cache.checkout-prod.svc:6379",
			"value: valkey-checkout-production-cache.checkout-prod.svc",
			`value: "6379"`,
		} {
			if !strings.Contains(string(out), want) {
				t.Errorf("missing %q in rendered env:\n%s", want, out)
			}
		}
	})

	t.Run("password is withheld with a reason", func(t *testing.T) {
		resolved := cacheFixture(model.PresetSmall)
		resolved.Components[0].Env["CACHE_PASSWORD"] = model.EnvValue{
			From: &model.ServiceBinding{Service: "cache", Key: "password"},
		}
		_, err := Render(resolved, valkeyProfile(), nil)
		if err == nil {
			t.Fatalf("binding a key the service cannot supply must fail")
		}
		errs, ok := err.(Errors)
		if !ok || len(errs) != 1 {
			t.Fatalf("expected one structured error, got %#v", err)
		}
		if errs[0].Code != ErrBindingUnavailableKey {
			t.Fatalf("code = %q, want %q", errs[0].Code, ErrBindingUnavailableKey)
		}
		if !strings.Contains(errs[0].Remediation, "no application credential") {
			t.Errorf("the refusal must explain why, not list alternatives: %q", errs[0].Remediation)
		}
	})

	t.Run("unknown key lists what a cache can answer", func(t *testing.T) {
		resolved := cacheFixture(model.PresetSmall)
		resolved.Components[0].Env["CACHE_DB"] = model.EnvValue{
			From: &model.ServiceBinding{Service: "cache", Key: "database"},
		}
		_, err := Render(resolved, valkeyProfile(), nil)
		errs, ok := err.(Errors)
		if !ok || len(errs) != 1 {
			t.Fatalf("expected one structured error, got %#v", err)
		}
		if errs[0].Code != ErrBindingUnknownKey {
			t.Fatalf("code = %q, want %q", errs[0].Code, ErrBindingUnknownKey)
		}
		if !strings.Contains(errs[0].Remediation, "host, port, uri") {
			t.Errorf("remediation must list the keys that exist, sorted: %q", errs[0].Remediation)
		}
	})
}

// TestValkeyResourceNameRefused: the Valkey operator derives
// internal-<cluster>-system-passwords, which is 26 characters longer than the
// name kelson chose and still has to fit a DNS label.
func TestValkeyResourceNameRefused(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Project = strings.Repeat("a", 30)
	resolved.DataServices = []model.ResolvedDataService{{Name: "cache", Kind: model.ComponentValkey, Preset: model.PresetSmall}}
	_, err := Render(resolved, valkeyProfile(), nil)
	if err == nil {
		t.Fatalf("an over-long derived name must be refused")
	}
	if code := renderErrorCode(t, err); code != ErrServiceName {
		t.Fatalf("code = %q, want %q", code, ErrServiceName)
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
