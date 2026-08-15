package forge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
)

// sign is what GitHub does to a delivery body, so the accept case is exercised
// against a real HMAC rather than a hard-coded digest that would have to be
// recomputed every time the fixture changes.
func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// The webhook endpoint is the public attack surface ADR-0033 names, and this is
// the gate on it. Every case that is not an exact HMAC match over the exact
// bytes must be refused, including the ones that look like configuration
// mistakes rather than attacks.
func TestVerifySignature(t *testing.T) {
	secret := []byte("a-webhook-secret")
	body := []byte(`{"zen":"Non-blocking is better than blocking."}`)

	tests := []struct {
		name      string
		secret    []byte
		signature string
		body      []byte
		wantErr   bool
	}{
		{
			name:      "a genuine delivery",
			secret:    secret,
			signature: sign(secret, body),
			body:      body,
		},
		{
			name:      "signed with another secret",
			secret:    secret,
			signature: sign([]byte("someone-elses-secret"), body),
			body:      body,
			wantErr:   true,
		},
		{
			name:      "the body was changed in flight",
			secret:    secret,
			signature: sign(secret, body),
			body:      append(append([]byte{}, body...), ' '),
			wantErr:   true,
		},
		{
			name:    "no signature header",
			secret:  secret,
			body:    body,
			wantErr: true,
		},
		{
			name:      "the deprecated sha1 header shape is not honoured",
			secret:    secret,
			signature: "sha1=" + hex.EncodeToString(make([]byte, 20)),
			body:      body,
			wantErr:   true,
		},
		{
			name:      "a digest that is not hex",
			secret:    secret,
			signature: signaturePrefix + "not-hex-at-all",
			body:      body,
			wantErr:   true,
		},
		{
			name:      "a digest of the wrong length",
			secret:    secret,
			signature: signaturePrefix + "abcd",
			body:      body,
			wantErr:   true,
		},
		{
			// Fail closed: a connection with no secret cannot verify anything,
			// and accepting on that basis is a listener with no check at all.
			name:      "the connection has no webhook secret",
			signature: sign(secret, body),
			body:      body,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Conn{Provider: nameGitHub, WebhookSecret: tt.secret}
			header := http.Header{}
			if tt.signature != "" {
				header.Set(signatureHeader, tt.signature)
			}

			err := newGitHub().VerifySignature(c, header, tt.body)
			if (err != nil) != tt.wantErr {
				t.Fatalf("VerifySignature() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrBadSignature) {
				t.Errorf("error = %v, want every refusal to wrap ErrBadSignature", err)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		body    string
		want    Event
		wantIs  error
		wantErr bool
	}{
		{
			name: "ping",
			kind: "ping",
			body: `{"zen":"Design for failure.","hook_id":1,
			        "repository":{"full_name":"acme/checkout","html_url":"https://github.com/acme/checkout"},
			        "installation":{"id":42}}`,
			want: Event{
				Kind:           "ping",
				RepoFullName:   "acme/checkout",
				RepoHTMLURL:    "https://github.com/acme/checkout",
				InstallationID: 42,
			},
		},
		{
			name: "push",
			kind: "push",
			body: `{"ref":"refs/heads/main","before":"aaa","after":"bbb111",
			        "head_commit":{"id":"bbb111"},
			        "repository":{"full_name":"acme/checkout","html_url":"https://github.com/acme/checkout"},
			        "installation":{"id":42}}`,
			want: Event{
				Kind:           "push",
				RepoFullName:   "acme/checkout",
				RepoHTMLURL:    "https://github.com/acme/checkout",
				Ref:            "refs/heads/main",
				HeadSHA:        "bbb111",
				InstallationID: 42,
			},
		},
		{
			// A branch deletion has a null head_commit and an all-zero `after`.
			// It still parses: what to do about it is the caller's decision,
			// not a parse failure.
			name: "push that deletes a branch",
			kind: "push",
			body: `{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000",
			        "head_commit":null,
			        "repository":{"full_name":"acme/checkout"}}`,
			want: Event{
				Kind:         "push",
				RepoFullName: "acme/checkout",
				Ref:          "refs/heads/gone",
				HeadSHA:      "0000000000000000000000000000000000000000",
			},
		},
		{
			name: "pull_request",
			kind: "pull_request",
			body: `{"action":"synchronize","number":412,
			        "pull_request":{"number":412,"head":{"sha":"ccc222"}},
			        "repository":{"full_name":"acme/checkout","html_url":"https://github.com/acme/checkout"},
			        "installation":{"id":42}}`,
			want: Event{
				Kind:           "pull_request",
				RepoFullName:   "acme/checkout",
				RepoHTMLURL:    "https://github.com/acme/checkout",
				HeadSHA:        "ccc222",
				PRNumber:       412,
				Action:         "synchronize",
				InstallationID: 42,
			},
		},
		{
			name: "pull_request without the top-level number",
			kind: "pull_request",
			body: `{"action":"opened",
			        "pull_request":{"number":7,"head":{"sha":"ddd333"}},
			        "repository":{"full_name":"acme/checkout"}}`,
			want: Event{
				Kind:         "pull_request",
				RepoFullName: "acme/checkout",
				HeadSHA:      "ddd333",
				PRNumber:     7,
				Action:       "opened",
			},
		},
		{
			// The event that records the installation id on the connection
			// (ADR-0033 decision 2 step 3). It carries no repository of its own.
			name: "installation",
			kind: "installation",
			body: `{"action":"created","installation":{"id":99}}`,
			want: Event{Kind: "installation", Action: "created", InstallationID: 99},
		},
		{
			name:   "an event nobody subscribed to",
			kind:   "issues",
			body:   `{"action":"opened"}`,
			wantIs: ErrIgnoredEvent,
		},
		{
			name:   "no event header",
			body:   `{}`,
			wantIs: ErrIgnoredEvent,
		},
		{
			// A mapped kind whose body is broken is a real failure, not
			// something to skip: the delivery was for us and we could not read
			// it.
			name:    "a mapped kind with an unreadable body",
			kind:    "push",
			body:    `{"ref":`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			if tt.kind != "" {
				header.Set(eventHeader, tt.kind)
			}

			got, err := newGitHub().ParseEvent(header, []byte(tt.body))
			switch {
			case tt.wantIs != nil:
				if !errors.Is(err, tt.wantIs) {
					t.Fatalf("error = %v, want it to wrap %v", err, tt.wantIs)
				}
				return
			case tt.wantErr:
				if err == nil {
					t.Fatalf("ParseEvent() = %+v, want an error", got)
				}
				if errors.Is(err, ErrIgnoredEvent) {
					t.Error("a broken body for a subscribed event must not be reported as an ignorable one")
				}
				return
			case err != nil:
				t.Fatalf("ParseEvent(): %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseEvent() =\n\t%+v\nwant\n\t%+v", got, tt.want)
			}
		})
	}
}
