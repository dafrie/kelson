package explain

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/redact"
)

// The manifest reader: recorded rendered bytes in, the handful of facts a
// diagnosis needs out (issue #77, ADR-0023).
//
// It reads the *recorded* manifests of a revision rather than re-rendering the
// spec, for the reason rollback replays recorded bytes (#38): a re-render
// answers "what would we deploy now", and every question here is about what was
// actually applied. Two recorded revisions therefore diff honestly even when
// the spec has moved on since.
//
// Only the fields a cause needs are extracted — container names, images,
// environment variables and HTTP probes. Everything else in a workload is
// skipped on purpose: this is a reader for a diagnosis, not a second manifest
// model, and internal/diff already owns generic manifest comparison.

// workloadKinds are the kinds whose pod template this reader understands, each
// with the path prefix its containers live under. A kind absent from the map is
// skipped rather than guessed at.
var workloadKinds = map[string][]string{
	"Deployment":  {"spec", "template", "spec"},
	"StatefulSet": {"spec", "template", "spec"},
	"DaemonSet":   {"spec", "template", "spec"},
	"Job":         {"spec", "template", "spec"},
	"CronJob":     {"spec", "jobTemplate", "spec", "template", "spec"},
}

// workload is one manifest's pod template, reduced.
type workload struct {
	Kind       string
	Name       string
	Namespace  string
	Containers []container
}

// ref is the workload identity a cause and an EnvChange carry, e.g.
// "Deployment/web". The namespace is left off because a diff between two
// revisions of the same environment is always within one namespace and the
// extra segment costs a reader more than it tells them.
func (w workload) ref() string { return w.Kind + "/" + w.Name }

// container is one container of a pod template, reduced to the fields a cause
// is built from.
type container struct {
	Name  string
	Image string
	// Index is the container's position in its list, so a field reference reads
	// as spec.template.spec.containers[0].env[NAME] — the path convention
	// internal/diff already uses.
	Index int
	// Init marks an initContainer, whose field path differs.
	Init bool
	// Env is ordered by name so a diff never depends on manifest order.
	Env    []envVar
	Probes []probe
	// path is the pod-spec path prefix this container's fields hang off.
	path string
}

// envVar is one environment variable as the manifest holds it.
type envVar struct {
	Name string
	// Value is the literal, scrubbed. Empty when the variable is a reference.
	Value string
	// Ref is the reference form: "secret <name>/<key>", "configmap <name>/<key>"
	// or "field <path>". Empty when the variable is a literal.
	Ref string
	// SecretName and SecretKey are set for a secretKeyRef, so a cause can name
	// the Secret and key a container is waiting on without re-parsing Ref.
	SecretName string
	SecretKey  string
}

// form is the variable's rendered form: what a diff shows on either side.
func (v envVar) form() string {
	if v.Ref != "" {
		return v.Ref
	}
	return v.Value
}

// probe is one HTTP probe, which is the only kind kelson renders
// (internal/renderer/workload.go) and therefore the only kind a probe cause can
// name a path and port for.
type probe struct {
	// Kind is "readiness" or "liveness".
	Kind string
	Path string
	Port string
}

func (p probe) String() string {
	return fmt.Sprintf("%s probe GET %s on port %s", p.Kind, p.Path, p.Port)
}

// readWorkloads decodes the workload manifests of a recorded revision. A
// manifest that does not decode is skipped rather than failing the read: an
// explanation built from three of four workloads is worth more than no
// explanation, and the renderer — not this reader — owns manifest validity.
func readWorkloads(manifests []delivery.Manifest) []workload {
	var out []workload
	for _, m := range manifests {
		prefix, ok := workloadKinds[m.Kind]
		if !ok {
			continue
		}
		var doc map[string]any
		if err := yaml.Unmarshal(m.YAML, &doc); err != nil || doc == nil {
			continue
		}
		podSpec, ok := nestedMap(doc, prefix...)
		if !ok {
			continue
		}
		w := workload{Kind: m.Kind, Name: m.Name, Namespace: m.Namespace}
		w.Containers = append(w.Containers, readContainers(podSpec, prefix, "containers")...)
		w.Containers = append(w.Containers, readContainers(podSpec, prefix, "initContainers")...)
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ref() < out[j].ref() })
	return out
}

