package redact

import (
	"bytes"
	"strings"
	"testing"
)

const sentinelValue = "s3nt1nel-VALUE-do-not-print-9f2c"

func TestDocumentRedactsSecretValuesKeepsKeys(t *testing.T) {
	doc := []byte("apiVersion: v1\n" +
		"kind: Secret\n" +
		"metadata:\n" +
		"  name: checkout-db\n" +
		"type: Opaque\n" +
		"data:\n" +
		"  DATABASE_URL: " + sentinelValue + "\n" +
		"  API_KEY: " + sentinelValue + "\n" +
		"stringData:\n" +
		"  PLAIN: " + sentinelValue + "\n")

	out, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	got := string(out)
	if strings.Contains(got, sentinelValue) {
		t.Fatalf("the secret value survived redaction:\n%s", got)
	}
	for _, key := range []string{"DATABASE_URL", "API_KEY", "PLAIN", "checkout-db", "Opaque"} {
		if !strings.Contains(got, key) {
			t.Errorf("redaction dropped %q, which is a reference and must stay:\n%s", key, got)
		}
	}
	if n := strings.Count(got, Sentinel); n != 3 {
		t.Errorf("want 3 redacted values, got %d:\n%s", n, got)
	}
}

func TestDocumentIsOrderPreservingAndDeterministic(t *testing.T) {
	doc := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  ZED: aaaaaaaa\n  ALPHA: bbbbbbbb\n  mid: cccccccc\n")
	first, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	second, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("Document is not deterministic:\n%s\n---\n%s", first, second)
	}
	keys := []string{"ZED", "ALPHA", "mid"}
	at := make([]int, len(keys))
	for i, k := range keys {
		at[i] = strings.Index(string(first), k)
		if at[i] < 0 {
			t.Fatalf("key %q missing:\n%s", k, first)
		}
	}
	if at[0] >= at[1] || at[1] >= at[2] {
		t.Errorf("authored key order was not preserved:\n%s", first)
	}
}

func TestDocumentLeavesNonSecretsByteIdentical(t *testing.T) {
	doc := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 2\n")
	out, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if !bytes.Equal(out, doc) {
		t.Fatalf("a non-Secret document was rewritten:\nwant %q\ngot  %q", doc, out)
	}
}

// A ConfigMap holds configuration, not credentials; blanking it would break a
// diff that legitimately shows what changed.
func TestDocumentLeavesConfigMapDataAlone(t *testing.T) {
	doc := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\ndata:\n  LOG_LEVEL: debug\n")
	out, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if !bytes.Contains(out, []byte("debug")) {
		t.Fatalf("ConfigMap data was redacted:\n%s", out)
	}
}

// A CRD in another API group that happens to be named Secret is not a
// Kubernetes Secret, and emptying it out would hide a real change.
func TestDocumentIgnoresForeignSecretKind(t *testing.T) {
	doc := []byte("apiVersion: example.com/v1\nkind: Secret\nmetadata:\n  name: x\ndata:\n  field: visible-value\n")
	out, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if !bytes.Contains(out, []byte("visible-value")) {
		t.Fatalf("a foreign kind named Secret was redacted:\n%s", out)
	}
}

func TestDocumentFindsSecretsNestedInAList(t *testing.T) {
	doc := []byte("apiVersion: v1\nkind: List\nitems:\n" +
		"  - apiVersion: v1\n    kind: Secret\n    metadata:\n      name: a\n    data:\n      K: " + sentinelValue + "\n" +
		"  - apiVersion: v1\n    kind: ConfigMap\n    metadata:\n      name: b\n    data:\n      K: keepme-visible\n")
	out, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if bytes.Contains(out, []byte(sentinelValue)) {
		t.Fatalf("a nested Secret was not redacted:\n%s", out)
	}
	if !bytes.Contains(out, []byte("keepme-visible")) {
		t.Fatalf("the nested ConfigMap was redacted:\n%s", out)
	}
}

