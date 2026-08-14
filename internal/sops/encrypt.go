package sops

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"gopkg.in/yaml.v3"
)

// Version is the sops file-format version kelson writes into `sops.version`.
//
// It is the release whose behaviour this package was read against and whose
// binary the cross-implementation test in encrypt_sops_test.go decrypts with.
// The field is not optional in the format and a made-up value would be a claim
// nobody could check; this one is a claim the test suite checks whenever a
// `sops` binary is on PATH.
const Version = "3.13.3"

// EncryptedRegex limits encryption to a Secret's values. It is the expression
// Flux's own SOPS guide uses, and it is written into every file kelson
// produces rather than left to a repository's `.sops.yaml`: the rule that
// decides which bytes are secret belongs in the file whose bytes they are.
//
// Keeping `metadata`, `type` and `apiVersion` in the clear is what makes an
// encrypted Secret reviewable — a pull request shows which Secret in which
// namespace gained which key, and shows the value as ciphertext.
const EncryptedRegex = `^(data|stringData)$`

// nonceSize is sops' AES-GCM nonce length. It is 32, not the 12 bytes
// crypto/cipher defaults to, so the GCM instance has to be built with
// NewGCMWithNonceSize or every value kelson wrote would fail to decrypt.
const nonceSize = 32

// gcmTagSize is the trailing authentication tag Seal appends, which sops
// stores in its own `tag:` field rather than leaving it on the ciphertext.
const gcmTagSize = 16

// dataKeySize is the length of the per-file data key: AES-256.
const dataKeySize = 32

// Secret is the one document this package encrypts.
//
// It is a narrow type on purpose. The MAC of a SOPS file covers every leaf
// value in document order, so the encoder and the MAC must agree on that
// order; taking a fixed shape rather than an arbitrary tree means there is one
// traversal, written once, and no way for a caller to produce a document whose
// MAC describes a different ordering than its bytes.
type Secret struct {
	// Name and Namespace address the Secret. Both are emitted in the clear —
	// they are what makes the encrypted file reviewable.
	Name      string
	Namespace string
	// Labels are emitted in the clear, sorted by key.
	Labels map[string]string
	// Data are the credential keys. Values are the raw bytes; they are
	// base64-encoded (as a Kubernetes Secret's `data` requires) and then
	// encrypted. A caller that holds a value has already registered it with
	// internal/redact.
	Data map[string][]byte
}

// Options configures one encryption.
type Options struct {
	// Recipients are age public keys ("age1…"). At least one is required. The
	// data key is wrapped once per recipient, so any one of the corresponding
	// identities can open the file — which is what makes "add the new key,
	// then remove the old one" a rotation with no gap.
	Recipients []string

	// Now is the `lastmodified` timestamp. It is authenticated data for the
	// MAC, so it is an input rather than a call to time.Now inside: a test can
	// pin it, and nothing about the output depends on an ambient clock except
	// through this field. Zero means time.Now().UTC().
	Now time.Time

	// Rand is the entropy for the data key and the per-value nonces. Zero
	// means crypto/rand.Reader. It is injectable so a test can prove the
	// failure path, never so that output becomes predictable — age draws its
	// own randomness regardless, so an encryption is never reproducible.
	Rand io.Reader
}

var encryptedPath = regexp.MustCompile(EncryptedRegex)

