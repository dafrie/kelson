package forgeconn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// The resolution half of ADR-0033 decisions 4 and 5. The matcher's own
// ordering is already pinned in internal/controlstore; what is tested here is
// what this package adds to it — reading the Secret, minting through the right
// adapter, the bootstrap token's rank, and the two refusals that must name the
// field that fixes them.

// fakeStore serves connections and their material from literals. ReadAuthSecret
// mirrors the real store's contract in the one respect callers depend on: a
// connection with no material is an error, not an empty AuthMaterial.
type fakeStore struct {
	conns    []controlstore.StoredConnection
	material map[string]controlstore.AuthMaterial
	listErr  error
}

func (s *fakeStore) List(context.Context) ([]controlstore.StoredConnection, error) {
	return s.conns, s.listErr
}

func (s *fakeStore) ReadAuthSecret(_ context.Context, c controlstore.StoredConnection) (controlstore.AuthMaterial, error) {
	m, ok := s.material[c.Name]
	if !ok {
		return controlstore.AuthMaterial{}, controlstore.NotFound("connection/"+c.Name,
			"no Secret for "+c.Name, "create it")
	}
	return m, nil
}

func tokenConnection(name, host string) controlstore.StoredConnection {
	return controlstore.StoredConnection{
		Name: name,
		Spec: model.GitConnectionSpec{
			Provider: model.GitProviderGitHub,
			Host:     host,
			Auth:     model.GitConnectionAuth{Token: &model.TokenAuth{SecretRef: name + "-secret"}},
		},
	}
}

func genericConnection(name, host string) controlstore.StoredConnection {
	c := tokenConnection(name, host)
	c.Spec.Provider = model.GitProviderGeneric
	return c
}

func storeWith(conns ...controlstore.StoredConnection) *fakeStore {
	s := &fakeStore{conns: conns, material: map[string]controlstore.AuthMaterial{}}
	for _, c := range conns {
		s.material[c.Name] = controlstore.AuthMaterial{
			SecretRef: c.Name + "-secret",
			Token:     "tok-" + c.Name,
		}
	}
	return s
}

func TestResolveHostMatchMintsTheStoredToken(t *testing.T) {
	r := &Resolver{Store: storeWith(tokenConnection("acme-github", "https://github.com"))}

	cred, res, ok, err := r.Credential(context.Background(), "https://github.com/acme/checkout", "")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !ok {
		t.Fatal("a host match must resolve, not fall through to anonymous")
	}
	if res.Name() != "acme-github" {
		t.Errorf("connection = %q, want acme-github", res.Name())
	}
	if res.Match.Kind != controlstore.MatchHost {
		t.Errorf("match kind = %v, want MatchHost", res.Match.Kind)
	}
	if cred.Password != "tok-acme-github" {
		t.Errorf("password = %q, want the stored token", cred.Password)
	}
	if !cred.ExpiresAt.IsZero() {
		t.Error("a stored token does not expire on kelson's clock; ExpiresAt must stay zero")
	}
}

// A public repository no connection covers is not a failure: ADR-0033 decision
// 4 keeps anonymous the default, and MatchNone's own doc says a caller turns it
// into "clone anonymously".
func TestResolveNoMatchIsAnonymousAndNotAnError(t *testing.T) {
	r := &Resolver{Store: storeWith(tokenConnection("acme-github", "https://github.com"))}

	_, ok, err := r.Resolve(context.Background(), "https://gitlab.com/acme/checkout", "")
	if err != nil {
		t.Fatalf("an unmatched source must not be an error: %v", err)
	}
	if ok {
		t.Error("an unmatched source must not resolve to a connection")
	}
}

func TestResolveExplicitNameWins(t *testing.T) {
	store := storeWith(
		tokenConnection("acme-github", "https://github.com"),
		genericConnection("mirror", "https://git.acme.internal"),
	)
	r := &Resolver{Store: store}

	res, ok, err := r.Resolve(context.Background(), "https://github.com/acme/checkout", "mirror")
	if err != nil || !ok {
		t.Fatalf("Resolve: %v (ok=%v)", err, ok)
	}
	if res.Name() != "mirror" {
		t.Errorf("connection = %q, want the explicitly named mirror", res.Name())
	}
	if res.Match.Kind != controlstore.MatchExplicit {
		t.Errorf("match kind = %v, want MatchExplicit", res.Match.Kind)
	}
}

// Both refusals must name `source.connection`: it is the field ADR-0033
// decision 4 provides to fix them, and an error that does not name it leaves
// the reader to guess.
func TestAmbiguousAndMissingNameTheFieldThatFixesThem(t *testing.T) {
	store := storeWith(
		tokenConnection("one", "https://github.com"),
		tokenConnection("two", "https://github.com"),
	)
	r := &Resolver{Store: store}

	_, _, err := r.Resolve(context.Background(), "https://github.com/acme/checkout", "")
	assertRefusal(t, err, "$.spec.source.connection", []string{"one", "two"})

	_, _, err = r.Resolve(context.Background(), "https://github.com/acme/checkout", "three")
	assertRefusal(t, err, "$.spec.source.connection", []string{`"three"`, "one, two"})
}

func assertRefusal(t *testing.T, err error, field string, mustSay []string) {
	t.Helper()
	var de delivery.Error
	if !errors.As(err, &de) {
		t.Fatalf("error = %v (%T), want a delivery.Error", err, err)
	}
	if de.Field != field {
		t.Errorf("field = %q, want %q", de.Field, field)
	}
	if de.Remediation == "" {
		t.Error("a refusal with no remediation is a refusal nobody can act on")
	}
	text := de.Message + " " + de.Remediation
	for _, want := range mustSay {
		if !strings.Contains(text, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, text)
		}
	}
}

