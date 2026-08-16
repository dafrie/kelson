package model

// Environment is where a Project runs and what differs there: target,
// routing, policy, secret backend, and per-Component overrides (issue #25).
// Environments are scoped to a Project by reference (spec.project); everything
// precedence-related is defined by rules P1–P6 in docs/model.md.
//
// What an Environment deliberately does *not* carry is how it is delivered.
// There is one spine — render, publish an OCI artifact, let Flux reconcile it
// (ADR-0028) — so there is no adapter to select and no `delivery:` block to
// write.
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

	Routing *Routing       `yaml:"routing,omitempty" json:"routing,omitempty"`
	Policy  *Policy        `yaml:"policy,omitempty" json:"policy,omitempty"`
	Secrets *SecretBackend `yaml:"secrets,omitempty" json:"secrets,omitempty"`

	// Previews declares that this environment spawns a child environment per
	// open pull request (ADR-0017).
	Previews *Previews `yaml:"previews,omitempty" json:"previews,omitempty"`

	// AutoDeploy opts this environment into following its components' sources:
	// a push to a repository a component is bound to re-renders and republishes
	// this environment, instead of waiting for a person or a pipeline verb
	// (ADR-0036 decision 1). Absent means false — every kelson deploy is
	// initiated deliberately until an environment says otherwise, and manual
	// stays the default.
	//
	// It is a pointer so that "unset" and "false" are different documents. That
	// is what makes the per-component field in Components an *override* rather
	// than a merge: a component may set it either way under an environment that
	// says nothing, and the resolver can tell "this level declined to decide"
	// from "this level decided no".
	AutoDeploy *bool `yaml:"autoDeploy,omitempty" json:"autoDeploy,omitempty" jsonschema:"default=false,description=follow the components' sources — a push to a bound repository re-renders and republishes this environment"`

	// Components carry per-Component overrides, matched by name. It is a list
	// keyed by `name:`, not a mapping of name to override: one spelling for the
	// Project's components and the Environment's overrides of them (ADR-0014),
	// and a list that keeps the order the author wrote.
	//
	// Names must exist in the Project, and what an override may set follows the
	// kind of the component it names: image/replicas/resources/env for a
	// workload (rules P1, P2, P3) plus autoDeploy and imageTracked (ADR-0036),
	// preset for a data component (rule P5).
	Components []ComponentOverride `yaml:"components,omitempty" json:"components,omitempty"`

	// Overlays concatenate after the Project's own overlays (rule P6).
	Overlays []Overlay `yaml:"overlays,omitempty" json:"overlays,omitempty"`
}

// Routing carries domain and gateway defaults for an Environment.
type Routing struct {
	// DomainSuffix provides the default hostname <component>.<suffix> for
	// components with a port and no explicit domains.
	DomainSuffix string `yaml:"domainSuffix,omitempty" json:"domainSuffix,omitempty"`

	// GatewayClass names the Gateway the rendered HTTPRoute attaches to;
	// empty means the class the ClusterProfile detected. There is no
	// ingressClass counterpart: kelson renders Gateway API only (#140), and a
	// spec that still carries `ingressClass:` is rejected as an unknown field
	// rather than silently ignored.
	GatewayClass string `yaml:"gatewayClass,omitempty" json:"gatewayClass,omitempty"`

	// TLS defaults to true; cert issuance delegates to cert-manager when the
	// ClusterProfile reports it.
	TLS *bool `yaml:"tls,omitempty" json:"tls,omitempty"`
}

