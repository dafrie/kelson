package flux

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

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

// TestAnnotationReconcilerTriggersSourceThenKustomization is the same contract
// the flux-CLI reconciler used to carry — source first, then the Kustomization
// — asserted against the dynamic client that replaced it (#137): two patches,
// in that order, each stamping reconcile.fluxcd.io/requestedAt.
func TestAnnotationReconcilerTriggersSourceThenKustomization(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, gitRepositoryGVR, object(gitRepositoryGVK, "apps", "deploy", nil))
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, "apps", "web", nil))

	var patched []string
	dyn.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa := action.(k8stesting.PatchAction)
		patched = append(patched, pa.GetResource().Resource+"/"+pa.GetName())
		return false, nil, nil
	})

	at := time.Date(2026, 8, 13, 10, 30, 0, 0, time.UTC)
	r := AnnotationReconciler{Client: dyn, Now: func() time.Time { return at }}
	k := Kustomization{Name: "web", Namespace: "apps", SourceKind: "GitRepository", SourceName: "deploy"}
	if err := r.Reconcile(context.Background(), k); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if want := []string{"gitrepositories/deploy", "kustomizations/web"}; !reflect.DeepEqual(patched, want) {
		t.Fatalf("patched = %v, want %v", patched, want)
	}

	// The annotation is the whole trigger: a patch that landed without it would
	// touch the object and reconcile nothing.
	for _, c := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{{gitRepositoryGVR, "deploy"}, {kustomizationGVR, "web"}} {
		got, err := dyn.Resource(c.gvr).Namespace("apps").Get(context.Background(), c.name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", c.name, err)
		}
		if stamp := got.GetAnnotations()[requestedAtAnnotation]; stamp != at.Format(time.RFC3339Nano) {
			t.Fatalf("%s annotation = %q, want %q", c.name, stamp, at.Format(time.RFC3339Nano))
		}
	}
}

// TestAnnotationReconcilerReportsFailure verifies a trigger that could not
// reach the object is a delivery/apply-failed saying the commit is safe — the
// same contract the CLI fallback carried.
func TestAnnotationReconcilerReportsFailure(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	r := AnnotationReconciler{Client: dyn}
	err := r.Reconcile(context.Background(), Kustomization{Name: "web", Namespace: "apps"})
	if err == nil {
		t.Fatal("patching a Kustomization that does not exist must fail")
	}
	if !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
	if !strings.Contains(err.Error(), "committed") {
		t.Fatalf("error must note the change is committed: %v", err)
	}
}

// TestAnnotationReconcilerWithoutClientIsLoud verifies the misconfiguration is
// named rather than silently degrading to the poll interval.
func TestAnnotationReconcilerWithoutClientIsLoud(t *testing.T) {
	err := AnnotationReconciler{}.Reconcile(context.Background(), Kustomization{Name: "web", Namespace: "apps"})
	if err == nil || !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
}

// TestAnnotationReconcilerRejectsNonGitSource verifies a source kind this
// adapter cannot have written is reported instead of guessed at.
func TestAnnotationReconcilerRejectsNonGitSource(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	r := AnnotationReconciler{Client: dyn}
	err := r.Reconcile(context.Background(), Kustomization{
		Name: "web", Namespace: "apps", SourceKind: "OCIRepository", SourceName: "artifacts",
	})
	if err == nil || !strings.Contains(err.Error(), "OCIRepository") {
		t.Fatalf("error = %v, want the source kind named", err)
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
