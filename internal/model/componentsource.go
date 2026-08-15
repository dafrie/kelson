package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

// ComponentSource is what `source:` says on a component, and it is a union
// because two decisions landed on the same key.
//
//	components:
//	  - name: web
//	    source: app                        # ADR-0035: which repository this builds from
//	  - name: ingress
//	    kind: helm
//	    source: {repository: https://…}    # ADR-0016: where this chart is fetched from
//
// ADR-0016 gave `kind: helm` a chart source — the Helm repository or OCI
// registry a chart comes from. ADR-0035 decision 3 gives a buildable component a
// source *binding* — the name of one of the sources the Project or the instance
// declares. Both spell it `source:`, and ADR-0035's YAML is the one this model
// follows, so the key carries both.
//
// The two never meet on one component: a helm component builds nothing and a
// buildable component has no chart, so the kind decides which arm is meaningful
// and validation refuses the other rather than ignoring it (issue #141). What
// makes the union safe is that the arms are different *shapes* — a scalar is a
// name, a mapping is a chart source — so a document is never ambiguous about
// which one it wrote, which is exactly how [EnvValue] tells its three arms
// apart.
type ComponentSource struct {
	// Name is the source this component builds from: an entry of the Project's
	// `sources:` (or its singular `source:`), else a GitSource the instance
	// declares. Project-local names shadow global ones (ADR-0035 decision 3).
	Name string

	// Chart is where a `kind: helm` component's chart is fetched from: exactly
	// one of a classic Helm repository or an OCI registry (ADR-0016 decision 4).
	Chart *ChartSource
}

// SourceName is the source a component binds to by name, or "" for a component
// that names none — which means the Project's default source, and means nothing
// at all for a kind that does not build.
//
// It exists so callers never have to reach through the union's pointer and
// decide what a nil means; the resolver, the validator and the build plane ask
// the same question the same way.
func (c Component) SourceName() string {
	if c.Source == nil {
		return ""
	}
	return c.Source.Name
}

// ChartSourceOf is the chart source a helm component names, or nil. It is the
// other half of [Component.SourceName]: one accessor per arm, so a caller that
// wants one is never handed the other.
func (c Component) ChartSourceOf() *ChartSource {
	if c.Source == nil {
		return nil
	}
	return c.Source.Chart
}

// componentSourceShapePrefix marks every decode error this type produces, the
// way [envValueShapePrefix] does for an environment value and for the same
// reason: a custom unmarshaller can only report through a yaml.TypeError
// string, so the prefix is how decode.go recognizes the message and attaches
// [ComponentSourceRemediation] to it instead of the generic "fix the value".
const componentSourceShapePrefix = "component source: "

// ComponentSourceRemediation lists both forms `source:` takes. It is one string
// because the two forms are one decision — what is this component made of — and
// an author who wrote the wrong shape is choosing between them.
const ComponentSourceRemediation = "a component's source: is one of two things. On a buildable component it is a " +
	"name: the source this component builds from, declared in the Project's spec.sources (or its singular " +
	"spec.source) or by a GitSource the instance offers — `source: app` (ADR-0035). On a kind: helm component " +
	"it is a mapping naming where the chart is fetched from: {repository: <Helm repository URL>} or " +
	"{oci: <OCI registry URL>} (ADR-0016). No kind takes both, and no kind takes a list"

func componentSourceShapeError(line int, what string) error {
	return &yaml.TypeError{Errors: []string{
		fmt.Sprintf("line %d: %s%s", line, componentSourceShapePrefix, what),
	}}
}

func (s *ComponentSource) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var name string
		if err := n.Decode(&name); err != nil {
			return err
		}
		s.Name, s.Chart = name, nil
		return nil
	case yaml.MappingNode:
		var chart ChartSource
		if err := n.Decode(&chart); err != nil {
			return err
		}
		s.Name, s.Chart = "", &chart
		return nil
	default:
		return componentSourceShapeError(n.Line,
			"a sequence is neither a source name nor a chart source")
	}
}

func (s ComponentSource) MarshalYAML() (any, error) {
	if s.Chart != nil {
		return s.Chart, nil
	}
	return s.Name, nil
}

func (s *ComponentSource) UnmarshalJSON(b []byte) error {
	if trimmed := strings.TrimSpace(string(b)); strings.HasPrefix(trimmed, "\"") {
		var name string
		if err := json.Unmarshal(b, &name); err != nil {
			return err
		}
		s.Name, s.Chart = name, nil
		return nil
	}
	var chart ChartSource
	if err := json.Unmarshal(b, &chart); err != nil {
		return fmt.Errorf("%sa source is a name or a chart source: %w", componentSourceShapePrefix, err)
	}
	s.Name, s.Chart = "", &chart
	return nil
}

func (s ComponentSource) MarshalJSON() ([]byte, error) {
	if s.Chart != nil {
		return json.Marshal(s.Chart)
	}
	return json.Marshal(s.Name)
}

// JSONSchema fixes the generated schema for the union: a source name, or a
// chart source object.
//
// In the CRD this node becomes `x-kubernetes-preserve-unknown-fields`, as every
// union does — a structural schema cannot carry a `oneOf` (internal/schemagen's
// preserveUnknown). Nothing is accepted that was not accepted before: what the
// API server stops checking here, validate.go refuses one hop later, with the
// kind in hand and therefore with something to say about which arm was meant.
func (ComponentSource) JSONSchema() *jsonschema.Schema {
	r := &jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true, AllowAdditionalProperties: false}
	chart := r.Reflect(&ChartSource{})
	chart.ID = ""
	chart.Version = ""
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{
				Type:        "string",
				Pattern:     `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`,
				MaxLength:   ptr(uint64(63)),
				Description: "the name of a source this component builds from, declared in spec.sources or by a GitSource",
			},
			chart,
		},
	}
}

// ptr is the one-line helper the schema above needs for a bound the jsonschema
// package takes by pointer.
func ptr[T any](v T) *T { return &v }
