package controller

// The Flux CRD fixtures (issue #243) and the three things that must stay true
// about them.
//
// testdata/flux-crds/ holds the CustomResourceDefinitions for the two kinds
// this package server-side applies — Kustomization and OCIRepository — cut
// verbatim out of one pinned upstream Flux install manifest by
// hack/flux-crds.sh. The envtest suite installs them into its API server
// (envtest_test.go), which is what turns "kelson wrote a map with these keys"
// into "a real API server accepted this object against Flux's own schema".
//
// # Why bytes are committed at all, when ADR-0021 says nothing is vendored
//
// That rule governs what kelson *applies to a user's cluster*: the pins table
// holds a URL and a digest, `kelson install` fetches at install time, and the
// Bitnami lesson behind it is untouched. Nothing here is ever applied anywhere
// but a throwaway envtest API server that exists for the length of one `go
// test`.
//
// A fixture cannot follow the rule even so. envtest reads CRDs off local disk
// before its API server starts, so a suite that fetched them would fail
// air-gapped, fail in an offline runner, and make every run depend on GitHub
// being reachable. What it follows instead is ADR-0030 decision 2's shape — a
// mechanically regenerated snapshot in kelson's custody rather than a
// hand-edited copy — and this file is the discipline that makes that claim
// checkable rather than aspirational:
//
//   - the bytes are the ones the script wrote, so nobody has quietly edited a
//     schema to make a test pass;
//   - the pinned Flux version is inside the minor the product actually installs
//     (install.FluxDistributionVersion, ADR-0030), so the schemas the tests
//     validate against are the schemas the cluster will serve;
//   - the fixtures declare the exact group, version and kind this package
//     writes (internal/delivery/flux's GVRs), so a Flux API bump cannot leave
//     the controller applying to a version its own fixtures do not describe.
//
// These run in `go test ./...` with no build tag and no envtest assets, which
// is the point: drift is caught on every CI run, not only on the machines that
// can start an API server.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/delivery/install"
)

// fluxCRDDir is the fixture directory, relative to this package. It is here
// rather than in envtest_test.go so the drift tests below and the tagged suite
// name one path between them.
const fluxCRDDir = "testdata/flux-crds"

// The pin. Refresh it with `make flux-crds`, which fetches the manifest,
// verifies fluxCRDSourceSHA before it reads a byte of it, rewrites the
// fixtures and prints this block for pasting. See hack/flux-crds.sh for the
// procedure and for why the newest patch of the pinned minor is the right
// choice.
const (
	fluxCRDVersion   = "v2.9.4"
	fluxCRDSourceURL = "https://github.com/fluxcd/flux2/releases/download/v2.9.4/install.yaml"
	fluxCRDSourceSHA = "9eb86c5f9d606b2ac2cfe71223ab2f23faa2d59ccb21df4e08e5610e54d535f8"
)

// fluxCRDDigests is every file the fixture directory may hold, and the sha256
// of the bytes hack/flux-crds.sh cut out of the manifest above.
var fluxCRDDigests = map[string]string{
	"kustomize.toolkit.fluxcd.io_kustomizations.yaml": "5d6514b855fc51a6b12dbe22e3170b8c50cc13aa6909a2f1a3c225c38955ecc1",
	"source.toolkit.fluxcd.io_ocirepositories.yaml":   "acb786924b1c358010b55a4608fcb0328d9515364fa56fbcb30af6354102e010",
}

