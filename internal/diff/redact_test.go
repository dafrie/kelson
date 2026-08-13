package diff_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/redact"
)

// The sentinel property for the rendered (L1) diff (issue #117).
//
// The spec cannot hold a secret literal (ADR-0009), but an overlay may inject
// any manifest at all — including a Secret — and everything downstream of the
// renderer would then be handling real credential bytes. These fixtures are
// what such an overlay produces.

const sentinel = "s3ntinel-VALUE-must-never-print-8a41"

const (
	prevSecret = `apiVersion: v1
kind: Secret
metadata:
  name: checkout-db
  namespace: shop
type: Opaque
data:
  DATABASE_URL: b2xkLXZhbHVl
`
	curSecret = `apiVersion: v1
kind: Secret
metadata:
  name: checkout-db
  namespace: shop
type: Opaque
data:
  DATABASE_URL: ` + sentinel + `
  NEW_KEY: ` + sentinel + `
`
)

// TestDiffNeverPrintsASecretValue: the value appears in no representation of
// the diff — not the typed fields, not the JSON agents parse, not the terminal
// rendering a human reads.
func TestDiffNeverPrintsASecretValue(t *testing.T) {
	d, err := diff.BetweenDocuments("shop", "production",
		[][]byte{[]byte(prevSecret)}, [][]byte{[]byte(curSecret)}, nil)
	if err != nil {
		t.Fatalf("BetweenDocuments: %v", err)
	}

	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Errorf("the secret value reached the JSON diff:\n%s", encoded)
	}

	var term bytes.Buffer
	if err := diff.Write(&term, d, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Contains(term.String(), sentinel) {
		t.Errorf("the secret value reached the terminal diff:\n%s", term.String())
	}
	if !strings.Contains(term.String(), redact.Sentinel) {
		t.Errorf("nothing was marked as redacted, so the diff is silently incomplete:\n%s", term.String())
	}
}

// The keys are the reference and a reference is exactly what ADR-0009 says a
// spec may carry, so a diff that hid them would be useless: "a secretKeyRef is
// fine to show, a value never is" (issue #117).
func TestDiffKeepsSecretKeysAndIdentity(t *testing.T) {
	d, err := diff.BetweenDocuments("shop", "production",
		[][]byte{[]byte(prevSecret)}, [][]byte{[]byte(curSecret)}, nil)
	if err != nil {
		t.Fatalf("BetweenDocuments: %v", err)
	}
	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	for _, want := range []string{"checkout-db", "DATABASE_URL", "NEW_KEY"} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Errorf("the diff dropped %q, which is a reference and must stay:\n%s", want, encoded)
		}
	}
}

// A Secret added or removed wholesale takes its whole data mapping through the
// diff as one value, which is the shape that would leak every key at once.
func TestDiffRedactsAWholeAddedDataMapping(t *testing.T) {
	const before = `apiVersion: v1
kind: Secret
metadata:
  name: checkout-db
  namespace: shop
type: Opaque
`
	d, err := diff.BetweenDocuments("shop", "production",
		[][]byte{[]byte(before)}, [][]byte{[]byte(curSecret)}, nil)
	if err != nil {
		t.Fatalf("BetweenDocuments: %v", err)
	}
	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Errorf("an added data mapping carried its values through:\n%s", encoded)
	}
	if !bytes.Contains(encoded, []byte("DATABASE_URL")) {
		t.Errorf("the added keys were dropped along with the values:\n%s", encoded)
	}
}

// A ConfigMap is configuration, and a diff that blanked it would be lying about
// what changed. Redaction is selected by the resource's own kind, never by what
// the value looks like.
func TestDiffLeavesConfigMapValuesVisible(t *testing.T) {
	const prev = `apiVersion: v1
kind: ConfigMap
metadata:
  name: web
  namespace: shop
data:
  LOG_LEVEL: info
`
	const cur = `apiVersion: v1
kind: ConfigMap
metadata:
  name: web
  namespace: shop
data:
  LOG_LEVEL: debug
`
	d, err := diff.BetweenDocuments("shop", "production",
		[][]byte{[]byte(prev)}, [][]byte{[]byte(cur)}, nil)
	if err != nil {
		t.Fatalf("BetweenDocuments: %v", err)
	}
	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if !bytes.Contains(encoded, []byte("debug")) {
		t.Errorf("a ConfigMap value was redacted:\n%s", encoded)
	}
}
