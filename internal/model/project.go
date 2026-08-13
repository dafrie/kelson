package model

// Project is the shared-configuration document: image/build, environment,
// and the Components that deploy together (ADR-0006, amended by ADR-0014).
// It stays environment-agnostic; everything that differs per target lives in
// the Environment document.
type Project struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta  `yaml:"metadata" json:"metadata" jsonschema:"required"`
	Spec     ProjectSpec `yaml:"spec" json:"spec" jsonschema:"required"`
}

type ProjectSpec struct {
	Source *Source `yaml:"source,omitempty" json:"source,omitempty"`
	Build  *Build  `yaml:"build,omitempty" json:"build,omitempty"`

	// Image is a pre-built image reference shared by all components.
	// Component.image overrides it (rule P3).
	Image string `yaml:"image,omitempty" json:"image,omitempty" jsonschema:"description=pre-built image reference, used when not building from source"`

	// Env is shared by every Component (rule P1: project < component <
	// environment override, key-by-key).
	Env map[string]EnvValue `yaml:"env,omitempty" json:"env,omitempty"`

	// Components is the single leaf list of ADR-0014: workloads and managed
	// data services in one place, distinguished by kind. There is no second
	// list, and adding one is the accident ADR-0014 exists to stop.
	Components []Component `yaml:"components" json:"components" jsonschema:"required,minItems=1"`

	// Defaults are fallbacks for Environment-level concerns (rule P4). An
	// explicit Environment value always wins.
	Defaults *ProjectDefaults `yaml:"defaults,omitempty" json:"defaults,omitempty"`

	// Overlays are the escape hatch of record (rule P6: project first).
	Overlays []Overlay `yaml:"overlays,omitempty" json:"overlays,omitempty"`
}

type Source struct {
	Git string `yaml:"git" json:"git" jsonschema:"required,format=uri,description=git URL of the application source"`
	Ref string `yaml:"ref,omitempty" json:"ref,omitempty" jsonschema:"default=main"`
}

type BuildStrategy string

const (
	BuildAuto       BuildStrategy = "auto"
	BuildDockerfile BuildStrategy = "dockerfile"
	BuildBuildpacks BuildStrategy = "buildpacks"
	BuildNone       BuildStrategy = "none"
)

type Build struct {
	Strategy BuildStrategy `yaml:"strategy,omitempty" json:"strategy,omitempty" jsonschema:"default=auto,enum=auto,enum=dockerfile,enum=buildpacks,enum=none"`
	// Dockerfile path within the source; default "Dockerfile".
	Dockerfile string `yaml:"dockerfile,omitempty" json:"dockerfile,omitempty"`
}

// ComponentKind is what a component renders to. The set is closed, which is
// the precondition ADR-0014 names for a single list being worth having: the
// moment a kind cannot be enumerated the abstraction stops paying (the
// OAM/KubeVela `type: raw` lesson).
type ComponentKind string

const (
	ComponentService ComponentKind = "service" // Deployment + Service + routing
	ComponentWorker  ComponentKind = "worker"  // Deployment, no routing
	ComponentCron    ComponentKind = "cron"    // CronJob
	ComponentAgent   ComponentKind = "agent"   // worker-shaped, with its own identity and tool policy
	// Data kinds delegate their topology to an operator (ADR-0005, ADR-0007).
	ComponentPostgres ComponentKind = "postgres"
	ComponentValkey   ComponentKind = "valkey"
)

// ComponentKinds is the enum, in the order error remediations list it.
var ComponentKinds = []ComponentKind{
	ComponentService, ComponentWorker, ComponentCron, ComponentAgent,
	ComponentPostgres, ComponentValkey,
}

// IsData reports whether a kind renders a managed data service rather than a
// workload. The two halves of the list differ in what they render and in
// which precedence rules reach them (P1–P3 versus P5), and nowhere else.
func (k ComponentKind) IsData() bool {
	return k == ComponentPostgres || k == ComponentValkey
}

// IsWorkload reports whether a kind renders a pod-bearing workload.
func (k ComponentKind) IsWorkload() bool {
	switch k {
	case ComponentService, ComponentWorker, ComponentCron, ComponentAgent:
		return true
	}
	return false
}

// Valid reports whether a kind is one of the enum's members.
func (k ComponentKind) Valid() bool { return k.IsWorkload() || k.IsData() }

