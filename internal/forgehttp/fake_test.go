package forgehttp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forgeconn"
	"github.com/dafrie/kelson/internal/model"
)

// The fakes every endpoint test drives. No test in this package touches a
// cluster or the network: the connections come from literals, the signatures
// are computed with the same HMAC the real verifier checks, and the poke is
// recorded rather than performed.

const (
	appSecret   = "webhook-secret-of-the-acme-app"
	otherSecret = "webhook-secret-of-the-globex-app"
)

// fakeStore is the forgeconn.Store half: connections and their material.
type fakeStore struct {
	conns    []controlstore.StoredConnection
	material map[string]controlstore.AuthMaterial
}

func (s *fakeStore) List(context.Context) ([]controlstore.StoredConnection, error) {
	return s.conns, nil
}

func (s *fakeStore) ReadAuthSecret(_ context.Context, c controlstore.StoredConnection) (controlstore.AuthMaterial, error) {
	m, ok := s.material[c.Name]
	if !ok {
		return controlstore.AuthMaterial{}, controlstore.NotFound("connection/"+c.Name, "no Secret", "create it")
	}
	return m, nil
}

// appConnection is a github connection authenticating as an app, with a
// webhook secret — the shape the manifest flow creates.
func appConnection(name, host string, webhookSecret string) (controlstore.StoredConnection, controlstore.AuthMaterial) {
	return controlstore.StoredConnection{
			Name: name,
			Spec: model.GitConnectionSpec{
				Provider: model.GitProviderGitHub,
				Host:     host,
				Auth: model.GitConnectionAuth{GitHubApp: &model.GitHubAppAuth{
					AppID:     42,
					SecretRef: name + "-app",
				}},
			},
		}, controlstore.AuthMaterial{
			SecretRef:     name + "-app",
			PrivateKeyPEM: []byte("-----BEGIN RSA PRIVATE KEY-----\nnot-a-real-key\n-----END RSA PRIVATE KEY-----\n"),
			WebhookSecret: []byte(webhookSecret),
		}
}

func storeWith(pairs ...struct {
	conn controlstore.StoredConnection
	mat  controlstore.AuthMaterial
}) *fakeStore {
	s := &fakeStore{material: map[string]controlstore.AuthMaterial{}}
	for _, p := range pairs {
		s.conns = append(s.conns, p.conn)
		s.material[p.conn.Name] = p.mat
	}
	return s
}

func pair(conn controlstore.StoredConnection, mat controlstore.AuthMaterial) struct {
	conn controlstore.StoredConnection
	mat  controlstore.AuthMaterial
} {
	return struct {
		conn controlstore.StoredConnection
		mat  controlstore.AuthMaterial
	}{conn, mat}
}

// fakeSpecs serves stored projects from literals.
type fakeSpecs struct{ stored []controlstore.Stored }

func (f *fakeSpecs) List(context.Context) ([]controlstore.Stored, error) { return f.stored, nil }

// projectWithPreviews is one stored project whose single environment previews a
// repository.
func projectWithPreviews(project, environment, repo string) controlstore.Stored {
	doc := fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: %s
spec:
  project: %s
  previews:
    provider: github
    repo: %s
    secretRef: forge-auth
    artifacts:
      repository: oci://ghcr.io/acme/%s-previews
`, environment, project, repo, project)
	return controlstore.Stored{
		Project:      project,
		Environments: []string{environment},
		Documents: controlstore.Documents{
			Environments: map[string][]byte{environment: []byte(doc)},
		},
	}
}

// projectFromSource is one stored project that declares a source in a
// repository, which is what makes a push to that repository the project's
// business (ADR-0035 decision 4).
func projectFromSource(project, repo string) controlstore.Stored {
	doc := fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: %s
spec:
  components:
    - name: web
      kind: service
      port: 8080
  source:
    git: %s
    ref: main
`, project, repo)
	return controlstore.Stored{
		Project:   project,
		Documents: controlstore.Documents{Project: []byte(doc)},
	}
}

// fakeAutoDeploy is the trigger seam, recorded rather than performed. What it
// stands in for is internal/api's resolve-build-write pipeline, which has its
// own tests; what matters here is that the handler asks the right question
// synchronously and enqueues the right work behind it.
type fakeAutoDeploy struct {
	mu       sync.Mutex
	plan     PushPlan
	planErr  error
	plans    []Push
	runs     []Push
	outcome  PushOutcome
	runError error
}

func (f *fakeAutoDeploy) PlanPush(_ context.Context, p Push) (PushPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, p)
	return f.plan, f.planErr
}

func (f *fakeAutoDeploy) RunPush(_ context.Context, p Push) (PushOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, p)
	return f.outcome, f.runError
}

func (f *fakeAutoDeploy) planned() []Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Push(nil), f.plans...)
}

func (f *fakeAutoDeploy) ran() []Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Push(nil), f.runs...)
}

// fakePoker records what was stamped instead of stamping it.
type fakePoker struct {
	mu     sync.Mutex
	poked  []string
	failed map[string]error
}

func (f *fakePoker) Poke(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := namespace + "/" + name
	if err, ok := f.failed[ref]; ok {
		return err
	}
	f.poked = append(f.poked, ref)
	return nil
}

func (f *fakePoker) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.poked...)
}

// fakeConnections is the write half: what the manifest callback and the
// installation event reach for.
type fakeConnections struct {
	created       map[string]model.GitConnectionSpec
	createErr     error
	installations map[string]int64
	recordErr     error
}

func newFakeConnections() *fakeConnections {
	return &fakeConnections{
		created:       map[string]model.GitConnectionSpec{},
		installations: map[string]int64{},
	}
}

func (f *fakeConnections) List(context.Context) ([]controlstore.StoredConnection, error) {
	return nil, nil
}

func (f *fakeConnections) Get(_ context.Context, name string) (controlstore.StoredConnection, error) {
	spec, ok := f.created[name]
	if !ok {
		return controlstore.StoredConnection{}, controlstore.NotFound("connection/"+name, "none", "create it")
	}
	return controlstore.StoredConnection{Name: name, Spec: spec}, nil
}

func (f *fakeConnections) Create(_ context.Context, name string, spec model.GitConnectionSpec, _ controlstore.CreateConnectionOptions) (controlstore.StoredConnection, error) {
	if f.createErr != nil {
		return controlstore.StoredConnection{}, f.createErr
	}
	f.created[name] = spec
	return controlstore.StoredConnection{Name: name, Spec: spec}, nil
}

func (f *fakeConnections) RecordInstallation(_ context.Context, name string, id int64) (controlstore.StoredConnection, error) {
	if f.recordErr != nil {
		return controlstore.StoredConnection{}, f.recordErr
	}
	f.installations[name] = id
	return controlstore.StoredConnection{Name: name}, nil
}

// fakeSecrets records what the manifest callback wrote.
type fakeSecrets struct {
	written map[string]map[string][]byte
	err     error
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{written: map[string]map[string][]byte{}}
}

func (f *fakeSecrets) Write(_ context.Context, name string, data map[string][]byte) error {
	if f.err != nil {
		return f.err
	}
	f.written[name] = data
	return nil
}

// sign computes the header GitHub sends, with the same HMAC internal/forge
// verifies — so a fixture that passes here would pass against a real delivery
// and a fixture that fails would fail against one.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// delivery builds a signed webhook request.
func delivery(t *testing.T, kind, secret string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, WebhookPath, bytesReader(body))
	req.Header.Set("X-GitHub-Event", kind)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func handlerWith(t *testing.T, opts Options) *Handler {
	t.Helper()
	h, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func resolverOver(store *fakeStore) *forgeconn.Resolver {
	return &forgeconn.Resolver{Store: store}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// serve runs one request through the registered routes, which is what proves
// the method-scoped patterns as well as the handlers. The credential check is
// the permissive one — these tests are about the forge endpoints, and the tests
// that are about the gate hand in their own (serveGated) or drive the real
// server's (cmd/kelson-server/main_test.go).
func serve(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	return serveGated(h, req, allowAll)
}

// serveGated is serve with a stated credential check.
func serveGated(h *Handler, req *http.Request, authenticate Authenticator) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.Register(mux, authenticate)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// allowAll is the check a server with neither a password nor an agent store
// applies: internal/api answers every request with no refusal, and so does this.
func allowAll(*http.Request) string { return "" }
