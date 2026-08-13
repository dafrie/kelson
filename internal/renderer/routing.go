package renderer

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// routingResources renders traffic management for a service from the
// ClusterProfile: an HTTPRoute, and a Certificate when the profile reports
// cert-manager (docs/architecture.md: adopt, don't install — never emit a
// competing ACME client).
//
// Gateway API is the only routing substrate kelson renders (#140). A cluster
// without it is a capability gap reported to the caller, not an Ingress: the
// 2026-08-12 architecture review found that an unreleased platform has no
// legacy installed base to serve, and SIG Network retired ingress-nginx in
// March 2026. Profile.IngressClasses stays detected but is never consumed
// here — it is advisory data for the migration nudge (#112).
func routingResources(resolved *model.Resolved, app *model.ResolvedApplication, profile clusterprofile.ClusterProfile, prov provenance) ([]Manifest, error) {
	if app.Kind != model.WorkloadService || len(app.Domains) == 0 {
		// No domains means nothing to route: a cluster with no Gateway API is
		// only a problem for a spec that actually asks to be reachable.
		return nil, nil
	}
	if profile.GatewayAPI == nil {
		return nil, Errors{gatewayMissingError(app, prov, profile)}
	}

	routing := resolved.Environment.Routing
	out := []Manifest{httpRoute(app, routing, profile, prov)}
	if routing.TLS && profile.CertManager != nil && len(profile.CertManager.ClusterIssuers) > 0 {
		out = append(out, certificate(app, profile, prov))
	}
	return out, nil
}

// gatewayMissingError is the loud capability gap #140 demands in place of the
// old silent Ingress fallback. It names the application whose domains cannot
// be served and points at installing a Gateway implementation; Envoy Gateway
// is kelson's default candidate (#60).
func gatewayMissingError(app *model.ResolvedApplication, prov provenance, profile clusterprofile.ClusterProfile) Error {
	msg := "declares domains (" + strings.Join(app.Domains, ", ") +
		") but the cluster profile reports no Gateway API; kelson renders Gateway API only and will not fall back to Ingress"
	remediation := "install a Gateway API implementation (Envoy Gateway is the default candidate) and re-detect the cluster profile, or remove the domains from this application"
	if len(profile.IngressClasses) > 0 {
		// Detected ingress classes are the most likely reason a user expected
		// this to work, so say plainly that they are not a substitute (#112).
		remediation += ". The detected ingress class(es) (" + ingressClassNames(profile) +
			") are advisory only and are never rendered against"
	}
	return Error{
		Code:        ErrGatewayAPIMissing,
		Application: app.Name,
		Target:      "HTTPRoute/" + prov.name(),
		Message:     msg,
		Remediation: remediation,
	}
}

func ingressClassNames(profile clusterprofile.ClusterProfile) string {
	names := make([]string, len(profile.IngressClasses))
	for i, c := range profile.IngressClasses {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// gatewayParentName picks the Gateway the route attaches to: the
// environment's explicit gatewayClass wins, else the sole/first class the
// profile detected.
func gatewayParentName(routing model.ResolvedRouting, profile clusterprofile.ClusterProfile) string {
	if routing.GatewayClass != "" {
		return routing.GatewayClass
	}
	if len(profile.GatewayAPI.Classes) > 0 {
		return profile.GatewayAPI.Classes[0]
	}
	return ""
}

func hostnamesNode(domains []string) *yaml.Node {
	items := make([]*yaml.Node, len(domains))
	for i, d := range domains {
		items[i] = strNode(d)
	}
	return seqNode(items...)
}

func httpRoute(app *model.ResolvedApplication, routing model.ResolvedRouting, profile clusterprofile.ClusterProfile, prov provenance) Manifest {
	specKV := []any{}
	if parent := gatewayParentName(routing, profile); parent != "" {
		specKV = append(specKV, "parentRefs", seqNode(mapNode("name", parent)))
	}
	specKV = append(specKV,
		"hostnames", hostnamesNode(app.Domains),
		"rules", seqNode(mapNode(
			"matches", seqNode(mapNode(
				"path", mapNode("type", "PathPrefix", "value", "/"),
			)),
			"backendRefs", seqNode(mapNode(
				"name", app.Name,
				"port", app.Port,
			)),
		)),
	)
	return baseManifest("gateway.networking.k8s.io/v1", "HTTPRoute", prov, mapNode(specKV...))
}

func tlsSecretName(app *model.ResolvedApplication) string {
	return app.Name + "-tls"
}

func certificate(app *model.ResolvedApplication, profile clusterprofile.ClusterProfile, prov provenance) Manifest {
	certProv := prov
	// The Certificate shares its name with the TLS secret it provisions.
	certProv.resourceName = tlsSecretName(app)
	spec := mapNode(
		"secretName", tlsSecretName(app),
		"issuerRef", mapNode(
			"kind", "ClusterIssuer",
			"name", profile.CertManager.ClusterIssuers[0],
		),
		"dnsNames", hostnamesNode(app.Domains),
	)
	return baseManifest("cert-manager.io/v1", "Certificate", certProv, spec)
}

func serviceMonitor(app *model.ResolvedApplication, prov provenance) Manifest {
	spec := mapNode(
		"selector", mapNode("matchLabels", selectorLabels(prov)),
		"endpoints", seqNode(mapNode(
			"port", "http",
			"path", "/metrics",
		)),
	)
	return baseManifest("monitoring.coreos.com/v1", "ServiceMonitor", prov, spec)
}
