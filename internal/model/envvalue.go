package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

// ServiceBinding references one well-known key of a Project's data component.
// Rendered as a secretKeyRef against the credential Secret the component's
// operator generates. Values never appear in the spec (ADR-0009).
//
// The key stays `service:` after ADR-0014 renamed the list it points into:
// what a binding names is the service a data component provides, and renaming
// it to `component:` would have widened the field's apparent range to every
// kind — most of which nothing can bind to.
type ServiceBinding struct {
	Service string `yaml:"service" json:"service" jsonschema:"required,description=name of a data component (kind postgres or valkey) declared in the Project"`
	Key     string `yaml:"key" json:"key" jsonschema:"required,description=well-known key of the service type, e.g. uri for postgres"`
}

// SecretRef references one key of a Secret that exists outside the spec
// (ADR-0018). It is written inline as the value of an environment variable:
//
//	env:
//	  DATABASE_URL: { secret: checkout-db, key: url }
//
// `secret` is a *name*, never a value. Under the v0 `cluster` backend it names
// a Kubernetes Secret in the environment's namespace, which kelson references
// and never creates, reads or writes at render time — authoring it is
// `kelson secret set` (issue #116). The reference itself is backend-agnostic:
// the Environment's `secrets.backend` selects the mechanism that puts the value
// there (issues #80, #81), and the spec text does not change when it changes.
//
// The two arms of an env mapping are told apart by their own key — `secret:`
// here, `from:` for a ServiceBinding — because they answer different questions:
// a binding names a component of this Project and kelson derives the Secret, a
// reference names the Secret itself. Both render as a secretKeyRef, which is
// the point: there is one mechanism, and neither form can carry a value.
type SecretRef struct {
	Name string `yaml:"secret" json:"secret" jsonschema:"required,description=name of a Secret in the environment's namespace; kelson references it and never creates or reads it"`
	Key  string `yaml:"key" json:"key" jsonschema:"required,description=key within that Secret, e.g. url"`
}

// EnvValue is the union allowed for an environment variable: a plain,
// non-secret string, a service binding {from: {service, key}}, or a secret
// reference {secret: <name>, key: <key>}. Secret literals are rejected by
// validation (code secret/literal).
//
// The union is closed by construction: an EnvValue that decoded successfully is
// one of exactly these three, so no later stage has to ask whether a value it
// holds might be a credential. That is the model half of the guarantee in
// issue #82 — the renderer half is that both reference forms emit
// valueFrom.secretKeyRef and there is no field on any rendered resource that
// could carry an inline Secret value.
type EnvValue struct {
	Literal string
	From    *ServiceBinding
	Secret  *SecretRef
}

// IsZero lets encoding skip unset values under omitempty.
func (e EnvValue) IsZero() bool { return e.Literal == "" && e.From == nil && e.Secret == nil }

func (e EnvValue) String() string {
	switch {
	case e.Secret != nil:
		return fmt.Sprintf("secret:%s/%s", e.Secret.Name, e.Secret.Key)
	case e.From != nil:
		return fmt.Sprintf("from:%s/%s", e.From.Service, e.From.Key)
	}
	return e.Literal
}

// envValueShapePrefix marks every decode error this type produces. A custom
// unmarshaller can only report through a yaml.TypeError string — returning
// anything else aborts the whole document and loses the other errors — so the
// prefix is how decode.go recognizes the message and attaches
// EnvValueRemediation to it instead of the generic "fix the value" line. An
// author who wrote the wrong mapping needs to see the shapes that are right.
const envValueShapePrefix = "environment value: "

// EnvValueRemediation lists every form an environment value may take. It is
// one string because the three forms are one decision — what is the source of
// this variable — and an author who got it wrong is choosing between them.
const EnvValueRemediation = "an environment value is one of three things: a plain string for non-secret configuration " +
	"(LOG_LEVEL: info); a secret reference {secret: <secret name>, key: <key>}, which renders as a " +
	"valueFrom.secretKeyRef against a Secret in the environment's namespace that kelson never reads; " +
	"or a data-service binding {from: {service: <component>, key: <well-known key>}}, which " +
	"renders as a secretKeyRef against the credentials the component's operator generates. " +
	"A secret value itself is never written in the spec"

