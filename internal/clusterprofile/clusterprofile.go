// Package clusterprofile defines the ClusterProfile input to the renderer.
//
// The renderer takes a ClusterProfile as an argument (never looks one up), so
// the type is pure and safe for the renderer to import. Detection — which
// talks to a live cluster — lives in a sibling package that the renderer must
// NOT import (issue #20).
//
// # Absent, present, and unknown are three different things
//
// A nil component pointer means "not detected". A non-nil one means present,
// with Version empty if the version could not be read. Neither says anything
// about components the probe was not allowed to look at: that is what
// [ClusterProfile.Incomplete] is for.
//
// The distinction is load-bearing rather than pedantic. If a probe lacks RBAC
// to list ClusterIssuers and reports CertManager as nil, the renderer emits an
// Ingress with no TLS and the manifest looks intentional. Recording the gap
// instead lets the caller refuse to render, or render and say what it could
// not see. It is the same rule the preview engine follows for policy
// (internal/diff.Unvalidated): "we checked and it is absent" and "we could not
// check" must never collapse into one answer.
package clusterprofile

// ClusterProfile records what a cluster already provides, so the renderer can
// adapt without installing competing software (ADR-0003, docs/architecture.md).
//
// This is the whole reason kelson can be adopted by a team already running
// Kubernetes properly: the profile turns the combinatorial space of cluster
// shapes into cheap fixtures, so "renders correctly on a Gateway API cluster
// and on an Ingress cluster" is a golden test rather than two real clusters.
type ClusterProfile struct {
	// Kubernetes is the cluster's own version and shape. Absent when the
	// profile was written by hand and the author did not care.
	Kubernetes *Kubernetes `yaml:"kubernetes,omitempty" json:"kubernetes,omitempty"`

	GatewayAPI     *GatewayAPI    `yaml:"gatewayAPI,omitempty" json:"gatewayAPI,omitempty"`
	IngressClasses []IngressClass `yaml:"ingressClasses,omitempty" json:"ingressClasses,omitempty"`
	CertManager    *CertManager   `yaml:"certManager,omitempty" json:"certManager,omitempty"`

	// ExternalSecrets is the external-secrets operator and the stores it has
	// configured. Secret *values* never appear here — a profile is a
	// capability report and is safe to print (ADR-0009).
	ExternalSecrets *ExternalSecrets `yaml:"externalSecrets,omitempty" json:"externalSecrets,omitempty"`

	// PolicyEngines are admission-policy controllers such as Kyverno or
	// Gatekeeper. Preview needs these to explain a rejection instead of
	// reporting a bare failure (issue #45).
	PolicyEngines []PolicyEngine `yaml:"policyEngines,omitempty" json:"policyEngines,omitempty"`

	// StorageClasses carry the snapshot capability that database branching
	// depends on (issue #108, ADR-0007). The default k3s local-path
	// provisioner has no snapshot driver at all, which is a nudge at install
	// time rather than a surprise at branch time.
	StorageClasses []StorageClass `yaml:"storageClasses,omitempty" json:"storageClasses,omitempty"`

	CloudNativePG *Component  `yaml:"cnpg,omitempty" json:"cnpg,omitempty"`
	Flux          *Component  `yaml:"flux,omitempty" json:"flux,omitempty"`
	ArgoCD        *Component  `yaml:"argocd,omitempty" json:"argocd,omitempty"`
	MetricsServer *Component  `yaml:"metricsServer,omitempty" json:"metricsServer,omitempty"`
	Prometheus    *Prometheus `yaml:"prometheus,omitempty" json:"prometheus,omitempty"`

	// Incomplete lists what the probe could not determine, and why. An empty
	// slice means every field above is a finding rather than a guess.
	//
	// Detection must append here instead of failing the whole capture: a
	// profile that is 90% known and honest about the rest is more useful than
	// no profile, and far more useful than a profile that quietly reports
	// absence for anything it could not read.
	Incomplete []Gap `yaml:"incomplete,omitempty" json:"incomplete,omitempty"`
}

// Component is a detected piece of cluster software. Its presence is carried
// by the pointer being non-nil, so an empty Component still means "installed,
// version unknown" — which is a real state and must not be confused with
// absence.
type Component struct {
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Namespace is where the component runs, when that is knowable and
	// useful to report back to a human.
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
}

