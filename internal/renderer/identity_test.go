package renderer

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
)

// Per-component identity (ADR-0014 decision D). Before it, only a web service
// got a ServiceAccount and every worker and cron ran as the namespace's
// `default` — inheriting whatever that account had been granted by anything
// else in the namespace. The rule is now uniform, which is what makes it a rule
// a reader can derive rather than a fact they have to learn.

// TestEveryWorkloadComponentGetsAServiceAccount covers all four workload kinds
// in one render, including the agent that motivated the decision.
func TestEveryWorkloadComponentGetsAServiceAccount(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Components = append(resolved.Components, model.ResolvedComponent{
		Name:     "triage",
		Kind:     model.ComponentAgent,
		Image:    "ghcr.io/acme/checkout:1.2.3",
		Replicas: model.Replicas{Min: 1},
	})

	ms, err := Render(resolved, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	accounts := map[string]bool{}
	for _, m := range ms {
		if m.Kind == "ServiceAccount" {
			accounts[m.Name] = true
		}
	}
	for _, c := range resolved.Components {
		if !accounts[c.Name] {
			t.Errorf("component %q (kind %s) rendered no ServiceAccount of its own", c.Name, c.Kind)
		}
	}
	if len(accounts) != len(resolved.Components) {
		t.Errorf("got %d ServiceAccounts for %d components: %v", len(accounts), len(resolved.Components), accounts)
	}
}

// TestPodTemplatesNameTheirServiceAccount: rendering the account and not using
// it would be theatre — the pod would still run as `default`.
func TestPodTemplatesNameTheirServiceAccount(t *testing.T) {
	resolved := resolvedFixture()
	ms, err := Render(resolved, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind != "Deployment" && m.Kind != "CronJob" {
			continue
		}
		out, err := Encode([]Manifest{m})
		if err != nil {
			t.Fatalf("Encode failed: %v", err)
		}
		want := "serviceAccountName: " + m.Name
		if !strings.Contains(string(out), want) {
			t.Errorf("%s/%s does not name its own ServiceAccount (%q):\n%s", m.Kind, m.Name, want, out)
		}
	}
}

// TestAgentRendersAsAWorkerWithIdentity is decision C stated as an assertion:
// an agent component is a Deployment and a ServiceAccount, and nothing else.
// No Service, no route — those would be an agent runtime this milestone does
// not have.
func TestAgentRendersAsAWorkerWithIdentity(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Components = []model.ResolvedComponent{{
		Name:     "triage",
		Kind:     model.ComponentAgent,
		Image:    "ghcr.io/acme/checkout:1.2.3",
		Command:  []string{"helpdesk", "agent"},
		Replicas: model.Replicas{Min: 1},
	}}

	ms, err := Render(resolved, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := kinds(ms)
	want := []string{"Namespace/checkout-prod", "ServiceAccount/triage", "Deployment/triage"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("agent render mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// TestDataComponentsRenderNoServiceAccount: CloudNativePG creates the identity
// its cluster pods run under, and a second one from kelson would be an unused
// object claiming to be a security boundary (ADR-0005).
func TestDataComponentsRenderNoServiceAccount(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Components = nil
	resolved.DataServices = []model.ResolvedDataService{
		{Name: "db", Kind: model.ComponentPostgres, Preset: model.PresetSmall},
	}

	ms, err := Render(resolved, cnpgProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind == "ServiceAccount" {
			t.Errorf("a data component rendered a ServiceAccount (%s); the operator owns that identity", m.Name)
		}
	}
}

// TestServiceAccountCarriesProvenance: the account is a real kelson resource,
// not a bare name, so uninstall and diff can attribute it like any other.
func TestServiceAccountCarriesProvenance(t *testing.T) {
	ms, err := Render(resolvedFixture(), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind != "ServiceAccount" || m.Name != "worker" {
			continue
		}
		out, err := Encode([]Manifest{m})
		if err != nil {
			t.Fatalf("Encode failed: %v", err)
		}
		for _, want := range []string{
			"app.kubernetes.io/managed-by: kelson",
			"kelson.dev/application: worker",
			"kelson.dev/environment: production",
			"kelson.dev/project: checkout",
			"kelson.dev/spec-hash:",
			"namespace: checkout-prod",
		} {
			if !strings.Contains(string(out), want) {
				t.Errorf("ServiceAccount/worker is missing %q:\n%s", want, out)
			}
		}
		return
	}
	t.Fatal("no ServiceAccount rendered for the worker component")
}
