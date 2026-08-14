package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/secret"
)

// `kelson secret` under the sops backend. The property running through this
// file is the same one the cluster tests assert — the value went where it was
// supposed to and appeared in none of the output — plus the one this backend
// adds: which store the command chose is decided by the *spec*, so a user
// cannot write a credential into the cluster for an environment whose secrets
// live in Git, or the other way round.

const sopsRecipient = "age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw"

// fakeSOPSStore records what reached the sops store and answers Drift from a
// script. The real one needs a repository; what the command depends on is the
// request it hands over and the report it formats.
type fakeSOPSStore struct {
	fakeSecretStore
	drift      []secret.SOPSDrift
	recipients []string
}

func (f *fakeSOPSStore) Drift(context.Context, secret.Target) ([]secret.SOPSDrift, error) {
	return f.drift, f.err
}

func (f *fakeSOPSStore) Recipients() []string {
	if f.recipients == nil {
		return []string{sopsRecipient}
	}
	return f.recipients
}

func (f *fakeSOPSStore) RepoPath(name string) string {
	return "clusters/prod/secrets/" + name + ".enc.yaml"
}

// specFile writes a Project/Environment pair using the given secrets block and
// returns its path.
func specFile(t *testing.T, secrets string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	body := `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: nginx:1
  components:
    - {name: api, port: 8080}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  delivery:
    mode: flux
    git: {repo: https://example.test/deploy.git, path: clusters/prod}
  secrets:
` + secrets
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the spec: %v", err)
	}
	return path
}

func sopsSpec(t *testing.T) string {
	t.Helper()
	return specFile(t, "    backend: sops\n    ageRecipients: ["+sopsRecipient+"]\n")
}

func runSecretWithSops(t *testing.T, cluster *fakeSecretStore, sops *fakeSOPSStore, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	var connectSops sopsSecretConnector
	if sops != nil {
		connectSops = func(*model.Resolved) (sopsStore, error) { return sops, nil }
	}
	cmd := rootWithSecretBackends(cluster.connector(), connectSops)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	msg, code = resolveExit(cmd.Execute())
	return outBuf.String(), code, msg
}

// TestSetSelectsTheSOPSBackendFromTheSpec: -f is what tells kelson where the
// value goes, and nothing reaches the cluster store for a sops environment.
func TestSetSelectsTheSOPSBackendFromTheSpec(t *testing.T) {
	cluster, sops := &fakeSecretStore{}, &fakeSOPSStore{}
	stdout, code, msg := runSecretWithSops(t, cluster, sops,
		"secret", "set", "checkout-db", "-f", sopsSpec(t), "--env", "production", "url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, msg)
	}
	if len(cluster.sets) != 0 {
		t.Fatalf("the value reached the cluster store for a sops environment")
	}
	if len(sops.sets) != 1 {
		t.Fatalf("the sops store received %d sets", len(sops.sets))
	}
	req := sops.sets[0]
	if req.Name != "checkout-db" || req.Project != "checkout" || req.Environment != "production" {
		t.Errorf("request = %+v", req)
	}
	if req.Namespace != "checkout-production" {
		t.Errorf("namespace = %q; it must be the one the render targets", req.Namespace)
	}
	assertNoValue(t, "stdout", stdout)

	// The operator's half of the setup is printed, because kelson cannot do it
	// and a Kustomization without it applies the ciphertext verbatim.
	for _, want := range []string{"decryption:", "provider: sops", "name: " + model.DefaultAgeKeySecret, "age identity"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout must state the Kustomization requirement (%q):\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "clusters/prod/secrets/checkout-db.enc.yaml") {
		t.Errorf("stdout must name where the file landed:\n%s", stdout)
	}
	if !strings.Contains(stdout, sopsRecipient) {
		t.Errorf("stdout must name who can read it:\n%s", stdout)
	}
}

// TestSetUsesTheClusterStoreForAClusterSpec is the other half: passing -f does
// not change the backend, it reveals it.
func TestSetUsesTheClusterStoreForAClusterSpec(t *testing.T) {
	cluster, sops := &fakeSecretStore{}, &fakeSOPSStore{}
	_, code, msg := runSecretWithSops(t, cluster, sops,
		"secret", "set", "checkout-db", "-f", specFile(t, "    backend: cluster\n"), "--env", "production", "url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, msg)
	}
	if len(sops.sets) != 0 {
		t.Fatalf("a cluster environment must not write to the delivery repository")
	}
	if len(cluster.sets) != 1 {
		t.Fatalf("the cluster store received %d sets", len(cluster.sets))
	}
}

// TestExternalSecretsBackendIsRefused: under that backend no value passes
// through kelson at all, so writing one here would put kelson in a fight with
// the controller that owns the Secret. It is refused by name rather than done.
func TestExternalSecretsBackendIsRefused(t *testing.T) {
	cluster, sops := &fakeSecretStore{}, &fakeSOPSStore{}
	_, code, msg := runSecretWithSops(t, cluster, sops,
		"secret", "set", "checkout-db", "-f",
		specFile(t, "    backend: externalSecrets\n    store: vault-backend\n"),
		"--env", "production", "url="+setSentinel)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "externalSecrets") || !strings.Contains(msg, "vault-backend") {
		t.Errorf("the refusal must name the backend and the store: %q", msg)
	}
	if len(cluster.sets) != 0 || len(sops.sets) != 0 {
		t.Fatalf("nothing must have been written")
	}
	assertNoValue(t, "the error message", msg)
}

