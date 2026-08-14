package sops

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

// The cross-implementation check: what kelson wrote is what getsops/sops
// reads.
//
// internal/sops implements the SOPS format rather than importing it (see the
// package doc for the 98-module reason), so "the format is right" cannot be
// something this package asserts against its own code. TestRoundTrip checks
// the encoder against the specification with a decryptor written from that
// specification; this test checks it against the implementation that matters.
//
// It is skipped when no `sops` is on PATH, which is the normal case in CI: the
// binary is 74 MB and making it a build prerequisite would be paying the
// dependency cost this package exists to avoid. Run it deliberately —
//
//	go install github.com/getsops/sops/v3/cmd/sops@v3.13.3
//	go test ./internal/sops/ -run Sops
//
// — whenever anything in encrypt.go changes, and say in the pull request that
// you did. KELSON_SOPS_BIN points at a binary that is not on PATH.
func TestSopsBinaryDecrypts(t *testing.T) {
	bin := sopsBinary(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generating an identity: %v", err)
	}

	encrypted, err := EncryptSecret(testSecret(), Options{Recipients: []string{id.Recipient().String()}})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	path := filepath.Join(t.TempDir(), "checkout-db.enc.yaml")
	if err := os.WriteFile(path, encrypted, 0o600); err != nil {
		t.Fatalf("writing the encrypted file: %v", err)
	}

	cmd := exec.Command(bin, "decrypt", "--output-type", "json", path)
	cmd.Env = append(os.Environ(), "SOPS_AGE_KEY="+id.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sops decrypt failed: %v\n%s", err, stderr.String())
	}

	var doc struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("parsing sops output: %v", err)
	}
	if doc.Kind != "Secret" || doc.Metadata.Name != "checkout-db" || doc.Metadata.Namespace != "checkout-production" {
		t.Errorf("sops read back %s %s/%s", doc.Kind, doc.Metadata.Namespace, doc.Metadata.Name)
	}
	want := testSecret()
	for key, value := range want.Data {
		if got := decodeBase64(t, doc.Data[key]); got != string(value) {
			t.Errorf("sops decrypted data.%s to something other than what was encrypted", key)
		}
	}
}

// TestSopsBinaryDetectsTampering: the real implementation must reject a file
// whose plaintext half was edited, which is the guarantee TestMACCovers…
// asserts against the local decryptor.
func TestSopsBinaryDetectsTampering(t *testing.T) {
	bin := sopsBinary(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generating an identity: %v", err)
	}
	encrypted, err := EncryptSecret(testSecret(), Options{Recipients: []string{id.Recipient().String()}})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	tampered := bytes.Replace(encrypted,
		[]byte("namespace: checkout-production"), []byte("namespace: kube-system     "), 1)
	path := filepath.Join(t.TempDir(), "moved.enc.yaml")
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	cmd := exec.Command(bin, "decrypt", path)
	cmd.Env = append(os.Environ(), "SOPS_AGE_KEY="+id.String())
	if err := cmd.Run(); err == nil {
		t.Fatalf("sops accepted a file whose namespace had been changed")
	}
}

func sopsBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("KELSON_SOPS_BIN"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("sops")
	if err != nil {
		t.Skip("no sops binary on PATH; set KELSON_SOPS_BIN or install github.com/getsops/sops/v3/cmd/sops")
	}
	return bin
}

func decodeBase64(t *testing.T, s string) string {
	t.Helper()
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
	return string(out)
}
