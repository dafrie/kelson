// Command specrefdoc generates the user-facing spec reference
// (docs/reference/*.md) from the committed JSON Schemas (schema/*.json). It is
// invoked by `go generate ./internal/model` and is the single source path from
// the schemas to the reference documentation (issue #22).
//
// It mirrors internal/schemagen: a main package wired into go generate that
// produces committed, drift-tested output. The rendering logic lives in the
// package itself so the golden test can call it directly.
package main

//go:generate go run . -schema ../../schema -out ../../docs

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	schemaDir := flag.String("schema", "../../schema", "directory holding the committed JSON Schemas")
	outDir := flag.String("out", "../../docs", "docs root; pages are written under <out>/reference")
	flag.Parse()

	pages, err := Generate(*schemaDir)
	if err != nil {
		fatal(err)
	}
	written, err := WriteAll(filepath.Clean(*outDir), pages)
	if err != nil {
		fatal(err)
	}
	for _, w := range written {
		fmt.Println("wrote", filepath.Join(*outDir, filepath.FromSlash(w)))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "specrefdoc:", err)
	os.Exit(1)
}