// TestSOPSListHasNoAgeColumn: a file's age is a fact about the repository, not
// about the credential, and a column headed AGE that meant something else
// would be worse than no column.
func TestSOPSListHasNoAgeColumn(t *testing.T) {
	sops := &fakeSOPSStore{}
	sops.secrets = []secret.Secret{{Name: "checkout-db", Namespace: "checkout-production", Keys: []string{"url"}}}
	stdout, code, msg := runSecretWithSops(t, &fakeSecretStore{}, sops,
		"secret", "list", "-f", sopsSpec(t), "--env", "production")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, msg)
	}
	if strings.Contains(stdout, "AGE") {
		t.Errorf("the sops listing must not claim an age:\n%s", stdout)
	}
	for _, want := range []string{"delivery repository", "checkout-db", "url"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing missing %q:\n%s", want, stdout)
		}
	}
}

// TestSOPSDeleteSaysTheValueIsStillInHistory: removing the file removes the
// Secret, not the credential. Saying so is the difference between a user
// rotating it and a user believing it is gone.
func TestSOPSDeleteSaysTheValueIsStillInHistory(t *testing.T) {
	sops := &fakeSOPSStore{}
	stdout, code, msg := runSecretWithSops(t, &fakeSecretStore{}, sops,
		"secret", "delete", "checkout-db", "-f", sopsSpec(t), "--env", "production", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, msg)
	}
	if len(sops.deletes) != 1 {
		t.Fatalf("the sops store received %d deletes", len(sops.deletes))
	}
	for _, want := range []string{"clusters/prod/secrets/checkout-db.enc.yaml", "history", "rotation"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestRotateReportsDriftAndExitsTwo: `rotate` changes nothing, names every
// stale file and both ways to fix each, and follows the `kelson diff` exit
// contract so it can gate CI.
func TestRotateReportsDriftAndExitsTwo(t *testing.T) {
	const second = "age1cnnsmzvurnpxzzggcwm4rvk44hgskzhrz92nryk6879kf8zll3jqqjhvt3"
	sops := &fakeSOPSStore{
		recipients: []string{sopsRecipient, second},
		drift: []secret.SOPSDrift{{
			Name:       "checkout-db",
			Path:       "clusters/prod/secrets/checkout-db.enc.yaml",
			Keys:       []string{"token", "url"},
			Recipients: []string{sopsRecipient},
			Missing:    []string{second},
		}},
	}
	stdout, code, msg := runSecretWithSops(t, &fakeSecretStore{}, sops,
		"secret", "rotate", "-f", sopsSpec(t), "--env", "production")
	if code != exitDiff {
		t.Fatalf("exit = %d, want %d (%s)", code, exitDiff, msg)
	}
	for _, want := range []string{
		"clusters/prod/secrets/checkout-db.enc.yaml",
		"missing:  " + second,
		"kelson secret set checkout-db",
		"token=<value> url=<value>",
		"sops updatekeys clusters/prod/secrets/checkout-db.enc.yaml",
		"nothing was changed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report is missing %q:\n%s", want, stdout)
		}
	}
}

func TestRotateWithNoDriftExitsZero(t *testing.T) {
	stdout, code, msg := runSecretWithSops(t, &fakeSecretStore{}, &fakeSOPSStore{},
		"secret", "rotate", "-f", sopsSpec(t), "--env", "production")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, msg)
	}
	if !strings.Contains(stdout, "wrapped for exactly those recipients") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

// TestRotateRefusesANonSOPSEnvironment: under backend cluster a Secret is not
// encrypted to anybody, so there is nothing for this command to report and
// saying so beats printing an empty table.
func TestRotateRefusesANonSOPSEnvironment(t *testing.T) {
	_, code, msg := runSecretWithSops(t, &fakeSecretStore{}, &fakeSOPSStore{},
		"secret", "rotate", "-f", specFile(t, "    backend: cluster\n"), "--env", "production")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "backend sops") {
		t.Errorf("message = %q", msg)
	}
}

// TestSOPSDryRunCommitsNothing: the dry run reports as a dry run and the store
// is told, so the encryption still happens and the commit does not.
func TestSOPSDryRunCommitsNothing(t *testing.T) {
	sops := &fakeSOPSStore{}
	stdout, code, msg := runSecretWithSops(t, &fakeSecretStore{}, sops,
		"secret", "set", "checkout-db", "-f", sopsSpec(t), "--env", "production", "--dry-run", "url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, msg)
	}
	if len(sops.sets) != 1 || !sops.sets[0].DryRun {
		t.Fatalf("the dry run did not reach the store as one: %+v", sops.sets)
	}
	if !strings.Contains(stdout, "committed nothing") {
		t.Errorf("stdout:\n%s", stdout)
	}
	assertNoValue(t, "stdout", stdout)
}
