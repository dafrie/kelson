// Command gendoc generates docs/reference/support-matrix.md from the declared
// support matrix in internal/clusterprofile/support. It is invoked by
// `go generate ./internal/clusterprofile/support/gendoc` and is the single
// source path from the matrix data to the reference documentation (issue #57).
//
// It mirrors internal/specrefdoc: a main package wired into go generate that
// produces committed, drift-tested output. The rendering logic lives in the
// package itself so the drift test can call it directly.
package main

//go:generate go run . -out ../../../../docs

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	outDir := flag.String("out", "../../../../docs", "docs root; the page is written under <out>/reference")
	flag.Parse()

	page := Render()
	target := filepath.Join(filepath.Clean(*outDir), "reference", "support-matrix.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(target, page, 0o644); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", filepath.Join(*outDir, "reference", "support-matrix.md"))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gendoc:", err)
	os.Exit(1)
}
