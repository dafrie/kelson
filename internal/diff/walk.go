package diff

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The recursive, semantic diff over two yaml.Node trees. It walks both trees
// in document order and emits one FieldDiff per changed leaf (or per added or
// removed named item), so the output is deterministic and short — the two
// things that make a diff trustworthy and readable (issue #42).
//
// Provenance noise (issue #36) is suppressed by name: kelson's reserved
// bookkeeping keys never surface as fields. A renderer upgrade therefore
// produces an empty diff, while a real spec change always surfaces through
// the offending field.

// provenanceKeys are the reserved kelson bookkeeping keys suppressed from the
// diff wherever they appear.
var provenanceKeys = []string{
	"kelson.dev/renderer-version",
	"kelson.dev/spec-hash",
	"kelson.dev/overlays",
	"kelson.dev/project",
	"kelson.dev/component",
	"kelson.dev/environment",
	"app.kubernetes.io/managed-by",
}

// isProvenanceKey reports whether key is one of the reserved bookkeeping keys
// stamped by the renderer. These are suppressed because they are derived from
// (or orthogonal to) user intent: a renderer-version change alone is a plain
// upgrade and must not diff (issue #42). spec-hash is signal rather than pure
// noise — the renderer derives it from the resolved spec, so a change to it is
// always accompanied by the real field change that caused it, which we do
// report; suppressing the annotation itself never masks that underlying edit.
func isProvenanceKey(key string) bool {
	for _, k := range provenanceKeys {
		if key == k {
			return true
		}
	}
	return false
}

// diffResourceFields diffs the body of one resource (root mapping including
// metadata and spec) between prev (before) and cur (after). The root mappings
// are diffed with an empty base path, so field paths read spec.* and
// metadata.*.
func diffResourceFields(prev, cur resource, origin OriginFor) []FieldDiff {
	var fields []FieldDiff
	diffMappings(&fields, prev.ref, prev.ref.Kind, prev.root, cur.root, "", origin)
	return fields
}

// diffNodes compares a single value slot between before (prev) and after
// (cur), appending one FieldDiff per change onto out.
func diffNodes(out *[]FieldDiff, ref ResourceRef, kind, path string, prev, cur *yaml.Node, origin OriginFor) {
	switch {
	case prev == nil:
		*out = append(*out, newField(ref, path, nil, nodeValue(cur), origin))
	case cur == nil:
		*out = append(*out, newField(ref, path, nodeValue(prev), nil, origin))
	case prev.Kind != cur.Kind:
		*out = append(*out, newField(ref, path, nodeValue(prev), nodeValue(cur), origin))
	default:
		switch prev.Kind {
		case yaml.ScalarNode:
			if scalarEqual(prev, cur) {
				return
			}
			*out = append(*out, newField(ref, path, scalarDecode(prev), scalarDecode(cur), origin))
		case yaml.MappingNode:
			diffMappings(out, ref, kind, prev, cur, path, origin)
		case yaml.SequenceNode:
			diffSequences(out, ref, kind, prev, cur, path, origin)
		}
	}
}

// diffMappings diffs two mapping nodes key-by-key, emitting before-removed and
// after-only keys in document order so the output never depends on map
// iteration.
func diffMappings(out *[]FieldDiff, ref ResourceRef, kind string, prev, cur *yaml.Node, path string, origin OriginFor) {
	curIndex := indexByKey(cur)

	// Track which keys exist in cur so after-only keys are seen later.
	processed := map[string]bool{}
	for i := 0; i+1 < len(prev.Content); i += 2 {
		key := prev.Content[i].Value
		if isProvenanceKey(key) {
			continue
		}
		processed[key] = true
		after, ok := curIndex[key]
		if !ok {
			*out = append(*out, newField(ref, joinPath(path, key), nodeValue(prev.Content[i+1]), nil, origin))
			continue
		}
		diffNodes(out, ref, kind, joinPath(path, key), prev.Content[i+1], after, origin)
	}

	for j := 0; j+1 < len(cur.Content); j += 2 {
		key := cur.Content[j].Value
		if isProvenanceKey(key) {
			continue
		}
		if processed[key] {
			continue
		}
		*out = append(*out, newField(ref, joinPath(path, key), nil, nodeValue(cur.Content[j+1]), origin))
	}
}

// diffSequences diffs two sequence nodes. Sequences of mappings keyed by
// `name` (env, containers, ports, ...) merge per name — the Kubernetes
// authoring convention; anything else is compared positionally.
func diffSequences(out *[]FieldDiff, ref ResourceRef, kind string, prev, cur *yaml.Node, path string, origin OriginFor) {
	if mergeableByName(prev) && mergeableByName(cur) && !seqIndexStyle(path) {
		diffNamedSequence(out, ref, kind, prev, cur, path, origin)
		return
	}

	n := len(prev.Content)
	if len(cur.Content) < n {
		n = len(cur.Content)
	}
	for i := 0; i < n; i++ {
		diffNodes(out, ref, kind, joinIndex(path, i), prev.Content[i], cur.Content[i], origin)
	}
	for i := n; i < len(cur.Content); i++ {
		*out = append(*out, newField(ref, joinIndex(path, i), nil, nodeValue(cur.Content[i]), origin))
	}
	for i := n; i < len(prev.Content); i++ {
		*out = append(*out, newField(ref, joinIndex(path, i), nodeValue(prev.Content[i]), nil, origin))
	}
}

