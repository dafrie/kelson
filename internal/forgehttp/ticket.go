package forgehttp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The one-time ticket that carries an authenticated caller across a top-level
// navigation (issue #248, ADR-0033 decision 2).
//
// # Why a ticket exists at all
//
// The manifest flow starts in the browser and ends on GitHub, which means the
// browser must *navigate* to /start — a full page load, because the response is
// a form that posts itself to github.com. A navigation is not a fetch: it
// carries no Authorization header, and the UI's credential is deliberately not
// reachable from a URL. So the credential is checked once, at
// [Handler.manifestSession], and what crosses the gap is this: a token that
// authorizes exactly one GET of /start and nothing else.
//
// # What makes it safe to put in a URL
//
// It is not a credential and cannot be turned into one. It names no user, opens
// no session, and grants exactly one action on one endpoint. It lives two
// minutes — long enough for a click, far shorter than the fifteen the flow
// itself gets — and it is refused the second time it is presented. A ticket
// that leaks through a Referer header, a shoulder, or a shared screenshot is
// therefore already spent or nearly expired, and even fresh it buys the holder
// only the right to be sent to GitHub's app-creation page.
//
// # Why the signing key dies with the process
//
// It is 32 bytes of crypto/rand made at startup and never written down, which
// is the same trade internal/api/auth.go takes for session tokens and for the
// same reason (ADR-0013 §1: this process holds no state a restart or a second
// replica would lose or fork). A restart invalidates every outstanding ticket
// and a second replica does not honour the first's — both cost a user one
// re-click of Connect GitHub, which is the honest price of holding no shared
// secret and no ticket table.
//
// The spent set below is the one exception, and it is bounded rather than
// stateless: single use cannot be proven without remembering what was used.
// Entries live at most [ticketTTL], so the set is bounded by the mint rate over
// two minutes and empties itself; losing it in a restart or forking it across
// replicas costs at most one extra use of a ticket that expires in two minutes,
// by the browser that just minted it.

// ticketTTL bounds a ticket. Two minutes is a click, not a session: the window
// between the UI receiving a start URL and the browser navigating to it.
const ticketTTL = 2 * time.Minute

// ticketParam is the query parameter /start reads its ticket from. It is the
// only token this package ever puts in a URL.
const ticketParam = "ticket"

// verdict is what [tickets.spend] answers, split three ways because the caller
// owes an honest status code: a ticket that never was is not the same as one
// that was and is finished with.
type verdict int

const (
	// ticketBad is missing, malformed, or not signed by this process.
	ticketBad verdict = iota
	// ticketDead was validly signed and is expired or already spent.
	ticketDead
	// ticketOK was valid and is now spent.
	ticketOK
)

// tickets mints and spends the one-time tokens /start requires.
type tickets struct {
	key []byte

	mu sync.Mutex
	// spent maps a nonce to its expiry, so the same ticket cannot be presented
	// twice. Nothing here is a credential: a nonce is 16 random bytes that name
	// nobody.
	spent map[string]int64
	// sweepAt is the size the set must reach before the next expiry sweep.
	// Sweeping on every insert would be O(n) per request; sweeping when the set
	// has doubled amortizes it to O(1) while keeping the bound.
	sweepAt int
}

// newTickets makes the per-process ticket authority.
func newTickets() (*tickets, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &tickets{key: key, spent: map[string]int64{}, sweepAt: 64}, nil
}

// mint returns a ticket valid for [ticketTTL] from now.
//
// The shape is internal/api/auth.go's session token, for the same reason it is
// used there: base64url(payload) "." base64url(HMAC(payload)), so verification
// needs nothing the server had to remember. The payload is an expiry and a
// nonce, and the nonce exists only to make each ticket distinguishable from
// every other — it is what the spent set keys on.
func (t *tickets) mint(now time.Time) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		strconv.FormatInt(now.Add(ticketTTL).Unix(), 10) + ":" + base64.RawURLEncoding.EncodeToString(nonce)))
	return payload + "." + base64.RawURLEncoding.EncodeToString(t.sign(payload)), nil
}

func (t *tickets) sign(payload string) []byte {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte(payload)) //nolint:errcheck // hash.Hash never returns an error
	return mac.Sum(nil)
}

// spend verifies a ticket and consumes it. A ticket is spent whatever the
// caller does next: a flow that fails after this point is restarted from the
// UI, which is one fetch, and the alternative — a ticket returned to the pool
// on error — is a ticket that can be retried by whoever holds the URL.
//
// The signature is checked before the claims are read, because an unsigned
// token's expiry is not evidence of anything.
func (t *tickets) spend(raw string, now time.Time) verdict {
	payload, signature, found := strings.Cut(strings.TrimSpace(raw), ".")
	if !found {
		return ticketBad
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(t.sign(payload), got) {
		return ticketBad
	}
	claims, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ticketBad
	}
	expiry, nonce, found := strings.Cut(string(claims), ":")
	if !found || nonce == "" {
		return ticketBad
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		return ticketBad
	}
	if now.After(time.Unix(unix, 0)) {
		return ticketDead
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if _, used := t.spent[nonce]; used {
		return ticketDead
	}
	t.sweep(now)
	t.spent[nonce] = unix
	return ticketOK
}

// sweep drops expired nonces. A nonce past its expiry is refused by the expiry
// check above whether or not it is still remembered, so forgetting it changes
// no answer — it only stops the set from growing without bound.
//
// The caller holds the lock.
func (t *tickets) sweep(now time.Time) {
	if len(t.spent) < t.sweepAt {
		return
	}
	for nonce, expiry := range t.spent {
		if now.After(time.Unix(expiry, 0)) {
			delete(t.spent, nonce)
		}
	}
	t.sweepAt = 2*len(t.spent) + 64
}
