package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReady is the summary condition both kinds carry (ADR-0027
// decision 1). Everything else a reader might want to know is a reason on it.
const ConditionReady = "Ready"

// ConditionProgressing says whether kelson is still working on this
// Environment, which is a different question from whether it is Ready.
//
// It exists for one situation that Ready cannot express: a rolled-back
// environment is Ready — the revision it is pinned to is live and healthy — and
// is *deliberately not tracking the spec* (ADR-0028 decision 5). Progressing
// carries that fact, with reason RollbackPinned and a message naming both ways
// out, so "why is my spec edit not deploying" is answered by
// `kubectl describe` rather than by reading the controller's source.
//
// Only an Environment carries it. A Project has no delivery of its own, so it
// has nothing to be progressing towards.
const ConditionProgressing = "Progressing"

// ConditionReachable is the forge's own answer, and only a GitConnection
// carries it (ADR-0033 decision 1).
//
// It is separate from Ready because the two fail for opposite reasons and want
// opposite fixes: Ready=False is a document or a Secret kelson can see is wrong,
// Reachable=False is everything kelson cannot see — a revoked installation, an
// expired token, a forge that is down, an instance behind a NAT. Collapsing
// them would make "your connection is broken" the answer to both, which is the
// checked-versus-could-not-check distinction the ClusterProfile already refuses
// to blur.
const ConditionReachable = "Reachable"

// AnnotationRollbackTo pins an Environment to a revision it has already
// published (ADR-0028 decision 5):
//
//	kubectl annotate environment production kelson.dev/rollback-to=6-9f0a1b2c
//
// While it is in force the controller repoints the OCIRepository at that
// immutable tag and suspends re-rendering: steps 3 and 4 do not run, so the
// current spec cannot be republished over the thing you just rolled back to.
// Two things resume tracking and only two — removing the annotation, or editing
// the spec, because a spec edit is an unambiguous statement of new intent and
// an operator who has just fixed the bug should not have to remember an
// annotation as well.
//
// `kelson rollback` is porcelain over this.
const AnnotationRollbackTo = "kelson.dev/rollback-to"

// AnnotationOrphanOnDelete makes deleting an Environment leave its workloads
// running (issue #242, ADR-0028's 2026-08-15 amendment):
//
//	kubectl annotate environment production kelson.dev/orphan-on-delete=true
//
// The default is the opposite and stays the default: deleting an Environment
// deletes kelson's Kustomization, the Kustomization prunes everything it
// applied, and the running application goes with it (ADR-0028's 2026-08-14
// amendment). That is the one place where deleting a kelson custom resource
// destroys something — `kelson uninstall` removes kelson and leaves every
// application running (issue #59) — and the asymmetry is exactly why there is an
// opt-out for a single environment.
//
// With the annotation in force the finalizer still runs and still releases
// itself; the only thing it skips is the teardown. The OCIRepository and the
// Kustomization stay where they are, keep their kelson.dev provenance labels,
// and keep reconciling the last artifact kelson published — with nothing left
// that declares them, which is what *orphaned* means here. Re-applying an
// Environment of the same name in the same namespace adopts the pair back;
// deleting the Kustomization by hand tears the workloads down after all
// (docs/delivery.md).
//
// It is read at deletion time, not at apply time, so it can be added to an
// Environment that is already Terminating — which is how an operator releases
// one whose teardown is stuck. It skips whatever teardown has not happened yet
// and cannot restore what has.
//
// Any value Go's strconv.ParseBool reads as true opts in, and "false" is the
// default said out loud. A value it cannot read at all — `ture`, `yes`, or an
// empty string — opts in as well, and the controller records that it did:
// kelson will not prune a production namespace because somebody mistyped the
// annotation whose whole purpose was to prevent that, and deleting the
// Kustomization afterwards is one command where restoring what it pruned is an
// outage.
const AnnotationOrphanOnDelete = "kelson.dev/orphan-on-delete"

// The reasons ConditionReady takes. They are a closed set on purpose: a reason
// is what a `kubectl get -o jsonpath` or an agent branches on, so an ad-hoc
// string invented at a call site is a value nobody can write a check against.
const (
	// ReasonReady — the spec validated, resolved and rendered.
	ReasonReady = "Ready"

	// ReasonSpecInvalid — internal/model/validate.go refused the document.
	// The errors are in Status.ValidationErrors with their slash codes, and
	// nothing downstream ran (ADR-0028 decision 1, step 1).
	ReasonSpecInvalid = "SpecInvalid"

	// ReasonProjectNotFound — spec.project names a Project that does not exist
	// in this namespace. It is its own reason rather than a validation error
	// because it is not a property of the document: the same Environment
	// becomes valid the moment the Project is applied, and an author who
	// applied two files in the wrong order needs to be told that and not told
	// their spec is wrong.
	ReasonProjectNotFound = "ProjectNotFound"

	// ReasonRenderFailed — the document validated but the renderer refused it,
	// which is a kelson-side gap (an unimplemented field, an overlay it cannot
	// resolve) rather than an authoring mistake.
	ReasonRenderFailed = "RenderFailed"

	// ReasonClusterProfileUnavailable — the controller could not read what this
	// cluster provides, so step 2 has no answer to give steps 3 to 5.
	//
	// It is deliberately *not* FluxNotInstalled. A probe that failed and a
	// cluster that genuinely has no Flux look identical in an empty
	// ClusterProfile and mean opposite things: one is fixed by `kelson install`,
	// the other by looking at RBAC or at the API server, and telling an operator
	// to install Flux they already have is how a controller sends somebody down
	// the wrong path for an afternoon. The controller retries with backoff and
	// re-probes each time.
	ReasonClusterProfileUnavailable = "ClusterProfileUnavailable"

	// ReasonSourcesUnavailable — the controller could not list the GitSources
	// this instance declares (ADR-0035 decision 2), so resolution cannot tell
	// whether a component's `source:` names one.
	//
	// It is its own reason for the same reason ReasonClusterProfileUnavailable
	// is: a listing that failed and an instance that declares no GitSource
	// produce the same empty list and mean opposite things. Resolving against
	// the empty one would refuse the component with `ref/unknown-source` — a
	// SpecInvalid pointing at a spec that is fine — and send an author to edit
	// a binding that was never wrong. The controller retries with backoff and
	// lists again.
	ReasonSourcesUnavailable = "SourcesUnavailable"

	// ReasonRolledBack — the environment is serving a revision it was pinned to
	// by AnnotationRollbackTo. Ready is True: the pinned artifact is live. What
	// is *not* true is that the environment tracks its spec, and that is what
	// ConditionProgressing says (ADR-0028 decision 5).
	ReasonRolledBack = "RolledBack"
)

// The reasons the delivery steps refuse with (ADR-0028 decision 1, steps 4 to
// 6). They are a closed set for the same reason the validation reasons are, and
// internal/controller/errors.go maps each one to exactly one requeue behaviour —
// so what a reader sees in a condition also tells them whether anything is
// going to happen next without them.
const (
	// ReasonFluxNotInstalled — the ClusterProfile reports no Flux, so there is
	// nothing in the cluster that would reconcile what kelson published.
	//
	// This is the *expected* state of a fresh cluster (ADR-0030), not a
	// malfunction: the controller waits on a timer, returns no error, and never
	// crash-loops. `kelson install` is the fix.
	ReasonFluxNotInstalled = "FluxNotInstalled"

	// ReasonRegistryNotConfigured — the controller was never told where to
	// publish (--registry / KELSON_REGISTRY). A registry is a hard requirement
	// of the spine and this is the sharpest new edge in the rebuild (ADR-0028,
	// "Consequences"), so it is named rather than folded into a push failure.
	ReasonRegistryNotConfigured = "RegistryNotConfigured"

	// ReasonArtifactRefInvalid — the registry prefix, the pair's names or the
	// generation do not make a repository and a tag. Nothing will fix itself.
	ReasonArtifactRefInvalid = "ArtifactRefInvalid"

	// ReasonRegistryUnreachable — the registry did not answer. Transient by
	// assumption, so it is the one delivery failure that returns an error and
	// takes controller-runtime's exponential backoff.
	ReasonRegistryUnreachable = "RegistryUnreachable"

	// ReasonPushDenied — the registry answered, and said no. A credential
	// problem is an operator's to fix, on human time, so this waits on a timer
	// rather than retrying into a rate limit.
	ReasonPushDenied = "PushDenied"

	// ReasonFluxApplyForbidden — the API server refused the OCIRepository or
	// the Kustomization write. That is RBAC, which is an operator's to grant.
	ReasonFluxApplyForbidden = "FluxApplyForbidden"

	// ReasonFieldManagerConflict — a server-side apply hit a field another
	// manager owns. kelson applies with ForceOwnership, so reaching this means
	// something structural is contended and a human has to look.
	ReasonFieldManagerConflict = "FieldManagerConflict"

	// ReasonNameConflict — a live OCIRepository or Kustomization of the name
	// this pair would use already belongs to a different environment namespace.
	// Applying over it would silently redirect somebody else's deployment, so
	// kelson refuses and changes nothing.
	ReasonNameConflict = "NameConflict"

	// ReasonRollbackTargetUnknown — AnnotationRollbackTo names a revision
	// neither status.history nor the registry holds. kelson will not point an
	// OCIRepository at a tag it cannot confirm it published, and both places it
	// can confirm one have been asked: the bounded mirror, and the registry's
	// own tag list, which is the record the mirror mirrors (ADR-0028
	// decision 4, issue #241).
	ReasonRollbackTargetUnknown = "RollbackTargetUnknown"

	// ReasonRegistryReadDenied — the registry answered a read and said no. It
	// is separate from ReasonPushDenied because the fix is: a credential
	// scoped to pull as well as push. Reporting it as "that revision does not
	// exist" would turn "kelson may not look" into a fact about the registry's
	// contents, which is the one mistake a durable record must not make.
	ReasonRegistryReadDenied = "RegistryReadDenied"
)

// The reasons ConditionReady takes when step 6 observed a delivery that ended
// badly (ADR-0028 decision 1, step 6). They are as closed as the two sets above
// and they are their own block because none of them is a *refusal*: the render,
// the publish and the apply of the Flux pair all succeeded, and what is being
// reported is what the cluster then did with them. That is also why
// internal/controller/errors.go holds no requeue row for them — there is no
// error to return and nothing to back off from, only a phase to re-observe.
//
// One reason per cause is the whole point of the block. Before it, every
// badly-ended phase carried ReasonRenderFailed, so a crash-looping Deployment
// was reported as a renderer that had refused a document it rendered perfectly
// (issue #256).
const (
	// ReasonApplyFailed — kelson published the revision and Flux would not put
	// it on the cluster: the kustomize build, the decryption, the dry-run or
	// the apply itself failed (phase Rejected). The message is Flux's own
	// words, which is what makes it actionable.
	//
	// It is not ReasonFluxApplyForbidden, which is the other end of the
	// pipeline: that one is the API server refusing *kelson's* write of the
	// OCIRepository and the Kustomization, before Flux has seen the revision at
	// all.
	ReasonApplyFailed = "ApplyFailed"

	// ReasonWorkloadDegraded — the revision is live and the workload readback
	// found a definitive failure under it (phase Degraded; issue #240).
	// status.workloads carries the counts and names the workload, the pod and
	// the container; the condition message names the first of them.
	ReasonWorkloadDegraded = "WorkloadDegraded"

	// ReasonUnhealthy — the revision is live and reported unhealthy, and kelson
	// cannot name a workload for it (phase Degraded): a Kustomization health
	// check or a prune that failed over a kind the readback does not classify
	// (a CronJob, a CNPG Cluster), or a readback that was never wired or was
	// refused.
	//
	// It is the coarse half of a pair on purpose. ReasonWorkloadDegraded is
	// what a reader gets when kelson *can* say which workload, and collapsing
	// the two would make "something under this environment is unhealthy" and
	// "Deployment/…/web is crash-looping, and status.workloads names the pod"
	// the same answer — which is the distinction the whole readback exists to
	// draw.
	ReasonUnhealthy = "Unhealthy"
)

// The reasons ConditionProgressing takes.
const (
	// ReasonRollbackPinned — re-rendering is suspended because
	// AnnotationRollbackTo is in force. The message names both ways out.
	ReasonRollbackPinned = "RollbackPinned"

	// ReasonReconciling — a revision is published and Flux has not finished
	// with it.
	ReasonReconciling = "Reconciling"

	// ReasonSettled — there is nothing left to do for the current generation,
	// whether that ended well (Healthy) or badly (Rejected, Degraded). Ready is
	// what says which.
	ReasonSettled = "Settled"
)

// The Environment phase vocabulary, mirroring internal/delivery/statemachine's
// (which is the engine that will drive it once delivery is real — ADR-0028
// decision 1, step 6).
//
// They are declared as untyped string constants here rather than imported,
// because internal/delivery cannot be part of a public package's contract.
// api/kelson/v1alpha1/phase_test.go asserts the two lists are identical, so the
// copy cannot drift into a second vocabulary.
const (
	PhaseProposed    = "Proposed"
	PhaseCommitted   = "Committed"
	PhaseReconciling = "Reconciling"
	PhaseApplied     = "Applied"
	PhaseHealthy     = "Healthy"
	PhaseRejected    = "Rejected"
	PhaseDegraded    = "Degraded"
)

// The workload health vocabulary, mirroring internal/observation's Code
// (ADR-0028 decision 1, step 6; issue #240).
//
// They are declared as untyped string constants here for exactly the reason the
// Phase* constants above are: internal/observation cannot be part of a public
// package's contract, and a status field is a contract. Nothing here computes
// one — internal/observation does, and internal/controller copies the value
// across — and api/kelson/v1alpha1/workload_test.go asserts the two lists are
// identical so the copy cannot drift into a second dialect. A code that meant
// `crash-loop-back-off` in `kelson status` and something else in `kubectl get
// environment` would be worse than no code at all.
const (
	// WorkloadHealthy — every pod in the workload's selector set is ready and
	// the workload's own conditions agree.
	WorkloadHealthy = "healthy"

	// WorkloadProgressing — no failure yet, and not finished either. It is a
	// wait state and never a diagnosis.
	WorkloadProgressing = "progressing"

	// WorkloadCrashLoopBackOff — the kubelet named a container's waiting reason
	// CrashLoopBackOff, or the container has terminated repeatedly.
	WorkloadCrashLoopBackOff = "crash-loop-back-off"

	// WorkloadImagePullBackOff — the image cannot be pulled.
	WorkloadImagePullBackOff = "image-pull-back-off"

	// WorkloadFailingProbe — a container is running and not ready, which is a
	// readiness probe that is not passing.
	WorkloadFailingProbe = "failing-probe"

	// WorkloadInsufficientResources — unschedulable because the cluster lacks
	// the requested CPU or memory.
	WorkloadInsufficientResources = "insufficient-resources"

	// WorkloadSchedulingFailed — unschedulable for a non-resource reason: a node
	// selector, an affinity rule, a taint.
	WorkloadSchedulingFailed = "scheduling-failed"

	// WorkloadMissing — the workload object is not in the cluster.
	WorkloadMissing = "missing"

	// WorkloadSecretSyncFailed — an ExternalSecret backing this workload has
	// Ready=False, so the Secret its pods reference is not being written.
	WorkloadSecretSyncFailed = "secret-sync-failed"
)

// MaxUnhealthyWorkloads bounds Status.Workloads.Unhealthy, and
// MaxUnhealthyContainers bounds one entry's Containers.
//
// They exist for the reason [MaxHistoryEntries] does, one step sharper: this
// list is written from live cluster state that a bad rollout can make
// arbitrarily large — fifty replicas that all CrashLoopBackOff are fifty
// failing containers — and every byte of it goes into every watch event every
// controller in the cluster receives. The counts beside the list are the
// complete answer to "how bad is it"; the list is the sample that makes it
// diagnosable, and a sample does not have to be exhaustive to name the pod.
//
// Anything past the bound is dropped rather than summarized, in a defined
// order: workloads by resource name, containers by pod then container name. A
// truncated list is therefore the same list every reconcile rather than a
// rotating window, which is what keeps `kubectl get -w` from showing a status
// that churns while nothing changed.
const (
	MaxUnhealthyWorkloads  = 10
	MaxUnhealthyContainers = 5
)

// MaxHistoryEntries bounds Status.History. It mirrors the delivery plane's
// retention default (ADR-0028 decision 4): the record of what was deployed is
// the registry's tag list, and this is a window onto it for humans and for the
// API — not the record itself. A status that grew without bound would put the
// whole deployment history in every watch event every controller in the cluster
// receives.
const MaxHistoryEntries = 20

// ValidationError is one entry of `status.validationErrors`: a
// [github.com/dafrie/kelson/internal/model.Error] as it appears on a custom
// resource.
//
// The field names are model.Error's, deliberately. ADR-0027 decision 5 makes the
// point that there is *one* taxonomy — the CLI, the wire API and this status
// carry the same codes, the same field paths and the same remediation text — so
// an agent that learned to branch on `schema/unknown-field` at the CLI branches
// on it here without translation. Renaming a field on the way into the status
// would make the status a second dialect of the same vocabulary.
//
// It is a type and not a conversion: turning model.Errors into these is the
// controller's job (internal/controller), because this package holds no
// behaviour.
type ValidationError struct {
	// Code is the stable, machine-actionable code, e.g. schema/unknown-field.
	Code string `json:"code"`

	// Resource names the document the error is about, e.g. "Environment/prod".
	Resource string `json:"resource,omitempty"`

	// Field is the JSONPath-like location within it, e.g.
	// $.spec.components[2].port.
	Field string `json:"field,omitempty"`

	// Message says what is wrong.
	Message string `json:"message"`

	// Remediation says what to do about it, stated as an action.
	Remediation string `json:"remediation,omitempty"`

	// DocsURL links the code's documentation page.
	DocsURL string `json:"docsUrl,omitempty"`

	// Line and Column locate the error in the authored YAML, 1-based, when the
	// source was available. A document that arrived as a custom resource has no
	// source text, so they are usually zero here — the field exists because the
	// same taxonomy carries them when the CLI produced the error (ADR-0027
	// decision 5).
	Line int `json:"line,omitempty"`

	Column int `json:"column,omitempty"`
}

// HistoryEntry is one revision this environment has published: the bounded
// mirror of ADR-0028 decision 4.
//
// It is a mirror and not the record. The record is the registry's tag list,
// which holds every artifact ever published for this environment, immutably;
// this is a window onto it for humans and for the API, bounded at
// [MaxHistoryEntries] because a status that grew without limit would put the
// whole deployment history into every watch event every controller in the
// cluster receives. A query past the window is a registry query.
type HistoryEntry struct {
	// Revision is the artifact tag: <generation>-<spec-hash-short>.
	Revision string `json:"revision"`

	// Digest is the artifact's OCI digest, sha256:… — what was actually put
	// there, as opposed to where it was put. A tag is written once and never
	// rewritten, so the two agree forever; the digest is what proves it.
	Digest string `json:"digest,omitempty"`

	// SpecHash is the hash of the resolved spec this revision was rendered
	// from. Two entries with the same hash are the same input, which is what
	// makes a rollback target recognisable without fetching it.
	SpecHash string `json:"specHash,omitempty"`

	// Images are the container images this revision resolved to, in component
	// order.
	//
	// Deprecated: it is a positional list with the imageless components left
	// out, so nothing downstream can say which component an image belongs to
	// without re-deriving it — which is what internal/api's promote path had to
	// do, and why [HistoryEntry.ComponentImages] exists. It stays populated for
	// one release, as a flat mirror of ComponentImages, so a status reader
	// written against the older shape keeps working; new readers must use
	// ComponentImages.
	Images []string `json:"images,omitempty"`

	// ComponentImages are the same images, each named with the component that
	// resolved it. It is what makes "which build is in production" a `kubectl
	// get` rather than an artifact pull, and it is what a promotion reads from
	// the source environment (ADR-0016 decision 2) — by name now, rather than
	// by guessing from the image repository.
	//
	// A component that resolved no image is absent, as it is from Images: an
	// entry here is a record of something that ran.
	ComponentImages []ComponentImage `json:"componentImages,omitempty"`

	// Outcome is the delivery phase this revision reached — one of the Phase*
	// constants. It is refreshed while the revision is the current one and then
	// frozen, so an old entry says how that deployment ended rather than what
	// it looked like one second after it was published.
	Outcome string `json:"outcome,omitempty"`

	// Timestamp is when the entry was recorded.
	Timestamp metav1.Time `json:"timestamp,omitempty"`
}

// ComponentImage is one component of a revision and the image it resolved to.
//
// The pair is recorded rather than derived because the component name is known
// exactly at the moment the revision is rendered — it is in the resolved spec
// the controller is holding — and is only ever a guess afterwards: a positional
// list shifts when a component is added or removed, and matching by image
// repository is ambiguous the moment two components share one.
type ComponentImage struct {
	// Component is the component's name as the resolved spec spelled it.
	Component string `json:"component"`

	// Image is the fully qualified image reference this revision resolved that
	// component to, tag or digest as authored.
	Image string `json:"image"`
}

// WorkloadsStatus is the observation plane's readback: what the workloads this
// environment's revision applied are actually doing (ADR-0028 decision 1, step
// 6; issue #240).
//
// # Why it exists beside the phase
//
// `status.phase` is Flux's answer, and it is a good one: the Kustomization
// kelson writes carries `wait: true`, so Ready=True already means the applied
// set converged. What it cannot say is *which* thing did not converge, and
// "Kustomization not ready" is where a stuck rollout stops being debuggable
// from `kubectl get environment` and starts requiring three more commands and
// knowledge of where kelson put things. This field is those three commands,
// already run.
//
// # Counts, then a bounded sample
//
// The counts are complete and the list is not: `degraded` is how many workloads
// are failing, `unhealthy` holds at most [MaxUnhealthyWorkloads] of them, and
// the difference is real and deliberate (see the constants). Nothing here is
// authored — a user who writes it is overwritten by the next reconcile.
//
// # Nothing from inside a container
//
// Container *logs* are not here and will not be. The pod name, the container
// name, the kubelet's own reason: those identify the failure and are safe to
// put in an object anybody who can read the Environment can read. A crash dump
// is the single most likely place for a connection string to appear, and an
// Environment's status is not an access-controlled surface for it. `kelson
// logs` reads them, under a grant that was asked for separately.
type WorkloadsStatus struct {
	// Checked is how many workloads were read and classified. Zero with no
	// `unavailable` is the honest answer for an environment that renders no
	// Deployment at all — a CronJob-only spec, say — and not a failure.
	Checked int32 `json:"checked,omitempty"`

	// Healthy, Progressing and Degraded partition Checked. Progressing is a
	// wait state and never a diagnosis: a rollout that has not finished is not
	// a rollout that failed.
	Healthy     int32 `json:"healthy,omitempty"`
	Progressing int32 `json:"progressing,omitempty"`
	Degraded    int32 `json:"degraded,omitempty"`

	// Unhealthy names the failing workloads, at most [MaxUnhealthyWorkloads] of
	// them, ordered by resource name. Only definitive failures appear: a
	// progressing workload is counted above and left out here, because a list
	// that mixed "broken" with "not finished" is the conflation the whole
	// observation plane exists to prevent (issue #53).
	Unhealthy []UnhealthyWorkload `json:"unhealthy,omitempty"`

	// Unavailable is why the readback could not be done, when it could not: the
	// API server refused the list, the namespace is gone, the request timed out.
	//
	// It is a separate field rather than a zero count because "we looked and
	// everything is fine" and "we could not look" are opposite facts that
	// produce identical counts, and reporting the second as the first is how a
	// controller tells somebody their broken environment is healthy. It is the
	// same discipline the ClusterProfile applies to detection, and it never
	// fails the reconcile: the revision was delivered whether or not the
	// readback worked, and a status that is missing one section is better than
	// a deploy that is refused for it.
	Unavailable string `json:"unavailable,omitempty"`
}

// UnhealthyWorkload is one failing workload: what it is, what is wrong with it
// in the closed vocabulary above, and what to do about it.
type UnhealthyWorkload struct {
	// Resource names the workload, e.g. "Deployment/checkout-production/web".
	// The spelling is internal/observation's, unchanged, so it is the same
	// string `kelson status` prints for the same object.
	Resource string `json:"resource"`

	// Code is the machine-actionable verdict — one of the Workload* constants.
	// Branch on this, never on Reason.
	Code string `json:"code"`

	// Reason is the human-readable detail behind the code: the kubelet's own
	// waiting reason, the unschedulable message, the probe condition.
	Reason string `json:"reason,omitempty"`

	// Remediation is the fix stated as an action, one line per code. It is
	// carried rather than derived so that a reader of the YAML gets it without
	// a lookup table, exactly as `status.validationErrors` does.
	Remediation string `json:"remediation,omitempty"`

	// Containers names the failing containers inside it, at most
	// [MaxUnhealthyContainers], ordered by pod then container name. This is the
	// "which pod, which container" half — the reason this readback is worth
	// holding RBAC over what Flux already checked.
	Containers []UnhealthyContainer `json:"containers,omitempty"`
}

// UnhealthyContainer is one failing container, named and diagnosed. It carries
// no output from inside the container — see [WorkloadsStatus].
type UnhealthyContainer struct {
	// Pod is the pod the container is in.
	Pod string `json:"pod,omitempty"`

	// Name is the container's name as the pod spec spelled it.
	Name string `json:"name"`

	// Code is this container's own verdict, which may be less severe than the
	// workload's: the workload reports the worst of them.
	Code string `json:"code"`

	// Reason is the kubelet's reason for this container.
	Reason string `json:"reason,omitempty"`
}

// ProjectStatus is what the controller observed about a Project.
//
// A Project has no delivery of its own — it is the shared half of a spec, and
// what runs is always an Environment — so its status is validation and nothing
// else: did this document parse and validate, as of which generation.
type ProjectStatus struct {
	// ObservedGeneration is the .metadata.generation this status describes. A
	// status whose observedGeneration trails the object's generation has not
	// caught up yet, and every consumer — kubectl wait, an agent, the server —
	// must read it before believing the rest.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries Ready as its summary.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ValidationErrors is what validate.go said, when it said anything. An
	// invalid spec is a status and not a rejection (ADR-0027 decision 5).
	ValidationErrors []ValidationError `json:"validationErrors,omitempty"`
}

// GitSourceStatus is what the control plane observed about a GitSource: whether
// the document is usable, as of which generation, and nothing else (ADR-0035
// decision 2).
//
// The absence of a Reachable condition is the decision, not a gap. A source is
// data — a repository URL, a ref, a connection name — and the question of
// whether that repository answers is a question about the *credential*, which
// lives on the GitConnection and already has a condition there. Asking it twice
// would give an operator two places to look and two answers that can disagree.
type GitSourceStatus struct {
	// ObservedGeneration is the .metadata.generation this status describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries Ready as its summary.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ValidationErrors is what validate.go said about this source, with the
	// slash codes intact. An invalid spec is a status and not a rejection here
	// for the same reason it is on every other kind (ADR-0027 decision 5).
	ValidationErrors []ValidationError `json:"validationErrors,omitempty"`
}

// EnvironmentStatus is what the controller observed about an Environment: the
// validation record a Project also has, plus the delivery record.
type EnvironmentStatus struct {
	// ObservedGeneration is the .metadata.generation this status describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries Ready as its summary.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase is the delivery state-machine phase (the Phase* constants above).
	// It is empty until something has been delivered.
	Phase string `json:"phase,omitempty"`

	// Revision is the settled revision: the artifact tag the OCIRepository is
	// pinned to. Empty until something has been published.
	Revision string `json:"revision,omitempty"`

	// RollbackRevision is the value of AnnotationRollbackTo the controller has
	// acted on. It is what makes a rollback idempotent — a reconcile that sees
	// the same annotation it already honoured does not re-verify or re-apply —
	// and it is what distinguishes a *new* rollback from a standing one.
	RollbackRevision string `json:"rollbackRevision,omitempty"`

	// RollbackGeneration is the .metadata.generation the rollback was applied
	// at. It is the whole mechanism behind "editing the spec resumes tracking"
	// (ADR-0028 decision 5): a generation past this one means the author has
	// stated new intent since the rollback, so the annotation goes inert and
	// normal publishing resumes without anybody having to remember to remove
	// it.
	RollbackGeneration int64 `json:"rollbackGeneration,omitempty"`

	// ValidationErrors is what validate.go said about this Environment and its
	// Project, with the slash codes intact.
	ValidationErrors []ValidationError `json:"validationErrors,omitempty"`

	// History is the bounded mirror of the published revisions, newest first,
	// at most MaxHistoryEntries entries.
	History []HistoryEntry `json:"history,omitempty"`

	// Workloads is what the workloads the current revision applied are doing —
	// the finer-grained answer under Phase (issue #240).
	//
	// It is a pointer because absent and empty are different: nil means this
	// reconcile did not read the workloads back at all (nothing was delivered,
	// or the controller was started without the readback), and a present value
	// with `checked: 0` means it looked and there was nothing of the kind it
	// classifies. It is rewritten wholesale every reconcile that observes,
	// never merged, because a half-refreshed readback is a status that mixes
	// two moments in time.
	Workloads *WorkloadsStatus `json:"workloads,omitempty"`
}

// GitConnectionStatus is what the control plane observed about a GitConnection:
// whether the document is usable, and what the forge said when kelson used it
// (ADR-0033 decision 1).
//
// The two provider-reported fields are the honest answer to "did this actually
// work". A connection whose document validates, whose Secret exists and whose
// credential the forge rejects looks identical to a working one until something
// asks the forge — so the connection asks, and writes down what came back.
// Neither field is authored: an author who sets them is overwritten by the next
// refresh.
type GitConnectionStatus struct {
	// ObservedGeneration is the .metadata.generation this status describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries Ready as its summary and Reachable as the forge's own
	// answer (ConditionReady, ConditionReachable).
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Account is who the credential acts as, as the provider reports it: the
	// organization or user the GitHub App is installed on, or the account a
	// token belongs to. It is what makes a list of connections readable —
	// `provider: github` twice says nothing, `acme` and `acme-staging` says
	// which is which.
	Account string `json:"account,omitempty"`

	// Repositories is how many repositories this credential can see, as the
	// provider reports it. It is a scope readout rather than a count anybody
	// needs: an installation that should cover forty repositories and reports
	// one is a picked-the-wrong-repository mistake, visible in
	// `kubectl get gitconnections` without opening the forge.
	Repositories int32 `json:"repositories,omitempty"`

	// ValidationErrors is what validate.go said about this connection, with the
	// slash codes intact. An invalid spec is a status and not a rejection here
	// for the same reason it is on the other two kinds (ADR-0027 decision 5).
	ValidationErrors []ValidationError `json:"validationErrors,omitempty"`
}