// TestFluxCRDFixturesAreTheBytesTheScriptWrote is the "cannot be hand-edited"
// half of ADR-0030 decision 2's argument, enforced instead of asserted.
//
// The directory listing is checked as well as the digests, in both directions.
// A missing file is an envtest API server quietly serving one kind instead of
// two; an *extra* file is worse, because envtest installs every CRD it finds in
// the directory, so a stray YAML would put a schema into the suite that no pin
// covers and no review saw.
func TestFluxCRDFixturesAreTheBytesTheScriptWrote(t *testing.T) {
	entries, err := os.ReadDir(fluxCRDDir)
	if err != nil {
		t.Fatalf("reading %s: %v", fluxCRDDir, err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Errorf("%s holds a subdirectory %q; envtest reads this directory flat", fluxCRDDir, entry.Name())
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".yaml" {
			// envtest only reads .json/.yaml/.yml, so anything else is
			// documentation and harmless. README.md is one.
			continue
		}
		want, ok := fluxCRDDigests[name]
		if !ok {
			t.Errorf("%s/%s is not in fluxCRDDigests: envtest will install it and nothing pins where it came from. "+
				"Add it to hack/flux-crds.sh's CRDS list or delete it.", fluxCRDDir, name)
			continue
		}
		seen[name] = true
		got, err := digestOf(filepath.Join(fluxCRDDir, name))
		if err != nil {
			t.Fatalf("hashing %s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s/%s hashes to %s, want %s — the fixture was edited by hand, or the pin was bumped "+
				"without running `make flux-crds`. These bytes are upstream's; edit the pin, never the file.",
				fluxCRDDir, name, got, want)
		}
	}
	for name := range fluxCRDDigests {
		if !seen[name] {
			t.Errorf("%s/%s is pinned but missing; run `make flux-crds`", fluxCRDDir, name)
		}
	}
}

// TestFluxCRDFixturesTrackThePinnedFluxDistribution is the drift story the
// fixture exists to have.
//
// A fixture that is merely *some* Flux release proves nothing useful: the
// suite would be validating kelson's objects against a schema no cluster
// kelson provisions actually serves. So the pinned version has to satisfy
// install.FluxDistributionVersion — the semver expression ADR-0030 puts in the
// FluxInstance — and bumping that expression without refreshing the fixtures
// fails here, in `go test ./...`, rather than as a puzzling envtest failure six
// months later.
func TestFluxCRDFixturesTrackThePinnedFluxDistribution(t *testing.T) {
	if !satisfies(fluxCRDVersion, install.FluxDistributionVersion) {
		t.Errorf("the CRD fixtures are cut from Flux %s, but install.FluxDistributionVersion is %q: "+
			"the envtest suite would be validating against a schema the Flux kelson installs does not serve. "+
			"Bump FLUX_VERSION in hack/flux-crds.sh to a %s release and run `make flux-crds`.",
			fluxCRDVersion, install.FluxDistributionVersion, install.FluxDistributionVersion)
	}
	// The same rule pins.go states for its own rows: a URL and a version that
	// disagree is a pin that documents one thing and installs another.
	if !strings.Contains(fluxCRDSourceURL, fluxCRDVersion) {
		t.Errorf("fluxCRDSourceURL %q does not contain the pinned version %q", fluxCRDSourceURL, fluxCRDVersion)
	}
	if len(fluxCRDSourceSHA) != 64 {
		t.Errorf("fluxCRDSourceSHA is %d characters; a sha256 is 64", len(fluxCRDSourceSHA))
	}
	// And the script has to agree with this file, because the script is what a
	// refresh runs and this file is what CI reads.
	script, err := os.ReadFile(filepath.Join("..", "..", "hack", "flux-crds.sh"))
	if err != nil {
		t.Fatalf("reading hack/flux-crds.sh: %v", err)
	}
	for _, want := range []string{
		`FLUX_VERSION="` + fluxCRDVersion + `"`,
		`INSTALL_SHA256="` + fluxCRDSourceSHA + `"`,
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("hack/flux-crds.sh does not contain %s; the script and the pin here have parted company", want)
		}
	}
}

