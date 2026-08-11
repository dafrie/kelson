// Package clusterprofile defines the ClusterProfile input to the renderer.
//
// The renderer takes a ClusterProfile as an argument (never looks one up), so
// the type is pure and safe for the renderer to import. Detection — which
// talks to a live cluster — lives in a sibling package that the renderer must
// NOT import (issue #20).
package clusterprofile

// ClusterProfile records what a cluster already provides, so the renderer can
// adapt without installing competing software (ADR-0003, docs/architecture.md).
type ClusterProfile struct {
	GatewayAPI     *GatewayAPI  `yaml:"gatewayAPI,omitempty" json:"gatewayAPI,omitempty"`
	IngressClasses []string     `yaml:"ingressClasses,omitempty" json:"ingressClasses,omitempty"`
	CertManager    *CertManager `yaml:"certManager,omitempty" json:"certManager,omitempty"`
	CloudNativePG  bool         `yaml:"cnpg,omitempty" json:"cnpg,omitempty"`
	Flux           bool         `yaml:"flux,omitempty" json:"flux,omitempty"`
	ArgoCD         bool         `yaml:"argocd,omitempty" json:"argocd,omitempty"`
	MetricsServer  bool         `yaml:"metricsServer,omitempty" json:"metricsServer,omitempty"`
	Prometheus     bool         `yaml:"prometheus,omitempty" json:"prometheus,omitempty"`
}

type GatewayAPI struct {
	Version string   `yaml:"version,omitempty" json:"version,omitempty"`
	Classes []string `yaml:"classes,omitempty" json:"classes,omitempty"`
}

type CertManager struct {
	ClusterIssuers []string `yaml:"clusterIssuers,omitempty" json:"clusterIssuers,omitempty"`
}
