package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// `kelson secret` under the sops backend, which is gated (issue #224).
//
// The property this file used to assert — that `-f` is what decides where a
// value goes, so a credential for a sops environment never reaches the cluster
// store — is now asserted in its strongest form: for a sops environment nothing
// is written *anywhere*, and the refusal says so. ADR-0028 deleted the git
// writer that committed the encrypted file, and decision 7 records what
// replaces it (the encrypted Secrets ship inside the published artifact).
//
// What is deliberately still tested is the selection: the backend is read from
// the spec, and the cluster store is never reached for an environment that does
// not use it. A gate that leaked to the cluster store would be worse than the
// missing feature.

const sopsRecipient = "age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw"

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

func runSecretSpec(t *testing.T, cluster *fakeSecretStore, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	cmd := rootWithSecrets(cluster.connector())
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	msg, code = resolveExit(cmd.Execute())
	return outBuf.String(), code, msg
}

// Every verb refuses a sops environment with the tracked code, and none of them
// falls through to the cluster store. Falling through is the failure that would
// matter: a credential written to the cluster for an environment whose secrets
// are supposed to live encrypted in the artifact is a credential in the wrong
// place, silently.
func TestSOPSBackendIsRefusedAndReachesNoStore(t *testing.T) {
	spec := sopsSpec(t)
	cases := []struct {
		name string
		args []string
	}{
		{"set", []string{"secret", "set", "checkout-db", "-f", spec, "--env", "production", "url=" + setSentinel}},
		{"list", []string{"secret", "list", "-f", spec, "--env", "production"}},
		{"delete", []string{"secret", "delete", "checkout-db", "-f", spec, "--env", "production", "--yes"}},
		{"rotate", []string{"secret", "rotate", "-f", spec, "--env", "production"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &fakeSecretStore{}
			stdout, code, msg := runSecretSpec(t, cluster, tc.args...)
			if code != exitErr {
				t.Fatalf("exit = %d, want %d\n%s", code, exitErr, stdout)
			}
			if len(cluster.sets)+len(cluster.deletes)+len(cluster.lists) != 0 {
				t.Fatalf("the cluster store was reached for a sops environment: %+v", cluster)
			}
			assertNoValue(t, "stdout", stdout)
			assertNoValue(t, "the error message", msg)
			if tc.name == "rotate" {
				// `rotate` has always been sops-only, and its answer for a
				// non-sops environment is the same one it gives here; what
				// matters is that it writes nothing and names the backend.
				return
			}
			if !strings.Contains(msg, string(delivery.ErrNotImplemented)) {
				t.Errorf("the refusal must carry %s, got: %s", delivery.ErrNotImplemented, msg)
			}
			if !strings.Contains(msg, "#224") {
				t.Errorf("the refusal must name the tracking issue, got: %s", msg)
			}
		})
	}
}

// The cluster backend is unaffected: a spec that selects it (or selects
// nothing) still writes through the cluster store, which is what keeps the gate
// above from reading as "kelson secret is broken".
func TestSetUsesTheClusterStoreForAClusterSpec(t *testing.T) {
	spec := specFile(t, "    backend: cluster\n")
	cluster := &fakeSecretStore{}
	stdout, code, msg := runSecretSpec(t, cluster,
		"secret", "set", "checkout-db", "-f", spec, "--env", "production", "url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if len(cluster.sets) != 1 {
		t.Fatalf("the cluster store received %d writes, want 1", len(cluster.sets))
	}
	assertNoValue(t, "stdout", stdout)
}

// externalSecrets is refused for its own reason, and that reason must not be
// replaced by the sops gate: no value passes through kelson at all under that
// backend, which is a permanent statement rather than a tracked gap.
func TestExternalSecretsBackendIsRefused(t *testing.T) {
	spec := specFile(t, "    backend: externalSecrets\n    store: vault-backend\n")
	cluster := &fakeSecretStore{}
	_, code, msg := runSecretSpec(t, cluster,
		"secret", "set", "checkout-db", "-f", spec, "--env", "production", "url="+setSentinel)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "vault-backend") {
		t.Errorf("the refusal should name the store this environment reads from, got: %s", msg)
	}
	if strings.Contains(msg, "#224") {
		t.Errorf("externalSecrets is not a tracked gap, it is how that backend works: %s", msg)
	}
	if len(cluster.sets) != 0 {
		t.Fatal("the cluster store was reached for an externalSecrets environment")
	}
}