func envValueShapeError(line int, what string) error {
	return &yaml.TypeError{Errors: []string{
		fmt.Sprintf("line %d: %s%s", line, envValueShapePrefix, what),
	}}
}

// envValueForm is the shape an env mapping declares, decided by which of the
// two discriminating keys it carries. Reading the keys off the node rather than
// probing with a struct is what lets a mapping carrying both be refused: after
// decoding into a struct, "absent" and "present but empty" look the same.
type envValueForm struct{ hasFrom, hasSecret, hasKey bool }

func envMappingForm(n *yaml.Node) envValueForm {
	var f envValueForm
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "from":
			f.hasFrom = true
		case "secret":
			f.hasSecret = true
		case "key":
			f.hasKey = true
		}
	}
	return f
}

func (e *EnvValue) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		e.Literal, e.From, e.Secret = s, nil, nil
		return nil
	case yaml.MappingNode:
		form := envMappingForm(n)
		switch {
		case form.hasFrom && (form.hasSecret || form.hasKey):
			return envValueShapeError(n.Line,
				"a mapping is either a secret reference or a service binding, and this one is written as both")
		case form.hasSecret:
			var ref SecretRef
			if err := n.Decode(&ref); err != nil {
				return err
			}
			e.Literal, e.From, e.Secret = "", nil, &ref
			return nil
		case form.hasFrom:
			var ref struct {
				From *ServiceBinding `yaml:"from"`
			}
			if err := n.Decode(&ref); err != nil {
				return err
			}
			e.Literal, e.From, e.Secret = "", ref.From, nil
			return nil
		}
		return envValueShapeError(n.Line, "a mapping value is a reference, and this one names neither secret: nor from:")
	default:
		return envValueShapeError(n.Line, "a sequence is not one of the forms an environment value takes")
	}
}

func (e EnvValue) MarshalYAML() (any, error) {
	switch {
	case e.Secret != nil:
		return e.Secret, nil
	case e.From != nil:
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
		e.Literal, e.From, e.Secret = s, nil, nil
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	_, hasFrom := probe["from"]
	_, hasSecret := probe["secret"]
	_, hasKey := probe["key"]
	switch {
	case hasFrom && (hasSecret || hasKey):
		return fmt.Errorf("%sa mapping is either a secret reference or a service binding, and this one is written as both", envValueShapePrefix)
	case hasSecret:
		var ref SecretRef
		if err := json.Unmarshal(b, &ref); err != nil {
			return err
		}
		e.Literal, e.From, e.Secret = "", nil, &ref
		return nil
	case hasFrom:
		var ref struct {
			From *ServiceBinding `json:"from"`
		}
		if err := json.Unmarshal(b, &ref); err != nil {
			return err
		}
		e.Literal, e.From, e.Secret = "", ref.From, nil
		return nil
	}
	return fmt.Errorf("%sa mapping value is a reference, and this one names neither \"secret\" nor \"from\"", envValueShapePrefix)
}

func (e EnvValue) MarshalJSON() ([]byte, error) {
	switch {
	case e.Secret != nil:
		return json.Marshal(e.Secret)
	case e.From != nil:
		return json.Marshal(map[string]*ServiceBinding{"from": e.From})
	}
	return json.Marshal(e.Literal)
}

// JSONSchema fixes the generated schema for the union type: a string, an
// object carrying a service binding, or an object carrying a secret reference.
func (EnvValue) JSONSchema() *jsonschema.Schema {
	r := &jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true, AllowAdditionalProperties: false}
	arm := func(v any) *jsonschema.Schema {
		s := r.Reflect(v)
		s.ID = ""
		s.Version = ""
		return s
	}
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string"},
			arm(&envValueRef{}),
			arm(&SecretRef{}),
		},
	}
}

type envValueRef struct {
	From *ServiceBinding `json:"from" jsonschema:"required"`
}

// ServiceKeys lists the well-known binding keys per data-component kind.
// Validation rejects anything not on this list (ref/unknown-service-key); the
// renderer renders exactly these keys as secretKeyRefs.
var ServiceKeys = map[ComponentKind][]string{
	ComponentPostgres: {"uri", "host", "port", "database", "username", "password"},
	ComponentValkey:   {"uri", "host", "port", "password"},
}