// diffNamedSequence diffs name-keyed mapping sequences, joining by name and
// walking names in before-then-after document order.
func diffNamedSequence(out *[]FieldDiff, ref ResourceRef, kind string, prev, cur *yaml.Node, path string, origin OriginFor) {
	curIndex := indexByName(cur)

	processed := map[string]bool{}
	for _, item := range prev.Content {
		name := namedKey(item)
		if name == "" {
			continue
		}
		processed[name] = true
		after, ok := curIndex[name]
		if !ok {
			*out = append(*out, newField(ref, joinKey(path, name), nodeValue(item), nil, origin))
			continue
		}
		diffMappings(out, ref, kind, item, after, joinKey(path, name), origin)
	}

	for _, item := range cur.Content {
		name := namedKey(item)
		if name == "" || processed[name] {
			continue
		}
		*out = append(*out, newField(ref, joinKey(path, name), nil, nodeValue(item), origin))
	}
}

// newField builds one FieldDiff, attributing origin and risk.
func newField(ref ResourceRef, path string, before, after any, origin OriginFor) FieldDiff {
	o := origin(ref, path)
	if o == "" {
		o = OriginSpec
	}
	return FieldDiff{
		Path:   path,
		Before: before,
		After:  after,
		Origin: o,
		Risk:   fieldRisk(ref.Kind, path),
	}
}

// fieldRisk classifies a single field change in Kubernetes terms (issue #42):
//   - under a workload's pod template -> the pods roll: restart-required
//   - an immutable field (Deployment/StatefulSet selector, Service clusterIP,
//     StatefulSet volumeClaimTemplates) -> the API server would reject the
//     write or it is destructive: disruptive
//   - everything else under spec -> additive config change
func fieldRisk(kind, path string) Risk {
	if strings.HasPrefix(path, "spec.template.") ||
		strings.HasPrefix(path, "spec.jobTemplate.spec.template.") {
		return RiskRestart
	}
	switch {
	case kind == "Service" && (path == "spec.clusterIP" || path == "spec.selector" || strings.HasPrefix(path, "spec.selector.")):
		return RiskDisruptive
	case (kind == "Deployment" || kind == "StatefulSet") && (path == "spec.selector" || strings.HasPrefix(path, "spec.selector.")):
		return RiskDisruptive
	case kind == "StatefulSet" && strings.HasPrefix(path, "spec.volumeClaimTemplates"):
		return RiskDisruptive
	}
	return RiskAdditive
}

// --- helpers ----------------------------------------------------------------

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
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

// indexByKey maps mapping keys to their value nodes in a mapping node,
// iterating the alternating key/value content explicitly so a scalar value is
// never mistaken for a key.
func indexByKey(m *yaml.Node) map[string]*yaml.Node {
	index := map[string]*yaml.Node{}
	if m == nil || m.Kind != yaml.MappingNode {
		return index
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		index[m.Content[i].Value] = m.Content[i+1]
	}
	return index
}

// indexByName maps name-keyed sequence items to the items themselves.
func indexByName(m *yaml.Node) map[string]*yaml.Node {
	index := map[string]*yaml.Node{}
	for _, item := range m.Content {
		if name := namedKey(item); name != "" {
			index[name] = item
		}
	}
	return index
}

func namedKey(m *yaml.Node) string {
	if m == nil || m.Kind != yaml.MappingNode {
		return ""
	}
	if n := mapGet(m, "name"); n != nil {
		return n.Value
	}
	return ""
}

func mergeableByName(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		return false
	}
	for _, item := range n.Content {
		if namedKey(item) == "" {
			return false
		}
	}
	return true
}

func scalarEqual(a, b *yaml.Node) bool {
	return a.Tag == b.Tag && a.Value == b.Value
}

// scalarDecode lifts a scalar node into a Go scalar for Before/After.
func scalarDecode(n *yaml.Node) any {
	switch n.Tag {
	case "!!null":
		return nil
	case "!!bool":
		v, _ := strconv.ParseBool(n.Value)
		return v
	case "!!int":
		v, _ := strconv.Atoi(n.Value)
		return v
	case "!!float":
		v, _ := strconv.ParseFloat(n.Value, 64)
		return v
	default:
		return n.Value
	}
}

// nodeValue decodes a (possibly composite) node into a Go value. Mapping and
// sequence nodes are decoded through a document wrapper into map/slice values;
// JSON marshals those with sorted keys, so an added/removed subtree still
// produces deterministic output.
func nodeValue(n *yaml.Node) any {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return scalarDecode(n)
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{n}}
	var v any
	if err := doc.Decode(&v); err != nil {
		return nil
	}
	return v
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func joinIndex(base string, i int) string {
	return base + "[" + strconv.Itoa(i) + "]"
}

func joinKey(base, name string) string {
	return base + "[" + name + "]"
}

// seqIndexStyle reports whether a sequence is addressed positionally ([n])
// rather than by name ([name]). The contract's canonical examples are
// containers[0] and env[DATABASE_URL]: pod-spec container lists are indexed,
// while every other name-keyed list (env, ports, volumes, ...) is keyed by
// the strategic-merge identity name.
func seqIndexStyle(path string) bool {
	return strings.HasSuffix(path, "containers")
}