// ComponentOverride changes one Project Component in this Environment only.
//
// It is one type for both halves of the component list, matching the spec's
// one list (ADR-0014). The fields are disjoint by kind and validation says so:
// a workload override sets image/imageTracked/replicas/resources/env/autoDeploy
// and a data override sets preset, and each is an error on the other side
// rather than a field that resolves into nothing.
type ComponentOverride struct {
	Name string `yaml:"name" json:"name" jsonschema:"required"`

	// Image pins this component to one image reference in this Environment
	// only, and is the promotion primitive of v0 (ADR-0016): promoting
	// staging to production is writing the digest staging deployed here.
	// It wins over the component's image and the Project's (rule P3), and
	// therefore also over `--image`, which stands in for the Project's — a
	// pinned environment does not move when a build produces something new.
	Image string `yaml:"image,omitempty" json:"image,omitempty" jsonschema:"description=pins this component's image in this environment only; the promotion primitive (rule P3)"`

	// ImageTracked says the image named for this component here is a starting
	// point rather than a hold: it renders exactly as any other image does
	// (rule P3 is untouched — the revision it names is what runs), but it does
	// not put the component in [Resolved.ImagePins], so the stale set may still
	// move it and the trigger overwrites it on the next push (ADR-0036
	// decision 5).
	//
	// The trigger writes it with every pin it splices, which is what makes
	// auto-deploy *tracking* rather than one move: `Environment.spec.components[].image`
	// is the only component-keyed image the model has, so without this the
	// trigger's own write would read back as "a person is holding this still"
	// (ADR-0036's amendment states the collision, decision 5 resolves it).
	//
	// An author may write it deliberately — image plus marker is "start here,
	// and let tracking advance it". Absent, an image here means what it has
	// always meant: a person pinned this, and no trigger overwrites it.
	//
	// It is the innermost scope's answer to "does an image hold this component
	// still here", so it cancels the component's own `image:` as well as the
	// one on this override — the P1–P3 instinct, and the alternative is a
	// marker that silently resolves into nothing under a component that carries
	// an image (issue #141).
	//
	// It is a plain bool where its neighbour AutoDeploy is a pointer, and the
	// difference is meaning rather than style: AutoDeploy overrides an outer
	// scope, so it needs a third value for "declined to decide", and this
	// overrides nothing — there is no environment-wide marker for it to differ
	// from, so absent and false are the same document.
	ImageTracked bool `yaml:"imageTracked,omitempty" json:"imageTracked,omitempty" jsonschema:"description=workloads only; the image named here is a starting point rather than a hold — tracking may still advance it and the trigger overwrites it on the next push"`

	Replicas  *Replicas           `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	Resources *Resources          `yaml:"resources,omitempty" json:"resources,omitempty"`
	Env       map[string]EnvValue `yaml:"env,omitempty" json:"env,omitempty"`

	// AutoDeploy narrows or widens the Environment's own tracking for this
	// component alone: its value if set, else the environment's, else false
	// (ADR-0036 decision 1). Nothing between the two levels is an error, in
	// either direction — `false` keeps one risky component manual while the
	// environment tracks, and `true` under an environment that sets nothing is
	// how single-component tracking is written.
	//
	// It is a workload field. A data component and a chart build nothing of
	// ours and are bound to no source (ADR-0035 decision 3), so there is no
	// push that could move one, and setting it on either is refused with the
	// same code as an image on a database rather than resolved into nothing.
	AutoDeploy *bool `yaml:"autoDeploy,omitempty" json:"autoDeploy,omitempty" jsonschema:"description=workloads only; follow this component's source here — it overrides the environment's own setting"`

	// Preset overrides a data component's topology for this Environment
	// (rule P5): `shared` in development, `ha-small` in production, from one
	// Project spec.
	Preset ServicePreset `yaml:"preset,omitempty" json:"preset,omitempty" jsonschema:"enum=shared,enum=small,enum=ha-small,enum=ha-medium,enum=branch,description=data components only"`
}

// AgentMode governs what agents may do unsupervised in this Environment
// (ADR-0025, issue #75).
type AgentMode string

const (
	AgentsAllow       AgentMode = "allow"
	AgentsProposeOnly AgentMode = "propose-only"
)

// AgentOperation names one mutating thing an agent can ask kelson-server to do.
// It is the vocabulary of `policy.forbid`, and it is deliberately coarse: one
// name per mutating RPC, so a reader of a spec can tell what a `forbid:` entry
// switches off without reading the schema (ADR-0025 §4).
type AgentOperation string

const (
	AgentOpDeploy       AgentOperation = "deploy"
	AgentOpRollback     AgentOperation = "rollback"
	AgentOpPromote      AgentOperation = "promote"
	AgentOpBuild        AgentOperation = "build"
	AgentOpSecretSet    AgentOperation = "secret-set"
	AgentOpSecretDelete AgentOperation = "secret-delete"
	AgentOpSpecWrite    AgentOperation = "spec-write"
	AgentOpSpecDelete   AgentOperation = "spec-delete"
)

// AgentOperations is every operation `policy.forbid` accepts, in the order an
// error message lists them. internal/api pins its own RPC table against this
// list, so an operation added here without an enforcement point fails a test
// rather than becoming a word the spec accepts and nothing reads.
func AgentOperations() []AgentOperation {
	return []AgentOperation{
		AgentOpDeploy, AgentOpRollback, AgentOpPromote, AgentOpBuild,
		AgentOpSecretSet, AgentOpSecretDelete, AgentOpSpecWrite, AgentOpSpecDelete,
	}
}

// PolicyRequireDryRun is the one requirement `policy.require` defines: an agent
// mutation must have passed kelson's own server-side dry-run in the same
// request before it is applied.
const PolicyRequireDryRun = "dry-run"

// Policy carries deployment authorization for humans and agents. Everything in
// it except `deployers` is *agent* policy: it narrows what an agent principal
// may do in this environment unsupervised, and it never restricts a human
// (ADR-0025).
//
// Every field is opt-in and restricts only what it names. An environment that
// declares no policy — or a policy that leaves a field unset — narrows nothing,
// because the credential an operator issued is already the deliberate grant
// (ADR-0024 §3) and a guardrail nobody wrote down is one nobody can audit.
type Policy struct {
	// Agents is what an agent may do here without a human in the loop.
	// `propose-only` refuses every live mutation and answers with the
	// escalation path instead.
	Agents AgentMode `yaml:"agents,omitempty" json:"agents,omitempty" jsonschema:"default=allow,enum=allow,enum=propose-only,description=what an agent may do here unsupervised; propose-only refuses every live mutation"`

	// Require lists the guards that must hold before an agent mutation is
	// applied. `dry-run` obliges the server to run its own dry-run first and
	// to refuse a change the dry-run says would be rejected.
	Require []string `yaml:"require,omitempty" json:"require,omitempty" jsonschema:"description=guards that must hold before an agent deploy; only dry-run is defined"`

	// MaxReplicas caps the replica count of any workload an agent deploys
	// here. It is a pointer so `maxReplicas: 0` is representable and can be
	// refused: zero would read as "no replicas at all", which is a scale-down
	// switch disguised as a ceiling.
	MaxReplicas *int `yaml:"maxReplicas,omitempty" json:"maxReplicas,omitempty" jsonschema:"minimum=1,description=agents only; the highest replica count an agent may deploy in this environment"`

	// Protect names components an agent may neither remove from the spec nor
	// scale to zero. It is the blast-radius rule for the components whose loss
	// is not a rollback away — a database, a queue, a cache holding sessions.
	Protect []string `yaml:"protect,omitempty" json:"protect,omitempty" jsonschema:"description=agents only; components an agent may not delete or scale to zero"`

	// Forbid switches off named operations for agents in this environment.
	Forbid []AgentOperation `yaml:"forbid,omitempty" json:"forbid,omitempty" jsonschema:"description=agents only; operations refused to agents here — one of deploy / rollback / promote / build / secret-set / secret-delete / spec-write / spec-delete"`

	// Deployers lists subjects allowed to deploy; empty means the Project's
	// owning team. It is about humans, so #75 does not implement it — see the
	// gate row in notimplemented.go.
	Deployers []string `yaml:"deployers,omitempty" json:"deployers,omitempty"`
}

// AllowsUnsupervised reports whether an agent may change this environment's
// state without a human. It is the `propose-only` test, written as a method so
// that "unset means allow" is decided in one place.
func (p Policy) AllowsUnsupervised() bool {
	return p.Agents != AgentsProposeOnly
}

// Forbids reports whether this policy switches op off for agents.
func (p Policy) Forbids(op AgentOperation) bool {
	for _, f := range p.Forbid {
		if f == op {
			return true
		}
	}
	return false
}

// RequiresDryRun reports whether an agent mutation here must pass kelson's own
// dry-run before it is applied.
func (p Policy) RequiresDryRun() bool {
	for _, r := range p.Require {
		if r == PolicyRequireDryRun {
			return true
		}
	}
	return false
}

// Protects reports whether component is on this policy's protected list.
func (p Policy) Protects(component string) bool {
	for _, name := range p.Protect {
		if name == component {
			return true
		}
	}
	return false
}

// SecretBackend selects where secret values live (ADR-0009). The schema
// accommodates all three backends from day one so v0.2 backends are not a
// breaking change.
//
// `store` and `refreshInterval` configure the `externalSecrets` backend and
// nothing else (ADR-0020); `ageRecipients` and `ageKeySecret` configure `sops`
// and nothing else (ADR-0022). Setting a field under the wrong backend is
// refused rather than ignored.
type SecretBackend struct {
	Backend SecretBackendType `yaml:"backend" json:"backend" jsonschema:"required,enum=cluster,enum=externalSecrets,enum=sops"`

	// Store names the SecretStore or ClusterSecretStore that backend
	// externalSecrets reads from. It is a *name*, resolved against the
	// ClusterProfile at render time: a namespaced SecretStore in this
	// environment's namespace, or a cluster-scoped ClusterSecretStore.
	//
	// It is optional. When the profile offers exactly one store the renderer
	// uses it; when it offers several and the spec names none, or when the name
	// matches both a SecretStore and a ClusterSecretStore, the render is
	// refused rather than resolved by a tiebreak nobody wrote down (ADR-0020).
	Store string `yaml:"store,omitempty" json:"store,omitempty" jsonschema:"description=externalSecrets only; name of a SecretStore in this namespace or a ClusterSecretStore — optional when the cluster offers exactly one"`

	// RefreshInterval is how often external-secrets re-reads the value from the
	// backing store, as a Go duration (30s, 15m, 1h). Empty means
	// DefaultSecretRefreshInterval, which is external-secrets' own default
	// written out explicitly so the manifest always says what the cluster will
	// do.
	RefreshInterval string `yaml:"refreshInterval,omitempty" json:"refreshInterval,omitempty" jsonschema:"default=1h,description=externalSecrets only; how often the value is re-read from the backing store; a positive Go duration such as 30s or 15m or 1h"`

	// AgeRecipients are the age public keys `kelson secret set` encrypts to
	// under backend sops. Required for that backend, refused for the others.
	//
	// These are *public* keys and they belong in the spec in the clear, which
	// is the whole shape of the backend: encrypting needs only the recipient,
	// decrypting needs the identity, and kelson only ever does the first
	// (ADR-0022). The private half lives in a Kubernetes Secret the operator
	// creates and kustomize-controller reads; nothing in kelson can hold it.
	//
	// More than one is the rotation story: a file is wrapped once per
	// recipient and any matching identity opens it, so adding the new key,
	// re-encrypting and then dropping the old one is a handover with no
	// window in which nobody can read the file.
	AgeRecipients []string `yaml:"ageRecipients,omitempty" json:"ageRecipients,omitempty" jsonschema:"description=sops only; age public keys (age1…) that encrypted secrets are readable by — required for backend sops"`

	// AgeKeySecret names the Kubernetes Secret holding the age *identity*,
	// which Flux's Kustomization references as
	// `spec.decryption.secretRef.name`. Empty means DefaultAgeKeySecret.
	//
	// kelson writes the reference and never the Secret. Creating it is the
	// operator's step and it is documented rather than automated, because a
	// kelson that could write that Secret would be a kelson that holds the
	// key that opens every encrypted file in the repository.
	AgeKeySecret string `yaml:"ageKeySecret,omitempty" json:"ageKeySecret,omitempty" jsonschema:"default=sops-age,description=sops only; name of the Secret holding the age identity — created by the operator in the Kustomization's namespace, never by kelson"`
}

// DefaultAgeKeySecret is the Secret name kelson writes into a Kustomization's
// `spec.decryption.secretRef` when the spec names none. It is the name Flux's
// own SOPS guide uses, so the documented setup and the rendered reference
// agree without the author having to restate either.
const DefaultAgeKeySecret = "sops-age"

// DefaultSecretRefreshInterval is the ExternalSecret refresh interval kelson
// writes when the spec sets none. It is external-secrets' own default (its
// kubebuilder default is 1h0m0s), spelled the way an author would write it, and
// it is emitted explicitly rather than left implicit for the reason the previews
// interval is: a manifest should say what the cluster will do.
const DefaultSecretRefreshInterval = "1h"

// PreviewProvider is the forge whose change requests become previews. The
// enum is deliberately two values wide: flux-operator also speaks Azure
// DevOps, Gitea/Forgejo and AWS CodeCommit, and each is a one-line mapping
// away, but an enum value is a promise that the shape has been run
// (ADR-0017).
type PreviewProvider string

const (
	PreviewGitHub PreviewProvider = "github"
	PreviewGitLab PreviewProvider = "gitlab"
)

// Previews is the per-pull-request child-environment declaration (ADR-0017).
//
// It describes two halves of one pipeline. `provider`, `repo`, `secretRef`,
// `interval`, `filter` and `skip` say which change requests become previews —
// they become a flux-operator ResourceSetInputProvider. `artifacts` says where
// the rendered manifests for those previews are pulled from — it becomes the
// OCIRepository inside the ResourceSet's template.
//
// Nothing here is a credential. `secretRef` on both halves is a Secret *name*
// and never a value (ADR-0009, #79). What changed with ADR-0033 decision 4 is
// only who may create the Secret behind the forge one: an author still brings
// their own by naming it, and an author who names nothing gets one kelson
// materializes from the git connection covering `repo`.
type Previews struct {
	// Provider selects the forge. github → GitHubPullRequest,
	// gitlab → GitLabMergeRequest.
	Provider PreviewProvider `yaml:"provider" json:"provider" jsonschema:"required,enum=github,enum=gitlab,description=the forge whose change requests become previews"`

	// Repo is the HTTP(S) URL of the repository whose pull requests become
	// previews — the *source* repository, the one the component builds from.
	Repo string `yaml:"repo" json:"repo" jsonschema:"required,description=HTTP(S) URL of the source repository whose change requests become previews"`

	// SecretRef names the Secret holding forge credentials, in the
	// environment's namespace. Its keys are flux-operator's: username and
	// password for basic auth, or the githubApp* keys.
	//
	// It is optional (ADR-0033 decision 4). Set it to bring your own Secret;
	// leave it unset and kelson materializes the credential from the git
	// connection covering `previews.repo`, into a Secret it owns at
	// `<project>-<environment>-previews`. The name is derived on both sides —
	// the renderer writes it into the ResourceSetInputProvider and the
	// controller writes the Secret — so nothing has to store it.
	SecretRef string `yaml:"secretRef,omitempty" json:"secretRef,omitempty" jsonschema:"description=name of the Secret holding forge credentials; never a token. Omit it to have kelson materialize one from the git connection covering previews.repo"`

	// Interval is how often the forge is polled for change requests. It
	// becomes the fluxcd.controlplane.io/reconcileEvery annotation.
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty" jsonschema:"default=10m,description=how often the forge is polled for change requests"`

	Filter *PreviewFilter `yaml:"filter,omitempty" json:"filter,omitempty"`
	Skip   *PreviewSkip   `yaml:"skip,omitempty" json:"skip,omitempty"`

	// Artifacts is where the per-pull-request rendered manifests live.
	Artifacts PreviewArtifacts `yaml:"artifacts" json:"artifacts" jsonschema:"required"`
}

