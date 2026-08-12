package renderer

import (
	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// routingResources renders traffic management for a service from the
// ClusterProfile: HTTPRoute where Gateway API exists, Ingress where only
// ingress classes exist, nothing where the cluster routes nothing. TLS is
// delegated to cert-manager when the profile reports it (docs/architecture.md:
// adopt, don't install — never emit a competing ACME client).
func routingResources(resolved *model.Resolved, app *model.ResolvedApplication, profile clusterprofile.ClusterProfile, prov provenance) []Manifest {
	if app.Kind != model.WorkloadService || len(app.Domains) == 0 {
		return nil
	}
	routing := resolved.Environment.Routing

	var out []Manifest
	switch {
	case profile.GatewayAPI != nil:
		out = append(out, httpRoute(app, routing, profile, prov))
	case len(profile.IngressClasses) > 0:
		out = append(out, ingress(app, routing, profile, prov))
	default:
		// No routing substrate on this cluster: domains are declared but
		// nothing to attach them to. Emitting a route here would produce a
		// resource nothing reconciles.
		return nil
	}
	if routing.TLS && profile.CertManager != nil && len(profile.CertManager.ClusterIssuers) > 0 {
		out = append(out, certificate(app, profile, prov))
	}
	return out
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

func ingressClassName(routing model.ResolvedRouting, profile clusterprofile.ClusterProfile) string {
	if routing.IngressClass != "" {
		return routing.IngressClass
	}
	return profile.DefaultIngressClass()
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

func ingress(app *model.ResolvedApplication, routing model.ResolvedRouting, profile clusterprofile.ClusterProfile, prov provenance) Manifest {
	rules := make([]*yaml.Node, len(app.Domains))
	for i, d := range app.Domains {
		rules[i] = mapNode(
			"host", d,
			"http", mapNode(
				"paths", seqNode(mapNode(
					"path", "/",
					"pathType", "Prefix",
					"backend", mapNode(
						"service", mapNode(
							"name", app.Name,
							"port", mapNode("number", app.Port),
						),
					),
				)),
			),
		)
	}
	specKV := []any{
		"ingressClassName", ingressClassName(routing, profile),
		"rules", seqNode(rules...),
	}
	if routing.TLS {
		specKV = append(specKV, "tls", seqNode(mapNode(
			"hosts", hostnamesNode(app.Domains),
			"secretName", tlsSecretName(app),
		)))
	}
	return baseManifest("networking.k8s.io/v1", "Ingress", prov, mapNode(specKV...))
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
