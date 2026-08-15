package model

import (
	"slices"
	"strings"
	"testing"
)

// The rules ADR-0016 decision 4 added to the leaf: the chart coordinates a
// `kind: helm` component must name, and the field boundaries between it and the
// other two halves of the one components list.

// TestHelmComponentValidates is the shape an author writes, in both source
// forms, with nothing else set.
func TestHelmComponentValidates(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - name: ingress
      kind: helm
      chart: ingress-nginx
      chartVersion: 4.11.3
      source: {repository: "https://kubernetes.github.io/ingress-nginx"}
      values:
        controller: {replicaCount: 2}
      valuesFrom:
        - {secretRef: ingress-tls-values}
    - name: podinfo
      kind: helm
      chart: podinfo
      chartVersion: 6.7.1
      source: {oci: "oci://ghcr.io/stefanprodan/charts"}
    - {name: web, port: 8080}
`))
	if len(errs) > 0 {
		t.Fatalf("a helm component must validate, got:\n%v", errs)
	}
	comps := docs[0].(*Project).Spec.Components
	for i := 0; i < 2; i++ {
		if got := comps[i].EffectiveKind(); got != ComponentHelm {
			t.Errorf("components[%d] kind = %q, want %q", i, got, ComponentHelm)
		}
		if !comps[i].EffectiveKind().IsChart() {
			t.Errorf("components[%d] must be a chart kind", i)
		}
	}
	if comps[0].EffectiveKind().IsData() || comps[0].EffectiveKind().IsWorkload() {
		t.Error("a helm component is neither a data component nor a workload")
	}
}

// TestHelmChartVersionMustBePinned is the reproducibility rule of ADR-0016. An
// unpinned chart installs different manifests on different days, and the diff —
// which only ever shows the HelmRelease — would report no change at all.
func TestHelmChartVersionMustBePinned(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  components:
    - {name: ingress, kind: helm, chart: ingress-nginx, source: {repository: "https://charts.example"}}
`)
	var got *Error
	for i := range errs {
		if errs[i].Field == "$.spec.components[0].chartVersion" {
			got = &errs[i]
		}
	}
	if got == nil {
		t.Fatalf("an unpinned chart must be refused, got:\n%v", errs)
	}
	if got.Code != ErrMissingRequired {
		t.Errorf("code = %q, want %q", got.Code, ErrMissingRequired)
	}
	if !strings.Contains(got.Remediation, "chartVersion:") {
		t.Errorf("the remediation must say to pin the version: %q", got.Remediation)
	}
}

// TestHelmChartNameRequired: a chart component that names no chart configures
// nothing at all.
func TestHelmChartNameRequired(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  components:
    - {name: ingress, kind: helm, chartVersion: "1.0.0", source: {repository: "https://charts.example"}}
`)
	if !hasFieldCode(errs, "$.spec.components[0].chart", ErrMissingRequired) {
		t.Fatalf("a chart component must name a chart, got:\n%v", errs)
	}
}

// TestHelmSourceIsExactlyOne: the two forms render different Flux source kinds,
// so both set is as wrong as neither — kelson would have to pick one and the
// author would not know which.
func TestHelmSourceIsExactlyOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		source   string
		wantCode Code
	}{
		{"neither", "", ErrMissingRequired},
		{"empty block", "source: {}", ErrMissingRequired},
		{"both", `source: {repository: "https://charts.example", oci: "oci://ghcr.io/acme/charts"}`, ErrMutuallyExclusive},
		{"repository is not a URL", `source: {repository: "charts.example"}`, ErrInvalidFormat},
		{"oci without the scheme", `source: {oci: "ghcr.io/acme/charts"}`, ErrInvalidFormat},
		{"repository given an oci URL", `source: {repository: "oci://ghcr.io/acme/charts"}`, ErrInvalidFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := decodeProjectSpec(t, `  image: i:1
  components:
    - {name: ingress, kind: helm, chart: c, chartVersion: "1.0.0", `+tc.source+`}
`)
			if !slices.Contains(errs.Codes(), tc.wantCode) {
				t.Fatalf("want %s, got:\n%v", tc.wantCode, errs)
			}
		})
	}
}

// TestHelmComponentRejectsWorkloadFields is the #141 gate pattern applied to
// the third half of the list: what a chart runs is the chart's business, so a
// port or an image on it attaches to nothing.
func TestHelmComponentRejectsWorkloadFields(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: ingress
      kind: helm
      chart: ingress-nginx
      chartVersion: 4.11.3
      source: {repository: "https://charts.example"}
      port: 8080
      health: /healthz
      image: nginx:1
      replicas: {min: 3}
      env: {LOG_LEVEL: info}
      preset: small
`)
	for _, want := range []string{"port", "health", "image", "replicas", "env", "preset"} {
		field := "$.spec.components[0]." + want
		if !hasFieldCode(errs, field, ErrMutuallyExclusive) {
			t.Errorf("a helm component setting %s must be rejected, got:\n%v", want, errs)
		}
	}
}

