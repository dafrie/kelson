package sops

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"gopkg.in/yaml.v3"
)

// The plaintext these tests prove never reaches the output. It is a literal in
// a test file, which is the only place in this repository a credential-shaped
// string is allowed to be.
const (
	testURL   = "postgres://checkout:hunter2@db.internal:5432/checkout"
	testToken = "sk_live_51NotARealStripeKey"
)

func testSecret() Secret {
	return Secret{
		Name:      "checkout-db",
		Namespace: "checkout-production",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kelson",
			"kelson.dev/managed-secret":    "true",
			"kelson.dev/project":           "checkout",
		},
		Data: map[string][]byte{
			"url":   []byte(testURL),
			"token": []byte(testToken),
		},
	}
}

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generating a test age identity: %v", err)
	}
	return id
}

// TestRoundTrip is the acceptance criterion of issue #81 stated as a test:
// what kelson wrote decrypts with the matching age identity, and the values
// that come back are the values that went in.
//
// The decryptor below is written from the format description rather than from
// this package's own code, so it checks the encoder against the specification
// instead of against itself. TestSopsBinaryDecrypts checks it against the real
// implementation whenever one is on PATH.
func TestRoundTrip(t *testing.T) {
	id := newIdentity(t)
	out, err := EncryptSecret(testSecret(), Options{Recipients: []string{id.Recipient().String()}})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	got := decryptForTest(t, out, id)
	if got["url"] != testURL {
		t.Errorf("data.url did not round-trip")
	}
	if got["token"] != testToken {
		t.Errorf("data.token did not round-trip")
	}
}

// TestPlaintextNeverAppears is the other half of the acceptance criterion:
// nothing recognisable of the value survives into the bytes that are committed.
//
// Both spellings are checked. A Secret's `data` is base64, so a test that only
// looked for the raw string would pass for an implementation that forgot to
// encrypt and merely encoded.
func TestPlaintextNeverAppears(t *testing.T) {
	id := newIdentity(t)
	out, err := EncryptSecret(testSecret(), Options{Recipients: []string{id.Recipient().String()}})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	for _, plaintext := range []string{testURL, testToken} {
		if bytes.Contains(out, []byte(plaintext)) {
			t.Errorf("the encrypted file contains a plaintext value")
		}
		if bytes.Contains(out, []byte(base64.StdEncoding.EncodeToString([]byte(plaintext)))) {
			t.Errorf("the encrypted file contains a base64-encoded value")
		}
	}
	// The parts that must stay readable, because an unreviewable encrypted
	// file is what pushes people back to plaintext.
	for _, want := range []string{"checkout-db", "checkout-production", "url:", "token:", "kelson"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the encrypted file should still name %q in the clear", want)
		}
	}
}

// TestEveryValueIsEncrypted pins the encrypted_regex rule: everything under
// `data` is ciphertext and everything outside it is not. A regression that
// encrypted the whole document would still round-trip and would still hide the
// values — and would make the file useless to a reviewer and to kustomize.
func TestEveryValueIsEncrypted(t *testing.T) {
	id := newIdentity(t)
	out, err := EncryptSecret(testSecret(), Options{Recipients: []string{id.Recipient().String()}})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parsing the encrypted file: %v", err)
	}
	data, ok := doc["data"].(map[string]any)
	if !ok {
		t.Fatalf("the encrypted file has no data mapping")
	}
	for k, v := range data {
		if !strings.HasPrefix(fmt.Sprint(v), "ENC[AES256_GCM,") {
			t.Errorf("data.%s is not encrypted", k)
		}
	}
	if doc["kind"] != "Secret" || doc["apiVersion"] != "v1" || doc["type"] != "Opaque" {
		t.Errorf("the manifest's own identity must stay readable, got kind=%v apiVersion=%v type=%v",
			doc["kind"], doc["apiVersion"], doc["type"])
	}
	meta, ok := doc["metadata"].(map[string]any)
	if !ok || meta["name"] != "checkout-db" || meta["namespace"] != "checkout-production" {
		t.Errorf("metadata must stay readable, got %v", doc["metadata"])
	}
	// A label value of "true" is the shape that would be re-read as a bool and
	// break the MAC if the encoder did not quote it.
	labels, _ := meta["labels"].(map[string]any)
	if labels["kelson.dev/managed-secret"] != "true" {
		t.Errorf("a \"true\" label must survive as a string, got %#v", labels["kelson.dev/managed-secret"])
	}
}

