package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/serverstate"
)

// Server-side authorization for agent principals (issue #74, ADR-0023 §3).
//
// # Why an interceptor and not a check in each handler
//
// A check a handler performs is a check a handler can forget. This one runs
// before every registered method, reads its rule from the one table in scope.go,
// and refuses anything the table does not cover — so the failure mode of
// forgetting is a refused RPC rather than an open one.
//
// # What it enforces, in order
//
//  1. The method has a row. No row is a refusal, for every caller.
//  2. The credential is live: not expired, not revoked. Both are read from
//     cluster state during this request (auth.go does the read); nothing about
//     an identity is cached, which is what makes revocation immediate.
//  3. Administrative methods are refused to agents outright.
//  4. The operation class is granted (read / mutate).
//  5. The identity is within its request budget (ratelimit.go).
//  6. The target — (project, environment) — is inside the scope.
//
// A human or an anonymous caller stops at step 1: the password path of #84 is
// untouched by any of this, which is the other half of #74's acceptance
// criterion. Revoking an agent cannot lock a person out because a person's
// credential is never consulted here.
//
// # The target check and streaming
//
// The target lives in the request message, which for a server-streaming RPC
// (Deploy, Rollback, Build, Watch, FollowLogs) is not decoded until the handler
// receives it. So the stream's connection is wrapped and the check runs on the
// first Receive, before the handler has done anything with the message. Deploy
// is a server-streaming method and is exactly the one the acceptance criterion
// names, so this is not an edge case — it is the main path.

// Authorization error codes. They travel on the wire as the structured error's
// `code` (ADR-0013 §2), so an agent branches on them rather than on prose.
const (
	// ErrUnmappedMethod is a registered RPC with no row in the scope table. It
	// is refused to everyone; the coverage test exists so this is never seen in
	// production.
	ErrUnmappedMethod = "auth/unmapped-method"
	// ErrOutOfScope is a credential reaching outside its projects,
	// environments or operation classes.
	ErrOutOfScope = "auth/out-of-scope"
	// ErrTargetUnknown is a request whose target cannot be derived from its
	// shape, made by a credential that is restricted and therefore cannot be
	// given the benefit of the doubt.
	ErrTargetUnknown = "auth/target-unknown"
	// ErrCredentialExpired is a credential past its expiry.
	ErrCredentialExpired = "auth/expired"
	// ErrCredentialRevoked is a revoked identity.
	ErrCredentialRevoked = "auth/revoked"
	// ErrHumanOnly is an agent credential attempting an administrative method.
	ErrHumanOnly = "auth/human-only"
	// ErrRateLimited is an identity over its request budget.
	ErrRateLimited = "auth/rate-limited"
)

const authDocsBase = "https://kelson.dev/server/agents"

// authzError is one authorization refusal, in the same shape every other plane's
// errors have so errors.go can project it onto the wire without a second
// taxonomy.
type authzError struct {
	Code        string
	Resource    string
	Message     string
	Remediation string
}

func (e authzError) Error() string {
	return fmt.Sprintf("%s [%s] %s: %s", e.Resource, e.Code, e.Message, e.Remediation)
}

func (e authzError) wire() *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        e.Code,
		Resource:    e.Resource,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     authDocsBase + "/" + e.Code,
	}
}

// denied is the refusal every authorization failure but the rate limit becomes.
func denied(e authzError) error { return fail(connect.CodePermissionDenied, e) }

// exhausted is what a breached rate limit becomes: resource-exhausted, which is
// the code a client is expected to back off on rather than to treat as a
// permanent refusal.
func exhausted(e authzError) error { return fail(connect.CodeResourceExhausted, e) }

// authorizer is the interceptor. It holds no per-request state; the buckets in
// `limits` are per identity and outlive any one call.
type authorizer struct {
	now    func() time.Time
	logger *slog.Logger
	limits *limiter
}

var _ connect.Interceptor = (*authorizer)(nil)

func newAuthorizer(now func() time.Time, logger *slog.Logger) *authorizer {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &authorizer{now: now, logger: logger, limits: newLimiter()}
}

// WrapUnary gates a unary RPC. The message is already decoded, so the whole
// decision is made before the handler runs.
func (a *authorizer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		p := principalOf(ctx)
		procedure := req.Spec().Procedure
		row, err := a.admit(p, procedure)
		if err != nil {
			return nil, err
		}
		if err := a.checkTarget(p, procedure, row, req.Any()); err != nil {
			return nil, err
		}
		a.record(p, procedure, "allowed", nil)
		return next(ctx, req)
	}
}

