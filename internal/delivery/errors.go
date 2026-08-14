package delivery

import (
	"errors"
	"fmt"
)

// Code is a delivery-plane error code, extending the model taxonomy with the
// last-mile failure classes (issue #32: uniform error surface across adapters).
// Codes are a compatibility promise.
type Code string

const (
	// Conflicted is returned when a server-side apply or git write conflicts
	// with a concurrent writer — the deterministic, loud failure ADR-0001 and
	// issue #40 require (never silent last-write-wins).
	ErrConflicted Code = "delivery/conflict"

	// PrunedTargeted is returned when pruning would delete a resource that
	// lacks kelson provenance labels (issue #33: never touch what kelson did
	// not create).
	ErrNotProvenanced Code = "delivery/not-provenanced"

	// ApplyFailed is the generic last-mile failure; the audit trail and Cause
	// carry detail.
	ErrApplyFailed Code = "delivery/apply-failed"

	// NotWatched is returned when a Git-mode adapter wrote to a path that the
	// reconciler (Flux/Argo) is not watching (issue #34: misconfiguration must
	// be reported, not left hanging).
	ErrNotWatched Code = "delivery/not-watched"

	// Unsupported marks an operation this plane cannot perform on the target
	// it was given, so callers can negotiate up front rather than fail at
	// apply time.
	ErrUnsupported Code = "delivery/unsupported"

	// NotImplemented marks a delivery capability kelson used to have, deleted
	// with the old machinery, and not yet rebuilt on the spine
	// ([ADR-0028](docs/adr/0028-delivery-spine.md)).
	//
	// It is deliberately its own code rather than a reuse of
	// delivery/unsupported. "Unsupported" is a statement about the target — ask
	// something else and it works — and this is a statement about kelson: the
	// capability is gone from every target until the tracked work lands. An
	// agent must be able to tell "try another way" from "there is no way yet",
	// and the difference between those two is the difference between retrying
	// and stopping.
	//
	// The taxonomy is the one internal/model's `schema/not-implemented` gate
	// table already established (notimplemented.go): a thing kelson cannot do
	// is refused by name, with the tracking issue in the remediation, rather
	// than half-wired or silently skipped.
	ErrNotImplemented Code = "delivery/not-implemented"

	// ImmutableField is returned when the API server refuses an update because
	// a field of the live object cannot change in place. It is a distinct code
	// from ApplyFailed because the remedy is distinct and unusual: there is
	// nothing to fix in the spec — the spec is what the object should be — and
	// re-deploying will fail identically until the live object is deleted.
	// The component rename's selector change
	// ([ADR-0032](docs/adr/0032-finish-the-component-rename.md)) is the case
	// that motivated it, and a caller that switches on the code should not have
	// to parse a Kubernetes validation message to tell "your spec is wrong"
	// from "delete this and deploy again".
	//
	// The direct adapter that raised it was deleted with the direct plane
	// ([ADR-0028](docs/adr/0028-delivery-spine.md)); the code and its helpers
	// stay because the failure belongs to any last mile that applies to a live
	// API server, and the spine's reconcilers meet it too.
	ErrImmutableField Code = "delivery/immutable-field"

	// ReleaseFailed is returned when a component's release command — the
	// migration hook of issue #104 — did not succeed. It is a distinct code
	// from ApplyFailed because the situation it describes is distinct and the
	// action it asks for is too: nothing is wrong with the manifests, the
	// cluster accepted everything it was given, and the deploy stopped
	// deliberately BEFORE the workloads rolled. The previous revision is still
	// serving, and the fix is in the migration, not in the spec.
	ErrReleaseFailed Code = "delivery/release-failed"
)

