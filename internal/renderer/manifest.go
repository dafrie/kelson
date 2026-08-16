package renderer

import (
	"bytes"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Manifest is a single rendered Kubernetes resource: a structured YAML
// document plus the identity (apiVersion/kind/name/namespace) used for
// overlay targeting and output naming. The document is an ordered yaml.Node
// tree — rendering never relies on Go map iteration order.
type Manifest struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string

	// Stage says which of the two delivery stages this resource belongs to
	// when the set carries a release hook. It is a fact about the resource, not
	// a delivery decision: the renderer says "this Job must finish before the
	// workloads roll", and the controller decides that the way to express that
	// is two Kustomizations with a dependsOn (release.go, ADR-0028 decision 3).
	Stage Stage

	doc *yaml.Node
}

// Stage is a rendered resource's position relative to the release hook
// (issue #227, ADR-0019 decision 2).
//
// A rendered set expresses sequencing only through order, and order is not a
// wait (issue #89). What a release hook needs is a *barrier*: the migration has
// to finish before the workloads roll. The renderer cannot build a barrier — it
// emits manifests — but it is the only thing that knows which side of one each
// resource belongs on, so it says so here and the delivery plane builds the
// barrier out of it.
//
// The three values are a partition into two sets, not three:
//
//	release stage  = StagePrerequisite + StageRelease
//	workload stage = StagePrerequisite + StageWorkload
//
// StagePrerequisite is in *both* deliberately. The Job's pod needs the
// Namespace, the ServiceAccount it names, the Secrets its env references and
// the database it migrates, and duplicating those into the release stage is
// what lets the release stage stand up on its own — while leaving the workload
// stage's ownership of them exactly where it already was, so nothing changes
// about what prunes what (internal/controller/fluxobjects.go).
type Stage uint8

const (
	// StageWorkload is the default and the whole set when nothing declares a
	// release hook: the resources that roll once the hook has succeeded.
	StageWorkload Stage = iota
	// StagePrerequisite is a resource both stages apply — everything the
	// renderer emits before the release Job, plus the ServiceAccount the Job's
	// pod names.
	StagePrerequisite
	// StageRelease is the release stage alone: the hook Jobs, which must never
	// reach the Kustomization that applies the workloads.
	StageRelease
)

// HasReleaseStage reports whether a rendered set carries a release hook, which
// is the one thing that makes the split worth paying for. A set without one is
// published and delivered exactly as it was before #227.
func HasReleaseStage(manifests []Manifest) bool {
	for _, m := range manifests {
		if m.Stage == StageRelease {
			return true
		}
	}
	return false
}

// YAML encodes the manifest as a single YAML document.
func (m Manifest) YAML() ([]byte, error) {
	return encodeNode(m.doc)
}

// Encode renders a manifest list as one multi-document YAML stream suitable
// for kubectl apply. Each document is preceded by "---".
func Encode(manifests []Manifest) ([]byte, error) {
	var buf bytes.Buffer
	for _, m := range manifests {
		doc, err := m.YAML()
		if err != nil {
			return nil, err
		}
		buf.WriteString("---\n")
		buf.Write(doc)
	}
	return buf.Bytes(), nil
}

func encodeNode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- Ordered yaml.Node builders ---------------------------------------------

func strNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func intNode(i int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(i)}
}

func boolNode(b bool) *yaml.Node {
	if b {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}
}

// asNode lifts simple Go values into yaml.Nodes. Supported: *yaml.Node
// (as-is), string, int, bool, []string. Anything larger is built explicitly
// with mapNode/seqNode so field order is always deliberate.
func asNode(v any) *yaml.Node {
	switch t := v.(type) {
	case *yaml.Node:
		return t
	case string:
		return strNode(t)
	case int:
		return intNode(t)
	case bool:
		return boolNode(t)
	case []string:
		items := make([]*yaml.Node, len(t))
		for i, s := range t {
			items[i] = strNode(s)
		}
		return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
	default:
		return strNode("")
	}
}

// mapNode builds a mapping node from alternating key/value pairs, preserving
// the given order.
func mapNode(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			panic("renderer: mapNode keys must be strings")
		}
		n.Content = append(n.Content, strNode(key), asNode(kv[i+1]))
	}
	return n
}

func seqNode(items ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
}

func docNode(root *yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
}

// --- Mapping node accessors -------------------------------------------------

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

// mapGet returns the value node for key in a mapping node, or nil.
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

// mapSet sets key to val in a mapping node, replacing in place if present and
// appending otherwise. Deterministic either way.
func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, strNode(key), val)
}

func mapGetOrCreate(m *yaml.Node, key string) *yaml.Node {
	if v := mapGet(m, key); v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapSet(m, key, v)
	return v
}

// cloneNode deep-copies a yaml.Node tree, so patch trees merged into rendered
// documents never alias the parsed overlay.
func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	if n.Content != nil {
		out.Content = make([]*yaml.Node, len(n.Content))
		for i, c := range n.Content {
			out.Content[i] = cloneNode(c)
		}
	}
	return &out
}
