package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/serverstate"
)

// `kelson agent` is the surface that mints credentials, so the assertions here
// are about what reaches the store and what reaches the terminal: the scope the
// operator asked for, and a token printed once with the fact that it is once.
//
// The store is faked for the reason every command test here fakes its plane —
// the real one needs a cluster, and it has its own tests against a fake
// clientset in internal/serverstate.

const testToken = "kagt.deploybot.TOKEN-SENTINEL-4d9a"

type fakeAgentStore struct {
	created []serverstate.AgentSpec
	revoked []string
	agents  []serverstate.Agent
	err     error
}

func (f *fakeAgentStore) Create(_ context.Context, spec serverstate.AgentSpec) (serverstate.Agent, string, error) {
	f.created = append(f.created, spec)
	if f.err != nil {
		return serverstate.Agent{}, "", f.err
	}
	ttl := spec.TTL
	if ttl == 0 {
		ttl = serverstate.DefaultAgentTTL
	}
	created := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	return serverstate.Agent{
		Name:    spec.Name,
		Created: created,
		Expires: created.Add(ttl),
		Scope:   spec.Scope,
		Limit:   serverstate.Limit{RequestsPerMinute: serverstate.DefaultRequestsPerMinute, Burst: serverstate.DefaultBurst},
	}, testToken, nil
}

func (f *fakeAgentStore) List(context.Context) ([]serverstate.Agent, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.agents, nil
}

func (f *fakeAgentStore) Revoke(_ context.Context, name string) (serverstate.Agent, error) {
	f.revoked = append(f.revoked, name)
	if f.err != nil {
		return serverstate.Agent{}, f.err
	}
	return serverstate.Agent{
		Name:      name,
		Revoked:   true,
		RevokedAt: time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeAgentStore) connector() agentConnector {
	return func(string, string) (agentStore, error) { return f, nil }
}

func runAgent(t *testing.T, store *fakeAgentStore, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	root := &cobra.Command{Use: "kelson", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newAgentCmdWith(store.connector()))
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), code, msg
}

// TestAgentCreatePassesTheScopeThroughAndPrintsTheTokenOnce: the flags are the
// operator's whole statement of what the agent may do, so they must arrive at
// the store unchanged, and the credential must be printed with the fact that
// this is the only time it exists.
func TestAgentCreatePassesTheScopeThroughAndPrintsTheTokenOnce(t *testing.T) {
	store := &fakeAgentStore{}
	out, code, msg := runAgent(t, store, "agent", "create", "deploybot",
		"--project", "shop", "--env", "development", "--allow", "mutate", "--ttl", "2h")
	if code != 0 {
		t.Fatalf("create exited %d: %s", code, msg)
	}
	if len(store.created) != 1 {
		t.Fatalf("the store received %d issuances, want 1", len(store.created))
	}
	spec := store.created[0]
	if spec.Name != "deploybot" || spec.TTL != 2*time.Hour {
		t.Errorf("the issuance = %+v, want deploybot with a two-hour lifetime", spec)
	}
	if len(spec.Scope.Projects) != 1 || spec.Scope.Projects[0] != "shop" {
		t.Errorf("projects = %v, want [shop]", spec.Scope.Projects)
	}
	if len(spec.Scope.Environments) != 1 || spec.Scope.Environments[0] != "development" {
		t.Errorf("environments = %v, want [development]", spec.Scope.Environments)
	}
	if len(spec.Scope.Operations) != 1 || spec.Scope.Operations[0] != serverstate.OpMutate {
		t.Errorf("operations = %v, want [mutate]", spec.Scope.Operations)
	}

	if !strings.Contains(out, testToken) {
		t.Errorf("the token was not printed:\n%s", out)
	}
	if !strings.Contains(out, "shown once") {
		t.Errorf("the output does not say the token is shown once:\n%s", out)
	}
	if !strings.Contains(out, "KELSON_AGENT_TOKEN") {
		t.Errorf("the output does not say how to give the token to an agent:\n%s", out)
	}
}

