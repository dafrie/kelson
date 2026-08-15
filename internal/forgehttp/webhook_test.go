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

// tracking is one instance whose deliveries reach the autoDeploy trigger: the
// same app connection, the stored projects the test names, and a trigger seam
// that records instead of resolving.
func tracking(t *testing.T, poker *fakePoker, trigger AutoDeployer, stored ...controlstore.Stored) *Handler {
	t.Helper()
	acme, acmeMat := appConnection("acme-github", "", appSecret)
	opts := Options{
		Sources:     resolverOver(storeWith(pair(acme, acmeMat))),
		Connections: newFakeConnections(),
		Specs:       &fakeSpecs{stored: stored},
		Previews:    poker,
	}
	// A nil AutoDeployer must stay a nil *interface*, not an interface holding a
	// nil pointer: the handler branches on the first and would call through the
	// second.
	if trigger != nil {
		opts.AutoDeploy = trigger
	}
	return handlerWith(t, opts)
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

// A push to a repository no stored project builds from does nothing, says so,
// and never reaches the trigger. That is most pushes to most repositories, and
// ADR-0036 decision 2 makes the silence deliberate.
func TestPushNothingFollowsIsANoOp(t *testing.T) {
	poker := &fakePoker{}
	trigger := &fakeAutoDeploy{}
	h := tracking(t, poker, trigger, projectWithPreviews("checkout", "staging", "https://github.com/acme/checkout"))

	rec := serve(h, delivery(t, "push", appSecret, []byte(pushPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if acted, _ := decodeBody(t, rec)["acted"].(bool); acted {
		t.Error("a push acted on a project that declares no source in this repository")
	}
	if len(trigger.planned()) != 0 {
		t.Errorf("the trigger was asked about %v", trigger.planned())
	}
	if got := poker.seen(); len(got) != 0 {
		t.Errorf("a push poked %v", got)
	}
}

// The live path: a push to a repository a stored project builds from is
// resolved inside the delivery's own deadline, answered with what it enqueued,
// and the work runs behind it.
func TestPushEnqueuesTheTrigger(t *testing.T) {
	trigger := &fakeAutoDeploy{plan: PushPlan{Environments: map[string][]string{"staging": {"web"}}}}
	h := tracking(t, &fakePoker{}, trigger, projectFromSource("checkout", "https://github.com/acme/checkout"))

	rec := serve(h, delivery(t, "push", appSecret, []byte(pushPayload)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — the delivery is answered before the work runs", rec.Code)
	}
	body := decodeBody(t, rec)
	if acted, _ := body["acted"].(bool); !acted {
		t.Errorf("acted = false for a push that enqueued a trigger: %v", body)
	}

	h.inflight.Wait()
	ran := trigger.ran()
	if len(ran) != 1 {
		t.Fatalf("ran %d triggers, want one", len(ran))
	}
	got := ran[0]
	if got.Project != "checkout" || got.SHA != "9f1c0de5b2a1c3d4e5f60718293a4b5c6d7e8f90" {
		t.Errorf("trigger = %+v, want the project and the pushed head", got)
	}
	// The ref travels unreduced: exactly one piece of code decides what
	// `refs/heads/main` is, and it is on the other side of this seam beside the
	// model contract that consumes it.
	if got.Ref != "refs/heads/main" {
		t.Errorf("ref = %q, want the delivery's own spelling", got.Ref)
	}
	// The repository is spelled the way a spec spells one, with the host from
	// the connection rather than from the payload.
	if got.Repo != "https://github.com/acme/checkout" {
		t.Errorf("repo = %q, want a spec-shaped repository reference", got.Repo)
	}
	// And the connection is carried, because it is the audit trail's principal
	// for everything this sets in motion (ADR-0036 decision 4).
	if got.Connection != "acme-github" {
		t.Errorf("connection = %q, want the one whose secret verified the delivery", got.Connection)
	}
}

// A multi-source kelson-built project still refuses (#252). ADR-0036 decision 3
// wants that refusal on the environment's conditions; nothing in this process
// can write them, so it lands here — named, in full, in the delivery's own
// answer rather than vanishing into a 202.
func TestPushRefusalRidesTheResponse(t *testing.T) {
	refusal := "project checkout builds from 2 repositories (a, b) and kelson's build plane produces one image " +
		"per build. Per-component image production is issue #252"
	trigger := &fakeAutoDeploy{plan: PushPlan{Refused: refusal}}
	h := tracking(t, &fakePoker{}, trigger, projectFromSource("checkout", "https://github.com/acme/checkout"))

	rec := serve(h, delivery(t, "push", appSecret, []byte(pushPayload)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	body := decodeBody(t, rec)
	refused, _ := body["refused"].([]any)
	if len(refused) != 1 {
		t.Fatalf("refused = %v, want the refusal named in the answer", body)
	}
	if text, _ := refused[0].(string); !strings.Contains(text, "#252") || !strings.Contains(text, "checkout") {
		t.Errorf("the refusal was summarised away: %q", text)
	}
	h.inflight.Wait()
}

// A server with no trigger wired verifies a push, resolves it and does nothing —
// which is what every kelson did before ADR-0036, and is still right for a
// process with no spec store to write into.
func TestPushWithNoTriggerWiredIsSilent(t *testing.T) {
	h := tracking(t, &fakePoker{}, nil, projectFromSource("checkout", "https://github.com/acme/checkout"))
	rec := serve(h, delivery(t, "push", appSecret, []byte(pushPayload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if acted, _ := decodeBody(t, rec)["acted"].(bool); acted {
		t.Error("a push acted with no trigger wired")
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