// TestMultipleRecipients: every recipient's identity opens the file on its
// own. That is what makes key rotation gapless — add the new recipient,
// re-encrypt, hand over, then drop the old one.
func TestMultipleRecipients(t *testing.T) {
	a, b := newIdentity(t), newIdentity(t)
	out, err := EncryptSecret(testSecret(), Options{
		Recipients: []string{a.Recipient().String(), b.Recipient().String()},
	})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	for name, id := range map[string]*age.X25519Identity{"first": a, "second": b} {
		got := decryptForTest(t, out, id)
		if got["url"] != testURL {
			t.Errorf("the %s recipient could not read the file", name)
		}
	}
}

// TestWrongIdentityCannotDecrypt states the obvious property explicitly,
// because "it decrypted" is only evidence when "it did not decrypt" is also
// reachable.
func TestWrongIdentityCannotDecrypt(t *testing.T) {
	out, err := EncryptSecret(testSecret(), Options{Recipients: []string{newIdentity(t).Recipient().String()}})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if _, err := unwrapDataKey(out, newIdentity(t)); err == nil {
		t.Fatalf("an unrelated identity must not open the file")
	}
}

// TestMACCoversTheWholeDocument: tampering with a value kelson left in the
// clear must invalidate the file. This is why the MAC hashes unencrypted
// leaves too, and it is the property that stops a ciphertext being moved to a
// Secret of a different name in a different namespace.
func TestMACCoversTheWholeDocument(t *testing.T) {
	id := newIdentity(t)
	out, err := EncryptSecret(testSecret(), Options{Recipients: []string{id.Recipient().String()}})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	tampered := bytes.Replace(out, []byte("namespace: checkout-production"), []byte("namespace: kube-system     "), 1)
	if bytes.Equal(tampered, out) {
		t.Fatalf("the test could not find the namespace to tamper with")
	}
	if err := verifyMAC(tampered, id); err == nil {
		t.Fatalf("a moved Secret must fail its MAC check")
	}
	if err := verifyMAC(out, id); err != nil {
		t.Fatalf("the untampered file must pass its MAC check: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	id := newIdentity(t)
	ok := []string{id.Recipient().String()}
	cases := []struct {
		name string
		s    Secret
		opts Options
	}{
		{"no recipients", testSecret(), Options{}},
		{"not an age recipient", testSecret(), Options{Recipients: []string{"ssh-ed25519 AAAA"}}},
		{"no name", Secret{Namespace: "ns", Data: map[string][]byte{"k": []byte("v")}}, Options{Recipients: ok}},
		{"no namespace", Secret{Name: "n", Data: map[string][]byte{"k": []byte("v")}}, Options{Recipients: ok}},
		{"no keys", Secret{Name: "n", Namespace: "ns"}, Options{Recipients: ok}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncryptSecret(tc.s, tc.opts); err == nil {
				t.Fatalf("expected a refusal")
			}
		})
	}
}

// TestEntropyFailureIsReported: a Rand that cannot deliver must fail the
// encryption rather than produce a file with a short key.
func TestEntropyFailureIsReported(t *testing.T) {
	id := newIdentity(t)
	_, err := EncryptSecret(testSecret(), Options{
		Recipients: []string{id.Recipient().String()},
		Rand:       failingReader{},
	})
	if err == nil {
		t.Fatalf("expected the entropy failure to be reported")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestLastModifiedIsAnInput(t *testing.T) {
	id := newIdentity(t)
	stamp := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)
	out, err := EncryptSecret(testSecret(), Options{Recipients: []string{id.Recipient().String()}, Now: stamp})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if !bytes.Contains(out, []byte(`lastmodified: "2026-08-14T09:30:00Z"`)) {
		t.Errorf("lastmodified must be the timestamp the caller passed:\n%s", out)
	}
}

func TestInspectReadsThePublicHalf(t *testing.T) {
	a, b := newIdentity(t), newIdentity(t)
	recipients := []string{a.Recipient().String(), b.Recipient().String()}
	out, err := EncryptSecret(testSecret(), Options{Recipients: recipients})
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	f, err := Inspect(out)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if f.Name != "checkout-db" || f.Namespace != "checkout-production" {
		t.Errorf("Inspect read %s/%s", f.Namespace, f.Name)
	}
	if strings.Join(f.Keys, ",") != "token,url" {
		t.Errorf("keys = %v, want the sorted data keys", f.Keys)
	}
	if !f.EncryptedTo(recipients) {
		t.Errorf("EncryptedTo must recognise its own recipient set")
	}
	if f.EncryptedTo(recipients[:1]) {
		t.Errorf("a shrunk recipient set is drift, not a match")
	}
	if f.EncryptedTo(append(recipients, newIdentity(t).Recipient().String())) {
		t.Errorf("a grown recipient set is drift, not a match")
	}
}

func TestInspectRefusesUnencrypted(t *testing.T) {
	for _, in := range []string{
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: n\ndata:\n  k: dg==\n",
		"apiVersion: v1\nkind: Secret\nsops:\n  age: []\n",
		"- not a mapping\n",
		"\tnot yaml at all",
	} {
		if _, err := Inspect([]byte(in)); err == nil {
			t.Errorf("Inspect must refuse %q", in)
		}
	}
}

// --- an independent decryptor, written from the format description ---------

var encValue = regexp.MustCompile(`^ENC\[AES256_GCM,data:(.*),iv:(.*),tag:(.*),type:(.*)\]$`)

// decryptForTest opens the file the way kustomize-controller does: unwrap the
// data key with the age identity, decrypt each value with its own path as
// additional data, and check the MAC over what came back.
func decryptForTest(t *testing.T, encrypted []byte, id *age.X25519Identity) map[string]string {
	t.Helper()
	if err := verifyMAC(encrypted, id); err != nil {
		t.Fatalf("MAC check: %v", err)
	}
	dataKey, err := unwrapDataKey(encrypted, id)
	if err != nil {
		t.Fatalf("unwrapping the data key: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(encrypted, &doc); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	data := mapGet(documentRoot(&doc), "data")
	out := map[string]string{}
	for i := 0; i+1 < len(data.Content); i += 2 {
		key := data.Content[i].Value
		raw, err := decryptValue(data.Content[i+1].Value, dataKey, "data:"+key+":")
		if err != nil {
			t.Fatalf("decrypting data.%s: %v", key, err)
		}
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			t.Fatalf("base64-decoding data.%s: %v", key, err)
		}
		out[key] = string(decoded)
	}
	return out
}

// verifyMAC recomputes the SHA-512 over every leaf in document order, skipping
// the `sops` block, and compares it with the decrypted `mac` field.
func verifyMAC(encrypted []byte, id *age.X25519Identity) error {
	dataKey, err := unwrapDataKey(encrypted, id)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(encrypted, &doc); err != nil {
		return err
	}
	root := documentRoot(&doc)
	sopsBlock := mapGet(root, "sops")
	stamp := scalarOf(mapGet(sopsBlock, "lastmodified"))

	digest := sha512.New()
	var visit func(n *yaml.Node, path []string) error
	visit = func(n *yaml.Node, path []string) error {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i].Value
				if len(path) == 0 && key == "sops" {
					continue
				}
				if err := visit(n.Content[i+1], append(path, key)); err != nil {
					return err
				}
			}
			return nil
		case yaml.ScalarNode:
			value := n.Value
			if encValue.MatchString(value) {
				plain, err := decryptValue(value, dataKey, strings.Join(path, ":")+":")
				if err != nil {
					return err
				}
				value = plain
			}
			digest.Write([]byte(value))
			return nil
		default:
			return fmt.Errorf("unexpected node kind %v", n.Kind)
		}
	}
	if err := visit(root, nil); err != nil {
		return err
	}

	want, err := decryptValue(scalarOf(mapGet(sopsBlock, "mac")), dataKey, stamp)
	if err != nil {
		return fmt.Errorf("decrypting the mac: %w", err)
	}
	if got := fmt.Sprintf("%X", digest.Sum(nil)); got != want {
		return fmt.Errorf("mac mismatch: computed %s, file says %s", got, want)
	}
	return nil
}

func unwrapDataKey(encrypted []byte, id *age.X25519Identity) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(encrypted, &doc); err != nil {
		return nil, err
	}
	ages := mapGet(mapGet(documentRoot(&doc), "sops"), "age")
	if ages == nil {
		return nil, errors.New("no age recipients")
	}
	var last error
	for _, entry := range ages.Content {
		enc := scalarOf(mapGet(entry, "enc"))
		r, err := age.Decrypt(armor.NewReader(strings.NewReader(enc)), id)
		if err != nil {
			last = err
			continue
		}
		key, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	if last == nil {
		last = errors.New("no age entry could be opened")
	}
	return nil, last
}

func decryptValue(value string, dataKey []byte, additionalData string) (string, error) {
	m := encValue.FindStringSubmatch(value)
	if m == nil {
		return "", fmt.Errorf("%q is not a sops value", value)
	}
	body, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil {
		return "", err
	}
	tag, err := base64.StdEncoding.DecodeString(m[3])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, append(body, tag...), []byte(additionalData))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
