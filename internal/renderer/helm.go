package renderer

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
)

// Helm components, rendered as the two resources helm-controller and
// source-controller need and nothing else (ADR-0016 decision 4).
//
// # What kelson owns here, and what it does not
//
// kelson renders a chart *source* (a HelmRepository or an OCIRepository) and a
// HelmRelease that names it. That pair is the whole of kelson's inventory for a
// helm component: every object the chart expands into — its Deployments, its
// Services, its CRDs — is created by helm-controller under Helm's own release
// ownership, appears in no kelson manifest set, and is never pruned, adopted or
// diffed by kelson. This is ADR-0005's delegation rule applied to charts: kelson
// writes a CR, kelson does not template an engine.
//
// Two consequences follow directly and are documented rather than mitigated:
// a preview shows the HelmRelease changing (chart, version, values) and never
// the workloads the chart produces, and drift inside the release belongs to
// helm-controller's own drift detection. docs/model.md states both in the words
// a reviewer needs.
//
// # The Flux-only gate
//
// A HelmRelease applied where no helm-controller runs is a manifest that does
// nothing at all — the quiet success issue #141 exists to prevent. So a helm
// component renders only when the environment's delivery mode is flux, and the
// refusal is structured.
//
// The gate lives here, in the pure renderer, because the delivery mode is spec
// data: it is resolved from the Environment document (P4) and arrives on
// model.Resolved. It is emphatically not read from the cluster — whether
// helm-controller is *installed* is a ClusterProfile question, judged by
// internal/clusterprofile/helm for the capability surfaces, and deliberately not
// consulted here. Deciding renderability from cluster state would make the same
// document render differently against two clusters.
//
// ADR-0016 records this as the first delivery-mode-gated spec surface and as a
// decision taken for chart delegation only, not a pattern to reach for.

const (
	// helmReleaseAPIVersion is helm-controller's stable API. It matches the
	// version the delivery plane already reads HelmRelease status from
	// (internal/delivery/flux/dynamic.go) — one API version per group across
	// kelson, or the reader and the writer would eventually disagree.
	helmReleaseAPIVersion = "helm.toolkit.fluxcd.io/v2"
	// helmRepositoryAPIVersion is source-controller's stable API for classic
	// Helm repositories.
	helmRepositoryAPIVersion = "source.toolkit.fluxcd.io/v1"
	// ociRepositoryAPIVersion is source-controller's API for OCI artifacts. It
	// is still v1beta2 while HelmRepository is v1, and that asymmetry is
	// upstream's rather than a choice here: OCIRepository was promoted later,
	// and writing v1 for it would produce a manifest older source-controllers
	// reject for no gain.
	ociRepositoryAPIVersion = "source.toolkit.fluxcd.io/v1beta2"
)

const (
	// chartSourceInterval is how often source-controller re-checks the source
	// for the pinned version. It is not how often anything upgrades: the version
	// is pinned, so this only ever finds the same artifact again, and an hour is
	// enough to notice a re-pushed tag without polling a public registry every
	// few minutes.
	chartSourceInterval = "1h"
	// chartReleaseInterval is helm-controller's reconcile interval for the
	// release: how quickly it notices and corrects drift inside the chart's own
	// objects. Ten minutes matches Flux's own documented default for a
	// HelmRelease and is the layer kelson explicitly does not police itself.
	chartReleaseInterval = "10m"
)

// helmChartLayerMediaType selects the chart layer inside an OCI artifact.
// An OCI chart is an artifact with a single layer of this media type; naming it
// (with operation: copy) is what tells source-controller to unpack a chart
// rather than treat the artifact as opaque content.
const helmChartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// maxChartResourceName caps <project>-<environment>-<component>, because that
// name becomes the Helm release name. Helm itself refuses a release name longer
// than 53 characters, and it refuses it at install time inside the controller —
// a failure that surfaces as a HelmRelease condition long after the apply that
// a reader would connect it to. Refusing at render time costs nothing and points
// at the field.
const maxChartResourceName = 53

