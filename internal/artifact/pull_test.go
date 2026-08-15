package artifact_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dafrie/kelson/internal/artifact"
)

// The pull half is tested against a registry that first accepted a real push,
// because the property worth pinning is not "an HTTP GET works" — it is that
// the two halves are inverses. Package fixes an order, a mode and a timestamp
// to make the digest stable; if extraction did not put exactly those files back
// in exactly that order, every diff against a recorded revision would report
// changes nobody made, and it would do so quietly.

// pullRegistry is enough of the distribution API for a push and then a pull:
// the two-step blob upload, the manifest PUT, and the GETs the pull half needs.
type pullRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	requests  []string
	scopes    []string

	// challenge makes the registry demand a bearer token, so a pull can be
	// shown to ask for the scope it needs.
	challenge bool
	// corrupt rewrites a served blob or manifest, which is how the integrity
	// checks are exercised without a second registry implementation.
	corruptBlob    func(digest string, body []byte) []byte
	servedDigest   func(tag, digest string) string
	corruptRequest func(tag string, body []byte) []byte
}

func newPullRegistry() *pullRegistry {
	return &pullRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
}

func (f *pullRegistry) handler(base *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		if r.URL.Path == "/token" {
			f.mu.Lock()
			f.scopes = append(f.scopes, r.URL.Query().Get("scope"))
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "a-bearer-token"})
			return
		}
		if f.challenge && r.Header.Get("Authorization") != "Bearer a-bearer-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+*base+`/token",service="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case strings.Contains(r.URL.Path, "/blobs/uploads/"):
			f.upload(w, r)
		case strings.Contains(r.URL.Path, "/blobs/"):
			f.blob(w, r)
		case strings.Contains(r.URL.Path, "/manifests/"):
			f.manifest(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *pullRegistry) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		w.Header().Set("Location", "/v2/acme/kelson/shop-production/blobs/uploads/session-1")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.blobs[r.URL.Query().Get("digest")] = body
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (f *pullRegistry) blob(w http.ResponseWriter, r *http.Request) {
	digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	body, ok := f.blobs[digest]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if f.corruptBlob != nil {
		body = f.corruptBlob(digest, body)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (f *pullRegistry) manifest(w http.ResponseWriter, r *http.Request) {
	tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if r.Method == http.MethodPut {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.manifests[tag] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		return
	}
	f.mu.Lock()
	body, ok := f.manifests[tag]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if f.corruptRequest != nil {
		body = f.corruptRequest(tag, body)
	}
	digest := sha256Of(body)
	if f.servedDigest != nil {
		digest = f.servedDigest(tag, digest)
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", artifact.ManifestMediaType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (f *pullRegistry) start(t *testing.T) string {
	t.Helper()
	var base string
	srv := httptest.NewServer(f.handler(&base))
	base = srv.URL
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://") + "/acme/kelson/shop-production"
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// publishedSet is the file set every test in this file round-trips. It is
// deliberately awkward where the format is: a file whose bytes are not valid
// UTF-8, one that is empty, one whose name would sort before its predecessor,
// and enough of them that an order that was not preserved would be visible.
func publishedSet() []artifact.File {
	return []artifact.File{
		{Path: "001-namespace-shop.yaml", Data: []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: shop\n")},
		{Path: "002-deployment-api.yaml", Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n")},
		{Path: "000-config-binary.yaml", Data: []byte{0x00, 0xff, 0xfe, 'a', '\n'}},
		{Path: "003-empty.yaml", Data: []byte{}},
	}
}

func publish(t *testing.T, repository, tag string, files []artifact.File) artifact.Artifact {
	t.Helper()
	a, err := artifact.Package(artifact.Contents{
		Repository:  repository,
		Tag:         tag,
		Files:       files,
		Annotations: map[string]string{artifact.AnnRevision: tag, artifact.AnnProject: "shop"},
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if _, err := (&artifact.Pusher{Insecure: true}).Push(context.Background(), a); err != nil {
		t.Fatalf("Push: %v", err)
	}
	return a
}

// TestPullRoundTripsThePackagedSet is the load-bearing one: what Package wrote
// and Push uploaded is what Pull hands back — same paths, same bytes, same
// order — and the digest of the manifest it was read from is the digest the
// push reported. Everything the diff against a recorded revision claims rests
// on this being exact rather than approximately right.
func TestPullRoundTripsThePackagedSet(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	pushed := publish(t, repository, "7-a1b2c3d4", publishedSet())

	pulled, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  "7-a1b2c3d4",
		Digest:     pushed.Digest,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulled.Digest != pushed.Digest {
		t.Errorf("pulled digest = %s, pushed %s", pulled.Digest, pushed.Digest)
	}
	want := publishedSet()
	if len(pulled.Files) != len(want) {
		t.Fatalf("pulled %d files, packaged %d: %v", len(pulled.Files), len(want), pathsOf(pulled.Files))
	}
	for i, f := range want {
		if pulled.Files[i].Path != f.Path {
			t.Errorf("file %d is %q, packaged %q — the tar's order is the render's order",
				i, pulled.Files[i].Path, f.Path)
		}
		if !bytes.Equal(pulled.Files[i].Data, f.Data) {
			t.Errorf("file %s came back as %q, packaged %q", f.Path, pulled.Files[i].Data, f.Data)
		}
	}
	if pulled.Annotations[artifact.AnnRevision] != "7-a1b2c3d4" {
		t.Errorf("annotations = %v, want the revision the artifact was published under", pulled.Annotations)
	}
	if pulled.Address() != repository+"@"+pushed.Digest {
		t.Errorf("Address() = %s, want the digest-pinned reference", pulled.Address())
	}
}

// A pull may name the digest instead of the tag, which is what a caller holding
// a `status.history[].digest` has: the tag is a pointer and the digest is the
// bytes.
func TestPullAcceptsADigestReference(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	pushed := publish(t, repository, "7-a1b2c3d4", publishedSet())
	fake.mu.Lock()
	fake.manifests[pushed.Digest] = fake.manifests["7-a1b2c3d4"]
	fake.mu.Unlock()

	pulled, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  pushed.Digest,
	})
	if err != nil {
		t.Fatalf("Pull by digest: %v", err)
	}
	if len(pulled.Files) != len(publishedSet()) {
		t.Errorf("pulled %d files, want %d", len(pulled.Files), len(publishedSet()))
	}
}

// A read asks for the read scope. A token minted for a push would authorise the
// wrong half on a registry that scopes them separately, and the 403 that
// followed would say nothing about which.
func TestPullAsksForThePullScope(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	publish(t, repository, "7-a1b2c3d4", publishedSet())
	fake.challenge = true

	if _, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  "7-a1b2c3d4",
	}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.scopes) == 0 {
		t.Fatal("no token was requested, so nothing asked for a scope")
	}
	for _, scope := range fake.scopes {
		if !strings.HasSuffix(scope, ":pull") {
			t.Errorf("token scope = %q, want the read scope", scope)
		}
	}
}

// A tag the registry does not hold is an answer, not a failure, and callers
// branch on it: "there is no revision 9-deadbeef" is a typo, where a registry
// that refused to look is a credential.
func TestPullReportsAMissingTagAsNotFound(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	publish(t, repository, "7-a1b2c3d4", publishedSet())

	_, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  "9-deadbeef",
	})
	if !artifact.NotFound(err) {
		t.Fatalf("Pull of an absent tag = %v, want a not-found refusal", err)
	}
}

// The registry saying a tag resolves to one digest and serving another is the
// case no fallback is acceptable for: the manifest that arrived describes an
// artifact nobody published.
func TestPullRefusesAManifestThatIsNotTheDigestTheTagResolvedTo(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	publish(t, repository, "7-a1b2c3d4", publishedSet())
	fake.servedDigest = func(string, string) string { return sha256Of([]byte("something else entirely")) }

	_, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  "7-a1b2c3d4",
	})
	var integrity *artifact.IntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("Pull = %v, want an integrity error", err)
	}
	if integrity.Want == "" || integrity.Got == "" || integrity.Want == integrity.Got {
		t.Errorf("integrity error names %q and %q, want both digests and two different ones",
			integrity.Want, integrity.Got)
	}
	if !strings.Contains(integrity.Error(), integrity.Want) || !strings.Contains(integrity.Error(), integrity.Got) {
		t.Errorf("message = %q, want both digests in it", integrity.Error())
	}
}

