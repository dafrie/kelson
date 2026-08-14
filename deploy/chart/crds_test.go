package chart

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The chart's crds/ directory is a copy of deploy/crds/, and a copy nobody
// compares is a copy that drifts.
//
// It has to be a copy rather than a reference: Helm requires plain,
// untemplated YAML in a chart's `crds/` directory, and a chart is packaged from
// its own directory, so a symlink out of it does not survive `helm package`.
// The generated files are the source of truth (internal/schemagen, ADR-0027
// decision 4) and this test is what keeps the copy honest — the drift would
// otherwise be silent and sharp, because a chart carrying last week's schema
// would prune fields today's controller expects to read.
//
// Refresh the copies with:
//
//	go generate ./internal/model && cp deploy/crds/*.yaml deploy/chart/kelson/crds/
const (
	generatedCRDDir = "../crds"
	chartCRDDir     = "kelson/crds"
)

func TestChartCRDsMatchGenerated(t *testing.T) {
	names := crdNames(t, generatedCRDDir)
	if len(names) == 0 {
		t.Fatalf("%s holds no CRDs — the comparison would pass vacuously", generatedCRDDir)
	}
	if got := crdNames(t, chartCRDDir); !equalStrings(got, names) {
		t.Fatalf("the chart ships %v but %s holds %v.\n"+
			"Copy the generated files into the chart: cp deploy/crds/*.yaml deploy/chart/kelson/crds/",
			got, generatedCRDDir, names)
	}
	for _, name := range names {
		want := readFile(t, filepath.Join(generatedCRDDir, name))
		got := readFile(t, filepath.Join(chartCRDDir, name))
		if !bytes.Equal(want, got) {
			t.Errorf("%s in the chart has drifted from the generated %s.\n"+
				"These must stay byte-identical: a `kubectl apply -f deploy/crds` install and a "+
				"`helm install` have to register the same schema.\n"+
				"Fix it with: cp deploy/crds/*.yaml deploy/chart/kelson/crds/", name, name)
		}
	}
}

// TestChartCRDsAreNotTemplated pins Helm's rule about the directory: files under
// crds/ are applied verbatim and are never rendered, so a `{{` in one is a
// literal brace in a cluster's API schema rather than a value.
func TestChartCRDsAreNotTemplated(t *testing.T) {
	for _, name := range crdNames(t, chartCRDDir) {
		if bytes.Contains(readFile(t, filepath.Join(chartCRDDir, name)), []byte("{{")) {
			t.Errorf("%s contains a Helm action; files under crds/ are applied verbatim and never rendered", name)
		}
	}
}

func crdNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".yaml" {
			names = append(names, e.Name())
		}
	}
	return names
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
