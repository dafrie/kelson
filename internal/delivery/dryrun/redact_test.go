package dryrun

import (
	"bytes"
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/redact"
)

// The sentinel property for the server-side (L2) preview (issue #117).
//
// L2 is the only preview level that reads live objects back, so it is the only
// one that can pull a Secret's *stored* content into a diff for a spec that
// never carried a value. That makes it the sharpest version of the leak: the
// user did everything ADR-0009 asks and the preview prints the credential
// anyway.

const liveSentinel = "l1ve-cluster-SENTINEL-never-print-77b3"

func liveSecret() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      "checkout-db",
			"namespace": tNS,
			"annotations": map[string]any{
				// kubectl's apply annotation carries a whole copy of the object,
				// data included. It is the leak nobody looks for.
				"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"DATABASE_URL":"` + liveSentinel + `"}}`,
			},
		},
		"type": "Opaque",
		"data": map[string]any{"DATABASE_URL": liveSentinel},
	}}
}

func TestServerPreviewNeverPrintsLiveSecretContent(t *testing.T) {
	c := newCluster()
	c.seedLive(liveSecret())

	desired := manifest(t, "v1", "Secret", "checkout-db", tNS, map[string]any{
		"type": "Opaque",
		"data": map[string]any{"DATABASE_URL": "bmV3LXZhbHVl"},
	})
	c.handle = func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) { return obj, nil }

	d, err := newEngine(t, c).Preview(context.Background(), set(desired))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if d.Level != diff.LevelServer {
		t.Fatalf("level = %s, want %s", d.Level, diff.LevelServer)
	}

	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if bytes.Contains(encoded, []byte(liveSentinel)) {
		t.Errorf("the live Secret's stored value reached the L2 diff:\n%s", encoded)
	}
	if bytes.Contains(encoded, []byte("bmV3LXZhbHVl")) {
		t.Errorf("the submitted Secret's value reached the L2 diff:\n%s", encoded)
	}
	if !bytes.Contains(encoded, []byte("DATABASE_URL")) {
		t.Errorf("the key was dropped; a preview must still say which key changed:\n%s", encoded)
	}
	if !bytes.Contains(encoded, []byte(redact.Sentinel)) {
		t.Errorf("nothing was marked redacted, so the preview is silently incomplete:\n%s", encoded)
	}
}

// The L1 fallback is a second, separate constructor inside this engine, taken
// when the caller lacks dry-run permission. It reads live objects too, so it
// leaks by the same route if it is not redacted with the rest.
func TestDegradedPreviewNeverPrintsLiveSecretContent(t *testing.T) {
	c := newCluster()
	c.seedLive(liveSecret())

	desired := manifest(t, "v1", "Secret", "checkout-db", tNS, map[string]any{
		"type": "Opaque",
		"data": map[string]any{"DATABASE_URL": "bmV3LXZhbHVl"},
	})

	d := newEngine(t, c).degraded(context.Background(), diff.LevelServer, set(desired), apierrors.NewForbidden(
		schema.GroupResource{Resource: "secrets"}, "checkout-db",
		errors.New("secrets is forbidden: User cannot patch resource")))
	if !d.Degraded {
		t.Fatal("the fallback did not mark itself degraded")
	}
	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if bytes.Contains(encoded, []byte(liveSentinel)) {
		t.Errorf("the degraded preview printed live Secret content:\n%s", encoded)
	}
}