// The digest a caller recorded is checked too, which is what makes a pull an
// assertion that these are the bytes that revision was published as, and not
// merely the bytes behind that tag today.
func TestPullRefusesARecordedDigestThatDisagrees(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	publish(t, repository, "7-a1b2c3d4", publishedSet())

	_, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  "7-a1b2c3d4",
		Digest:     sha256Of([]byte("what the mirror remembers")),
	})
	var integrity *artifact.IntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("Pull = %v, want an integrity error", err)
	}
}

// The manifest's digest proves which blobs belong to the artifact; it says
// nothing about whether the registry then served those blobs. This is the other
// half of the chain.
func TestPullRefusesALayerServedAsDifferentBytes(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	pushed := publish(t, repository, "7-a1b2c3d4", publishedSet())
	fake.corruptBlob = func(digest string, body []byte) []byte {
		if digest == pushed.LayerDigest {
			return append(append([]byte(nil), body...), 0x00)
		}
		return body
	}

	_, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  "7-a1b2c3d4",
	})
	var integrity *artifact.IntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("Pull = %v, want an integrity error about the layer", err)
	}
	if integrity.Want != pushed.LayerDigest {
		t.Errorf("integrity error names %s, want the layer descriptor's %s", integrity.Want, pushed.LayerDigest)
	}
}

