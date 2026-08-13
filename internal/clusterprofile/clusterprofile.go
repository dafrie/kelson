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
// to list ClusterIssuers and reports CertManager as nil, the renderer emits a
// route with no Certificate and the manifest looks intentional. Recording the
// gap instead lets the caller refuse to render, or render and say what it
// could not see: "we checked and it is absent" and "we could not check" must
// never collapse into one answer.
//
// # The tri-state discipline, stated once
//
// That rule is not local to detection. Every judgement kelson makes about a
// cluster — is this version supported, can this storage snapshot, does this
// resource pass admission — has three answers, and the third one is the point:
// yes, no, and we could not tell. This package is where that discipline is
// written down, and the codebase spells it one way (issue #144):
//
//   - [Outcome] is the answer. One type, one set of constants, for every
//     "can/does/is" judgement about a cluster, with [OutcomeUnknown] as its
//     zero value so an unmade judgement can never read as a confident yes.
//     Its users today are internal/clusterprofile/support (version skew) and
//     internal/clusterprofile/storage (snapshot capability).
//   - [Gap] is the reason behind an Unknown, and [ClusterProfile.Incomplete]
//     is the list of them detection produces. An Outcome says which of the
//     three answers applies; the Gap says why the third one does.
//     [ClusterProfile.GapFor] is how a judgement finds it, so every Unknown
//     can name the permission that would resolve it instead of shrugging.
//
// One judgement deliberately keeps its own shape: internal/diff.Unvalidated,
// the preview's record of a resource the API server never got to evaluate. It
// follows the same discipline — an unvalidated resource is neither a violation
// nor a pass — but it is a per-resource record on a serialized contract that
// the UI and agents parse (diff_json), not a three-valued field, and its
// "which resource, which missing prerequisite, in this batch or not" payload
// does not fit an enum. Sharing the vocabulary there would mean changing a
// wire format to make two internal types look alike, which is the wrong trade.
package clusterprofile

// ClusterProfile records what a cluster already provides, so the renderer can
// adapt without installing competing software (ADR-0003, docs/architecture.md).
//
// This is the whole reason kelson can be adopted by a team already running
// Kubernetes properly: the profile turns the combinatorial space of cluster
// shapes into cheap fixtures, so "renders correctly with cert-manager and
// without it, on Envoy Gateway and on Istio" is a golden test rather than
// four real clusters.
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

	// CloudNativePG is the CNPG operator: the prerequisite for every managed
	// `type: postgres` service (ADR-0005). It is not a bare Component because
	// which *preset* a cluster can host depends on more than presence — the
	// `shared` preset needs the Database CRD that arrived in CNPG 1.25 — so the
	// profile records the version and the CRDs the API server actually serves
	// (issue #90).
	CloudNativePG *CloudNativePG `yaml:"cnpg,omitempty" json:"cnpg,omitempty"`

	Flux *Component `yaml:"flux,omitempty" json:"flux,omitempty"`

	// FluxOperator is flux-operator, which is a separate finding from Flux:
	// it manages the Flux installation and publishes a FluxReport the delivery
	// plane prefers over aggregating controller Deployments itself (issue
	// #137). Recording it here is what keeps that preference a *finding* —
	// before #157 the adapter established availability by attempting the read
	// and falling back, which inverts the detection model (ADR-0003:
	// detection tells the planes what exists).
	//
	// LICENSE: flux-operator is AGPL-3.0 and kelson is MIT, so knowing it is
	// installed may only ever come from unstructured reads of its API group —
	// no Go module of theirs is imported anywhere in this repository
	// (docs/architecture.md, "Living with flux-operator").
	FluxOperator *Component `yaml:"fluxOperator,omitempty" json:"fluxOperator,omitempty"`

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

