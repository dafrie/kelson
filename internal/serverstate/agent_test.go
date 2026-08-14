package serverstate

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dafrie/kelson/internal/redact"
)

// The agent identity store's tests (issue #74). What they hold the store to:
//
//   - The token exists once. What is written to the cluster contains no part of
//     it that could be turned back into it, and the value is registered with
//     the redaction set the moment it is minted.
//   - Authentication is a proof, not a lookup: a real name with a wrong secret
//     fails exactly as a made-up name does.
//   - Revocation is a one-object write and it is idempotent.
//   - Issuance refuses what it cannot honour rather than clamping it.

const testAgent = "deploybot"

func newAgentStore(t *testing.T, client *fake.Clientset, now func() time.Time) *AgentStore {
	t.Helper()
	s, err := NewAgentStore(AgentStoreOptions{Client: client, Namespace: testNamespace, Now: now})
	if err != nil {
		t.Fatalf("new agent store: %v", err)
	}
	return s
}

func at(base time.Time) func() time.Time { return func() time.Time { return base } }

func readScope() Scope {
	return Scope{Operations: []Operation{OpRead}}
}

// TestCreatedTokenAuthenticatesAndTheStoredObjectCannotProduceIt is the whole
// credential contract in one test: the value works, and the cluster does not
// hold it.
func TestCreatedTokenAuthenticatesAndTheStoredObjectCannotProduceIt(t *testing.T) {
	client := newFakeClient()
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	store := newAgentStore(t, client, at(base))

	agent, token, err := store.Create(t.Context(), AgentSpec{
		Name:  testAgent,
		Scope: Scope{Projects: []string{testProject}, Environments: []string{"development"}, Operations: []Operation{OpMutate}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(token, AgentTokenPrefix+testAgent+".") {
		t.Errorf("token %q does not carry the prefix and the identity name", token)
	}
	if agent.Expires != base.Add(DefaultAgentTTL) {
		t.Errorf("expiry = %s, want %s", agent.Expires, base.Add(DefaultAgentTTL))
	}

	got, err := store.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatalf("authenticate the token just minted: %v", err)
	}
	if got.Name != testAgent || !got.Scope.AllowsEnvironment("development") || got.Scope.AllowsEnvironment("production") {
		t.Errorf("authenticated identity = %+v, want the scope it was created with", got)
	}

	object, err := client.CoreV1().Secrets(testNamespace).Get(t.Context(), agentSecretName(testAgent), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the stored identity: %v", err)
	}
	secretPart := strings.TrimPrefix(token, AgentTokenPrefix+testAgent+".")
	for key, value := range object.Data {
		if strings.Contains(string(value), secretPart) {
			t.Errorf("the stored identity holds the credential itself in %q", key)
		}
	}
	if redact.Scrub("the token is "+token) == "the token is "+token {
		t.Error("the minted token was not registered with internal/redact, so it can reach a log line")
	}
}

// TestAuthenticateRefusesEveryWrongCredentialTheSameWay: a real name with a
// wrong secret must be indistinguishable from a name that never existed, or
// failed authentication becomes a way to enumerate identities.
func TestAuthenticateRefusesEveryWrongCredentialTheSameWay(t *testing.T) {
	client := newFakeClient()
	store := newAgentStore(t, client, at(time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)))
	_, token, err := store.Create(t.Context(), AgentSpec{Name: testAgent, Scope: readScope()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	forged := token[:len(token)-1] + flipLast(token)
	cases := map[string]string{
		"a wrong secret for a real identity": forged,
		"an identity that does not exist":    AgentTokenPrefix + "ghost." + strings.TrimPrefix(token, AgentTokenPrefix+testAgent+"."),
		"not a token at all":                 "hunter2",
		"the prefix and nothing else":        AgentTokenPrefix,
		"a token with no secret":             AgentTokenPrefix + testAgent + ".",
	}
	var messages []string
	for what, candidate := range cases {
		_, err := store.Authenticate(t.Context(), candidate)
		if err == nil {
			t.Fatalf("%s authenticated", what)
		}
		if !AsAgentCredential(err) {
			t.Errorf("%s produced %v, want an agent/invalid-credential", what, err)
		}
		messages = append(messages, err.Error())
	}
	for _, message := range messages[1:] {
		if message != messages[0] {
			t.Errorf("two wrong credentials produced different messages:\n%s\n%s", messages[0], message)
		}
	}
}

func flipLast(token string) string {
	last := token[len(token)-1]
	if last == 'A' {
		return "B"
	}
	return "A"
}

// TestRevokeIsImmediateIdempotentAndLocal: the identity is marked dead in one
// object, revoking twice is not an error, and nothing else in the namespace
// changes.
func TestRevokeIsImmediateIdempotentAndLocal(t *testing.T) {
	client := newFakeClient()
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	store := newAgentStore(t, client, at(base))
	if _, _, err := store.Create(t.Context(), AgentSpec{Name: testAgent, Scope: readScope()}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := store.Create(t.Context(), AgentSpec{Name: "reporter", Scope: readScope()}); err != nil {
		t.Fatalf("create the bystander: %v", err)
	}

	revoked, err := store.Revoke(t.Context(), testAgent)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !revoked.Revoked || !revoked.RevokedAt.Equal(base) {
		t.Errorf("revoked identity = %+v, want revoked at %s", revoked, base)
	}

	again, err := store.Revoke(t.Context(), testAgent)
	if err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
	if !again.RevokedAt.Equal(revoked.RevokedAt) {
		t.Errorf("the second revocation moved the timestamp: %s then %s", revoked.RevokedAt, again.RevokedAt)
	}

	bystander, err := store.Get(t.Context(), "reporter")
	if err != nil {
		t.Fatalf("read the bystander: %v", err)
	}
	if bystander.Revoked {
		t.Error("revoking one identity revoked another")
	}

	if _, err := store.Revoke(t.Context(), "never-existed"); !AsNotFound(err) {
		t.Errorf("revoking an unknown identity = %v, want store/not-found", err)
	}
}

// TestListKeepsRevokedAndExpiredIdentities: a credential that vanished on
// expiry would make "was this action taken by an identity that still exists?"
// unanswerable, which is the opposite of what an audit trail needs (#78).
func TestListKeepsRevokedAndExpiredIdentities(t *testing.T) {
	client := newFakeClient()
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	store := newAgentStore(t, client, at(base))
	for _, name := range []string{"zeta", "alpha"} {
		if _, _, err := store.Create(t.Context(), AgentSpec{Name: name, TTL: time.Hour, Scope: readScope()}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if _, err := store.Revoke(t.Context(), "zeta"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	agents, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(agents) != 2 || agents[0].Name != "alpha" || agents[1].Name != "zeta" {
		t.Fatalf("list = %+v, want alpha then zeta", agents)
	}
	if !agents[1].Revoked {
		t.Error("the revoked identity is listed as live")
	}
	if !agents[0].Expired(base.Add(2 * time.Hour)) {
		t.Error("an identity an hour past its expiry does not report as expired")
	}
	if agents[0].Expired(base.Add(30 * time.Minute)) {
		t.Error("a live identity reports as expired")
	}
}

// TestCreateRefusesWhatItCannotHonour: every refusal is a refusal, never a
// clamp. An operator who asked for a year must learn they did not get one.
func TestCreateRefusesWhatItCannotHonour(t *testing.T) {
	client := newFakeClient()
	store := newAgentStore(t, client, at(time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)))

	cases := map[string]AgentSpec{
		"no operation class": {Name: testAgent},
		"the admin class":    {Name: testAgent, Scope: Scope{Operations: []Operation{OpAdmin}}},
		"an unknown class":   {Name: testAgent, Scope: Scope{Operations: []Operation{Operation("root")}}},
		"a lifetime past the max": {Name: testAgent, TTL: MaxAgentTTL + time.Hour,
			Scope: readScope()},
		"a negative lifetime":                 {Name: testAgent, TTL: -time.Hour, Scope: readScope()},
		"a name that is not a DNS-1123 label": {Name: "Deploy Bot", Scope: readScope()},
		"a project that is not one":           {Name: testAgent, Scope: Scope{Projects: []string{"Shop!"}, Operations: []Operation{OpRead}}},
		"a budget past the max": {Name: testAgent, Scope: readScope(),
			Limit: Limit{RequestsPerMinute: MaxRequestsPerMinute + 1}},
	}
	for what, spec := range cases {
		_, _, err := store.Create(t.Context(), spec)
		if !AsAgentScope(err) {
			t.Errorf("creating with %s = %v, want an agent/invalid-scope", what, err)
		}
	}

	if _, _, err := store.Create(t.Context(), AgentSpec{Name: testAgent, Scope: readScope()}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, err := store.Create(t.Context(), AgentSpec{Name: testAgent, Scope: readScope()})
	if !AsAgentExists(err) {
		t.Errorf("creating a second identity with the same name = %v, want agent/exists", err)
	}
}

// TestCreateFillsInTheDefaults: an issuance that names no lifetime and no
// budget still gets both, because "unset" must never mean "unbounded".
func TestCreateFillsInTheDefaults(t *testing.T) {
	client := newFakeClient()
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	store := newAgentStore(t, client, at(base))

	agent, _, err := store.Create(t.Context(), AgentSpec{Name: testAgent, Scope: readScope()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if agent.Expires != base.Add(DefaultAgentTTL) {
		t.Errorf("expiry = %s, want the default TTL from %s", agent.Expires, base)
	}
	if agent.Limit.RequestsPerMinute != DefaultRequestsPerMinute || agent.Limit.Burst != DefaultBurst {
		t.Errorf("limit = %+v, want the defaults", agent.Limit)
	}

	stored, err := store.Get(t.Context(), testAgent)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Limit != agent.Limit || !stored.Expires.Equal(agent.Expires) {
		t.Errorf("the round trip changed the identity: %+v then %+v", agent, stored)
	}
}

// TestScopeMembership pins the two rules the enforcement in internal/api reads
// off this type: an empty list is every value, and mutate implies read.
func TestScopeMembership(t *testing.T) {
	empty := Scope{Operations: []Operation{OpRead}}
	if !empty.AllowsProject("anything") || !empty.AllowsEnvironment("anything") {
		t.Error("an empty project or environment list is not being read as 'every one'")
	}
	if empty.Restricted() {
		t.Error("a scope naming no project and no environment reports as restricted")
	}

	scoped := Scope{Projects: []string{"shop"}, Environments: []string{"development"}, Operations: []Operation{OpMutate}}
	if !scoped.Restricted() {
		t.Error("a scope naming a project reports as unrestricted")
	}
	if scoped.AllowsEnvironment("production") || scoped.AllowsProject("billing") {
		t.Error("a named list matched something it does not name")
	}
	if scoped.AllowsEnvironment("") {
		t.Error("an unnamed environment matched a restricted scope")
	}
	if !scoped.Allows(OpRead) {
		t.Error("mutate does not imply read")
	}
	if scoped.Allows(OpAdmin) {
		t.Error("mutate implies admin, which would let an agent mint an agent")
	}
	if empty.Allows(OpMutate) {
		t.Error("read implies mutate")
	}
}
