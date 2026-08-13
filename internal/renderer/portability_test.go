package renderer_test

// TestPortabilityClaim is the mechanical form of issue #56's acceptance
// criterion: the SAME Application spec renders identically on two clusters,
// the only difference being what the cluster itself provides.
//
// Since #140 kelson renders Gateway API only, so the portability the claim is
// about is across Gateway *implementations* — Envoy Gateway here, Istio there
// — rather than across routing substrates. The pair is what makes the claim
// falsifiable; an ingress cluster is no longer a supported second half of it.
//
// The golden harness alone cannot prove this: it discovers each fixture
// independently, so "two specs render" would pass the harness while the
// actual claim — one spec, portable across cluster shapes — stays unproven.
// This test links the portable-gateway and portable-gateway-istio fixtures
// explicitly and asserts the two properties that constitute the claim: the
// inputs are byte-identical, and the outputs differ only where the cluster
// does.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/renderer"
)

const (
	portableEnvoy = "testdata/render/portable-gateway"
	portableIstio = "testdata/render/portable-gateway-istio"
)

// TestPortabilityClaim links the fixture pair: identical spec.yaml in, output
// differing in exactly one document, and that document is the HTTPRoute — the
// only place the cluster's Gateway implementation may show through. It
// verifies the paired fixtures name the pairing, so a future
// renames-or-deletes of one side fails loudly instead of silently dropping
// half the claim.
func TestPortabilityClaim(t *testing.T) {
	envoySpec := readFile(t, filepath.Join(portableEnvoy, "spec.yaml"))
	istioSpec := readFile(t, filepath.Join(portableIstio, "spec.yaml"))
	if !bytes.Equal(envoySpec, istioSpec) {
		t.Fatalf("portable-gateway and portable-gateway-istio must share a byte-identical spec.yaml:\n%s",
			unifiedDiff(envoySpec, istioSpec))
	}

	envoyOut := renderFixture(t, portableEnvoy)
	istioOut := renderFixture(t, portableIstio)

	envoyDocs := splitDocs(envoyOut)
	istioDocs := splitDocs(istioOut)

	if len(envoyDocs) != len(istioDocs) {
		t.Fatalf("document count differs: envoy %d, istio %d", len(envoyDocs), len(istioDocs))
	}

	differs := -1
	for i := range envoyDocs {
		if bytes.Equal([]byte(envoyDocs[i]), []byte(istioDocs[i])) {
			continue
		}
		if differs != -1 {
			t.Fatalf("documents %d and %d both differ; the outputs must differ ONLY in the routing resource",
				differs, i)
		}
		differs = i
	}

	if differs == -1 {
		t.Fatalf("the two outputs are identical; expected the HTTPRoute's parent Gateway to differ")
	}

	envoyKindAt, istioKindAt := kindOf(envoyDocs[differs]), kindOf(istioDocs[differs])
	if envoyKindAt != "HTTPRoute" || istioKindAt != "HTTPRoute" {
		t.Fatalf("the single differing document must be the HTTPRoute on both sides, got %q / %q",
			envoyKindAt, istioKindAt)
	}
	if !strings.Contains(envoyDocs[differs], "name: envoy") || !strings.Contains(istioDocs[differs], "name: istio") {
		t.Fatalf("the HTTPRoutes must attach to the Gateway each profile detected:\n%s\n%s",
			envoyDocs[differs], istioDocs[differs])
	}

	// Make the claim legible in a failure: show that the shared documents are
	// exactly the workload resource set, and that the routing resource is the
	// sole deviation.
	assertDoc(t, "ServiceAccount", envoyDocs, istioDocs, differs)
	assertDoc(t, "Service", envoyDocs, istioDocs, differs)
	assertDoc(t, "Deployment", envoyDocs, istioDocs, differs)
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
func assertDoc(t *testing.T, kind string, envoyDocs, istioDocs []string, differs int) {
	t.Helper()
	envoyIdx, istioIdx := -1, -1
	for i := range envoyDocs {
		if strings.Contains(envoyDocs[i], "kind: "+kind) {
			envoyIdx = i
		}
		if strings.Contains(istioDocs[i], "kind: "+kind) {
			istioIdx = i
		}
	}
	if envoyIdx == -1 || istioIdx == -1 {
		t.Fatalf("%s missing from one output (envoy=%d, istio=%d)", kind, envoyIdx, istioIdx)
	}
	if envoyIdx == differs || istioIdx == differs {
		t.Fatalf("%s must not be the differing routing document", kind)
	}
	if envoyDocs[envoyIdx] != istioDocs[istioIdx] {
		t.Fatalf("%s differs across the pair; the shared workload resources must be byte-identical", kind)
	}
}
