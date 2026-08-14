// Command schemagen generates the published JSON Schemas for the model
// package (schema/project.schema.json, schema/environment.schema.json) and the
// CustomResourceDefinitions that carry the same model into a cluster
// (deploy/crds/*.yaml). It is invoked by `go generate ./internal/model` and is
// the single source path from Go types to both machine-readable forms.
//
// Two output formats, one pipeline. ADR-0027 decision 4 refuses a second,
// marker-driven generator for the CRDs: a marker set would be a second answer to
// *what is a valid kelson spec?*, and the first time a field gained a json tag
// without a marker (or the reverse) the JSON Schema and the CRD would disagree
// about what a user may write, silently. Everything below reflects over the same
// structs, once, and writes it out twice.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "../../schema", "output directory for the JSON Schemas, relative to the model package dir")
	crds := flag.String("crds", "../../deploy/crds", "output directory for the CustomResourceDefinitions")
	flag.Parse()

	if err := writeAll(*out, Schemas()); err != nil {
		fatal(err)
	}
	if err := writeAll(*crds, CRDs()); err != nil {
		fatal(err)
	}
}

// writeAll writes each generated file into dir, creating it if needed, and
// names what it wrote. The files are keyed by base name, so a caller passing a
// directory decides the whole path.
func writeAll(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range sortedKeys(files) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "schemagen:", err)
	os.Exit(1)
}
