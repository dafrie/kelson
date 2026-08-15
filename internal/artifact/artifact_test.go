package artifact_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/delivery"
)

// The artifact is what source-controller fetches and kustomize-controller
// builds, so these tests pin the contract with those two: the media types they
// dispatch on, the layout they extract, and the annotations flux's own tooling
// reads back. They also pin determinism, which is kelson's own claim rather
// than flux's, and which is the whole reason two callers may share one
// publisher (ADR-0028 decision 2).

const testTag = "7-1a2b3c4d"

const testRepository = "ghcr.io/acme/kelson/checkout-production"

// testSet is a rendered manifest set in the shape both callers hand over: a
// Namespace first, because a set is applied in the order it is written, then
// the workloads that target it.
func testSet() delivery.ManifestSet {
	return delivery.ManifestSet{
		Project:     "checkout",
		Environment: "production",
		Manifests: []delivery.Manifest{
			{APIVersion: "v1", Kind: "Namespace", Name: "checkout-production",
				YAML: []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: checkout-production\n")},
			{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Namespace: "checkout-production",
				YAML: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n")},
			{APIVersion: "v1", Kind: "Service", Name: "web", Namespace: "checkout-production",
				YAML: []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n")},
		},
	}
}

func testContents() artifact.Contents {
	return artifact.Contents{
		Repository: testRepository,
		Tag:        testTag,
		Files:      artifact.ManifestFiles(testSet()),
		Annotations: map[string]string{
			artifact.AnnRevision:    testTag,
			artifact.AnnSource:      "https://github.com/acme/checkout",
			artifact.AnnProject:     "checkout",
			artifact.AnnEnvironment: "production",
		},
	}
}

func mustPackage(t *testing.T, c artifact.Contents) artifact.Artifact {
	t.Helper()
	a, err := artifact.Package(c)
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	return a
}

type manifestDoc struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
	Annotations map[string]string `json:"annotations"`
}

func decodeManifest(t *testing.T, a artifact.Artifact) manifestDoc {
	t.Helper()
	var doc manifestDoc
	if err := json.Unmarshal(a.Manifest, &doc); err != nil {
		t.Fatalf("the artifact manifest is not JSON: %v", err)
	}
	return doc
}

// TestArtifactMediaTypes is the dispatch source-controller performs: a Flux
// config media type and a single gzipped-tar content layer, inside an OCI image
// manifest. Getting any of the three wrong produces an artifact a registry
// accepts and an OCIRepository rejects.
func TestArtifactMediaTypes(t *testing.T) {
	a := mustPackage(t, testContents())
	doc := decodeManifest(t, a)

	if doc.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2", doc.SchemaVersion)
	}
	if doc.MediaType != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("manifest mediaType = %q", doc.MediaType)
	}
	if doc.Config.MediaType != "application/vnd.cncf.flux.config.v1+json" {
		t.Errorf("config mediaType = %q, want flux's config type", doc.Config.MediaType)
	}
	if len(doc.Layers) != 1 {
		t.Fatalf("the artifact has %d layers, want exactly 1", len(doc.Layers))
	}
	if doc.Layers[0].MediaType != "application/vnd.cncf.flux.content.v1.tar+gzip" {
		t.Errorf("layer mediaType = %q, want flux's content type", doc.Layers[0].MediaType)
	}

	// The descriptors must address the blobs actually uploaded, or the registry
	// stores a manifest pointing at nothing.
	if doc.Config.Digest != a.ConfigDigest || doc.Config.Size != int64(len(a.Config)) {
		t.Errorf("the config descriptor does not describe the config blob")
	}
	if doc.Layers[0].Digest != a.LayerDigest || doc.Layers[0].Size != int64(len(a.Layer)) {
		t.Errorf("the layer descriptor does not describe the layer blob")
	}
}

