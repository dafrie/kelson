package forgehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/controlstore"
)

// The webhook endpoint (ADR-0034 decisions 1 and 2). Every payload here is
// signed with the same HMAC internal/forge verifies, so a fixture that passes
// here would pass against a real delivery — and one that fails would fail
// against one.

const pullRequestPayload = `{
  "action": "synchronize",
  "number": 412,
  "pull_request": {"number": 412, "head": {"sha": "9f1c0de5b2a1c3d4e5f60718293a4b5c6d7e8f90"}},
  "repository": {"full_name": "acme/checkout", "html_url": "https://github.com/acme/checkout"},
  "installation": {"id": 678910}
}`

const pushPayload = `{
  "ref": "refs/heads/main",
  "after": "9f1c0de5b2a1c3d4e5f60718293a4b5c6d7e8f90",
  "repository": {"full_name": "acme/checkout", "html_url": "https://github.com/acme/checkout"},
  "installation": {"id": 678910}
}`

const installationPayload = `{
  "action": "created",
  "installation": {"id": 678910},
  "repository": {"full_name": "acme/checkout", "html_url": "https://github.com/acme/checkout"}
}`

const pingPayload = `{"zen": "Non-blocking is better than blocking.", "repository": {"full_name": "acme/checkout"}}`

// previewing is one instance: one github app connection, one project whose
// staging environment previews the repository the deliveries are about.
func previewing(t *testing.T, poker *fakePoker, conns *fakeConnections) *Handler {
	t.Helper()
	acme, acmeMat := appConnection("acme-github", "", appSecret)
	return handlerWith(t, Options{
		Sources:     resolverOver(storeWith(pair(acme, acmeMat))),
		Connections: conns,
		Specs: &fakeSpecs{stored: []controlstore.Stored{
			projectWithPreviews("checkout", "staging", "https://github.com/acme/checkout"),
		}},
		Previews: poker,
	})
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v (%s)", err, rec.Body.String())
	}
	return body
}

