package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

// ServiceBinding references one well-known key of a declared Project service.
// Rendered as a secretKeyRef against the service's credential Secret
// (<project>-<service>-credentials). Values never appear in the spec
// (ADR-0009).
type ServiceBinding struct {
	Service string `yaml:"service" json:"service" jsonschema:"required,description=name of a service declared in the Project"`
	Key     string `yaml:"key" json:"key" jsonschema:"required,description=well-known key of the service type, e.g. uri for postgres"`
}

// EnvValue is the union allowed for an environment variable: a plain,
// non-secret string, or a service binding {from: {service, key}}. Secret
// literals are rejected by validation (code secret/literal).
type EnvValue struct {
	Literal string
	From    *ServiceBinding
}

// IsZero lets encoding skip unset values under omitempty.
func (e EnvValue) IsZero() bool { return e.Literal == "" && e.From == nil }

func (e EnvValue) String() string {
	if e.From != nil {
		return fmt.Sprintf("from:%s/%s", e.From.Service, e.From.Key)
	}
	return e.Literal
}

func (e *EnvValue) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		e.Literal, e.From = s, nil
		return nil
	case yaml.MappingNode:
		var ref struct {
			From *ServiceBinding `yaml:"from"`
		}
		if err := n.Decode(&ref); err != nil {
			return err
		}
		if ref.From == nil {
			return &yaml.TypeError{Errors: []string{
				fmt.Sprintf("line %d: env value must be a string or {from: {service, key}}", n.Line),
			}}
		}
		e.Literal, e.From = "", ref.From
		return nil
	default:
		return &yaml.TypeError{Errors: []string{
			fmt.Sprintf("line %d: env value must be a string or {from: {service, key}}", n.Line),
		}}
	}
}

func (e EnvValue) MarshalYAML() (any, error) {
	if e.From != nil {
		return map[string]*ServiceBinding{"from": e.From}, nil
	}
	return e.Literal, nil
}

func (e *EnvValue) UnmarshalJSON(b []byte) error {
	if strings.HasPrefix(strings.TrimSpace(string(b)), "\"") {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		e.Literal, e.From = s, nil
		return nil
	}
	var ref struct {
		From *ServiceBinding `json:"from"`
	}
	if err := json.Unmarshal(b, &ref); err != nil {
		return err
	}
	if ref.From == nil {
		return fmt.Errorf("env value must be a string or {\"from\": {\"service\": ..., \"key\": ...}}")
	}
	e.Literal, e.From = "", ref.From
	return nil
}

func (e EnvValue) MarshalJSON() ([]byte, error) {
	if e.From != nil {
		return json.Marshal(map[string]*ServiceBinding{"from": e.From})
	}
	return json.Marshal(e.Literal)
}

// JSONSchema fixes the generated schema for the union type: a string, or an
// object carrying a service binding.
func (EnvValue) JSONSchema() *jsonschema.Schema {
	r := &jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true, AllowAdditionalProperties: false}
	from := r.Reflect(&envValueRef{})
	from.ID = ""
	from.Version = ""
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{{Type: "string"}, from},
	}
}

type envValueRef struct {
	From *ServiceBinding `json:"from" jsonschema:"required"`
}

// ServiceKeys lists the well-known binding keys per service type. Validation
// rejects anything not on this list (ref/unknown-service-key); the renderer
// renders exactly these keys as secretKeyRefs.
var ServiceKeys = map[string][]string{
	"postgres": {"uri", "host", "port", "database", "username", "password"},
	"valkey":   {"uri", "host", "port", "password"},
}