// Kubernetes is the cluster itself.
type Kubernetes struct {
	// Version is the server version, e.g. v1.31.2. Version skew against what
	// kelson requires is reported explicitly rather than discovered as a
	// mysterious apply failure (issue #57).
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Platform is the distribution when it can be identified: k3s, gke, eks,
	// talos. Best-effort and advisory; nothing may branch on it silently.
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty"`
	// NodeArchitectures are the distinct node architectures present, e.g.
	// amd64, arm64. A build that produces only linux/amd64 cannot schedule on
	// an arm64-only cluster, and that is worth knowing before the push.
	NodeArchitectures []string `yaml:"nodeArchitectures,omitempty" json:"nodeArchitectures,omitempty"`
}

// GatewayAPI is the Gateway API CRDs and the classes they offer.
type GatewayAPI struct {
	Version string   `yaml:"version,omitempty" json:"version,omitempty"`
	Classes []string `yaml:"classes,omitempty" json:"classes,omitempty"`
}

// IngressClass is one ingress controller's class.
type IngressClass struct {
	Name string `yaml:"name" json:"name"`
	// Controller is the controller name from the IngressClass spec, e.g.
	// k8s.io/ingress-nginx. Reported so a human can tell two classes apart.
	Controller string `yaml:"controller,omitempty" json:"controller,omitempty"`
	// Default marks the class annotated as the cluster default. The renderer
	// prefers it over list order, because list order is an accident of
	// detection and the default is a decision the cluster's owner made.
	Default bool `yaml:"default,omitempty" json:"default,omitempty"`
}

// CertManager is the cert-manager installation and the issuers it exposes.
type CertManager struct {
	Version        string   `yaml:"version,omitempty" json:"version,omitempty"`
	ClusterIssuers []string `yaml:"clusterIssuers,omitempty" json:"clusterIssuers,omitempty"`
}

// ExternalSecrets is the external-secrets operator and its configured stores.
type ExternalSecrets struct {
	Version             string   `yaml:"version,omitempty" json:"version,omitempty"`
	SecretStores        []string `yaml:"secretStores,omitempty" json:"secretStores,omitempty"`
	ClusterSecretStores []string `yaml:"clusterSecretStores,omitempty" json:"clusterSecretStores,omitempty"`
}

// PolicyEngine is an admission-policy controller.
type PolicyEngine struct {
	// Name is the engine id: kyverno, gatekeeper.
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

// StorageClass is one storage class and whether it can snapshot.
type StorageClass struct {
	Name        string `yaml:"name" json:"name"`
	Provisioner string `yaml:"provisioner,omitempty" json:"provisioner,omitempty"`
	Default     bool   `yaml:"default,omitempty" json:"default,omitempty"`
	// VolumeSnapshotClass is a snapshot class whose driver matches this
	// class's provisioner. Empty means snapshot-based database branching is
	// unavailable on this class and the restore-based path applies instead
	// (issue #108).
	VolumeSnapshotClass string `yaml:"volumeSnapshotClass,omitempty" json:"volumeSnapshotClass,omitempty"`
}

// Prometheus is the monitoring stack's CRDs. ServiceMonitor and PodMonitor are
// tracked separately because a cluster can have one without the other, and
// emitting the wrong kind produces a resource nothing ever reads.
//
// The renderer currently branches on presence alone and emits a ServiceMonitor.
// Refining that to respect these two flags is a rendering change with golden
// consequences, so it is left to whoever detects them for real rather than
// smuggled in with the schema.
type Prometheus struct {
	Version        string `yaml:"version,omitempty" json:"version,omitempty"`
	ServiceMonitor bool   `yaml:"serviceMonitor,omitempty" json:"serviceMonitor,omitempty"`
	PodMonitor     bool   `yaml:"podMonitor,omitempty" json:"podMonitor,omitempty"`
}

// Gap is one thing detection could not determine.
type Gap struct {
	// Field is the profile field left unknown, in YAML path form:
	// "certManager.clusterIssuers", "storageClasses".
	Field string `yaml:"field" json:"field"`
	// Reason is why, in a form a human can act on — typically a missing RBAC
	// verb, so the message should name the permission that would fix it.
	Reason string `yaml:"reason" json:"reason"`
}

// HasPolicyEngine reports whether any admission-policy controller was
// detected. Preview uses this to decide whether an unexplained rejection is
// worth attributing to policy at all (issue #45).
func (p ClusterProfile) HasPolicyEngine() bool { return len(p.PolicyEngines) > 0 }

// DefaultIngressClass returns the class the renderer should use: the one the
// cluster marks default, else the first detected. Empty when there are none.
func (p ClusterProfile) DefaultIngressClass() string {
	for _, c := range p.IngressClasses {
		if c.Default {
			return c.Name
		}
	}
	if len(p.IngressClasses) > 0 {
		return p.IngressClasses[0].Name
	}
	return ""
}
