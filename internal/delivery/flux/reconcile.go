package flux

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/dafrie/kelson/internal/delivery"
)

// Reconciler pokes Flux to reconcile now instead of at its poll interval.
// Perceived latency is the whole point of the trigger: without it a Git-mode
// deployment feels minutes slower than direct mode for no good reason
// (docs/architecture.md, Delivery adapters).
type Reconciler interface {
	// Reconcile triggers reconciliation of the source behind k and of k
	// itself. It must return promptly; it does not wait for the result —
	// Status answers "is it live yet".
	Reconcile(ctx context.Context, k Kustomization) error
}

// WebhookReconciler notifies a notification-controller Receiver. This is the
// preferred trigger for a control plane: it needs no cluster credentials and
// no binaries, only the receiver URL and its token.
type WebhookReconciler struct {
	// URL is the receiver endpoint, e.g.
	// https://flux-webhook.example.com/hook/<receiver-path>.
	URL string
	// Token is the receiver's shared secret. When set, the request carries an
	// X-Signature HMAC over the body, matching notification-controller's
	// generic-hmac receiver type.
	Token string
	// HTTP is optional; tests inject an httptest client.
	HTTP interface {
		Do(*http.Request) (*http.Response, error)
	}
}

func (w WebhookReconciler) Reconcile(ctx context.Context, k Kustomization) error {
	if w.URL == "" {
		return delivery.ApplyFailed("flux/reconcile", "webhook",
			"no Flux receiver webhook is configured",
			"configure the notification-controller Receiver URL, or use the annotation reconciler")
	}
	payload, err := json.Marshal(map[string]string{
		"kustomization": k.Namespace + "/" + k.Name,
		"source":        k.SourceKind + "/" + k.SourceName,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kelson")
	if w.Token != "" {
		mac := hmac.New(sha256.New, []byte(w.Token))
		mac.Write(payload)
		req.Header.Set("X-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	client := w.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return triggerErr(k, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return triggerErr(k, fmt.Errorf("receiver returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	return nil
}

// requestedAtAnnotation is what a Flux controller watches for an
// out-of-schedule reconciliation request. Stamping it with the current time is
// exactly what `flux reconcile` does under the hood.
const requestedAtAnnotation = "reconcile.fluxcd.io/requestedAt"

// fieldManager identifies kelson's writes on the annotation patch, so a human
// reading managedFields can see which component asked for the reconciliation.
const fieldManager = "kelson"

// AnnotationReconciler triggers reconciliation by stamping
// reconcile.fluxcd.io/requestedAt through the dynamic client. It replaces the
// flux-CLI fallback this adapter used to shell out to (issue #137): the CLI's
// `reconcile` command is this annotation patch, so kelson can do it directly
// with the cluster connection the rest of the delivery plane already holds and
// needs no binary on PATH — which also means it works from a container image
// that ships neither kubectl nor flux.
//
// The Receiver webhook stays the preferred trigger (WebhookReconciler): it
// needs no cluster credentials at all. This is the fallback for installs that
// have credentials but no reachable receiver.
type AnnotationReconciler struct {
	// Client is the dynamic client, the same one DynamicStatusReader reads with.
	Client dynamic.Interface
	// Now is injectable so tests get a deterministic stamp.
	Now func() time.Time
}

func (a AnnotationReconciler) Reconcile(ctx context.Context, k Kustomization) error {
	if a.Client == nil {
		return delivery.ApplyFailed("flux/reconcile", "client",
			"the annotation trigger has no Kubernetes client",
			"build one with internal/delivery/kube.Connect and pass it as Client, or configure a Receiver webhook")
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}
	// RFC3339Nano: the value only has to differ from the last one the
	// controller saw, and Flux itself writes this format.
	stamp := now().UTC().Format(time.RFC3339Nano)

	// Reconcile the source first: reconciling the Kustomization alone would
	// re-apply the artifact Flux already has, which is exactly the revision we
	// are trying to move past.
	if k.SourceName != "" {
		gvr, err := sourceGVR(k.SourceKind)
		if err != nil {
			return triggerErr(k, err)
		}
		if err := a.stamp(ctx, gvr, k.Namespace, k.SourceName, stamp); err != nil {
			return triggerErr(k, err)
		}
	}
	if err := a.stamp(ctx, kustomizationGVR, k.Namespace, k.Name, stamp); err != nil {
		return triggerErr(k, err)
	}
	return nil
}

func (a AnnotationReconciler) stamp(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, at string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{requestedAtAnnotation: at}},
	})
	if err != nil {
		return err
	}
	_, err = a.Client.Resource(gvr).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
	return err
}

// sourceGVR resolves a Kustomization's sourceRef kind to the resource holding
// it. Only GitRepository is resolvable: this adapter writes to a git repository
// by construction (Capabilities.RequiresGit), so a Kustomization covering
// kelson's path with any other source kind is a misconfiguration to report
// rather than a trigger to guess at.
func sourceGVR(kind string) (schema.GroupVersionResource, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "gitrepository":
		return gitRepositoryGVR, nil
	}
	return schema.GroupVersionResource{}, fmt.Errorf(
		"source kind %q is not a GitRepository, so kelson cannot trigger it", kind)
}

// Chain tries reconcilers in order and succeeds on the first that works. The
// canonical setup is {webhook, annotation}: the webhook when the control plane
// can reach the receiver, the annotation patch when it holds cluster
// credentials instead.
type Chain []Reconciler

func (c Chain) Reconcile(ctx context.Context, k Kustomization) error {
	if len(c) == 0 {
		return delivery.ApplyFailed("flux/reconcile", "reconciler",
			"no reconciliation trigger is configured",
			"configure a Receiver webhook or the annotation trigger so deployments do not wait for the poll interval")
	}
	var errs []error
	for _, r := range c {
		err := r.Reconcile(ctx, k)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// NoopReconciler disables the immediate trigger; Flux still picks the change
// up at its poll interval. Useful for read-only installs, and honest about the
// latency cost.
type NoopReconciler struct{}

func (NoopReconciler) Reconcile(context.Context, Kustomization) error { return nil }

func triggerErr(k Kustomization, err error) error {
	e := delivery.ApplyFailed("flux/reconcile", "",
		fmt.Sprintf("committed, but could not trigger immediate reconciliation of Kustomization %s/%s", k.Namespace, k.Name),
		"the change is committed and Flux will still pick it up at its poll interval; "+
			"fix the receiver URL/token or kelson's permission to annotate Flux objects to restore immediate delivery")
	e.Cause = err.Error()
	return e
}

var (
	_ Reconciler = WebhookReconciler{}
	_ Reconciler = AnnotationReconciler{}
	_ Reconciler = Chain{}
	_ Reconciler = NoopReconciler{}
)
