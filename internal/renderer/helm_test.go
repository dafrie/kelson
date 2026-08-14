package renderer

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
)

// Helm-component rendering (ADR-0016 decision 4). The rendered shapes are
// pinned by the golden fixtures under testdata/render/helm-*; these tests pin
// what a golden file cannot hold — the delivery-mode gate and its refusal, the
// ownership boundary, and the two ways a name or a source can be wrong.

// chartFixture is the standard fixture with one helm component, in an
// environment whose delivery mode is flux — the only mode a chart renders in.
func chartFixture(chart model.ResolvedChart) *model.Resolved {
	r := resolvedFixture()
	r.Environment.Mode = model.DeliveryFlux
	r.Charts = []model.ResolvedChart{chart}
	return r
}

func repositoryChart() model.ResolvedChart {
	return model.ResolvedChart{
		Name:    "ingress",
		Chart:   "ingress-nginx",
		Version: "4.11.3",
		Source:  model.ChartSource{Repository: "https://kubernetes.github.io/ingress-nginx"},
	}
}

// TestHelmRequiresFluxMode is the gate of ADR-0016 decision 4. Direct mode has
// no helm-controller to delegate to, so a HelmRelease applied there is a
// manifest that does nothing — and kelson refuses rather than emitting it.
func TestHelmRequiresFluxMode(t *testing.T) {
	for _, mode := range []model.DeliveryMode{model.DeliveryDirect, model.DeliveryArgoCD, ""} {
		r := chartFixture(repositoryChart())
		r.Environment.Mode = mode
		_, err := Render(r, gatewayProfile(), nil)
		if err == nil {
			t.Fatalf("mode %q rendered a HelmRelease nothing would reconcile", mode)
		}
		errs, ok := err.(Errors)
		if !ok || len(errs) != 1 {
			t.Fatalf("mode %q: expected one structured error, got %#v", mode, err)
		}
		e := errs[0]
		if e.Code != ErrHelmRequiresFlux {
			t.Errorf("mode %q: code = %q, want %q", mode, e.Code, ErrHelmRequiresFlux)
		}
		if e.Component != "ingress" {
			t.Errorf("mode %q: the error must name the component, got %q", mode, e.Component)
		}
		if !strings.Contains(e.Message, string(mode)) && mode != "" {
			t.Errorf("mode %q: the message must name the mode: %s", mode, e.Message)
		}
		for _, want := range []string{"delivery.mode: flux", "ADR-0016"} {
			if !strings.Contains(e.Remediation, want) {
				t.Errorf("mode %q: remediation must contain %q: %s", mode, want, e.Remediation)
			}
		}
	}
}

// TestHelmGateReportsEveryComponent: one run should list all the work, the way
// unresolved images do. Fixing one component and re-running to find the next is
// the loop this avoids.
func TestHelmGateReportsEveryComponent(t *testing.T) {
	r := chartFixture(repositoryChart())
	r.Environment.Mode = model.DeliveryDirect
	second := repositoryChart()
	second.Name = "cert-manager"
	r.Charts = append(r.Charts, second)

	_, err := Render(r, gatewayProfile(), nil)
	errs, ok := err.(Errors)
	if !ok || len(errs) != 2 {
		t.Fatalf("expected one error per helm component, got %#v", err)
	}
	if errs[0].Component != "ingress" || errs[1].Component != "cert-manager" {
		t.Fatalf("errors must follow spec order: %v", errs)
	}
}

// TestHelmGateIgnoresProfile is the other half of the gate's contract: it is
// decided from spec data alone. A cluster with no helm-controller detected
// still renders — whether the controller is installed is a capability finding
// (internal/clusterprofile/helm), not a rendering decision, or the same
// document would render differently against two clusters.
func TestHelmGateIgnoresProfile(t *testing.T) {
	ms, err := Render(chartFixture(repositoryChart()), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("a flux-mode chart must render against any profile: %v", err)
	}
	if !hasKind(ms, "HelmRelease") {
		t.Fatalf("no HelmRelease in %v", kinds(ms))
	}
}

