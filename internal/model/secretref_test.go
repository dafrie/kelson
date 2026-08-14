package model

import (
	"slices"
	"strings"
	"testing"
)

// Tests for the secret reference form (issue #79, ADR-0018): an environment
// value written as `{secret: <name>, key: <key>}`.

func projectWithEnv(env string) []byte {
	return []byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:1
  env:
` + env + `
  components:
    - name: web
      port: 8080
`)
}

// TestSecretReferenceAccepted: the reference form is valid at every scope an
// env map exists at, and carries no secret/literal complaint even under a
// credential-shaped variable name — which is the whole point of writing it.
func TestSecretReferenceAccepted(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:1
  env:
    LOG_LEVEL: info
    DATABASE_URL: {secret: checkout-db, key: url}
  components:
    - name: web
      port: 8080
      env:
        STRIPE_API_KEY: {secret: payments, key: stripe-api-key}
        SESSION_PEPPER: {secret: payments, key: session.pepper}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  components:
    - name: web
      env:
        STRIPE_API_KEY: {secret: payments-prod, key: stripe-api-key}
`))
	if len(errs) != 0 {
		t.Fatalf("secret references are valid, got:\n%v", errs)
	}
	p, ok := docs[0].(*Project)
	if !ok {
		t.Fatalf("expected *Project, got %T", docs[0])
	}
	ref := p.Spec.Env["DATABASE_URL"].Secret
	if ref == nil {
		t.Fatalf("DATABASE_URL did not decode as a secret reference: %+v", p.Spec.Env["DATABASE_URL"])
	}
	if ref.Name != "checkout-db" || ref.Key != "url" {
		t.Errorf("reference = %+v, want {checkout-db url}", *ref)
	}
	if p.Spec.Env["LOG_LEVEL"].Literal != "info" {
		t.Errorf("a plain string beside a reference must stay a plain string: %+v", p.Spec.Env["LOG_LEVEL"])
	}
}

// TestSecretReferenceRoundTrips: what decodes re-encodes to the same shape, so
// a spec kelson rewrites (promotion writes Environment documents back to disk)
// does not silently change form.
func TestSecretReferenceRoundTrips(t *testing.T) {
	for _, ev := range []EnvValue{
		{Literal: "info"},
		{Secret: &SecretRef{Name: "payments", Key: "stripe-api-key"}},
		{From: &ServiceBinding{Service: "db", Key: "uri"}},
	} {
		j, err := ev.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%s): %v", ev, err)
		}
		var back EnvValue
		if err := back.UnmarshalJSON(j); err != nil {
			t.Fatalf("UnmarshalJSON(%s): %v", j, err)
		}
		if back.String() != ev.String() {
			t.Errorf("round trip changed %s into %s (via %s)", ev, back, j)
		}
	}
}

