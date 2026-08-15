package forge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The [WebhookSource] half. Two properties carry the whole of it: nothing is
// parsed before it is verified (the caller's job, in that order), and nothing
// parsed is trusted as state — an [Event] names what to reconcile and the
// reconcile re-reads the truth (ADR-0034 decision 1).

const (
	// signatureHeader is GitHub's HMAC-SHA256 header. The SHA-1 header of the
	// same shape (X-Hub-Signature) is deliberately not accepted: it is
	// deprecated, it is weaker, and honouring it would let a sender choose the
	// algorithm their forgery is checked against.
	signatureHeader = "X-Hub-Signature-256"
	// eventHeader carries the event kind; the body does not.
	eventHeader = "X-GitHub-Event"
	// signaturePrefix is the algorithm label GitHub prefixes the hex digest with.
	signaturePrefix = "sha256="
)

// VerifySignature implements [WebhookSource].
//
// Every failure is the same failure — [ErrBadSignature] — including a
// connection with no webhook secret at all. That is fail-closed on purpose: a
// listener that processes deliveries it cannot verify has no signature check,
// and "the secret is missing" is a configuration problem the connection's
// Ready condition reports, not a reason to accept an unauthenticated POST to a
// public endpoint (ADR-0033's "new public attack surface").
func (g *gitHubProvider) VerifySignature(c Conn, header http.Header, body []byte) error {
	if len(c.WebhookSecret) == 0 {
		return fmt.Errorf("forge/github: %s has no webhook secret, so no delivery can be verified: %w", c, ErrBadSignature)
	}
	presented := strings.TrimSpace(header.Get(signatureHeader))
	if presented == "" {
		return fmt.Errorf("forge/github: the delivery carries no %s header: %w", signatureHeader, ErrBadSignature)
	}
	if !strings.HasPrefix(presented, signaturePrefix) {
		return fmt.Errorf("forge/github: the delivery's signature is not %s: %w", signaturePrefix, ErrBadSignature)
	}
	sum, err := hex.DecodeString(strings.TrimPrefix(presented, signaturePrefix))
	if err != nil {
		return fmt.Errorf("forge/github: the delivery's signature is not hex: %w", ErrBadSignature)
	}

	mac := hmac.New(sha256.New, c.WebhookSecret)
	mac.Write(body)
	// hmac.Equal rather than bytes.Equal or a string compare: the comparison
	// is against an attacker-supplied value and must not leak how far it got.
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return fmt.Errorf("forge/github: the delivery's signature does not match: %w", ErrBadSignature)
	}
	return nil
}

// webhookPayload is the union of the fields the four mapped events carry.
// Decoding one shape rather than four keeps the mapping below flat: GitHub's
// payloads nest the same objects under the same names, and what differs
// between events is which of them are present.
type webhookPayload struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Number     int    `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// ParseEvent implements [WebhookSource].
//
// An event kind this seam does not map returns [ErrIgnoredEvent] and nothing
// else, so a caller skips it silently: an installation is subscribed to what
// ADR-0033 decision 2's manifest asks for, GitHub sends more than that (`ping`
// on creation, `installation` on lifecycle), and a listener that logged every
// unmapped delivery as an error would report the forge working correctly as a
// fault.
func (g *gitHubProvider) ParseEvent(header http.Header, body []byte) (Event, error) {
	kind := strings.TrimSpace(header.Get(eventHeader))
	if kind == "" {
		return Event{}, fmt.Errorf("forge/github: the delivery carries no %s header: %w", eventHeader, ErrIgnoredEvent)
	}
	switch kind {
	case "ping", "push", "pull_request", "installation":
	default:
		return Event{}, fmt.Errorf("forge/github: %q: %w", kind, ErrIgnoredEvent)
	}

	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("forge/github: decoding a %s delivery: %w", kind, err)
	}

	e := Event{
		Kind:           kind,
		RepoFullName:   p.Repository.FullName,
		RepoHTMLURL:    p.Repository.HTMLURL,
		Action:         p.Action,
		InstallationID: p.Installation.ID,
	}
	switch kind {
	case "push":
		// `after` is the commit the ref now points at, which is what a build
		// and a status are about. `head_commit` is null for a branch deletion
		// and would make the two disagree.
		e.Ref, e.HeadSHA = p.Ref, p.After
	case "pull_request":
		e.HeadSHA = p.PullRequest.Head.SHA
		e.PRNumber = p.Number
		if e.PRNumber == 0 {
			// Top-level `number` is the documented field; the nested one is
			// the fallback for payloads that omit it.
			e.PRNumber = p.PullRequest.Number
		}
	}
	return e, nil
}
