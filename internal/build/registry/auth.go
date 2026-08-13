package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// SecretRef names a Kubernetes Secret that holds registry credentials.
//
// Credentials are references, never literals (ADR-0009): the spec and the
// build requests carry this reference, and the value is resolved out-of-band
// from the cluster.
type SecretRef struct {
	// Name and Namespace locate the Secret in the cluster.
	Name      string
	Namespace string
	// Registry is the registry host the credential is for, matching the key
	// inside the Secret's dockerconfigjson (e.g. "ghcr.io").
	Registry string
}

// Credential is the resolved auth for one registry push.
type Credential struct {
	Username string
	Password string
	// Auth is the precomputed base64("user:pass") from the dockerconfigjson
	// ".auth" field, when present.
	Auth string
}

// Resolver resolves registry credentials from a Secret reference. It is a
// reference, not an implementation of the cluster read: the Kubernetes client
// libraries are not permitted on this plane's depguard allow-list, so the
// concrete, client-backed Resolver lives in a cluster-facing plane while this
// package only consumes the surface. Tests use a fake; no test touches a
// cluster or the network.
type Resolver interface {
	// Resolve returns the credential for the given Secret reference.
	Resolve(ctx context.Context, ref SecretRef) (Credential, error)
}

// CredentialFromDockerConfigJSON extracts the credential for registry from the
// standard "kubernetes.io/dockerconfigjson" Secret payload (the decoded
// .data.dockerconfigjson). It supports the docker auth shape
// {"auths": {"<registry>": {"username","password" | "auth"}}}. Error messages
// never carry the credential value: credentials only ever become a String via
// Credential.String, which redacts them.
func CredentialFromDockerConfigJSON(data []byte, registry string) (Credential, error) {
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Credential{}, fmt.Errorf("registry: invalid dockerconfigjson: %s", err)
	}
	if len(cfg.Auths) == 0 {
		return Credential{}, fmt.Errorf("registry: no registry credentials in dockerconfigjson for %s", registry)
	}
	entry, ok := cfg.Auths[registry]
	if !ok {
		return Credential{}, fmt.Errorf("registry: no credential for registry %s", registry)
	}

	c := Credential{Username: entry.Username, Password: entry.Password, Auth: entry.Auth}
	if c.Auth == "" && c.Username != "" {
		// Reconstruct the ".auth" short form from username:password. This is a
		// derived value; logging or error text never carries it because
		// Credential.String redacts the password.
		c.Auth = base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
	}
	return c, nil
}

// SecretValues returns the literal strings in this credential that must never
// appear in any output, for registration with internal/redact (issue #117).
//
// Credential.String protects the credential from being *formatted*; this
// protects it from being echoed by something kelson does not format — a build
// log, an API server message, a driver that prints its own auth config. The two
// are complementary: String is the envelope, this is the net underneath it.
//
// The username is not included. It is an identity, it is printed deliberately
// by String, and scrubbing it would blank a word like "robot" out of unrelated
// output.
func (c Credential) SecretValues() []string {
	out := make([]string, 0, 2)
	if c.Password != "" {
		out = append(out, c.Password)
	}
	if c.Auth != "" {
		out = append(out, c.Auth)
	}
	return out
}

// String implements fmt.Stringer so a Credential can never be leaked through a
// log line or an error message that interpolates it with %v or %s. This is the
// envelope for the "never log a credential" property (ADR-0009): producers
// report "which registry, which username", never "what password".
func (c Credential) String() string {
	if c.Username == "" {
		return "credentials for anonymous push"
	}
	return fmt.Sprintf("username %q (password redacted)", c.Username)
}
