package renderer_test

// Golden-file harness for the renderer (issue #27).
//
// Each directory under testdata/render is one fixture: spec.yaml (a Project
// and an Environment document), profile.yaml (a ClusterProfile), and
// expected.golden (the rendered multi-document YAML). Cluster shapes are
// fixtures, not configuration: a new shape is a new directory.
//
// Regenerate goldens deliberately with: KELSON_GOLDEN_UPDATE=1 go test ./internal/renderer/
// Every case also asserts determinism: the fixture renders 8 times and every
// byte must be identical.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

const goldenUpdateEnv = "KELSON_GOLDEN_UPDATE"

func TestGoldenFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata/render")
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
			runFixture(t, filepath.Join("testdata/render", e.Name()))
		})
	}
	if ran == 0 {
		t.Fatalf("no fixtures found under testdata/render")
	}
}

func runFixture(t *testing.T, dir string) {
	t.Helper()
	resolved, profile := loadFixture(t, dir)
	resolver := func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	}

	const renders = 8
	var first []byte
	for i := 0; i < renders; i++ {
		manifests, err := renderer.Render(resolved, profile, resolver)
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		out, err := renderer.Encode(manifests)
		if err != nil {
			t.Fatalf("Encode failed: %v", err)
		}
		if i == 0 {
			first = out
			continue
		}
		if !bytes.Equal(first, out) {
			t.Fatalf("render %d differed from first render (determinism violated)", i)
		}
	}

	goldenPath := filepath.Join(dir, "expected.golden")
	if os.Getenv(goldenUpdateEnv) == "1" {
		if err := os.WriteFile(goldenPath, first, 0o644); err != nil {
			t.Fatalf("updating golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden %s: %v (create it with %s=1)", goldenPath, err, goldenUpdateEnv)
	}
	if !bytes.Equal(want, first) {
		t.Fatalf("golden mismatch against %s\n%s\nregenerate with %s=1 go test ./internal/renderer/",
			goldenPath, unifiedDiff(want, first), goldenUpdateEnv)
	}
}

func loadFixture(t *testing.T, dir string) (*model.Resolved, clusterprofile.ClusterProfile) {
	t.Helper()
	specBytes, err := os.ReadFile(filepath.Join(dir, "spec.yaml"))
	if err != nil {
		t.Fatalf("reading spec.yaml: %v", err)
	}
	docs, derrs := model.DecodeDocuments(specBytes)
	if len(derrs) > 0 {
		t.Fatalf("spec.yaml does not validate:\n%v", derrs)
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
		t.Fatalf("spec.yaml must contain one Project and one Environment document")
	}
	resolved, rerrs := model.Resolve(project, environment)
	if len(rerrs) > 0 {
		t.Fatalf("spec does not resolve:\n%v", rerrs)
	}

	profileBytes, err := os.ReadFile(filepath.Join(dir, "profile.yaml"))
	if err != nil {
		t.Fatalf("reading profile.yaml: %v", err)
	}
	var profile clusterprofile.ClusterProfile
	if err := yaml.Unmarshal(profileBytes, &profile); err != nil {
		t.Fatalf("profile.yaml does not parse: %v", err)
	}
	return resolved, profile
}

type diffLine struct {
	op   byte // ' ', '-', '+'
	text string
}

// unifiedDiff prints an expected-vs-got diff with context lines, so a golden
// failure is reviewable without re-running anything.
func unifiedDiff(want, got []byte) string {
	a := strings.Split(strings.TrimRight(string(want), "\n"), "\n")
	b := strings.Split(strings.TrimRight(string(got), "\n"), "\n")

	// Longest common subsequence over lines; fixtures are small enough that
	// the quadratic table is the simple, obviously-correct choice.
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var ops []diffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffLine{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffLine{'-', a[i]})
			i++
		default:
			ops = append(ops, diffLine{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffLine{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffLine{'+', b[j]})
	}

	const context = 3
	var out strings.Builder
	out.WriteString("--- expected.golden\n+++ rendered output\n")
	for start := 0; start < len(ops); {
		if ops[start].op == ' ' {
			start++
			continue
		}
		end := start
		for end < len(ops) && (ops[end].op != ' ' || gap(end+1, ops) <= 2*context) {
			end++
		}
		lo := start - context
		if lo < 0 {
			lo = 0
		}
		hi := end + context
		if hi > len(ops) {
			hi = len(ops)
		}
		out.WriteString("@@\n")
		for k := lo; k < hi; k++ {
			fmt.Fprintf(&out, "%c%s\n", ops[k].op, ops[k].text)
		}
		start = end
	}
	return out.String()
}

// gap counts consecutive unchanged lines starting at idx.
func gap(idx int, ops []diffLine) int {
	n := 0
	for idx+n < len(ops) && ops[idx+n].op == ' ' {
		n++
	}
	return n
}