// TestMalformedEnvMappingRejected: a mapping that is neither form is
// schema/invalid-format, and the remediation shows every form there is —
// an author who wrote the wrong shape is choosing between them.
func TestMalformedEnvMappingRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"neither discriminator", `    DATABASE_URL: {name: checkout-db, key: url}`},
		{"key without secret", `    DATABASE_URL: {key: url}`},
		{"empty mapping", `    DATABASE_URL: {}`},
		{"both forms at once", `    DATABASE_URL: {secret: checkout-db, key: url, from: {service: db, key: uri}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := DecodeDocuments(projectWithEnv(tc.env))
			var got *Error
			for i := range errs {
				if errs[i].Code == ErrInvalidFormat && strings.Contains(errs[i].Remediation, "{secret:") {
					got = &errs[i]
				}
			}
			if got == nil {
				t.Fatalf("malformed env mapping must be %s with the form list, got:\n%v", ErrInvalidFormat, errs)
			}
			if !strings.Contains(got.Remediation, "{from:") {
				t.Errorf("remediation must show the binding form too, got %q", got.Remediation)
			}
			if !strings.Contains(got.Remediation, "plain string") {
				t.Errorf("remediation must show the plain form too, got %q", got.Remediation)
			}
			if got.Line == 0 {
				t.Errorf("a shape error must carry a source line: %+v", got)
			}
		})
	}
}

// TestSecretReferenceUnknownFieldRejected: the reference has exactly two keys,
// and a third is an unknown field rather than something quietly dropped.
func TestSecretReferenceUnknownFieldRejected(t *testing.T) {
	_, errs := DecodeDocuments(projectWithEnv(`    DATABASE_URL: {secret: checkout-db, key: url, namespace: other}`))
	var got *Error
	for i := range errs {
		if errs[i].Code == ErrUnknownField {
			got = &errs[i]
		}
	}
	if got == nil {
		t.Fatalf("an extra key in a reference must be %s, got:\n%v", ErrUnknownField, errs)
	}
	if got.Field != "$.spec.env.DATABASE_URL.namespace" {
		t.Errorf("field = %q, want $.spec.env.DATABASE_URL.namespace", got.Field)
	}
}

// TestSecretReferenceHalvesRequired: a half-written reference names the half
// that is missing, rather than the shape in general — the author is already in
// the right form and needs the field, not the grammar.
func TestSecretReferenceHalvesRequired(t *testing.T) {
	for _, tc := range []struct {
		name, env, field string
	}{
		{"no key", `    DATABASE_URL: {secret: checkout-db}`, "$.spec.env.DATABASE_URL.key"},
		{"empty key", `    DATABASE_URL: {secret: checkout-db, key: ""}`, "$.spec.env.DATABASE_URL.key"},
		{"empty name", `    DATABASE_URL: {secret: "", key: url}`, "$.spec.env.DATABASE_URL.secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := DecodeDocuments(projectWithEnv(tc.env))
			var got *Error
			for i := range errs {
				if errs[i].Code == ErrMissingRequired && errs[i].Field == tc.field {
					got = &errs[i]
				}
			}
			if got == nil {
				t.Fatalf("expected %s on %s, got:\n%v", ErrMissingRequired, tc.field, errs)
			}
			if got.Remediation == "" {
				t.Errorf("missing remediation: %+v", got)
			}
		})
	}
}

// TestSecretReferenceNameAndKeyRules: the name must be a Secret's name and the
// key must be a key a Secret can hold. kelson refuses at authoring time what
// the API server would refuse at apply time.
func TestSecretReferenceNameAndKeyRules(t *testing.T) {
	for _, tc := range []struct {
		name, env, field string
	}{
		{"name is not DNS-1123", `    DATABASE_URL: {secret: Checkout_DB, key: url}`, "$.spec.env.DATABASE_URL.secret"},
		{"key has a slash", `    DATABASE_URL: {secret: checkout-db, key: "db/url"}`, "$.spec.env.DATABASE_URL.key"},
		{"key has a space", `    DATABASE_URL: {secret: checkout-db, key: "db url"}`, "$.spec.env.DATABASE_URL.key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := DecodeDocuments(projectWithEnv(tc.env))
			var got *Error
			for i := range errs {
				if errs[i].Code == ErrInvalidFormat && errs[i].Field == tc.field {
					got = &errs[i]
				}
			}
			if got == nil {
				t.Fatalf("expected %s on %s, got:\n%v", ErrInvalidFormat, tc.field, errs)
			}
		})
	}
	// Dots, dashes and underscores are legal Secret keys and must stay legal.
	for _, key := range []string{"url", "session.pepper", "stripe-api-key", "TLS_CRT", "a.b-c_d"} {
		_, errs := DecodeDocuments(projectWithEnv(`    DATABASE_URL: {secret: checkout-db, key: ` + key + `}`))
		if len(errs) != 0 {
			t.Errorf("key %q must be accepted, got:\n%v", key, errs)
		}
	}
}

// TestSecretReferenceIsNotALiteral: a reference under a credential-shaped
// variable name is exactly what ADR-0009 asks authors to write, so it must
// never trip the secret/literal heuristic.
func TestSecretReferenceIsNotALiteral(t *testing.T) {
	_, errs := DecodeDocuments(projectWithEnv(
		"    API_KEY: {secret: payments, key: api-key}\n" +
			"    DATABASE_PASSWORD: {secret: checkout-db, key: password}"))
	if slices.Contains(errs.Codes(), ErrSecretLiteral) {
		t.Errorf("a reference carries no value and must never be secret/literal, got:\n%v", errs)
	}
	if len(errs) != 0 {
		t.Fatalf("references are valid, got:\n%v", errs)
	}
}

// TestSecretBackendUngated: `backend` is consumed since ADR-0018 and is no
// longer refused at validation. What the *renderer* does with a backend other
// than cluster is internal/renderer/secrets_test.go's business; here the point
// is only that an author can write it.
//
// All three backends are here since ADR-0020: `store` left the gate table with
// it, and `externalSecrets` no longer requires one — with exactly one store on
// the cluster the renderer resolves it, and only the renderer can know that.
// `sops` carries its recipient list, which ADR-0021 makes required: there is
// nothing to default an encryption key to.
func TestSecretBackendUngated(t *testing.T) {
	for backend, extra := range map[string]string{
		"cluster":         "",
		"externalSecrets": "",
		"sops":            ", ageRecipients: [" + testAgeRecipient + "]",
	} {
		_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  secrets: {backend: ` + backend + extra + "}\n"))
		if len(errs) != 0 {
			t.Errorf("backend %q must validate, got:\n%v", backend, errs)
		}
	}
}

