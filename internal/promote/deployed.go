package promote

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/delivery"
)

// The label the renderer stamps on every resource that belongs to one
// component (internal/renderer, docs/architecture.md "Provenance"). It is the
// correlation a promotion reads: the manifests are the recorded output of one
// revision and carry no spec, so the component a pod template belongs to is
// whatever this label says it is.
//
// Data components deliberately carry no such label — a managed data service is
// not owned by a component in this sense (internal/renderer/dataservice.go) —
// which is why they fall out of [Deployed] without a special case.
const labelComponent = "kelson.dev/component"

// Deployed extracts the image each workload component runs, from the rendered
// manifests a revision recorded.
//
// This is the deployed truth a promotion is built on: the bytes that were
// applied, read back, rather than a re-render of the spec that produced them.
// Only pod-bearing kinds are read — Deployment and CronJob — because they are
// the only ones that carry an image at all; every other rendered resource is
// skipped silently, which is correct rather than lossy.
//
// A component the manifests do not mention is simply absent from the result.
// [Plan] turns that absence into a skip with a reason; guessing an image here
// would be the one thing a promotion must never do.
func Deployed(manifests []delivery.Manifest) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range manifests {
		pod, component, err := podTemplate(m)
		if err != nil {
			return nil, err
		}
		if pod == nil || component == "" {
			continue
		}
		image, ok := containerImage(pod, component)
		if !ok {
			continue
		}
		out[component] = image
	}
	return out, nil
}

// podTemplate returns the pod spec of a recorded manifest and the component it
// belongs to, or (nil, "") for a manifest that carries neither.
func podTemplate(m delivery.Manifest) (*yaml.Node, string, error) {
	switch m.Kind {
	case "Deployment", "CronJob":
	default:
		return nil, "", nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(m.YAML, &doc); err != nil {
		return nil, "", fmt.Errorf("promote: reading the recorded %s/%s: %w", m.Kind, m.Name, err)
	}
	root := documentBody(&doc)
	if root == nil {
		return nil, "", nil
	}

	component := scalarAt(root, "metadata", "labels", labelComponent)
	if component == "" {
		// A rendered resource with no component label belongs to no component:
		// the Namespace, and every resource a managed data service renders.
		return nil, "", nil
	}

	var template *yaml.Node
	if m.Kind == "Deployment" {
		template = mapValue(root, "spec", "template", "spec")
	} else {
		template = mapValue(root, "spec", "jobTemplate", "spec", "template", "spec")
	}
	return template, component, nil
}

// containerImage picks the image of the container that is the component.
//
// The renderer names a component's container after the component, so the match
// is exact and does not depend on ordering. A single unnamed-match container is
// accepted as a fallback so a recorded revision from an overlay-patched
// manifest — where the container may have been renamed — still promotes rather
// than silently skipping; more than one container with no name match is
// ambiguous, and ambiguity is a skip.
func containerImage(pod *yaml.Node, component string) (string, bool) {
	containers := mapValue(pod, "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return "", false
	}
	var only string
	var count int
	for _, c := range containers.Content {
		if c.Kind != yaml.MappingNode {
			continue
		}
		image := scalarAt(c, "image")
		if image == "" {
			continue
		}
		if scalarAt(c, "name") == component {
			return image, true
		}
		count++
		only = image
	}
	if count == 1 {
		return only, true
	}
	return "", false
}

// documentBody unwraps a decoded document to its root mapping.
func documentBody(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// mapValue walks a chain of mapping keys and returns the value node, or nil
// when any step is missing or is not a mapping.
func mapValue(n *yaml.Node, path ...string) *yaml.Node {
	for _, key := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		n = childValue(n, key)
	}
	return n
}

// childValue returns the value node for one key of a mapping.
func childValue(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// scalarAt walks a key chain and returns the scalar at the end, or "".
func scalarAt(n *yaml.Node, path ...string) string {
	v := mapValue(n, path...)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}
