package main

// Drift tests for the two committed outputs (schema/*.json and
// deploy/crds/*.yaml).
//
// They mirror internal/specrefdoc's golden test, which is the convention this
// repository uses for every generated artifact: the generator is a package, a
// test calls it, and the committed bytes must equal what it produces. The
// moment someone hand-edits a schema — or changes a Go type in internal/model
// and does not regenerate — the test goes red, which is the only thing that
// makes "generated, not hand-maintained" true rather than aspirational.
//
// Regenerate the committed files deliberately with:
//
//	go generate ./internal/model
//
// or, equivalently for the drift test's purposes:
//
//	KELSON_SCHEMA_UPDATE=1 go test ./internal/schemagen/

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	updateEnv = "KELSON_SCHEMA_UPDATE"
	schemaDir = "../../schema"
	crdDir    = "../../deploy/crds"
)

func TestSchemasMatchCommitted(t *testing.T) {
	assertCommitted(t, schemaDir, Schemas)
}

func TestCRDsMatchCommitted(t *testing.T) {
	assertCommitted(t, crdDir, CRDs)
}

// assertCommitted regenerates into dir and compares, generating twice first so
// a non-deterministic generator fails on determinism rather than on a diff that
// looks like someone else's fault.
func assertCommitted(t *testing.T, dir string, generate func() map[string][]byte) {
	t.Helper()
	files := generate()
	again := generate()
	if len(files) == 0 {
		t.Fatalf("the generator produced nothing, so this comparison would pass vacuously")
	}
	for _, name := range sortedKeys(files) {
		if !bytes.Equal(files[name], again[name]) {
			t.Fatalf("generation is not deterministic for %s", name)
		}
	}

	if os.Getenv(updateEnv) == "1" {
		if err := writeAll(dir, files); err != nil {
			t.Fatalf("updating %s: %v", dir, err)
		}
		t.Logf("updated %s", dir)
		return
	}

	for _, name := range sortedKeys(files) {
		target := filepath.Join(dir, name)
		committed, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("reading committed %s: %v (generate it with `go generate ./internal/model`)", target, err)
		}
		if !bytes.Equal(committed, files[name]) {
			t.Errorf("%s has drifted from what the generator produces.\n"+
				"Regenerate with `go generate ./internal/model` (or %s=1 go test ./internal/schemagen/), "+
				"and change the Go types in internal/model rather than this file.\n\n%s",
				target, updateEnv, firstDifference(committed, files[name]))
		}
	}
}

// firstDifference names the first line that differs, which is almost always
// enough to see what changed and is far shorter than two whole schemas.
func firstDifference(want, got []byte) string {
	w := bytes.Split(want, []byte("\n"))
	g := bytes.Split(got, []byte("\n"))
	n := max(len(w), len(g))
	for i := range n {
		var wl, gl string
		if i < len(w) {
			wl = string(w[i])
		}
		if i < len(g) {
			gl = string(g[i])
		}
		if wl != gl {
			return fmt.Sprintf("first difference at line %d:\n  committed: %s\n  generated: %s", i+1, wl, gl)
		}
	}
	return "the files differ only in trailing bytes"
}

// TestCRDSchemasAreStructural checks the properties a CRD's openAPIV3Schema
// must have for the API server to accept it as *structural* — the precondition
// for pruning, for defaulting and for the status subresource.
//
// It is a cheap local check and not a substitute for installing the CRD, which
// is what the envtest suite does (internal/controller, `make envtest`). What it
// buys is that a plain `go test ./...` on a machine with no cluster assets still
// catches the mistakes that are easy to make here: a node with properties and no
// type, a leftover `additionalProperties: false`, a `default` the API server
// would start writing into people's objects.
func TestCRDSchemasAreStructural(t *testing.T) {
	for name, body := range CRDs() {
		t.Run(name, func(t *testing.T) {
			var doc map[string]any
			if err := yaml.Unmarshal(body, &doc); err != nil {
				t.Fatalf("the generated CRD is not YAML: %v", err)
			}
			schema := crdOpenAPISchema(t, doc)
			walkSchema(t, schema, "$")
		})
	}
}

