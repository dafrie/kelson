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

	d := &Diff{
		Level:       LevelRendered,
		Project:     project,
		Environment: environment,
		Resources:   out,
		Summary:     summarize(out),
	}
	return d, nil
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