func TestUnknownProviderIsRefusedRatherThanTreatedAsGeneric(t *testing.T) {
	c := tokenConnection("gitlab", "https://gitlab.com")
	c.Spec.Provider = "gitlab"
	r := &Resolver{Store: storeWith(c)}

	_, _, err := r.Resolve(context.Background(), "https://gitlab.com/acme/checkout", "")
	var de delivery.Error
	if !errors.As(err, &de) {
		t.Fatalf("error = %v, want a delivery.Error", err)
	}
	if !strings.Contains(de.Message, "gitlab") {
		t.Errorf("refusal must name the provider it does not speak: %s", de.Message)
	}
}

// ADR-0033 decision 5: the bootstrap token answers only where no stored
// connection does, so creating a GitConnection is an upgrade and not a
// migration.
func TestBootstrapRanksBelowEveryStoredConnection(t *testing.T) {
	r := &Resolver{
		Store:     storeWith(tokenConnection("acme-github", "https://github.com")),
		Bootstrap: NewBootstrap("bootstrap-token", ""),
	}

	res, ok, err := r.Resolve(context.Background(), "https://github.com/acme/checkout", "")
	if err != nil || !ok {
		t.Fatalf("Resolve: %v (ok=%v)", err, ok)
	}
	if res.Bootstrap {
		t.Error("a stored connection covering the host must beat the bootstrap token")
	}

	// …and it answers where nothing else does, which is what keeps today's
	// process-wide behaviour intact.
	res, ok, err = r.Resolve(context.Background(), "https://gitlab.com/acme/checkout", "")
	if err != nil || !ok {
		t.Fatalf("Resolve: %v (ok=%v)", err, ok)
	}
	if !res.Bootstrap {
		t.Error("with no stored connection covering the host, the bootstrap token must answer")
	}
	if res.Name() != BootstrapName {
		t.Errorf("bootstrap connection name = %q, want %q", res.Name(), BootstrapName)
	}
}

func TestBootstrapIsNameableAndMintsItsToken(t *testing.T) {
	r := &Resolver{Bootstrap: NewBootstrap("bootstrap-token", "kelson")}

	cred, res, ok, err := r.Credential(context.Background(), "https://github.com/acme/checkout", BootstrapName)
	if err != nil || !ok {
		t.Fatalf("Credential: %v (ok=%v)", err, ok)
	}
	if !res.Bootstrap {
		t.Error("naming the bootstrap connection must resolve to it")
	}
	if cred.Password != "bootstrap-token" || cred.Username != "kelson" {
		t.Errorf("credential = %s, want the bootstrap token and its username", cred)
	}
}

func TestNoBootstrapAndNoConnectionsIsAnonymous(t *testing.T) {
	r := &Resolver{}
	_, ok, err := r.Resolve(context.Background(), "https://github.com/acme/checkout", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ok {
		t.Error("a resolver with nothing configured must resolve to nothing")
	}
}

// The value must be unprintable from the moment the process learns it, not from
// the moment somebody remembers to scrub (issue #117, ADR-0033 decision 2).
func TestBootstrapTokenIsRegisteredWithRedact(t *testing.T) {
	const token = "ghp-bootstrap-registration-probe"
	if NewBootstrap(token, "") == nil {
		t.Fatal("NewBootstrap returned nil for a non-empty token")
	}
	if got := redact.Scrub("token is " + token); strings.Contains(got, token) {
		t.Errorf("the bootstrap token survived redaction: %q", got)
	}
}

func TestMintedCredentialIsRegisteredWithRedact(t *testing.T) {
	const token = "tok-redaction-probe-connection"
	store := storeWith(tokenConnection("redaction-probe-connection", "https://github.com"))
	r := &Resolver{Store: store}

	if _, _, _, err := r.Credential(context.Background(), "https://github.com/acme/checkout", ""); err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if got := redact.Scrub("clone with " + token); strings.Contains(got, token) {
		t.Errorf("the minted credential survived redaction: %q", got)
	}
}

func TestByProviderSkipsConnectionsWhoseSecretCannotBeRead(t *testing.T) {
	good := tokenConnection("good", "https://github.com")
	broken := tokenConnection("broken", "https://github.com")
	elsewhere := genericConnection("elsewhere", "https://git.acme.internal")
	store := storeWith(good, elsewhere)
	store.conns = append(store.conns, broken) // no material recorded for it

	r := &Resolver{Store: store}
	found, skipped, err := r.ByProvider(context.Background(), "github")
	if err != nil {
		t.Fatalf("ByProvider: %v", err)
	}
	if len(found) != 1 || found[0].Name() != "good" {
		t.Errorf("found = %v, want just the readable github connection", resolutionNames(found))
	}
	if len(skipped) != 1 || skipped[0] != "broken" {
		t.Errorf("skipped = %v, want [broken]: a caller must be able to say what it could not consider", skipped)
	}
}

func resolutionNames(rs []Resolution) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name())
	}
	return out
}

func TestDeprecationIsEmptyWithoutABootstrapToken(t *testing.T) {
	var b *Bootstrap
	if b.Deprecation() != "" {
		t.Error("no bootstrap token means nothing to deprecate")
	}
	if got := NewBootstrap("x", "").Deprecation(); !strings.Contains(got, BootstrapEnv) || !strings.Contains(got, "deprecated") {
		t.Errorf("deprecation notice = %q, want it to name the variable and say deprecated", got)
	}
}
