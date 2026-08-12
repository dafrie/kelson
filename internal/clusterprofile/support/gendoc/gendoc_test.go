package main

// Drift test for the generated support matrix (issue #57).
//
// The acceptance criterion is that the support matrix documentation is
// generated from the same data the checks enforce, not hand-maintained. This
// test makes that mechanical: it regenerates docs/reference/support-matrix.md
// from support.Components and fails if the committed Markdown differs from
// what the generator produces. It also asserts determinism by generating
// twice and requiring identical bytes.
//
// Regenerate the committed page deliberately with:
//
//	KELSON_REF_UPDATE=1 go test ./internal/clusterprofile/support/gendoc/
//
// which is exactly what `go generate ./internal/clusterprofile/support/gendoc`
// does in a checked-in, normal way.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const docsDir = "../../../../docs"

// TestSupportMatrixGolden is the drift gate: the committed page under
// docs/reference must equal what Render produces from the matrix.
func TestSupportMatrixGolden(t *testing.T) {
	page := Render()
	again := Render()
	if !bytes.Equal(page, again) {
		t.Fatal("generation not deterministic")
	}

	target := filepath.Join(docsDir, "reference", "support-matrix.md")
	if os.Getenv("KELSON_REF_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(target, page, 0o644); err != nil {
			t.Fatalf("updating support matrix: %v", err)
		}
		t.Logf("updated %s", target)
		return
	}

	committed, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading committed %s: %v (generate it with KELSON_REF_UPDATE=1)", target, err)
	}
	if !bytes.Equal(committed, page) {
		t.Fatalf("generated support matrix drifted from committed %s.\n"+
			"Regenerate deliberately with KELSON_REF_UPDATE=1 go test ./internal/clusterprofile/support/gendoc/, "+
			"or edit support.Components in matrix.go (not this Markdown).\n\n%s",
			target, diffLines(committed, page))
	}
}

// diffLines shows the first differing line of want vs got.
func diffLines(want, got []byte) string {
	w := bytes.Split(want, []byte("\n"))
	g := bytes.Split(got, []byte("\n"))
	n := len(w)
	if len(g) > n {
		n = len(g)
	}
	for i := 0; i < n; i++ {
		var wl, gl string
		if i < len(w) {
			wl = string(w[i])
		}
		if i < len(g) {
			gl = string(g[i])
		}
		if wl != gl {
			return "first differing line (committed vs generated):\n  committed:\t" +
				wl + "\n  generated:\t" + gl
		}
	}
	return "no differing line found"
}
