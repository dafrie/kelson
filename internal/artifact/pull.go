package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

// The other direction: an artifact this package published, read back out of the
// registry as the file set it was packaged from.
//
// # Why this exists
//
// ADR-0028 decision 4 makes the registry the record — `status.history[]` is a
// bounded mirror of it — and two questions can only be answered by the bytes
// themselves: what would change relative to revision N (issue #247,
// RenderService.Diff with `from_revision`), and what a rollback to revision N
// actually changes (ADR-0028 decision 5's preview). Both used to be served by
// the rendered-history store ADR-0027 decision 7 deleted; neither can be served
// by re-rendering a stored spec, because that answers a different question —
// what that spec produces under *today's* renderer and ClusterProfile, which is
// not what was applied.
//
// # Why it lives on [Pusher] rather than in a client of its own
//
// The same argument tags.go makes for [Pusher.Tags] and [Pusher.Resolve]: it is
// the same conversation with the same registry, under the same credential,
// through the same 401-challenge-and-token dance, and a second type would be a
// second credential story to configure and a second place to remember that a
// token is scoped. Reads ask for the `pull` scope, and the token cache is keyed
// by scope precisely so a pull and a push in one process do not hand each other
// a token that authorises the wrong half.
//
// # Nothing here trusts the registry
//
// Every byte is checked against the digest that named it: the manifest against
// what the reference resolved to (and against the digest the caller recorded,
// when it has one), and each blob against its descriptor. A mismatch is
// [IntegrityError] and it is fatal — never a warning, never a fallback to the
// bytes that arrived. An artifact is immutable by construction (ADR-0028
// decision 2 writes a tag once and never rewrites it), so a digest that does
// not match is either a corrupted transfer or a registry serving something
// other than what kelson published, and both of those are answers no diff may
// be computed on top of.

// MaxArtifactBytes bounds one pulled artifact: the compressed layer as it
// arrives, and the extracted set as it is unpacked.
//
// A rendered manifest set is kilobytes; the bound is three orders of magnitude
// above that so it is never met by a real environment, and it exists because a
// gzip stream is otherwise an unbounded allocation in a server that fetches one
// on request. Extraction stops at the bound rather than truncating, for the
// reason [Pusher.Tags] refuses a partial tag list: a set that is missing files
// is a diff that reports deletions nobody made.
const MaxArtifactBytes = 32 << 20

// PullRequest names one artifact to read back.
type PullRequest struct {
	// Repository is the source, without a scheme or a tag,
	// e.g. "ghcr.io/acme/kelson/shop-production".
	Repository string
	// Reference is the tag or the `sha256:…` digest to fetch. A digest is
	// checked against the bytes that arrive like any other expectation.
	Reference string
	// Digest is the manifest digest the caller already recorded for this
	// reference — `status.history[].digest` for a revision, the digest a
	// previous [Pusher.Resolve] returned. Empty means the caller has none, and
	// the pull is then verified against the registry's own answer alone (see
	// [Pulled.Digest]). A non-empty one that disagrees with the bytes is an
	// [IntegrityError].
	Digest string
}

// Pulled is one artifact read back: the manifest that described it, what it
// said about itself, and the files it carried.
type Pulled struct {
	// Repository and Reference are what was asked for.
	Repository string
	Reference  string
	// Digest is the sha256 of the manifest as received — computed here, not
	// copied from a header — so it is the digest of the bytes this result was
	// actually built from.
	Digest string
	// Annotations are the OCI manifest's, as [Package] wrote them: the revision,
	// the source and kelson's own provenance keys.
	Annotations map[string]string
	// Files are the artifact's contents in the order the tar holds them, which
	// for anything [Package] produced is the order the caller packaged them in
	// (render order, which is apply order).
	Files []File
}

// Reference is the pulled artifact's canonical, digest-pinned address — the
// same spelling [Artifact.Reference] prints for a push.
func (p Pulled) Address() string { return p.Repository + "@" + p.Digest }

