package git

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/dafrie/kelson/internal/delivery"
)

// Auth is the authentication surface of the git writer. It is an interface on
// purpose (issue #39): tokens, SSH keys and GitHub App installations are all
// legitimate ways to reach a deployment repository, and which one an install
// uses is deployment configuration, not a code change.
//
// Two capabilities are needed and they are not the same:
//   - GitAuth authenticates the git transport (clone/push).
//   - APIToken authenticates the provider REST API (pull-request creation).
//     SSH keys cannot mint one; such a method reports ok=false and the caller
//     fails with a clear "PR mode needs an API credential" error rather than a
//     confusing 401.
type Auth interface {
	// Name identifies the method for error messages.
	Name() string
	// GitAuth returns the transport auth for clone/push. A nil AuthMethod is
	// valid and means anonymous (local paths, public read-only remotes).
	GitAuth() (transport.AuthMethod, error)
	// APIToken returns a bearer token for the forge API. ok is false when this
	// method cannot provide one.
	APIToken(ctx context.Context) (token string, ok bool, err error)
}

// Anonymous is the no-credential method: local filesystem remotes and public
// repositories. It is the default when no Auth is configured.
type Anonymous struct{}

func (Anonymous) Name() string { return "anonymous" }

func (Anonymous) GitAuth() (transport.AuthMethod, error) { return nil, nil }

func (Anonymous) APIToken(context.Context) (string, bool, error) { return "", false, nil }

// Token authenticates with a personal access token / project token. This is
// the fully supported method for HTTPS remotes and forge APIs: the same token
// pushes the branch and opens the pull request.
type Token struct {
	// Token is the secret. It is never logged or written into a commit.
	Token string
	// Username is the HTTP basic user. Forges ignore its value but require it
	// to be non-empty; the default matches GitHub's convention.
	Username string
}

func (Token) Name() string { return "token" }

func (t Token) GitAuth() (transport.AuthMethod, error) {
	if t.Token == "" {
		return nil, delivery.ApplyFailed("git/auth", "token",
			"token authentication configured with an empty token",
			"set the delivery credential (e.g. KELSON_GIT_TOKEN) or configure a different auth method")
	}
	user := t.Username
	if user == "" {
		user = "x-access-token"
	}
	return &githttp.BasicAuth{Username: user, Password: t.Token}, nil
}

func (t Token) APIToken(context.Context) (string, bool, error) {
	if t.Token == "" {
		return "", false, delivery.ApplyFailed("git/auth", "token",
			"token authentication configured with an empty token",
			"set the delivery credential before using pull-request mode")
	}
	return t.Token, true, nil
}

// SSHKey authenticates the git transport with an SSH private key. Supported
// for commit mode against ssh:// and git@host:owner/repo remotes.
//
// SSH cannot authenticate a forge REST API, so pull-request mode additionally
// needs a Token (compose with Pair). That is a property of the forges, not a
// gap in this implementation.
type SSHKey struct {
	// User is the SSH user; defaults to "git".
	User string
	// KeyPath is the private key file (e.g. ~/.ssh/id_ed25519).
	KeyPath string
	// Passphrase decrypts the key; empty for unencrypted keys.
	Passphrase string
	// KnownHostsPath overrides the default known_hosts lookup. Host key
	// verification is never disabled here.
	KnownHostsPath string
}

func (SSHKey) Name() string { return "ssh-key" }

func (s SSHKey) GitAuth() (transport.AuthMethod, error) {
	if s.KeyPath == "" {
		return nil, delivery.ApplyFailed("git/auth", "keyPath",
			"ssh authentication configured without a private key path",
			"point keyPath at the deploy key kelson should use")
	}
	user := s.User
	if user == "" {
		user = "git"
	}
	keys, err := gitssh.NewPublicKeysFromFile(user, s.KeyPath, s.Passphrase)
	if err != nil {
		e := delivery.ApplyFailed("git/auth", "keyPath",
			"could not load the configured ssh private key",
			"check the key path and passphrase; the key must be readable by the kelson process")
		e.Cause = err.Error()
		return nil, e
	}
	if s.KnownHostsPath != "" {
		cb, err := gitssh.NewKnownHostsCallback(s.KnownHostsPath)
		if err != nil {
			e := delivery.ApplyFailed("git/auth", "knownHostsPath",
				"could not load the configured known_hosts file",
				"check the path, or omit it to use the default ssh known_hosts")
			e.Cause = err.Error()
			return nil, e
		}
		keys.HostKeyCallback = cb
	}
	return keys, nil
}

func (s SSHKey) APIToken(context.Context) (string, bool, error) { return "", false, nil }

// GitHubApp authenticates as a GitHub App installation.
//
// TODO(#39): minting installation tokens (JWT sign with the app private key →
// POST /app/installations/{id}/access_tokens, cached until expiry) is not
// implemented yet. The shape is fixed here so adopting it is a change in this
// one type; until then the method fails loudly rather than silently degrading
// to another credential.
type GitHubApp struct {
	AppID          string
	InstallationID string
	PrivateKeyPath string
}

func (GitHubApp) Name() string { return "github-app" }

func (g GitHubApp) GitAuth() (transport.AuthMethod, error) {
	return nil, g.notImplemented()
}

func (g GitHubApp) APIToken(context.Context) (string, bool, error) {
	return "", false, g.notImplemented()
}

func (GitHubApp) notImplemented() error {
	return delivery.Error{
		Code:        delivery.ErrUnsupported,
		Resource:    "git/auth",
		Field:       "githubApp",
		Message:     "GitHub App authentication is not implemented yet",
		Remediation: "use token authentication (an app installation token works) or an ssh deploy key",
		DocsURL:     "https://kelson.dev/delivery/git#authentication",
	}
}

// Pair combines a transport credential with an API credential, for the common
// "push over SSH, open the PR with a token" setup.
type Pair struct {
	Transport Auth
	API       Auth
}

func (p Pair) Name() string { return p.Transport.Name() + "+" + p.API.Name() }

func (p Pair) GitAuth() (transport.AuthMethod, error) { return p.Transport.GitAuth() }

func (p Pair) APIToken(ctx context.Context) (string, bool, error) { return p.API.APIToken(ctx) }

var (
	_ Auth = Anonymous{}
	_ Auth = Token{}
	_ Auth = SSHKey{}
	_ Auth = GitHubApp{}
	_ Auth = Pair{}
)
