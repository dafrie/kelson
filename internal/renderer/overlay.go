package renderer

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
)

// Overlays are the escape hatch of record (docs/architecture.md; model rule
// P6: project overlays first, then environment). Two kinds, applied in
// resolved order against the resources rendered so far:
//
//   - patch: a YAML document carrying the apiVersion/kind/metadata.name of a
//     rendered resource; its body is strategic-merged into that resource.
//   - manifest: a YAML stream of additional resources, emitted as-is (stamped
//     with provenance like everything else) after the core resources.
//
// Merge semantics are the predictable subset of strategic merge: mappings
// merge key-by-key recursively; sequences of mappings keyed by `name` merge
// per named element; any other sequence or scalar replaces wholesale. An
// explicit null value deletes the key. A patch that matches no resource is a
// structured error, never a silent no-op.
//
// Every resource an overlay touches records it in the kelson.dev/overlays
// annotation, so the diff always shows where output diverges from the model —
// deterministically, in overlay application order.
func applyOverlays(manifests *[]Manifest, resolved *model.Resolved, resolver OverlayResolver) error {
	for _, ov := range resolved.Overlays {
		path := ov.Patch
		if path == "" {
			path = ov.Manifest
		}
		body, err := resolver(path)
		if err != nil {
			return Errors{{Code: ErrOverlayLoad, Overlay: path, Message: err.Error()}}
		}
		switch {
		case ov.Patch != "":
			if err := applyPatch(manifests, path, body); err != nil {
				return err
			}
		default:
			if err := applyOverlayManifests(manifests, resolved, path, body); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyPatch(manifests *[]Manifest, path string, body []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return Errors{{Code: ErrOverlayInvalid, Overlay: path, Message: fmt.Sprintf("patch is not valid YAML: %v", err)}}
	}
	root := docRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return Errors{{Code: ErrOverlayInvalid, Overlay: path, Message: "patch must be a YAML mapping carrying apiVersion, kind and metadata.name"}}
	}
	kind := scalarValue(mapGet(root, "kind"))
	name := ""
	if md := mapGet(root, "metadata"); md != nil {
		name = scalarValue(mapGet(md, "name"))
	}
	if kind == "" || name == "" {
		return Errors{{Code: ErrOverlayInvalid, Overlay: path, Message: "patch must carry kind and metadata.name to identify its target"}}
	}
	apiVersion := scalarValue(mapGet(root, "apiVersion"))

	target := findTarget(*manifests, apiVersion, kind, name)
	if target == nil {
		return Errors{{
			Code:    ErrOverlayTarget,
			Overlay: path,
			Target:  kind + "/" + name,
			Message: "patch targets a resource not present in the rendered set; check kind and metadata.name against the rendered manifests",
		}}
	}

	merged := mergeNodes(docRoot(target.doc), root)
	target.doc = docNode(merged)
	target.APIVersion = scalarValue(mapGet(merged, "apiVersion"))
	target.Kind = scalarValue(mapGet(merged, "kind"))
	if md := mapGet(merged, "metadata"); md != nil {
		target.Name = scalarValue(mapGet(md, "name"))
		if ns := scalarValue(mapGet(md, "namespace")); ns != "" {
			target.Namespace = ns
		}
	}

	// Provenance in the diff: the resource records that an overlay diverged
	// it from the model (docs/architecture.md, Provenance).
	md := mapGet(docRoot(target.doc), "metadata")
	annotations := mapGetOrCreate(md, "annotations")
	recordOverlay(annotations, path)
	return nil
}

func findTarget(manifests []Manifest, apiVersion, kind, name string) *Manifest {
	for i := range manifests {
		m := &manifests[i]
		if m.Kind != kind || m.Name != name {
			continue
		}
		if apiVersion != "" && m.APIVersion != apiVersion {
			continue
		}
		return m
	}
	return nil
}

func applyOverlayManifests(manifests *[]Manifest, resolved *model.Resolved, path string, body []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	n := 0
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err != nil {
			// end of stream — io.EOF matched by message because the
			// renderer-purity lint forbids importing io (issue #20).
			if err.Error() == "EOF" {
				break
			}
			return Errors{{Code: ErrOverlayInvalid, Overlay: path, Message: fmt.Sprintf("manifest is not valid YAML: %v", err)}}
		}
		root := docRoot(&doc)
		if root == nil || (root.Kind == yaml.ScalarNode && root.Tag == "!!null") {
			continue // empty document in the stream
		}
		if root.Kind != yaml.MappingNode {
			return Errors{{Code: ErrOverlayInvalid, Overlay: path, Message: "manifest documents must be YAML mappings"}}
		}
		n++
		m, err := overlayManifest(resolved, path, body, &doc)
		if err != nil {
			return err
		}
		*manifests = append(*manifests, m)
	}
	if n == 0 {
		return Errors{{Code: ErrOverlayInvalid, Overlay: path, Message: "manifest overlay contains no documents"}}
	}
	return nil
}