func TestDocumentAcceptsJSON(t *testing.T) {
	doc := []byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"K":"` + sentinelValue + `"}}`)
	out, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if bytes.Contains(out, []byte(sentinelValue)) {
		t.Fatalf("JSON input was not redacted:\n%s", out)
	}
}

func TestDocumentPassesUnparseableBytesThrough(t *testing.T) {
	doc := []byte("\tnot: [valid\n\t  yaml")
	out, err := Document(doc)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if !bytes.Equal(out, doc) {
		t.Fatalf("unparseable input was rewritten: %q", out)
	}
}

func TestDocumentsPreservesOrder(t *testing.T) {
	docs := [][]byte{
		[]byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: one\ndata:\n  K: " + sentinelValue + "\n"),
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: two\ndata:\n  K: v\n"),
	}
	out, err := Documents(docs)
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 documents, got %d", len(out))
	}
	if bytes.Contains(out[0], []byte(sentinelValue)) {
		t.Errorf("first document not redacted:\n%s", out[0])
	}
	if !bytes.Contains(out[1], []byte("name: two")) {
		t.Errorf("document order changed:\n%s", out[1])
	}
}

func TestValue(t *testing.T) {
	tests := []struct {
		name string
		kind string
		path string
		in   any
		want any
	}{
		{"leaf under data", "Secret", "data.API_KEY", sentinelValue, Sentinel},
		{"leaf under stringData", "Secret", "stringData.API_KEY", sentinelValue, Sentinel},
		{"dotted key under data", "Secret", "data..dockerconfigjson", sentinelValue, Sentinel},
		{"non-secret kind", "ConfigMap", "data.LOG_LEVEL", "debug", "debug"},
		{"unrelated path on a Secret", "Secret", "metadata.name", "checkout", "checkout"},
		{"type field on a Secret", "Secret", "type", "Opaque", "Opaque"},
		{"nil stays nil", "Secret", "data.K", nil, nil},
		{
			"last-applied annotation embeds the whole object",
			"Secret",
			"metadata.annotations." + lastAppliedKey,
			`{"data":{"K":"` + sentinelValue + `"}}`,
			Sentinel,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Value(tc.kind, tc.path, tc.in); got != tc.want {
				t.Errorf("Value(%q, %q, %v) = %v, want %v", tc.kind, tc.path, tc.in, got, tc.want)
			}
		})
	}
}

// An L2 readback emits the whole annotations mapping when the live object has
// annotations the render does not, so the apply annotation arrives as a value
// inside a map rather than at its own path.
func TestValueRedactsTheApplyAnnotationInsideAMapping(t *testing.T) {
	in := map[string]any{
		lastAppliedKey:       `{"data":{"K":"` + sentinelValue + `"}}`,
		"kelson.dev/project": "shop",
	}
	got, ok := Value("Secret", "metadata.annotations", in).(map[string]any)
	if !ok {
		t.Fatalf("want a map back, got %T", Value("Secret", "metadata.annotations", in))
	}
	if got[lastAppliedKey] != Sentinel {
		t.Errorf("the apply annotation kept its embedded copy of the object: %v", got[lastAppliedKey])
	}
	if got["kelson.dev/project"] != "shop" {
		t.Errorf("an unrelated annotation was redacted: %v", got["kelson.dev/project"])
	}
	if in[lastAppliedKey] == Sentinel {
		t.Error("Value mutated the caller's map")
	}
}

func TestValueRedactsTheApplyAnnotationNestedUnderMetadata(t *testing.T) {
	in := map[string]any{"annotations": map[string]any{lastAppliedKey: sentinelValue}}
	got, ok := Value("Secret", "metadata", in).(map[string]any)
	if !ok {
		t.Fatalf("want a map back, got %T", Value("Secret", "metadata", in))
	}
	ann, _ := got["annotations"].(map[string]any)
	if ann[lastAppliedKey] != Sentinel {
		t.Errorf("a nested apply annotation survived: %v", ann[lastAppliedKey])
	}
}

func TestValueRedactsAWholeAddedDataMapping(t *testing.T) {
	in := map[string]any{"API_KEY": sentinelValue, "DATABASE_URL": sentinelValue}
	got, ok := Value("Secret", "data", in).(map[string]any)
	if !ok {
		t.Fatalf("want a map back, got %T", Value("Secret", "data", in))
	}
	if len(got) != 2 {
		t.Fatalf("want both keys kept, got %v", got)
	}
	for k, v := range got {
		if v != Sentinel {
			t.Errorf("key %q kept its value %v", k, v)
		}
	}
	if in["API_KEY"] != sentinelValue {
		t.Error("Value mutated the caller's map")
	}
}
