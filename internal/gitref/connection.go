package gitref

import (
	"context"
	"fmt"

	"github.com/dafrie/kelson/internal/forgeconn"
)

// Which credential reads *this* repository (ADR-0033 decisions 4 and 5).
//
// # Why [Auth] was not enough
//
// [Auth] answers "what credential", and until connections existed that was the
// whole question: there was one, it came out of `KELSON_GIT_TOKEN`, and it was
// used for every remote the process read. A connection is scoped to a forge, so
// the question grew an argument — *which* repository — and a field holding one
// credential cannot express it. [AuthSource] is that argument, and [Fixed] is
// the old behaviour expressed in the new shape rather than kept beside it.
//
// # Resolution failures are refusals, not anonymous reads
//
// A source no connection covers is read anonymously, which is correct for a
// public repository and is [forgeconn]'s ordinary "false" answer. Two
// connections covering one repository, or a `source.connection` naming one that
// does not exist, are different: they are the author having said something
// kelson cannot act on, so they surface as the structured refusal
// [forgeconn.Resolver.Resolve] built — naming the field that fixes them —
// rather than as a quiet fall back to anonymous that fails later at the remote
// with a 404 nobody can trace back to a spec.

// AuthSource picks the credential one repository is read with.
//
// It is an interface with two implementations rather than a function type
// because the two answers are worth naming: [Fixed] is "the same credential for
// everything", which is what a process-wide token is, and [Connections] is
// "whatever the project's connection mints", which is what ADR-0033 replaces it
// with.
type AuthSource interface {
	// AuthFor returns the credential for one remote. Returning
	// [Anonymous] is a real answer — a public repository needs no credential —
	// and is never an error.
	AuthFor(ctx context.Context, repo string) (Auth, error)
}

// Fixed answers with one credential for every repository.
//
// It exists so a caller that genuinely has one credential — the CLI reading
// `KELSON_GIT_TOKEN` out of the user's own environment, a test — keeps a
// one-line wiring, and so that [RemoteResolver] has exactly one code path
// instead of a nil check around the interesting one.
type Fixed struct{ Auth Auth }

// AuthFor implements [AuthSource].
func (f Fixed) AuthFor(context.Context, string) (Auth, error) {
	if f.Auth == nil {
		return Anonymous{}, nil
	}
	return f.Auth, nil
}

// Connections resolves the project's GitConnection and mints from it
// (ADR-0033 decision 4).
//
// One value serves one project, because [Connection] is that project's
// `spec.source.connection` — the explicit override the matcher honours before
// it considers hosts. A caller serving several projects builds one of these per
// project rather than sharing one and hoping the override is right; the
// [forgeconn.Resolver] behind it is the shared, concurrency-safe part.
type Connections struct {
	// Resolver reads the instance's connections and mints from them. Nil is a
	// source that always answers anonymously, which is what a server with no
	// cluster access can honestly say.
	Resolver *forgeconn.Resolver

	// Connection is `Project.spec.source.connection`, or empty to resolve by
	// host match. An empty string is the common case: one connection means zero
	// configuration (ADR-0033 decision 4).
	Connection string
}

// AuthFor implements [AuthSource].
//
// The minted credential is a [Token] because that is what an HTTPS remote read
// takes, whichever shape produced it: a GitHub App installation token and a
// pasted PAT are both a basic-auth pair by the time they reach go-git, and the
// difference between them — one expires within the hour — is the forge
// adapter's to manage and not this transport's.
func (c Connections) AuthFor(ctx context.Context, repo string) (Auth, error) {
	if c.Resolver == nil {
		return Anonymous{}, nil
	}
	cred, res, ok, err := c.Resolver.Credential(ctx, repo, c.Connection)
	if err != nil {
		return nil, err
	}
	if !ok {
		return Anonymous{}, nil
	}
	if cred.Password == "" {
		return nil, fmt.Errorf("gitref: connection %q minted an empty credential for %s", res.Name(), repo)
	}
	return Token{Token: cred.Password, Username: cred.Username}, nil
}

var (
	_ AuthSource = Fixed{}
	_ AuthSource = Connections{}
)
