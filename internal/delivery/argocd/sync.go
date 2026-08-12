package argocd

import (
	"context"
	"fmt"

	"github.com/dafrie/kelson/internal/delivery"
)

// Syncer makes Argo apply the committed revision now instead of at its poll
// interval. Perceived latency is the whole point of the trigger: without it a
// Git-mode deployment feels minutes slower than direct mode for no good reason
// (docs/architecture.md, Delivery adapters). With auto-sync enabled Argo would
// get there on its own; with manual sync this call is the delivery.
type Syncer interface {
	// Sync triggers a sync of app to the given revision. It must return
	// promptly; it does not wait for the operation to finish — Status answers
	// "is it live yet".
	Sync(ctx context.Context, app Application, revision string) error
}

// NoopSyncer disables the immediate trigger. An auto-sync Application still
// picks the change up at its poll interval; a manual-sync one waits for a human
// to sync it. Useful for read-only tokens, and honest about the latency cost.
type NoopSyncer struct{}

func (NoopSyncer) Sync(context.Context, Application, string) error { return nil }

// syncErr reports a commit that landed but could not be synced. The distinction
// the message carries is the one the user needs: the manifests are safe in git,
// and whether they arrive anyway depends on the Application's sync policy.
func syncErr(app Application, err error) error {
	fix := "the change is committed; sync the Application manually (argocd app sync " + app.Name + ") " +
		"and check the kelson token's 'applications, sync' permission in the Argo CD RBAC policy"
	if app.AutoSync {
		fix = "the change is committed and Argo will still pick it up at its poll interval because auto-sync is " +
			"enabled; fix the API server URL or the token's 'applications, sync' permission to restore immediate delivery"
	}
	e := delivery.ApplyFailed("argocd/sync", "",
		fmt.Sprintf("committed, but could not trigger an immediate sync of Application %s/%s", app.Namespace, app.Name),
		fix)
	e.Cause = err.Error()
	return e
}

var _ Syncer = NoopSyncer{}
