package build

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// How a build pod gets its source, shared by every driver.
//
// Cloning in-pod is a decision of the build plane, not of one strategy: an
// in-cluster build starts with an empty workspace, and cloning there keeps the
// control plane out of the data path — nothing uploads a tarball through
// kelson. Both drivers render the same clone, so the script lives here rather
// than once per driver, where the two copies would drift and only one of them
// would be the one anybody read.
//
// These are strings, not containers: the shape of the init container is the
// driver's (its image, its security context, its volume names), and only the
// script and the path convention are common.

// Workspace is where the source tree lives inside a build pod. Every driver
// mounts the same path, so a build's context path means the same thing
// whichever strategy renders it.
const Workspace = "/workspace"

// The clone credential's projection (ADR-0033 decision 5).
//
// Until this existed the clone fetched with no credential at all, which is the
// gap ADR-0033 names in its Context: the server resolved a private
// repository's ref with the one credential it had, and then the build pod
// failed to fetch it. The credential is minted per build, written into a Secret
// beside the build's other per-run objects, and projected here.
const (
	// CloneCredentialVolume is the volume name both drivers give the
	// projection. It is shared so the mount and the volume cannot drift.
	CloneCredentialVolume = "clone-credential"

	// CloneCredentialDir is where the projection is mounted. It is outside
	// [Workspace] deliberately: the workspace becomes the build context, and a
	// credential file inside it would be one `COPY . .` away from the image.
	CloneCredentialDir = "/kelson/clone-credential"

	// CloneCredentialUsernameKey and CloneCredentialPasswordKey are the Secret's
	// keys, and the file names inside [CloneCredentialDir]. They are the pair a
	// forge.Credential carries; the username is an identity and the password is
	// the secret, and both are read by the helper below rather than by anything
	// that could print them.
	CloneCredentialUsernameKey = "username"
	CloneCredentialPasswordKey = "password"

	// CloneCredentialMode is 0400: readable by the build user and nobody else.
	// Kubernetes takes the mode as a decimal int32.
	CloneCredentialMode int32 = 0o400
)

// CloneSecretName is the per-run Secret's name, derived from the build Job's so
// the two are visibly one build's objects. Both drivers derive their Job name
// the same way and then call this, rather than each inventing a suffix.
func CloneSecretName(jobName string) string { return jobName + "-clone" }

// CloneCredential is the HTTPS basic-auth pair a clone authenticates with.
//
// It is this package's own type rather than forge.Credential because the build
// plane must not know how a credential was minted — an installation token and a
// pasted PAT are the same two strings by the time a fetch uses them, and the
// expiry that separates them is the forge adapter's problem, not the pod's.
type CloneCredential struct {
	Username, Password string
}

// Set reports whether there is a credential to project at all. An unset one is
// an anonymous fetch, which is correct for a public repository.
func (c CloneCredential) Set() bool { return c.Password != "" }

// SecretValues returns the literal strings that must never appear in output,
// for registration with internal/redact (issue #117). The username is not
// included, for forge.Credential.SecretValues's reason: it is an identity, it
// is printed deliberately, and scrubbing "x-access-token" out of unrelated text
// helps nobody.
func (c CloneCredential) SecretValues() []string {
	if c.Password == "" {
		return nil
	}
	return []string{c.Password}
}

// String implements fmt.Stringer so a credential reached by a `%v` on the
// Request that carries it cannot spill. Same envelope as forge.Credential's.
func (c CloneCredential) String() string {
	if c.Username == "" && c.Password == "" {
		return "no credential"
	}
	return fmt.Sprintf("username %q (password redacted)", c.Username)
}

// CloneAuth mints the credential one build's clone fetches with.
//
// It is a seam rather than a field on [Request] because the Request is built by
// the API plane from a spec, and minting needs a cluster client and a forge
// round trip that the API plane deliberately cannot reach (.golangci.yml). The
// driver holds this instead and asks at submit time, which is also when the
// credential must be freshest: an installation token lives about an hour, so
// minting it when the Request was assembled rather than when the pod starts
// would be the difference between a build that fetches and one that does not.
type CloneAuth interface {
	// CloneCredential returns the credential for req.SourceGit. A zero
	// [CloneCredential] means "fetch anonymously" and is never an error.
	CloneCredential(ctx context.Context, req Request) (CloneCredential, error)
}

