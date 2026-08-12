package diff_test

// Acceptance test for issue #42: "A change touching one environment variable
// reports exactly that, with the correct restart implication."

import (
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

func resolveOne(t *testing.T, logLevel string) *model.Resolved {
	t.Helper()
	return &model.Resolved{
		Project: "checkout",
		Environment: model.ResolvedEnvironment{
			Name:      "production",
			Namespace: "checkout-prod",
			Routing:   model.ResolvedRouting{DomainSuffix: "acme.com", GatewayClass: "envoy", TLS: true},
		},
		Applications: []model.ResolvedApplication{
			{
				Name:     "web",
				Kind:     model.WorkloadService,
				Image:    "ghcr.io/acme/checkout:1.2.3",
				Port:     8080,
				Health:   "/healthz",
				Domains:  []string{"checkout.acme.com"},
				Replicas: model.Replicas{Min: 2},
				Env: map[string]model.EnvValue{
					"LOG_LEVEL": {Literal: logLevel},
				},
			},
		},
	}
}

func profile() clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{GatewayAPI: &clusterprofile.GatewayAPI{Version: "v1.6.0", Classes: []string{"envoy"}}}
}

// TestEnvVarChangeReportsExactlyOneField is the #42 acceptance criterion: a
// change touching one environment variable produces exactly one FieldDiff at
// that field, with the correct (restart-required) implication.
func TestEnvVarChangeReportsExactlyOneField(t *testing.T) {
	prev, err := renderer.Render(resolveOne(t, "info"), profile(), nil)
	if err != nil {
		t.Fatalf("render prev: %v", err)
	}
	cur, err := renderer.Render(resolveOne(t, "debug"), profile(), nil)
	if err != nil {
		t.Fatalf("render cur: %v", err)
	}

	d, err := diff.Between("checkout", "production", prev, cur, nil)
	if err != nil {
		t.Fatalf("Between: %v", err)
	}

	if len(d.Resources) != 1 {
		t.Fatalf("expected exactly one changed resource, got %d:\n%+v", len(d.Resources), d.Resources)
	}
	r := d.Resources[0]
	if r.Kind != "Deployment" || r.Name != "web" {
		t.Fatalf("expected Deployment/web, got %s/%s", r.Kind, r.Name)
	}
	if r.Op != diff.OpModified {
		t.Fatalf("expected modified, got %q", r.Op)
	}
	if r.Risk != diff.RiskRestart {
		t.Fatalf("expected restart-required, got %q", r.Risk)
	}

	if len(r.Fields) != 1 {
		t.Fatalf("expected exactly one field diff, got %d:\n%+v", len(r.Fields), r.Fields)
	}
	f := r.Fields[0]
	wantPath := "spec.template.spec.containers[0].env[LOG_LEVEL].value"
	if f.Path != wantPath {
		t.Fatalf("field path = %q, want %q", f.Path, wantPath)
	}
	if f.Risk != diff.RiskRestart {
		t.Fatalf("field risk = %q, want restart-required", f.Risk)
	}
	if f.Origin != diff.OriginSpec {
		t.Fatalf("field origin = %q, want spec", f.Origin)
	}

	if s := d.Summary; s.Added != 0 || s.Modified != 1 || s.Removed != 0 {
		t.Fatalf("summary counts wrong: %+v", s)
	}
	if len(d.Summary.Restarting) != 1 || d.Summary.Restarting[0] != "web" {
		t.Fatalf("summary.restarting = %v, want [web]", d.Summary.Restarting)
	}
	if d.Summary.MaxRisk != diff.RiskRestart {
		t.Fatalf("summary.maxRisk = %q, want restart-required", d.Summary.MaxRisk)
	}
}
