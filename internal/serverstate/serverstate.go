// Package serverstate is kelson-server's state plane: the spec documents
// clients converge on and the deployment history of what kelson actually did
// (issue #139, ADR-0013 §1).
//
// # All server state is cluster state
//
// The kelson-server process holds nothing a restart or a second replica would
// lose or fork. Every byte this package owns lives in ConfigMaps in one
// namespace, carrying kelson's provenance labels, which makes
// `kubectl get configmaps -l kelson.dev/project=shop` the audit trail and
// namespaced RBAC the access control.
//
// ConfigMaps rather than a CRD, deliberately. ADR-0003's additive-install
// doctrine means the server must work against a cluster where kelson has
// installed nothing: ConfigMaps need no CRD registration and no admission
// wiring, and `get/list/watch/create/update/delete configmaps` in one namespace
// is the entire permission footprint. A CRD-backed store is the recorded
// migration target for when the controller exists (M13+); it lands behind the
// same interfaces (direct.History here, SpecStore's methods for specs) so the
// swap does not touch a handler.
//
// # Optimistic concurrency is Kubernetes resourceVersion
//
// Spec writes carry the ConfigMap's resourceVersion through as an opaque
// version string. kelson does not reimplement what the API server already
// guarantees; a stale write comes back as a store/version-conflict.
//
// # Idempotency is honest about its scope
//
// Mutating calls record their idempotency key as an annotation on the object
// they wrote, and a replayed key returns the recorded outcome. There is no
// distributed dedup beyond what one namespace's ConfigMaps provide — two
// servers pointed at different namespaces do not share keys.
package serverstate

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Provenance keys, matching what the renderer stamps on deployed resources
// (docs/architecture.md, Provenance) so state objects and deployed objects are
// discoverable by the same queries.
const (
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelProject     = "kelson.dev/project"
	labelEnvironment = "kelson.dev/environment"
	labelRevision    = "kelson.dev/revision"

	// labelState separates the two kinds of state object living in the same
	// namespace, so listing specs never has to filter history out by name
	// prefix.
	labelState = "kelson.dev/state"

	stateSpec    = "spec"
	stateHistory = "history"

	managedByKelson = "kelson"

	// annIdempotencyKey records the key of the write that produced the current
	// contents (ADR-0013 §2).
	annIdempotencyKey = "kelson.dev/idempotency-key"
)

// Code is a state-plane error code. The wire error taxonomy passes codes
// through verbatim (ADR-0013 §2), so these strings are a compatibility promise
// and agents branch on them.
type Code string

const (
	// ErrVersionConflict is a write whose expected version does not match the
	// stored one — the optimistic-concurrency failure #69 requires.
	ErrVersionConflict Code = "store/version-conflict"

	// ErrNotFound is a read or write against state that does not exist.
	ErrNotFound Code = "store/not-found"

	// ErrTooLarge is a payload that cannot fit a ConfigMap. It fails the call
	// rather than storing a truncated record: a history that lies is worse
	// than none (ADR-0013 §1).
	ErrTooLarge Code = "store/too-large"
)

// Error is one structured state-plane failure. It shares the shape of
// delivery.Error (code/resource/message/remediation/docsUrl/cause) so the API
// layer maps every plane's errors onto the one wire Error without inventing a
// second taxonomy.
type Error struct {
	Code        Code   `json:"code"`
	Resource    string `json:"resource"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	DocsURL     string `json:"docsUrl"`
	Cause       string `json:"cause,omitempty"`
}

func (e Error) Error() string {
	msg := fmt.Sprintf("%s [%s] %s: %s (see %s)", e.Resource, e.Code, e.Message, e.Remediation, e.DocsURL)
	if e.Cause != "" {
		msg += " (cause: " + e.Cause + ")"
	}
	return msg
}

const storeDocsBase = "https://kelson.dev/server/errors"

func newError(code Code, resource, msg, remediation string) Error {
	return Error{
		Code:        code,
		Resource:    resource,
		Message:     msg,
		Remediation: remediation,
		DocsURL:     storeDocsBase + "/" + string(code),
	}
}

// VersionConflict reports a write that lost an optimistic-concurrency check.
func VersionConflict(resource, msg, remediation string) Error {
	return newError(ErrVersionConflict, resource, msg, remediation)
}

// NotFound reports state that does not exist.
func NotFound(resource, msg, remediation string) Error {
	return newError(ErrNotFound, resource, msg, remediation)
}

// TooLarge reports a payload a ConfigMap cannot hold.
func TooLarge(resource, msg, remediation string) Error {
	return newError(ErrTooLarge, resource, msg, remediation)
}

// AsVersionConflict reports whether err is a store/version-conflict.
func AsVersionConflict(err error) bool { return hasCode(err, ErrVersionConflict) }

// AsNotFound reports whether err is a store/not-found.
func AsNotFound(err error) bool { return hasCode(err, ErrNotFound) }

// AsTooLarge reports whether err is a store/too-large.
func AsTooLarge(err error) bool { return hasCode(err, ErrTooLarge) }

func hasCode(err error, code Code) bool {
	var se Error
	return errors.As(err, &se) && se.Code == code
}

func withCause(e Error, cause error) Error {
	e.Cause = cause.Error()
	return e
}

var _ error = Error{}

// validSegment keeps a caller-supplied identifier safe to splice into an object
// name and a label value. It is the cluster-side counterpart of direct's
// path-safety check: there the risk is escaping the data dir, here it is
// building a name the API server rejects — or worse, one that collides with
// another project's. A DNS-1123 label is the strictest of the constraints in
// play (object names, label values, ConfigMap data keys), so requiring it once
// satisfies all three.
func validSegment(what, v string) error {
	if v == "" {
		return fmt.Errorf("serverstate: %s must not be empty", what)
	}
	if problems := validation.IsDNS1123Label(v); len(problems) > 0 {
		return fmt.Errorf("serverstate: %s %q is not a DNS-safe name: %s", what, v, strings.Join(problems, "; "))
	}
	return nil
}
