package build_test

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/redact"
)

// The private-build fix (ADR-0033 decision 5): the clone that used to fetch
// with no credential at all now fetches with one, and the one property every
// assertion here circles is that the *value* never leaves the Secret.

const probeToken = "ghs_clone_credential_probe_token"

func credentialRequest() build.Request {
	return build.Request{
		Project:     "checkout",
		SourceGit:   "https://github.com/acme/checkout.git",
		SourceRef:   "9f1c0de5b2a1",
		CloneSecret: "build-checkout-9f1c0de5-clone",
		Image:       "ghcr.io/acme/checkout",
	}
}

// The script reads the credential through git's credential helper, from the
// files the kubelet projects. It is a helper and not a URL because this string
// is the init container's `command`: a token interpolated into the remote would
// be in the pod spec, in `ps`, in git's error output and in the `.git/config`
// the workspace carries into the build context.
func TestCloneScriptReadsTheCredentialFromTheMountedFiles(t *testing.T) {
	script := build.CloneScript(credentialRequest())

	for _, want := range []string{
		"credential.helper=",
		build.CloneCredentialDir + "/" + build.CloneCredentialUsernameKey,
		build.CloneCredentialDir + "/" + build.CloneCredentialPasswordKey,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("clone script must contain %q\n%s", want, script)
		}
	}
	// The remote is still the plain URL: no userinfo, no token.
	if !strings.Contains(script, "git remote add origin 'https://github.com/acme/checkout.git'") {
		t.Errorf("the remote must stay credential-free\n%s", script)
	}
	if strings.Contains(script, "@github.com") {
		t.Errorf("a credential reached the remote URL\n%s", script)
	}
	// Only `get` is answered; a helper that errored on store/erase would make
	// every fetch print a warning about a file the kubelet owns.
	if !strings.Contains(script, `[ "$1" = get ]`) {
		t.Errorf("the helper must answer only the get operation\n%s", script)
	}
}

// Without a Secret the script is exactly what it always was: an anonymous
// fetch, which is correct for a public repository.
func TestCloneScriptWithoutACredentialIsUnchanged(t *testing.T) {
	req := credentialRequest()
	req.CloneSecret = ""
	script := build.CloneScript(req)
	if strings.Contains(script, "credential.helper") {
		t.Errorf("an anonymous clone must configure no credential helper\n%s", script)
	}
}

// The Secret is where the value lives, and it is the only place it lives.
func TestCloneSecretManifestCarriesTheValueAndTheJobsLabels(t *testing.T) {
	labels := map[string]string{"kelson.dev/project": "checkout"}
	out, err := build.CloneSecretManifest("build-checkout-9f1c0de5-clone", "kelson-builds", labels,
		build.CloneCredential{Username: "x-access-token", Password: probeToken})
	if err != nil {
		t.Fatalf("CloneSecretManifest: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: Secret",
		"name: build-checkout-9f1c0de5-clone",
		"namespace: kelson-builds",
		"kelson.dev/project: checkout",
		build.CloneCredentialPasswordKey + ": " + probeToken,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Secret manifest must contain %q\n%s", want, s)
		}
	}

	// A forge that wants no username still needs one present beside the token.
	out, err = build.CloneSecretManifest("n", "ns", nil, build.CloneCredential{Password: "t"})
	if err != nil {
		t.Fatalf("CloneSecretManifest: %v", err)
	}
	if !strings.Contains(string(out), "username: x-access-token") {
		t.Errorf("an empty username must default rather than render empty\n%s", out)
	}

	// An empty credential is a bug at the call site, not a Secret with an empty
	// password that the clone would then present as though it were one.
	if _, err := build.CloneSecretManifest("n", "ns", nil, build.CloneCredential{}); err == nil {
		t.Error("an empty credential must not render a Secret")
	}
}

// A credential reached through a `%v` on anything that holds it prints an
// envelope, not the token (ADR-0009).
func TestCloneCredentialDoesNotPrintItself(t *testing.T) {
	cred := build.CloneCredential{Username: "x-access-token", Password: probeToken}
	if got := cred.String(); strings.Contains(got, probeToken) {
		t.Errorf("CloneCredential.String leaked the password: %q", got)
	}
	req := credentialRequest()
	if got := strings.Join([]string{cred.String(), req.CloneSecret}, " "); strings.Contains(got, probeToken) {
		t.Errorf("the credential leaked through a Request field: %q", got)
	}
	if values := cred.SecretValues(); len(values) != 1 || values[0] != probeToken {
		t.Errorf("SecretValues = %v, want just the password", values)
	}
	if len(build.CloneCredential{}.SecretValues()) != 0 {
		t.Error("an empty credential has nothing to register")
	}
}

// A registered value is unprintable process-wide from the moment it is learned
// (issue #117). This asserts the wiring the drivers rely on rather than the
// scrubber itself, which internal/redact tests.
func TestRegisteredCloneCredentialIsScrubbed(t *testing.T) {
	redact.Register(build.CloneCredential{Password: probeToken}.SecretValues()...)
	if got := redact.Scrub("fatal: could not read Username for " + probeToken); strings.Contains(got, probeToken) {
		t.Errorf("a registered clone credential survived redaction: %q", got)
	}
}

func TestCloneSecretNameIsDerivedFromTheJob(t *testing.T) {
	if got := build.CloneSecretName("build-checkout-9f1c0de5"); got != "build-checkout-9f1c0de5-clone" {
		t.Errorf("CloneSecretName = %q, want the Job's name plus -clone", got)
	}
}
