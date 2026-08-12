package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// The image reference standing in for a build result when an example builds
// from source. Pinned by digest, like the build plane's own output.
const exampleBuiltImage = "ghcr.io/acme/example@sha256:9f6ad2c1b4d5e8073a1c2f4b6d8e0a1c3e5f7091b2d4c6e8a0f2b4d6c8e0a2f4"

// TestExamplesRender walks every project under examples/ and renders each of
// its environments, which is what a reader of the README does first.
//
// The contract (issue #136): a render either succeeds with no unresolved image
// anywhere in the output, or fails with the structured image/unresolved error —
// never `image: "@"`, which is what every source+build example used to produce.
// Examples that do fail must then render cleanly once --image supplies the
// artifact a build would have produced, so the error names a way out that
// actually works.
func TestExamplesRender(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading examples: %v", err)
	}
	rendered := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files := specFilesIn(t, dir)
		for _, env := range environmentsIn(t, files) {
			rendered++
			t.Run(e.Name()+"/"+env, func(t *testing.T) {
				args := append(renderArgs(files), "--env", env)
				stdout, _, err := runKelson(t, append([]string{"render"}, args...)...)
				if err == nil {
					assertNoPlaceholder(t, stdout)
					return
				}
				if !strings.Contains(err.Error(), "["+renderer.ErrImageUnresolved+"]") {
					t.Fatalf("render failed for a reason other than an unresolved image: %v", err)
				}
				if strings.Contains(stdout, "kind: ") {
					t.Fatalf("a failed render must print no manifests:\n%s", stdout)
				}
				// The remediation must work: supplying the build result renders.
				withImage := append(slices.Clone(args), "--image", exampleBuiltImage)
				stdout, _, err = runKelson(t, append([]string{"render"}, withImage...)...)
				if err != nil {
					t.Fatalf("render with --image failed: %v", err)
				}
				assertNoPlaceholder(t, stdout)
				if !strings.Contains(stdout, "image: "+exampleBuiltImage) {
					t.Fatalf("--image did not reach the workloads:\n%s", stdout)
				}
			})
		}
	}
	if rendered == 0 {
		t.Fatalf("no examples found under %s", root)
	}
}

// assertNoPlaceholder fails if any rendered image is the build placeholder,
// whatever quoting the YAML encoder chose for it.
func assertNoPlaceholder(t *testing.T, manifests string) {
	t.Helper()
	for _, line := range strings.Split(manifests, "\n") {
		field := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		value, ok := strings.CutPrefix(field, "image:")
		if !ok {
			continue
		}
		if strings.Trim(strings.TrimSpace(value), `"'`) == model.ImageUnresolved {
			t.Fatalf("rendered an unresolved image placeholder:\n%s", line)
		}
	}
}

func specFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no spec files in %s", dir)
	}
	return files
}

func environmentsIn(t *testing.T, files []string) []string {
	t.Helper()
	_, environments, _, err := loadSpecFiles(files)
	if err != nil {
		t.Fatalf("loading %v: %v", files, err)
	}
	names := make([]string, len(environments))
	for i, e := range environments {
		names[i] = e.Metadata.Name
	}
	return names
}

func renderArgs(files []string) []string {
	args := make([]string, 0, 2*len(files))
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return args
}
