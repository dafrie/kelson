package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/dafrie/kelson/internal/delivery"
)

// The media types source-controller expects of an artifact it will hand to
// kustomize-controller. They are flux's, verified against fluxcd/pkg/oci, and
// they are written here rather than imported for the reason ADR-0017 states
// about the whole flux-operator API: kelson consumes these projects' contracts
// as strings and depends on none of their modules.
const (
	// ConfigMediaType marks the artifact's config blob. It is what tells a
	// reader this is a Flux artifact and not an image, which is why an
	// OCIRepository can consume it and a container runtime cannot.
	ConfigMediaType = "application/vnd.cncf.flux.config.v1+json"
	// LayerMediaType marks the single content layer: a gzipped tar of the
	// manifests, which source-controller extracts and kustomize-controller
	// builds from.
	LayerMediaType = "application/vnd.cncf.flux.content.v1.tar+gzip"
	// ManifestMediaType is the OCI image manifest schema the artifact itself is
	// described by. Registries that reject unknown artifact types still accept
	// this, which is why flux uses it rather than the newer artifactType field.
	ManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
)

// The OCI annotations flux's own tooling reads back off an artifact. Setting
// them means `flux pull artifact` and the OCIRepository's status describe a
// revision in the terms someone would ask about: which revision, which source.
const (
	AnnRevision = "org.opencontainers.image.revision"
	AnnSource   = "org.opencontainers.image.source"
	AnnCreated  = "org.opencontainers.image.created"
)

// The provenance annotations kelson adds, keyed like the labels it stamps on
// every rendered resource, so an artifact in a registry answers the same
// "whose is this?" question a resource in a cluster does.
const (
	AnnProject     = "kelson.dev/project"
	AnnEnvironment = "kelson.dev/environment"
)

// artifactEpoch is the timestamp every artifact carries, in the tar headers and
// in the config blob's created field.
//
// A wall clock here would make the digest of an unchanged render change on
// every publish: re-running a reconcile would push a new artifact for a spec
// whose manifests are byte-identical, and "the same render produces the same
// artifact" would stop being checkable. The build time is not lost by fixing
// it — the revision annotation names what was published, and the registry
// records when the push happened. What is gained is that a republish is a
// no-op the registry already has.
var artifactEpoch = time.Unix(0, 0).UTC()

// Contents is everything a caller has to decide before an artifact exists:
// where it goes, under which tag, what is in it and what it says about itself.
//
// It is the whole of the seam between this package and its two callers
// (ADR-0028 decision 2). A preview publish knows about change requests and head
// commits; a spine reconcile knows about generations and spec hashes; neither
// vocabulary appears here, because both arrive as [Contents.Annotations].
type Contents struct {
	// Repository is the push target without a scheme or a tag,
	// e.g. "ghcr.io/acme/checkout-previews".
	Repository string
	// Tag is the immutable name this artifact is published under.
	Tag string
	// Files are the artifact's contents, in the order they are written into
	// the tar. The order is the caller's — for a rendered set it is apply
	// order — and it is part of the digest, so it must be deterministic.
	Files []File
	// Annotations are written onto the OCI manifest verbatim, except that
	// [AnnCreated] is always set by [Package] itself. See the package doc.
	Annotations map[string]string
}

// Artifact is a packaged set: the two blobs, the manifest that describes them,
// and where it goes. It is produced offline by [Package] and consumed by
// [Pusher.Push] — packaging never touches the network and pushing never
// re-packages, so a `--dry-run` rung can hold a real artifact in memory and
// print its digest without a credential.
type Artifact struct {
	// Repository is the push target without a scheme or a tag,
	// e.g. "ghcr.io/acme/checkout-previews".
	Repository string
	// Tag is the name the artifact is published under.
	Tag string

	// Manifest is the OCI image manifest JSON, and Digest its sha256 — the
	// digest the registry will store this artifact under and the one an
	// OCIRepository reports once it has fetched it.
	Manifest []byte
	Digest   string

	// Config and Layer are the two blobs the manifest references, with their
	// digests.
	Config       []byte
	ConfigDigest string
	Layer        []byte
	LayerDigest  string

	// Files is the artifact's content listing, in the order it was written into
	// the tar: the paths a Kustomization will build.
	Files []string
}

// Reference is the artifact's canonical, digest-pinned address. It is what a
// publish prints, for the same reason `kelson build` prints a pinned image: a
// tag says where it was put, a digest says what was put there.
func (a Artifact) Reference() string { return a.Repository + "@" + a.Digest }

