package model

// Environment is where a Project runs and what differs there: target,
// routing, delivery mode, policy, secret backend, and per-Application
// overrides (issue #25). Environments are scoped to a Project by reference
// (spec.project); everything precedence-related is defined by rules P1–P6 in
// docs/model.md.
type Environment struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta      `yaml:"metadata" json:"metadata" jsonschema:"required"`
	Spec     EnvironmentSpec `yaml:"spec" json:"spec" jsonschema:"required"`
}

type EnvironmentSpec struct {
	// Project names the Project document this environment deploys. Required.
	Project string `yaml:"project" json:"project" jsonschema:"required"`

	// Cluster names the target cluster from the environment registry.
	// Empty means the cluster kelson is running on.
	Cluster string `yaml:"cluster,omitempty" json:"cluster,omitempty"`

	// Namespace is the target namespace; default "<project>-<environment>".
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`

	Routing  *Routing       `yaml:"routing,omitempty" json:"routing,omitempty"`
	Delivery *Delivery      `yaml:"delivery,omitempty" json:"delivery,omitempty"`
	Policy   *Policy        `yaml:"policy,omitempty" json:"policy,omitempty"`
	Secrets  *SecretBackend `yaml:"secrets,omitempty" json:"secrets,omitempty"`

	// Applications carry per-Application overrides, matched by name
	// (rules P1, P2). Names must exist in the Project.
	Applications []AppOverride `yaml:"applications,omitempty" json:"applications,omitempty"`

	// Services carry per-Environment preset overrides, matched by name
	// (rule P5). Names must exist in the Project.
	Services []ServiceOverride `yaml:"services,omitempty" json:"services,omitempty"`

	// Overlays concatenate after the Project's own overlays (rule P6).
	Overlays []Overlay `yaml:"overlays,omitempty" json:"overlays,omitempty"`
}

// Routing carries domain and ingress defaults for an Environment.
type Routing struct {
	// DomainSuffix provides the default hostname <application>.<suffix> for
	// applications with a port and no explicit domains.
	DomainSuffix string `yaml:"domainSuffix,omitempty" json:"domainSuffix,omitempty"`

	// Exactly one of these may be set (schema/mutually-exclusive).
	IngressClass string `yaml:"ingressClass,omitempty" json:"ingressClass,omitempty"`
	GatewayClass string `yaml:"gatewayClass,omitempty" json:"gatewayClass,omitempty"`

	// TLS defaults to true; cert issuance delegates to cert-manager when the
	// ClusterProfile reports it.
	TLS *bool `yaml:"tls,omitempty" json:"tls,omitempty"`
}

// AppOverride changes one Project Application in this Environment only.
type AppOverride struct {
	Name      string              `yaml:"name" json:"name" jsonschema:"required"`
	Replicas  *Replicas           `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	Resources *Resources          `yaml:"resources,omitempty" json:"resources,omitempty"`
	Env       map[string]EnvValue `yaml:"env,omitempty" json:"env,omitempty"`
}

// ServiceOverride changes a service's preset in this Environment (rule P5).
type ServiceOverride struct {
	Name   string        `yaml:"name" json:"name" jsonschema:"required"`
	Preset ServicePreset `yaml:"preset" json:"preset" jsonschema:"required,enum=shared,enum=small,enum=ha-small,enum=ha-medium,enum=branch"`
}

type DeliveryMode string

const (
	DeliveryDirect DeliveryMode = "direct"
	DeliveryFlux   DeliveryMode = "flux"
	DeliveryArgoCD DeliveryMode = "argocd"
)

// Delivery selects the adapter for this Environment (ADR-0001). All adapters
// consume identical rendered output; they differ only in who calls apply.
type Delivery struct {
	Mode DeliveryMode `yaml:"mode" json:"mode" jsonschema:"required,enum=direct,enum=flux,enum=argocd"`

	// Git configures where rendered manifests are committed. Required for
	// flux and argocd (semantic/git-target-missing); meaningless for direct.
	Git *GitTarget `yaml:"git,omitempty" json:"git,omitempty"`
}

type GitTarget struct {
	Repo   string `yaml:"repo" json:"repo" jsonschema:"required,description=git URL of the deployment repository"`
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty" jsonschema:"default=main"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty" jsonschema:"description=directory within the repo for rendered manifests"`
}

// AgentMode governs what agents may do unsupervised in this Environment
// (docs/architecture.md, agent principals).
type AgentMode string

const (
	AgentsAllow       AgentMode = "allow"
	AgentsProposeOnly AgentMode = "propose-only"
)

// Policy carries deployment authorization for humans and agents.
type Policy struct {
	// Agents defaults to propose-only: agent changes open pull requests.
	// require: [dry-run] obliges a dry-run before applying.
	Agents  AgentMode `yaml:"agents,omitempty" json:"agents,omitempty" jsonschema:"default=propose-only,enum=allow,enum=propose-only"`
	Require []string  `yaml:"require,omitempty" json:"require,omitempty" jsonschema:"description=guards that must hold before deploy; only dry-run is defined"`

	// Deployers lists subjects allowed to deploy; empty means the Project's
	// owning team.
	Deployers []string `yaml:"deployers,omitempty" json:"deployers,omitempty"`
}

// SecretBackend selects where secret values live (ADR-0009). The schema
// accommodates all three backends from day one so v0.2 backends are not a
// breaking change.
type SecretBackend struct {
	Backend SecretBackendType `yaml:"backend" json:"backend" jsonschema:"required,enum=cluster,enum=externalSecrets,enum=sops"`

	// Store names the ClusterSecretStore for backend externalSecrets.
	Store string `yaml:"store,omitempty" json:"store,omitempty"`
}

type SecretBackendType string

const (
	SecretsCluster         SecretBackendType = "cluster"
	SecretsExternalSecrets SecretBackendType = "externalSecrets"
	SecretsSOPS            SecretBackendType = "sops"
)