// CloneScript is the shell script that fetches exactly one commit into
// [Workspace].
//
// A ref that names a commit is fetched directly at depth 1, which is both the
// fastest path and the only one that guarantees the build matches Revision. A
// branch or tag is fetched at depth 1 too; that is a moving target, and this
// comment says so rather than pretending the result is pinned.
//
// # The credential never appears in this script
//
// This string is the init container's `command`, so `kubectl get job -o yaml`
// prints it and so does every event that quotes a failing pod. What goes in it
// when [Request.CloneSecret] is set is a *credential helper*, and the helper is
// two `cat` calls against the projected files — paths, never values. That is
// the whole reason the token is not simply interpolated into the remote URL:
// a URL with a token in it is visible in `ps`, in the pod spec, in git's own
// error output and in the `.git/config` the workspace carries into the build
// context.
//
// The helper is passed with `-c` rather than written with `git config`, so it
// is not persisted into `.git/config` either — the workspace *is* the build
// context, and a config file naming kelson's mount paths would ride into the
// image for no reason.
func CloneScript(req Request) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "git init -q %s\n", Workspace)
	fmt.Fprintf(&b, "cd %s\n", Workspace)
	fmt.Fprintf(&b, "git remote add origin %s\n", ShellQuote(req.SourceGit))
	ref := req.SourceRef
	if ref == "" {
		ref = "HEAD"
	}
	fmt.Fprintf(&b, "git %sfetch --depth 1 origin %s\n", credentialFlag(req), ShellQuote(ref))
	b.WriteString("git checkout -q FETCH_HEAD\n")
	return b.String()
}

// credentialFlag is the `-c credential.helper=…` git reads the projected
// credential through, or "" for an anonymous fetch.
//
// A helper value beginning with `!` is a shell command git runs, handing it the
// operation (`get`, `store`, `erase`) as its first argument and the request on
// stdin. Only `get` is answered; `store` and `erase` exit silently, because
// there is nothing to write back to a file the kubelet owns and a helper that
// errored on them would make every fetch print a warning.
func credentialFlag(req Request) string {
	if req.CloneSecret == "" {
		return ""
	}
	helper := fmt.Sprintf(`!f() { [ "$1" = get ] || exit 0; echo username="$(cat %s/%s)"; echo password="$(cat %s/%s)"; }; f`,
		CloneCredentialDir, CloneCredentialUsernameKey, CloneCredentialDir, CloneCredentialPasswordKey)
	return "-c " + ShellQuote("credential.helper="+helper) + " "
}

// CloneSecretManifest renders the per-run Secret holding a clone credential.
//
// It is rendered rather than built with a Kubernetes client for the reason
// every workload in this plane is: internal/build sits on the standard-library
// allow-list and may not import client-go (.golangci.yml). The caller submits
// it beside the Job, and the executor that creates it makes the Job its owner
// so the two share one lifecycle — the Secret is deleted when the build's Job
// is, by kelson's own cleanup and by the API server's garbage collector if this
// process never gets to run it.
//
// The labels are the Job's own, passed in rather than derived here, so the
// Secret answers the same `kubectl get -l kelson.dev/project=…` the Job does.
func CloneSecretManifest(name, namespace string, labels map[string]string, cred CloneCredential) ([]byte, error) {
	if name == "" || namespace == "" {
		return nil, fmt.Errorf("build: a clone credential Secret needs a name and a namespace")
	}
	if !cred.Set() {
		return nil, fmt.Errorf("build: refusing to render an empty clone credential Secret")
	}
	username := cred.Username
	if username == "" {
		// Forges ignore the value beside a token but require one to be
		// present; this is the same default gitref.Token applies.
		username = "x-access-token"
	}
	type secretMeta struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels,omitempty"`
	}
	doc := struct {
		APIVersion string            `yaml:"apiVersion"`
		Kind       string            `yaml:"kind"`
		Metadata   secretMeta        `yaml:"metadata"`
		Type       string            `yaml:"type"`
		StringData map[string]string `yaml:"stringData"`
	}{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata:   secretMeta{Name: name, Namespace: namespace, Labels: labels},
		Type:       "Opaque",
		StringData: map[string]string{
			CloneCredentialUsernameKey: username,
			CloneCredentialPasswordKey: cred.Password,
		},
	}
	return yaml.Marshal(doc)
}

// ContextPath is the build context inside the cloned workspace. A monorepo
// clones whole and builds one subdirectory, so ContextDir selects the subtree
// rather than changing what is fetched.
func ContextPath(req Request) string {
	dir := strings.Trim(req.ContextDir, "/")
	if dir == "" || dir == "." {
		return Workspace
	}
	return Workspace + "/" + dir
}

// ShellQuote wraps a value in single quotes for a generated shell command.
// The values here come from the spec, not from a build's own output, but a
// repository URL is still user input reaching a shell — quoting it is the
// difference between a config error and a command injection.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