// A verified pull_request delivery pokes the ResourceSetInputProvider of every
// environment previewing that repository, and does nothing else. The object's
// address is derived from the same functions the renderer writes it with.
func TestPullRequestPokesTheInputProvider(t *testing.T) {
	poker := &fakePoker{}
	h := previewing(t, poker, newFakeConnections())

	rec := serve(h, delivery(t, "pull_request", appSecret, []byte(pullRequestPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	want := []string{"checkout-staging/checkout-staging-previews"}
	if got := poker.seen(); !reflect.DeepEqual(got, want) {
		t.Errorf("poked %v, want %v", got, want)
	}
}

// A repository nothing previews verifies and then does nothing — which is a
// 200, because the delivery was fine and there was simply nothing to reconcile.
func TestPullRequestForAnUnwatchedRepositoryPokesNothing(t *testing.T) {
	acme, acmeMat := appConnection("acme-github", "", appSecret)
	poker := &fakePoker{}
	h := handlerWith(t, Options{
		Sources: resolverOver(storeWith(pair(acme, acmeMat))),
		Specs: &fakeSpecs{stored: []controlstore.Stored{
			projectWithPreviews("other", "staging", "https://github.com/acme/something-else"),
		}},
		Previews: poker,
	})

	rec := serve(h, delivery(t, "pull_request", appSecret, []byte(pullRequestPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := poker.seen(); len(got) != 0 {
		t.Errorf("poked %v for a repository no environment previews", got)
	}
}

// The host has to agree too: `acme/checkout` on gitlab.com is not the
// repository a GitHub delivery is about, and anyone mirroring has both.
func TestPreviewsOnAnotherForgeAreNotPoked(t *testing.T) {
	acme, acmeMat := appConnection("acme-github", "", appSecret)
	poker := &fakePoker{}
	h := handlerWith(t, Options{
		Sources: resolverOver(storeWith(pair(acme, acmeMat))),
		Specs: &fakeSpecs{stored: []controlstore.Stored{
			projectWithPreviews("checkout", "staging", "https://gitlab.com/acme/checkout"),
		}},
		Previews: poker,
	})

	serve(h, delivery(t, "pull_request", appSecret, []byte(pullRequestPayload)))
	if got := poker.seen(); len(got) != 0 {
		t.Errorf("poked %v for a same-named repository on another forge", got)
	}
}

// A poke that fails is reported and never fatal: the poll interval is the
// backstop, and a 500 would only make GitHub redeliver something already
// handled as well as it can be.
func TestAFailedPokeIsReportedAndNotFatal(t *testing.T) {
	acme, acmeMat := appConnection("acme-github", "", appSecret)
	poker := &fakePoker{failed: map[string]error{
		"checkout-staging/checkout-staging-previews": http.ErrNotSupported,
	}}
	h := handlerWith(t, Options{
		Sources: resolverOver(storeWith(pair(acme, acmeMat))),
		Specs: &fakeSpecs{stored: []controlstore.Stored{
			projectWithPreviews("checkout", "staging", "https://github.com/acme/checkout"),
		}},
		Previews: poker,
	})

	rec := serve(h, delivery(t, "pull_request", appSecret, []byte(pullRequestPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a lost poke costs latency, not correctness", rec.Code)
	}
	if failed, _ := decodeBody(t, rec)["failed"].([]any); len(failed) != 1 {
		t.Errorf("the response must name what could not be poked: %s", rec.Body.String())
	}
}

// The multi-app case: two apps, two secrets, and the delivery is attributed to
// whichever secret actually signed it.
func TestTheConnectionThatVerifiesIsTheOneThatActs(t *testing.T) {
	acme, acmeMat := appConnection("acme-github", "", appSecret)
	globex, globexMat := appConnection("globex-github", "", otherSecret)
	conns := newFakeConnections()
	h := handlerWith(t, Options{
		Sources:     resolverOver(storeWith(pair(acme, acmeMat), pair(globex, globexMat))),
		Connections: conns,
	})

	rec := serve(h, delivery(t, "installation", otherSecret, []byte(installationPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := conns.installations["globex-github"]; got != 678910 {
		t.Errorf("installations = %v, want the installation recorded against the connection whose secret signed it",
			conns.installations)
	}
	if _, wrong := conns.installations["acme-github"]; wrong {
		t.Error("the delivery was attributed to a connection whose secret did not sign it")
	}
}

// An uninstall zeroes the ID back: an app that has been uninstalled cannot
// mint, and a connection claiming it can fails at clone time instead of at its
// Ready condition.
func TestUninstallClearsTheInstallation(t *testing.T) {
	conns := newFakeConnections()
	h := previewing(t, &fakePoker{}, conns)

	body := strings.Replace(installationPayload, `"action": "created"`, `"action": "deleted"`, 1)
	rec := serve(h, delivery(t, "installation", appSecret, []byte(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got, ok := conns.installations["acme-github"]; !ok || got != 0 {
		t.Errorf("installation = %v (recorded=%v), want it cleared", got, ok)
	}
}

// A push is a deliberate no-op until autoDeploy exists (ADR-0034 decision 4),
// and the response says so rather than pretending something happened.
func TestPushIsANoOpAndSaysSo(t *testing.T) {
	poker := &fakePoker{}
	h := previewing(t, poker, newFakeConnections())

	rec := serve(h, delivery(t, "push", appSecret, []byte(pushPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if acted, _ := body["acted"].(bool); acted {
		t.Error("a push acted on something; autoDeploy is not implemented")
	}
	if reason, _ := body["reason"].(string); !strings.Contains(reason, "autoDeploy") {
		t.Errorf("the response must say why nothing happened: %v", body)
	}
	if got := poker.seen(); len(got) != 0 {
		t.Errorf("a push poked %v", got)
	}
}

func TestPingIsAnswered(t *testing.T) {
	h := previewing(t, &fakePoker{}, newFakeConnections())
	rec := serve(h, delivery(t, "ping", appSecret, []byte(pingPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an unanswered ping reads as a broken webhook URL", rec.Code)
	}
}

// The refusals. An unverifiable delivery changes nothing, and the answer says
// nothing about which connections this instance holds.
func TestUnverifiableDeliveriesAreRefusedWithNoStateChange(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*http.Request)
		wantErr string
	}{
		{"wrong secret", func(r *http.Request) {
			r.Header.Set("X-Hub-Signature-256", sign("not-the-secret", []byte(pullRequestPayload)))
		}, "did not verify"},
		{"no signature", func(r *http.Request) {
			r.Header.Del("X-Hub-Signature-256")
		}, "did not verify"},
		{"signature not hex", func(r *http.Request) {
			r.Header.Set("X-Hub-Signature-256", "sha256=zzzz")
		}, "did not verify"},
		{"sha1 header is not accepted", func(r *http.Request) {
			r.Header.Del("X-Hub-Signature-256")
			r.Header.Set("X-Hub-Signature", "sha1=whatever")
		}, "did not verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poker := &fakePoker{}
			conns := newFakeConnections()
			h := previewing(t, poker, conns)

			req := delivery(t, "pull_request", appSecret, []byte(pullRequestPayload))
			tc.mutate(req)
			rec := serve(h, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if got, _ := decodeBody(t, rec)["error"].(string); !strings.Contains(got, tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", got, tc.wantErr)
			}
			if len(poker.seen()) != 0 || len(conns.installations) != 0 {
				t.Error("an unverifiable delivery changed state")
			}
			// The answer must not tell an unauthenticated caller which
			// connections exist.
			if strings.Contains(rec.Body.String(), "acme-github") {
				t.Errorf("the refusal named a connection: %s", rec.Body.String())
			}
		})
	}
}

// An instance holding no github connection verifies nothing, which is the same
// 401 rather than a different code that would advertise the fact.
func TestNoConnectionsMeansNoDeliveryVerifies(t *testing.T) {
	h := handlerWith(t, Options{Sources: resolverOver(storeWith())})
	rec := serve(h, delivery(t, "pull_request", appSecret, []byte(pullRequestPayload)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A verified delivery whose body is not JSON is a 400 and changes nothing: the
// signature proves the sender, not the shape.
func TestVerifiedButUnparseableIsABadRequest(t *testing.T) {
	poker := &fakePoker{}
	h := previewing(t, poker, newFakeConnections())

	rec := serve(h, delivery(t, "pull_request", appSecret, []byte("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(poker.seen()) != 0 {
		t.Error("an unparseable delivery poked something")
	}
}

// An event kind this seam does not map is accepted and skipped. A listener that
// logged the forge working correctly as a fault would be noise in every
// delivery log.
func TestUnmappedEventKindsAreAccepted(t *testing.T) {
	h := previewing(t, &fakePoker{}, newFakeConnections())
	rec := serve(h, delivery(t, "check_suite", appSecret, []byte(`{"action":"requested"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

// The route is POST-only: a GET is the mux's 405, not a handler's problem.
func TestWebhookRefusesOtherMethods(t *testing.T) {
	h := previewing(t, &fakePoker{}, newFakeConnections())
	rec := serve(h, httptest.NewRequest(http.MethodGet, WebhookPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET %s = %d, want 405", WebhookPath, rec.Code)
	}
}

// Failing to record an installation is the one delivery whose loss is more than
// latency, so it asks GitHub to redeliver.
func TestAFailedInstallationRecordAsksForARedelivery(t *testing.T) {
	conns := newFakeConnections()
	conns.recordErr = http.ErrNotSupported
	h := previewing(t, &fakePoker{}, conns)

	rec := serve(h, delivery(t, "installation", appSecret, []byte(installationPayload)))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 so the forge retries", rec.Code)
	}
}
