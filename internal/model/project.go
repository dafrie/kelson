package model

import "time"

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
	// ComponentHelm delegates a third-party chart to helm-controller: kelson
	// renders a HelmRelease and its source, and never templates the chart
	// (ADR-0016 decision 4).
	ComponentHelm ComponentKind = "helm"
)

// ComponentKinds is the enum, in the order error remediations list it.
var ComponentKinds = []ComponentKind{
	ComponentService, ComponentWorker, ComponentCron, ComponentAgent,
	ComponentPostgres, ComponentValkey, ComponentHelm,
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

// IsChart reports whether a kind delegates a third-party Helm chart to
// helm-controller. It is a third half of the list, not a data kind: nothing
// binds to it, it has no preset, and what it installs is the chart author's
// business rather than kelson's (ADR-0016).
func (k ComponentKind) IsChart() bool { return k == ComponentHelm }

// Valid reports whether a kind is one of the enum's members.
func (k ComponentKind) Valid() bool { return k.IsWorkload() || k.IsData() || k.IsChart() }

// Component is one entry of spec.components: a deployable, a managed data
// service (ADR-0014), or a third-party chart delegated to helm-controller
// (ADR-0016).
//
// The kind is derived from the shape unless it is written: port → service,
// schedule → cron, neither → worker. That derivation is ADR-0006's and is the
// reason the minimum viable Project is six lines; an explicit `kind:` is
// checked against the enum *and* against the shape, so it can say what a
// component is without becoming a second opinion about it.
//
// Not every field applies to every kind. Rather than accept a field that does
// nothing, validation rejects it where it is meaningless — `preset` on a
// workload, `port` on a database, `tools` on anything but an agent, `chart` on
// anything but a helm component — which is the price of the single list and the
// rule of issue #141.
type Component struct {
	Name string `yaml:"name" json:"name" jsonschema:"required,pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"`

	// Kind is optional for workloads and required for data services and helm
	// components, which have no shape to derive from.
	// The description below carries no comma on purpose: the jsonschema tag
	// parser splits on them, so a comma silently truncates what the generated
	// schema — and therefore docs/reference/project.md — says about the field.
	Kind ComponentKind `yaml:"kind,omitempty" json:"kind,omitempty" jsonschema:"enum=service,enum=worker,enum=cron,enum=agent,enum=postgres,enum=valkey,enum=helm,description=derived from port/schedule when omitted; required for postgres and valkey and helm"`

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

	// Auth turns on password authentication for a data component whose operator
	// reads a user password from a Secret it does not create, and names the
	// Secret to read it from:
	//
	//	- name: cache
	//	  kind: valkey
	//	  preset: small
	//	  auth: { secret: cache-auth, key: password }
	//
	// It is a SecretRef and therefore exactly the shape ADR-0018 standardised
	// for an env value, because it is the same promise: a name and a key, never
	// a value, resolved by somebody other than kelson. One Secret then has two
	// consumers — the operator hashes it into the ACL user, and a workload
	// binding to `password` reads it back as a secretKeyRef — and the password
	// itself exists in neither the spec nor the rendered manifests.
	//
	// `kind: valkey` only, for now. `kind: postgres` refuses it: CloudNativePG's
	// initdb bootstrap *generates* the application credential and publishes it,
	// so a Secret an author writes would be a second, unused credential rather
	// than the one the database accepts (ADR-0015 amendment, 2026-08-14).
	Auth *SecretRef `yaml:"auth,omitempty" json:"auth,omitempty" jsonschema:"description=valkey components only; names the Secret holding the cache password — kelson references it and never creates or reads it"`

	// Chart is the chart a `kind: helm` component installs, by name — the name
	// inside the repository, not a path and not a URL. Meaningless on every
	// other kind (ADR-0016 decision 4).
	Chart string `yaml:"chart,omitempty" json:"chart,omitempty" jsonschema:"description=helm components only; the chart name within its source"`

	// ChartVersion is the exact chart version, and it is required. An unpinned
	// chart makes the render non-reproducible in the one way kelson cannot
	// detect: the same document would install different manifests on different
	// days, and the diff — which only ever sees the HelmRelease — would show
	// nothing at all.
	ChartVersion string `yaml:"chartVersion,omitempty" json:"chartVersion,omitempty" jsonschema:"description=helm components only; the exact chart version — required because an unpinned chart is not reproducible"`

	// Source is where the chart comes from: exactly one of a classic Helm
	// repository or an OCI registry.
	Source *ChartSource `yaml:"source,omitempty" json:"source,omitempty" jsonschema:"description=helm components only; exactly one of repository or oci"`

	// Values are the chart's values, written verbatim into the HelmRelease's
	// spec.values.
	//
	// They are plain configuration and nothing else. Unlike a `Secret` kelson
	// renders, a HelmRelease is not redacted anywhere — its values appear in
	// every diff, in the rendered output and in the delivery repository — so a
	// credential written here is a credential published there. Secret material
	// belongs in ValuesFrom, against a Secret somebody else manages (ADR-0009,
	// docs/model.md). Nothing enforces that in v0 beyond saying so: kelson does
	// not sniff the content of a value to guess what it is.
	Values map[string]any `yaml:"values,omitempty" json:"values,omitempty" jsonschema:"description=helm components only; chart values rendered verbatim into the HelmRelease — plain configuration only and never secret material (put that in valuesFrom)"`

	// ValuesFrom names Secrets and ConfigMaps helm-controller merges into the
	// release's values before the inline ones. This is where a chart's
	// credentials go.
	ValuesFrom []ValuesFrom `yaml:"valuesFrom,omitempty" json:"valuesFrom,omitempty" jsonschema:"description=helm components only; Secrets and ConfigMaps merged into the chart values by helm-controller"`

	// Release is the release-command hook: a command run to completion against
	// this component's image, with this component's environment, before the new
	// revision's workloads roll (issue #104). It is where database migrations
	// go.
	//
	// It belongs on a *component* rather than on the Project because everything
	// the command needs is a component's: the image it runs, the env it reads,
	// the data-service bindings it resolves and the ServiceAccount it runs
	// under. A Project-level hook would have to pick one component's image and
	// then pretend it had not.
	//
	// Workload kinds only, and not `cron`: a cron component is a schedule, and
	// a release command runs at deploy time. Data components and charts refuse
	// it for the reason they refuse every workload field — what they run is
	// their operator's business (ADR-0005).
	Release *Release `yaml:"release,omitempty" json:"release,omitempty" jsonschema:"description=command run to completion before this revision's workloads roll — direct delivery mode only"`

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

// Release is a component's release-command hook (issue #104): the command that
// runs, to completion and successfully, before the revision's workloads roll.
//
// The shape is a struct rather than a bare command list because the two
// questions a migration raises are "what runs" and "how long may it take", and
// the second one has no other place to live. Retries are deliberately not a
// field: the Job never retries a *failed* migration behind the deploy's back
// (docs/model.md, "Release commands"), and the retry that does exist — a
// re-deploy — is a user action, not a setting.
type Release struct {
	// Command is the argv of the release command. It is required: a release
	// hook with nothing to run is a Job that succeeds and means nothing.
	Command []string `yaml:"command" json:"command" jsonschema:"required,minItems=1,description=argv of the command; it runs with the component's image and environment"`

	// Timeout is how long the command may run before Kubernetes fails the Job,
	// as a Go duration ("10m"). It becomes the Job's activeDeadlineSeconds.
	// Empty means DefaultReleaseTimeout.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty" jsonschema:"default=10m,description=Go duration such as 30m; the Job's activeDeadlineSeconds"`
}

// DefaultReleaseTimeout is how long a release command may run when the spec
// names no timeout. It is a deliberate ceiling rather than "no deadline": a
// migration that hangs holds the deploy, and a Job with no deadline holds the
// namespace long after the deploy that started it gave up.
const DefaultReleaseTimeout = 10 * time.Minute

// ChartSource is where a helm component's chart is fetched from. Exactly one
// field is set: the two are different Flux source kinds, not two spellings of
// one — `repository` renders a HelmRepository, `oci` renders an OCIRepository.
type ChartSource struct {
	// Repository is a classic Helm repository URL (the one with an index.yaml),
	// e.g. https://charts.bitnami.com/bitnami.
	Repository string `yaml:"repository,omitempty" json:"repository,omitempty" jsonschema:"format=uri,description=classic Helm repository URL — the one serving index.yaml"`
	// OCI is an OCI registry URL holding the chart, without the chart name:
	// oci://ghcr.io/acme/charts. kelson appends the chart name to it, so the
	// same value serves every chart a registry publishes.
	OCI string `yaml:"oci,omitempty" json:"oci,omitempty" jsonschema:"description=OCI registry URL holding the chart — the registry path without the chart name (oci://ghcr.io/acme/charts)"`
}

// ValuesFrom is one Secret or ConfigMap helm-controller reads chart values
// from. Exactly one of the two is set.
//
// It is a name, never a value: this is the only way a helm component's chart
// gets a credential, because the spec carries references and the cluster
// carries values (ADR-0009).
type ValuesFrom struct {
	SecretRef    string `yaml:"secretRef,omitempty" json:"secretRef,omitempty" jsonschema:"description=name of a Secret in the environment namespace"`
	ConfigMapRef string `yaml:"configMapRef,omitempty" json:"configMapRef,omitempty" jsonschema:"description=name of a ConfigMap in the environment namespace"`
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
// component-facing abstraction; the topology itself is delegated to
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
