package secret

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, machine-actionable code in this package's own `secret/*`
// vocabulary. It is the same compatibility promise model.Code makes: an
// existing code never changes meaning, and every surface that reports one — the
// CLI, the API's structured errors, an MCP tool answer — passes it through
// verbatim rather than inventing a second taxonomy (ADR-0013 §2).
type Code string

const (
	// ErrInvalidTarget is a (project, environment) pair that cannot address a
	// namespace. Both halves are required: the namespace is derived from them.
	ErrInvalidTarget Code = "secret/invalid-target"
	// ErrInvalidName is a Secret name the API server would refuse, or one a
	// `{secret: <name>, key: <key>}` reference could not name.
	ErrInvalidName Code = "secret/invalid-name"
	// ErrInvalidKey is a key outside Kubernetes' Secret key alphabet. It is
	// refused here rather than at apply time so the message can name the key
	// instead of relaying a field-path error from the API server.
	ErrInvalidKey Code = "secret/invalid-key"
	// ErrNoValues is a Set with nothing to write. It is an error rather than a
	// no-op because the alternative is a command that reports success for a
	// mistyped `key=value` it silently dropped.
	ErrNoValues Code = "secret/no-values"
	// ErrNamespaceMissing is a target namespace that does not exist. Creating
	// it is the delivery plane's job (the renderer emits the Namespace and
	// `kelson deploy` applies it), so this package reports the gap rather than
	// filling it — a Secret in a namespace no environment targets is a secret
	// nothing will ever read.
	ErrNamespaceMissing Code = "secret/namespace-missing"
	// ErrNotManaged is a Secret that exists without kelson's managed-secret
	// label. kelson writes, lists and deletes only what it manages.
	ErrNotManaged Code = "secret/not-managed"
	// ErrNotFound is a delete of a Secret that is not there.
	ErrNotFound Code = "secret/not-found"
	// ErrWriteFailed is the API server refusing a write for a reason this
	// package has no more specific code for. The cause travels separately from
	// the message so a caller can log one and branch on the other.
	ErrWriteFailed Code = "secret/write-failed"
	// ErrReadFailed is the same for a read: a list or a read-back that the API
	// server would not answer.
	ErrReadFailed Code = "secret/read-failed"

	// The `sops` backend's own refusals (issue #81, ADR-0022). They are in this
	// package's vocabulary rather than the git plane's because what a user is
	// doing is setting a secret; that it reaches a repository rather than an
	// API server is the backend's business, not theirs.

	// ErrSOPSNoTarget is backend sops on an environment with no delivery
	// repository to write into. It is unreachable from a validated spec —
	// sops requires flux, which requires a git target — and exists so the
	// store cannot be constructed into a state where Set has nowhere to go.
	ErrSOPSNoTarget Code = "secret/sops-no-target"
	// ErrSOPSNoRecipients is backend sops with no age recipients. Same shape:
	// the model makes the field required, and this is the seam refusing to be
	// built without it rather than encrypting to nobody.
	ErrSOPSNoRecipients Code = "secret/sops-no-recipients"
	// ErrSOPSPartialSet is a `set` that would drop keys the encrypted Secret
	// already holds. Under this backend a set writes the whole file — kelson
	// would need the age identity to carry the other keys forward, and never
	// holds one — so a partial write is refused with the missing keys named
	// rather than done.
	ErrSOPSPartialSet Code = "secret/sops-partial-set"
	// ErrSOPSNotEncrypted is a file where an encrypted Secret belongs that is
	// not a SOPS document. Almost always a plaintext Secret committed by hand,
	// which is the failure this backend exists to prevent; kelson reports it
	// rather than overwriting it, because the remediation includes treating
	// its contents as compromised.
	ErrSOPSNotEncrypted Code = "secret/sops-not-encrypted"
	// ErrSOPSEncryptFailed is the encryption itself refusing — in practice a
	// recipient the spec's spelling check let through that age's parser did
	// not.
	ErrSOPSEncryptFailed Code = "secret/sops-encrypt-failed"
	// ErrSOPSWriteFailed is the delivery repository refusing a read, a commit
	// or a push. It is the git plane's failure relayed with its cause, and the
	// most common one is the compare-and-swap conflict that means the branch
	// moved while the command was running.
	ErrSOPSWriteFailed Code = "secret/sops-write-failed"
)

// DocsBaseURL mirrors model.DocsBaseURL so a `secret/*` code documents itself
// at the same shape of URL as a `schema/*` one.
const DocsBaseURL = "https://kelson.dev/model/errors"

// Error is one structured refusal from this package.
//
// It carries no field locator: a secret is not a document and there is no line
// to point at. What it does carry is Resource — `Secret/<namespace>/<name>` —
// because every one of these errors is about a specific object in a specific
// namespace, and "not managed by kelson" without saying which Secret in which
// namespace is unactionable.
type Error struct {
	Code        Code   `json:"code"`
	Resource    string `json:"resource"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	DocsURL     string `json:"docsUrl"`
	// Cause is the underlying failure, when there is one worth relaying (an
	// API server rejection). It is never a value: nothing in this package
	// formats a secret value into any of these fields, and internal/redact
	// would replace it if some future caller did.
	Cause string `json:"cause,omitempty"`
}

func (e Error) Error() string {
	if e.Resource == "" {
		return fmt.Sprintf("[%s] %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s [%s]: %s", e.Resource, e.Code, e.Message)
}

// newError builds an Error with its docs URL filled in, so no call site can
// produce a code that documents itself differently from the others.
func newError(code Code, resource, message, remediation string) Error {
	return Error{
		Code:        code,
		Resource:    resource,
		Message:     message,
		Remediation: remediation,
		DocsURL:     DocsBaseURL + "/" + strings.ReplaceAll(string(code), "/", "-"),
	}
}

// withCause returns a copy carrying an underlying failure.
func (e Error) withCause(err error) Error {
	if err != nil {
		e.Cause = err.Error()
	}
	return e
}

// resourceOf is the Resource string every error in this package uses.
func resourceOf(namespace, name string) string {
	switch {
	case namespace == "" && name == "":
		return ""
	case namespace == "":
		return "Secret/" + name
	default:
		return "Secret/" + namespace + "/" + name
	}
}

// AsCode reports whether err is (or wraps) an [Error] with the given code. It
// is the branch every caller outside this package should use: the API handler
// mapping a refusal onto a ConnectRPC code, and the CLI deciding whether a
// delete refusal deserves the adoption hint.
func AsCode(err error, code Code) bool {
	var se Error
	return errors.As(err, &se) && se.Code == code
}
