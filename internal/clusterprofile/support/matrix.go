package support

// The declared support matrix (issue #57).
//
// This is the single source of truth for the oldest version kelson will
// render against for each component it adopts. Both the runtime checks
// (Check) and the generated reference doc (internal/clusterprofile/support/
// gendoc) read this slice, so the documented support matrix cannot drift from
// what the checks enforce — the exact failure the issue calls out.
//
// The Minimum values are the current floor, not eternal law: move support
// forward by editing one row here and regenerating the doc, never by editing
// the Markdown. Each row also records the explicit degradation path for a
// too-old version, because "render an older API" and "refuse with a reason"
// are different decisions a reader of the matrix must be able to distinguish.
//
// A row carries three facts beyond the floor, and each answers a question a
// bare version number cannot:
//
//   - Affects — what specifically stops working below the floor. A statement
//     that says only "too old" makes the reader guess which part of their spec
//     just became unrenderable, which is the confusion issue #57 is about.
//   - Tested — the newest version kelson is exercised against, where the repo
//     pins one. Above it is a note, never a refusal.
//   - Degrade — refuse, or render an older API.

// Degrade is the decision for a too-old version of a component. It is recorded
// as data here so the checks can expose it to callers and the doc can show it;
// the actual enforcement — telling the renderer which older API to emit or to
// refuse — lives in the renderer, which is out of this package's scope.
type Degrade string

const (
	// DegradeRefuse means a version below the minimum makes kelson refuse to
	// render and say why, rather than emit a manifest the cluster cannot serve.
	DegradeRefuse Degrade = "refuse"
	// DegradeRenderOlder means a version below the minimum is still usable,
	// but kelson must render the older API it can serve.
	DegradeRenderOlder Degrade = "render-older"
)

// Component is one adopted component's support floor and its degradation
// decision.
type Component struct {
	// Name is the canonical id used to key results: certification such as
	// "cert-manager", "kubernetes", and — for policy — the engine name.
	Name string
	// Minimum is the oldest supported version, as a string the semver
	// parser accepts.
	Minimum string
	// Tested is the newest version kelson is actually exercised against, when
	// this repository pins one. Empty means no upper bound is claimed.
	//
	// A cluster above it is a *note*, never a refusal. kelson has no evidence
	// that a newer release broke anything — only that it has not seen it — and
	// refusing on that basis would be a worse failure than the silent skew this
	// matrix exists to prevent. The ceiling is stated so a surprise on a newer
	// version reads as "outside what was tested" rather than as a mystery.
	Tested string
	// Degrade says what a too-old version should do.
	Degrade Degrade
	// GapField is the ClusterProfile field root whose detection Gap hides this
	// component. A profile whose probe lacked RBAC to read the component is
	// Unknown, never silently fine.
	GapField string
	// Affects names what specifically stops working below Minimum: the presets,
	// kinds or flows that refuse, or the manifest this cluster would reject.
	//
	// It is the half of issue #57 that makes a skew statement actionable. "Too
	// old" alone leaves a reader to guess which part of their spec just became
	// unrenderable; naming the degradation is what turns version skew into a
	// decision instead of a confusing failure well after deploy.
	Affects string
	// Note explains the degradation decision in terms a maintainer can judge
	// when bumping the floor.
	Note string
}

