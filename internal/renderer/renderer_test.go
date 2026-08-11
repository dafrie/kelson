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
