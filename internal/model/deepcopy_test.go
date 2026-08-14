package model

import (
	"reflect"
	"testing"
)

// richProject is a ProjectSpec with every kind of mutable member the reflective
// copier has to handle: a pointer struct, a slice of structs, a map of union
// values, a nested map[string]any and a slice of strings.
func richProject() *ProjectSpec {
	return &ProjectSpec{
		Source: &Source{Git: "https://example.test/app.git", Ref: "main"},
		Build:  &Build{Strategy: BuildDockerfile, Dockerfile: "Dockerfile"},
		Image:  "ghcr.io/acme/app:1",
		Env: map[string]EnvValue{
			"LOG_LEVEL":    {Literal: "info"},
			"DATABASE_URL": {From: &ServiceBinding{Service: "db", Key: "uri"}},
			"API_TOKEN":    {Secret: &SecretRef{Name: "api", Key: "token"}},
		},
		Components: []Component{
			{
				Name:      "web",
				Port:      8080,
				Command:   []string{"/bin/app", "serve"},
				Domains:   []string{"app.example.test"},
				Replicas:  &Replicas{Min: 2, Max: 5},
				Resources: &Resources{Requests: &ResourceList{CPU: "50m", Memory: "64Mi"}},
				Env:       map[string]EnvValue{"PORT": {Literal: "8080"}},
				Release:   &Release{Command: []string{"migrate"}, Timeout: "5m"},
			},
			{
				Name: "db", Kind: ComponentPostgres, Preset: PresetSmall,
				Auth: &SecretRef{Name: "db-auth", Key: "password"},
			},
			{
				Name: "grafana", Kind: ComponentHelm, Chart: "grafana", ChartVersion: "8.0.0",
				Source: &ChartSource{Repository: "https://grafana.github.io/helm-charts"},
				Values: map[string]any{
					"replicas": 2,
					"ingress":  map[string]any{"enabled": true, "hosts": []any{"g.example.test"}},
				},
				ValuesFrom: []ValuesFrom{{SecretRef: "grafana-admin"}},
			},
			{Name: "agent", Kind: ComponentAgent, Tools: []string{"read"}},
		},
		Defaults: &ProjectDefaults{
			DeliveryMode: DeliveryFlux,
			Policy:       &Policy{Agents: AgentsAllow, Protect: []string{"db"}},
			Secrets:      &SecretBackend{Backend: SecretsCluster},
		},
		Overlays: []Overlay{{Patch: "overlays/patch.yaml"}},
	}
}

func richEnvironment() *EnvironmentSpec {
	return &EnvironmentSpec{
		Project:   "checkout",
		Namespace: "checkout-prod",
		Routing:   &Routing{DomainSuffix: "example.test"},
		Delivery:  &Delivery{Mode: DeliveryFlux, Git: &GitTarget{Repo: "https://example.test/deploy.git", Branch: "main"}},
		Policy:    &Policy{Agents: AgentsProposeOnly, Forbid: []AgentOperation{AgentOpDeploy}},
		Secrets:   &SecretBackend{Backend: SecretsSOPS, AgeRecipients: []string{"age1abc"}},
		Components: []ComponentOverride{
			{
				Name:     "web",
				Image:    "ghcr.io/acme/app@sha256:deadbeef",
				Replicas: &Replicas{Min: 3, Max: 9},
				Env:      map[string]EnvValue{"FEATURE": {Literal: "on"}},
			},
			{Name: "db", Preset: PresetHASmall},
		},
		Overlays: []Overlay{{Manifest: "overlays/extra.yaml"}},
	}
}