// Pull fetches an artifact and returns it as the file set it was packaged from.
//
// It is the exact inverse of [Package]: the manifest names one
// [LayerMediaType] layer, the layer is a gzipped tar written with fixed modes
// and a fixed timestamp, and unpacking it yields the same [File] slice, in the
// same order, byte for byte. That round trip is what the diff against a
// recorded revision stands on, so it is asserted rather than assumed
// (pull_test.go).
//
// A reference the registry does not hold is [ReasonArtifactNotFound], which
// callers test with [NotFound] — it is an answer ("there is no such revision"),
// and it must stay distinguishable from a registry that refused to be read.
func (p *Pusher) Pull(ctx context.Context, req PullRequest) (Pulled, error) {
	repo, err := parseRepository(req.Repository)
	if err != nil {
		return Pulled{}, err
	}
	repo.actions = pullActions

	reference := strings.TrimSpace(req.Reference)
	if reference == "" {
		return Pulled{}, Error{
			Reason:      ReasonRepositoryInvalid,
			Message:     "no tag or digest to pull from " + repo.host + "/" + repo.path,
			Remediation: "name the revision to read, e.g. 7-a1b2c3d4",
		}
	}

	body, served, err := p.fetchManifest(ctx, repo, reference)
	if err != nil {
		return Pulled{}, err
	}
	digest := digestOf(body)
	where := repo.host + "/" + repo.path + ":" + reference
	// Three claims about what this reference names, from three parties: the
	// registry's own header, the caller's record, and — when the reference is
	// itself a digest — the request. Each is checked against the bytes, and any
	// one of them disagreeing ends the pull.
	for _, claim := range []struct{ source, want string }{
		{"the registry's Docker-Content-Digest", strings.TrimSpace(served)},
		{"the digest kelson recorded for this revision", strings.TrimSpace(req.Digest)},
		{"the digest this pull asked for", digestReference(reference)},
	} {
		if claim.want != "" && claim.want != digest {
			return Pulled{}, &IntegrityError{
				Doing:  "pulling " + where + ", where " + claim.source + " does not describe the bytes it served",
				Want:   claim.want,
				Got:    digest,
				Source: claim.source,
			}
		}
	}

	var doc ociManifest
	if err := json.Unmarshal(body, &doc); err != nil {
		return Pulled{}, Error{
			Reason:      ReasonArtifactUnreadable,
			Message:     where + " served a manifest kelson could not read: " + err.Error(),
			Remediation: "something other than a registry is answering /v2/<name>/manifests/<reference>; check what sits in front of it",
		}
	}
	if doc.Config.MediaType != ConfigMediaType {
		return Pulled{}, Error{
			Reason: ReasonArtifactUnreadable,
			Message: fmt.Sprintf("%s is not a flux artifact: its config is %s, not %s",
				where, quoted(doc.Config.MediaType), quoted(ConfigMediaType)),
			Remediation: "this repository holds something else — an image, or another tool's artifact; " +
				"kelson reads only the artifacts it published (ADR-0028 decision 2)",
		}
	}

	pulled := Pulled{
		Repository:  req.Repository,
		Reference:   reference,
		Digest:      digest,
		Annotations: doc.Annotations,
	}
	for i, layer := range doc.Layers {
		if layer.MediaType != LayerMediaType {
			// Skipping it would hand back a file set that is missing whatever
			// the layer held, and a diff computed on a partial set reports
			// deletions nobody made.
			return Pulled{}, Error{
				Reason: ReasonArtifactUnreadable,
				Message: fmt.Sprintf("%s carries a layer kelson cannot read: layer %d is %s, not %s",
					where, i, quoted(layer.MediaType), quoted(LayerMediaType)),
				Remediation: "kelson refuses a partial extraction rather than presenting one as the whole set",
			}
		}
		blob, err := p.fetchBlob(ctx, repo, layer, where)
		if err != nil {
			return Pulled{}, err
		}
		files, err := untar(blob, where)
		if err != nil {
			return Pulled{}, err
		}
		pulled.Files = append(pulled.Files, files...)
	}
	if len(pulled.Files) == 0 {
		return Pulled{}, Error{
			Reason:      ReasonArtifactUnreadable,
			Message:     where + " holds no files: its layers are empty",
			Remediation: "kelson never publishes an empty set (Package refuses one), so this artifact was written by something else",
		}
	}
	return pulled, nil
}

// fetchManifest GETs one manifest and returns its bytes together with the
// digest the registry says the reference resolves to.
func (p *Pusher) fetchManifest(ctx context.Context, repo target, reference string) (body []byte, served string, err error) {
	res, err := p.do(ctx, repo, http.MethodGet, p.endpoint(repo, "/manifests/"+reference), nil, "")
	if err != nil {
		return nil, "", err
	}
	defer closeBody(res)
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, "", Error{
			Reason:      ReasonArtifactNotFound,
			Message:     repo.host + "/" + repo.path + " holds no " + quoted(reference),
			Remediation: "list what it does hold with `kelson history`, or with the registry's own client (`crane ls`)",
		}
	case res.StatusCode != http.StatusOK:
		return nil, "", responseError(res, "fetching the manifest of "+repo.host+"/"+repo.path+":"+reference)
	}
	body, err = readBounded(res.Body, manifestLimit)
	if err != nil {
		return nil, "", Error{
			Reason:      ReasonArtifactUnreadable,
			Message:     repo.host + "/" + repo.path + ":" + reference + ": " + err.Error(),
			Remediation: "an OCI manifest is a small JSON document; something else is being served here",
		}
	}
	return body, res.Header.Get("Docker-Content-Digest"), nil
}