// testAgeRecipient is a real age public key, generated for these tests. It is
// a *public* key, which is the point of the backend: it can appear in a spec,
// in a test and in a commit message without being a secret. The identity it
// pairs with was discarded.
const testAgeRecipient = "age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw"

// TestSOPSFieldsAreBackendScoped is TestExternalSecretsFieldsAreBackendScoped
// from the other side: the sops-only fields configure sops and nothing else.
func TestSOPSFieldsAreBackendScoped(t *testing.T) {
	for _, field := range []string{"ageRecipients: [" + testAgeRecipient + "]", "ageKeySecret: sops-age"} {
		_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  secrets: {backend: cluster, ` + field + "}\n"))
		if !slices.Contains(errs.Codes(), ErrMutuallyExclusive) {
			t.Errorf("%q under backend cluster must be refused, got:\n%v", field, errs)
		}
	}
}

// TestAgeRecipientsAreChecked: the list is required under sops, and the one
// mistake that must never pass silently — pasting the private half — is
// refused with its own message rather than as a format error.
func TestAgeRecipientsAreChecked(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  Code
	}{
		{"missing", "", ErrMissingRequired},
		{"empty list", "ageRecipients: []", ErrMissingRequired},
		{"not a recipient", "ageRecipients: [nonsense]", ErrInvalidFormat},
		{"private key", "ageRecipients: [AGE-SECRET-KEY-1Y2DQSCCZNAAF70QZYMRGPTWPGYRCZ2R4SDZZF52VUEJG4WVEC7ZQ6RDHC9]", ErrInvalidFormat},
		{"duplicate", "ageRecipients: [" + testAgeRecipient + ", " + testAgeRecipient + "]", ErrDuplicateName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field := tc.value
			if field != "" {
				field = ", " + field
			}
			_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  secrets: {backend: sops` + field + "}\n"))
			if !slices.Contains(errs.Codes(), tc.want) {
				t.Errorf("want %s, got:\n%v", tc.want, errs)
			}
		})
	}
}

// TestAgeKeySecretDefaults: the renderer writes this name into a
// Kustomization's spec.decryption.secretRef and must never have to decide what
// an empty one means, so resolution fills it in — beside the externalSecrets
// refresh interval, for the same reason.
func TestAgeKeySecretDefaults(t *testing.T) {
	p, e := loadPair(t, `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  components:
    - {name: api, image: nginx}`, `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  delivery: {mode: flux, git: {repo: https://example.test/deploy.git}}
  secrets: {backend: sops, ageRecipients: [`+testAgeRecipient+`]}
`)
	r, errs := Resolve(p, e)
	if len(errs) != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if r.Environment.Secrets.AgeKeySecret != DefaultAgeKeySecret {
		t.Errorf("ageKeySecret = %q, want the default %q", r.Environment.Secrets.AgeKeySecret, DefaultAgeKeySecret)
	}
}

// TestExternalSecretsFieldsAreBackendScoped: `store` and `refreshInterval`
// configure the externalSecrets backend and nothing else, so writing either
// under `cluster` is refused rather than ignored — the quiet-success shape
// issue #141 exists to prevent.
func TestExternalSecretsFieldsAreBackendScoped(t *testing.T) {
	for _, field := range []string{"store: vault-backend", "refreshInterval: 15m"} {
		_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  secrets: {backend: cluster, ` + field + "}\n"))
		if !slices.Contains(errs.Codes(), ErrMutuallyExclusive) {
			t.Errorf("%q under backend cluster must be refused, got:\n%v", field, errs)
		}
	}
}

// TestRefreshIntervalIsADuration: the renderer writes the interval through
// verbatim and may not import `time` (ADR-0001), so the parse has to happen
// here or a typo reaches external-secrets as an unparseable spec field.
func TestRefreshIntervalIsADuration(t *testing.T) {
	for _, tc := range []struct {
		interval string
		valid    bool
	}{
		{"1h", true},
		{"30s", true},
		{"24h0m0s", true},
		{"1 hour", false},
		{"hourly", false},
		{"3600", false},
		{"0", false},
		{"-5m", false},
	} {
		_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  secrets: {backend: externalSecrets, refreshInterval: "` + tc.interval + "\"}\n"))
		got := len(errs) == 0
		if got != tc.valid {
			t.Errorf("refreshInterval %q: valid=%v, want %v (errors: %v)", tc.interval, got, tc.valid, errs)
		}
	}
}
