package kube_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery/kube"
)

// TestConnectMissingKubeconfigFails: an explicit path that does not exist must
// fail, and say what to do about it. Connecting is the step where a preview
// stops being offline, so its failure has to be legible rather than a bare
// wrapped syscall error.
func TestConnectMissingKubeconfigFails(t *testing.T) {
	_, err := kube.Connect(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Connect succeeded with a nonexistent kubeconfig")
	}
	for _, want := range []string{"kube:", "KUBECONFIG"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestConnectMalformedKubeconfigFails: a file that exists but is not a
// kubeconfig must fail at Connect rather than at first use, so `kelson diff
// --dry-run=server` reports a configuration problem instead of a mid-preview
// crash.
func TestConnectMalformedKubeconfigFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := writeFile(path, "this is not a kubeconfig\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := kube.Connect(path); err == nil {
		t.Fatal("Connect succeeded with a malformed kubeconfig")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
