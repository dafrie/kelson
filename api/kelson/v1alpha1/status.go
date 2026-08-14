package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReady is the summary condition both kinds carry (ADR-0027
// decision 1). Everything else a reader might want to know is a reason on it.
const ConditionReady = "Ready"

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
// It is three fields and not six. ADR-0028 specifies the digest, the resolved
// images and the outcome as well, and every one of them is produced by the
// publish step — which is stubbed behind internal/controller's Deliverer until
// issue #224 lands. A field the controller could only ever write empty would be
// a promise the status does not keep, so they arrive with the thing that fills
// them.
type HistoryEntry struct {
	// Revision is the artifact tag: <generation>-<spec-hash-short>.
	Revision string `json:"revision"`

	// SpecHash is the hash of the resolved spec this revision was rendered
	// from. Two entries with the same hash are the same input, which is what
	// makes a rollback target recognisable without fetching it.
	SpecHash string `json:"specHash,omitempty"`

	// Timestamp is when the entry was recorded.
	Timestamp metav1.Time `json:"timestamp,omitempty"`
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

	// Revision is the settled revision: the artifact tag currently serving.
	// Empty until the publish step exists (issue #224).
	Revision string `json:"revision,omitempty"`

	// ValidationErrors is what validate.go said about this Environment and its
	// Project, with the slash codes intact.
	ValidationErrors []ValidationError `json:"validationErrors,omitempty"`

	// History is the bounded mirror of the published revisions, newest first,
	// at most MaxHistoryEntries entries.
	History []HistoryEntry `json:"history,omitempty"`
}