// Package turns a set of files into a Flux OCI artifact.
//
// The layout is the caller's — [ManifestFiles] is the one every kelson caller
// uses, the flat directory ADR-0017 decision 10 specifies. The Kustomizations
// kelson writes build `path: ./`, so the files sit at the root of the tar with
// no directory above them.
func Package(c Contents) (Artifact, error) {
	if len(c.Files) == 0 {
		return Artifact{}, fmt.Errorf("artifact: there is nothing to publish: the set holds no files")
	}

	layer, err := tarball(c.Files)
	if err != nil {
		return Artifact{}, err
	}

	// The caller's annotations first, then the created stamp, which this
	// package owns: a caller that supplied a wall clock would break the
	// determinism every other line here protects.
	annotations := make(map[string]string, len(c.Annotations)+1)
	maps.Copy(annotations, c.Annotations)
	annotations[AnnCreated] = artifactEpoch.Format(time.RFC3339)

	config, err := marshal(configBlob{
		Created:      artifactEpoch.Format(time.RFC3339),
		Architecture: "",
		OS:           "",
		RootFS:       rootFS{Type: "layers", DiffIDs: []string{}},
	})
	if err != nil {
		return Artifact{}, err
	}

	a := Artifact{
		Repository:   c.Repository,
		Tag:          c.Tag,
		Config:       config,
		ConfigDigest: digestOf(config),
		Layer:        layer,
		LayerDigest:  digestOf(layer),
	}
	for _, f := range c.Files {
		a.Files = append(a.Files, f.Path)
	}

	manifest, err := marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     ManifestMediaType,
		Config: descriptor{
			MediaType: ConfigMediaType,
			Digest:    a.ConfigDigest,
			Size:      int64(len(a.Config)),
		},
		Layers: []descriptor{{
			MediaType: LayerMediaType,
			Digest:    a.LayerDigest,
			Size:      int64(len(a.Layer)),
		}},
		Annotations: annotations,
	})
	if err != nil {
		return Artifact{}, err
	}
	a.Manifest = manifest
	a.Digest = digestOf(manifest)
	return a, nil
}