// CloudNativePG is the detected CNPG operator (issue #90).
//
// Presence alone does not answer the question a database service asks. kelson's
// database rendering is declarative end to end — managed roles for application
// credentials, the Database CRD for the `shared` preset of ADR-0007, schemas
// and extensions on that Database — and each of those arrived in a different
// CNPG release. So the profile records the raw facts (version, namespace, the
// CRDs the API server actually serves) and leaves the per-capability verdict to
// internal/clusterprofile/postgres, which is where the version floors live.
//
// kelson targets the *latest* CNPG as its baseline (owner decision,
// 2026-08-13). An older operator is not a global refusal, it is a list of
// declarative capabilities it cannot serve — which is why this records a
// version rather than a boolean.
//
// Only one CNPG operator can run per cluster: its resources are cluster-scoped
// and a second install fights the first. A profile that reports CNPG present is
// therefore an instruction to adopt it, never to install alongside it
// (ADR-0005).
type CloudNativePG struct {
	// Version is the operator version, e.g. 1.25.0, read from the operator
	// Deployment. Empty means "installed, version unknown" — a real state that
	// must not be read as too old.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Namespace is where the operator Deployment runs (cnpg-system by default),
	// so a human told to upgrade it knows where to look.
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	// CRDs are the resources the API server actually serves in the
	// postgresql.cnpg.io group, as plural resource names: clusters, databases,
	// poolers, backups. This is the capability as the cluster reports it rather
	// than as the version implies — the two can disagree when a partial CRD set
	// was applied, and the served set is the one that will accept a manifest.
	//
	// Empty means the served set could not be read; a Gap on "cnpg.crds"
	// records why. Use [CloudNativePG.ServesCRD] rather than testing the slice,
	// so "not served" and "not read" stay distinguishable at the call site.
	CRDs []string `yaml:"crds,omitempty" json:"crds,omitempty"`
}