// overlayManifest stamps an overlay-contributed manifest with provenance:
// existing labels/annotations are preserved in place, kelson's are merged in.
// A namespace is filled in only when the author left it unset.
func overlayManifest(resolved *model.Resolved, path string, body []byte, doc *yaml.Node) (Manifest, error) {
	hash, err := overlayHash(resolved, path, body)
	if err != nil {
		return Manifest{}, Errors{{Code: ErrInternal, Message: err.Error()}}
	}
	root := docRoot(doc)
	prov := provenance{
		project:     resolved.Project,
		environment: resolved.Environment.Name,
		namespace:   resolved.Environment.Namespace,
		specHash:    hash,
	}
	md := mapGetOrCreate(root, "metadata")
	if mapGet(md, "namespace") == nil {
		mapSet(md, "namespace", strNode(prov.namespace))
	}
	labels := mapGetOrCreate(md, "labels")
	mergeMapSet(labels, mapNode(
		"app.kubernetes.io/managed-by", "kelson",
		"kelson.dev/environment", prov.environment,
		"kelson.dev/project", prov.project,
	))
	annotations := mapGetOrCreate(md, "annotations")
	mergeMapSet(annotations, prov.annotations())
	recordOverlay(annotations, path)

	m := Manifest{
		APIVersion: scalarValue(mapGet(root, "apiVersion")),
		Kind:       scalarValue(mapGet(root, "kind")),
		Namespace:  scalarValue(mapGet(md, "namespace")),
		doc:        doc,
	}
	if n := mapGet(md, "name"); n != nil {
		m.Name = n.Value
	}
	if m.Kind == "" || m.Name == "" {
		return Manifest{}, Errors{{Code: ErrOverlayInvalid, Overlay: path, Message: "manifest documents must carry kind and metadata.name"}}
	}
	return m, nil
}

// mergeMapSet sets each key from src into dst (replace in place or append),
// preserving the author's existing entries.
func mergeMapSet(dst, src *yaml.Node) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		mapSet(dst, src.Content[i].Value, src.Content[i+1])
	}
}

// recordOverlay appends path to the kelson.dev/overlays annotation. Overlay
// application order is fixed by rule P6, so the comma-joined value is
// deterministic.
func recordOverlay(annotations *yaml.Node, path string) {
	const key = "kelson.dev/overlays"
	if existing := mapGet(annotations, key); existing != nil {
		existing.Value += "," + path
		return
	}
	mapSet(annotations, key, strNode(path))
}

func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// mergeNodes merges a patch node into a destination node, following the
// documented strategic-merge subset. It mutates dst and returns it (or a
// clone of the patch when the patch side replaces wholesale).
func mergeNodes(dst, patch *yaml.Node) *yaml.Node {
	switch {
	case dst.Kind == yaml.MappingNode && patch.Kind == yaml.MappingNode:
		return mergeMappings(dst, patch)
	case dst.Kind == yaml.SequenceNode && patch.Kind == yaml.SequenceNode:
		if mergeableByName(dst) && mergeableByName(patch) {
			return mergeNamedSequences(dst, patch)
		}
		return cloneNode(patch)
	default:
		return cloneNode(patch)
	}
}

func mergeMappings(dst, patch *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(patch.Content); i += 2 {
		key, pv := patch.Content[i], patch.Content[i+1]
		if pv.Tag == "!!null" {
			// Explicit null deletes, JSON-merge-patch style.
			mapDelete(dst, key.Value)
			continue
		}
		existing := mapGet(dst, key.Value)
		if existing == nil {
			mapSet(dst, key.Value, cloneNode(pv))
			continue
		}
		mapSet(dst, key.Value, mergeNodes(existing, pv))
	}
	return dst
}

// mergeableByName reports whether a sequence consists solely of mappings
// each keyed by a scalar `name` — the strategic-merge join key.
func mergeableByName(n *yaml.Node) bool {
	if len(n.Content) == 0 {
		return false
	}
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			return false
		}
		nm := mapGet(item, "name")
		if nm == nil || nm.Kind != yaml.ScalarNode {
			return false
		}
	}
	return true
}

func mergeNamedSequences(dst, patch *yaml.Node) *yaml.Node {
	for _, pv := range patch.Content {
		name := mapGet(pv, "name").Value
		var match *yaml.Node
		for _, dv := range dst.Content {
			if mapGet(dv, "name").Value == name {
				match = dv
				break
			}
		}
		if match == nil {
			dst.Content = append(dst.Content, cloneNode(pv))
			continue
		}
		mergeNodes(match, pv)
	}
	return dst
}

func mapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
