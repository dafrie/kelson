package main

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/invopop/jsonschema"

	"github.com/dafrie/kelson/internal/model"
)

// documents are the kelson document kinds, in the order everything downstream
// lists them. Both outputs — the JSON Schemas and the CRDs — are driven from
// this one table, so a document kind is one row and not two pipelines.
//
// GitConnection (ADR-0033) is the row that proves the claim: it is not an
// authoring document, nothing renders it, and it needed no pipeline of its own —
// the same reflection over the same struct produces its published schema and its
// CRD, and the only thing hand-written for it is what the Go types cannot say
// (its names, its printer columns, its status and its CEL rules).
var documents = []struct {
	// schemaFile is the committed JSON Schema, under schema/.
	schemaFile string
	// crdFile is the committed CustomResourceDefinition, under deploy/crds/.
	crdFile string
	// doc is the Go value reflected over.
	doc any
	// crd is everything about the custom resource that is not derived from the
	// Go types: names, printer columns, the status schema and the CEL rules.
	crd crdDef
}{
	{
		schemaFile: "project.schema.json",
		crdFile:    "kelson.dev_projects.yaml",
		doc:        &model.Project{},
		crd:        projectCRD,
	},
	{
		schemaFile: "environment.schema.json",
		crdFile:    "kelson.dev_environments.yaml",
		doc:        &model.Environment{},
		crd:        environmentCRD,
	},
	{
		schemaFile: "gitconnection.schema.json",
		crdFile:    "kelson.dev_gitconnections.yaml",
		doc:        &model.GitConnection{},
		crd:        gitConnectionCRD,
	},
	{
		schemaFile: "gitsource.schema.json",
		crdFile:    "kelson.dev_gitsources.yaml",
		doc:        &model.GitSource{},
		crd:        gitSourceCRD,
	},
}

// reflector is the one reflector both outputs use.
//
// DoNotReference inlines every type at its use site rather than emitting $defs
// and $ref. That is what the published JSON Schema wants (an agent reading it
// should not have to resolve pointers), and it happens to be what a CRD
// *requires*: a structural openAPIV3Schema has no $ref at all, so a referencing
// reflector would mean an inlining pass here.
func reflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{
		DoNotReference:            true,
		ExpandedStruct:            true,
		AllowAdditionalProperties: false,
	}
}

// Schemas returns the committed JSON Schemas, keyed by file name.
func Schemas() map[string][]byte {
	r := reflector()
	out := make(map[string][]byte, len(documents))
	for _, d := range documents {
		schema := r.Reflect(d.doc)
		schema.ID = jsonschema.ID("https://kelson.dev/model/" + d.schemaFile)
		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			fatal(err)
		}
		// encoding/json escapes <, > and & for HTML safety, which is noise in a
		// schema nobody embeds in a page and makes the descriptions unreadable.
		data = bytes.ReplaceAll(data, []byte(`\u003c`), []byte("<"))
		data = bytes.ReplaceAll(data, []byte(`\u003e`), []byte(">"))
		data = bytes.ReplaceAll(data, []byte(`\u0026`), []byte("&"))
		out[d.schemaFile] = append(data, '\n')
	}
	return out
}

// reflectSpec returns the `spec` subtree of a document's reflected schema, as
// plain JSON values. It is the half of the schema a CRD carries: apiVersion,
// kind and metadata are the API server's, not the model's.
//
// Numbers are decoded as json.Number so an integer bound (`maximum: 65535`)
// survives into YAML as an integer instead of being widened to a float and
// re-serialized in exponent notation.
func reflectSpec(doc any) map[string]any {
	schema := reflector().Reflect(doc)
	data, err := json.Marshal(schema)
	if err != nil {
		fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		fatal(err)
	}
	props, _ := root["properties"].(map[string]any)
	spec, ok := props["spec"].(map[string]any)
	if !ok {
		fatal(errNoSpec{})
	}
	return spec
}

type errNoSpec struct{}

func (errNoSpec) Error() string {
	return "the reflected document has no spec property: the model's document types changed shape"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
