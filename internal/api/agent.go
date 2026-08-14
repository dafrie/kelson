package api

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/serverstate"
)

// AgentService served: issue, list and revoke agent identities (issue #74,
// ADR-0023).
//
// # The token appears once, in one field, and is never logged
//
// CreateAgent is the only RPC in this schema that returns a credential.
// internal/serverstate registers the value with internal/redact the moment it
// mints it, so from before this handler sees it the token cannot reach a log
// line, an error message or an error detail (issue #117) — including the audit
// line the authorization interceptor writes for this very call.
//
// # These three are human-only, enforced elsewhere on purpose
//
// The refusal lives in the scope table (scope.go) and the interceptor that
// reads it, not in these handlers, so it is the same mechanism that refuses an
// out-of-scope deploy rather than a second one written here. An agent
// credential reaching any of these gets `auth/human-only` before the handler
// runs.

// CreateAgent mints an identity and returns its token exactly once.
func (s *Server) CreateAgent(ctx context.Context, req *connect.Request[kelsonv1alpha1.CreateAgentRequest]) (*connect.Response[kelsonv1alpha1.CreateAgentResponse], error) {
	if s.agents == nil {
		return nil, unimplemented("the agent identity store")
	}
	msg := req.Msg
	scope, err := storeScope(msg.GetScope())
	if err != nil {
		return nil, failAgent(err)
	}
	ttl, err := storeTTL(msg.GetTtlSeconds())
	if err != nil {
		return nil, failAgent(err)
	}
	agent, token, err := s.agents.Create(ctx, serverstate.AgentSpec{
		Name:  msg.GetName(),
		TTL:   ttl,
		Scope: scope,
		Limit: serverstate.Limit{
			RequestsPerMinute: int(msg.GetLimit().GetRequestsPerMinute()),
			Burst:             int(msg.GetLimit().GetBurst()),
		},
	})
	if err != nil {
		return nil, failAgent(err)
	}
	return connect.NewResponse(&kelsonv1alpha1.CreateAgentResponse{
		Agent: wireAgent(agent),
		Token: token,
	}), nil
}

// ListAgents reports every identity the server holds, revoked and expired ones
// included. There is no field a credential could travel in.
func (s *Server) ListAgents(ctx context.Context, _ *connect.Request[kelsonv1alpha1.ListAgentsRequest]) (*connect.Response[kelsonv1alpha1.ListAgentsResponse], error) {
	if s.agents == nil {
		return nil, unimplemented("the agent identity store")
	}
	agents, err := s.agents.List(ctx)
	if err != nil {
		return nil, failAgent(err)
	}
	out := make([]*kelsonv1alpha1.AgentIdentity, 0, len(agents))
	for _, agent := range agents {
		out = append(out, wireAgent(agent))
	}
	return connect.NewResponse(&kelsonv1alpha1.ListAgentsResponse{Agents: out}), nil
}

// RevokeAgent kills a credential. It is idempotent, and it writes one object:
// no human session and no other identity is touched.
func (s *Server) RevokeAgent(ctx context.Context, req *connect.Request[kelsonv1alpha1.RevokeAgentRequest]) (*connect.Response[kelsonv1alpha1.RevokeAgentResponse], error) {
	if s.agents == nil {
		return nil, unimplemented("the agent identity store")
	}
	agent, err := s.agents.Revoke(ctx, req.Msg.GetName())
	if err != nil {
		return nil, failAgent(err)
	}
	return connect.NewResponse(&kelsonv1alpha1.RevokeAgentResponse{Agent: wireAgent(agent)}), nil
}

// storeTTL turns the wire's seconds into a Duration, bounded before the
// multiplication rather than after it. A large enough seconds value overflows
// int64 nanoseconds and could land back inside the permitted range as some
// unrelated lifetime, which would be an expiry nobody asked for and nobody
// could see was wrong.
func storeTTL(seconds int64) (time.Duration, error) {
	const maxSeconds = int64(serverstate.MaxAgentTTL / time.Second)
	if seconds < 0 || seconds > maxSeconds {
		return 0, authzError{
			Code:        string(serverstate.ErrAgentScope),
			Resource:    "agent/scope",
			Message:     fmt.Sprintf("a lifetime of %d seconds is outside the permitted range (0 < ttl <= %d)", seconds, maxSeconds),
			Remediation: "ask for a shorter lifetime and rotate: create the successor identity, then revoke this one",
		}
	}
	return time.Duration(seconds) * time.Second, nil
}

