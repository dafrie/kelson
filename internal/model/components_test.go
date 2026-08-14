package model

import (
	"slices"
	"strings"
	"testing"
)

// The rules ADR-0014 added to the leaf: kind derivation, the agreement between
// a written kind and the shape it claims, and the per-field errors that are the
// price of holding workloads and data services in one list.

// decodeProjectSpec wraps a Project spec body so these cases read as the
// documents an author would actually write.
func decodeProjectSpec(t *testing.T, spec string) Errors {
	t.Helper()
	_, errs := DecodeDocuments([]byte("apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata: {name: p}\nspec:\n" + spec))
	return errs
}

// TestComponentKindDerivation pins ADR-0006's derivation rule inside ADR-0014's
// list: the shape still names the workload, so the six-line Project is
// unchanged by the rename.
func TestComponentKindDerivation(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
    - {name: worker}
    - {name: nightly, schedule: "0 3 * * *"}
    - {name: triage, kind: agent}
    - {name: db, kind: postgres}
`))
	if len(errs) > 0 {
		t.Fatalf("the derivation document must validate, got:\n%v", errs)
	}
	want := []ComponentKind{ComponentService, ComponentWorker, ComponentCron, ComponentAgent, ComponentPostgres}
	for i, c := range docs[0].(*Project).Spec.Components {
		if got := c.EffectiveKind(); got != want[i] {
			t.Errorf("components[%d] (%s) kind = %q, want %q", i, c.Name, got, want[i])
		}
	}
}

// TestExplicitKindMustMatchTheShape: an explicit kind states what a component
// is; it never silently overrules the fields that say otherwise.
func TestExplicitKindMustMatchTheShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   string
		wantErr bool
	}{
		{"service with port", "{name: web, kind: service, port: 8080}", false},
		{"service without port", "{name: web, kind: service}", true},
		{"cron with schedule", `{name: n, kind: cron, schedule: "0 3 * * *"}`, false},
		{"cron without schedule", "{name: n, kind: cron}", true},
		{"worker bare", "{name: w, kind: worker}", false},
		{"worker with port", "{name: w, kind: worker, port: 8080}", true},
		{"agent bare", "{name: a, kind: agent}", false},
		{"agent with port", "{name: a, kind: agent, port: 8080}", true},
		{"agent with schedule", `{name: a, kind: agent, schedule: "0 3 * * *"}`, true},
		{"unknown kind", "{name: x, kind: lambda}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - "+tc.entry+"\n")
			if tc.wantErr && len(errs) == 0 {
				t.Errorf("%s must be rejected", tc.entry)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("%s must validate, got:\n%v", tc.entry, errs)
			}
		})
	}
}

// TestDataComponentRejectsWorkloadFields: the price of one list is that every
// field meaningless for a kind says so, rather than resolving into nothing.
func TestDataComponentRejectsWorkloadFields(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: db
      kind: postgres
      preset: small
      port: 5432
      image: postgres:16
      replicas: {min: 3}
`)
	fields := map[string]bool{}
	for _, e := range errs {
		if e.Code == ErrMutuallyExclusive {
			fields[e.Field] = true
		}
	}
	for _, want := range []string{
		"$.spec.components[0].port",
		"$.spec.components[0].image",
		"$.spec.components[0].replicas",
	} {
		if !fields[want] {
			t.Errorf("a data component setting %s must be rejected, got:\n%v", want, errs)
		}
	}
}

// TestPresetOnWorkloadRejected is the mirror image: a preset is a topology, and
// a workload has none.
func TestPresetOnWorkloadRejected(t *testing.T) {
	errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - {name: web, port: 8080, preset: ha-small}\n")
	found := false
	for _, e := range errs {
		if e.Code == ErrMutuallyExclusive && e.Field == "$.spec.components[0].preset" {
			found = true
			if !strings.Contains(e.Remediation, "kind: postgres") {
				t.Errorf("remediation should name the kind that makes preset real, got %q", e.Remediation)
			}
		}
	}
	if !found {
		t.Errorf("preset on a workload must be rejected, got:\n%v", errs)
	}
}

