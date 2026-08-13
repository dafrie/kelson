package model

// Project is the shared-configuration document: image/build, environment,
// service bindings and the Applications that deploy together (ADR-0006).
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

	// Image is a pre-built image reference shared by all applications.
	// Application.image overrides it (rule P3).
	Image string `yaml:"image,omitempty" json:"image,omitempty" jsonschema:"description=pre-built image reference, used when not building from source"`

	// Env is shared by every Application (rule P1: project < application <
	// environment override, key-by-key).
	Env map[string]EnvValue `yaml:"env,omitempty" json:"env,omitempty"`

	Services []Service `yaml:"services,omitempty" json:"services,omitempty" jsonschema:"description=data services this project's applications bind to"`

	Applications []Application `yaml:"applications" json:"applications" jsonschema:"required,minItems=1"`

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

// Application is one deployable, rendering to one workload. The workload
// kind is derived (see docs/model.md): port → web service, schedule →
// CronJob, neither → worker. schedule and port are mutually exclusive.
type Application struct {
	Name string `yaml:"name" json:"name" jsonschema:"required,pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"`

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
}

// WorkloadKind is the workload an Application renders to.
type WorkloadKind string

const (
	WorkloadService WorkloadKind = "service" // Deployment + Service + routing
	WorkloadWorker  WorkloadKind = "worker"  // Deployment, no routing
	WorkloadCron    WorkloadKind = "cron"    // CronJob
)

// Workload derives the workload kind from the application's shape.
func (a Application) Workload() WorkloadKind {
	switch {
	case a.Schedule != "":
		return WorkloadCron
	case a.Port != 0:
		return WorkloadService
	default:
		return WorkloadWorker
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

// Service declares a data service applications bind to. kelson owns the
// application-facing abstraction; topology is delegated to operators
// (ADR-0005, ADR-0007).
type Service struct {
	Name   string        `yaml:"name" json:"name" jsonschema:"required"`
	Type   string        `yaml:"type" json:"type" jsonschema:"required,enum=postgres,enum=valkey"`
	Preset ServicePreset `yaml:"preset,omitempty" json:"preset,omitempty" jsonschema:"default=shared,enum=shared,enum=small,enum=ha-small,enum=ha-medium,enum=branch"`
}

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