// PreviewFilter narrows which change requests become previews.
type PreviewFilter struct {
	// Labels selects change requests carrying any of these labels. Empty
	// means every open change request that passes the branch filters, which
	// is a broad default and why Limit exists.
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty" jsonschema:"description=only change requests carrying one of these labels become previews"`

	// IncludeBranch and ExcludeBranch are regular expressions matched against
	// the change request's branch name.
	IncludeBranch string `yaml:"includeBranch,omitempty" json:"includeBranch,omitempty" jsonschema:"description=regular expression; only matching branches become previews"`
	ExcludeBranch string `yaml:"excludeBranch,omitempty" json:"excludeBranch,omitempty" jsonschema:"description=regular expression; matching branches are excluded"`

	// Limit caps how many previews may exist at once. Unset means
	// PreviewDefaultLimit, which is deliberately far below flux-operator's own
	// default of 100 (ADR-0017): the ceiling is a cost control and a surprising
	// one is expensive.
	//
	// It is a pointer so that `limit: 0` is representable and can be refused.
	// flux-operator reads a zero as "use my default", which would turn the one
	// spelling an author might reach for to mean "no previews" into the
	// broadest setting there is.
	Limit *int `yaml:"limit,omitempty" json:"limit,omitempty" jsonschema:"default=10,minimum=1,maximum=10000,description=maximum number of simultaneous previews"`
}