// ServesCRD reports whether the API server serves this plural resource in the
// postgresql.cnpg.io group, e.g. "databases". False when the set was never read
// — callers that need to tell that from a real absence must check len(CRDs) or
// the detection Gap first, which is what the postgres judgement does.
func (c CloudNativePG) ServesCRD(plural string) bool {
	for _, name := range c.CRDs {
		if name == plural {
			return true
		}
	}
	return false
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
//
// Detected but never rendered against: kelson renders Gateway API only since
// #140. It stays here as advisory data for the migration nudge (#112) — a
// cluster running a retired ingress controller is exactly who needs to be told
// what kelson will and will not do for them.
type IngressClass struct {
	Name string `yaml:"name" json:"name"`
	// Controller is the controller name from the IngressClass spec, e.g.
	// k8s.io/ingress-nginx. Reported so a human can tell two classes apart.
	Controller string `yaml:"controller,omitempty" json:"controller,omitempty"`
	// Default marks the class annotated as the cluster default: a decision the
	// cluster's owner made, which the nudge should name rather than guessing
	// from an accident of detection order.
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

// StorageClass is one storage class, whether it can snapshot, and what a
// snapshot on it actually costs (issues #108, #91).
//
// The cost is the part that decides whether database branching is a feature or
// a trap: a thin copy-on-write clone is seconds and no extra space, a full-copy
// snapshot is minutes and a second copy of the database, and the k3s default
// has no snapshot driver at all (ADR-0007). Recording all three states, plus
// how confidently each was determined, is what lets the branching judgement
// name the mechanism instead of silently doing something expensive.
//
// Not recorded here: whether an object-store backup destination is configured,
// which ADR-0007 makes the universal branching fallback. Nothing in the spec
// model configures one yet — that is issue #94's scope (a destination per
// environment) — and inventing a field the rest of kelson cannot fill would
// make an unconfigured cluster indistinguishable from an unread one. Until
// then, the fallback's availability is unknown by construction and the
// branching verdict says so (internal/clusterprofile/storage).
type StorageClass struct {
	Name        string `yaml:"name" json:"name"`
	Provisioner string `yaml:"provisioner,omitempty" json:"provisioner,omitempty"`
	Default     bool   `yaml:"default,omitempty" json:"default,omitempty"`
	// VolumeSnapshotClass is a snapshot class whose driver matches this
	// class's provisioner. Empty means snapshot-based database branching is
	// unavailable on this class and the restore-based path applies instead
	// (issue #108).
	VolumeSnapshotClass string `yaml:"volumeSnapshotClass,omitempty" json:"volumeSnapshotClass,omitempty"`
	// SnapshotDriver is the CSI driver of that snapshot class, e.g.
	// rbd.csi.ceph.com. It is reported separately from the class name because
	// the driver — not the name an administrator chose — is what determines
	// what a snapshot costs.
	SnapshotDriver string `yaml:"snapshotDriver,omitempty" json:"snapshotDriver,omitempty"`
	// CloneCapability is what cloning this class's volumes costs.
	// Empty is read as CloneUnknown, so a hand-written profile that omits it
	// cannot read as a promise.
	CloneCapability CloneCapability `yaml:"cloneCapability,omitempty" json:"cloneCapability,omitempty"`
	// CloneConfidence is how that answer was reached, because a lookup by
	// driver name and a direct observation are not equally trustworthy and the
	// difference has to survive into the report (issue #91).
	CloneConfidence CloneConfidence `yaml:"cloneConfidence,omitempty" json:"cloneConfidence,omitempty"`
}

// CloneCapability is what it costs to clone a volume on a storage class: the
// three-way distinction issue #91's acceptance criterion asks the profile to
// make, plus the unknown that keeps a guess from masquerading as an answer.
type CloneCapability string

const (
	// CloneUnknown means the cost could not be determined — an unrecognised CSI
	// driver, or snapshot classes the probe could not read. Never a guess.
	CloneUnknown CloneCapability = "unknown"
	// CloneNone means volumes here cannot be snapshotted at all: no snapshot
	// class matches the provisioner. The k3s local-path default is this case,
	// and branching must fall back to a restore (ADR-0007).
	CloneNone CloneCapability = "none"
	// CloneFullCopy means a snapshot restores into a full-size volume: EBS, GCE
	// PD, Azure Disk. Branching works and costs time and space proportional to
	// the database.
	CloneFullCopy CloneCapability = "full-copy"
	// CloneThin means copy-on-write clones: Ceph RBD, ZFS, LVM-thin. Branching
	// is seconds and near-zero extra space.
	CloneThin CloneCapability = "thin"
)

// CloneConfidence records how a [CloneCapability] was arrived at. It exists
// because issue #91 asks explicitly for the confidence to be recorded rather
// than for the answer to look uniform: "the driver is in our table" and "we
// watched the cluster report no snapshot class" are different kinds of true,
// and an unrecognised driver is neither.
type CloneConfidence string

const (
	// CloneConfidenceObserved means the answer came from cluster state alone:
	// no snapshot class serves this provisioner, so nothing can clone it. No
	// table was consulted and none would change the answer.
	CloneConfidenceObserved CloneConfidence = "observed"
	// CloneConfidenceKnownDriver means the CSI driver is in the maintained
	// table in internal/clusterprofile/storage.
	CloneConfidenceKnownDriver CloneConfidence = "known-driver"
	// CloneConfidenceUnknownDriver means snapshots exist but the driver is not
	// in the table, so whether they are thin is unknown — reported as such
	// rather than assumed either way.
	CloneConfidenceUnknownDriver CloneConfidence = "unknown-driver"
	// CloneConfidenceUnreadable means the snapshot classes could not be listed
	// at all; the matching Gap in Incomplete carries the permission that would
	// settle it.
	CloneConfidenceUnreadable CloneConfidence = "unreadable"
)

// Capability normalises the recorded capability, treating the empty value a
// hand-written profile may omit as [CloneUnknown]. Judgements read this rather
// than the field, so "not written down" and "written down as unknown" behave
// identically and neither can be mistaken for a yes.
func (s StorageClass) Capability() CloneCapability {
	if s.CloneCapability == "" {
		return CloneUnknown
	}
	return s.CloneCapability
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

// Gap is one thing detection could not determine: the reason record behind an
// [OutcomeUnknown]. Judgements find the one that applies with
// [ClusterProfile.GapFor].
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

// DefaultIngressClass returns the class the cluster marks default, else the
// first detected. Empty when there are none. Advisory only — nothing in the
// renderer consumes it since #140; it exists for the migration nudge (#112)
// and for humans reading a captured profile.
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
