package main

import "gopkg.in/yaml.v3"

// omap is a mapping that keeps the order it was written in.
//
// A CRD is a document humans read: `apiVersion` belongs at the top and
// `openAPIV3Schema` at the bottom, and a `type` belongs before the `properties`
// it types. Marshalling a Go map would sort every key alphabetically and
// scatter both, so the emitter builds omaps instead — the order in the source is
// the order in the file, and the output is deterministic because nothing here
// ranges over a map without sorting first (the same rule internal/specrefdoc
// keeps).
type omap []kv

type kv struct {
	k string
	v any
}

// set appends a key. It is append-only by design: an emitter that could
// overwrite a key it already wrote would make the output depend on statement
// order in a way the reader of the YAML cannot see.
func (m *omap) set(k string, v any) { *m = append(*m, kv{k, v}) }

// setIf appends a key only when cond holds, which is most of the translation:
// an absent JSON Schema keyword must not become an empty OpenAPI one.
func (m *omap) setIf(cond bool, k string, v any) {
	if cond {
		m.set(k, v)
	}
}

func (m omap) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, entry := range m {
		value := &yaml.Node{}
		if err := value.Encode(entry.v); err != nil {
			return nil, err
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: entry.k},
			value,
		)
	}
	return node, nil
}
