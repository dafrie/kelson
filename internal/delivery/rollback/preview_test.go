package rollback

import (
	"context"
	"testing"

	"github.com/dafrie/kelson/internal/diff"
)

// TestPreviewRevisionEndToEnd drives the whole chain a rollback preview needs:
// recorded history bytes → document diff → irreversibility findings. It is the
// piece that was missing while diff.Between only accepted renderer output.
//
// Direction is the thing most easily got backwards: the before side is current
// state and the after side is the target revision, so rolling back from
// revision 3 to revision 1 must report the change that *applying the rollback*
// makes.
func TestPreviewRevisionEndToEnd(t *testing.T) {
	store := openStoreT(t, 20)
	appendT(t, store, "rev-00000001", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000002", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000003", "apps/v1", "Deployment", "web")

	src := &DirectSource{Store: store, Project: "shop", Environment: "production"}
	d, findings, err := PreviewRevision(context.Background(), src, "shop", "production", "rev-00000001")
	if err != nil {
		t.Fatalf("PreviewRevision: %v", err)
	}
	if d.Project != "shop" || d.Environment != "production" {
		t.Errorf("identity = %s/%s", d.Project, d.Environment)
	}
	if len(d.Resources) != 1 {
		t.Fatalf("resources = %+v, want the one Deployment", d.Resources)
	}
	r := d.Resources[0]
	if r.Kind != "Deployment" || r.Name != "web" {
		t.Errorf("identity not read from the recorded document: %s/%s", r.Kind, r.Name)
	}
	if r.Op != diff.OpModified {
		t.Errorf("op = %q, want modified", r.Op)
	}
	// appendT varies spec.replicas per revision, so rolling back to rev-1 must
	// report that field moving from the current value back to the old one.
	if len(r.Fields) != 1 {
		t.Fatalf("fields = %+v, want exactly spec.replicas", r.Fields)
	}
	if got := r.Fields[0]; got.Before != "rev-00000003" || got.After != "rev-00000001" {
		t.Errorf("field = %v -> %v, want rev-3 -> rev-1 (before is current state, after is the target)",
			got.Before, got.After)
	}

	// Even a clean rollback carries the migrations caveat: kelson runs no
	// migrations (#104), so it must say the rollback is unverified on that
	// axis rather than let silence imply it is safe.
	var caveat bool
	for _, f := range findings {
		if f.Cause == CauseMigrations {
			caveat = true
		}
	}
	if !caveat {
		t.Errorf("findings = %+v, want the migrations caveat present even on a clean rollback", findings)
	}
}

// TestPreviewRevisionPrunedRevision: a revision aged out of retention must be
// reported as such, not as an empty or clean preview — a rollback target that
// silently resolves to nothing is the worst possible answer here.
func TestPreviewRevisionPrunedRevision(t *testing.T) {
	store := openStoreT(t, 1)
	appendT(t, store, "rev-00000001", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000002", "apps/v1", "Deployment", "web")

	src := &DirectSource{Store: store, Project: "shop", Environment: "production"}
	_, _, err := PreviewRevision(context.Background(), src, "shop", "production", "rev-00000001")
	if err == nil {
		t.Fatal("PreviewRevision succeeded for a pruned revision")
	}
}

// TestPreviewRevisionNilSource fails loudly rather than panicking.
func TestPreviewRevisionNilSource(t *testing.T) {
	if _, _, err := PreviewRevision(context.Background(), nil, "shop", "production", "rev-1"); err == nil {
		t.Fatal("PreviewRevision succeeded with a nil Source")
	}
}
