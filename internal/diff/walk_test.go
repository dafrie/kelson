package diff

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// unit tests for the provenance-noise policy and field-risk classification.
// The golden fixtures in testdata/diff exercise the engine end-to-end; these
// pin the two transition points where "exactly that one field" is decided.

func mapping(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Content = append(n.Content, scalar(kv[i].(string)))
		switch v := kv[i+1].(type) {
		case *yaml.Node:
			n.Content = append(n.Content, v)
		case string:
			n.Content = append(n.Content, scalar(v))
		default:
			panic("mapping: unsupported value")
		}
	}
	return n
}

func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// TestRendererVersionIsNoise: two renders whose only difference is the
// kelson.dev/renderer-version annotation must produce no field diff — a
// renderer upgrade is not a spec change (issue #42).
func TestRendererVersionIsNoise(t *testing.T) {
	prev := mapping("metadata", hasAnnotations("kelson.dev/renderer-version", "0.1.0"),
		"spec", mapping("replicas", "2"))
	cur := mapping("metadata", hasAnnotations("kelson.dev/renderer-version", "0.2.0"),
		"spec", mapping("replicas", "2"))

	var fields []FieldDiff
	origin := func(ResourceRef, string) Origin { return OriginSpec }
	ref := ResourceRef{Kind: "Deployment", Name: "web"}
	diffMappings(&fields, ref, "Deployment", prev, cur, "", origin)
	if len(fields) != 0 {
		t.Fatalf("renderer-version bump produced a spurious diff: %+v", fields)
	}
}

// TestSpecHashIsSignal: spec-hash is suppressed as bookkeeping, but it must
// never mask a real field change — when the underlying spec field also
// changed, that field is reported.
func TestSpecHashIsSignal(t *testing.T) {
	prev := mapping("metadata", hasAnnotations("kelson.dev/spec-hash", "sha256:aaa"),
		"spec", mapping("paused", "false"))
	cur := mapping("metadata", hasAnnotations("kelson.dev/spec-hash", "sha256:bbb"),
		"spec", mapping("paused", "true"))

	var fields []FieldDiff
	origin := func(ResourceRef, string) Origin { return OriginSpec }
	ref := ResourceRef{Kind: "Deployment", Name: "web"}
	diffMappings(&fields, ref, "Deployment", prev, cur, "", origin)
	if len(fields) != 1 {
		t.Fatalf("expected exactly the real field change, got %d fields: %+v", len(fields), fields)
	}
	if fields[0].Path != "spec.paused" {
		t.Fatalf("expected spec.paused, got %q", fields[0].Path)
	}
}

// TestSpecHashAloneIsSilent: when ONLY the spec-hash annotation differs (no
// accompanying field edit), nothing is reported either.
func TestSpecHashAloneIsSilent(t *testing.T) {
	prev := mapping("metadata", hasAnnotations("kelson.dev/spec-hash", "sha256:aaa"),
		"spec", mapping("paused", "false"))
	cur := mapping("metadata", hasAnnotations("kelson.dev/spec-hash", "sha256:bbb"),
		"spec", mapping("paused", "false"))

	var fields []FieldDiff
	origin := func(ResourceRef, string) Origin { return OriginSpec }
	ref := ResourceRef{Kind: "Deployment", Name: "web"}
	diffMappings(&fields, ref, "Deployment", prev, cur, "", origin)
	if len(fields) != 0 {
		t.Fatalf("spec-hash alone must be silent, got %d fields: %+v", len(fields), fields)
	}
}

// TestFieldRisk classifies the Kubernetes semantics the blast radius depends
// on (issue #42).
func TestFieldRisk(t *testing.T) {
	cases := []struct {
		kind, path string
		want       Risk
	}{
		{"Deployment", "spec.template.spec.containers[0].env[LOG_LEVEL].value", RiskRestart},
		{"Deployment", "spec.template.metadata.labels.foo", RiskRestart},
		{"CronJob", "spec.jobTemplate.spec.template.spec.containers[0].env[A].value", RiskRestart},
		{"Deployment", "spec.selector.matchLabels.foo", RiskDisruptive},
		{"StatefulSet", "spec.volumeClaimTemplates[0].spec.storageClassName", RiskDisruptive},
		{"Service", "spec.clusterIP", RiskDisruptive},
		{"Service", "spec.selector.app", RiskDisruptive},
		{"Deployment", "spec.replicas", RiskAdditive},
		{"HPA", "spec.maxReplicas", RiskAdditive},
		{"Ingress", "spec.rules[0].host", RiskAdditive},
	}
	for _, c := range cases {
		if got := fieldRisk(c.kind, c.path); got != c.want {
			t.Errorf("fieldRisk(%q, %q) = %q, want %q", c.kind, c.path, got, c.want)
		}
	}
}

func hasAnnotations(kv ...string) *yaml.Node {
	inner := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		inner.Content = append(inner.Content, scalar(kv[i]), scalar(kv[i+1]))
	}
	return mapping("annotations", inner)
}
