package install

import (
	"sort"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// The pins table (issue #60).
//
// This is the single source of truth for what `kelson install` will fetch and
// apply. One row per component, and everything a row promises is checkable
// without reading any Go: the exact upstream URL, the exact bytes at it, and
// the namespace the manifest creates.
//
// # Updating a pin
//
// Editing a version means editing three fields together — Version, ManifestURL
// and SHA256 — and the drift test in pins_test.go fails when they disagree, so
// a half-updated row cannot merge. The procedure is:
//
//	curl -fsSLO <the new ManifestURL>
//	sha256sum <the downloaded file>
//
// and paste both into the row. Do not compute the digest from a mirror, a proxy
// cache or a locally re-serialized copy: the value here is what the bytes at
// that URL hash to, and an install compares against it before it applies
// anything (see [Installer.Plan]).
//
// Nothing here is vendored. The repository holds the URL and the digest and
// never a copy of the manifest — ADR-0005, and the Bitnami lesson in the
// package doc.
//
// # Why some rows are deferred
//
// A deferred row is a refusal with a reason and a follow-up, not a silence.
// `kelson install external-secrets` says why it will not install it and names
// the issue, which is more useful than an "unknown component" error that makes
// the user wonder whether they typed it wrong.

// Status is whether a row can be installed today.
type Status string

const (
	// StatusSupported means this component installs.
	StatusSupported Status = "supported"
	// StatusDeferred means kelson knows the component, detects it, renders
	// against it — and declines to install it, for the reason in FollowUp.
	StatusDeferred Status = "deferred"
)

// Component is one installable platform component and its pin.
type Component struct {
	// Name is the CLI token and the value of the kelson.dev/installed-component
	// label. It matches the support-matrix name where one exists
	// (internal/clusterprofile/support), so the two tables can be read together.
	Name string
	// Title is what the component calls itself, for output addressed to humans.
	Title string
	// Status is whether this row installs.
	Status Status
	// Version is the upstream release pinned, exactly as upstream tags it.
	Version string
	// ManifestURL is the upstream-published install manifest for that release.
	// It must contain Version: a URL and a version that disagree is a pin that
	// documents one thing and installs another. Empty when Authored is true —
	// there is no manifest to fetch.
	ManifestURL string
	// SHA256 is the hex digest of the bytes kelson applies. For an ordinary row
	// those are the bytes at ManifestURL and an install that fetches anything
	// else refuses and applies nothing; for a Rendered row they are the
	// committed snapshot at RenderedPath, checked before it is decoded. Empty
	// when Authored is true, for the same reason as ManifestURL — and empty on
	// a Rendered row whose snapshot has not been generated yet, which is a
	// build that refuses to install it (fluxaio.go).
	SHA256 string
	// Authored marks a row whose manifest kelson composes itself rather than
	// fetching one — the exception ADR-0021 §2 states and ADR-0030's amendment
	// records a second instance of. ManifestURL and SHA256 are unset; Image and
	// ImageDigest carry the pin instead, and Installer.load builds the objects
	// in Go (see registry.go) rather than downloading and decoding YAML.
	Authored bool
	// Image is the pinned container image reference, without a tag or digest,
	// for an Authored row. Meaningless when Authored is false.
	Image string
	// ImageDigest is the hex sha256 digest of Image at Version, so the object
	// kelson authors always pulls image@sha256:<ImageDigest> — never a mutable
	// tag — whatever registry.k8s.io or GHCR later moves. Meaningless when
	// Authored is false.
	ImageDigest string
	// Rendered marks a row whose manifests kelson RENDERS at release time from
	// an upstream module and commits, rather than fetching at install time —
	// ADR-0030 decision 2, the first of the two exceptions ADR-0021 §2 carves
	// out and the reason that ADR exists. ManifestURL is unset because upstream
	// publishes no install manifest to point at; the three Module fields below
	// record which upstream produced the bytes and SHA256 records kelson's own
	// digest of them, so both questions the ADR names — *which upstream is this*
	// and *are these the bytes kelson shipped* — are answered in this table.
	Rendered bool
	// RenderedPath is where the committed snapshot lives inside the embedded
	// filesystem (fluxaio.go). Meaningless when Rendered is false.
	RenderedPath string
	// ModuleRef is the OCI reference of the upstream module the snapshot was
	// rendered from, exactly as the render script pulls it. Meaningless when
	// Rendered is false.
	ModuleRef string
	// ModuleVersion is that module's own tag. It is documentation and a
	// cross-check — ModuleDigest is what is pulled — and it must package
	// Version, which the render script asserts before it renders anything.
	ModuleVersion string
	// ModuleDigest is the hex sha256 of the module's OCI manifest: the exact
	// upstream artifact the committed bytes came out of, never a tag. This is
	// the "which upstream did this come from" half of the provenance; SHA256 is
	// the "are these the bytes kelson shipped" half.
	ModuleDigest string
	// Namespace is the namespace the manifest creates and the component runs
	// in, reported in the preview so a user knows where it is about to land.
	Namespace string
	// ProfileField is the ClusterProfile field root whose detection Gap makes
	// this component's presence Unknown (clusterprofile.GapFor). Empty when no
	// ClusterProfile signal exists for this component at all — today only
	// "registry": kelson has no way to observe whether a cluster already has
	// one, so Presence never rises above No and the offer is
	// unconditional-but-explicit rather than presence-gated (docs/install.md).
	ProfileField string
	// Provides is what installing this unlocks, in terms of what kelson renders.
	// A component list that says only "cert-manager" makes the reader guess why
	// they would want it.
	Provides string
	// FollowUp is the reason a deferred row is deferred, and where the work is
	// tracked. Empty for a supported row.
	FollowUp string
}

// Pinned Flux distribution for the FluxInstance `kelson install flux` creates.
//
// The version is a minor-pinned semver expression rather than an exact release
// on purpose. flux-operator owns the install and upgrade lifecycle of the Flux
// controllers (ADR-0016) and reconciles patch releases within the expression by
// itself; pinning an exact patch here would mean kelson taking custody of an
// upgrade cadence it explicitly delegates. The minor is pinned because a minor
// bump is an API-surface decision — internal/clusterprofile/support carries the
// floors kelson renders against — and that one is kelson's to make deliberately.
const (
	// FluxDistributionVersion is the semver expression in FluxInstance
	// spec.distribution.version.
	FluxDistributionVersion = "2.9.x"
	// FluxDistributionRegistry is the registry the operator pulls Flux images
	// from. Upstream's own default, named explicitly because the CRD requires it.
	FluxDistributionRegistry = "ghcr.io/fluxcd"
)

// FluxComponents is the controller set the FluxInstance asks for.
//
// helm-controller is in the list deliberately: `kind: helm` components render a
// HelmRelease and a FluxInstance may legally install a subset that leaves that
// controller out, which is a cluster running Flux with nothing to reconcile a
// chart (ADR-0016, internal/clusterprofile/helm). image-reflector-controller and
// image-automation-controller are left out because kelson renders nothing that
// uses them, and an unused controller is footprint a user did not ask for.
var FluxComponents = []string{
	"source-controller",
	"kustomize-controller",
	"helm-controller",
	"notification-controller",
}

// Components is the pins table, in stable authoring order.
var Components = []Component{
	// flux-aio comes first because table order is offer order, and ADR-0030
	// decision 1 makes this the default offer on a cluster with no Flux at all:
	// every Flux controller in one pod, one Deployment, one ServiceAccount,
	// sized for the k3s and edge clusters that ADR-0028's hard Flux requirement
	// would otherwise shut out. Full Flux through flux-operator is the row
	// below, and stays an explicit choice.
	//
	// This is the row that costs ADR-0021 §2 its "nothing is vendored" rule,
	// and the price is stated rather than skirted. Upstream publishes flux-aio
	// ONLY as a timoni module, so there is no install.yaml to pin a URL and a
	// digest against. What is committed instead is a mechanically regenerated
	// snapshot: hack/flux-aio-render.sh runs a checksum-verified timoni binary
	// against ModuleDigest, writes RenderedPath, and .github/workflows/release.yml
	// fails the release when the committed bytes are not what those two pins
	// produce. ModuleRef/ModuleVersion/ModuleDigest say where the bytes came
	// from; SHA256 says they are the bytes kelson shipped.
	//
	// Nothing about timoni reaches a user, a cluster or go.mod (ADR-0030
	// decision 3). The binary runs once per kelson release, on a CI runner, and
	// its output is bytes.
	//
	// ProfileField is "flux" — the same finding the row below reads, and
	// deliberately so. A cluster with Flux is adopted whatever installed it:
	// `flux bootstrap`, flux-operator, flux-aio, a vendor's distribution. The
	// finding is what matters, not its provenance (ADR-0030 decision 1).
	{
		Name:   FluxAIOName,
		Title:  "flux-aio (every Flux controller in one pod)",
		Status: StatusSupported,
		// The Flux release the pinned module packages, which is what
		// kelson.dev/installed-version stamps and what the support floor is
		// checked against. It must stay inside FluxDistributionVersion's minor
		// for the reason hack/flux-crds.sh gives: the Flux this product
		// installs and the Flux schemas its tests validate against have to be
		// the same Flux.
		Version:       "v2.9.4",
		Rendered:      true,
		RenderedPath:  "rendered/flux-aio.yaml",
		ModuleRef:     "oci://ghcr.io/stefanprodan/modules/flux-aio",
		ModuleVersion: "2.9.4-0",
		ModuleDigest:  "2fdfc00b5a1b59017f63ec0ab78be8b013fa7d542a57d6f1a6db4df64eab5a5a",
		// SHA256 is filled in by hack/flux-aio-render.sh, which prints the
		// block to paste here. It is empty until the snapshot is generated and
		// committed, and a build in that state REFUSES to install this row
		// rather than installing something unpinned — see fluxaio.go and
		// TestRenderedSnapshotMatchesThePin, which fail loudly in both
		// directions.
		SHA256:       "",
		Namespace:    "flux-system",
		ProfileField: "flux",
		Provides: "the delivery spine (ADR-0028) on a cluster that has no Flux: the OCIRepository and " +
			"Kustomization pair kelson-controller writes, and the helm-controller every `kind: helm` " +
			"component needs — in one pod rather than six. It installs no flux-operator, so PR previews " +
			"need `kelson install flux` as well (ADR-0030 decision 4)",
	},
	{
		Name:         FluxOperatorName,
		Title:        "flux-operator (and the Flux controllers it installs)",
		Status:       StatusSupported,
		Version:      "v0.58.0",
		ManifestURL:  "https://github.com/controlplaneio-fluxcd/flux-operator/releases/download/v0.58.0/install.yaml",
		SHA256:       "51c707087ca7b6d342b71b79270d13416b2a1b24fb30e39ebc8ac07922e1b56a",
		Namespace:    "flux-system",
		ProfileField: "flux",
		Provides: "the delivery spine (ADR-0028): the OCIRepository and Kustomization the kelson controller " +
			"writes, and the helm-controller every `kind: helm` component needs",
	},
	{
		Name:         "cert-manager",
		Title:        "cert-manager",
		Status:       StatusSupported,
		Version:      "v1.21.1",
		ManifestURL:  "https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml",
		SHA256:       "5f6a499b8c1857d57f560f536e0dcc830914b45c420899fe7ad0692c8624e408",
		Namespace:    "cert-manager",
		ProfileField: "certManager",
		Provides: "TLS on routed services: the cert-manager.io/v1 Certificate kelson renders when routing.tls " +
			"is set. It installs no ClusterIssuer — which ACME account or CA to trust is your decision, not " +
			"kelson's, and detection reports the issuers you create",
	},
	{
		Name:         "cnpg",
		Title:        "CloudNativePG",
		Status:       StatusSupported,
		Version:      "v1.30.0",
		ManifestURL:  "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.0/cnpg-1.30.0.yaml",
		SHA256:       "f8bede43fe4ee0d478c2355b204a36876b2ae4faac60f2a9452280b293da3b88",
		Namespace:    "cnpg-system",
		ProfileField: "cnpg",
		Provides: "every `kind: postgres` component (ADR-0005, ADR-0007): the operator that backs the managed " +
			"database type, its presets, and the declarative capabilities internal/clusterprofile/postgres " +
			"reports per version",
	},
	// envoy-gateway was a deferred row until 2026-08-14, on the ground that
	// installing a Gateway API implementation claims a GatewayClass beside a
	// cluster's existing routing. That premise does not hold for the pinned
	// release: upstream's install.yaml creates the Gateway API CRDs, the
	// controller and its namespace, and NO GatewayClass — Envoy Gateway acts
	// only on classes naming gateway.envoyproxy.io/gatewayclass-controller, so
	// the install carries no traffic and touches nobody's routing until the
	// user creates one (the same installed-not-configured boundary as
	// cert-manager's ClusterIssuer). What survives of the original concern is a
	// footprint rule, enforced in refuse(): a --all-missing sweep declines this
	// row when detection reports an ingress stack, because adding a second
	// routing implementation is a decision the user must make by name.
	{
		Name:         "envoy-gateway",
		Title:        "Envoy Gateway",
		Status:       StatusSupported,
		Version:      "v1.8.3",
		ManifestURL:  "https://github.com/envoyproxy/gateway/releases/download/v1.8.3/install.yaml",
		SHA256:       "37a62afe9bb07d87e86c5c2cff32f046f17397cb4fca9f2a741165826212d781",
		Namespace:    "envoy-gateway-system",
		ProfileField: "gatewayAPI",
		Provides: "all HTTP routing: the Gateway API CRDs and a controller that implements them, which every " +
			"HTTPRoute kelson renders needs (issue #140). It creates no GatewayClass — which class carries " +
			"traffic is your decision, made after the install",
	},
	// registry is bring-your-own by default (docs/install.md): the spine
	// (ADR-0028) pushes rendered-manifest OCI artifacts, and `kelson build`
	// pushes app images, and most teams already have somewhere to put them.
	// This row exists for the cluster that does not — self-contained,
	// lightweight, k3s-and-edge-shaped, the same audience ADR-0030 wrote
	// flux-aio for.
	//
	// CNCF Distribution (registry:2 / `distribution/distribution`, the project
	// ADR-0021 decision 2's rule is written for) publishes no install.yaml —
	// there is no Kubernetes manifest to pin a URL and a digest against, only a
	// container image. So this row is Authored (ADR-0030's 2026-08-14
	// amendment, the second instance of the exception decision 2's own §2
	// already names): the Deployment, Service and PersistentVolumeClaim are
	// kelson's own, written in registry.go, and what is pinned is the image —
	// ghcr.io/distribution/distribution, the project's own registry rather
	// than a Docker Official Images mirror of it, by digest, never a mutable
	// tag.
	//
	// ProfileField is deliberately empty. No ClusterProfile finding says
	// whether a cluster already has a registry — kelson cannot see credentials
	// a user configured out of band, a registry running outside the cluster,
	// or one behind a proxy — so Presence can only ever answer No, never Yes
	// or Unknown, and installing this is always an explicit choice
	// (refuse()'s registry/--all-missing case is where that is enforced: named
	// installs it, a sweep never does).
	{
		Name:         "registry",
		Title:        "an in-cluster OCI registry (CNCF Distribution)",
		Status:       StatusSupported,
		Version:      "3.1.1",
		Authored:     true,
		Image:        "ghcr.io/distribution/distribution",
		ImageDigest:  "bca24727f4002e51f959c18c42e816e4d1078198081a9837e16b8b7d7e43ebf8",
		Namespace:    RegistryNamespace,
		ProfileField: "",
		Provides: "somewhere to push: the delivery spine's rendered-manifest OCI artifacts (ADR-0028) and " +
			"`kelson build`'s app images. The cluster-internal endpoint is " + RegistryEndpoint + ", plain " +
			"HTTP, so anything that pushes or pulls through it must be told it is insecure " +
			"(--insecure-registries / $KELSON_INSECURE_REGISTRIES) — bring-your-own remains the default and " +
			"the recommendation for a team; this is the self-contained-cluster offer",
	},
	{
		Name:         "external-secrets",
		Title:        "external-secrets",
		Status:       StatusDeferred,
		Namespace:    "external-secrets",
		ProfileField: "externalSecrets",
		Provides: "the `externalSecrets` secret backend (ADR-0020): the ExternalSecret kelson renders per " +
			"referenced Secret, so no kelson process ever holds a secret value",
		FollowUp: "upstream publishes external-secrets as a Helm chart and no plain install manifest, so " +
			"installing it means rendering a chart. Embedding a Helm engine to do that is a larger decision " +
			"than this issue makes — ADR-0021 §3 records the trade — and vendoring a rendered copy is exactly " +
			"the custody ADR-0005 refuses. Tracked as a follow-up on issue #60",
	},
}

// Lookup returns the pin for a component by name, and whether it is tracked.
func Lookup(name string) (Component, bool) {
	for _, c := range Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// Names returns every component name the table knows, sorted. It is what the
// CLI prints when it is given a name that is not in the table — a list of the
// real answers beats "unknown component".
func Names() []string {
	out := make([]string, 0, len(Components))
	for _, c := range Components {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// Supported returns the rows that can be installed today, in table order.
func Supported() []Component {
	var out []Component
	for _, c := range Components {
		if c.Status == StatusSupported {
			out = append(out, c)
		}
	}
	return out
}

// Presence is what detection says about this component, and why.
//
// The Outcome is the whole point. Yes means detection found it, and the install
// is refused because kelson must never modify a component it did not install.
// No means detection looked and it is absent, which is the only case that
// installs. Unknown means a detection Gap hid it — the probe lacked the RBAC to
// see the API group — and installing on an unknown would be the guess the
// tri-state exists to prevent (issue #144).
func (c Component) Presence(prof clusterprofile.ClusterProfile) (clusterprofile.Outcome, string) {
	if gap, ok := prof.GapFor(c.ProfileField); ok {
		return clusterprofile.OutcomeUnknown, "detection could not read it: " + gap.Reason
	}
	// flux is two findings, and both are a refusal for different reasons. An
	// existing flux-operator owns the Flux installation already; Flux
	// controllers without the operator are somebody's hand-managed install, and
	// dropping a FluxInstance beside them would hand a second manager the same
	// controllers.
	if c.Name == FluxOperatorName {
		switch {
		case prof.FluxOperator != nil:
			return clusterprofile.OutcomeYes, describe("flux-operator", prof.FluxOperator)
		case prof.Flux != nil:
			return clusterprofile.OutcomeYes, describe("the Flux controllers", prof.Flux) +
				", installed by something other than flux-operator"
		default:
			return clusterprofile.OutcomeNo, ""
		}
	}
	// flux-aio reads the same findings and draws no distinction between them,
	// which is ADR-0030 decision 1 exactly: a cluster with Flux is adopted and
	// nothing is installed, whether that Flux came from flux-operator, from
	// `flux bootstrap`, from flux-aio itself or from a vendor's distribution.
	// The single-pod shape is a trade that is right for a cluster with no Flux
	// and wrong as a migration for a cluster that already has some, so the
	// refusal here is not merely "already present" bookkeeping — it is the ADR
	// declining to reshape somebody's working reconciler.
	if c.Name == FluxAIOName {
		switch {
		case prof.Flux != nil:
			return clusterprofile.OutcomeYes, describe("the Flux controllers", prof.Flux)
		case prof.FluxOperator != nil:
			return clusterprofile.OutcomeYes, describe("flux-operator", prof.FluxOperator) +
				", which owns the Flux installation on this cluster"
		default:
			return clusterprofile.OutcomeNo, ""
		}
	}
	switch {
	case c.Name == "cert-manager" && prof.CertManager != nil:
		return clusterprofile.OutcomeYes, versioned("cert-manager", prof.CertManager.Version)
	case c.Name == "cnpg" && prof.CloudNativePG != nil:
		return clusterprofile.OutcomeYes,
			versioned("CloudNativePG", prof.CloudNativePG.Version) + namespaced(prof.CloudNativePG.Namespace)
	case c.Name == "envoy-gateway" && prof.GatewayAPI != nil:
		return clusterprofile.OutcomeYes, versioned("the Gateway API", prof.GatewayAPI.Version)
	case c.Name == "external-secrets" && prof.ExternalSecrets != nil:
		return clusterprofile.OutcomeYes, versioned("external-secrets", prof.ExternalSecrets.Version)
	}
	return clusterprofile.OutcomeNo, ""
}

func describe(what string, c *clusterprofile.Component) string {
	return versioned(what, c.Version) + namespaced(c.Namespace)
}

// versioned names a detected component, keeping "installed, version unknown"
// distinguishable from a version detection actually read.
func versioned(what, version string) string {
	if version == "" {
		return what + " is present (version unknown)"
	}
	return what + " " + version + " is present"
}

func namespaced(ns string) string {
	if ns == "" {
		return ""
	}
	return " in namespace " + ns
}
