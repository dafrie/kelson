package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// The CRD emitter (ADR-0027 decision 4).
//
// It translates the reflected JSON Schema of a document's `spec` into a
// *structural* openAPIV3Schema — which is not the same dialect, and the
// differences are the whole of this file:
//
//   - `additionalProperties: false` is dropped. A structural schema may not
//     carry it beside `properties`, and it would be redundant if it could: a CRD
//     with a structural schema prunes unknown fields at the API server, which is
//     the same refusal by a different mechanism. The JSON Schema keeps it,
//     because a plain JSON Schema validator has no pruning to fall back on.
//
//   - `default` is dropped, and this one is a behaviour decision rather than a
//     dialect one. CRD defaults are *applied*: the API server writes them into
//     the stored object. `preset` defaults to `shared` in the model, but only
//     for a data component — materialising it onto every component would create
//     documents that validate.go then refuses (`preset` on a workload is an
//     error, issue #141). Defaults stay documentation until each one has been
//     checked against the shape rules that gate the field.
//
//   - A `oneOf` becomes `x-kubernetes-preserve-unknown-fields: true`. See
//     preserveUnknown below.
//
// Everything else — types, enums, patterns, bounds, required lists, nesting —
// carries across unchanged, because it came from the same reflection that
// produced schema/*.json.

// crdDef is everything about a custom resource that the Go types do not say:
// its names, how `kubectl get` prints it, what its status looks like, and the
// CEL rules the API server enforces before a controller ever sees the document.
type crdDef struct {
	kind     string
	listKind string
	plural   string
	singular string
	// shortNames are the `kubectl get` aliases. They are prefixed because
	// `env` and `proj` are words other projects' CRDs want too.
	shortNames []string
	// description is the openAPIV3Schema description of the whole object.
	description string
	// printerColumns are the additionalPrinterColumns, in display order.
	printerColumns []omap
	// status is the hand-written schema of the status subresource. It mirrors
	// api/kelson/v1alpha1's status types, which are not reflected here because
	// they are not part of the authoring model — the drift between the two is
	// policed by api/kelson/v1alpha1/crd_test.go.
	status omap
	// validations are the CEL rules, keyed by a path within the spec subtree:
	// "" is the spec itself, "components" the list, "components[]" one element,
	// "components[].replicas" a field of one element.
	validations map[string][]celRule
}

// celRule is one x-kubernetes-validations entry.
//
// The rules are hand-maintained here rather than generated from validate.go's
// tables, and ADR-0027 decision 5 is explicit that this is a second spelling of
// a subset of the Go validation. What it buys is the thing a status cannot: a
// `kubectl apply` of a malformed document fails at the API server, at the
// author's terminal, instead of being accepted and explained some seconds later
// by a controller the author may not be watching.
//
// The rules kept here are the ones that are trivially derivable and cannot go
// stale quietly: they name fields that exist in the schema above them, so a
// renamed field breaks the CRD's own validation at install time. Anything
// needing resolution, a ClusterProfile or cross-document context is not
// expressible in CEL and stays in validate.go, which remains the taxonomy of
// record — the message below quotes the code validate.go would produce, so the
// two refusals name the same thing.
type celRule struct {
	rule    string
	message string
}

func (r celRule) omap() omap {
	var m omap
	m.set("rule", r.rule)
	m.set("message", r.message)
	return m
}

// replicasBounds is the min/max rule, shared by Project components and
// Environment component overrides — the same Replicas struct in both places, so
// the same rule in both places.
var replicasBounds = celRule{
	rule: "!has(self.max) || self.max == 0 || !has(self.min) || self.max >= self.min",
	message: "replicas.max must be 0 (a fixed count of replicas.min) or at least replicas.min " +
		"(schema/out-of-range)",
}

var projectCRD = crdDef{
	kind:        "Project",
	listKind:    "ProjectList",
	plural:      "projects",
	singular:    "project",
	shortNames:  []string{"kproj"},
	description: "Project is the shared-configuration document: image and build coordinates, shared environment, and the components that deploy together. Everything that differs per deployment target lives in an Environment.",
	printerColumns: []omap{
		printerColumn("Ready", "string", `.status.conditions[?(@.type=="Ready")].status`,
			"whether the document validated"),
		printerColumn("Age", "date", ".metadata.creationTimestamp", ""),
	},
	status: projectStatusSchema(),
	validations: map[string][]celRule{
		"components[]": {{
			rule: "!(has(self.port) && has(self.schedule))",
			message: "a component sets port or schedule and never both: a port makes it a service, " +
				"a schedule makes it a cron (schema/mutually-exclusive)",
		}},
		"components[].replicas": {replicasBounds},
	},
}

