package forge

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dafrie/kelson/internal/redact"
)

// The four failures a caller must be able to branch on without reading a
// message. Everything else this package returns is descriptive text: these are
// the ones where the caller's next action differs.
var (
	// ErrBadSignature: a webhook delivery did not verify against the
	// connection's secret, or could not be verified at all. Both are the same
	// answer — refuse the delivery — because a listener that processes what it
	// cannot verify is a listener with no signature check
	// ([ADR-0034](docs/adr/0034-forge-driven-delivery.md): HMAC gates every
	// event).
	ErrBadSignature = errors.New("forge: webhook signature does not verify")

	// ErrIgnoredEvent: a delivery this seam does not map. Forges send many
	// event kinds and an installation subscribes to two of them; the rest are
	// skipped silently, so this is a sentinel rather than a log line — the
	// caller answers 200 and moves on.
	ErrIgnoredEvent = errors.New("forge: event kind carries nothing this seam acts on")

	// ErrAuthFailed: the connection's credential was rejected, is missing, or
	// is not the shape the operation needs. It is the connection's Ready
	// condition going false, and the user's action is to reconnect or rotate.
	ErrAuthFailed = errors.New("forge: the connection's credential was rejected")

	// ErrNotInstalled: the credential is good but the app is not installed
	// where it was asked to act, or the installation no longer covers the
	// repository. Distinct from ErrAuthFailed because the remedy is different:
	// nothing is wrong with the key, the user has to add the repository to the
	// installation (ADR-0033 decision 2 step 3).
	ErrNotInstalled = errors.New("forge: the app is not installed for that account or repository")

	// ErrWriteNotPermitted: the credential authenticated, the installation
	// covers the repository, and the forge refused the *write* for lack of a
	// permission. It is the expected answer for [PRProposer] on a connection
	// created by the app-manifest flow, because that manifest asks for
	// `contents: read` and opening a pull request writes objects
	// (githubmanifest.go).
	//
	// It is a fifth sentinel rather than a shade of ErrAuthFailed because the
	// two send a user to different screens and only one of them is alarming:
	// ErrAuthFailed means rotate a credential, and this means grant a
	// permission on an app that is otherwise working perfectly. A caller that
	// reported "your credential was rejected" here would send somebody to
	// re-do the whole connect ceremony for a checkbox.
	ErrWriteNotPermitted = errors.New("forge: the connection may read this repository and not write to it")
)

// maxErrorBody is how much of a forge's error response is quoted back. Enough
// for GitHub's {"message": ...}, short enough that a misconfigured endpoint
// answering with an HTML page does not paste it into an event.
const maxErrorBody = 512

// httpError is a non-2xx answer from a forge API, carrying the status so a
// caller that knows what a 404 means at *that* endpoint can re-label it.
//
// The quoted body is scrubbed through internal/redact: a forge that echoes a
// request header back in an error message must not turn kelson's own error
// path into the leak the redaction rule exists to prevent.
type httpError struct {
	Method string
	URL    string
	Status int
	Body   string

	// sentinel is what errors.Is matches, when the status maps to one.
	sentinel error
}

func (e *httpError) Error() string {
	status := http.StatusText(e.Status)
	if status == "" {
		status = fmt.Sprintf("status %d", e.Status)
	}
	msg := fmt.Sprintf("%s %s: %s", e.Method, e.URL, status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	if e.sentinel != nil {
		return fmt.Sprintf("%s (%s)", msg, e.sentinel)
	}
	return msg
}

func (e *httpError) Unwrap() error { return e.sentinel }

// newHTTPError maps the two statuses that mean the same thing at every
// endpoint. 404 is left unmapped on purpose: at /app/installations it means
// "not installed", at /repos/{full}/branches it means "no such repository",
// and one sentinel for both would send half the callers to the wrong remedy.
func newHTTPError(method, url string, status int, body []byte) *httpError {
	e := &httpError{Method: method, URL: url, Status: status, Body: errorBody(body)}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		e.sentinel = ErrAuthFailed
	}
	return e
}

func errorBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "…"
	}
	return redact.Scrub(strings.Join(strings.Fields(s), " "))
}

// isStatus reports whether err came back with a given HTTP status, so an
// endpoint-specific meaning can be attached where the endpoint is known.
func isStatus(err error, status int) bool {
	var he *httpError
	return errors.As(err, &he) && he.Status == status
}

// notInstalled re-labels an endpoint-specific 404. Two %w verbs so the result
// matches both ErrNotInstalled and the underlying httpError.
func notInstalled(err error) error {
	if isStatus(err, http.StatusNotFound) {
		return fmt.Errorf("%w: %w", ErrNotInstalled, err)
	}
	return err
}

// writeNotPermitted re-labels a 403 on a write endpoint, the same way
// [notInstalled] re-labels a 404 on a read one and for the same reason: the
// status means something narrower where the endpoint is known.
//
// [newHTTPError] maps 403 to ErrAuthFailed globally, and that stays true here —
// the wrap keeps the underlying error reachable, so a caller that only knows
// about the four original sentinels still reads this as an auth failure and
// fails closed. What the extra sentinel buys is the caller that knows better.
func writeNotPermitted(err error) error {
	if isStatus(err, http.StatusForbidden) {
		return fmt.Errorf("%w: %w", ErrWriteNotPermitted, err)
	}
	return err
}
