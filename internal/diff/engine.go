package diff

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/renderer"
)

// ResourceRef is the identity a resource is matched on across two renders:
// apiVersion/kind/name/namespace. The four-plane model guarantees these are
// stable across renders, so they are the join key for the resource diff.
type ResourceRef struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// OriginFor reports who authored a field's value. It is resolved per
// (resource, field path) because a single resource can mix spec-authored and
// overlay-authored (or, on L2, admission/defaulting-authored) fields. A nil
// OriginFor attributes every field to the model (OriginSpec), which is the
// correct default for a render with no overlays.
type OriginFor func(ref ResourceRef, path string) Origin

// Between computes the L1 rendered diff (issue #42): a semantic diff over
// the two structured manifest sets, not a text diff over YAML. Resources are
// joined by identity; fields are walked in document order so the result is
// byte-deterministic for the same inputs.
//
// origin may be nil (all fields OriginSpec). Pass the OriginFor returned by
// NewOverlayOrigins to attribute overlay-contributed fields.
func Between(project, environment string, prev, cur []renderer.Manifest, origin OriginFor) (*Diff, error) {
	if origin == nil {
		origin = func(ResourceRef, string) Origin { return OriginSpec }
	}

	prevRes, err := buildResources(prev)
	if err != nil {
		return nil, err
	}
	curRes, err := buildResources(cur)
	if err != nil {
		return nil, err
	}
	return between(project, environment, prevRes, curRes, origin), nil
}

// between is the join shared by both entry points: resources matched by
// identity, fields walked in document order so the result is deterministic.
func between(project, environment string, prevRes, curRes []resource, origin OriginFor) *Diff {
	if origin == nil {
		origin = func(ResourceRef, string) Origin { return OriginSpec }
	}

	curIndex := map[ResourceRef]resource{}
	curOrder := []ResourceRef{}
	for _, r := range curRes {
		if _, dup := curIndex[r.ref]; dup {
			continue
		}
		curIndex[r.ref] = r
		curOrder = append(curOrder, r.ref)
	}

	seen := map[ResourceRef]bool{}
	var out []ResourceDiff
	for _, p := range prevRes {
		c, ok := curIndex[p.ref]
		if !ok {
			out = append(out, removedResource(p))
			continue
		}
		seen[p.ref] = true
		if rd, ok := modifiedResource(p, c, origin); ok {
			out = append(out, rd)
		}
	}
	for _, ref := range curOrder {
		if seen[ref] {
			continue
		}
		out = append(out, addedResource(curIndex[ref]))
	}

	// Redact here rather than at each caller: this is the single constructor
	// both L1 entry points funnel through, and a Diff is a display artifact by
	// construction (see Redact).
	return Redact(&Diff{
		Level:       LevelRendered,
		Project:     project,
		Environment: environment,
		Resources:   out,
		Summary:     summarize(out),
	})
}