// TestChartOwnsOnlyTheReleaseAndItsSource is the ownership boundary of
// ADR-0005 applied to charts, asserted as a count. kelson's inventory for a
// helm component is exactly two resources; everything the chart installs is
// helm-controller's and appears in no manifest set of kelson's, which is what
// makes the values-only preview honest rather than a bug.
func TestChartOwnsOnlyTheReleaseAndItsSource(t *testing.T) {
	r := chartFixture(repositoryChart())
	r.Components = nil
	ms, err := Render(r, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := kinds(ms)
	want := []string{"Namespace/checkout-prod", "HelmRepository/checkout-production-ingress", "HelmRelease/checkout-production-ingress"}
	if len(got) != len(want) {
		t.Fatalf("a helm component owns the release and its source and nothing else, got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestChartSourceRendersBeforeTheRelease: the release references the source by
// name, and order is the only sequencing a rendered set can express (#89).
func TestChartsRenderBeforeWorkloads(t *testing.T) {
	ms, err := Render(chartFixture(repositoryChart()), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := kinds(ms)
	if got[1] != "HelmRepository/checkout-production-ingress" || got[2] != "HelmRelease/checkout-production-ingress" {
		t.Fatalf("source then release must follow the Namespace, got %v", got)
	}
	for _, m := range ms[3:] {
		if m.Kind == "HelmRelease" || m.Kind == "HelmRepository" {
			t.Fatalf("a chart resource sorted after a workload: %v", got)
		}
	}
}

// TestOCIChartPinsTheVersionAsATag: an OCIRepository is one chart, so the pin
// is the artifact's tag and the release needs no chart selector. Getting this
// backwards produces a release that silently tracks whatever the registry last
// pushed, which is the failure chartVersion exists to prevent.
func TestOCIChartPinsTheVersionAsATag(t *testing.T) {
	chart := repositoryChart()
	chart.Name = "podinfo"
	chart.Chart = "podinfo"
	chart.Version = "6.7.1"
	chart.Source = model.ChartSource{OCI: "oci://ghcr.io/stefanprodan/charts"}

	ms, err := Render(chartFixture(chart), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	source := manifestYAML(t, ms, "OCIRepository")
	for _, want := range []string{"url: oci://ghcr.io/stefanprodan/charts/podinfo", "tag: 6.7.1"} {
		if !strings.Contains(source, want) {
			t.Errorf("OCIRepository missing %q:\n%s", want, source)
		}
	}
	release := manifestYAML(t, ms, "HelmRelease")
	if !strings.Contains(release, "chartRef:") {
		t.Errorf("an OCI chart is referenced with chartRef:\n%s", release)
	}
	if strings.Contains(release, "sourceRef:") {
		t.Errorf("an OCI release selects nothing — the source is the chart:\n%s", release)
	}
}

// TestChartValuesAreDeterministic: a Go map has no order, so the renderer sorts
// keys at every level. Without that the golden harness's eight renders would
// disagree — this asserts it directly, on a map wide enough that iteration
// order would show.
func TestChartValuesAreDeterministic(t *testing.T) {
	chart := repositoryChart()
	chart.Values = map[string]any{
		"zulu": 1, "alpha": true, "mike": "x",
		"nested": map[string]any{"z": 1, "a": 2, "m": []any{3, "four", false}},
	}
	var first string
	for i := 0; i < 8; i++ {
		ms, err := Render(chartFixture(chart), gatewayProfile(), nil)
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		got := manifestYAML(t, ms, "HelmRelease")
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("render %d differed:\n%s\n---\n%s", i, first, got)
		}
	}
	// Sorted at both levels, and scalars keep the type they were written as: a
	// chart that expects a string and gets an integer fails in its own
	// templates, far from the spec line that caused it.
	for _, want := range []string{"alpha: true", "mike: x", "zulu: 1", "a: 2", "m:", "- four"} {
		if !strings.Contains(first, want) {
			t.Errorf("values missing %q:\n%s", want, first)
		}
	}
	if strings.Index(first, "alpha") > strings.Index(first, "mike") {
		t.Errorf("keys are not sorted:\n%s", first)
	}
}

// TestChartNameTooLong: the resource name becomes the Helm release name, and
// Helm refuses one longer than 53 characters inside the controller — long after
// the apply a reader would connect it to.
func TestChartNameTooLong(t *testing.T) {
	r := chartFixture(repositoryChart())
	r.Project = strings.Repeat("p", 40)
	_, err := Render(r, gatewayProfile(), nil)
	if err == nil {
		t.Fatal("expected a name-length refusal")
	}
	if code := renderErrorCode(t, err); code != ErrChartName {
		t.Fatalf("code = %q, want %q", code, ErrChartName)
	}
}

// TestChartWithoutSourceIsInternal: model validation requires exactly one
// source, so a chart arriving here without one came from the API plane feeding
// the renderer a Resolved directly. It must refuse loudly rather than render
// half a release (#141).
func TestChartWithoutSourceIsInternal(t *testing.T) {
	chart := repositoryChart()
	chart.Source = model.ChartSource{}
	_, err := Render(chartFixture(chart), gatewayProfile(), nil)
	if err == nil {
		t.Fatal("expected a refusal for a chart with no source")
	}
	if code := renderErrorCode(t, err); code != ErrInternal {
		t.Fatalf("code = %q, want %q", code, ErrInternal)
	}
}

// TestChartHashIsScopedToTheChart: an unrelated spec edit must leave a
// release's annotation — and therefore the release — untouched.
func TestChartHashIsScopedToTheChart(t *testing.T) {
	before, err := Render(chartFixture(repositoryChart()), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	r := chartFixture(repositoryChart())
	r.Components[1].Image = "ghcr.io/acme/checkout:9.9.9"
	after, err := Render(r, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if manifestYAML(t, before, "HelmRelease") != manifestYAML(t, after, "HelmRelease") {
		t.Fatal("a worker's new image changed the HelmRelease")
	}
}

func hasKind(ms []Manifest, kind string) bool {
	for _, m := range ms {
		if m.Kind == kind {
			return true
		}
	}
	return false
}

func manifestYAML(t *testing.T, ms []Manifest, kind string) string {
	t.Helper()
	for _, m := range ms {
		if m.Kind != kind {
			continue
		}
		out, err := m.YAML()
		if err != nil {
			t.Fatalf("encoding %s: %v", kind, err)
		}
		return string(out)
	}
	t.Fatalf("no %s in %v", kind, kinds(ms))
	return ""
}
