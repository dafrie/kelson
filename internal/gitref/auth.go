package gitref

import (
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/dafrie/kelson/internal/delivery"
)

// Auth authenticates the read of a remote repository's refs.
//
// It is a much smaller interface than the one the deleted git writer had
// (internal/delivery/git, ADR-0028 decision 9), and deliberately: that one also
// had to mint forge API tokens for pull-request mode and load ssh deploy keys
// for a push. Nothing here writes to a repository, so all that is needed is a
// transport credential for an anonymous or token-authenticated `ls-remote`.
type Auth interface {
	// Name identifies the method for error messages.
	Name() string
	// GitAuth returns the transport auth. A nil AuthMethod is valid and means
	// anonymous (local paths, public read-only remotes).
	GitAuth() (transport.AuthMethod, error)
}

// Anonymous is the no-credential method: local filesystem remotes and public
// repositories. It is the default when no Auth is configured.
type Anonymous struct{}

// Name implements [Auth].
func (Anonymous) Name() string { return "anonymous" }

// GitAuth implements [Auth].
func (Anonymous) GitAuth() (transport.AuthMethod, error) { return nil, nil }

// Token authenticates with a personal access token / project token, which is
// what a private source repository over HTTPS needs.
type Token struct {
	// Token is the secret. It is never logged.
	Token string
	// Username is the HTTP basic user. Forges ignore its value but require it
	// to be non-empty; the default matches GitHub's convention.
	Username string
}

// Name implements [Auth].
func (Token) Name() string { return "token" }

// GitAuth implements [Auth].
func (t Token) GitAuth() (transport.AuthMethod, error) {
	if t.Token == "" {
		return nil, delivery.ApplyFailed("git/auth", "token",
			"token authentication configured with an empty token",
			"set the source credential (e.g. KELSON_GIT_TOKEN), or leave it unset to read a public repository anonymously")
	}
	user := t.Username
	if user == "" {
		user = "x-access-token"
	}
	return &githttp.BasicAuth{Username: user, Password: t.Token}, nil
}

var (
	_ Auth = Anonymous{}
	_ Auth = Token{}
)