// Components is the declared support matrix, in stable authoring order.
var Components = []Component{
	{
		Name:     "kubernetes",
		Minimum:  "1.26",
		Tested:   "1.34",
		Degrade:  DegradeRefuse,
		GapField: "kubernetes",
		Affects: "everything kelson renders and applies — the workload, routing, autoscaling and delivery " +
			"manifests all target APIs this floor is derived from",
		Note: "the floor is derived from the APIs kelson actually emits and calls, not from upstream's own support " +
			"window: batch/v1 CronJob is GA from 1.21, server-side apply — how every delivery adapter writes — from " +
			"1.22, and autoscaling/v2 HorizontalPodAutoscaler from 1.23. The binding constraint is Gateway API v1: " +
			"kelson renders gateway.networking.k8s.io/v1 exclusively (issue #140) and the Gateway API releases that " +
			"serve it require Kubernetes 1.26 or newer. Below the floor there is no older API to render, so the " +
			"degradation is a refusal. Kelson does not refuse a cluster merely because upstream stopped patching it: " +
			"that is the operator's risk to take, and a floor that tracked the upstream window would refuse most of " +
			"the clusters kelson exists to be adopted by. The ceiling is the other half of the same honesty — Tested " +
			"is the version the e2e harness runs (hack/e2e/lib.sh pins a kindest/node image) and the Kubernetes " +
			"client libraries are one minor newer again; a cluster above it is reported as a note, because kelson has " +
			"not seen that release, not because it is known to be broken.",
	},
	{
		Name:     "gateway-api",
		Minimum:  "1.0.0",
		Degrade:  DegradeRefuse,
		GapField: "gatewayAPI",
		Affects: "all HTTP routing: the HTTPRoute kelson renders for every service that declares domains, " +
			"and the Gateway it attaches to",
		Note: "the Gateway API CRDs kelson targets changed shape across experimental v0.x; " +
			"rendering against a v0 cluster would need a second manifest family, so refuse for now.",
	},
	{
		Name:     "cert-manager",
		Minimum:  "1.14.0",
		Degrade:  DegradeRefuse,
		GapField: "certManager",
		Affects: "TLS on routed services: the cert-manager.io/v1 Certificate kelson renders when routing.tls is " +
			"set and the cluster reports a ClusterIssuer",
		Note: "cert-manager's ClusterIssuer and Certificate CRDs used by the TLS path " +
			"stabilised with v1.14; older installs are refused with a message naming the required version.",
	},
	{
		Name:     "cnpg",
		Minimum:  "1.23.0",
		Degrade:  DegradeRefuse,
		GapField: "cnpg",
		Affects: "every `kind: postgres` component: the preset judgement in internal/clusterprofile/postgres " +
			"refuses to render a database this operator cannot serve",
		Note: "the CNPG Cluster API version kelson writes for database services is not emitted " +
			"for older operators; the degradation is a refusal pending an older-API render path. " +
			"kelson targets the latest CloudNativePG release: the declarative capabilities above this floor " +
			"(managed roles from 1.20, the Database CRD from 1.25, schemas and extensions from 1.26) are reported " +
			"per capability by internal/clusterprofile/postgres rather than refused here, so an older operator " +
			"loses the features it cannot serve and keeps the ones it can (issue #90).",
	},
	{
		Name:     "valkey-operator",
		Minimum:  "0.5.0",
		Degrade:  DegradeRefuse,
		GapField: "valkey",
		Affects: "every `kind: valkey` component: the preset judgement in internal/clusterprofile/valkey " +
			"refuses to render a cache this operator cannot serve",
		Note: "the ValkeyCluster API kelson writes for cache components (ADR-0015). The floor is the " +
			"release whose CRD shape kelson renders — spec.config for maxmemory and the eviction policy, " +
			"spec.persistence, spec.podDisruptionBudget — and an older operator is refused rather than " +
			"served a manifest it would silently drop fields from. The operator's API is still v1alpha1 " +
			"and the floor is expected to move with it (issue #98).",
	},
	{
		Name:     "flux",
		Minimum:  "2.0.0",
		Degrade:  DegradeRefuse,
		GapField: "flux",
		Affects: "the delivery spine: the OCIRepository and Kustomization the kelson controller writes for " +
			"every environment, and the chart source a HelmRelease fetches from",
		Note: "flux v2's Kustomization/OCIRepository API is what the kelson controller writes (ADR-0028).",
	},
	{
		Name:     "helm-controller",
		Minimum:  "1.0.0",
		Degrade:  DegradeRefuse,
		GapField: "helmController",
		Affects: "every `kind: helm` component: the helm.toolkit.fluxcd.io/v2 HelmRelease kelson renders is " +
			"rejected by a controller that serves only v2beta1/v2beta2",
		Note: "the HelmRelease API kelson writes for `kind: helm` components is helm.toolkit.fluxcd.io/v2, " +
			"which helm-controller serves as stable from 1.0.0 (Flux 2.3). A controller below that floor serves " +
			"v2beta1/v2beta2 instead, and the difference is not cosmetic — v2 moved the chart reference and the " +
			"drift-detection settings — so kelson refuses rather than emitting a manifest the controller would " +
			"reject. It is a separate row from `flux` because a FluxInstance may install a components subset and " +
			"leave helm-controller out entirely (ADR-0016, issue #60).",
	},
	// flux-operator is deliberately not a row. Detection records it (issue
	// #157) so the delivery plane can prefer its FluxReport, but every row here
	// declares a version floor whose too-old behaviour is refuse or
	// render-older, and kelson does neither: an old or missing operator makes
	// the health readback fall back to aggregating the Flux controller
	// Deployments (internal/delivery/flux/dynamic.go). A floor here would
	// document a refusal that never happens.
	{
		Name:     "argo-cd",
		Minimum:  "2.9.0",
		Degrade:  DegradeRefuse,
		GapField: "argocd",
		Affects: "the Argo CD delivery path. None ships (issue #138) and the spine is Flux-only " +
			"(ADR-0028), so nothing degrades yet — the floor is recorded so detection can say what it saw",
		Note: "the Application CRD kelson targets for the Argo CD delivery path.",
	},
	{
		Name:     "metrics-server",
		Minimum:  "0.6.0",
		Degrade:  DegradeRefuse,
		GapField: "metricsServer",
		Affects: "autoscaling: the autoscaling/v2 HorizontalPodAutoscaler kelson renders for a component with " +
			"a replica range has no metrics source to scale on",
		Note: "metrics-server's aggregated API is stable well above this floor; the floor guards " +
			"against installs old enough to lack the HPA autoscaling API kelson relies on.",
	},
	{
		Name:     "prometheus",
		Minimum:  "0.66.0",
		Degrade:  DegradeRefuse,
		GapField: "prometheus",
		Affects:  "scrape configuration: the monitoring.coreos.com/v1 ServiceMonitor kelson renders per service",
		Note:     "the prometheus-operator ServiceMonitor/PodMonitor CRD versions kelson emits.",
	},
	{
		Name:     "kyverno",
		Minimum:  "1.10.0",
		Degrade:  DegradeRefuse,
		GapField: "policyEngines",
		Affects: "preview's admission explanations: a rejected resource is still reported, but without naming " +
			"the policy that rejected it (issue #45)",
		Note: "kyverno's policy API version that preview consults for admission reasons (issue #45).",
	},
	{
		Name:     "gatekeeper",
		Minimum:  "3.13.0",
		Degrade:  DegradeRefuse,
		GapField: "policyEngines",
		Affects: "preview's admission explanations: a rejected resource is still reported, but without naming " +
			"the constraint that rejected it (issue #45)",
		Note: "Gatekeeper's ConstraintTemplate/Constraint APIs preview reads for admission reasons.",
	},
}

// Lookup returns the support entry for a component by name, and whether it is
// tracked. Policy engines are keyed by engine name (kyverno, gatekeeper).
func Lookup(name string) (Component, bool) {
	for _, c := range Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}
