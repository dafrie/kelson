package renderer

import (
	"reflect"
	"testing"
)

// TestRenderDeterministic guards the core purity property from ADR-0001: the
// same inputs must always produce byte-identical output. This is the seed of
// the golden-file harness (issue #27) and the CI purity gate (issue #20).
func TestRenderDeterministic(t *testing.T) {
	const n = 25
	first := Render(Spec{})
	for i := 0; i < n; i++ {
		if got := Render(Spec{}); !reflect.DeepEqual(first, got) {
			t.Fatalf("render %d differed from first render: %#v vs %#v", i, first, got)
		}
	}
}

// TestRenderIsPureFunction strengthens the determinism guarantee: rendering
// must depend only on the Spec argument, not on prior calls or incidental
// package state. Two independently-created but equal Spec values must render
// to identical manifests.
func TestRenderIsPureFunction(t *testing.T) {
	a, b := Spec{}, Spec{}
	if gotA, gotB := Render(a), Render(b); !reflect.DeepEqual(gotA, gotB) {
		t.Fatalf("equal specs produced different output: %#v vs %#v", gotA, gotB)
	}
}

// TestRenderProducesKnownEmptyOutput documents the current M0 contract: an
// empty Spec renders to an empty manifest list. This gives the determinism
// tests an observable value and will evolve into golden-file assertions once
// the spec model is implemented (issue #27).
func TestRenderProducesKnownEmptyOutput(t *testing.T) {
	got := Render(Spec{})
	if got == nil {
		t.Fatalf("Render returned nil; want empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("Render returned %d manifests; want 0 for empty spec", len(got))
	}
}