// EncryptSecret returns the bytes of a SOPS-encrypted Secret manifest.
//
// The plaintext exists only as the caller's own Data map and as the byte
// slices of the AES-GCM seals below. Nothing here writes a file, and the
// returned bytes carry no plaintext: the values are ciphertext and everything
// else in the document is metadata a reviewer is meant to read.
func EncryptSecret(s Secret, opts Options) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	recipients, err := parseRecipients(opts.Recipients)
	if err != nil {
		return nil, err
	}

	entropy := opts.Rand
	if entropy == nil {
		entropy = rand.Reader
	}
	dataKey := make([]byte, dataKeySize)
	if _, err := io.ReadFull(entropy, dataKey); err != nil {
		return nil, fmt.Errorf("sops: drawing a data key: %w", err)
	}

	doc := s.document()
	mac, err := encryptTree(doc, dataKey, entropy)
	if err != nil {
		return nil, err
	}

	lastModified := opts.Now
	if lastModified.IsZero() {
		lastModified = time.Now()
	}
	stamp := lastModified.UTC().Format(time.RFC3339)
	encryptedMAC, err := encryptValue(mac, dataKey, stamp, entropy)
	if err != nil {
		return nil, fmt.Errorf("sops: encrypting the file MAC: %w", err)
	}

	wrapped, err := wrapDataKey(dataKey, opts.Recipients, recipients)
	if err != nil {
		return nil, err
	}
	mapSet(doc, "sops", mapNode(
		"age", wrapped,
		"encrypted_regex", strNode(EncryptedRegex),
		"lastmodified", strNode(stamp),
		"mac", strNode(encryptedMAC),
		"version", strNode(Version),
	))

	return encode(doc)
}

func (s Secret) validate() error {
	switch {
	case s.Name == "":
		return errors.New("sops: a Secret needs a name")
	case s.Namespace == "":
		return errors.New("sops: a Secret needs a namespace")
	case len(s.Data) == 0:
		return errors.New("sops: a Secret with no keys would encrypt nothing")
	}
	return nil
}

// document builds the plaintext manifest as an ordered YAML tree.
//
// Every scalar is tagged `!!str` explicitly. That is not cosmetic: a label
// value of "true" or a base64 blob that happens to be all digits would be
// re-read as a bool or an int by whoever decrypts the file, and the MAC — which
// hashes the *value*, not its spelling — would then be computed over "True" or
// "12345" and fail to match. Tagging forces the encoder to quote whatever
// needs quoting.
func (s Secret) document() *yaml.Node {
	metadata := mapNode("name", strNode(s.Name), "namespace", strNode(s.Namespace))
	if len(s.Labels) > 0 {
		labels := mapNode()
		for _, k := range sortedKeys(s.Labels) {
			mapSet(labels, k, strNode(s.Labels[k]))
		}
		mapSet(metadata, "labels", labels)
	}

	data := mapNode()
	for _, k := range sortedByteKeys(s.Data) {
		data.Content = append(data.Content, strNode(k),
			strNode(base64.StdEncoding.EncodeToString(s.Data[k])))
	}

	return mapNode(
		"apiVersion", strNode("v1"),
		"kind", strNode("Secret"),
		"metadata", metadata,
		"type", strNode("Opaque"),
		"data", data,
	)
}

// encryptTree replaces every encryptable leaf with its ciphertext and returns
// the file MAC: the uppercase hex SHA-512 of every leaf's plaintext bytes in
// document order.
//
// Both halves happen in one traversal because they must see the same order.
// Unencrypted leaves are hashed too — sops' `mac_only_encrypted` defaults to
// off, so `apiVersion`, `kind` and the metadata are part of what the MAC
// authenticates, which is what stops an attacker moving a ciphertext to a
// Secret of another name.
func encryptTree(doc *yaml.Node, dataKey []byte, entropy io.Reader) (string, error) {
	digest := sha512.New()
	if err := walk(doc, nil, digest, dataKey, entropy); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X", digest.Sum(nil)), nil
}

func walk(node *yaml.Node, path []string, digest hash.Hash, dataKey []byte, entropy io.Reader) error {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode {
				return errors.New("sops: only string keys can be encrypted")
			}
			if err := walk(value, append(path, key.Value), digest, dataKey, entropy); err != nil {
				return err
			}
		}
		return nil
	case yaml.ScalarNode:
		digest.Write([]byte(node.Value))
		if !shouldEncrypt(path) {
			return nil
		}
		out, err := encryptValue(node.Value, dataKey, strings.Join(path, ":")+":", entropy)
		if err != nil {
			return fmt.Errorf("sops: encrypting %s: %w", strings.Join(path, "."), err)
		}
		node.Value = out
		return nil
	default:
		// Sequences are the one shape sops supports that this package does
		// not. A Secret manifest has none, and guessing at sops' path spelling
		// for a list element would produce a file that decrypts everywhere
		// except where it matters.
		return fmt.Errorf("sops: cannot encrypt a %v node", node.Kind)
	}
}