// TestAgentCreateDefaultsToReadOnly: the default has to be the narrow end. An
// operator who forgot --allow must not have handed out a deploy credential.
func TestAgentCreateDefaultsToReadOnly(t *testing.T) {
	store := &fakeAgentStore{}
	if _, code, msg := runAgent(t, store, "agent", "create", "reporter"); code != 0 {
		t.Fatalf("create exited %d: %s", code, msg)
	}
	ops := store.created[0].Scope.Operations
	if len(ops) != 1 || ops[0] != serverstate.OpRead {
		t.Errorf("the default grant = %v, want [read]", ops)
	}
	if store.created[0].TTL != serverstate.DefaultAgentTTL {
		t.Errorf("the default lifetime = %s, want %s", store.created[0].TTL, serverstate.DefaultAgentTTL)
	}
}

// TestAgentCreateQuietPrintsTheTokenAlone: the flag exists so a token can be
// captured into an environment variable without a shell doing surgery on prose.
func TestAgentCreateQuietPrintsTheTokenAlone(t *testing.T) {
	store := &fakeAgentStore{}
	out, code, msg := runAgent(t, store, "agent", "create", "deploybot", "--quiet")
	if code != 0 {
		t.Fatalf("create exited %d: %s", code, msg)
	}
	if strings.TrimSpace(out) != testToken {
		t.Errorf("--quiet printed more than the token:\n%q", out)
	}
}

// TestAgentCreateRefusesAnUnknownOperationClass: a typo that silently granted
// nothing would produce an identity that fails on its first call for a reason
// nothing explained.
func TestAgentCreateRefusesAnUnknownOperationClass(t *testing.T) {
	store := &fakeAgentStore{}
	_, code, msg := runAgent(t, store, "agent", "create", "deploybot", "--allow", "admin")
	if code == 0 {
		t.Fatal("--allow admin was accepted, which would be an agent that can mint agents")
	}
	if !strings.Contains(msg, "read or mutate") {
		t.Errorf("the refusal does not name the classes that exist: %s", msg)
	}
	if len(store.created) != 0 {
		t.Error("a refused issuance still reached the store")
	}
}

// TestAgentListReportsStateAndNeverACredential.
func TestAgentListReportsStateAndNeverACredential(t *testing.T) {
	base := time.Now()
	store := &fakeAgentStore{agents: []serverstate.Agent{
		{
			Name: "deploybot", Expires: base.Add(time.Hour),
			Scope: serverstate.Scope{Projects: []string{"shop"}, Operations: []serverstate.Operation{serverstate.OpMutate}},
			Limit: serverstate.Limit{RequestsPerMinute: 120, Burst: 30},
		},
		{Name: "gone", Expires: base.Add(time.Hour), Revoked: true, RevokedAt: base},
		{Name: "stale", Expires: base.Add(-time.Hour)},
	}}
	out, code, msg := runAgent(t, store, "agent", "list")
	if code != 0 {
		t.Fatalf("list exited %d: %s", code, msg)
	}
	for _, want := range []string{"deploybot", "(live)", "gone", "(revoked", "stale", "(expired)", "shop", "mutate"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testToken) || strings.Contains(out, "kagt.") {
		t.Errorf("the listing printed something that looks like a credential:\n%s", out)
	}
}

// TestAgentRevokeNamesTheIdentityAndSaysItIsImmediate: an operator revoking a
// credential in an incident needs to know whether they still have to do
// something else.
func TestAgentRevokeNamesTheIdentityAndSaysItIsImmediate(t *testing.T) {
	store := &fakeAgentStore{}
	out, code, msg := runAgent(t, store, "agent", "revoke", "deploybot")
	if code != 0 {
		t.Fatalf("revoke exited %d: %s", code, msg)
	}
	if len(store.revoked) != 1 || store.revoked[0] != "deploybot" {
		t.Errorf("the store was asked to revoke %v", store.revoked)
	}
	if !strings.Contains(out, "deploybot") || !strings.Contains(out, "next request") {
		t.Errorf("the confirmation does not say what happened and when:\n%s", out)
	}
	if !strings.Contains(out, "nothing else was touched") {
		t.Errorf("the confirmation does not say no one else was affected:\n%s", out)
	}
}