// PreviewSkip gates preview *updates* on CI, which is a different question
// from which change requests get a preview at all.
type PreviewSkip struct {
	// Labels pauses updates for a change request carrying any of these
	// labels. A label prefixed with `!` pauses while the label is *absent*,
	// which is how a "tests passed" gate is written.
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty" jsonschema:"description=pause preview updates while one of these labels is present; a ! prefix inverts the test"`
}

// PreviewArtifacts is the OCI repository the per-pull-request manifests are
// published to and pulled from.
type PreviewArtifacts struct {
	// Repository is an oci:// URL without a tag: the tag is the change
	// request's head commit SHA, chosen per pull request at reconcile time
	// (ADR-0017 decision 2).
	Repository string `yaml:"repository" json:"repository" jsonschema:"required,description=oci:// URL of the repository holding per-pull-request manifests; no tag"`

	// SecretRef names an image-pull Secret for a private artifact repository.
	SecretRef string `yaml:"secretRef,omitempty" json:"secretRef,omitempty" jsonschema:"description=name of a docker-registry Secret for a private artifact repository"`
}

// PreviewDefaultLimit is the ceiling kelson applies when the spec sets none.
// See ADR-0017 for why it is not flux-operator's 100.
const PreviewDefaultLimit = 10

// PreviewDefaultInterval is the forge polling interval kelson applies when the
// spec sets none. It matches flux-operator's own default and is written out
// explicitly so the manifest always says what the cluster will do.
const PreviewDefaultInterval = "10m"

type SecretBackendType string

const (
	SecretsCluster         SecretBackendType = "cluster"
	SecretsExternalSecrets SecretBackendType = "externalSecrets"
	SecretsSOPS            SecretBackendType = "sops"
)
