package api

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/secret"
)

// The sentinel property, one test per output surface (issue #117).
//
// Each test plants a known value where it could plausibly travel and then greps
// the surface the client actually receives. What is asserted is the absence of
// the value in the bytes that leave the server — not that some function was
// called — because "we wired redaction in" is the claim that has failed for
// every platform that has leaked a secret into a log.
//
// Two sentinels, for the two mechanisms. secretManifestValue rides inside a
// Secret manifest and is caught structurally: kelson knows what a Secret is.
// resolvedCredential stands for a credential kelson has itself resolved
// (registry.Resolver, #51) and is caught by the known-value scrubber, which is
// the only thing that can help in text kelson does not own the shape of.
// authoredSecretValue is the third case, and the one #116 introduced: a value a
// user handed kelson deliberately, through SetSecret. It is neither structural
// (it never rides inside a manifest) nor resolved by kelson (nobody looked it
// up) — kelson was told it. The registration that covers it happens in the
// handler before anything else, and internal/secret registers it again on
// arrival; TestAuthoredSecretValueReachesNoOutputSurface is what holds that.
const (
	secretManifestValue = "cGFzc3dvcmQtU0VOVElORUwtOTFhYw=="
	resolvedCredential  = "resolved-credential-SENTINEL-3f7b"
	authoredSecretValue = "authored-secret-SENTINEL-7c2e"
)

// Surfaces 1 and 2 — the rendered diff against a recorded revision (#162) and
// the rollback preview (#38) — read the history store's real bytes back, and
// both were the readback paths a Secret in the history would surface through.
// ADR-0027 decision 7 deleted the store and ADR-0028 the preview, so there is
// no such readback to redact today; when the artifact-backed replacements land
// (issue #224) they need these two tests back, against the same sentinel.

// Surface 3: structured errors. A credential kelson has resolved is registered
// when it is learned, so it cannot reach an error message however deep in the
// planes the message was written.
func TestStructuredErrorsCarryNoResolvedCredential(t *testing.T) {
	redact.Register(resolvedCredential)

	// The failure is produced by the secret backend rather than by a scripted
	// adapter: the adapters are deleted (ADR-0028), and what matters is that a
	// *plane* error carrying a resolved credential is scrubbed on its way onto
	// the wire, whichever plane wrote it.
	store := newFakeSecrets()
	store.err = secret.Error{
		Code:        secret.ErrWriteFailed,
		Resource:    "Secret/hello-db",
		Message:     "the API server rejected the write: could not authenticate with " + resolvedCredential,
		Remediation: "check the credential " + resolvedCredential,
		Cause:       "unauthorized: " + resolvedCredential,
	}
	c := serve(t, Options{Secrets: store})

	_, streamErr := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("hello", "development"),
		Name:   "hello-db",
		Values: map[string]string{"url": "irrelevant"},
	}))
	if streamErr == nil {
		t.Fatal("the failing write did not surface as an error")
	}
	assertNoSentinel(t, "connect error message", []byte(streamErr.Error()), resolvedCredential)

	var cerr *connect.Error
	if !errors.As(streamErr, &cerr) {
		t.Fatalf("want a *connect.Error, got %T", streamErr)
	}
	details := 0
	for _, d := range cerr.Details() {
		value, verr := d.Value()
		if verr != nil {
			continue
		}
		wire, ok := value.(*kelsonv1alpha1.Error)
		if !ok {
			continue
		}
		details++
		assertNoSentinel(t, "structured error", []byte(wire.GetMessage()+wire.GetRemediation()+wire.GetCause()), resolvedCredential)
		if wire.GetCode() != string(secret.ErrWriteFailed) {
			t.Errorf("the code was rewritten by the scrub: %q", wire.GetCode())
		}
		if wire.GetResource() != "Secret/hello-db" {
			t.Errorf("the resource identity was rewritten by the scrub: %q", wire.GetResource())
		}
	}
	if details == 0 {
		t.Error("the structured detail was dropped; scrubbing must not cost the taxonomy")
	}
}

