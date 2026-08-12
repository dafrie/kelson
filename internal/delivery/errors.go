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

	// Unsupported marks an operation the adapter's Capabilities forbid, so
	// callers can negotiate up front rather than fail at apply time.
	ErrUnsupported Code = "delivery/unsupported"
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

// UnsupportedError reports a capability mismatch (issue #32).
func UnsupportedError(adapter, op string) error {
	return newError(ErrUnsupported, adapter, op,
		"adapter does not support this operation",
		"check the adapter's capabilities before calling "+op)
}

var _ error = Error{}