// fetchBlob GETs one blob and verifies it against the descriptor that named it,
// which is the half of the chain the manifest's own digest does not cover: a
// verified manifest proves which blobs belong to this artifact, and this proves
// the registry served those blobs.
func (p *Pusher) fetchBlob(ctx context.Context, repo target, d descriptor, where string) ([]byte, error) {
	res, err := p.do(ctx, repo, http.MethodGet, p.endpoint(repo, "/blobs/"+d.Digest), nil, "")
	if err != nil {
		return nil, err
	}
	defer closeBody(res)
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, Error{
			Reason:  ReasonArtifactNotFound,
			Message: where + " references a blob the registry does not hold (" + d.Digest + ")",
			Remediation: "the manifest and its blobs have come apart, which a registry garbage collection can do to " +
				"an artifact nothing referenced; republish the revision or roll back to one that is whole",
		}
	case res.StatusCode != http.StatusOK:
		return nil, responseError(res, "fetching a layer of "+where)
	}
	body, err := readBounded(res.Body, MaxArtifactBytes)
	if err != nil {
		return nil, Error{
			Reason:      ReasonArtifactUnreadable,
			Message:     where + ": reading layer " + d.Digest + ": " + err.Error(),
			Remediation: fmt.Sprintf("kelson reads at most %d bytes of one artifact layer", MaxArtifactBytes),
		}
	}
	if got := digestOf(body); got != d.Digest {
		return nil, &IntegrityError{
			Doing:  "pulling " + where + ", whose manifest names a layer the registry served different bytes for",
			Want:   d.Digest,
			Got:    got,
			Source: "the layer descriptor in the manifest",
		}
	}
	// The digest already proves these are the right bytes, so a size that
	// disagrees is the manifest contradicting itself rather than a bad
	// transfer. It is still refused: something wrote a descriptor that does not
	// describe its blob, and nothing downstream should be built on it.
	if d.Size > 0 && int64(len(body)) != d.Size {
		return nil, Error{
			Reason: ReasonArtifactUnreadable,
			Message: fmt.Sprintf("%s declares a %d-byte layer and its blob is %d bytes",
				where, d.Size, len(body)),
			Remediation: "the manifest does not describe its own content; kelson will not diff against it",
		}
	}
	return body, nil
}

// untar inverts [tarball]: the same files, in the same order, with the same
// bytes. Everything that function fixed is read back and nothing is inferred —
// modes and timestamps are not returned at all, because they were constants
// chosen to make the digest stable rather than facts about the files.
//
// What it refuses is anything the packaging never wrote. A tar entry that is
// not a regular file, or whose name escapes the artifact root, cannot have come
// from [Package]; kelson reads these bytes to compare them, never to write them
// to a filesystem, but a path that says "../../etc/passwd" is still a signal
// that this artifact is not one of kelson's and the honest answer is to stop.
func untar(layer []byte, where string) ([]File, error) {
	refuse := func(format string, a ...any) error {
		return Error{
			Reason:      ReasonArtifactUnreadable,
			Message:     where + ": " + fmt.Sprintf(format, a...),
			Remediation: "kelson reads the flat directory of rendered manifests it publishes (ADR-0017 decision 10) and nothing else",
		}
	}

	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		return nil, refuse("the layer is not a gzip stream: %s", err)
	}
	defer func() { _ = gz.Close() }()

	var (
		files  []File
		budget = int64(MaxArtifactBytes)
	)
	tr := tar.NewReader(io.LimitReader(gz, MaxArtifactBytes+1))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, refuse("reading the layer's tar: %s", err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
			// Directory entries and pax headers carry no content of their own.
			// Package writes neither, but a tar another tool produced may, and
			// dropping them loses nothing.
			continue
		default:
			return nil, refuse("entry %s is not a regular file (type %q)", quoted(hdr.Name), hdr.Typeflag)
		}
		name := path.Clean(hdr.Name)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || name == "." {
			return nil, refuse("entry %s does not name a file inside the artifact", quoted(hdr.Name))
		}
		if hdr.Size > budget {
			return nil, refuse("the extracted set is larger than the %d bytes kelson reads", MaxArtifactBytes)
		}
		data, err := readBounded(tr, budget)
		if err != nil {
			return nil, refuse("reading %s: %s", quoted(hdr.Name), err)
		}
		budget -= int64(len(data))
		files = append(files, File{Path: name, Data: data})
	}
	return files, nil
}

// readBounded reads at most limit bytes and reports anything longer as an
// error rather than returning a truncated prefix, which is the whole point:
// half a manifest that parses is worse than a refusal.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("more than %d bytes, which is more than kelson reads here", limit)
	}
	return body, nil
}

// manifestLimit bounds an OCI manifest. It is generous for a document naming
// two blobs and small enough that an endpoint answering with a web page is an
// error rather than a memory problem.
const manifestLimit = 1 << 20

// digestReference reports the digest a reference pins, or empty for a tag.
func digestReference(reference string) string {
	if strings.HasPrefix(reference, "sha256:") {
		return reference
	}
	return ""
}
