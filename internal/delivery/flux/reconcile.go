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
			"configure the notification-controller Receiver URL, or use the CLI reconciler")
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

// CLIReconciler shells out to the flux binary. It is the fallback for installs
// that run kelson next to a kubeconfig rather than behind a receiver.
type CLIReconciler struct {
	// Bin is the flux binary; defaults to "flux".
	Bin string
	// Run defaults to ExecRunner.
	Run Runner
}

func (c CLIReconciler) Reconcile(ctx context.Context, k Kustomization) error {
	bin := c.Bin
	if bin == "" {
		bin = "flux"
	}
	run := c.Run
	if run == nil {
		run = ExecRunner
	}

	// Reconcile the source first: reconciling the Kustomization alone would
	// re-apply the artifact Flux already has, which is exactly the revision we
	// are trying to move past.
	if k.SourceName != "" {
		kind := strings.ToLower(k.SourceKind)
		if kind == "" || kind == "gitrepository" {
			kind = "git"
		}
		if _, err := run(ctx, bin, "reconcile", "source", kind, k.SourceName, "-n", k.Namespace); err != nil {
			return triggerErr(k, err)
		}
	}
	if _, err := run(ctx, bin, "reconcile", "kustomization", k.Name, "-n", k.Namespace); err != nil {
		return triggerErr(k, err)
	}
	return nil
}

// Chain tries reconcilers in order and succeeds on the first that works. The
// canonical setup is {webhook, CLI}: the webhook when the control plane can
// reach the receiver, the binary otherwise.
type Chain []Reconciler

func (c Chain) Reconcile(ctx context.Context, k Kustomization) error {
	if len(c) == 0 {
		return delivery.ApplyFailed("flux/reconcile", "reconciler",
			"no reconciliation trigger is configured",
			"configure a Receiver webhook or the flux CLI so deployments do not wait for the poll interval")
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
			"fix the receiver URL/token or the flux CLI access to restore immediate delivery")
	e.Cause = err.Error()
	return e
}

var (
	_ Reconciler = WebhookReconciler{}
	_ Reconciler = CLIReconciler{}
	_ Reconciler = Chain{}
	_ Reconciler = NoopReconciler{}
)
