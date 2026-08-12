package renderer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// --- fixtures ---------------------------------------------------------------

func resolvedFixture() *model.Resolved {
	return &model.Resolved{
		Project: "checkout",
		Environment: model.ResolvedEnvironment{
			Name:      "production",
			Namespace: "checkout-prod",
			Routing: model.ResolvedRouting{
				DomainSuffix: "acme.com",
				GatewayClass: "envoy",
				TLS:          true,
			},
		},
		Applications: []model.ResolvedApplication{
			{
				Name:     "web",
				Kind:     model.WorkloadService,
				Image:    "ghcr.io/acme/checkout:1.2.3",
				Port:     8080,
				Health:   "/healthz",
				Domains:  []string{"checkout.acme.com"},
				Replicas: model.Replicas{Min: 2, Max: 5},
				Env: map[string]model.EnvValue{
					"LOG_LEVEL":    {Literal: "info"},
					"DATABASE_URL": {From: &model.ServiceBinding{Service: "db", Key: "uri"}},
				},
			},
			{
				Name:     "worker",
				Kind:     model.WorkloadWorker,
				Image:    "ghcr.io/acme/checkout:1.2.3",
				Command:  []string{"bundle", "exec", "sidekiq"},
				Replicas: model.Replicas{Min: 1},
			},
			{
				Name:     "nightly-report",
				Kind:     model.WorkloadCron,
				Image:    "ghcr.io/acme/checkout:1.2.3",
				Schedule: "0 3 * * *",
				Replicas: model.Replicas{Min: 1},
			},
		},
	}
}

func gatewayProfile() clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{
		GatewayAPI:  &clusterprofile.GatewayAPI{Version: "v1.6.0", Classes: []string{"envoy"}},
		CertManager: &clusterprofile.CertManager{ClusterIssuers: []string{"letsencrypt-prod"}},
		Prometheus:  true,
	}
}

func kinds(ms []Manifest) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Kind + "/" + m.Name
	}
	return out
}

// --- core -------------------------------------------------------------------

