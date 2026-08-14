// Package controlstore is kelson-server's cluster-backed state plane: the
// project specs clients converge on ([SpecStore]), the agent identities that
// may act ([AgentStore], ADR-0024) and the bounded audit ring of what they did
// ([AuditStore], ADR-0026).
//
// # Why this package is not called serverstate any more
//
// It was, and it also held the ConfigMap spec store and the ConfigMap
// rendered-history store. [ADR-0027](docs/adr/0027-crd-native-control-plane.md)
// deleted the history store outright — history is the registry's tag list now
// (ADR-0028 decision 4) — and re-backed the spec store with custom resources.
// What is left is *control-plane records*: who a principal is, what a principal
// did, and the documents a principal authored. None of it is a memory of a
// deployment, and the old name said it was (ADR-0027 decision 7).
//
// # All server state is cluster state
//
// The kelson-server process holds nothing a restart or a second replica would
// lose or fork. Specs are `kelson.dev/v1alpha1` custom resources; identities and
// the audit ring are Secrets and ConfigMaps in one namespace, carrying kelson's
// provenance labels, which makes `kubectl get configmaps -l
// kelson.dev/state=audit` the trail and namespaced RBAC the access control.
//
// Whether the identity and audit records should *also* become custom resources
// is a real question with a weaker case — an audit ring wants a fixed-size
// buffer more than it wants a typed API — and it is a tracked follow-up rather
// than part of the CRD swap (ADR-0027 decision 7).
//
// # Optimistic concurrency is Kubernetes resourceVersion
//
// Spec writes carry the stored object's resourceVersion through as an opaque
// version string. kelson does not reimplement what the API server already
// guarantees; a stale write comes back as a store/version-conflict, and it did
// so identically when the store was a ConfigMap (ADR-0027 decision 6: the store
// vocabulary is unchanged, only what it is a vocabulary *about* changed).
//
// # Idempotency is honest about its scope
//
// Mutating calls record their idempotency key as an annotation on the object
// they wrote, and a replayed key returns the recorded outcome. There is no
// distributed dedup beyond what one namespace's objects provide — two servers
// pointed at different namespaces do not share keys.
package controlstore

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

	// labelState separates the kinds of state object living in the same
	// namespace, so listing one never has to filter the others out by name
	// prefix.
	labelState = "kelson.dev/state"

	// stateSpec marks the custom resources the spec store owns. The Project and
	// Environment CRDs make the kind itself queryable, so the label is
	// provenance rather than a selector this package needs — it is what tells a
	// reader of `kubectl get environments -A --show-labels` which objects
	// kelson-server wrote.
	stateSpec = "spec"

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

// writeAttempts bounds the read-modify-write retry a forced write performs when
// something else changed the object between the read and the update.
const writeAttempts = 3

// validSegment keeps a caller-supplied identifier safe to splice into an object
// name and a label value. The risk it guards is building a name the API server
// rejects — or worse, one that collides with another project's. A DNS-1123
// label is the strictest of the constraints in play (object names, label
// values, ConfigMap data keys), so requiring it once satisfies all three.
func validSegment(what, v string) error {
	if v == "" {
		return fmt.Errorf("controlstore: %s must not be empty", what)
	}
	if problems := validation.IsDNS1123Label(v); len(problems) > 0 {
		return fmt.Errorf("controlstore: %s %q is not a DNS-safe name: %s", what, v, strings.Join(problems, "; "))
	}
	return nil
}