// Surface 4: the build log stream. Every chunk is checked, not the reassembled
// string, because a value split across two chunks is still a leak — and because
// reassembling first would hide exactly the bug the buffering exists to prevent.
func TestBuildLogChunksCarryNoResolvedCredential(t *testing.T) {
	redact.Register(resolvedCredential)

	builder := &fakeBuilder{
		logs: "#1 [internal] load build definition\n" +
			"#2 authenticating: password=" + resolvedCredential + "\n" +
			"#3 exporting to image DONE\n",
	}
	c := serve(t, Options{
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})

	out, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for i, chunk := range out.chunks {
		assertNoSentinel(t, "build log chunk", chunk, resolvedCredential)
		if i > 100 {
			break
		}
	}
	logs := out.logs()
	if !strings.Contains(logs, redact.Sentinel) {
		t.Errorf("the log was not marked where the credential was:\n%s", logs)
	}
	for _, want := range []string{"#1 [internal] load build definition", "#3 exporting to image DONE"} {
		if !strings.Contains(logs, want) {
			t.Errorf("redaction truncated the build log, losing %q:\n%s", want, logs)
		}
	}
}

// Surface 5: the manifests a Render returns. There is no path today that puts a
// Secret in a render over the API (the spec cannot hold a literal and the server
// refuses overlays, pipeline.go), so what is asserted here is the other half of
// the contract: redaction must leave every ordinary manifest byte-identical.
// A redactor that reformats what it had no reason to touch would turn every
// render into a diff.
func TestRenderedManifestsAreUntouchedWhenThereIsNothingToRedact(t *testing.T) {
	c := serve(t, Options{})
	req := &kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}
	first, err := c.render.Render(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := c.render.Render(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(first.Msg.GetManifests()) == 0 {
		t.Fatal("no manifests rendered")
	}
	for i, m := range first.Msg.GetManifests() {
		if !bytes.Equal(m.GetYaml(), second.Msg.GetManifests()[i].GetYaml()) {
			t.Fatalf("rendering the same spec twice produced different bytes for %s/%s", m.GetKind(), m.GetName())
		}
		if bytes.Contains(m.GetYaml(), []byte(redact.Sentinel)) {
			t.Errorf("%s/%s was redacted with nothing to redact:\n%s", m.GetKind(), m.GetName(), m.GetYaml())
		}
	}
}

// Surface 6: a value authored through SecretService (#116). This is the first
// RPC in the schema that receives a credential, so it is the first place a
// caller could put one into kelson's own output — and the surfaces that would
// carry it are the response, the listing, and every error the request produces
// afterwards.
//
// The failing path is asserted with a value that has already been written, so
// the error is produced by a request the store has seen the value from: the
// question is whether the *value* travels, not whether an error mentioning it
// can be constructed at all.
func TestAuthoredSecretValueReachesNoOutputSurface(t *testing.T) {
	store := newFakeSecrets()
	c := serve(t, Options{Secrets: store})

	set, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("hello", "development"),
		Name:   "hello-db",
		Values: map[string]string{"url": authoredSecretValue},
	}))
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	assertNoSentinel(t, "SetSecret response", []byte(set.Msg.String()), authoredSecretValue)
	if got := store.held("hello-development", "hello-db")["url"]; got != authoredSecretValue {
		t.Fatalf("the value did not reach the store, so this test is asserting nothing: %q", got)
	}

	list, err := c.secrets.ListSecrets(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListSecretsRequest{
		Target: secretTargetOf("hello", "development"),
	}))
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	assertNoSentinel(t, "ListSecrets response", []byte(list.Msg.String()), authoredSecretValue)
	if keys := list.Msg.GetSecrets()[0].GetKeys(); len(keys) != 1 || keys[0] != "url" {
		t.Errorf("keys = %v: the listing must still say what the Secret holds", keys)
	}

	// Now make the backend fail with a message that quotes the value, which is
	// the mistake a future plane could make. The registration is what has to
	// catch it, not the discipline of whoever wrote the message.
	store.err = secret.Error{
		Code:        secret.ErrWriteFailed,
		Resource:    "Secret/hello-development/hello-db",
		Message:     "the API server rejected the write of " + authoredSecretValue,
		Remediation: "retry with " + authoredSecretValue,
		Cause:       "invalid value: " + authoredSecretValue,
	}
	_, err = c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("hello", "development"),
		Name:   "hello-db",
		Values: map[string]string{"url": authoredSecretValue},
	}))
	if err == nil {
		t.Fatal("the failing write did not surface as an error")
	}
	assertNoSentinel(t, "connect error message", []byte(err.Error()), authoredSecretValue)

	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("want a *connect.Error, got %T", err)
	}
	details := 0
	for _, d := range cerr.Details() {
		value, verr := d.Value()
		if verr != nil {
			continue
		}
		wire, ok := value.(*kelsonv1alpha1.Error)
		if !ok {
			continue
		}
		details++
		assertNoSentinel(t, "structured error",
			[]byte(wire.GetMessage()+wire.GetRemediation()+wire.GetCause()), authoredSecretValue)
		if wire.GetCode() != string(secret.ErrWriteFailed) {
			t.Errorf("the code was rewritten by the scrub: %q", wire.GetCode())
		}
	}
	if details == 0 {
		t.Error("the structured detail was dropped; scrubbing must not cost the taxonomy")
	}
}