var environmentCRD = crdDef{
	kind:        "Environment",
	listKind:    "EnvironmentList",
	plural:      "environments",
	singular:    "environment",
	shortNames:  []string{"kenv"},
	description: "Environment is where a Project runs and what differs there: target namespace, routing, policy, secret backend and per-component overrides. It binds to a Project in the same namespace by name (spec.project).",
	printerColumns: []omap{
		printerColumn("Phase", "string", ".status.phase", "the delivery state-machine phase"),
		printerColumn("Revision", "string", ".status.revision", "the settled revision"),
		printerColumn("Ready", "string", `.status.conditions[?(@.type=="Ready")].status`,
			"whether the document validated and rendered"),
		printerColumn("Age", "date", ".metadata.creationTimestamp", ""),
	},
	status: environmentStatusSchema(),
	validations: map[string][]celRule{
		"components[].replicas": {replicasBounds},
	},
}

func printerColumn(name, typ, path, description string) omap {
	var m omap
	m.set("name", name)
	m.set("type", typ)
	m.set("jsonPath", path)
	m.setIf(description != "", "description", description)
	return m
}

// crdHeader is the DO-NOT-EDIT banner every generated file carries, in the
// house style: it names the generator and the command that regenerates it, so a
// reader who found the file first can get back to the source.
const crdHeader = `# Code generated by internal/schemagen. DO NOT EDIT.
#
# The openAPIV3Schema below is reflected from the Go types in internal/model —
# the same reflection that produces schema/*.json — so the CRD and the published
# JSON Schema cannot disagree about what a valid kelson spec is (ADR-0027
# decision 4). Regenerate with:
#
#	go generate ./internal/model
`

// CRDs returns the committed CustomResourceDefinitions, keyed by file name.
func CRDs() map[string][]byte {
	out := make(map[string][]byte, len(documents))
	for _, d := range documents {
		spec := openAPI(reflectSpec(d.doc), "", d.crd.validations)
		body, err := marshalYAML(crdDocument(d.crd, spec))
		if err != nil {
			fatal(err)
		}
		out[d.crdFile] = append([]byte(crdHeader), body...)
	}
	return out
}

// crdDocument assembles one CustomResourceDefinition around a translated spec
// schema.
func crdDocument(def crdDef, spec omap) omap {
	var names omap
	names.set("kind", def.kind)
	names.set("listKind", def.listKind)
	names.set("plural", def.plural)
	names.set("singular", def.singular)
	names.setIf(len(def.shortNames) > 0, "shortNames", def.shortNames)

	var root omap
	root.set("description", def.description)
	root.set("type", "object")
	root.set("properties", omap{
		{"apiVersion", stringField("APIVersion defines the versioned schema of this representation of an object.")},
		{"kind", stringField("Kind is a string value representing the REST resource this object represents.")},
		// metadata is deliberately opaque. A structural schema may not describe
		// ObjectMeta's fields, and the API server validates them itself.
		{"metadata", omap{{"type", "object"}}},
		{"spec", spec},
		{"status", def.status},
	})
	// spec is required and status is not: a document with no spec says nothing,
	// and status is written by the controller through the subresource — an
	// author never supplies one.
	root.set("required", []string{"spec"})

	var schema omap
	schema.set("openAPIV3Schema", root)

	var version omap
	version.set("name", "v1alpha1")
	version.set("served", true)
	version.set("storage", true)
	// The status subresource is what ADR-0027 calls the actual prize: a
	// controller's status write cannot race a user's spec write, and the
	// controller needs no write permission on the spec at all.
	version.set("subresources", omap{{"status", omap{}}})
	version.set("additionalPrinterColumns", def.printerColumns)
	version.set("schema", schema)

	var crdSpec omap
	crdSpec.set("group", "kelson.dev")
	crdSpec.set("names", names)
	crdSpec.set("scope", "Namespaced")
	crdSpec.set("versions", []omap{version})

	var meta omap
	meta.set("name", def.plural+".kelson.dev")
	meta.set("labels", omap{
		{"app.kubernetes.io/name", "kelson"},
		{"app.kubernetes.io/component", "crd"},
	})

	var doc omap
	doc.set("apiVersion", "apiextensions.k8s.io/v1")
	doc.set("kind", "CustomResourceDefinition")
	doc.set("metadata", meta)
	doc.set("spec", crdSpec)
	return doc
}

func stringField(description string) omap {
	var m omap
	m.set("description", description)
	m.set("type", "string")
	return m
}

