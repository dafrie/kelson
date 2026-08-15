package forge

import (
	"context"
	"fmt"
)

// genericProvider is the universal fallback (ADR-0033 decision 2): a token
// pasted into a Secret-backed form, for a bare git host and for every forge
// whose adapter has not been written yet.
//
// It implements [Provider] and nothing else, deliberately. There is no forge
// API behind it to browse repositories with, no signature scheme to verify a
// delivery against and nowhere to write a status — and a stub that answered
// those with an empty list or a silent success would make the UI claim
// capabilities the connection does not have. Returning the mandatory
// credential and failing the type assertion for the rest is the honest shape,
// and it is what makes "absence degrades the UI, never the deploy" a property
// of the type system rather than a promise.
type genericProvider struct{}

var _ Provider = genericProvider{}

// Name implements [Provider].
func (genericProvider) Name() string { return nameGeneric }

// MintCloneCredential implements [Provider]. There is nothing to mint: a
// stored token is the credential, so this returns it unchanged with a zero
// ExpiresAt — the caller reads that as "this one does not rotate itself".
//
// The username default matches gitref.Token (internal/gitref/auth.go), so the
// credential a build pod clones with and the one `git ls-remote` resolves with
// are the same pair.
func (genericProvider) MintCloneCredential(_ context.Context, c Conn, repoURL string) (Credential, error) {
	if c.Token == "" {
		return Credential{}, fmt.Errorf("forge/generic: %s carries no token for %s: %w", c, repoName(repoURL), ErrAuthFailed)
	}
	return tokenCredential(c), nil
}
