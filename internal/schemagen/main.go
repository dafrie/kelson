// Command schemagen generates the published JSON Schemas for the model
// package (schema/project.schema.json, schema/environment.schema.json). It is
// invoked by `go generate ./internal/model` and is the single source path
// from Go types to the machine-readable schema agents reason against.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/invopop/jsonschema"

	"github.com/dafrie/kelson/internal/model"
)

func main() {
	out := flag.String("out", "../../schema", "output directory, relative to the model package dir")
	flag.Parse()

	schemas := []struct {
		name string
		doc  any
	}{
		{"project.schema.json", &model.Project{}},
		{"environment.schema.json", &model.Environment{}},
	}

	r := &jsonschema.Reflector{
		DoNotReference:            true,
		ExpandedStruct:            true,
		AllowAdditionalProperties: false,
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	for _, s := range schemas {
		schema := r.Reflect(s.doc)
		schema.ID = jsonschema.ID("https://kelson.dev/model/" + s.name)
		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			fatal(err)
		}
		data = bytes.ReplaceAll(data, []byte(`\u003c`), []byte("<"))
		data = bytes.ReplaceAll(data, []byte(`\u003e`), []byte(">"))
		data = bytes.ReplaceAll(data, []byte(`\u0026`), []byte("&"))
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(*out, s.name), data, 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", filepath.Join(*out, s.name))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "schemagen:", err)
	os.Exit(1)
}