// helmRequiresFlux is the delivery-mode gate. It reports every helm component
// in the resolved spec, not just the first: one run should list all the work.
//
// It is called from Render alongside unresolvedImages, before anything is
// emitted, so a spec that cannot render produces errors rather than a partial
// manifest set.
func helmRequiresFlux(resolved *model.Resolved) Errors {
	if len(resolved.Charts) == 0 || resolved.Environment.Mode == model.DeliveryFlux {
		return nil
	}
	var errs Errors
	for i := range resolved.Charts {
		c := &resolved.Charts[i]
		errs = append(errs, Error{
			Code:        ErrHelmRequiresFlux,
			Application: c.Name,
			Message: "component " + quoted(c.Name) + " has kind " + quoted(string(model.ComponentHelm)) +
				", which renders a HelmRelease for helm-controller to reconcile, but environment " +
				quoted(resolved.Environment.Name) + " has delivery mode " +
				quoted(string(resolved.Environment.Mode)),
			Remediation: "set delivery.mode: flux on this environment, or remove the helm component. " +
				"A helm component is available in Flux mode only and ADR-0016 decides that deliberately: " +
				"kelson delegates charts to helm-controller instead of templating them, and a HelmRelease " +
				"applied where nothing reconciles it is a manifest that does nothing",
		})
	}
	return errs
}

// chartManifests renders one helm component: its chart source, then the
// HelmRelease that names it. Source first because the release references it,
// and a rendered set expresses sequencing only through order (issue #89).
func chartManifests(resolved *model.Resolved, chart *model.ResolvedChart) ([]Manifest, error) {
	name := scopedResourceName(resolved.Project, resolved.Environment.Name, chart.Name)
	if len(name) > maxChartResourceName {
		return nil, Errors{{
			Code:        ErrChartName,
			Application: chart.Name,
			Message: "the Helm release name for component " + quoted(chart.Name) + " would be " +
				quoted(name) + ", " + strconv.Itoa(len(name)) + " characters",
			Remediation: "shorten the project, environment or component name so that " +
				"<project>-<environment>-<component> is at most " + strconv.Itoa(maxChartResourceName) +
				" characters; Helm refuses a longer release name inside the controller, where the failure " +
				"is far from the field that caused it",
		}}
	}

	hash, herr := chartHash(resolved, chart, name)
	if herr != nil {
		return nil, Errors{{Code: ErrInternal, Application: chart.Name, Message: herr.Error()}}
	}
	prov := provenance{
		project:      resolved.Project,
		environment:  resolved.Environment.Name,
		component:    chart.Name,
		resourceName: name,
		namespace:    resolved.Environment.Namespace,
		specHash:     hash,
	}

	source, sourceKind, err := chartSourceManifest(chart, prov)
	if err != nil {
		return nil, err
	}
	return []Manifest{source, helmRelease(resolved, chart, prov, sourceKind)}, nil
}

// chartSourceManifest renders the source the release fetches its chart from,
// and returns the kind the release must reference.
//
// The two shapes carry the version pin in different places, which is upstream's
// design rather than an inconsistency kelson could flatten: a HelmRepository is
// an index that many charts and versions live in, so the pin belongs to the
// release; an OCIRepository *is* one chart, so the pin is its tag.
func chartSourceManifest(chart *model.ResolvedChart, prov provenance) (Manifest, string, error) {
	switch {
	case chart.Source.Repository != "":
		spec := mapNode(
			"interval", chartSourceInterval,
			"url", chart.Source.Repository,
		)
		return baseManifest(helmRepositoryAPIVersion, "HelmRepository", prov, spec), "HelmRepository", nil
	case chart.Source.OCI != "":
		spec := mapNode(
			"interval", chartSourceInterval,
			// The chart name is part of an OCI chart's address, so the spec's
			// registry URL and chart name meet here. That is why `source.oci`
			// is documented as the registry path *without* the chart: one
			// registry value serves every chart it publishes.
			"url", chart.Source.OCI+"/"+chart.Chart,
			"layerSelector", mapNode(
				"mediaType", helmChartLayerMediaType,
				"operation", "copy",
			),
			"ref", mapNode("tag", chart.Version),
		)
		return baseManifest(ociRepositoryAPIVersion, "OCIRepository", prov, spec), "OCIRepository", nil
	}
	// Unreachable for a validated spec: model validation requires exactly one
	// source. Kept, and loud, for the same reason serviceManifests keeps its
	// fallthrough — the renderer is also fed a Resolved directly by the API
	// plane, and a chart with no source must never render half a release.
	return Manifest{}, "", Errors{{
		Code:        ErrInternal,
		Application: chart.Name,
		Message:     "component " + quoted(chart.Name) + " reached the renderer with no chart source",
		Remediation: "set source.repository or source.oci on the component",
	}}
}

