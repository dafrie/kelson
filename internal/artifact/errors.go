package artifact

import "fmt"

// Reason codes for the publisher's own refusals, as opposed to a failure the
// registry reported. They mirror the three-field shape every kelson plane that
// refuses before it acts uses (build.Error, renderer.Error, preview.Error), and
// it is what lets a caller print a remediation without knowing which refusal it
// is holding.
//
// They are the artifact plane's own codes rather than the preview plane's,
// because this package now has two callers (ADR-0028 decision 2) and a code
// naming `previews.artifacts.repository` would be wrong for the spine's
// `--registry`. Each caller adds the sentence that names its own knob.
const (
	// ReasonNoRepository: nothing said where to publish. Publishing to nowhere
	// silently would be worse than a refusal nobody hits.
	ReasonNoRepository = "artifact/no-repository"
	// ReasonRepositoryInvalid: the repository is not one this publisher can
	// push to — a tag or digest on it, or a name the registry grammar rejects.
	ReasonRepositoryInvalid = "artifact/repository-invalid"
	// ReasonTagListTooLarge: the repository holds more tags than one listing
	// walks. It is a refusal rather than a truncation because a tag list is
	// lexically ordered, so a partial one is an arbitrary subset (tags.go).
	ReasonTagListTooLarge = "artifact/tag-list-too-large"
	// ReasonTagListUnreadable: something answered /v2/<name>/tags/list with a
	// body that is not a tag list. A proxy, a login page, a 200 from an
	// unrelated service — never a registry doing its job.
	ReasonTagListUnreadable = "artifact/tag-list-unreadable"
)

// Error is a publisher refusal: a named reason, what happened, and what to do.
type Error struct {
	Reason      string
	Message     string
	Remediation string
}

func (e Error) Error() string {
	return fmt.Sprintf("[%s] %s: %s", e.Reason, e.Message, e.Remediation)
}

// DeniedError is the registry answering, and saying no: an HTTP status it chose
// and, where it sent one, its own words.
//
// It is a type and not a formatted string because the two ways a push fails
// need two different reactions, and a caller must not have to grep a message to
// tell them apart. A refused push is a credential or a permission — an
// operator's problem, on human time — and retrying it into a rate limit helps
// nobody. That distinction is what internal/controller's error taxonomy turns
// into PushDenied (wait on a timer) versus RegistryUnreachable (exponential
// backoff), and it is only honest if this package reports which happened.
//
// The registry's own body is quoted because it is the only party that knows why
// it said no. It is bounded, and no request header ever reaches it.
type DeniedError struct {
	// StatusCode is the HTTP status the registry answered with; 401 and 403 are
	// the ones that mean "not you", the rest mean "not this".
	StatusCode int
	// Status is the status line, e.g. "403 Forbidden".
	Status string
	// Doing names the step, e.g. "uploading a blob to ghcr.io/acme/x".
	Doing string
	// Detail is the registry's own error body, trimmed and bounded, or empty.
	Detail string
}

func (e *DeniedError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("artifact: %s failed: %s", e.Doing, e.Status)
	}
	return fmt.Sprintf("artifact: %s failed: %s: %s", e.Doing, e.Status, e.Detail)
}

// Unauthorized reports whether the registry refused the *caller* rather than
// the request: a missing, wrong or insufficiently scoped credential.
func (e *DeniedError) Unauthorized() bool {
	return e.StatusCode == 401 || e.StatusCode == 403
}

// UnreachableError is the other half: the registry never answered. A DNS
// failure, a refused connection, a TLS handshake, a timeout — every one of them
// transient by assumption, and every one of them fixed by trying again later.
type UnreachableError struct {
	// Doing names the step, with any query string already stripped: a registry
	// hands back upload locations carrying signed tokens, and an error that
	// quoted one would put a credential-equivalent in a log.
	Doing string
	Err   error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("artifact: %s: %s", e.Doing, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }
