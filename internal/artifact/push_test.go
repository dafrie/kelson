package artifact_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/artifact"
)

// The push half is tested against a fake registry rather than a mocked client,
// because what is worth pinning is the conversation: which requests, in which
// order, with which content types, and what happens when the registry answers
// a challenge instead of the request. Nothing here touches a network beyond
// httptest's loopback listener.

// fakeRegistry implements enough of the OCI distribution API to accept a push:
// blob existence, the two-step blob upload, and the manifest PUT.
type fakeRegistry struct {
	mu sync.Mutex
	// blobs and manifests are the stored content, keyed by digest and by tag.
	blobs     map[string][]byte
	manifests map[string][]byte
	// requests is every method+path the pusher issued, in order.
	requests []string
	// contentTypes records what the manifest was PUT as.
	manifestType string

	// requireToken makes the registry answer unauthenticated requests with a
	// bearer challenge, the way the large hosted registries do.
	requireToken bool
	tokenIssued  bool
	username     string
	password     string
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
}

func (f *fakeRegistry) handler(t *testing.T, base *string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		if r.URL.Path == "/token" {
			user, pass, _ := r.BasicAuth()
			if user != f.username || pass != f.password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if got := r.URL.Query().Get("scope"); !strings.Contains(got, "acme/previews") {
				t.Errorf("token scope = %q, want it scoped to the repository", got)
			}
			f.mu.Lock()
			f.tokenIssued = true
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "a-bearer-token"})
			return
		}

		if f.requireToken && r.Header.Get("Authorization") != "Bearer a-bearer-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+*base+`/token",service="fake",scope="repository:acme/previews:pull,push"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case strings.Contains(r.URL.Path, "/blobs/uploads/"):
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "/v2/acme/previews/blobs/uploads/session-1")
				w.WriteHeader(http.StatusAccepted)
				return
			}
			body, _ := io.ReadAll(r.Body)
			digest := r.URL.Query().Get("digest")
			if digest == "" {
				t.Error("a blob was completed without a digest query")
			}
			f.mu.Lock()
			f.blobs[digest] = body
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/blobs/"):
			digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			f.mu.Lock()
			_, ok := f.blobs[digest]
			f.mu.Unlock()
			if ok {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/manifests/"):
			body, _ := io.ReadAll(r.Body)
			tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			f.mu.Lock()
			f.manifests[tag] = body
			f.manifestType = r.Header.Get("Content-Type")
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// start runs the fake and returns the repository reference pointing at it.
func (f *fakeRegistry) start(t *testing.T) string {
	t.Helper()
	var base string
	srv := httptest.NewServer(f.handler(t, &base))
	base = srv.URL
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://") + "/acme/previews"
}

func (f *fakeRegistry) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// artifactFor packages the fixture set for a fake registry's address.
func artifactFor(t *testing.T, repository string) artifact.Artifact {
	t.Helper()
	c := testContents()
	c.Repository = repository
	return mustPackage(t, c)
}

// TestPushUploadsBlobsThenManifest pins the order and the content: an
// interrupted push must leave unreferenced blobs, which registries collect, and
// never a manifest pointing at blobs that are not there.
func TestPushUploadsBlobsThenManifest(t *testing.T) {
	fake := newFakeRegistry()
	repository := fake.start(t)
	a := artifactFor(t, repository)

	ref, err := (&artifact.Pusher{}).Push(context.Background(), a)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if ref != a.Reference() {
		t.Errorf("Push returned %q, want the digest-pinned reference %q", ref, a.Reference())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := string(fake.manifests[a.Tag]); got != string(a.Manifest) {
		t.Errorf("the registry stored a different manifest under %s", a.Tag)
	}
	if fake.manifestType != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("the manifest was PUT as %q", fake.manifestType)
	}
	if got := fake.blobs[a.LayerDigest]; string(got) != string(a.Layer) {
		t.Error("the layer blob was not uploaded verbatim")
	}
	if _, ok := fake.blobs[a.ConfigDigest]; !ok {
		t.Error("the config blob was never uploaded")
	}

	last := fake.requests[len(fake.requests)-1]
	if !strings.Contains(last, "/manifests/") {
		t.Errorf("the last request was %q, want the manifest PUT to come last", last)
	}
}

// TestPushTagsWhatItWasGiven: the tag is the caller's (the head commit for a
// preview, <generation>-<spec-hash-short> for the spine) and the push must not
// invent one.
func TestPushTagsWhatItWasGiven(t *testing.T) {
	fake := newFakeRegistry()
	a := artifactFor(t, fake.start(t))
	if _, err := (&artifact.Pusher{}).Push(context.Background(), a); err != nil {
		t.Fatalf("Push: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.manifests[testTag]; !ok {
		t.Errorf("the artifact was not tagged %s (stored tags: %v)", testTag, keys(fake.manifests))
	}
}

// TestPushSkipsBlobsTheRegistryAlreadyHas is what makes republishing an
// unchanged commit nearly free — and it only works because packaging is
// deterministic.
func TestPushSkipsBlobsTheRegistryAlreadyHas(t *testing.T) {
	fake := newFakeRegistry()
	repository := fake.start(t)
	a := artifactFor(t, repository)

	pusher := &artifact.Pusher{}
	if _, err := pusher.Push(context.Background(), a); err != nil {
		t.Fatalf("first push: %v", err)
	}
	before := len(fake.calls())
	if _, err := pusher.Push(context.Background(), a); err != nil {
		t.Fatalf("second push: %v", err)
	}
	second := fake.calls()[before:]

	for _, call := range second {
		if strings.HasPrefix(call, "POST") {
			t.Errorf("the second push started an upload (%s) for a blob the registry already held", call)
		}
	}
	if len(second) != 3 {
		t.Errorf("the second push made %d requests (%v), want two HEADs and the manifest", len(second), second)
	}
}

// TestPushAnswersABearerChallenge is the flow every hosted registry uses: the
// first request is refused with a realm, the credential is exchanged for a
// token there, and the retry carries it.
func TestPushAnswersABearerChallenge(t *testing.T) {
	fake := newFakeRegistry()
	fake.requireToken = true
	fake.username, fake.password = "robot", "s3cret"
	a := artifactFor(t, fake.start(t))

	pusher := &artifact.Pusher{Credential: registry.Credential{Username: "robot", Password: "s3cret"}}
	if _, err := pusher.Push(context.Background(), a); err != nil {
		t.Fatalf("Push: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.tokenIssued {
		t.Error("no token was ever requested")
	}
	if _, ok := fake.manifests[a.Tag]; !ok {
		t.Error("the artifact never landed")
	}
}

// TestPushWithoutCredentialsSaysHowToSupplyThem: a 401 with no credential is
// the most common CI failure this command will produce, so the message names
// both ways to fix it and neither of them is "read the source".
func TestPushWithoutCredentialsSaysHowToSupplyThem(t *testing.T) {
	fake := newFakeRegistry()
	fake.requireToken = true
	fake.username, fake.password = "robot", "s3cret"
	a := artifactFor(t, fake.start(t))

	_, err := (&artifact.Pusher{}).Push(context.Background(), a)
	if err == nil {
		t.Fatal("an unauthenticated push succeeded against a registry that requires a token")
	}
	msg := err.Error()
	if !strings.Contains(msg, "docker login") || !strings.Contains(msg, "--registry-secret") {
		t.Errorf("the failure does not say how to supply a credential: %s", msg)
	}
}

// TestPushNeverEchoesTheCredential is ADR-0009's rule at this surface: an error
// message may name the registry and the repository, never the secret.
func TestPushNeverEchoesTheCredential(t *testing.T) {
	fake := newFakeRegistry()
	fake.requireToken = true
	fake.username, fake.password = "robot", "s3cret"
	a := artifactFor(t, fake.start(t))

	pusher := &artifact.Pusher{Credential: registry.Credential{Username: "robot", Password: "wrong-password"}}
	_, err := pusher.Push(context.Background(), a)
	if err == nil {
		t.Fatal("a push with the wrong password succeeded")
	}
	if strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("the failure quotes the credential: %s", err)
	}
}

// TestPushRefusesAPinnedRepository: the tag is the publisher's to choose, and a
// spec that pinned one would publish every change request over one artifact.
func TestPushRefusesAPinnedRepository(t *testing.T) {
	a := artifactFor(t, newFakeRegistry().start(t))
	a.Repository += ":latest"
	_, err := (&artifact.Pusher{}).Push(context.Background(), a)
	if err == nil {
		t.Fatal("a repository carrying a tag was accepted")
	}
	var refusal artifact.Error
	if !asArtifactError(err, &refusal) || refusal.Reason != artifact.ReasonRepositoryInvalid {
		t.Errorf("error = %v, want %s", err, artifact.ReasonRepositoryInvalid)
	}
}

func TestRegistryHost(t *testing.T) {
	host, err := artifact.RegistryHost("oci://ghcr.io/acme/checkout-previews")
	if err != nil {
		t.Fatalf("RegistryHost: %v", err)
	}
	if host != "ghcr.io" {
		t.Errorf("host = %q, want ghcr.io", host)
	}
}

// asArtifactError is errors.As without the import, because artifact.Error is a
// value type with no wrapping.
func asArtifactError(err error, target *artifact.Error) bool {
	e, ok := err.(artifact.Error)
	if !ok {
		return false
	}
	*target = e
	return true
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