// TestComponentAuth covers the `auth:` reference added by the 2026-08-14
// amendment to ADR-0015 (#98): a valkey-only field, held to the same shape
// ADR-0018 gave every other reference, and refused with a reason everywhere
// else rather than silently ignored (#141).
func TestComponentAuth(t *testing.T) {
	t.Run("valid on valkey", func(t *testing.T) {
		errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: cache
      kind: valkey
      preset: small
      auth: {secret: cache-auth, key: password}
`)
		if len(errs) != 0 {
			t.Fatalf("auth on a valkey component must validate, got:\n%v", errs)
		}
	})

	t.Run("carried through resolution", func(t *testing.T) {
		docs, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - {name: cache, kind: valkey, preset: small, auth: {secret: cache-auth, key: password}}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: p
  namespace: p-prod
`))
		if len(errs) != 0 {
			t.Fatalf("the document must validate, got:\n%v", errs)
		}
		res, errs := Resolve(docs[0].(*Project), docs[1].(*Environment))
		if len(errs) != 0 {
			t.Fatalf("Resolve failed: %v", errs)
		}
		auth := res.DataServices[0].Auth
		if auth == nil || auth.Name != "cache-auth" || auth.Key != "password" {
			t.Fatalf("auth did not survive resolution: %#v", auth)
		}
	})

	t.Run("half a reference is refused like any other", func(t *testing.T) {
		errs := decodeProjectSpec(t,
			"  image: i:1\n  components:\n    - {name: cache, kind: valkey, preset: small, auth: {secret: cache-auth}}\n")
		found := false
		for _, e := range errs {
			if e.Code == ErrMissingRequired && e.Field == "$.spec.components[0].auth.key" {
				found = true
			}
		}
		if !found {
			t.Errorf("auth reuses the secret-reference rules, got:\n%v", errs)
		}
	})

	for _, tc := range []struct {
		name, spec, field, wants string
	}{
		{
			"on postgres", "    - {name: db, kind: postgres, preset: small, auth: {secret: s, key: k}}",
			"$.spec.components[0].auth", "initdb",
		},
		{
			"on a workload", "    - {name: web, port: 8080, auth: {secret: s, key: k}}",
			"$.spec.components[0].auth", "{secret: s, key: k}",
		},
		{
			"on a chart",
			"    - {name: ing, kind: helm, chart: c, chartVersion: 1.0.0, source: {repository: https://example.com}, auth: {secret: s, key: k}}",
			"$.spec.components[0].auth", "kind: valkey",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := decodeProjectSpec(t, "  image: i:1\n  components:\n"+tc.spec+"\n")
			found := false
			for _, e := range errs {
				if e.Code == ErrMutuallyExclusive && e.Field == tc.field {
					found = true
					if !strings.Contains(e.Remediation, tc.wants) {
						t.Errorf("remediation must say where the credential comes from instead, got %q", e.Remediation)
					}
				}
			}
			if !found {
				t.Errorf("auth on this kind must be refused, got:\n%v", errs)
			}
		})
	}
}

