package preview_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/preview"
)

// The artifact is what source-controller fetches and kustomize-controller
// builds, so these tests pin the contract with those two: the media types they
// dispatch on, the layout they extract, and the annotations flux's own tooling
// reads back. They also pin determinism, which is kelson's own claim rather
// than flux's.

func mustPackage(t *testing.T, set *preview.Set) preview.Artifact {
	t.Helper()
	a, err := preview.Package(set)
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

func decodeManifest(t *testing.T, a preview.Artifact) manifestDoc {
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
	a := mustPackage(t, mustRender(t, testOptions()))
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

// TestArtifactAnnotations: what a reviewer asks of an artifact in a registry is
// "which change request, which commit, whose is it", and the answers are these.
func TestArtifactAnnotations(t *testing.T) {
	set := mustRender(t, testOptions())
	doc := decodeManifest(t, mustPackage(t, set))

	revision := doc.Annotations["org.opencontainers.image.revision"]
	if !strings.Contains(revision, "412") || !strings.Contains(revision, testSHA) {
		t.Errorf("revision = %q, want it to name change request 412 and commit %s", revision, testSHA)
	}
	if got := doc.Annotations["org.opencontainers.image.source"]; got != "https://github.com/acme/checkout" {
		t.Errorf("source = %q, want the forge repository", got)
	}
	if got := doc.Annotations["kelson.dev/project"]; got != "checkout" {
		t.Errorf("kelson.dev/project = %q", got)
	}
	if got := doc.Annotations["kelson.dev/environment"]; got != "staging" {
		t.Errorf("kelson.dev/environment = %q", got)
	}
	if got := doc.Annotations["kelson.dev/preview"]; got != set.Namespace {
		t.Errorf("kelson.dev/preview = %q, want the preview namespace %q", got, set.Namespace)
	}
}

// TestArtifactLayoutIsTheRenderedSet: the Kustomization builds `path: ./`, so
// the manifests sit at the tar's root in apply order, laid out by the same
// function a Git-mode commit is laid out by (ADR-0017 asked stage 2 to reuse
// that vocabulary).
func TestArtifactLayoutIsTheRenderedSet(t *testing.T) {
	set := mustRender(t, testOptions())
	a := mustPackage(t, set)
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
	if !bytes.Contains(entries[0].body, []byte(set.Namespace)) {
		t.Errorf("the first file does not declare %s", set.Namespace)
	}
}

// TestArtifactIsDeterministic is the property that makes a republish free and a
// digest meaningful: the same render packages to the same bytes, timestamps and
// ordering included. A wall clock anywhere in the packaging would fail this.
func TestArtifactIsDeterministic(t *testing.T) {
	first := mustPackage(t, mustRender(t, testOptions()))
	second := mustPackage(t, mustRender(t, testOptions()))

	if first.Digest != second.Digest {
		t.Errorf("two packagings of one render produced different digests: %s and %s", first.Digest, second.Digest)
	}
	if !bytes.Equal(first.Layer, second.Layer) {
		t.Error("the layer is not byte-identical between two packagings")
	}
	if !bytes.Equal(first.Config, second.Config) {
		t.Error("the config blob is not byte-identical between two packagings")
	}

	// And a different change request must produce a different artifact, or the
	// determinism above would be indistinguishable from a constant.
	opts := testOptions()
	opts.PR = "413"
	if other := mustPackage(t, mustRender(t, opts)); other.Digest == first.Digest {
		t.Error("two different change requests packaged to the same digest")
	}
}

// TestArtifactTarHeadersCarryNoEnvironment: uid, gid and mtime are the three
// fields that would otherwise make the same render produce a different digest
// on a different machine.
func TestArtifactTarHeadersCarryNoEnvironment(t *testing.T) {
	a := mustPackage(t, mustRender(t, testOptions()))
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
	a := mustPackage(t, mustRender(t, testOptions()))
	want := "ghcr.io/acme/checkout-previews@" + a.Digest
	if a.Reference() != want {
		t.Errorf("Reference() = %q, want %q", a.Reference(), want)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256 digest", a.Digest)
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
