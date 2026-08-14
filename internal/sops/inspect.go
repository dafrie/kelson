package sops

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is what can be read out of an encrypted file without holding a key.
//
// SOPS encrypts values, not structure: the mapping keys, the Secret's name and
// namespace, and the list of recipients the data key was wrapped for are all
// in the clear. That is what makes an encrypted Secret reviewable in a pull
// request, and it is what lets `kelson secret list` and `kelson secret rotate`
// answer their questions with no key material anywhere near the process.
//
// There is deliberately no field a value could be in. Every other type in
// kelson that describes a Secret has the same property (internal/secret's
// Secret, the SecretSummary on the wire), and this one keeps it for the same
// reason: a formatter cannot leak what it was never handed.
type File struct {
	// Name and Namespace are the Secret's, read from metadata.
	Name      string
	Namespace string
	// Keys are the Secret's data keys, in the order the file lists them
	// (which is sorted, because that is how kelson wrote it).
	Keys []string
	// Recipients are the age public keys the data key was wrapped for. A file
	// can be opened by any identity matching any one of them.
	Recipients []string
}

// EncryptedTo reports whether this file's recipient set is exactly want,
// ignoring order. It is the question `kelson secret rotate` asks of every file
// in the delivery repository: a file wrapped for a key that is no longer in
// the spec is still readable by whoever holds that key, and a file *not*
// wrapped for a key that is in the spec cannot be read by its holder. Both are
// drift, and both are visible without decrypting anything.
func (f File) EncryptedTo(want []string) bool {
	if len(f.Recipients) != len(want) {
		return false
	}
	have := make(map[string]bool, len(f.Recipients))
	for _, r := range f.Recipients {
		have[r] = true
	}
	for _, r := range want {
		if !have[strings.TrimSpace(r)] {
			return false
		}
	}
	return true
}

// Inspect reads an encrypted Secret manifest's public half.
//
// It refuses a document with no `sops:` block rather than reporting an empty
// recipient list: a plaintext Secret sitting where an encrypted one belongs is
// the one thing this backend exists to prevent, and reading it as "encrypted
// to nobody" would let `rotate` report it as merely stale.
func Inspect(encrypted []byte) (File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(encrypted, &doc); err != nil {
		return File{}, fmt.Errorf("sops: parsing the encrypted file: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return File{}, errors.New("sops: the encrypted file is not a YAML mapping")
	}

	meta := mapGet(root, "metadata")
	out := File{
		Name:      scalarOf(mapGet(meta, "name")),
		Namespace: scalarOf(mapGet(meta, "namespace")),
	}
	if data := mapGet(root, "data"); data != nil && data.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(data.Content); i += 2 {
			out.Keys = append(out.Keys, data.Content[i].Value)
		}
	}

	sopsBlock := mapGet(root, "sops")
	if sopsBlock == nil {
		return File{}, errors.New("sops: the file carries no sops metadata, so it is not encrypted")
	}
	ages := mapGet(sopsBlock, "age")
	if ages == nil || ages.Kind != yaml.SequenceNode || len(ages.Content) == 0 {
		return File{}, errors.New("sops: the file is not encrypted to any age recipient")
	}
	for _, entry := range ages.Content {
		r := scalarOf(mapGet(entry, "recipient"))
		if r == "" {
			return File{}, errors.New("sops: an age entry names no recipient")
		}
		out.Recipients = append(out.Recipients, r)
	}
	return out, nil
}

func documentRoot(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalarOf(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}