// Component is one entry of spec.components: a deployable, or a managed data
// service (ADR-0014).
//
// The kind is derived from the shape unless it is written: port → service,
// schedule → cron, neither → worker. That derivation is ADR-0006's and is the
// reason the minimum viable Project is six lines; an explicit `kind:` is
// checked against the enum *and* against the shape, so it can say what a
// component is without becoming a second opinion about it.
//
// Not every field applies to every kind. Rather than accept a field that does
// nothing, validation rejects it where it is meaningless — `preset` on a
// workload, `port` on a database, `tools` on anything but an agent — which is
// the price of the single list and the rule of issue #141.
type Component struct {
	Name string `yaml:"name" json:"name" jsonschema:"required,pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"`

	// Kind is optional for workloads and required for data services, which
	// have no shape to derive from.
	Kind ComponentKind `yaml:"kind,omitempty" json:"kind,omitempty" jsonschema:"enum=service,enum=worker,enum=cron,enum=agent,enum=postgres,enum=valkey,description=derived from port/schedule when omitted; required for postgres and valkey"`

	Image   string   `yaml:"image,omitempty" json:"image,omitempty" jsonschema:"description=overrides the Project image (rule P3)"`
	Command []string `yaml:"command,omitempty" json:"command,omitempty" jsonschema:"description=container command; wins over the image default"`

	// Port makes this a web service: Deployment + Service + routing.
	Port   int    `yaml:"port,omitempty" json:"port,omitempty" jsonschema:"minimum=1,maximum=65535"`
	Health string `yaml:"health,omitempty" json:"health,omitempty" jsonschema:"description=HTTP liveness/readiness path, e.g. /healthz"`

	// Schedule ("0 3 * * *") makes this a CronJob; mutually exclusive with
	// port, health and domains.
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty" jsonschema:"description=five-field cron expression"`

	// Domains are explicit FQDNs and win over the default hostname derived
	// from the Environment's routing.domainSuffix.
	Domains []string `yaml:"domains,omitempty" json:"domains,omitempty"`

	Replicas  *Replicas  `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	Resources *Resources `yaml:"resources,omitempty" json:"resources,omitempty"`

	Env map[string]EnvValue `yaml:"env,omitempty" json:"env,omitempty"`

	// Preset is the topology of a data component, and is meaningless on a
	// workload (ADR-0007, docs/data-services.md).
	Preset ServicePreset `yaml:"preset,omitempty" json:"preset,omitempty" jsonschema:"default=shared,enum=shared,enum=small,enum=ha-small,enum=ha-medium,enum=branch,description=data components only"`

	// Tools is the tool subset an agent component may call — the per-agent
	// capability policy ADR-0014 records as mandatory practice for this
	// component type. It is validated and then refused
	// (schema/not-implemented) until there is a policy engine to enforce it
	// (issue #75): a tool allow-list nothing enforces is worse than none.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty" jsonschema:"description=agent components only; refused until issue #75"`
}

// EffectiveKind is the kind a component renders as: what `kind:` says, or
// what its shape derives when it says nothing.
func (c Component) EffectiveKind() ComponentKind {
	if c.Kind != "" {
		return c.Kind
	}
	return c.DerivedKind()
}

// DerivedKind is the workload kind the component's shape implies, ignoring
// any explicit `kind:`. Validation compares the two so a written kind that
// contradicts the shape is an error rather than a silent winner.
func (c Component) DerivedKind() ComponentKind {
	switch {
	case c.Schedule != "":
		return ComponentCron
	case c.Port != 0:
		return ComponentService
	default:
		return ComponentWorker
	}
}

// Replicas prescribes scaling. Max zero/absent means a fixed replica count of
// Min (default 1); Max > Min means autoscaling between the bounds.
type Replicas struct {
	Min int `yaml:"min" json:"min" jsonschema:"required,minimum=0"`
	Max int `yaml:"max,omitempty" json:"max,omitempty" jsonschema:"minimum=0"`
}

type Resources struct {
	Requests *ResourceList `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   *ResourceList `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type ResourceList struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty" jsonschema:"description=Kubernetes quantity, e.g. 500m or 2"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty" jsonschema:"description=Kubernetes quantity, e.g. 512Mi or 2Gi"`
}

// ServicePreset is the topology of a data component. kelson owns the
// application-facing abstraction; the topology itself is delegated to
// operators (ADR-0005, ADR-0007).
type ServicePreset string

const (
	PresetShared   ServicePreset = "shared"
	PresetSmall    ServicePreset = "small"
	PresetHASmall  ServicePreset = "ha-small"
	PresetHAMedium ServicePreset = "ha-medium"
	PresetBranch   ServicePreset = "branch"
)

// Overlay is the escape hatch: strategic-merge patches applied to rendered
// resources, or additional manifests emitted as-is. Exactly one of patch or
// manifest is set; paths are relative to the spec file.
type Overlay struct {
	Patch    string `yaml:"patch,omitempty" json:"patch,omitempty" jsonschema:"description=YAML document merged into the resource it targets"`
	Manifest string `yaml:"manifest,omitempty" json:"manifest,omitempty" jsonschema:"description=path to an extra Kubernetes manifest, emitted as-is"`
}

// ProjectDefaults are Project-level fallbacks for Environment concerns;
// an explicit Environment value always wins (rule P4).
type ProjectDefaults struct {
	DeliveryMode DeliveryMode   `yaml:"deliveryMode,omitempty" json:"deliveryMode,omitempty"`
	Policy       *Policy        `yaml:"policy,omitempty" json:"policy,omitempty"`
	Secrets      *SecretBackend `yaml:"secrets,omitempty" json:"secrets,omitempty"`
}
