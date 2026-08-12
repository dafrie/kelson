package argocd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

const applicationJSONBody = `{
  "metadata": {"name": "shop-production", "namespace": "argocd"},
  "spec": {
    "project": "platform",
    "source": {"repoURL": "https://forge.example/acme/deploy.git", "path": "manifests", "targetRevision": "main"},
    "syncPolicy": {"automated": {"prune": true, "selfHeal": true}}
  },
  "status": {
    "sync": {"status": "Synced", "revision": "1111111111111111111111111111111111111111"},
    "health": {"status": "Degraded", "message": "Deployment checkout has 0/3 replicas available"},
    "operationState": {"phase": "Succeeded", "syncResult": {"revision": "1111111111111111111111111111111111111111"}},
    "conditions": [{"type": "OrphanedResourceWarning", "message": "1 orphaned resource"}]
  }
}`

// TestClientReadsApplication verifies the Argo Application API is read over
// plain HTTP with a bearer token, and that Argo's own sync, health and sync
// policy land on the fields the phase mapping consumes.
func TestClientReadsApplication(t *testing.T) {
	var gotPath, gotAuth, gotNamespace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		gotNamespace = r.URL.Query().Get("appNamespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(applicationJSONBody))
	}))
	defer srv.Close()

	c, err := NewClient(ClientConfig{BaseURL: srv.URL + "/", Token: "secret", Namespace: "argocd", HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	app, found, err := c.Application(context.Background(), "shop-production")
	if err != nil || !found {
		t.Fatalf("application = %v, %v, %v", app, found, err)
	}
	if gotPath != "/api/v1/applications/shop-production" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotNamespace != "argocd" {
		t.Fatalf("appNamespace = %q", gotNamespace)
	}
	if app.Project != "platform" || app.RepoURL != "https://forge.example/acme/deploy.git" ||
		app.Path != "manifests" || app.TargetRevision != "main" {
		t.Fatalf("source/project = %+v", app)
	}
	if !app.AutoSync || !app.SelfHeal {
		t.Fatalf("sync policy = auto %v self-heal %v, want both true", app.AutoSync, app.SelfHeal)
	}
	if app.SyncStatus != SyncSynced || app.Health != HealthDegraded ||
		app.SyncedRevision != ourRevision || app.Operation.Revision != ourRevision {
		t.Fatalf("status = %+v", app)
	}
	if len(app.Conditions) != 1 || app.Conditions[0].Type != "OrphanedResourceWarning" {
		t.Fatalf("conditions = %+v", app.Conditions)
	}

	// The end-to-end guarantee: Argo says Degraded, kelson says Degraded.
	if st := phaseFor(app, ourRevision); st.Phase != delivery.PhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", st.Phase)
	}
}

// TestClientReadsMultiSourceApplication verifies spec.sources installs are read
// too; the first source is the one a delivery target maps to.
func TestClientReadsMultiSourceApplication(t *testing.T) {
	body := `{"metadata":{"name":"shop-production"},"spec":{"sources":[{"repoURL":"https://forge.example/acme/deploy.git","path":"manifests","targetRevision":"main"}]},"status":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, HTTP: srv.Client()})
	app, found, err := c.Application(context.Background(), "shop-production")
	if err != nil || !found {
		t.Fatalf("application = %v, %v", found, err)
	}
	if app.Path != "manifests" || app.TargetRevision != "main" {
		t.Fatalf("source = %+v", app)
	}
	if app.SyncStatus != SyncUnknown || app.Health != HealthUnknown {
		t.Fatalf("an Application Argo has not assessed yet must read as Unknown, got %+v", app)
	}
}

// TestClientApplicationNotFound verifies a missing Application is a
// configuration answer (found=false), not an error to unwrap — the adapter
// turns it into delivery/not-watched.
func TestClientApplicationNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"applications.argoproj.io \"nope\" not found","code":5}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, HTTP: srv.Client()})
	app, found, err := c.Application(context.Background(), "nope")
	if err != nil {
		t.Fatalf("a missing Application must not be an error: %v", err)
	}
	if found {
		t.Fatalf("found = true for %+v", app)
	}
}

// TestClientForbiddenNamesTheRBACFix verifies a token without access produces a
// structured error naming the Argo RBAC policy lines it needs.
func TestClientForbiddenNamesTheRBACFix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"permission denied: applications, get, default/shop-production"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, Token: "weak", HTTP: srv.Client()})
	_, _, err := c.Application(context.Background(), "shop-production")
	if err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
	if !strings.Contains(err.Error(), "RBAC") || !strings.Contains(err.Error(), "applications, sync") {
		t.Fatalf("error must carry the RBAC remediation: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error must carry Argo's own message as the cause: %v", err)
	}
}

// TestClientSyncPostsCommittedRevision is the #35 trigger acceptance: after the
// commit, kelson POSTs a sync for exactly the revision it wrote, so delivery
// does not wait for Argo's poll interval.
func TestClientSyncPostsCommittedRevision(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, Token: "secret", Namespace: "argocd", Prune: true, HTTP: srv.Client()})
	err := c.Sync(context.Background(), Application{Name: "shop-production", Namespace: "argocd"}, ourRevision)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/applications/shop-production/sync" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if gotBody["name"] != "shop-production" || gotBody["revision"] != ourRevision {
		t.Fatalf("body = %v", gotBody)
	}
	if gotBody["prune"] != true || gotBody["dryRun"] != false || gotBody["appNamespace"] != "argocd" {
		t.Fatalf("body = %v", gotBody)
	}
}

// TestClientSyncDefaultsToNoPrune verifies kelson does not implicitly delete
// live resources: pruning is the Application's sync policy to decide.
func TestClientSyncDefaultsToNoPrune(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, HTTP: srv.Client()})
	if err := c.Sync(context.Background(), Application{Name: "shop-production"}, ourRevision); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if gotBody["prune"] != false {
		t.Fatalf("body = %v", gotBody)
	}
	if _, ok := gotBody["appNamespace"]; ok {
		t.Fatalf("appNamespace must be omitted for a single-namespace install: %v", gotBody)
	}
}

// TestClientSyncFailureSaysTheCommitIsSafe verifies a failed trigger is reported
// as apply-failed, and that the remediation differs by sync policy: an auto-sync
// Application still converges, a manual one waits for a human.
func TestClientSyncFailureSaysTheCommitIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"application is being deleted"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, HTTP: srv.Client()})

	err := c.Sync(context.Background(), Application{Name: "shop-production", Namespace: "argocd"}, ourRevision)
	if err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
	if !strings.Contains(err.Error(), "committed") || !strings.Contains(err.Error(), "argocd app sync") {
		t.Fatalf("manual-sync remediation = %v", err)
	}

	err = c.Sync(context.Background(), Application{Name: "shop-production", Namespace: "argocd", AutoSync: true}, ourRevision)
	if !strings.Contains(err.Error(), "poll interval") {
		t.Fatalf("auto-sync remediation must say the change still arrives: %v", err)
	}
}

// TestNewClientRequiresBaseURL verifies the configuration error is structured.
func TestNewClientRequiresBaseURL(t *testing.T) {
	if _, err := NewClient(ClientConfig{Token: "t"}); err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
}

// TestNoopSyncer documents the read-only configuration: no trigger, no error —
// an auto-sync Application still picks the commit up at its poll interval.
func TestNoopSyncer(t *testing.T) {
	if err := (NoopSyncer{}).Sync(context.Background(), Application{}, ourRevision); err != nil {
		t.Fatalf("noop sync: %v", err)
	}
}
