package api

import (
	"context"

	"github.com/dafrie/kelson/internal/serverstate"
)

// Who is making this request (issue #74, ADR-0023).
//
// The gate in auth.go answers it once, at the edge, and puts the answer in the
// request's context. Everything downstream — the authorization interceptor, the
// rate limiter, the audit line — reads that one value rather than re-deriving
// it from headers, so there is exactly one place where a credential becomes a
// principal and exactly one place a future credential type has to be taught
// about.

// PrincipalType separates the two kinds of caller the server has, plus the
// absence of one. It is a string rather than an int because it is written into
// audit records that outlive this process (#78) and a number there would need a
// key nobody would keep.
type PrincipalType string

const (
	// PrincipalHuman is the shared-password path: a browser session or a
	// bearer password (#84's interim cut). It carries a display name when the
	// caller logged in through the UI and none when it sent the password
	// directly, because a shared password names nobody.
	PrincipalHuman PrincipalType = "human"

	// PrincipalAgent is an agent identity: named, scoped and expiring.
	PrincipalAgent PrincipalType = "agent"

	// PrincipalAnonymous is a server started without a password, where every
	// route is open. It exists so an audit line for that server says
	// "anonymous" rather than claiming a human was there.
	PrincipalAnonymous PrincipalType = "anonymous"
)

// Principal is the authenticated caller of one request.
type Principal struct {
	Type PrincipalType
	// Name is the display name of a human session or the identity name of an
	// agent. Empty for a bearer-password caller and for anonymous.
	Name string
	// Agent is the stored identity, present only for [PrincipalAgent]. It is
	// the value read from the cluster during *this* request — never a cached
	// one — which is what makes revocation immediate.
	Agent serverstate.Agent
}

// Subject is the principal as one audit-safe string: "agent:deploybot",
// "human:ada", "human" for a bearer-password caller, "anonymous" for none.
func (p Principal) Subject() string {
	if p.Type == "" {
		return string(PrincipalAnonymous)
	}
	if p.Name == "" {
		return string(p.Type)
	}
	return string(p.Type) + ":" + p.Name
}

// principalKey is the context key. It is an unexported type so no other package
// can write a principal into a context and grant itself one.
type principalKey struct{}

// WithPrincipal returns ctx carrying p. It is exported because
// cmd/kelson-server composes the gate and the handlers, and because a test that
// drives a handler directly needs to state who is calling.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal ctx carries. The bool is false on a
// server with no gate at all, which every caller treats as anonymous rather
// than as an error: an open server is a posture, not a failure.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// principalOf is PrincipalFrom with the absent case resolved, for the callers
// that only want to know what to enforce.
func principalOf(ctx context.Context) Principal {
	if p, ok := PrincipalFrom(ctx); ok {
		return p
	}
	return Principal{Type: PrincipalAnonymous}
}
