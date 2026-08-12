package diff_test

// Golden-file harness for the L1 diff engine (issue #42), in the renderer's
// style (internal/renderer/golden_test.go).
//
// Each directory under testdata/diff is one fixture: prev.yaml and cur.yaml
// (each a Project + Environment document), profile.yaml (a ClusterProfile),
// and expected.golden (the plain-text terminal render of the diff, no colour).
// Adding a case is adding a fixture, never editing test code.
//
// Regenerate goldens deliberately with: KELSON_GOLDEN_UPDATE=1 go test ./internal/diff/
// Every fixture also asserts determinism: the render+diff runs 8 times and
// every byte must be identical.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

const diffGoldenUpdateEnv = "KELSON_GOLDEN_UPDATE"

func TestGoldenFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata/diff")
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}
	ran := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ran++
		t.Run(e.Name(), func(t *testing.T) {
			runFixture(t, filepath.Join("testdata/diff", e.Name()))
		})
	}
	if ran == 0 {
		t.Fatalf("no fixtures found under testdata/diff")
	}
}

type pair struct {
	resolved *model.Resolved
	profile  clusterprofile.ClusterProfile
	resolver renderer.OverlayResolver
	dir      string
}

func loadPair(t *testing.T, dir string) *pair {
	t.Helper()
	profileBytes, err := os.ReadFile(filepath.Join(dir, "profile.yaml"))
	if err != nil {
		t.Fatalf("reading profile.yaml: %v", err)
	}
	var profile clusterprofile.ClusterProfile
	if err := yaml.Unmarshal(profileBytes, &profile); err != nil {
		t.Fatalf("profile.yaml does not parse: %v", err)
	}
	resolver := func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	}
	return &pair{profile: profile, resolver: resolver, dir: dir}
}

func (p *pair) render(t *testing.T, doc string) []renderer.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.dir, doc))
	if err != nil {
		t.Fatalf("reading %s: %v", doc, err)
	}
	docs, derrs := model.DecodeDocuments(raw)
	if len(derrs) > 0 {
		t.Fatalf("%s does not validate:\n%v", doc, derrs)
	}
	var project *model.Project
	var environment *model.Environment
	for _, d := range docs {
		switch v := d.(type) {
		case *model.Project:
			project = v
		case *model.Environment:
			environment = v
		}
	}
	if project == nil || environment == nil {
		t.Fatalf("%s must contain one Project and one Environment document", doc)
	}
	resolved, rerrs := model.Resolve(project, environment)
	if len(rerrs) > 0 {
		t.Fatalf("%s does not resolve:\n%v", doc, rerrs)
	}
	ms, err := renderer.Render(resolved, p.profile, p.resolver)
	if err != nil {
		t.Fatalf("Render(%s) failed: %v", doc, err)
	}
	p.resolved = resolved
	return ms
}

// originFor computes the overlay OriginFor for cur by re-rendering the same
// resolved spec with overlays stripped (the model-only baseline).
func (p *pair) originFor(t *testing.T, cur []renderer.Manifest) diff.OriginFor {
	t.Helper()
	if p.resolved == nil || len(p.resolved.Overlays) == 0 {
		return nil
	}
	modelOnly := *p.resolved
	modelOnly.Overlays = nil
	base, err := renderer.Render(&modelOnly, p.profile, nil)
	if err != nil {
		t.Fatalf("model-only render failed: %v", err)
	}
	org, err := diff.NewOverlayOrigins(cur, base)
	if err != nil {
		t.Fatalf("NewOverlayOrigins failed: %v", err)
	}
	return org
}

func runFixture(t *testing.T, dir string) {
	t.Helper()
	p := loadPair(t, dir)

	const runs = 8
	var first []byte
	for i := 0; i < runs; i++ {
		prev := p.render(t, "prev.yaml")
		cur := p.render(t, "cur.yaml")
		project := p.resolved.Project
		env := p.resolved.Environment.Name
		org := p.originFor(t, cur)

		d, err := diff.Between(project, env, prev, cur, org)
		if err != nil {
			t.Fatalf("Between failed: %v", err)
		}
		var buf bytes.Buffer
		if err := diff.Write(&buf, d, false); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		if i == 0 {
			first = buf.Bytes()
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("run %d differed from first (determinism violated)", i)
		}
	}

	goldenPath := filepath.Join(dir, "expected.golden")
	if os.Getenv(diffGoldenUpdateEnv) == "1" {
		if err := os.WriteFile(goldenPath, first, 0o644); err != nil {
			t.Fatalf("updating golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden %s: %v (create it with %s=1)", goldenPath, err, diffGoldenUpdateEnv)
	}
	if !bytes.Equal(want, first) {
		t.Fatalf("golden mismatch against %s; regenerate with %s=1 go test ./internal/diff/",
			goldenPath, diffGoldenUpdateEnv)
	}
}