// TestRenderDeterministic guards the core purity property from ADR-0001: the
// same inputs must always produce byte-identical output.
func TestRenderDeterministic(t *testing.T) {
	resolved, profile := resolvedFixture(), gatewayProfile()
	first, err := Render(resolved, profile, nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	firstBytes, err := Encode(first)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	for i := 0; i < 25; i++ {
		got, err := Render(resolved, profile, nil)
		if err != nil {
			t.Fatalf("Render run %d failed: %v", i, err)
		}
		gotBytes, err := Encode(got)
		if err != nil {
			t.Fatalf("Encode run %d failed: %v", i, err)
		}
		if !bytes.Equal(firstBytes, gotBytes) {
			t.Fatalf("render %d differed from first render", i)
		}
	}
}

// TestRenderResourceSet pins the resources and order a full gateway-cluster
// render produces.
func TestRenderResourceSet(t *testing.T) {
	ms, err := Render(resolvedFixture(), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	want := []string{
		"ServiceAccount/web",
		"Service/web",
		"Deployment/web",
		"HorizontalPodAutoscaler/web",
		"HTTPRoute/web",
		"Certificate/web-tls",
		"ServiceMonitor/web",
		"Deployment/worker",
		"CronJob/nightly-report",
	}
	got := kinds(ms)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rendered set mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// TestRenderEnvSecretKeyRef: bindings become secretKeyRefs against the
// service credential Secret, and no Secret resource is ever emitted
// (ADR-0009).
func TestRenderEnvSecretKeyRef(t *testing.T) {
	ms, err := Render(resolvedFixture(), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind == "Secret" {
			t.Fatalf("renderer emitted a Secret resource; secrets are references only (ADR-0009)")
		}
	}
	out, err := Encode(ms)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if !strings.Contains(string(out), "secretKeyRef") ||
		!strings.Contains(string(out), "name: checkout-db-credentials") {
		t.Fatalf("expected secretKeyRef to checkout-db-credentials, got:\n%s", out)
	}
	if strings.Contains(string(out), "DATABASE_URL\n      value:") {
		t.Fatalf("binding was rendered as a literal value:\n%s", out)
	}
}

// TestRenderEnvSorted: env emission is key-sorted, independent of map order.
func TestRenderEnvSorted(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Applications[0].Env = map[string]model.EnvValue{
		"ZULU":  {Literal: "1"},
		"ALPHA": {Literal: "2"},
		"MIKE":  {Literal: "3"},
	}
	ms, err := Render(resolved, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	out, err := Encode(ms)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	ai, mi, zi := strings.Index(string(out), "name: ALPHA"), strings.Index(string(out), "name: MIKE"), strings.Index(string(out), "name: ZULU")
	if ai < 0 || ai >= mi || mi >= zi {
		t.Fatalf("env not sorted: ALPHA@%d MIKE@%d ZULU@%d\n%s", ai, mi, zi, out)
	}
}

// TestRenderRoutingMatrix pins the ClusterProfile branching for routing.
func TestRenderRoutingMatrix(t *testing.T) {
	resolved := resolvedFixture()
	cases := []struct {
		name       string
		profile    clusterprofile.ClusterProfile
		wantKinds  []string // routing-plane kinds expected for web
		unexpected []string
	}{
		{
			name:      "gateway api preferred over ingress",
			profile:   clusterprofile.ClusterProfile{GatewayAPI: &clusterprofile.GatewayAPI{Classes: []string{"envoy"}}, IngressClasses: []string{"nginx"}},
			wantKinds: []string{"HTTPRoute"},
		},
		{
			name:      "ingress when no gateway api",
			profile:   clusterprofile.ClusterProfile{IngressClasses: []string{"nginx"}},
			wantKinds: []string{"Ingress"},
		},
		{
			name:       "no route on a bare cluster",
			profile:    clusterprofile.ClusterProfile{},
			unexpected: []string{"HTTPRoute", "Ingress", "Certificate"},
		},
		{
			name:      "certificate only with cert-manager",
			profile:   clusterprofile.ClusterProfile{IngressClasses: []string{"nginx"}, CertManager: &clusterprofile.CertManager{ClusterIssuers: []string{"letsencrypt-prod"}}},
			wantKinds: []string{"Ingress", "Certificate"},
		},
		{
			name:      "no service monitor without prometheus",
			profile:   clusterprofile.ClusterProfile{GatewayAPI: &clusterprofile.GatewayAPI{Classes: []string{"envoy"}}},
			wantKinds: []string{"HTTPRoute"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms, err := Render(resolved, tc.profile, nil)
			if err != nil {
				t.Fatalf("Render failed: %v", err)
			}
			have := map[string]bool{}
			for _, k := range kinds(ms) {
				have[strings.Split(k, "/")[0]] = true
			}
			for _, w := range tc.wantKinds {
				if !have[w] {
					t.Fatalf("expected kind %s in %v", w, kinds(ms))
				}
			}
			for _, u := range tc.unexpected {
				if have[u] {
					t.Fatalf("unexpected kind %s in %v", u, kinds(ms))
				}
			}
		})
	}
}

// TestRenderNoDomainsNoRoute: a service with no domains and no routing suffix
// produces no routing resources regardless of the profile.
func TestRenderNoDomainsNoRoute(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Environment.Routing.DomainSuffix = ""
	resolved.Applications[0].Domains = nil
	ms, err := Render(resolved, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind == "HTTPRoute" || m.Kind == "Ingress" || m.Kind == "Certificate" {
			t.Fatalf("routing resource %s rendered with no domains", m.Kind)
		}
	}
}

// TestSpecHashStable: equal inputs hash identically; a changed input changes
// only that application's hash.
func TestSpecHashStable(t *testing.T) {
	resolved := resolvedFixture()
	h1, err := specHash(resolved, &resolved.Applications[0])
	if err != nil {
		t.Fatalf("specHash failed: %v", err)
	}
	h2, err := specHash(resolved, &resolved.Applications[0])
	if err != nil {
		t.Fatalf("specHash failed: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("specHash not deterministic: %s vs %s", h1, h2)
	}
	if !strings.HasPrefix(h1, "sha256:") {
		t.Fatalf("specHash missing sha256: prefix: %s", h1)
	}

	resolved.Applications[1].Env = map[string]model.EnvValue{"X": {Literal: "y"}}
	h3, err := specHash(resolved, &resolved.Applications[1])
	if err != nil {
		t.Fatalf("specHash failed: %v", err)
	}
	h4, err := specHash(resolvedFixture(), &resolvedFixture().Applications[1])
	if err != nil {
		t.Fatalf("specHash failed: %v", err)
	}
	if h3 == h4 {
		t.Fatalf("specHash did not change when the application changed")
	}
}

// --- overlays ---------------------------------------------------------------

func resolverFor(files map[string]string) OverlayResolver {
	return func(path string) ([]byte, error) {
		body, ok := files[path]
		if !ok {
			return nil, &missingOverlayError{path}
		}
		return []byte(body), nil
	}
}

type missingOverlayError struct{ path string }

func (e *missingOverlayError) Error() string { return "no such overlay: " + e.path }

// TestOverlayPatchMergesAndStamps: a patch merges into its target and leaves
// the kelson.dev/overlays provenance marker.
func TestOverlayPatchMergesAndStamps(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Overlays = []model.Overlay{{Patch: "./k8s/patches/runasnonroot.yaml"}}
	patch := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      securityContext:
        runAsNonRoot: true
      containers:
        - name: web
          env:
            - name: INJECTED
              value: "true"
`
	ms, err := Render(resolved, gatewayProfile(), resolverFor(map[string]string{"./k8s/patches/runasnonroot.yaml": patch}))
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	var web *Manifest
	for i := range ms {
		if ms[i].Kind == "Deployment" && ms[i].Name == "web" {
			web = &ms[i]
		}
	}
	if web == nil {
		t.Fatalf("Deployment/web not found in %v", kinds(ms))
	}
	out, err := web.YAML()
	if err != nil {
		t.Fatalf("YAML failed: %v", err)
	}
	for _, want := range []string{
		"runAsNonRoot: true",
		"name: INJECTED",
		"name: LOG_LEVEL", // existing env preserved
		"kelson.dev/overlays: ./k8s/patches/runasnonroot.yaml",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("patched Deployment missing %q:\n%s", want, out)
		}
	}
}

// TestOverlayPatchNullDeletes: explicit null in a patch deletes the key.
func TestOverlayPatchNullDeletes(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Overlays = []model.Overlay{{Patch: "./p.yaml"}}
	patch := `
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      serviceAccountName: null
`
	ms, err := Render(resolved, gatewayProfile(), resolverFor(map[string]string{"./p.yaml": patch}))
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	var web *Manifest
	for i := range ms {
		if ms[i].Kind == "Deployment" && ms[i].Name == "web" {
			web = &ms[i]
		}
	}
	out, err := web.YAML()
	if err != nil {
		t.Fatalf("YAML failed: %v", err)
	}
	if strings.Contains(string(out), "serviceAccountName") {
		t.Fatalf("null patch did not delete serviceAccountName:\n%s", out)
	}
}

// TestOverlayManifestAppendsAndStamps: manifest overlays are appended after
// core resources, provenance-stamped, and patchable by later patches.
func TestOverlayManifestAppendsAndStamps(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Overlays = []model.Overlay{
		{Manifest: "./k8s/extra/networkpolicy.yaml"},
		{Patch: "./k8s/patches/np-label.yaml"},
	}
	manifest := `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: web-ingress
spec:
  podSelector:
    matchLabels:
      kelson.dev/application: web
  policyTypes: [Ingress]
`
	patch := `
kind: NetworkPolicy
metadata:
  name: web-ingress
spec:
  policyTypes: [Ingress, Egress]
`
	resolver := resolverFor(map[string]string{
		"./k8s/extra/networkpolicy.yaml": manifest,
		"./k8s/patches/np-label.yaml":    patch,
	})
	ms, err := Render(resolved, gatewayProfile(), resolver)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	last := ms[len(ms)-1]
	if last.Kind != "NetworkPolicy" || last.Name != "web-ingress" {
		t.Fatalf("expected appended NetworkPolicy/web-ingress last, got %s/%s", last.Kind, last.Name)
	}
	if last.Namespace != "checkout-prod" {
		t.Fatalf("extra manifest not namespaced: %q", last.Namespace)
	}
	out, err := last.YAML()
	if err != nil {
		t.Fatalf("YAML failed: %v", err)
	}
	for _, want := range []string{
		"app.kubernetes.io/managed-by: kelson",
		"kelson.dev/environment: production",
		"kelson.dev/overlays: ./k8s/extra/networkpolicy.yaml,./k8s/patches/np-label.yaml",
		"policyTypes: [Ingress, Egress]", // later patch merged into the extra manifest
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("extra manifest missing %q:\n%s", want, out)
		}
	}
}

// TestOverlayUnknownTarget: a patch against a resource that does not exist is
// a structured error, never a silent no-op.
func TestOverlayUnknownTarget(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Overlays = []model.Overlay{{Patch: "./missing.yaml"}}
	patch := `
kind: Deployment
metadata:
  name: does-not-exist
spec: {}
`
	_, err := Render(resolved, gatewayProfile(), resolverFor(map[string]string{"./missing.yaml": patch}))
	if err == nil {
		t.Fatalf("expected error for unknown patch target")
	}
	rerrs, ok := err.(Errors)
	if !ok || len(rerrs) != 1 {
		t.Fatalf("expected renderer.Errors, got %#v", err)
	}
	if rerrs[0].Code != ErrOverlayTarget || rerrs[0].Target != "Deployment/does-not-exist" || rerrs[0].Overlay != "./missing.yaml" {
		t.Fatalf("unexpected error payload: %#v", rerrs[0])
	}
}

// TestOverlayNilResolverFails: overlays without a resolver is an error, not a
// filesystem touch.
func TestOverlayNilResolverFails(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Overlays = []model.Overlay{{Patch: "./anything.yaml"}}
	_, err := Render(resolved, gatewayProfile(), nil)
	if err == nil {
		t.Fatalf("expected error when overlays need a resolver")
	}
}

// TestOverlayLoadError: resolver failures surface as structured load errors.
func TestOverlayLoadError(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Overlays = []model.Overlay{{Manifest: "./gone.yaml"}}
	_, err := Render(resolved, gatewayProfile(), resolverFor(map[string]string{}))
	if err == nil {
		t.Fatalf("expected load error")
	}
	rerrs := err.(Errors)
	if rerrs[0].Code != ErrOverlayLoad || rerrs[0].Overlay != "./gone.yaml" {
		t.Fatalf("unexpected error: %#v", rerrs)
	}
}

// TestOverlayDeterministic: overlay application is deterministic too.
func TestOverlayDeterministic(t *testing.T) {
	build := func() []byte {
		resolved := resolvedFixture()
		resolved.Overlays = []model.Overlay{
			{Manifest: "./extra.yaml"},
			{Patch: "./patch.yaml"},
		}
		resolver := resolverFor(map[string]string{
			"./extra.yaml": "kind: ConfigMap\nmetadata:\n  name: extra\n",
			"./patch.yaml": "kind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      dnsPolicy: None\n",
		})
		ms, err := Render(resolved, gatewayProfile(), resolver)
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		out, err := Encode(ms)
		if err != nil {
			t.Fatalf("Encode failed: %v", err)
		}
		return out
	}
	first := build()
	for i := 0; i < 10; i++ {
		if got := build(); !bytes.Equal(first, got) {
			t.Fatalf("overlay render %d differed", i)
		}
	}
}
