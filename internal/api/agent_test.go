package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/controlstore"
)

// AgentService's own tests. The interceptor's refusals are in authz_test.go;
// what is asserted here is the translation between the wire and the store, and
// the one property the schema is shaped to guarantee — that a credential leaves
// the server exactly once.

func agentScope(ops ...kelsonv1alpha1.AgentOperation) *kelsonv1alpha1.AgentScope {
	return &kelsonv1alpha1.AgentScope{Operations: ops}
}

// TestCreateAgentTranslatesTheScopeAndReturnsTheTokenOnce.
func TestCreateAgentTranslatesTheScopeAndReturnsTheTokenOnce(t *testing.T) {
	g := newGatedServer(t, Options{})
	human := g.agentsClient(testPassword)

	created, err := human.CreateAgent(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateAgentRequest{
		Name:       "deploybot",
		TtlSeconds: 3600,
		Scope: &kelsonv1alpha1.AgentScope{
			Projects:     []string{"shop"},
			Environments: []string{devEnv},
			Operations:   []kelsonv1alpha1.AgentOperation{kelsonv1alpha1.AgentOperation_AGENT_OPERATION_MUTATE},
		},
		Limit: &kelsonv1alpha1.AgentLimit{RequestsPerMinute: 30, Burst: 5},
	}))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if created.Msg.GetToken() == "" {
		t.Fatal("CreateAgent returned no token")
	}
	identity := created.Msg.GetAgent()
	if identity.GetName() != "deploybot" {
		t.Errorf("name = %q", identity.GetName())
	}
	if identity.GetExpiresUnixMs()-identity.GetCreatedUnixMs() != 3600*1000 {
		t.Errorf("the lifetime is %dms, want one hour", identity.GetExpiresUnixMs()-identity.GetCreatedUnixMs())
	}
	if got := identity.GetScope().GetOperations(); len(got) != 1 || got[0] != kelsonv1alpha1.AgentOperation_AGENT_OPERATION_MUTATE {
		t.Errorf("operations = %v", got)
	}
	if got := identity.GetLimit(); got.GetRequestsPerMinute() != 30 || got.GetBurst() != 5 {
		t.Errorf("limit = %v, want the budget the request asked for", got)
	}

	// The token exists once. The listing carries the identity and, by the shape
	// of the schema, cannot carry a credential — there is no field for one.
	listed, err := human.ListAgents(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(listed.Msg.GetAgents()) != 1 || listed.Msg.GetAgents()[0].GetName() != "deploybot" {
		t.Fatalf("the listing = %v", listed.Msg.GetAgents())
	}

	revoked, err := human.RevokeAgent(t.Context(), connect.NewRequest(&kelsonv1alpha1.RevokeAgentRequest{Name: "deploybot"}))
	if err != nil {
		t.Fatalf("RevokeAgent: %v", err)
	}
	if !revoked.Msg.GetAgent().GetRevoked() || revoked.Msg.GetAgent().GetRevokedUnixMs() == 0 {
		t.Errorf("the revoked identity = %v", revoked.Msg.GetAgent())
	}
}

// TestCreateAgentRefusesWhatItCannotHonour: the wire's own refusals, before the
// store's. An unrecognised operation and a lifetime that would overflow are the
// two a client can reach that the CLI cannot.
func TestCreateAgentRefusesWhatItCannotHonour(t *testing.T) {
	g := newGatedServer(t, Options{})
	human := g.agentsClient(testPassword)

	cases := map[string]*kelsonv1alpha1.CreateAgentRequest{
		"an unrecognised operation class": {
			Name:  "deploybot",
			Scope: agentScope(kelsonv1alpha1.AgentOperation_AGENT_OPERATION_UNSPECIFIED),
		},
		"a lifetime past the maximum": {
			Name:       "deploybot",
			TtlSeconds: int64(controlstore.MaxAgentTTL.Seconds()) + 1,
			Scope:      agentScope(kelsonv1alpha1.AgentOperation_AGENT_OPERATION_READ),
		},
		"a lifetime that would overflow a Duration": {
			Name:       "deploybot",
			TtlSeconds: 1 << 62,
			Scope:      agentScope(kelsonv1alpha1.AgentOperation_AGENT_OPERATION_READ),
		},
		"a negative lifetime": {
			Name:       "deploybot",
			TtlSeconds: -1,
			Scope:      agentScope(kelsonv1alpha1.AgentOperation_AGENT_OPERATION_READ),
		},
	}
	for what, req := range cases {
		_, err := human.CreateAgent(t.Context(), connect.NewRequest(req))
		if authCode(err) != connect.CodeInvalidArgument {
			t.Errorf("creating with %s = %v (code %s), want invalid-argument", what, err, authCode(err))
		}
	}
}

// TestAgentServiceWithoutAStoreIsUnimplemented: a server built without the seam
// says so rather than panicking or pretending it has no identities.
func TestAgentServiceWithoutAStoreIsUnimplemented(t *testing.T) {
	server := New(Options{})
	mux := http.NewServeMux()
	server.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := kelsonv1alpha1connect.NewAgentServiceClient(srv.Client(), srv.URL)
	_, err := client.ListAgents(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListAgentsRequest{}))
	if authCode(err) != connect.CodeUnimplemented {
		t.Errorf("ListAgents on a server with no identity store = %v, want unimplemented", err)
	}
}
