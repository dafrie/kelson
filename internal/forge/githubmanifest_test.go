package forge

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/redact"
)

// The manifest is the permission set kelson asks for, and ADR-0033 decision 2
// fixes it: less silently loses features, more is kelson asking for write
// access to source it only reads. Asserting it field by field is what keeps a
// later "just add repo:write" from being a one-line edit nobody reviews.
func TestAppManifestJSON(t *testing.T) {
	m := AppManifest{
		Name:        "kelson-acme",
		URL:         "https://kelson.acme.test",
		WebhookURL:  "https://kelson.acme.test/forge/github/webhook",
		RedirectURL: "https://kelson.acme.test/forge/github/manifest/callback",
		Description: "acme's kelson instance",
	}
	raw, err := m.JSON()
	if err != nil {
		t.Fatalf("AppManifest.JSON: %v", err)
	}

	var got struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		Description    string `json:"description"`
		HookAttributes struct {
			URL    string `json:"url"`
			Active bool   `json:"active"`
		} `json:"hook_attributes"`
		RedirectURL        string            `json:"redirect_url"`
		Public             bool              `json:"public"`
		DefaultPermissions map[string]string `json:"default_permissions"`
		DefaultEvents      []string          `json:"default_events"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the manifest is not JSON: %v", err)
	}

	if got.Name != m.Name || got.URL != m.URL || got.Description != m.Description {
		t.Errorf("identity = %q/%q/%q, want the manifest's", got.Name, got.URL, got.Description)
	}
	if got.HookAttributes.URL != m.WebhookURL || !got.HookAttributes.Active {
		t.Errorf("hook_attributes = %+v, want the webhook URL, active", got.HookAttributes)
	}
	if got.RedirectURL != m.RedirectURL {
		t.Errorf("redirect_url = %q, want %q", got.RedirectURL, m.RedirectURL)
	}
	if got.Public {
		t.Error("public = true: a per-instance app must not offer itself for installation by strangers")
	}

	wantPermissions := map[string]string{
		"contents":      "read",
		"metadata":      "read",
		"pull_requests": "write",
		"statuses":      "write",
		"checks":        "write",
	}
	if len(got.DefaultPermissions) != len(wantPermissions) {
		t.Errorf("default_permissions = %v, want exactly %v", got.DefaultPermissions, wantPermissions)
	}
	for k, v := range wantPermissions {
		if got.DefaultPermissions[k] != v {
			t.Errorf("default_permissions[%q] = %q, want %q", k, got.DefaultPermissions[k], v)
		}
	}
	if want := []string{"push", "pull_request"}; !slices.Equal(got.DefaultEvents, want) {
		t.Errorf("default_events = %v, want %v", got.DefaultEvents, want)
	}
}

func TestAppManifestJSONRejects(t *testing.T) {
	full := AppManifest{
		Name:        "kelson-acme",
		WebhookURL:  "https://kelson.acme.test/forge/github/webhook",
		RedirectURL: "https://kelson.acme.test/forge/github/manifest/callback",
	}
	tests := []struct {
		name   string
		mutate func(*AppManifest)
	}{
		{name: "no name", mutate: func(m *AppManifest) { m.Name = "" }},
		{name: "a blank name", mutate: func(m *AppManifest) { m.Name = "  " }},
		{name: "no webhook URL", mutate: func(m *AppManifest) { m.WebhookURL = "" }},
		{name: "no redirect URL", mutate: func(m *AppManifest) { m.RedirectURL = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := full
			tt.mutate(&m)
			if _, err := m.JSON(); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestManifestRegistrationURL(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		org     string
		state   string
		want    string
		wantErr bool
	}{
		{
			name:  "a personal account on github.com",
			host:  "https://github.com",
			state: "one-time-state",
			want:  "https://github.com/settings/apps/new?state=one-time-state",
		},
		{
			name: "no host means github.com",
			want: "https://github.com/settings/apps/new",
		},
		{
			name:  "an organisation",
			host:  "https://github.com",
			org:   "acme",
			state: "s",
			want:  "https://github.com/organizations/acme/settings/apps/new?state=s",
		},
		{
			// The web URL, not the API one: this is where a browser goes.
			name: "enterprise keeps its own host and path",
			host: "https://ghe.acme.example/",
			want: "https://ghe.acme.example/settings/apps/new",
		},
		{
			name:  "an organisation name is escaped, not interpolated",
			host:  "https://github.com",
			org:   "acme/../evil",
			state: "a state with spaces & symbols",
			want:  "https://github.com/organizations/acme%2F..%2Fevil/settings/apps/new?state=a+state+with+spaces+%26+symbols",
		},
		{name: "not a URL", host: "://nope", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ManifestRegistrationURL(tt.host, tt.org, tt.state)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ManifestRegistrationURL() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ManifestRegistrationURL(): %v", err)
			}
			if got != tt.want {
				t.Errorf("ManifestRegistrationURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExchangeManifestCode(t *testing.T) {
	// A real key, so the round-trip asserts something the first mint would
	// otherwise be the first to discover: the PEM that comes back is key
	// material this package can sign an assertion with.
	key := string(testKeyPEM(t))

	t.Run("turns the one-time code into the app's credentials", func(t *testing.T) {
		var path, method string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requireHeaders(t, r)
			path, method = r.URL.EscapedPath(), r.Method
			// Unauthenticated by design: the code is the credential.
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want the exchange to send none", got)
			}
			resp := map[string]any{
				"id":             12345,
				"slug":           "kelson-acme",
				"html_url":       "https://github.com/apps/kelson-acme",
				"pem":            key,
				"webhook_secret": "a-generated-webhook-secret",
				"client_id":      "Iv1.abc123",
				"client_secret":  "a-generated-client-secret",
			}
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		got, err := ExchangeManifestCode(t.Context(), server.URL, "the-one-time-code")
		if err != nil {
			t.Fatalf("ExchangeManifestCode: %v", err)
		}
		if want := "/api/v3/app-manifests/the-one-time-code/conversions"; path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		if method != http.MethodPost {
			t.Errorf("method = %q, want POST", method)
		}
		if got.AppID != 12345 || got.Slug != "kelson-acme" || got.HTMLURL != "https://github.com/apps/kelson-acme" {
			t.Errorf("identity = %d/%q/%q, want the created app's", got.AppID, got.Slug, got.HTMLURL)
		}
		if string(got.PEM) != key {
			t.Error("the private key did not survive the exchange")
		}
		if string(got.WebhookSecret) != "a-generated-webhook-secret" {
			t.Errorf("webhook secret = %q, want the generated one", got.WebhookSecret)
		}
		if got.ClientID != "Iv1.abc123" || got.ClientSecret != "a-generated-client-secret" {
			t.Errorf("client credentials = %q/%q, want the created app's", got.ClientID, got.ClientSecret)
		}

		// The key is usable: the exchange's whole point is a connection that
		// can sign an assertion afterwards, and a PEM mangled in transit would
		// only surface at the first mint.
		if _, err := parsePrivateKey(got.PEM); err != nil {
			t.Errorf("the exchanged key does not parse, so no mint could ever work: %v", err)
		}

		// Every secret it learned is unprintable from here on (issue #117).
		for _, secret := range []string{key, "a-generated-webhook-secret", "a-generated-client-secret"} {
			if strings.Contains(redact.Scrub("value: "+secret), secret) {
				t.Errorf("%q was not registered for redaction", secret)
			}
		}
		if strings.Contains(got.String(), key) || strings.Contains(got.String(), "a-generated-webhook-secret") {
			t.Errorf("AppCredentials.String() leaked material: %s", got.String())
		}
	})

	t.Run("refusals", func(t *testing.T) {
		tests := []struct {
			name    string
			code    string
			status  int
			body    string
			wantIs  error
			wantMsg string
		}{
			{name: "no code in the callback", code: "", wantMsg: "carried no code"},
			{
				name:    "the code was already used",
				code:    "spent",
				status:  http.StatusNotFound,
				body:    `{"message":"Not Found"}`,
				wantMsg: "single-use",
			},
			{
				name:    "the code expired",
				code:    "stale",
				status:  http.StatusUnprocessableEntity,
				body:    `{"message":"Validation Failed"}`,
				wantMsg: "expires",
			},
			{
				name:   "an answer with no app in it",
				code:   "ok",
				status: http.StatusCreated,
				body:   `{"slug":"kelson-acme"}`,
				wantIs: ErrAuthFailed,
			},
			{
				name:    "an answer that is not JSON",
				code:    "ok",
				status:  http.StatusCreated,
				body:    `<html>a proxy said no</html>`,
				wantMsg: "decoding the created app",
			},
			{
				name:   "the endpoint rejects the request",
				code:   "ok",
				status: http.StatusUnauthorized,
				body:   `{"message":"Bad credentials"}`,
				wantIs: ErrAuthFailed,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
				}))
				defer server.Close()

				_, err := ExchangeManifestCode(t.Context(), server.URL, tt.code)
				if err == nil {
					t.Fatal("want an error")
				}
				if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
					t.Errorf("error = %v, want it to wrap %v", err, tt.wantIs)
				}
				if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %v, want it to mention %q", err, tt.wantMsg)
				}
			})
		}
	})
}
