package support

import "github.com/dafrie/kelson/internal/clusterprofile"

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
	// Degrade says what a too-old version should do.
	Degrade Degrade
	// GapField is the ClusterProfile field root whose detection Gap hides this
	// component. A profile whose probe lacked RBAC to read the component is
	// Unknown, never silently fine.
	GapField string
	// Note explains the degradation decision in terms a maintainer can judge
	// when bumping the floor.
	Note string
}

// Components is the declared support matrix, in stable authoring order.
var Components = []Component{
	{
		Name:     "kubernetes",
		Minimum:  "1.27",
		Degrade:  DegradeRefuse,
		GapField: "kubernetes",
		Note: "kelson renders stock Kubernetes objects that have been stable for years; " +
			"below this floor the reasonable move is to refuse, because there is no older API to render.",
	},
	{
		Name:     "gateway-api",
		Minimum:  "1.0.0",
		Degrade:  DegradeRefuse,
		GapField: "gatewayAPI",
		Note: "the Gateway API CRDs kelson targets changed shape across experimental v0.x; " +
			"rendering against a v0 cluster would need a second manifest family, so refuse for now.",
	},
	{
		Name:     "cert-manager",
		Minimum:  "1.14.0",
		Degrade:  DegradeRefuse,
		GapField: "certManager",
		Note: "cert-manager's ClusterIssuer and Certificate CRDs used by the TLS path " +
			"stabilised with v1.14; older installs are refused with a message naming the required version.",
	},
	{
		Name:     "cnpg",
		Minimum:  "1.23.0",
		Degrade:  DegradeRefuse,
		GapField: "cnpg",
		Note: "the CNPG Cluster API version kelson writes for database services is not emitted " +
			"for older operators; the degradation is a refusal pending an older-API render path.",
	},
	{
		Name:     "flux",
		Minimum:  "2.0.0",
		Degrade:  DegradeRefuse,
		GapField: "flux",
		Note:     "flux v2's GitRepository/Kustomization API is what kelson commits against in GitOps mode.",
	},
	{
		Name:     "argo-cd",
		Minimum:  "2.9.0",
		Degrade:  DegradeRefuse,
		GapField: "argocd",
		Note:     "the Application CRD kelson targets for the Argo CD delivery path.",
	},
	{
		Name:     "metrics-server",
		Minimum:  "0.6.0",
		Degrade:  DegradeRefuse,
		GapField: "metricsServer",
		Note: "metrics-server's aggregated API is stable well above this floor; the floor guards " +
			"against installs old enough to lack the HPA autoscaling API kelson relies on.",
	},
	{
		Name:     "prometheus",
		Minimum:  "0.66.0",
		Degrade:  DegradeRefuse,
		GapField: "prometheus",
		Note:     "the prometheus-operator ServiceMonitor/PodMonitor CRD versions kelson emits.",
	},
	{
		Name:     "kyverno",
		Minimum:  "1.10.0",
		Degrade:  DegradeRefuse,
		GapField: "policyEngines",
		Note:     "kyverno's policy API version that preview consults for admission reasons (issue #45).",
	},
	{
		Name:     "gatekeeper",
		Minimum:  "3.13.0",
		Degrade:  DegradeRefuse,
		GapField: "policyEngines",
		Note:     "Gatekeeper's ConstraintTemplate/Constraint APIs preview reads for admission reasons.",
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

// gapCovers reports whether a profile detection gap hides the component whose
// field root is root. Prefix matching keeps the check robust to gaps reported
// at any depth ("certManager", "certManager.clusterIssuers").
func gapCovers(gaps []clusterprofile.Gap, root string) bool {
	for _, g := range gaps {
		if g.Field == root || len(g.Field) > len(root) && g.Field[:len(root)] == root && g.Field[len(root)] == '.' {
			return true
		}
	}
	return false
}
