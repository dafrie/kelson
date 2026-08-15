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

	// ReasonRollbackTargetUnknown — AnnotationRollbackTo names a revision that
	// is not in status.history. kelson will not point an OCIRepository at a tag
	// it cannot confirm it published; the mirror is bounded at
	// MaxHistoryEntries, so a target older than the window is this too.
	ReasonRollbackTargetUnknown = "RollbackTargetUnknown"
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