// TestProjectSpecDeepCopyIsEqual is the easy half: a copy must compare equal to
// its source. It is the half that would still pass with `*out = *in`, which is
// why TestProjectSpecDeepCopyIsIndependent exists below it.
func TestProjectSpecDeepCopyIsEqual(t *testing.T) {
	in := richProject()
	if got := in.DeepCopy(); !reflect.DeepEqual(in, got) {
		t.Fatalf("deep copy is not equal to its source")
	}
	if got := richEnvironment().DeepCopy(); !reflect.DeepEqual(richEnvironment(), got) {
		t.Fatalf("deep copy of EnvironmentSpec is not equal to its source")
	}
	var nilProject *ProjectSpec
	if nilProject.DeepCopy() != nil {
		t.Errorf("DeepCopy of a nil *ProjectSpec must be nil")
	}
	var nilEnv *EnvironmentSpec
	if nilEnv.DeepCopy() != nil {
		t.Errorf("DeepCopy of a nil *EnvironmentSpec must be nil")
	}
}

// TestProjectSpecDeepCopyIsIndependent mutates every mutable member of the copy
// and requires the source to be untouched. A controller-runtime cache hands the
// same object to every caller, so a shallow copy here is one reconciler editing
// another one's spec.
func TestProjectSpecDeepCopyIsIndependent(t *testing.T) {
	in := richProject()
	out := in.DeepCopy()

	out.Source.Ref = "mutated"
	out.Build.Dockerfile = "mutated"
	out.Env["LOG_LEVEL"] = EnvValue{Literal: "mutated"}
	out.Env["NEW"] = EnvValue{Literal: "added"}
	out.Env["DATABASE_URL"].From.Key = "mutated"
	out.Components[0].Command[0] = "mutated"
	out.Components[0].Domains = append(out.Components[0].Domains, "mutated")
	out.Components[0].Replicas.Max = 99
	out.Components[0].Resources.Requests.CPU = "mutated"
	out.Components[0].Env["PORT"] = EnvValue{Literal: "mutated"}
	out.Components[0].Release.Command[0] = "mutated"
	out.Components[1].Auth.Key = "mutated"
	out.Components[2].Values["replicas"] = 99
	out.Components[2].Values["ingress"].(map[string]any)["enabled"] = false
	out.Components[2].Values["ingress"].(map[string]any)["hosts"].([]any)[0] = "mutated"
	out.Components[2].ValuesFrom[0].SecretRef = "mutated"
	out.Components[3].Tools[0] = "mutated"
	out.Defaults.Policy.Protect[0] = "mutated"
	out.Defaults.Secrets.Backend = SecretsSOPS
	out.Overlays[0].Patch = "mutated"

	if !reflect.DeepEqual(in, richProject()) {
		t.Fatalf("mutating the copy changed the source:\n got: %#v\nwant: %#v", in, richProject())
	}
}

func TestEnvironmentSpecDeepCopyIsIndependent(t *testing.T) {
	in := richEnvironment()
	out := in.DeepCopy()

	out.Routing.DomainSuffix = "mutated"
	out.Delivery.Git.Branch = "mutated"
	out.Policy.Forbid[0] = AgentOpRollback
	out.Secrets.AgeRecipients[0] = "mutated"
	out.Components[0].Replicas.Min = 99
	out.Components[0].Env["FEATURE"] = EnvValue{Literal: "mutated"}
	out.Components = append(out.Components, ComponentOverride{Name: "added"})
	out.Overlays[0].Manifest = "mutated"

	if !reflect.DeepEqual(in, richEnvironment()) {
		t.Fatalf("mutating the copy changed the source:\n got: %#v\nwant: %#v", in, richEnvironment())
	}
}

// TestSpecTypesHaveNoUnexportedFields guards the one thing the reflective copier
// cannot do: an unexported field cannot be set through reflection, so it would
// be dropped from every copy without a word. The model has none — every field of
// a spec type is part of the authored document — and this test is what keeps
// that true rather than assumed (see deepcopy.go).
func TestSpecTypesHaveNoUnexportedFields(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice ||
			rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			if rt.Kind() == reflect.Map {
				walk(rt.Key(), path+"{key}")
			}
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				t.Errorf("%s.%s is unexported: a reflective deep copy silently drops it "+
					"(internal/model/deepcopy.go). Export it, or replace the reflective "+
					"copier with an explicit one for this type.", path, f.Name)
				continue
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(ProjectSpec{}), "ProjectSpec")
	walk(reflect.TypeOf(EnvironmentSpec{}), "EnvironmentSpec")
}
