package diff_test

import (
	"testing"

	"github.com/dafrie/kelson/internal/diff"
)

const (
	prevDeploy = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  template:
    spec:
      containers:
        - name: web
          image: ghcr.io/acme/web:v1
`
	curDeploy = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  template:
    spec:
      containers:
        - name: web
          image: ghcr.io/acme/web:v2
`
	svc = `apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
spec:
  ports:
    - port: 80
`
)

// TestBetweenDocumentsReportsFieldChange: a rollback preview compares recorded
// bytes, so the bytes entry point must produce the same field-level detail the
// manifest entry point does — identity read from the document itself.
func TestBetweenDocumentsReportsFieldChange(t *testing.T) {
	d, err := diff.BetweenDocuments("shop", "production",
		[][]byte{[]byte(prevDeploy)}, [][]byte{[]byte(curDeploy)}, nil)
	if err != nil {
		t.Fatalf("BetweenDocuments: %v", err)
	}
	if len(d.Resources) != 1 {
		t.Fatalf("resources = %d, want 1: %+v", len(d.Resources), d.Resources)
	}
	r := d.Resources[0]
	if r.Kind != "Deployment" || r.Name != "web" || r.Namespace != "shop" {
		t.Errorf("identity not read from the document: %s/%s in %q", r.Kind, r.Name, r.Namespace)
	}
	if r.Op != diff.OpModified {
		t.Errorf("op = %q, want modified", r.Op)
	}
	if len(r.Fields) != 1 {
		t.Fatalf("fields = %d, want exactly the image: %+v", len(r.Fields), r.Fields)
	}
	if r.Risk != diff.RiskRestart {
		t.Errorf("risk = %q, want restart-required (an image change rolls pods)", r.Risk)
	}
}

// TestBetweenDocumentsAddRemove: a resource present only on one side is an
// addition or a removal, and a removal is disruptive.
func TestBetweenDocumentsAddRemove(t *testing.T) {
	d, err := diff.BetweenDocuments("shop", "production",
		[][]byte{[]byte(svc)}, [][]byte{[]byte(prevDeploy)}, nil)
	if err != nil {
		t.Fatalf("BetweenDocuments: %v", err)
	}
	var ops []diff.Op
	for _, r := range d.Resources {
		ops = append(ops, r.Op)
	}
	if len(ops) != 2 {
		t.Fatalf("resources = %+v, want one removed and one added", d.Resources)
	}
	if d.Summary.Added != 1 || d.Summary.Removed != 1 {
		t.Errorf("summary = %+v, want 1 added and 1 removed", d.Summary)
	}
	if d.Summary.MaxRisk != diff.RiskDisruptive {
		t.Errorf("max risk = %q, want disruptive (a removal deletes a live resource)", d.Summary.MaxRisk)
	}
}

// TestBetweenDocumentsSkipsEmptyDocuments: a recorded multi-document stream can
// carry a trailing separator, which must not fail the whole preview.
func TestBetweenDocumentsSkipsEmptyDocuments(t *testing.T) {
	d, err := diff.BetweenDocuments("shop", "production",
		[][]byte{[]byte(prevDeploy), []byte("\n"), []byte("---\n")},
		[][]byte{[]byte(prevDeploy)}, nil)
	if err != nil {
		t.Fatalf("BetweenDocuments: %v", err)
	}
	if len(d.Resources) != 0 {
		t.Errorf("identical input produced changes: %+v", d.Resources)
	}
}