// storeScope translates the wire scope. An unspecified operation is refused
// here rather than dropped: a client that sent one meant something by it, and
// silently narrowing the grant would be the quiet success this project rejects.
func storeScope(scope *kelsonv1alpha1.AgentScope) (serverstate.Scope, error) {
	out := serverstate.Scope{
		Projects:     scope.GetProjects(),
		Environments: scope.GetEnvironments(),
	}
	for _, op := range scope.GetOperations() {
		switch op {
		case kelsonv1alpha1.AgentOperation_AGENT_OPERATION_READ:
			out.Operations = append(out.Operations, serverstate.OpRead)
		case kelsonv1alpha1.AgentOperation_AGENT_OPERATION_MUTATE:
			out.Operations = append(out.Operations, serverstate.OpMutate)
		default:
			return serverstate.Scope{}, authzError{
				Code:        string(serverstate.ErrAgentScope),
				Resource:    "agent/scope",
				Message:     "the scope names an unrecognised operation class",
				Remediation: "grant AGENT_OPERATION_READ, or READ and MUTATE",
			}
		}
	}
	return out, nil
}

// wireAgent projects an identity onto the wire. Timestamps are Unix
// milliseconds, matching EventService rather than the history entries' RFC 3339
// strings, because these are compared and sorted by clients rather than shown.
func wireAgent(agent serverstate.Agent) *kelsonv1alpha1.AgentIdentity {
	out := &kelsonv1alpha1.AgentIdentity{
		Name:          agent.Name,
		CreatedUnixMs: agent.Created.UnixMilli(),
		ExpiresUnixMs: agent.Expires.UnixMilli(),
		Revoked:       agent.Revoked,
		Scope: &kelsonv1alpha1.AgentScope{
			Projects:     agent.Scope.Projects,
			Environments: agent.Scope.Environments,
			Operations:   wireOperations(agent.Scope.Operations),
		},
		Limit: &kelsonv1alpha1.AgentLimit{
			RequestsPerMinute: int32(agent.Limit.RequestsPerMinute), //nolint:gosec // bounded by serverstate.MaxRequestsPerMinute
			Burst:             int32(agent.Limit.Burst),             //nolint:gosec // bounded by serverstate.MaxRequestsPerMinute
		},
	}
	if agent.Revoked {
		out.RevokedUnixMs = agent.RevokedAt.UnixMilli()
	}
	return out
}

func wireOperations(ops []serverstate.Operation) []kelsonv1alpha1.AgentOperation {
	out := make([]kelsonv1alpha1.AgentOperation, 0, len(ops))
	for _, op := range ops {
		switch op {
		case serverstate.OpRead:
			out = append(out, kelsonv1alpha1.AgentOperation_AGENT_OPERATION_READ)
		case serverstate.OpMutate:
			out = append(out, kelsonv1alpha1.AgentOperation_AGENT_OPERATION_MUTATE)
		default:
			out = append(out, kelsonv1alpha1.AgentOperation_AGENT_OPERATION_UNSPECIFIED)
		}
	}
	return out
}

// failAgent maps an identity-store refusal onto its ConnectRPC code. A name
// already taken is a failed precondition rather than an invalid argument: the
// request is well-formed and the world is what refuses it, which is the same
// split failSecret makes.
func failAgent(err error) error {
	switch {
	case serverstate.AsAgentExists(err):
		return fail(connect.CodeAlreadyExists, err)
	case serverstate.AsAgentScope(err):
		return fail(connect.CodeInvalidArgument, err)
	case serverstate.AsAgentCredential(err):
		return fail(connect.CodeUnauthenticated, err)
	default:
		return failRequest(err)
	}
}
