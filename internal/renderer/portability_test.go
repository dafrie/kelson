package renderer_test

// TestPortabilityClaim is the mechanical form of issue #56's acceptance
// criterion: the SAME Application spec renders identically on a Gateway API
// cluster and on an Ingress cluster, the only difference being the routing
// resource.
//
// The golden harness alone cannot prove this: it discovers each fixture
// independently, so "two specs render" would pass the harness while the
// actual claim — one spec, portable across routing substrates — stays
// unproven. This test links the portable-gateway and portable-ingress
// fixtures explicitly and asserts the two properties that constitute the
// claim: the inputs are byte-identical, and the outputs differ only where the
// routing substrate lives.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/renderer"
)

const (
	portableGateway = "testdata/render/portable-gateway"
	portableIngress = "testdata/render/portable-ingress"
)

// TestPortabilityClaim links the fixture pair: identical spec.yaml in, output
// differing in exactly one document, and that document is the routing
// substrate (HTTPRoute vs Ingress). It verifies the paired fixtures name the
// pairing, so a future renames-or-deletes of one side fails loudly instead of
// silently dropping half the claim.
func TestPortabilityClaim(t *testing.T) {
	gwSpec := readFile(t, filepath.Join(portableGateway, "spec.yaml"))
	igSpec := readFile(t, filepath.Join(portableIngress, "spec.yaml"))
	if !bytes.Equal(gwSpec, igSpec) {
		t.Fatalf("portable-gateway and portable-ingress must share a byte-identical spec.yaml:\n%s",
			unifiedDiff(gwSpec, igSpec))
	}

	gwOut := renderFixture(t, portableGateway)
	igOut := renderFixture(t, portableIngress)

	gwDocs := splitDocs(gwOut)
	igDocs := splitDocs(igOut)

	if len(gwDocs) != len(igDocs) {
		t.Fatalf("document count differs: gateway %d, ingress %d", len(gwDocs), len(igDocs))
	}

	differs := -1
	for i := range gwDocs {
		if bytes.Equal([]byte(gwDocs[i]), []byte(igDocs[i])) {
			continue
		}
		if differs != -1 {
			t.Fatalf("documents %d and %d both differ; the outputs must differ ONLY in the routing resource",
				differs, i)
		}
		differs = i
	}

	if differs == -1 {
		t.Fatalf("gateway and ingress outputs are identical; expected the routing resource to differ")
	}

	gwKindAt, igKindAt := kindOf(gwDocs[differs]), kindOf(igDocs[differs])
	if gwKindAt != "HTTPRoute" || igKindAt != "Ingress" {
		t.Fatalf("the single differing document must be HTTPRoute on gateway / Ingress on ingress, got %q / %q",
			gwKindAt, igKindAt)
	}

	// Make the claim legible in a failure: show that the shared documents are
	// exactly the workload resource set, and that the routing resource is the
	// sole deviation.
	assertDoc(t, "ServiceAccount", gwDocs, igDocs, differs)
	assertDoc(t, "Service", gwDocs, igDocs, differs)
	assertDoc(t, "Deployment", gwDocs, igDocs, differs)
}

// renderFixture resolves and renders one fixture directory, caching nothing.
func renderFixture(t *testing.T, dir string) []byte {
	t.Helper()
	resolved, profile := loadFixture(t, dir)
	resolver := func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	}
	manifests, err := renderer.Render(resolved, profile, resolver)
	if err != nil {
		t.Fatalf("Render %s failed: %v", dir, err)
	}
	out, err := renderer.Encode(manifests)
	if err != nil {
		t.Fatalf("Encode %s failed: %v", dir, err)
	}
	return out
}

// splitDocs splits an Encode() stream into its per-document bodies. The
// separator is written by renderer.Encode, so the split is exact and order
// preserving.
func splitDocs(out []byte) []string {
	var docs []string
	for _, piece := range strings.Split(string(out), "---\n") {
		if piece = strings.Trim(piece, "\n"); piece != "" {
			docs = append(docs, piece)
		}
	}
	return docs
}

// kindOf extracts the "kind: X" line from an encoded single-document body.
func kindOf(doc string) string {
	for _, line := range strings.Split(doc, "\n") {
		if v, ok := strings.CutPrefix(line, "kind: "); ok {
			return v
		}
	}
	return ""
}

// readFile is os.ReadFile that fails the test instead of panicking.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// assertDoc checks that a kind named in a fixture's document list appears
// exactly once and is byte-identical across the pair, and that it is not the
// differing index.
func assertDoc(t *testing.T, kind string, gwDocs, igDocs []string, differs int) {
	t.Helper()
	gwi, ige := -1, -1
	for i := range gwDocs {
		if strings.Contains(gwDocs[i], "kind: "+kind) {
			gwi = i
		}
		if strings.Contains(igDocs[i], "kind: "+kind) {
			ige = i
		}
	}
	if gwi == -1 || ige == -1 {
		t.Fatalf("%s missing from one output (gateway=%d, ingress=%d)", kind, gwi, ige)
	}
	if gwi == differs || ige == differs {
		t.Fatalf("%s must not be the differing routing document", kind)
	}
	if gwDocs[gwi] != igDocs[ige] {
		t.Fatalf("%s differs across the pair; the shared workload resources must be byte-identical", kind)
	}
}