// helmRelease renders the HelmRelease itself.
//
// targetNamespace is written explicitly even though it equals the namespace the
// HelmRelease lives in. It is the field that says where the chart's objects go,
// and leaving it to a default would mean a reader has to know helm-controller's
// defaulting rules to answer the first question they will ask about a chart
// component.
func helmRelease(resolved *model.Resolved, chart *model.ResolvedChart, prov provenance, sourceKind string) Manifest {
	specKV := []any{"interval", chartReleaseInterval}
	if sourceKind == "OCIRepository" {
		// chartRef is the OCI path: the source *is* the chart, pinned by its
		// tag, so there is nothing left for the release to select.
		specKV = append(specKV, "chartRef", mapNode(
			"kind", sourceKind,
			"name", prov.name(),
		))
	} else {
		specKV = append(specKV, "chart", mapNode(
			"spec", mapNode(
				"chart", chart.Chart,
				"version", chart.Version,
				"sourceRef", mapNode(
					"kind", sourceKind,
					"name", prov.name(),
				),
			),
		))
	}
	specKV = append(specKV, "targetNamespace", resolved.Environment.Namespace)
	if len(chart.ValuesFrom) > 0 {
		specKV = append(specKV, "valuesFrom", valuesFromNode(chart.ValuesFrom))
	}
	if len(chart.Values) > 0 {
		// Verbatim, and last: helm-controller merges valuesFrom first and lets
		// the inline values win, which is the precedence the spec's own reader
		// would assume from the order they appear in.
		specKV = append(specKV, "values", valuesNode(chart.Values))
	}
	return baseManifest(helmReleaseAPIVersion, "HelmRelease", prov, mapNode(specKV...))
}

// valuesFromNode renders the Secret and ConfigMap references in spec order.
func valuesFromNode(refs []model.ValuesFrom) *yaml.Node {
	items := make([]*yaml.Node, 0, len(refs))
	for _, ref := range refs {
		kind, name := "ConfigMap", ref.ConfigMapRef
		if ref.SecretRef != "" {
			kind, name = "Secret", ref.SecretRef
		}
		items = append(items, mapNode("kind", kind, "name", name))
	}
	return seqNode(items...)
}

// valuesNode turns the spec's inline values into a YAML node, with mapping keys
// sorted at every level.
//
// Sorting is what makes the output deterministic: a Go map has no order, so
// emitting one in iteration order would produce a different document per run
// and the golden harness would (rightly) call it non-determinism. The cost is
// that a hand-written values block comes back alphabetised, which is a
// presentation change to a document kelson generates, not to the one the author
// wrote.
func valuesNode(values map[string]any) *yaml.Node {
	kv := make([]any, 0, len(values)*2)
	for _, k := range sortedKeys(values) {
		kv = append(kv, k, valueNode(values[k]))
	}
	return mapNode(kv...)
}

// valueNode renders one chart value of any YAML shape.
//
// Scalars go through yaml.Node's own encoding rather than a hand-rolled
// switch on Go types, so a value round-trips as what it was written as: `8080`
// stays an integer and `"8080"` stays a string, which for a chart's values is
// the difference between a working release and a type error from the chart's
// own templates.
func valueNode(v any) *yaml.Node {
	switch t := v.(type) {
	case map[string]any:
		return valuesNode(t)
	case []any:
		items := make([]*yaml.Node, 0, len(t))
		for _, item := range t {
			items = append(items, valueNode(item))
		}
		return seqNode(items...)
	default:
		var n yaml.Node
		if err := n.Encode(t); err != nil {
			// yaml.Node.Encode fails only on a type YAML cannot represent, and
			// the values map is decoded *from* YAML, so nothing here can be one.
			// Rendering the Go value's text is a last resort that stays
			// deterministic rather than losing the field silently.
			return strNode(fmt.Sprint(t))
		}
		return &n
	}
}

// chartHash is the component's kelson.dev/spec-hash. Like a data service's, it
// covers only what this component's own manifests are built from, so an
// unrelated spec edit leaves a release's annotation — and therefore the release
// — untouched.
func chartHash(resolved *model.Resolved, chart *model.ResolvedChart, name string) (string, error) {
	return hashJSON(struct {
		Project     string              `json:"project"`
		Environment string              `json:"environment"`
		Namespace   string              `json:"namespace"`
		Resource    string              `json:"resource"`
		Chart       model.ResolvedChart `json:"chart"`
	}{
		Project:     resolved.Project,
		Environment: resolved.Environment.Name,
		Namespace:   resolved.Environment.Namespace,
		Resource:    name,
		Chart:       *chart,
	})
}
