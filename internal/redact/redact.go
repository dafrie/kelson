package redact

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"
)

// Sentinel is what every redacted value becomes. It is a fixed string rather
// than a length-preserving mask on purpose: a mask that echoes the length of a
// credential leaks the length of the credential, and a reader must be able to
// tell "this was redacted" from "this happened to be that value".
const Sentinel = "[redacted]"

// secretKeys are the two places a Kubernetes Secret keeps values. Keys are
// preserved everywhere — a key name is a reference and referencing a secret is
// the whole point of ADR-0009; only the value on the right-hand side goes.
var secretKeys = [...]string{"data", "stringData"}

// lastAppliedKey is kubectl's client-side apply annotation. On a Secret read
// back from a live cluster it carries a full JSON copy of the object,
// data values included, which is why an L2 preview must not print it verbatim.
const lastAppliedKey = "kubectl.kubernetes.io/last-applied-configuration"

// Document redacts one YAML (or JSON — JSON is valid YAML) document.
//
// A document that is not a Secret is returned byte-identical, so redaction
// never reformats output it had no reason to touch. A document that IS a Secret
// is re-encoded, which normalises its formatting; that is acceptable precisely
// because a redacted document is display output by construction and can never
// be applied (see the package doc).
func Document(doc []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		// Not parseable as YAML. There is nothing structural to redact, and
		// failing here would turn "kelson cannot pretty-print this" into "kelson
		// cannot show you this at all". The known-value Scrubber is the tool for
		// unstructured bytes.
		return doc, nil //nolint:nilerr // unparseable input is returned untouched by design
	}
	if !Node(&root) {
		return doc, nil
	}
	return encode(&root)
}

// Documents redacts each document of a manifest set independently, preserving
// order. The slice is not aliased: callers hold the real bytes and must keep
// holding them.
func Documents(docs [][]byte) ([][]byte, error) {
	out := make([][]byte, 0, len(docs))
	for _, d := range docs {
		red, err := Document(d)
		if err != nil {
			return nil, err
		}
		out = append(out, red)
	}
	return out, nil
}

// Node redacts a parsed YAML tree in place and reports whether anything
// changed. It descends through documents, sequences and mappings, so a Secret
// nested in a List (or in any wrapper a future caller invents) is found rather
// than missed.
func Node(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	if n.Kind == yaml.MappingNode && isSecret(n) {
		changed = redactSecretMapping(n)
	}
	for _, c := range n.Content {
		if Node(c) {
			changed = true
		}
	}
	return changed
}

// Value redacts one decoded value found at a field path inside a resource of
// the given kind. It is the entry point for callers that hold a diff rather
// than a document: internal/diff walks (kind, path, before, after) tuples and
// never has the surrounding YAML.
//
// Three shapes are covered, which is every shape a Secret's values can take in
// a field diff:
//
//   - the whole data/stringData mapping was added or removed, so v is a map
//     whose values all go;
//   - one key under it changed, so v is the leaf itself;
//   - the object was read back from a live cluster carrying kubectl's
//     last-applied-configuration annotation, which embeds the entire Secret as
//     JSON. That value is replaced wholesale rather than parsed: it is a copy
//     of the object, so nothing in it is worth showing that the diff does not
//     already show. It is caught whether it arrives as its own field path or
//     inside a whole `metadata.annotations` mapping, because an L2 readback
//     emits either depending on what the live object already had.
func Value(kind, path string, v any) any {
	if v == nil || kind != "Secret" {
		return v
	}
	if strings.Contains(path, lastAppliedKey) {
		return Sentinel
	}
	for _, key := range secretKeys {
		switch {
		case path == key:
			return redactedMap(v)
		case strings.HasPrefix(path, key+"."):
			return Sentinel
		}
	}
	return redactLastApplied(v)
}

// redactLastApplied replaces the kubectl apply annotation wherever it appears
// inside a decoded value. The recursion is there because the annotation can
// arrive nested at any depth the diff chose to cut at: as `metadata.annotations`
// (a mapping), as `metadata` (a mapping of mappings), or as the whole object.
//
// A value it does not change is returned as-is, so nothing is copied unless
// something was actually redacted and the caller's tree is never mutated
// through an alias.
func redactLastApplied(v any) any {
	out, changed := walkLastApplied(v)
	if !changed {
		return v
	}
	return out
}

func walkLastApplied(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		changed := false
		for k, val := range t {
			if strings.Contains(k, lastAppliedKey) {
				out[k] = Sentinel
				changed = true
				continue
			}
			rv, c := walkLastApplied(val)
			out[k] = rv
			changed = changed || c
		}
		if !changed {
			return t, false
		}
		return out, true
	case []any:
		out := make([]any, len(t))
		changed := false
		for i, item := range t {
			rv, c := walkLastApplied(item)
			out[i] = rv
			changed = changed || c
		}
		if !changed {
			return t, false
		}
		return out, true
	default:
		return v, false
	}
}

// redactedMap replaces every value of a decoded data/stringData mapping,
// returning a new map so the caller's tree is never mutated through an alias.
// A non-map value at that path means the document is malformed; it is redacted
// wholesale rather than passed through, because "unexpected shape under
// data" is not a reason to print it.
func redactedMap(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return Sentinel
	}
	out := make(map[string]any, len(m))
	for k := range m {
		out[k] = Sentinel
	}
	return out
}

// isSecret reports whether a mapping node is a core/v1 Secret.
//
// The apiVersion is checked so a custom resource that happens to be called
// Secret in another API group is not silently emptied out. An absent
// apiVersion is accepted: a patch fragment naming kind: Secret is still a
// Secret, and refusing to redact it would be the wrong way round.
func isSecret(m *yaml.Node) bool {
	if scalarAt(m, "kind") != "Secret" {
		return false
	}
	switch scalarAt(m, "apiVersion") {
	case "", "v1":
		return true
	default:
		return false
	}
}

// redactSecretMapping replaces the values under data and stringData, leaving
// the keys and their order exactly as they were.
func redactSecretMapping(m *yaml.Node) bool {
	changed := false
	for _, key := range secretKeys {
		values := mapGet(m, key)
		if values == nil || values.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(values.Content); i += 2 {
			if replaceScalar(values.Content[i+1]) {
				changed = true
			}
		}
	}
	return changed
}

// replaceScalar overwrites a value node with the sentinel, collapsing any
// composite value to it. Style is cleared so the result is a plain scalar
// whatever the input was (a block literal, a quoted string, an anchor).
func replaceScalar(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.ScalarNode && n.Value == Sentinel && n.Tag == "!!str" {
		return false
	}
	*n = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: Sentinel}
	return true
}

func scalarAt(m *yaml.Node, key string) string {
	if v := mapGet(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// encode writes a node back out with the same two-space indent the renderer
// uses (internal/renderer/manifest.go), so redacted output reads like the
// output it stands in for.
func encode(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