// Error is one structured delivery failure, sharing the shape of the model
// taxonomy (code/resource/field/message/remediation/docsUrl) so the CLI, UI
// and agents can react without parsing prose (issues #28, #32).
type Error struct {
	Code        Code   `json:"code"`
	Resource    string `json:"resource"`
	Field       string `json:"field,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	DocsURL     string `json:"docsUrl"`
	Cause       string `json:"cause,omitempty"`
}

func (e Error) Error() string {
	loc := e.Resource
	if e.Field != "" {
		loc += "." + e.Field
	}
	msg := fmt.Sprintf("%s [%s] %s: %s (see %s)", loc, e.Code, e.Message, e.Remediation, e.DocsURL)
	if e.Cause != "" {
		msg += " (cause: " + e.Cause + ")"
	}
	return msg
}

const deliveryDocsBase = "https://kelson.dev/delivery/errors"

// newError builds an Error with a derived docs URL.
func newError(code Code, resource, field, msg, remediation string) Error {
	return Error{
		Code:        code,
		Resource:    resource,
		Field:       field,
		Message:     msg,
		Remediation: remediation,
		DocsURL:     deliveryDocsBase + "/" + string(code),
	}
}

// AsConflict reports whether err is a delivery/conflict (issue #40).
func AsConflict(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrConflicted
}

// Conflict is a helper to construct an optimistic-concurrency failure.
func Conflict(resource, field, msg, remediation string) Error {
	return newError(ErrConflicted, resource, field, msg, remediation)
}

// NotProvenanced is a helper to report a prune candidate kelson does not own
// (issue #33). It is a refusal, not a failure: the resource is left untouched.
func NotProvenanced(resource, field, msg, remediation string) Error {
	return newError(ErrNotProvenanced, resource, field, msg, remediation)
}

// AsNotProvenanced reports whether err is a delivery/not-provenanced refusal.
func AsNotProvenanced(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrNotProvenanced
}

// NotWatched is a helper to construct the "we wrote somewhere no reconciler is
// looking" failure (issue #34). Misconfiguration must be reported, never left
// hanging as a deployment that silently never arrives.
func NotWatched(resource, field, msg, remediation string) Error {
	return newError(ErrNotWatched, resource, field, msg, remediation)
}

// AsNotWatched reports whether err is a delivery/not-watched (issue #34).
func AsNotWatched(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrNotWatched
}

// ApplyFailed is a helper to construct a generic last-mile failure.
func ApplyFailed(resource, field, msg, remediation string) Error {
	return newError(ErrApplyFailed, resource, field, msg, remediation)
}

// AsApplyFailed reports whether err is a delivery/apply-failed.
func AsApplyFailed(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrApplyFailed
}

// ImmutableField is a helper to construct the "this cannot be changed in
// place" refusal. The field is named so the answer says what is stuck, and the
// remediation must name the delete — a retry cannot help.
func ImmutableField(resource, field, msg, remediation string) Error {
	return newError(ErrImmutableField, resource, field, msg, remediation)
}

// AsImmutableField reports whether err is a delivery/immutable-field, so a
// caller can say "delete this object and deploy again" rather than "the deploy
// failed".
func AsImmutableField(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrImmutableField
}

// ReleaseFailed is a helper to construct a failed release-command hook
// (issue #104). The Job is named as the resource; its logs travel in Cause.
func ReleaseFailed(resource, field, msg, remediation string) Error {
	return newError(ErrReleaseFailed, resource, field, msg, remediation)
}

// AsReleaseFailed reports whether err is a delivery/release-failed, so a caller
// can say "the migration failed, your previous revision is still live" rather
// than "the deploy failed".
func AsReleaseFailed(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrReleaseFailed
}

// NotImplemented reports a delivery capability the spine rebuild removed and
// has not replaced yet, naming the issue that tracks its return.
//
// resource is what the caller asked for in kelson's own vocabulary ("deploy",
// "rollback", "history"), what is the sentence explaining the gap, and tracking
// is the issue reference — "#224", never a bare number and never prose without
// one, because the whole point of the code is that a caller can find out when
// the answer will change.
func NotImplemented(resource, what, tracking string) Error {
	return newError(ErrNotImplemented, resource, "", what,
		"the delivery spine is being rebuilt on the controller (ADR-0028): render, publish an OCI "+
			"artifact, let Flux reconcile. This capability returns with "+tracking+
			". `kelson render` and `kelson diff` are unaffected and work offline.")
}

// AsNotImplemented reports whether err is a delivery/not-implemented refusal,
// so a caller can say "not yet" rather than "it failed".
func AsNotImplemented(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrNotImplemented
}

// UnsupportedError reports a capability mismatch (issue #32).
func UnsupportedError(adapter, op string) error {
	return newError(ErrUnsupported, adapter, op,
		"adapter does not support this operation",
		"check the adapter's capabilities before calling "+op)
}

var _ error = Error{}

// AsUnsupported reports whether err is a delivery/unsupported capability
// mismatch, so a caller can tell it apart from a delivery/not-implemented gap
// in kelson itself.
func AsUnsupported(err error) bool {
	var de Error
	return errors.As(err, &de) && de.Code == ErrUnsupported
}