// TestArtifactCarriesTheCallersAnnotations: the annotations are the whole of
// what one caller knows and the other does not, so they are passed through
// verbatim.
func TestArtifactCarriesTheCallersAnnotations(t *testing.T) {
	doc := decodeManifest(t, mustPackage(t, testContents()))
	for key, want := range map[string]string{
		"org.opencontainers.image.revision": testTag,
		"org.opencontainers.image.source":   "https://github.com/acme/checkout",
		"kelson.dev/project":                "checkout",
		"kelson.dev/environment":            "production",
	} {
		if got := doc.Annotations[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestCreatedAnnotationIsAlwaysTheEpoch: a caller that supplied a wall clock
// would break the determinism every other line of the packaging protects, so
// the created stamp is not the caller's to set.
func TestCreatedAnnotationIsAlwaysTheEpoch(t *testing.T) {
	c := testContents()
	c.Annotations["org.opencontainers.image.created"] = "2026-08-14T12:00:00Z"
	doc := decodeManifest(t, mustPackage(t, c))
	if got := doc.Annotations["org.opencontainers.image.created"]; got != "1970-01-01T00:00:00Z" {
		t.Errorf("created = %q, want the fixed epoch: a caller must not be able to date an artifact", got)
	}
}

// TestArtifactLayoutIsTheRenderedSet: the Kustomizations kelson writes build
// `path: ./`, so the manifests sit at the tar's root in apply order.
func TestArtifactLayoutIsTheRenderedSet(t *testing.T) {
	set := testSet()
	a := mustPackage(t, testContents())
	entries := untar(t, a.Layer)

	if len(entries) != len(set.Manifests) {
		t.Fatalf("the artifact holds %d files for %d manifests", len(entries), len(set.Manifests))
	}
	for i, e := range entries {
		if strings.Contains(e.name, "/") {
			t.Errorf("%s is nested; the Kustomization builds the artifact root", e.name)
		}
		if !strings.HasSuffix(e.name, ".yaml") {
			t.Errorf("%s is not a manifest", e.name)
		}
		if e.name != a.Files[i] {
			t.Errorf("Files[%d] = %q but the tar holds %q", i, a.Files[i], e.name)
		}
	}
	// The Namespace leads, because a set is applied in the order it is written
	// and everything else in it targets that namespace.
	if !strings.Contains(entries[0].name, "namespace") {
		t.Errorf("the first file is %s; the Namespace must lead the set", entries[0].name)
	}
}

// TestManifestFilesKeepsTwoOfTheSameName: the same kind and name in two
// namespaces must both survive, and the apply-order prefix is what keeps them
// apart — which is also why the listing stays stable when a resource is added
// in the middle.
func TestManifestFilesKeepsTwoOfTheSameName(t *testing.T) {
	files := artifact.ManifestFiles(delivery.ManifestSet{Manifests: []delivery.Manifest{
		{Kind: "Secret", Name: "api", Namespace: "one", YAML: []byte("a")},
		{Kind: "Secret", Name: "api", Namespace: "two", YAML: []byte("b")},
	}})
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if files[0].Path == files[1].Path {
		t.Fatalf("two resources collided onto %q", files[0].Path)
	}
	if !strings.HasPrefix(files[0].Path, "001-") || !strings.HasPrefix(files[1].Path, "002-") {
		t.Errorf("the names do not carry apply order: %q, %q", files[0].Path, files[1].Path)
	}
}

// TestArtifactIsDeterministic is the property that makes a republish free and a
// digest meaningful: the same Contents packages to the same bytes, timestamps
// and ordering included. A wall clock anywhere in the packaging would fail this.
func TestArtifactIsDeterministic(t *testing.T) {
	first := mustPackage(t, testContents())
	second := mustPackage(t, testContents())

	if first.Digest != second.Digest {
		t.Errorf("two packagings of one input produced different digests: %s and %s", first.Digest, second.Digest)
	}
	if !bytes.Equal(first.Layer, second.Layer) {
		t.Error("the layer is not byte-identical between two packagings")
	}
	if !bytes.Equal(first.Config, second.Config) {
		t.Error("the config blob is not byte-identical between two packagings")
	}
	if !bytes.Equal(first.Manifest, second.Manifest) {
		t.Error("the OCI manifest is not byte-identical between two packagings")
	}

	// And different inputs must produce different artifacts, or the determinism
	// above would be indistinguishable from a constant. Both halves count: the
	// files, and the annotations that describe them.
	changed := testContents()
	changed.Files[0].Data = append(append([]byte(nil), changed.Files[0].Data...), []byte("# edited\n")...)
	if other := mustPackage(t, changed); other.Digest == first.Digest {
		t.Error("a changed manifest packaged to the same digest")
	}
	relabelled := testContents()
	relabelled.Annotations[artifact.AnnRevision] = "8-99887766"
	if other := mustPackage(t, relabelled); other.Digest == first.Digest {
		t.Error("a changed revision annotation packaged to the same digest")
	}
}

// TestArtifactTarHeadersCarryNoEnvironment: uid, gid and mtime are the three
// fields that would otherwise make the same render produce a different digest
// on a different machine.
func TestArtifactTarHeadersCarryNoEnvironment(t *testing.T) {
	a := mustPackage(t, testContents())
	for _, e := range untar(t, a.Layer) {
		if e.header.Uid != 0 || e.header.Gid != 0 {
			t.Errorf("%s is owned by %d:%d, want 0:0", e.name, e.header.Uid, e.header.Gid)
		}
		if e.header.Uname != "" || e.header.Gname != "" {
			t.Errorf("%s names an owner (%q/%q)", e.name, e.header.Uname, e.header.Gname)
		}
		if e.header.ModTime.Unix() != 0 {
			t.Errorf("%s is dated %s, want the fixed epoch", e.name, e.header.ModTime)
		}
		if e.header.Mode != 0o644 {
			t.Errorf("%s has mode %o, want 644", e.name, e.header.Mode)
		}
	}
}

// TestArtifactReferenceIsDigestPinned: a publish prints what was put there, not
// where it was put, for the same reason `kelson build` prints a pinned image.
func TestArtifactReferenceIsDigestPinned(t *testing.T) {
	a := mustPackage(t, testContents())
	want := testRepository + "@" + a.Digest
	if a.Reference() != want {
		t.Errorf("Reference() = %q, want %q", a.Reference(), want)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256 digest", a.Digest)
	}
}

// TestPackageRefusesAnEmptySet: publishing nothing would create a tag a
// Kustomization fetches and builds into zero resources, which prunes the
// environment. It is a refusal, not an empty artifact.
func TestPackageRefusesAnEmptySet(t *testing.T) {
	c := testContents()
	c.Files = nil
	if _, err := artifact.Package(c); err == nil {
		t.Fatal("an empty set was packaged")
	}
}

type tarEntry struct {
	name   string
	body   []byte
	header tar.Header
}

func untar(t *testing.T, layer []byte) []tarEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		t.Fatalf("the layer is not gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	var out []tarEntry
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading the layer: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		out = append(out, tarEntry{name: hdr.Name, body: body, header: *hdr})
	}
	return out
}