// crdOpenAPISchema digs the openAPIV3Schema out of a decoded CRD, failing with
// the path it lost its way at rather than with a nil map panic.
func crdOpenAPISchema(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		t.Fatalf("CRD has no spec")
	}
	versions, ok := spec["versions"].([]any)
	if !ok || len(versions) != 1 {
		t.Fatalf("CRD must serve exactly one version, got %v", spec["versions"])
	}
	version, _ := versions[0].(map[string]any)
	if version["served"] != true || version["storage"] != true {
		t.Errorf("the single version must be both served and storage")
	}
	if _, ok := version["subresources"].(map[string]any)["status"]; !ok {
		t.Errorf("the status subresource must be enabled (ADR-0027 decision 1)")
	}
	schema, ok := version["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	if !ok {
		t.Fatalf("version has no schema.openAPIV3Schema")
	}
	return schema
}

func walkSchema(t *testing.T, node map[string]any, path string) {
	t.Helper()
	for _, forbidden := range []string{"$ref", "$defs", "$schema", "$id", "oneOf", "anyOf", "allOf", "not", "default"} {
		if _, present := node[forbidden]; present {
			t.Errorf("%s carries %q, which a structural CRD schema may not have "+
				"(see internal/schemagen/crd.go for what each of these translates into)", path, forbidden)
		}
	}
	if ap, present := node["additionalProperties"]; present {
		if _, isSchema := ap.(map[string]any); !isSchema {
			t.Errorf("%s carries a boolean additionalProperties; it must be dropped or be a schema", path)
		}
		if _, alsoProperties := node["properties"]; alsoProperties {
			t.Errorf("%s carries both properties and additionalProperties, which are mutually exclusive", path)
		}
	}

	_, hasType := node["type"]
	_, opaque := node["x-kubernetes-preserve-unknown-fields"]
	if !hasType && !opaque {
		t.Errorf("%s specifies no type and is not marked preserve-unknown-fields: "+
			"every node of a structural schema must say what it is", path)
	}

	if props, ok := node["properties"].(map[string]any); ok {
		if node["type"] != "object" {
			t.Errorf("%s has properties but type %v", path, node["type"])
		}
		for _, name := range sortedKeys(props) {
			child, ok := props[name].(map[string]any)
			if !ok {
				t.Errorf("%s.%s is not a schema", path, name)
				continue
			}
			walkSchema(t, child, path+"."+name)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		if node["type"] != "array" {
			t.Errorf("%s has items but type %v", path, node["type"])
		}
		walkSchema(t, items, path+"[]")
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		walkSchema(t, ap, path+"{}")
	}
}

// TestCRDSpecMatchesPublishedSchema is the claim ADR-0027 decision 4 makes out
// loud: one pipeline, two output formats. Every property the JSON Schema
// declares under `spec` must be a property of the CRD's spec, and vice versa —
// if the two ever came from different reflections, this is where it shows.
func TestCRDSpecMatchesPublishedSchema(t *testing.T) {
	crds := CRDs()
	schemas := Schemas()
	for _, d := range documents {
		t.Run(d.crdFile, func(t *testing.T) {
			var published map[string]any
			if err := json.Unmarshal(schemas[d.schemaFile], &published); err != nil {
				t.Fatalf("decoding %s: %v", d.schemaFile, err)
			}
			wantSpec, _ := published["properties"].(map[string]any)["spec"].(map[string]any)
			want, _ := wantSpec["properties"].(map[string]any)

			var crd map[string]any
			if err := yaml.Unmarshal(crds[d.crdFile], &crd); err != nil {
				t.Fatalf("decoding %s: %v", d.crdFile, err)
			}
			gotSpec, _ := crdOpenAPISchema(t, crd)["properties"].(map[string]any)["spec"].(map[string]any)
			got, _ := gotSpec["properties"].(map[string]any)

			if a, b := strings.Join(sortedKeys(want), ","), strings.Join(sortedKeys(got), ","); a != b {
				t.Errorf("spec properties differ between the schema and the CRD:\n  %s: %s\n  %s: %s",
					d.schemaFile, a, d.crdFile, b)
			}
		})
	}
}