// openAPI translates one JSON Schema node into a structural OpenAPI v3 node.
//
// path is the node's location within the spec subtree, used to attach CEL rules:
// "" is the spec, "components" the list, "components[]" one element.
func openAPI(node map[string]any, path string, rules map[string][]celRule) omap {
	var out omap
	description, _ := node["description"].(string)
	out.setIf(description != "", "description", description)

	// A union arm cannot be expressed in a structural schema, so the node
	// becomes opaque and the API server stops checking it — see preserveUnknown.
	if _, union := node["oneOf"]; union {
		out.set(preserveUnknown, true)
		return out
	}

	typ, _ := node["type"].(string)
	out.setIf(typ != "", "type", typ)
	if format, ok := node["format"].(string); ok {
		out.set("format", format)
	}
	if enum, ok := node["enum"].([]any); ok {
		out.set("enum", enum)
	}
	if pattern, ok := node["pattern"].(string); ok {
		out.set("pattern", pattern)
	}
	for _, bound := range []string{"minLength", "maxLength", "minimum", "maximum", "minItems", "maxItems"} {
		if v, ok := node[bound]; ok {
			out.set(bound, number(v))
		}
	}
	if required, ok := node["required"].([]any); ok && len(required) > 0 {
		out.set("required", stringList(required))
	}

	if items, ok := node["items"].(map[string]any); ok {
		out.set("items", openAPI(items, path+"[]", rules))
	}

	properties, hasProperties := node["properties"].(map[string]any)
	if hasProperties {
		var sub omap
		for _, name := range sortedKeys(properties) {
			child, _ := properties[name].(map[string]any)
			sub.set(name, openAPI(child, join(path, name), rules))
		}
		out.set("properties", sub)
	}

	// `additionalProperties` is a *schema* only for a Go map type (env maps).
	// The `false` the reflector writes on every struct is dropped: a structural
	// schema may not carry it beside properties, and pruning already refuses
	// what it was refusing.
	additional, hasAdditional := node["additionalProperties"].(map[string]any)
	if hasAdditional {
		out.set("additionalProperties", openAPI(additional, path+"{}", rules))
	}

	// An object with neither properties nor a value schema is a free-form
	// document — `Component.Values`, the chart values written verbatim into a
	// HelmRelease. It has to be marked opaque or the API server would prune
	// every key in it.
	if typ == "object" && !hasProperties && !hasAdditional {
		out.set(preserveUnknown, true)
	}

	if rs := rules[path]; len(rs) > 0 {
		list := make([]omap, 0, len(rs))
		for _, r := range rs {
			list = append(list, r.omap())
		}
		out.set("x-kubernetes-validations", list)
	}
	return out
}

// preserveUnknown, for the record, is what happens to EnvValue.
//
// An environment value is a closed three-armed union — a plain string, a
// `{secret, key}` reference or a `{from: {service, key}}` binding — expressed in
// JSON Schema as a `oneOf` (model.EnvValue.JSONSchema). A structural CRD schema
// cannot say that: `oneOf` is permitted only as a *value validation*, whose
// branches may not carry `type`, `properties`, `additionalProperties` or
// `default`, and all three arms are structure rather than value constraints.
//
// So the union node becomes `x-kubernetes-preserve-unknown-fields: true`: the
// API server accepts and stores whatever is written there without pruning it,
// and the refusal moves to internal/model — the shape errors from
// EnvValue.UnmarshalJSON and the `secret/literal` rule in validate.go, surfaced
// as `status.validationErrors` by the controller. Nothing is accepted that was
// not accepted before; it is refused one hop later, by the plane that has the
// remediation text.
//
// Sharpening this is possible and deliberately not done in the first cut. The
// shape a future pass would take is a two-arm split — `type: string` versus an
// object with both reference forms as optional properties plus a CEL rule
// requiring exactly one — which needs the CRD schema to encode a union the
// authoring model expresses by construction, and is worth doing only with a
// test that proves the two agree on every document in examples/.
const preserveUnknown = "x-kubernetes-preserve-unknown-fields"

// join builds a spec-relative path for the CEL table.
func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// number converts a JSON number to the narrowest Go type that prints it back
// the way it was written. Every bound in the model is an integer, and an
// integer that round-tripped through float64 would come out of the YAML encoder
// as 6.5535e+04.
func number(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return i
	}
	f, err := n.Float64()
	if err != nil {
		fatal(fmt.Errorf("schema bound %q is not a number: %w", n, err))
	}
	return f
}

func stringList(vs []any) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// marshalYAML renders a document with two-space indentation, which is what
// every other YAML file in this repository uses and what `helm template` emits.
func marshalYAML(doc any) ([]byte, error) {
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