// tarball writes the files as a gzipped tar, deterministically.
//
// Everything that could vary between two runs of the same render is pinned:
// order comes from the caller (render order, which is apply order), the mode is
// a constant, ownership is root:root with no names, and every timestamp is
// [artifactEpoch] — including gzip's own header, which is why the writer is
// given an explicit empty header rather than the default. USTAR is chosen over
// PAX so no extended header can carry a field this function did not set.
func tarball(files []File) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("artifact: packaging: %w", err)
	}
	gz.Header = gzip.Header{ModTime: artifactEpoch}
	tw := tar.NewWriter(gz)
	// A file entry under a directory is preceded by an entry for the directory
	// itself. Nothing in the tar format requires that, and an extractor is not
	// required to invent the parent of a regular file either — so the artifact
	// says it rather than relying on whichever untar the consumer runs
	// (source-controller's, `flux pull artifact`'s, a reviewer's `tar -x`).
	// Order is the caller's, so this stays deterministic.
	dirs := map[string]bool{}
	for _, f := range files {
		dir, _, cut := strings.Cut(f.Path, "/")
		if !cut || dirs[dir] {
			continue
		}
		dirs[dir] = true
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     dir + "/",
			Mode:     0o755,
			ModTime:  artifactEpoch,
			Format:   tar.FormatUSTAR,
		}); err != nil {
			return nil, fmt.Errorf("artifact: packaging %s/: %w", dir, err)
		}
	}
	for _, f := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.Path,
			Mode:     0o644,
			Size:     int64(len(f.Data)),
			ModTime:  artifactEpoch,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("artifact: packaging %s: %w", f.Path, err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, fmt.Errorf("artifact: packaging %s: %w", f.Path, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("artifact: packaging: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("artifact: compressing: %w", err)
	}
	return buf.Bytes(), nil
}

// --- the OCI documents ------------------------------------------------------

// ociManifest is the image manifest as the registry stores it. Field order is
// the digest, so these structs are the schema and nothing here may be
// reordered casually.
type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// configBlob is the artifact's config document. Nothing reads it —
// source-controller cares about the layer — but it is a blob the manifest
// references by digest, so it has to exist and it has to be stable. It is
// shaped like the image config flux's own client writes, minus the fields that
// would describe a runnable image, because this is not one.
type configBlob struct {
	Created      string `json:"created"`
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	RootFS       rootFS `json:"rootfs"`
}

type rootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

// marshal encodes without HTML escaping and without a trailing newline, so the
// bytes that are digested are the bytes that are uploaded.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("artifact: encoding: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// digestOf is the registry's content address for a blob.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// File is one file in a published artifact: its path inside the tar and its
// bytes.
//
// It and [ManifestFiles] arrived here from the deleted git writer
// (internal/delivery/git) by way of internal/preview, which is where the layout
// was first written and which ADR-0028 decision 9 keeps by name while deleting
// everything around it. The layout is the artifact's, not the transport's:
// ADR-0017 decision 10 specifies a flat directory of rendered manifests, and
// the spine and the preview pipeline publish the same one (ADR-0028 decision 2,
// "one publisher rather than two").
type File struct {
	// Path is the file's name inside the artifact, relative to its root.
	Path string
	// Data is the file's contents, byte for byte as the renderer produced them.
	Data []byte
}

// ManifestFiles lays a rendered ManifestSet out as files. It is a pure function
// of the set: the same manifests always produce the same file names and
// contents, which is what makes an unchanged render produce a digest the
// registry already holds.
//
// The name carries the renderer's apply order as a zero-padded prefix. Flux does
// not need that ordering — kustomize-controller sorts resources itself — but it
// makes the directory readable when someone pulls an artifact to see what was
// deployed, and it keeps the listing stable when a resource is added in the
// middle.
//
// This is packaging, not rendering: the manifest bytes are passed through
// verbatim (ADR-0001).
//
// # The release stage (issue #227)
//
// A set that carries a release hook is laid out in two pieces instead of one,
// because the two are applied by two Kustomizations with a `dependsOn` between
// them (ADR-0028 decision 3, internal/controller/fluxobjects.go):
//
//	kustomization.yaml            the workload stage, listed explicitly
//	001-namespace-….yaml          … and its files, at the root, unmoved
//	release/001-….yaml            the release stage
//
// The workload stage stays at the root under the same names it has always had,
// so the Kustomization that applies it keeps `path: ./` and a rollback to an
// artifact published before any of this existed still resolves. What makes the
// root exclude `release/` is the generated `kustomization.yaml`: without one,
// kustomize-controller synthesises a kustomization from *everything it finds
// beneath the path*, subdirectories included, and the Job would be applied
// beside the Deployments it is supposed to gate — the precise failure the split
// exists to prevent, arriving quietly through the layout.
//
// A set with no release hook is laid out exactly as it was before this existed:
// a flat directory, no generated kustomization.yaml, byte-identical output for
// byte-identical input. The split must not tax a set that does not use it.
func ManifestFiles(set delivery.ManifestSet) []File {
	staged := false
	for _, m := range set.Manifests {
		if m.Stage == delivery.StageRelease {
			staged = true
			break
		}
	}
	if !staged {
		return manifestFiles(set.Manifests, "")
	}

	var workload, release []delivery.Manifest
	for _, m := range set.Manifests {
		if m.Stage != delivery.StageRelease {
			workload = append(workload, m)
		}
		if m.Stage != delivery.StageWorkload {
			release = append(release, m)
		}
	}
	files := manifestFiles(workload, "")
	files = append(files, kustomizationFile("", files))
	files = append(files, manifestFiles(release, delivery.ReleaseStageDir+"/")...)
	return files
}

// manifestFiles is the flat layout, optionally under a directory. Numbering
// restarts per directory, which is what keeps the workload stage's file names
// identical to the ones an unstaged set of the same manifests produces.
func manifestFiles(manifests []delivery.Manifest, dir string) []File {
	files := make([]File, 0, len(manifests))
	seen := map[string]int{}
	for i, m := range manifests {
		name := fmt.Sprintf("%03d-%s-%s.yaml", i+1, slugify(m.Kind), slugify(m.Name))
		if n := seen[name]; n > 0 {
			// Same kind+name in two namespaces: keep both, deterministically.
			name = fmt.Sprintf("%03d-%s-%s-%s.yaml", i+1, slugify(m.Kind), slugify(m.Namespace), slugify(m.Name))
		}
		seen[name]++
		files = append(files, File{Path: dir + name, Data: m.YAML})
	}
	return files
}

// kustomizationFile writes the explicit resource list that stops
// kustomize-controller from walking into the release directory. It is built by
// hand rather than through a YAML encoder because it is four lines of fixed
// shape and because sigs.k8s.io/kustomize is not a dependency this repository
// carries — and because the bytes have to be deterministic to the digest, which
// a literal is and a marshaller is only by convention.
func kustomizationFile(dir string, files []File) File {
	var b strings.Builder
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\n")
	b.WriteString("kind: Kustomization\n")
	b.WriteString("resources:\n")
	for _, f := range files {
		b.WriteString("  - " + strings.TrimPrefix(f.Path, dir) + "\n")
	}
	return File{Path: dir + "kustomization.yaml", Data: []byte(b.String())}
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	out := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "resource"
	}
	return out
}