// TestFluxCRDFixturesDeclareWhatTheControllerWrites closes the last gap between
// the fixture and the code.
//
// fluxobjects.go applies a GVK built from internal/delivery/flux's GVRs, and an
// envtest API server serving a *different* version of the same kind would fail
// those applies in a way that looks like a controller bug. Asserting the
// fixtures declare exactly the coordinates this package writes — served, and
// with the status subresource the observation step reads back through — turns
// a Flux API bump into one legible failure here instead.
func TestFluxCRDFixturesDeclareWhatTheControllerWrites(t *testing.T) {
	cases := []struct {
		file    string
		group   string
		version string
		kind    string
		plural  string
	}{
		{
			file:    "kustomize.toolkit.fluxcd.io_kustomizations.yaml",
			group:   flux.KustomizationGVR.Group,
			version: flux.KustomizationGVR.Version,
			kind:    flux.KindKustomization,
			plural:  flux.KustomizationGVR.Resource,
		},
		{
			file:    "source.toolkit.fluxcd.io_ocirepositories.yaml",
			group:   flux.OCIRepositoryGVR.Group,
			version: flux.OCIRepositoryGVR.Version,
			kind:    flux.KindOCIRepository,
			plural:  flux.OCIRepositoryGVR.Resource,
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			crd := readCRDFixture(t, tc.file)
			if crd.Kind != "CustomResourceDefinition" {
				t.Fatalf("%s is a %q, not a CustomResourceDefinition", tc.file, crd.Kind)
			}
			if crd.Spec.Group != tc.group {
				t.Errorf("group is %q, want %q", crd.Spec.Group, tc.group)
			}
			if crd.Spec.Names.Kind != tc.kind {
				t.Errorf("kind is %q, want %q", crd.Spec.Names.Kind, tc.kind)
			}
			if crd.Spec.Names.Plural != tc.plural {
				t.Errorf("plural is %q, want %q", crd.Spec.Names.Plural, tc.plural)
			}
			var found bool
			for _, v := range crd.Spec.Versions {
				if v.Name != tc.version {
					continue
				}
				found = true
				if !v.Served {
					t.Errorf("%s is declared but not served; every apply this package makes would 404", tc.version)
				}
				if v.Subresources == nil || v.Subresources.Status == nil {
					t.Errorf("%s has no status subresource, and step 6 reads the Kustomization's conditions "+
						"back through it (delivery.go's observe)", tc.version)
				}
			}
			if !found {
				t.Errorf("%s serves no version %q, which is the one fluxobjects.go applies", tc.file, tc.version)
			}
		})
	}
}

// crdFixture is the handful of fields the assertions above need. It is a local
// struct rather than apiextensions-apiserver's own type on purpose: pulling a
// whole API-server module into go.mod as a direct dependency to read four
// fields is the dependency-weight trade ADR-0022 and ADR-0030 both refuse, and
// sigs.k8s.io/yaml is already here.
type crdFixture struct {
	Kind string `json:"kind"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind   string `json:"kind"`
			Plural string `json:"plural"`
		} `json:"names"`
		Versions []struct {
			Name         string `json:"name"`
			Served       bool   `json:"served"`
			Storage      bool   `json:"storage"`
			Subresources *struct {
				Status *struct{} `json:"status"`
			} `json:"subresources"`
		} `json:"versions"`
	} `json:"spec"`
}

func readCRDFixture(t *testing.T, file string) crdFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fluxCRDDir, file))
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var crd crdFixture
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	return crd
}

func digestOf(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// satisfies reports whether an exact release satisfies one of the minor-pinned
// expressions pins.go writes into a FluxInstance — "2.9.x" and nothing more
// elaborate. A general semver range parser would be a dependency for one
// comparison; what this has to answer is only "is this release inside the minor
// the product pins", and an `x` component matches anything.
func satisfies(version, expression string) bool {
	got := strings.Split(strings.TrimPrefix(version, "v"), ".")
	want := strings.Split(strings.TrimPrefix(expression, "v"), ".")
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if want[i] == "x" || want[i] == "*" {
			continue
		}
		if want[i] != got[i] {
			return false
		}
	}
	return true
}
