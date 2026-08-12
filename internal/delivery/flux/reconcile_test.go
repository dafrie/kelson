package flux

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// TestWebhookReconcilerSignsRequest verifies the receiver call carries the
// payload and, with a token configured, the HMAC signature the
// notification-controller generic-hmac receiver expects.
func TestWebhookReconcilerSignsRequest(t *testing.T) {
	var gotSig string
	var gotPayload map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Signature")
		_ = json.NewDecoder(r.Body).Decode(&gotPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := WebhookReconciler{URL: srv.URL, Token: "shared-secret", HTTP: srv.Client()}
	k := Kustomization{Name: "web", Namespace: "apps", SourceKind: "GitRepository", SourceName: "deploy"}
	if err := r.Reconcile(context.Background(), k); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !strings.HasPrefix(gotSig, "sha256=") {
		t.Fatalf("signature = %q, want sha256=...", gotSig)
	}
	if gotPayload["kustomization"] != "apps/web" || gotPayload["source"] != "GitRepository/deploy" {
		t.Fatalf("payload = %v", gotPayload)
	}
}

// TestWebhookReconcilerNoTokenSkipsSignature verifies an unauthenticated
// receiver (or a receiver behind mTLS) still works.
func TestWebhookReconcilerNoTokenSkipsSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Signature") != "" {
			t.Fatalf("signature must be absent without a token")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	r := WebhookReconciler{URL: srv.URL, HTTP: srv.Client()}
	if err := r.Reconcile(context.Background(), Kustomization{Name: "web", Namespace: "apps"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// TestWebhookReconcilerErrorsAreStructured verifies a non-2xx response becomes
// a delivery/apply-failed that says the commit is safe.
func TestWebhookReconcilerErrorsAreStructured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := WebhookReconciler{URL: srv.URL, HTTP: srv.Client()}
	err := r.Reconcile(context.Background(), Kustomization{Name: "web", Namespace: "apps"})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
	if !strings.Contains(err.Error(), "committed") {
		t.Fatalf("error must note the change is committed: %v", err)
	}
}

// TestCLIReconcilerTriggersSourceThenKustomization verifies the flux CLI path
// reconciles the source first, then the kustomization.
func TestCLIReconcilerTriggersSourceThenKustomization(t *testing.T) {
	var calls [][]string
	r := CLIReconciler{Bin: "flux", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("ok"), nil
	}}
	k := Kustomization{Name: "web", Namespace: "apps", SourceKind: "GitRepository", SourceName: "deploy"}
	if err := r.Reconcile(context.Background(), k); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want 2", calls)
	}
	if calls[0][1] != "reconcile" || calls[0][2] != "source" || calls[0][3] != "git" || calls[0][4] != "deploy" {
		t.Fatalf("source call = %v", calls[0])
	}
	if calls[1][1] != "reconcile" || calls[1][2] != "kustomization" || calls[1][3] != "web" {
		t.Fatalf("kustomization call = %v", calls[1])
	}
}

// TestChainFallsBack verifies a chain tries reconcilers in order.
func TestChainFallsBack(t *testing.T) {
	ran := 0
	first := ReconcilerFunc(func(ctx context.Context, k Kustomization) error {
		ran++
		return delivery.ApplyFailed("x", "", "first failed", "retry")
	})
	second := ReconcilerFunc(func(ctx context.Context, k Kustomization) error {
		ran++
		return nil
	})
	c := Chain{first, second}
	if err := c.Reconcile(context.Background(), Kustomization{}); err != nil {
		t.Fatalf("chain: %v", err)
	}
	if ran != 2 {
		t.Fatalf("chain ran %d reconcilers, want 2", ran)
	}
}

// TestChainEmptyFails verifies no configured trigger is a loud error, never a
// silent "ok" (the change would only arrive at the poll interval).
func TestChainEmptyFails(t *testing.T) {
	var empty Chain
	if err := empty.Reconcile(context.Background(), Kustomization{}); err == nil {
		t.Fatalf("empty chain must fail loudly")
	}
}

// ReconcilerFunc adapts a function to the Reconciler interface for tests.
type ReconcilerFunc func(ctx context.Context, k Kustomization) error

func (f ReconcilerFunc) Reconcile(ctx context.Context, k Kustomization) error { return f(ctx, k) }

var _ Reconciler = (ReconcilerFunc)(nil)
