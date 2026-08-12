package main

// Drift test for the generated spec reference (issue #22).
//
// The acceptance criterion is that spec reference documentation is generated
// from the schema, not hand-maintained. This test makes that mechanical: it
// regenerates docs/reference/*.md from schema/*.json and fails if the
// committed Markdown differs from what the generator produces — the moment
// someone hand-edits the page (or the Go model changes without regenerating),
// the test goes red.
//
// Regenerate the committed pages deliberately with:
//
//	KELSON_REF_UPDATE=1 go test ./internal/specrefdoc/
//
// which is exactly what `go generate ./internal/specrefdoc` does in a
// checked-in, normal way.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const refUpdateEnv = "KELSON_REF_UPDATE"

// TestSpecReferenceGolden is the drift gate: the committed reference under
// docs/reference must equal what Generate produces from schema/. It also
// asserts determinism by generating twice and requiring identical bytes.
func TestSpecReferenceGolden(t *testing.T) {
	const schemaDir = "../../schema"
	const docsDir = "../../docs"

	pages, err := Generate(schemaDir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	again, err := Generate(schemaDir)
	if err != nil {
		t.Fatalf("Generate (2nd): %v", err)
	}

	for _, r := range refs {
		if !bytes.Equal(pages[r.file], again[r.file]) {
			t.Fatalf("generation not deterministic for %s", r.file)
		}
		target := filepath.Join(docsDir, filepath.FromSlash(r.file))
		committed, rerr := os.ReadFile(target)
		if os.Getenv(refUpdateEnv) == "1" {
			if _, werr := WriteAll(docsDir, pages); werr != nil {
				t.Fatalf("updating reference: %v", werr)
			}
			t.Logf("updated reference under %s", docsDir)
			continue
		}
		if rerr != nil {
			t.Fatalf("reading committed %s: %v (generate it with %s=1)", r.file, rerr, refUpdateEnv)
		}
		if !bytes.Equal(committed, pages[r.file]) {
			t.Fatalf("generated reference drifted from committed %s.\n"+
				"Regenerate deliberately with %s=1 go test ./internal/specrefdoc/, "+
				"or edit the Go model and schema (not this Markdown).\n\n%s",
				target, refUpdateEnv, diffLines(committed, pages[r.file]))
		}
	}
}

// diffLines shows the first differring line of want vs got.
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
			return "first differing line (committed vs generated):\n  committed: " +
				wl + "\n  generated: " + gl
		}
	}
	return "no differing line found"
}
