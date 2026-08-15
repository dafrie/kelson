package forgehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The gate on the manifest flow (issue #248, ADR-0033 decision 2). These tests
// are about the ticket alone; that the *server* wires the RPC credential check
// into it is asserted through the real mux in cmd/kelson-server.

func gatedHandler(t *testing.T, now func() int64) *Handler {
	t.Helper()
	return handlerWith(t, Options{
		Sources:     resolverOver(storeWith()),
		Connections: newFakeConnections(),
		Secrets:     newFakeSecrets(),
		ExternalURL: "https://kelson.acme.com",
		Now:         now,
	})
}

// The session endpoint is the authenticated door: a caller the server refuses
// gets no ticket, and gets told why in the words internal/api chose.
func TestManifestSessionRefusesAnUnauthenticatedCaller(t *testing.T) {
	h := gatedHandler(t, nil)

	const refusal = "kelson-server requires a credential: log in at /auth/login"
	rec := serveGated(h, httptest.NewRequest(http.MethodPost, ManifestSessionPath, nil),
		func(*http.Request) string { return refusal })

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}
	if body["message"] != refusal {
		t.Errorf("message = %q, want the check's own refusal passed through", body["message"])
	}
	if body["startUrl"] != "" {
		t.Error("a refused session must not carry a start URL")
	}
}

// A Handler registered without a check is a wiring bug, and fails closed rather
// than defaulting to open — the hole this endpoint exists to close.
func TestManifestSessionFailsClosedWithoutACheck(t *testing.T) {
	h := gatedHandler(t, nil)

	rec := serveGated(h, httptest.NewRequest(http.MethodPost, ManifestSessionPath, nil), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "startUrl") {
		t.Error("a misconfigured server must not mint a ticket")
	}
}

// The response shape the UI is written against: one relative start URL, and the
// ticket is the only token in it.
func TestManifestSessionAnswersAStartURL(t *testing.T) {
	h := gatedHandler(t, nil)

	target := startURL(t, h)
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parsing startUrl %q: %v", target, err)
	}
	if u.IsAbs() || u.Path != ManifestStartPath {
		t.Errorf("startUrl = %q, want a relative %s", target, ManifestStartPath)
	}
	if q := u.Query(); len(q) != 1 || q.Get(ticketParam) == "" {
		t.Errorf("startUrl query = %v, want the ticket and nothing else", u.RawQuery)
	}
}

// Single use is the property that makes a ticket safe in a URL: the second
// visit is refused, and refused as Gone because re-presenting it will never
// work.
func TestManifestTicketWorksOnceAndOnce(t *testing.T) {
	h := gatedHandler(t, nil)
	target := startURL(t, h)

	if rec := serve(h, httptest.NewRequest(http.MethodGet, target, nil)); rec.Code != http.StatusOK {
		t.Fatalf("first use = %d, want the manifest form: %s", rec.Code, rec.Body.String())
	}
	rec := serve(h, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("second use = %d, want 410: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Connect GitHub") {
		t.Errorf("the refusal must say how to start again: %s", rec.Body.String())
	}
}

// movableClock is what an expiry test needs from [Options.Now]: a source read
// on every call, so advancing the field moves time. A bound method value on a
// time.Time (clock.Unix) would freeze the reading at the moment it was bound,
// which is the opposite of the point.
type movableClock struct{ at time.Time }

func (c *movableClock) unix() int64 { return c.at.Unix() }

// Two minutes is a click, not a session. A ticket that outlived it is dead
// even though it was never used.
func TestManifestTicketExpires(t *testing.T) {
	clock := &movableClock{at: time.Now()}
	h := gatedHandler(t, clock.unix)

	target := startURL(t, h)
	clock.at = clock.at.Add(ticketTTL + time.Second)

	rec := serve(h, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("an expired ticket = %d, want 410: %s", rec.Code, rec.Body.String())
	}
}

// Everything that is not a live ticket this process signed is a 401: no ticket
// at all, a mangled one, and one signed by another process — which is what a
// ticket minted before a restart is.
func TestManifestStartRefusesWithoutALiveTicket(t *testing.T) {
	h := gatedHandler(t, nil)
	foreign := gatedHandler(t, nil)

	stale := queryOf(t, startURL(t, foreign)).Get(ticketParam)
	live := queryOf(t, startURL(t, h)).Get(ticketParam)

	for _, tc := range []struct{ name, query string }{
		{"no ticket", ""},
		{"empty ticket", "?" + ticketParam + "="},
		{"not a ticket", "?" + ticketParam + "=hello"},
		{"unsigned", "?" + ticketParam + "=" + url.QueryEscape(strings.SplitN(live, ".", 2)[0]+".")},
		{"another process's key", "?" + ticketParam + "=" + url.QueryEscape(stale)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, httptest.NewRequest(http.MethodGet, ManifestStartPath+tc.query, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "settings/apps/new") {
				t.Error("a refused start must not render the manifest form")
			}
		})
	}

	// …and the live ticket is still live: a refused attempt spends nothing.
	if rec := serve(h, httptest.NewRequest(http.MethodGet, ManifestStartPath+"?"+ticketParam+"="+url.QueryEscape(live), nil)); rec.Code != http.StatusOK {
		t.Errorf("the untouched ticket = %d, want it still good", rec.Code)
	}
}

// The refusal must not become a redirect to /connections the way the flow's own
// failures do: a browser that followed one would lose the status code, and the
// UI cannot tell "not signed in" from "cancelled on GitHub" without it.
func TestManifestStartRefusalKeepsItsStatus(t *testing.T) {
	h := gatedHandler(t, nil)

	rec := serve(h, httptest.NewRequest(http.MethodGet, ManifestStartPath, nil))
	if location := rec.Header().Get("Location"); location != "" {
		t.Errorf("Location = %q, want no redirect on an unauthenticated start", location)
	}
}