// scalarAt returns the scalar value of key in a mapping node, or "".
func scalarAt(mapping *yaml.Node, key string) string {
	if v := mappingAt(mapping, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// mappingAt returns the value node for key in a mapping node, or nil.
func mappingAt(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// resource is one parsed manifest plus its join identity.
type resource struct {
	ref  ResourceRef
	root *yaml.Node // root mapping of the document (apiVersion/kind/metadata/spec)
}

// buildResources parses a manifest set into ordered resources. Parsing a
// renderer-produced manifest can only fail if the renderer emitted invalid
// YAML, which is an internal invariant violation.
func buildResources(ms []renderer.Manifest) ([]resource, error) {
	out := make([]resource, 0, len(ms))
	for _, m := range ms {
		body, err := m.YAML()
		if err != nil {
			return nil, fmt.Errorf("diff: encoding manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("diff: parsing manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		root := docRoot(&doc)
		out = append(out, resource{
			ref:  ResourceRef{APIVersion: m.APIVersion, Kind: m.Kind, Name: m.Name, Namespace: m.Namespace},
			root: root,
		})
	}
	return out, nil
}

// BetweenDocuments diffs two sets of already-rendered YAML documents, one
// resource per document.
//
// It exists for callers holding rendered bytes they must not re-render: the
// rendered-history store and the Git writers keep the exact output that went
// live at a revision (issue #38), and a rollback preview compares against
// those bytes verbatim. Re-rendering the old spec to obtain manifests would
// answer a different question — what that spec produces *now*, under today's
// renderer and ClusterProfile — which is precisely the question a rollback
// preview must not ask.
//
// Identity comes from each document's own apiVersion/kind/metadata, since
// there is no renderer.Manifest carrying it alongside.
func BetweenDocuments(project, environment string, prev, cur [][]byte, origin OriginFor) (*Diff, error) {
	prevRes, err := buildResourcesFromDocuments(prev)
	if err != nil {
		return nil, err
	}
	curRes, err := buildResourcesFromDocuments(cur)
	if err != nil {
		return nil, err
	}
	return between(project, environment, prevRes, curRes, origin), nil
}

// buildResourcesFromDocuments parses raw rendered documents, reading each
// resource's identity out of the document itself. A document that is empty or
// carries no kind is skipped rather than failing the whole diff — a recorded
// stream can legitimately contain a trailing separator.
func buildResourcesFromDocuments(docs [][]byte) ([]resource, error) {
	out := make([]resource, 0, len(docs))
	for i, body := range docs {
		var doc yaml.Node
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("diff: parsing recorded document %d: %w", i, err)
		}
		root := docRoot(&doc)
		if root == nil {
			continue
		}
		ref := ResourceRef{
			APIVersion: scalarAt(root, "apiVersion"),
			Kind:       scalarAt(root, "kind"),
		}
		if meta := mappingAt(root, "metadata"); meta != nil {
			ref.Name = scalarAt(meta, "name")
			ref.Namespace = scalarAt(meta, "namespace")
		}
		if ref.Kind == "" {
			continue
		}
		out = append(out, resource{ref: ref, root: root})
	}
	return out, nil
}

// removedResource reports a resource present before and absent after. Deleting
// a resource is disruptive by construction.
func removedResource(r resource) ResourceDiff {
	return ResourceDiff{
		APIVersion: r.ref.APIVersion,
		Kind:       r.ref.Kind,
		Name:       r.ref.Name,
		Namespace:  r.ref.Namespace,
		Op:         OpRemoved,
		Risk:       RiskDisruptive,
	}
}

// addedResource reports a resource absent before and present after. Creating
// something new never touches running workloads, so it is additive.
func addedResource(r resource) ResourceDiff {
	return ResourceDiff{
		APIVersion: r.ref.APIVersion,
		Kind:       r.ref.Kind,
		Name:       r.ref.Name,
		Namespace:  r.ref.Namespace,
		Op:         OpAdded,
		Risk:       RiskAdditive,
	}
}

// modifiedResource diffs the two versions of an existing resource. The second
// return value is false when nothing changed after suppressing provenance
// noise, in which case no entry is emitted.
func modifiedResource(prev, cur resource, origin OriginFor) (ResourceDiff, bool) {
	fields := diffResourceFields(prev, cur, origin)
	if len(fields) == 0 {
		return ResourceDiff{}, false
	}
	return ResourceDiff{
		APIVersion: cur.ref.APIVersion,
		Kind:       cur.ref.Kind,
		Name:       cur.ref.Name,
		Namespace:  cur.ref.Namespace,
		Op:         OpModified,
		Risk:       maxFieldRisk(fields),
		Fields:     fields,
	}, true
}

// summarize rolls resources up into the blast-radius summary.
func summarize(resources []ResourceDiff) Summary {
	var s Summary
	s.MaxRisk = RiskCosmetic
	for _, r := range resources {
		switch r.Op {
		case OpAdded:
			s.Added++
		case OpModified:
			s.Modified++
		case OpRemoved:
			s.Removed++
		}
		switch r.Risk {
		case RiskRestart:
			s.Restarting = append(s.Restarting, r.Name)
		case RiskDisruptive:
			s.Disruptive = append(s.Disruptive, r.Name)
		}
		if riskRank(r.Risk) > riskRank(s.MaxRisk) {
			s.MaxRisk = r.Risk
		}
	}
	return s
}

// riskRank orders Risk from least to most severe, for computing MaxRisk.
func riskRank(r Risk) int {
	switch r {
	case RiskCosmetic:
		return 0
	case RiskAdditive:
		return 1
	case RiskRestart:
		return 2
	case RiskDisruptive:
		return 3
	default:
		return 0
	}
}

func maxFieldRisk(fields []FieldDiff) Risk {
	worst := RiskCosmetic
	for _, f := range fields {
		if riskRank(f.Risk) > riskRank(worst) {
			worst = f.Risk
		}
	}
	return worst
}