// A repository that holds an image, or another tool's artifact, is not a
// kelson revision — and answering with its layers would diff a rendered set
// against a container filesystem.
func TestPullRefusesSomethingThatIsNotAFluxArtifact(t *testing.T) {
	fake := newPullRegistry()
	repository := fake.start(t)
	publish(t, repository, "7-a1b2c3d4", publishedSet())
	fake.corruptRequest = func(_ string, body []byte) []byte {
		return bytes.ReplaceAll(body, []byte(artifact.ConfigMediaType), []byte("application/vnd.oci.image.config.v1+json"))
	}

	_, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
		Repository: repository,
		Reference:  "7-a1b2c3d4",
	})
	var refusal artifact.Error
	if !errors.As(err, &refusal) || refusal.Reason != artifact.ReasonArtifactUnreadable {
		t.Fatalf("Pull = %v, want an unreadable-artifact refusal", err)
	}
}

// Extraction refuses what packaging never writes. kelson reads these bytes to
// compare them and never writes them to a filesystem, but an entry that escapes
// the artifact root is proof this is not one of kelson's artifacts, and a
// partial answer would be worse than a refusal.
func TestPullRefusesATarEntryThatIsNotAPlainFileInTheRoot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header *tar.Header
	}{
		{"a symlink", &tar.Header{Typeflag: tar.TypeSymlink, Name: "link.yaml", Linkname: "/etc/passwd"}},
		{"an escaping path", &tar.Header{Typeflag: tar.TypeReg, Name: "../../etc/passwd", Size: 3}},
		{"an absolute path", &tar.Header{Typeflag: tar.TypeReg, Name: "/etc/passwd", Size: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newPullRegistry()
			repository := fake.start(t)
			pushed := publish(t, repository, "7-a1b2c3d4", publishedSet())
			fake.corruptBlob = func(digest string, body []byte) []byte {
				if digest != pushed.LayerDigest {
					return body
				}
				return hostileLayer(t, tc.header)
			}
			// The layer digest would catch the swap first, so this test serves
			// the hostile tar under its own digest by rewriting the manifest to
			// name it.
			fake.corruptRequest = func(_ string, manifest []byte) []byte {
				return bytes.ReplaceAll(manifest, []byte(pushed.LayerDigest),
					[]byte(sha256Of(hostileLayer(t, tc.header))))
			}
			fake.mu.Lock()
			fake.blobs[sha256Of(hostileLayer(t, tc.header))] = hostileLayer(t, tc.header)
			fake.mu.Unlock()

			_, err := (&artifact.Pusher{Insecure: true}).Pull(context.Background(), artifact.PullRequest{
				Repository: repository,
				Reference:  "7-a1b2c3d4",
			})
			var refusal artifact.Error
			if !errors.As(err, &refusal) || refusal.Reason != artifact.ReasonArtifactUnreadable {
				t.Fatalf("Pull = %v, want an unreadable-artifact refusal", err)
			}
		})
	}
}

// hostileLayer builds a gzipped tar holding one entry, for the refusals above.
func hostileLayer(t *testing.T, hdr *tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("writing the tar header: %v", err)
	}
	if hdr.Size > 0 {
		if _, err := tw.Write(bytes.Repeat([]byte("x"), int(hdr.Size))); err != nil {
			t.Fatalf("writing the tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the gzip: %v", err)
	}
	return buf.Bytes()
}

func pathsOf(files []artifact.File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}
