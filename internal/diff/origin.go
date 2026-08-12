package diff

import (
	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/renderer"
)

// NewOverlayOrigins builds an OriginFor that attributes overlay-contributed
// fields to OriginOverlay, by comparing each overlaid render against the same
// spec rendered from the model alone (overlays stripped).
//
// This is how the diff stays honest about authorship (issue #42): a field can
// be authored by the model spec, by an overlay, or — on L2 — by admission
// defaulting/sidecar injection. Confusing overlay output for a user edit (or
// vice versa) is exactly the confusion the Origin field exists to prevent
// (#29, #43).
//
// The comparison is a pure render-vs-render: overlay = rendered with overlays,
// modelOnly = the same resolved spec with Overlays cleared. Any field whose
// value the overlay introduced or changed is attributed OriginOverlay; every
// other field stays OriginSpec. A resource that exists only because of an
// overlay manifest is treated as fully overlay-authored.
func NewOverlayOrigins(overlaid, modelOnly []renderer.Manifest) (OriginFor, error) {
	over, err := buildResources(overlaid)
	if err != nil {
		return nil, err
	}
	model, err := buildResources(modelOnly)
	if err != nil {
		return nil, err
	}

	modelIndex := map[ResourceRef]resource{}
	for _, r := range model {
		if _, dup := modelIndex[r.ref]; !dup {
			modelIndex[r.ref] = r
		}
	}

	fullyOverlay := map[ResourceRef]bool{}
	partial := map[ResourceRef]map[string]bool{}
	for _, o := range over {
		m, ok := modelIndex[o.ref]
		if !ok {
			fullyOverlay[o.ref] = true
			continue
		}
		set := map[string]bool{}
		collectOverlayPaths(set, o.ref.Kind, o.ref, m.root, o.root, "")
		if len(set) > 0 {
			partial[o.ref] = set
		}
	}

	return func(ref ResourceRef, path string) Origin {
		if fullyOverlay[ref] {
			return OriginOverlay
		}
		if partial[ref][path] {
			return OriginOverlay
		}
		return OriginSpec
	}, nil
}

// collectOverlayPaths records every field path where cur diverges from a
// model-only render of the same resource — i.e. where an overlay authored the
// value. It mirrors the engine's document-order walk but records only paths.
func collectOverlayPaths(set map[string]bool, kind string, ref ResourceRef, prev, cur *yaml.Node, path string) {
	root := cur
	if prev == nil {
		collectAllPaths(set, kind, root, path)
		return
	}
	if cur == nil {
		return
	}
	if prev.Kind != cur.Kind {
		set[path] = true
		collectAllPaths(set, kind, cur, path)
		return
	}
	switch cur.Kind {
	case yaml.ScalarNode:
		if !scalarEqual(prev, cur) {
			set[path] = true
		}
	case yaml.MappingNode:
		collectMappingPaths(set, kind, ref, prev, cur, path)
	case yaml.SequenceNode:
		collectSequencePaths(set, kind, ref, prev, cur, path)
	}
}

func collectMappingPaths(set map[string]bool, kind string, ref ResourceRef, prev, cur *yaml.Node, path string) {
	curIndex := indexByKey(cur)
	processed := map[string]bool{}
	for i := 0; i+1 < len(prev.Content); i += 2 {
		key := prev.Content[i].Value
		if isProvenanceKey(key) {
			continue
		}
		processed[key] = true
		after, ok := curIndex[key]
		if !ok {
			set[joinPath(path, key)] = true
			continue
		}
		collectOverlayPaths(set, kind, ref, prev.Content[i+1], after, joinPath(path, key))
	}
	for j := 0; j+1 < len(cur.Content); j += 2 {
		key := cur.Content[j].Value
		if isProvenanceKey(key) || processed[key] {
			continue
		}
		set[joinPath(path, key)] = true
	}
}

func collectSequencePaths(set map[string]bool, kind string, ref ResourceRef, prev, cur *yaml.Node, path string) {
	if mergeableByName(prev) && mergeableByName(cur) && !seqIndexStyle(path) {
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
				set[joinKey(path, name)] = true
				continue
			}
			collectMappingPaths(set, kind, ref, item, after, joinKey(path, name))
		}
		for _, item := range cur.Content {
			name := namedKey(item)
			if name == "" || processed[name] {
				continue
			}
			set[joinKey(path, name)] = true
		}
		return
	}

	n := len(prev.Content)
	if len(cur.Content) < n {
		n = len(cur.Content)
	}
	for i := 0; i < n; i++ {
		collectOverlayPaths(set, kind, ref, prev.Content[i], cur.Content[i], joinIndex(path, i))
	}
	for i := n; i < len(cur.Content); i++ {
		set[joinIndex(path, i)] = true
	}
	for i := n; i < len(prev.Content); i++ {
		set[joinIndex(path, i)] = true
	}
}

// collectAllPaths records every (non-provenance) field path under a node, for
// a resource authored entirely by an overlay manifest.
func collectAllPaths(set map[string]bool, kind string, n *yaml.Node, path string) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.ScalarNode:
		set[path] = true
	case yaml.SequenceNode:
		if mergeableByName(n) {
			for _, item := range n.Content {
				collectAllPaths(set, kind, item, joinKey(path, namedKey(item)))
			}
			return
		}
		for i, item := range n.Content {
			collectAllPaths(set, kind, item, joinIndex(path, i))
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if isProvenanceKey(key) || key == "apiVersion" || key == "kind" || key == "name" || key == "namespace" {
				continue
			}
			if key == "metadata" {
				collectAllPaths(set, kind, n.Content[i+1], joinPath(path, key))
				continue
			}
			collectAllPaths(set, kind, n.Content[i+1], joinPath(path, key))
		}
	}
}