// TestToolsRequireAnAgentAndAreGated covers ADR-0014 decision C: the field is
// validated where it belongs and refused everywhere, because there is no policy
// engine to enforce it yet (#75).
func TestToolsRequireAnAgentAndAreGated(t *testing.T) {
	t.Run("on a worker", func(t *testing.T) {
		errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - {name: w, tools: [search]}\n")
		found := false
		for _, e := range errs {
			if e.Code == ErrMutuallyExclusive && e.Field == "$.spec.components[0].tools" {
				found = true
			}
		}
		if !found {
			t.Errorf("tools on a non-agent must be rejected, got:\n%v", errs)
		}
	})

	t.Run("on an agent", func(t *testing.T) {
		errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - {name: a, kind: agent, tools: [search]}\n")
		var gate *Error
		for i := range errs {
			if errs[i].Code == ErrNotImplemented && errs[i].Field == "$.spec.components[0].tools" {
				gate = &errs[i]
			}
		}
		if gate == nil {
			t.Fatalf("tools on an agent must be gated until #75, got:\n%v", errs)
		}
		if !strings.Contains(gate.Remediation, "#75") {
			t.Errorf("the gate must name where the work is tracked, got %q", gate.Remediation)
		}
	})

	t.Run("shape is still checked", func(t *testing.T) {
		errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: a
      kind: agent
      tools: ["search", "search", ""]
`)
		codes := errs.Codes()
		if !slices.Contains(codes, ErrDuplicateName) {
			t.Errorf("a duplicate tool must be reported even while the field is gated, got:\n%v", errs)
		}
		if !slices.Contains(codes, ErrMissingRequired) {
			t.Errorf("an empty tool name must be reported, got:\n%v", errs)
		}
	})
}

// TestAgentRendersAsAWorker: decision C lands thin, so an agent with no tools
// is an ordinary document that validates and resolves like the worker it is.
func TestAgentResolvesAsAWorkload(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - {name: triage, kind: agent}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
`)
	r, errs := Resolve(p, e)
	if len(errs) > 0 {
		t.Fatalf("an agent component must resolve, got:\n%v", errs)
	}
	if len(r.Components) != 1 || r.Components[0].Kind != ComponentAgent {
		t.Fatalf("agent must resolve as a workload component: %+v", r.Components)
	}
	if len(r.DataServices) != 0 {
		t.Errorf("an agent is not a data service: %+v", r.DataServices)
	}
}

// TestOverrideShapeFollowsTheTargetKind: which half of the model an Environment
// override belongs to is decided by the component it names, not by its own
// shape — which only becomes knowable with the Project in hand.
func TestOverrideShapeFollowsTheTargetKind(t *testing.T) {
	project := `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - {name: db, kind: postgres}
    - {name: web, port: 8080}
`
	for _, tc := range []struct {
		name     string
		override string
		wantCode Code
	}{
		{"preset on a workload", "    - {name: web, preset: small}", ErrMutuallyExclusive},
		{"replicas on a database", "    - {name: db, replicas: {min: 3}}", ErrMutuallyExclusive},
		{"empty override of a database", "    - {name: db}", ErrMissingRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, e := loadPair(t, project, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  components:
`+tc.override+"\n")
			errs := ValidateEnvironment(e, p)
			if !slices.Contains(errs.Codes(), tc.wantCode) {
				t.Errorf("want %s, got:\n%v", tc.wantCode, errs)
			}
		})
	}

	t.Run("valid overrides", func(t *testing.T) {
		p, e := loadPair(t, project, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  components:
    - {name: db, preset: ha-small}
    - {name: web, replicas: {min: 3}}
`)
		if errs := ValidateEnvironment(e, p); len(errs) > 0 {
			t.Errorf("an override of each kind must validate, got:\n%v", errs)
		}
	})
}

// TestBindingTargetsMustBeDataComponents: one list means a binding could now
// name a worker, and saying so is better than a secretKeyRef to nothing.
func TestBindingTargetsMustBeDataComponents(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  env:
    DATABASE_URL: {from: {service: worker, key: uri}}
  components:
    - {name: worker}
`)
	found := false
	for _, e := range errs {
		if e.Code == ErrUnknownService {
			found = true
			if !strings.Contains(e.Remediation, "kind: postgres") {
				t.Errorf("remediation should say what a bindable component is, got %q", e.Remediation)
			}
		}
	}
	if !found {
		t.Errorf("binding to a workload must be rejected, got:\n%v", errs)
	}
}

// TestComponentNamesShareOneNamespace: with one list, a database and a worker
// called the same thing is a collision, which it was not when the two lived in
// separate lists. They would collide in the namespace anyway.
func TestComponentNamesShareOneNamespace(t *testing.T) {
	errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - {name: cache, kind: postgres}\n    - {name: cache}\n")
	if !slices.Contains(errs.Codes(), ErrDuplicateName) {
		t.Errorf("a workload and a database cannot share a name, got:\n%v", errs)
	}
}
