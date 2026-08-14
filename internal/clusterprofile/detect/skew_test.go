package detect

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// The "newer than tested" ceiling (issue #57) is only honest if the number in
// the support matrix is the version kelson is actually exercised against. That
// version lives in the e2e harness, which pins a kind node image — two places,
// one fact, and nothing but this test stops them drifting the moment somebody
// bumps the harness.
//
// It lives in package detect rather than in support because support is pure by
// contract (no filesystem, so preview and the renderer can import it), and
// detect is where the repository's own files are already read in tests
// (rbac_test.go).

// nodeImagePattern extracts the Kubernetes version from the pinned kind node
// image, e.g. `NODE_IMAGE="kindest/node:v1.34.8@sha256:..."` -> 1.34.8.
var nodeImagePattern = regexp.MustCompile(`NODE_IMAGE="kindest/node:v([0-9]+\.[0-9]+)`)

// TestKubernetesTestedCeilingMatchesE2E fails when the matrix claims to be
// tested against a Kubernetes minor the e2e harness does not run. Either number
// may move; they may not move independently.
func TestKubernetesTestedCeilingMatchesE2E(t *testing.T) {
	comp, ok := support.Lookup("kubernetes")
	if !ok {
		t.Fatal("the support matrix has no kubernetes row")
	}
	if comp.Tested == "" {
		t.Fatal("the kubernetes row claims no tested ceiling, but the e2e harness pins one")
	}

	path := filepath.Join("..", "..", "..", "hack", "e2e", "lib.sh")
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	match := nodeImagePattern.FindStringSubmatch(string(data))
	if match == nil {
		t.Fatalf("no pinned kindest/node image found in %s — the ceiling has no source to agree with", path)
	}
	if got, want := comp.Tested, match[1]; got != want {
		t.Errorf("support matrix says kubernetes is tested up to %q, e2e runs %q (%s).\n"+
			"Bump the Tested field in internal/clusterprofile/support/matrix.go and regenerate the "+
			"support matrix doc, or pin the harness back.", got, want, path)
	}
}

// TestEveryMatrixRowNamesWhatDegrades: a floor without a named degradation is
// the failure issue #57 is about, one level up. "cnpg is too old" sends a
// reader to the source; "so every kind: postgres component refuses to render"
// is a decision they can make, and the matrix is where that sentence lives.
func TestEveryMatrixRowNamesWhatDegrades(t *testing.T) {
	for _, c := range support.Components {
		if strings.TrimSpace(c.Affects) == "" {
			t.Errorf("support matrix row %q declares a floor but does not say what degrades below it", c.Name)
		}
	}
}