// readContainers reads one container list off a pod spec.
func readContainers(podSpec map[string]any, prefix []string, field string) []container {
	raw, _ := podSpec[field].([]any)
	out := make([]container, 0, len(raw))
	for i, item := range raw {
		cm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := cm["name"].(string)
		image, _ := cm["image"].(string)
		c := container{
			Name:  name,
			Image: image,
			Index: i,
			Init:  field == "initContainers",
			Env:   readEnv(cm),
			path:  fmt.Sprintf("%s.%s[%d]", strings.Join(prefix, "."), field, i),
		}
		c.Probes = readProbes(cm)
		out = append(out, c)
	}
	return out
}

// envPath is the field reference for one variable of this container, in
// internal/diff's own path convention (walk.go): container lists are indexed,
// env entries are keyed by name.
func (c container) envPath(name string) string {
	return fmt.Sprintf("%s.env[%s]", c.path, name)
}

// readEnv reads a container's env list into name order.
//
// Every literal passes through redact.Scrub: a manifest cannot hold a secret
// *value* by construction (ADR-0009 rejects one at validation and the renderer
// emits references), but this is a display path and the process-wide scrubber
// is what makes that a property rather than an argument (#117).
func readEnv(cm map[string]any) []envVar {
	raw, _ := cm["env"].([]any)
	out := make([]envVar, 0, len(raw))
	for _, item := range raw {
		em, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := em["name"].(string)
		if name == "" {
			continue
		}
		v := envVar{Name: name}
		if literal, ok := em["value"].(string); ok {
			v.Value = redact.Scrub(literal)
		}
		if from, ok := em["valueFrom"].(map[string]any); ok {
			v.Ref, v.SecretName, v.SecretKey = readValueFrom(from)
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readValueFrom renders a reference form. It names the Secret and key for a
// secretKeyRef because "which Secret is this container waiting on" is the
// question a CreateContainerConfigError makes someone ask (ADR-0018).
func readValueFrom(from map[string]any) (form, secretName, secretKey string) {
	if ref, ok := from["secretKeyRef"].(map[string]any); ok {
		name, _ := ref["name"].(string)
		key, _ := ref["key"].(string)
		return "secret " + name + "/" + key, name, key
	}
	if ref, ok := from["configMapKeyRef"].(map[string]any); ok {
		name, _ := ref["name"].(string)
		key, _ := ref["key"].(string)
		return "configmap " + name + "/" + key, "", ""
	}
	if ref, ok := from["fieldRef"].(map[string]any); ok {
		path, _ := ref["fieldPath"].(string)
		return "field " + path, "", ""
	}
	return "valueFrom", "", ""
}

// readProbes reads the HTTP probes a probe cause names.
func readProbes(cm map[string]any) []probe {
	var out []probe
	for _, p := range []struct{ field, kind string }{
		{"readinessProbe", "readiness"},
		{"livenessProbe", "liveness"},
	} {
		pm, ok := cm[p.field].(map[string]any)
		if !ok {
			continue
		}
		get, ok := pm["httpGet"].(map[string]any)
		if !ok {
			continue
		}
		path, _ := get["path"].(string)
		out = append(out, probe{Kind: p.kind, Path: path, Port: scalar(get["port"])})
	}
	return out
}

// findWorkload returns the workload a verdict is about, by name. Verdicts carry
// "Kind/namespace/name" and the recorded manifests carry the same name, which
// is the correlation — the rendered set IS what kelson deployed.
func findWorkload(workloads []workload, name string) (workload, bool) {
	for _, w := range workloads {
		if w.Name == name {
			return w, true
		}
	}
	return workload{}, false
}

func (w workload) container(name string) (container, bool) {
	for _, c := range w.Containers {
		if c.Name == name {
			return c, true
		}
	}
	return container{}, false
}

// nestedMap walks a decoded document down a path of map keys.
func nestedMap(doc map[string]any, path ...string) (map[string]any, bool) {
	current := doc
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

// scalar renders a YAML scalar for display. Ports are the reason it exists: a
// port is an int in one manifest and a named string in another, and a probe
// cause must print either.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}