// WrapStreamingHandler gates a streaming RPC: everything knowable without the
// message up front, and the target on the first Receive.
func (a *authorizer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		p := principalOf(ctx)
		procedure := conn.Spec().Procedure
		row, err := a.admit(p, procedure)
		if err != nil {
			return err
		}
		if p.Type != PrincipalAgent {
			a.record(p, procedure, "allowed", nil)
			return next(ctx, conn)
		}
		return next(ctx, &scopedConn{
			StreamingHandlerConn: conn,
			check: func(msg any) error {
				if err := a.checkTarget(p, procedure, row, msg); err != nil {
					return err
				}
				a.record(p, procedure, "allowed", nil)
				return nil
			},
		})
	}
}

// WrapStreamingClient is the client half of the interface and is never used:
// this interceptor guards handlers.
func (a *authorizer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// scopedConn runs the target check on the first message a streaming handler
// receives. Returning an error from Receive is how the refusal reaches the
// client: connect's generated server-stream handler returns it before calling
// the implementation, so nothing the RPC would have done has happened yet.
type scopedConn struct {
	connect.StreamingHandlerConn
	check   func(msg any) error
	checked bool
}

func (c *scopedConn) Receive(msg any) error {
	if err := c.StreamingHandlerConn.Receive(msg); err != nil {
		return err
	}
	if c.checked {
		return nil
	}
	c.checked = true
	return c.check(msg)
}

// admit runs every check that does not need the request message, and records
// the outcome. It returns the table row so the caller can finish the decision.
func (a *authorizer) admit(p Principal, procedure string) (methodScope, error) {
	row, ok := scopeFor(procedure)
	if !ok {
		// Fail closed, for humans too. An RPC with no row is an RPC nobody
		// decided the rules for, and the honest answer to "may I?" is no.
		err := denied(authzError{
			Code:     ErrUnmappedMethod,
			Resource: procedure,
			Message:  "this method has no entry in kelson's RPC scope table, so the server cannot say who may call it",
			Remediation: "add a row to rpcScopes in internal/api/scope.go naming the method's operation class and reach; " +
				"the coverage test in scope_coverage_test.go fails until it is there (issue #74)",
		})
		a.record(p, procedure, "unmapped", err)
		return methodScope{}, err
	}

	if p.Type != PrincipalAgent {
		// The human and anonymous paths end here, unchanged by #74.
		return row, nil
	}

	agent := p.Agent
	now := a.now()
	switch {
	case agent.Revoked:
		err := denied(authzError{
			Code:        ErrCredentialRevoked,
			Resource:    "agent/" + agent.Name,
			Message:     fmt.Sprintf("agent identity %q was revoked at %s", agent.Name, agent.RevokedAt.Format(time.RFC3339)),
			Remediation: "ask an operator for a new identity: `kelson agent create`. Revocation is immediate and cannot be undone",
		})
		a.record(p, procedure, "revoked", err)
		return methodScope{}, err
	case agent.Expired(now):
		err := denied(authzError{
			Code:        ErrCredentialExpired,
			Resource:    "agent/" + agent.Name,
			Message:     fmt.Sprintf("the credential for agent identity %q expired at %s", agent.Name, agent.Expires.Format(time.RFC3339)),
			Remediation: "rotate: create the successor identity and revoke this one (`kelson agent create` then `kelson agent revoke`)",
		})
		a.record(p, procedure, "expired", err)
		return methodScope{}, err
	case row.Operation == serverstate.OpAdmin:
		err := denied(authzError{
			Code:     ErrHumanOnly,
			Resource: procedure,
			Message:  "issuing, listing and revoking agent identities is reserved to a human principal",
			Remediation: "run `kelson agent create|list|revoke` with a kube context, or call AgentService with the server password; " +
				"an agent that could mint an agent could mint one wider than itself (ADR-0023)",
		})
		a.record(p, procedure, "human-only", err)
		return methodScope{}, err
	case !agent.Scope.Allows(row.Operation):
		err := denied(authzError{
			Code:     ErrOutOfScope,
			Resource: procedure,
			Message: fmt.Sprintf("agent identity %q is not granted the %s operation class (scope: %s)",
				agent.Name, row.Operation, describeScope(agent.Scope)),
			Remediation: "issue a credential granting that class, or use a method the identity's classes cover",
		})
		a.record(p, procedure, "out-of-scope", err)
		return methodScope{}, err
	}

	if !a.limits.allow(agent.Name, agent.Limit.RequestsPerMinute, agent.Limit.Burst, now) {
		err := exhausted(authzError{
			Code:     ErrRateLimited,
			Resource: "agent/" + agent.Name,
			Message: fmt.Sprintf("agent identity %q is over its request budget of %d/minute (burst %d)",
				agent.Name, agent.Limit.RequestsPerMinute, agent.Limit.Burst),
			Remediation: "back off and retry; the budget refills continuously. An agent that needs a larger one is re-issued with a higher --rate",
		})
		a.record(p, procedure, "rate-limited", err)
		return methodScope{}, err
	}
	return row, nil
}

// checkTarget is the scope check that needs the request message. It is a no-op
// for anything but an agent — see admit.
func (a *authorizer) checkTarget(p Principal, procedure string, row methodScope, msg any) error {
	if p.Type != PrincipalAgent {
		return nil
	}
	agent := p.Agent
	restricted := agent.Scope.Restricted()

	switch row.Reach {
	case reachClusterWide:
		// No project is named in the question or in the answer.
	case reachEveryProject:
		if restricted {
			err := denied(authzError{
				Code:     ErrOutOfScope,
				Resource: procedure,
				Message: fmt.Sprintf("%s spans every project, and agent identity %q is scoped to %s",
					procedure, agent.Name, describeScope(agent.Scope)),
				Remediation: "call the project-addressed method instead (GetSpec names one project), or use an unscoped credential",
			})
			a.record(p, procedure, "out-of-scope", err)
			return err
		}
	case reachNamespace:
		if !restricted {
			break
		}
		var namespace string
		ok := false
		if row.Namespace != nil {
			namespace, ok = row.Namespace(msg)
		}
		if !ok {
			return a.unknownTarget(p, procedure, "the request names no namespace")
		}
		if !allowsAny(agent.Scope, namespaceTargets(namespace)) {
			err := denied(authzError{
				Code:     ErrOutOfScope,
				Resource: procedure,
				Message: fmt.Sprintf("namespace %q is not one agent identity %q may read (scope: %s)",
					namespace, agent.Name, describeScope(agent.Scope)),
				Remediation: "a scoped credential reaches a namespace only through the renderer's `<project>-<environment>` convention; " +
					"name a namespace of a project and environment the identity is scoped to",
			})
			a.record(p, procedure, "out-of-scope", err)
			return err
		}
	case reachTargeted:
		if !restricted {
			break
		}
		var targets []scopeTarget
		ok := false
		if row.Targets != nil {
			targets, ok = row.Targets(msg)
		}
		if !ok || len(targets) == 0 {
			return a.unknownTarget(p, procedure,
				"the request does not name the project and environment it acts on (an inline spec carries them inside the document)")
		}
		for _, t := range targets {
			if err := a.allows(p, procedure, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// allows is the per-target check. An environment the request did not name is
// refused for an environment-restricted credential rather than read as "all of
// them", which is the difference between a scope and a suggestion.
func (a *authorizer) allows(p Principal, procedure string, t scopeTarget) error {
	scope := p.Agent.Scope
	inScope := scope.AllowsProject(t.Project) &&
		(t.Environment != "" || len(scope.Environments) == 0) &&
		scope.AllowsEnvironment(t.Environment)
	if inScope {
		return nil
	}
	err := denied(authzError{
		Code:     ErrOutOfScope,
		Resource: procedure,
		Message: fmt.Sprintf("agent identity %q may not act on %s (scope: %s)",
			p.Agent.Name, t.String(), describeScope(scope)),
		Remediation: "name a project and environment the identity is scoped to, or ask an operator for a credential that covers this one",
	})
	a.record(p, procedure, "out-of-scope", err)
	return err
}

// allowsAny reports whether any candidate target is in scope. It is the
// namespace rule: several (project, environment) splits can produce the same
// namespace and one match is enough.
func allowsAny(scope serverstate.Scope, targets []scopeTarget) bool {
	for _, t := range targets {
		if scope.AllowsProject(t.Project) && t.Environment != "" && scope.AllowsEnvironment(t.Environment) {
			return true
		}
	}
	return false
}

func (a *authorizer) unknownTarget(p Principal, procedure, why string) error {
	err := denied(authzError{
		Code:     ErrTargetUnknown,
		Resource: procedure,
		Message: fmt.Sprintf("%s: %s, so a credential scoped to %s cannot be checked against it",
			procedure, why, describeScope(p.Agent.Scope)),
		Remediation: "address a stored spec by project name rather than inline, name the environment explicitly, " +
			"or use a credential that is not scoped to particular projects or environments",
	})
	a.record(p, procedure, "target-unknown", err)
	return err
}

// record is the audit attribution seam (#78). Every authenticated request that
// reaches the API leaves one line naming the principal, its type, the method
// and the outcome — which is the minimum an audit trail needs and the maximum
// this issue builds. It deliberately logs no request payload: the one RPC that
// carries secret values would put them here (issue #117).
func (a *authorizer) record(p Principal, procedure, outcome string, err error) {
	attrs := []any{
		slog.String("principal", p.Subject()),
		slog.String("principal_type", string(p.Type)),
		slog.String("procedure", procedure),
		slog.String("outcome", outcome),
	}
	if err == nil {
		a.logger.Info("rpc", attrs...)
		return
	}
	attrs = append(attrs, slog.String("code", connect.CodeOf(err).String()))
	a.logger.Warn("rpc refused", attrs...)
}