// TestChartFieldsRejectedOnOtherKinds is the mirror image, and it covers both
// other halves: a `chart:` on a worker or on a database renders nothing.
func TestChartFieldsRejectedOnOtherKinds(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: web
      port: 8080
      chart: ingress-nginx
      chartVersion: 4.11.3
    - name: db
      kind: postgres
      source: {repository: "https://charts.example"}
      values: {a: b}
      valuesFrom: [{secretRef: s}]
`)
	for _, want := range []string{
		"$.spec.components[0].chart",
		"$.spec.components[0].chartVersion",
		"$.spec.components[1].source",
		"$.spec.components[1].values",
		"$.spec.components[1].valuesFrom",
	} {
		if !hasFieldCode(errs, want, ErrMutuallyExclusive) {
			t.Errorf("%s must be rejected on a non-helm component, got:\n%v", want, errs)
		}
	}
	for _, e := range errs {
		if e.Field == "$.spec.components[0].chart" && !strings.Contains(e.Remediation, "kind: helm") {
			t.Errorf("the remediation must name the kind that makes the field real: %q", e.Remediation)
		}
	}
}

// TestValuesFromIsExactlyOneReference: an entry naming both a Secret and a
// ConfigMap would have to become two, and guessing which one the author meant
// is not kelson's to do.
func TestValuesFromIsExactlyOneReference(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    string
		wantCode Code
	}{
		{"neither", "{}", ErrMissingRequired},
		{"both", "{secretRef: s, configMapRef: c}", ErrMutuallyExclusive},
		{"not a name", "{secretRef: \"Not A Name\"}", ErrInvalidFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: ingress
      kind: helm
      chart: c
      chartVersion: "1.0.0"
      source: {repository: "https://charts.example"}
      valuesFrom: [`+tc.entry+`]
`)
			if !slices.Contains(errs.Codes(), tc.wantCode) {
				t.Fatalf("want %s, got:\n%v", tc.wantCode, errs)
			}
		})
	}
}

// TestChartValuesTakeAnyShapeButNonStringKeys: values are the chart author's
// vocabulary, not kelson's, so nothing about their content is inspected — no
// key is refused for looking like a credential, because content-sniffing would
// block legitimate configuration and miss the interesting cases (docs/model.md).
// The one structural rule is string keys, which Helm requires anyway and which
// keeps rendering and hashing total.
func TestChartValuesTakeAnyShapeButNonStringKeys(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: ingress
      kind: helm
      chart: c
      chartVersion: "1.0.0"
      source: {repository: "https://charts.example"}
      values:
        password: not-a-refusal
        apiKey: also-not-a-refusal
        deep: {list: [1, {a: b}, true], n: 3.5}
`)
	if len(errs) > 0 {
		t.Fatalf("values are not content-sniffed, got:\n%v", errs)
	}

	errs = decodeProjectSpec(t, `  image: i:1
  components:
    - name: ingress
      kind: helm
      chart: c
      chartVersion: "1.0.0"
      source: {repository: "https://charts.example"}
      values:
        ports:
          8080: http
`)
	if !slices.Contains(errs.Codes(), ErrInvalidFormat) {
		t.Fatalf("a non-string values key must be refused, got:\n%v", errs)
	}
}

// TestHelmOverridesAreRefused: the override block carries image, replicas,
// resources, env and preset, and a chart uses none of them. Per-environment
// values are a real ask and deliberately out of v0 — an override that silently
// did nothing is the failure #141 is about.
func TestHelmOverridesAreRefused(t *testing.T) {
	project := `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - name: ingress
      kind: helm
      chart: ingress-nginx
      chartVersion: 4.11.3
      source: {repository: "https://charts.example"}
`
	for _, tc := range []struct {
		name     string
		override string
		wantCode Code
	}{
		{"replicas", "    - {name: ingress, replicas: {min: 3}}", ErrMutuallyExclusive},
		{"image", "    - {name: ingress, image: nginx:1}", ErrMutuallyExclusive},
		{"preset", "    - {name: ingress, preset: small}", ErrMutuallyExclusive},
		{"empty", "    - {name: ingress}", ErrMissingRequired},
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
}

// TestHelmResolvesIntoItsOwnSlice: a chart carries no precedence rule, so it
// resolves through unchanged and lands beside the workloads rather than among
// them.
func TestHelmResolvesIntoItsOwnSlice(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: i:1
  components:
    - name: ingress
      kind: helm
      chart: ingress-nginx
      chartVersion: 4.11.3
      source: {oci: "oci://ghcr.io/acme/charts"}
      values: {controller: {replicaCount: 2}}
      valuesFrom: [{configMapRef: defaults}, {secretRef: creds}]
    - {name: web, port: 8080}
`, `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
`)
	resolved, errs := Resolve(p, e)
	if len(errs) > 0 {
		t.Fatalf("resolve failed:\n%v", errs)
	}
	if len(resolved.Charts) != 1 || len(resolved.Components) != 1 || len(resolved.DataServices) != 0 {
		t.Fatalf("charts=%d components=%d services=%d, want 1/1/0",
			len(resolved.Charts), len(resolved.Components), len(resolved.DataServices))
	}
	c := resolved.Charts[0]
	if c.Name != "ingress" || c.Chart != "ingress-nginx" || c.Version != "4.11.3" {
		t.Errorf("chart = %+v", c)
	}
	if c.Source.OCI != "oci://ghcr.io/acme/charts" || c.Source.Repository != "" {
		t.Errorf("source = %+v", c.Source)
	}
	if len(c.ValuesFrom) != 2 || c.ValuesFrom[0].ConfigMapRef != "defaults" || c.ValuesFrom[1].SecretRef != "creds" {
		t.Errorf("valuesFrom must keep spec order, got %+v", c.ValuesFrom)
	}
}

// TestHelmIsNotBindable: a binding names the service a data component provides,
// and a chart provides whatever it likes under names kelson does not know.
func TestHelmIsNotBindable(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  env:
    DB_URL: {from: {service: ingress, key: uri}}
  components:
    - name: ingress
      kind: helm
      chart: c
      chartVersion: "1.0.0"
      source: {repository: "https://charts.example"}
`)
	if !slices.Contains(errs.Codes(), ErrUnknownService) {
		t.Fatalf("binding to a helm component must be refused, got:\n%v", errs)
	}
}

func hasFieldCode(errs Errors, field string, code Code) bool {
	for _, e := range errs {
		if e.Field == field && e.Code == code {
			return true
		}
	}
	return false
}