// Surface 7: a build report's `message` (ADR-0034 decision 3). It is the one
// wire field in this package that carries plane error text on a *successful*
// response — a publish that failed rides in it beside the ones that succeeded —
// so it does not pass the errors.go boundary where every other free-text
// surface is scrubbed. The credential a preview publish authenticates with is
// resolved by kelson and registered when it is learned, and "403 Forbidden" is
// exactly the text a registry client could quote it into.
func TestReportMessageCarriesNoResolvedCredential(t *testing.T) {
	redact.Register(resolvedCredential)

	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": reportPreviewEnvDoc("staging", "https://github.com/acme/checkout"),
		"canary":  reportPreviewEnvDoc("canary", "https://github.com/acme/checkout"),
	})
	p.publisher.errs["canary"] = errors.New("401 Unauthorized pushing as " + resolvedCredential)

	res := report(t, p.clients, previewReport())
	if len(res.GetTriggered()) != 1 {
		t.Fatalf("triggered = %v; the partial failure this test needs did not happen", res.GetTriggered())
	}
	assertNoSentinel(t, "ReportBuild message", []byte(res.GetMessage()), resolvedCredential)
	if !strings.Contains(res.GetMessage(), "401 Unauthorized") {
		t.Errorf("the scrub took the diagnostic with the credential: %s", res.GetMessage())
	}
}

// Surface 8: the comment kelson upserts on a change request (ADR-0034 decision
// 5). It is the first text this server writes onto somebody else's system, and
// the only one a reader outside the deployment can see, so it is the surface
// where a leak is least recoverable — a Secret in a build log lives behind
// authentication, and a Secret in a pull request comment does not.
//
// The value planted is the credential a preview publish authenticates its push
// with: it is resolved by kelson and registered when it is learned, and the
// reference the registry reports back is text kelson does not own the shape of.
func TestThePreviewCommentCarriesNoResolvedCredential(t *testing.T) {
	redact.Register(resolvedCredential)

	p := previewReportServer(t)
	p.publisher.reference = "ghcr.io/acme/checkout-previews@sha256:beef?token=" + resolvedCredential
	report(t, p.clients, previewReport())

	written := p.outcomes.written()
	if len(written) != 1 {
		t.Fatalf("wrote %d comments; the surface this test guards was not produced", len(written))
	}
	assertNoSentinel(t, "preview comment body", []byte(written[0].Body), resolvedCredential)
	if !strings.Contains(written[0].Body, "ghcr.io/acme/checkout-previews") {
		t.Errorf("the scrub took the revision with the credential:\n%s", written[0].Body)
	}
}

func assertNoSentinel(t *testing.T, surface string, body []byte, sentinel string) {
	t.Helper()
	if bytes.Contains(body, []byte(sentinel)) {
		t.Errorf("the sentinel value reached the %s:\n%s", surface, body)
	}
}