// shouldEncrypt is sops' encrypted_regex rule: a leaf is encrypted when *any*
// element of its path matches. `data.url` matches on `data`; `metadata.name`
// matches on neither.
func shouldEncrypt(path []string) bool {
	for _, p := range path {
		if encryptedPath.MatchString(p) {
			return true
		}
	}
	return false
}

// encryptValue is sops' value encoding: AES-256-GCM with a fresh 32-byte
// nonce, the tag split out of the ciphertext into its own field, and the
// value's path as additional authenticated data.
func encryptValue(plaintext string, dataKey []byte, additionalData string, entropy io.Reader) (string, error) {
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, nonceSize)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), []byte(additionalData))
	body, tag := sealed[:len(sealed)-gcmTagSize], sealed[len(sealed)-gcmTagSize:]
	return fmt.Sprintf("ENC[AES256_GCM,data:%s,iv:%s,tag:%s,type:str]",
		base64.StdEncoding.EncodeToString(body),
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(tag)), nil
}

// parseRecipients turns the spec's `age1…` strings into age recipients.
//
// Only X25519 recipients are accepted. sops also speaks age's SSH recipients,
// and they are deliberately not supported here: an SSH key that decrypts a
// production credential is usually a key that also logs in somewhere, and the
// one-line remediation ("run age-keygen") is better than a second key type
// with its own failure modes.
func parseRecipients(recipients []string) ([]age.Recipient, error) {
	if len(recipients) == 0 {
		return nil, errors.New("sops: encryption needs at least one age recipient")
	}
	out := make([]age.Recipient, 0, len(recipients))
	for _, r := range recipients {
		parsed, err := age.ParseX25519Recipient(strings.TrimSpace(r))
		if err != nil {
			return nil, fmt.Errorf("sops: %q is not an age recipient: %w", r, err)
		}
		out = append(out, parsed)
	}
	return out, nil
}

// wrapDataKey encrypts the data key once per recipient and renders sops'
// `age:` list. Each entry is the ASCII-armoured age file holding the data key,
// beside the recipient it was encrypted to — the recipient is stored in the
// clear so a reader (and `kelson secret rotate`) can tell which keys open a
// file without holding any of them.
func wrapDataKey(dataKey []byte, spelled []string, recipients []age.Recipient) (*yaml.Node, error) {
	items := make([]*yaml.Node, 0, len(recipients))
	for i, r := range recipients {
		var buf bytes.Buffer
		armored := armor.NewWriter(&buf)
		w, err := age.Encrypt(armored, r)
		if err != nil {
			return nil, fmt.Errorf("sops: wrapping the data key for %s: %w", spelled[i], err)
		}
		if _, err := w.Write(dataKey); err != nil {
			return nil, fmt.Errorf("sops: wrapping the data key for %s: %w", spelled[i], err)
		}
		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("sops: wrapping the data key for %s: %w", spelled[i], err)
		}
		if err := armored.Close(); err != nil {
			return nil, fmt.Errorf("sops: wrapping the data key for %s: %w", spelled[i], err)
		}
		items = append(items, mapNode(
			"enc", blockNode(buf.String()),
			"recipient", strNode(strings.TrimSpace(spelled[i])),
		))
	}
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}, nil
}

// encode renders the document. Two spaces of indentation is kelson's house
// style everywhere else it writes YAML; sops' own emitter uses four, and the
// difference is invisible to every reader of the format.
func encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("sops: encoding the encrypted document: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("sops: encoding the encrypted document: %w", err)
	}
	return buf.Bytes(), nil
}

// --- node helpers --------------------------------------------------------

func strNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// blockNode emits a string as a literal block scalar, which is how sops writes
// the armoured age blocks and the only styling that keeps them readable.
func blockNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle, Value: v}
}

func mapNode(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		mapSet(n, kv[i].(string), kv[i+1].(*yaml.Node))
	}
	return n
}

func mapSet(n *yaml.Node, key string, value *yaml.Node) {
	n.Content = append(n.Content, strNode(key), value)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedByteKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
