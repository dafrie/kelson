package renderer

import (
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// componentManifests renders one workload component in a fixed resource order:
// ServiceAccount, Service, workload (Deployment|CronJob), HPA, HTTPRoute,
// Certificate, ServiceMonitor. Kinds that do not apply are simply absent.
//
// The ServiceAccount is the one resource every kind gets (ADR-0014 decision D)
// — though a component with a release hook has already had its emitted, ahead
// of the Job whose pod names it (release.go). An agent renders as a worker with
// that identity and nothing else: its tool policy is refused at validation
// until #75, so nothing agent-specific can reach this function.
//
// Everything this function emits stays [StageWorkload], which is the default:
// these are exactly the resources that must not roll until the release hook has
// succeeded.
func componentManifests(
	resolved *model.Resolved,
	c *model.ResolvedComponent,
	profile clusterprofile.ClusterProfile,
	services map[string]boundService,
) ([]Manifest, error) {
	hash, err := specHash(resolved, c)
	if err != nil {
		return nil, Errors{{Code: ErrInternal, Message: err.Error()}}
	}
	env, berrs := envList(c, services)
	if len(berrs) > 0 {
		return nil, berrs
	}
	prov := provenance{
		project:     resolved.Project,
		environment: resolved.Environment.Name,
		component:   c.Name,
		namespace:   resolved.Environment.Namespace,
		specHash:    hash,
	}

	// The ServiceAccount of a component with a release hook has already been
	// emitted, ahead of the Job whose pod names it (release.go). It is moved,
	// never duplicated: two ServiceAccount documents of the same name in one set
	// would be one resource applied twice and pruned once.
	var out []Manifest
	if c.Release == nil {
		out = append(out, serviceAccount(prov))
	}
	if c.Kind == model.ComponentService {
		out = append(out, service(c, prov))
	}
	switch c.Kind {
	case model.ComponentService, model.ComponentWorker, model.ComponentAgent:
		out = append(out, deployment(c, prov, env))
		if hpa, ok := autoscaler(c, prov); ok {
			out = append(out, hpa)
		}
	case model.ComponentCron:
		out = append(out, cronJob(c, prov, env))
	}
	routes, err := routingResources(resolved, c, profile, prov)
	if err != nil {
		return nil, err
	}
	out = append(out, routes...)
	if c.Kind == model.ComponentService && profile.Prometheus != nil {
		out = append(out, serviceMonitor(c, prov))
	}
	return out, nil
}

func serviceAccount(prov provenance) Manifest {
	// Every component gets one, named after it (ADR-0014 decision D). It is
	// metadata-only for now: it exists so a component has an identity to hang
	// policy on — IRSA, image pull, and for an agent the per-agent
	// ServiceAccount the prior art calls the primary blast-radius control —
	// without re-keying pods when that policy lands.
	//
	// Uniform across kinds on purpose. "Services have one, workers share
	// default" is a fact a reader has to learn; "every component has one" is a
	// fact they can derive, and the extra object costs a cluster nothing.
	// Data components are not here at all: CloudNativePG owns the identity its
	// clusters run under (ADR-0005).
	root := mapNode(
		"apiVersion", "v1",
		"kind", "ServiceAccount",
		"metadata", metadataNode(prov),
	)
	return Manifest{APIVersion: "v1", Kind: "ServiceAccount", Name: prov.name(), Namespace: prov.namespace, doc: docNode(root)}
}

func service(app *model.ResolvedComponent, prov provenance) Manifest {
	spec := mapNode(
		"selector", selectorLabels(prov),
		"ports", seqNode(mapNode(
			"name", "http",
			"port", app.Port,
			"targetPort", app.Port,
		)),
	)
	return baseManifest("v1", "Service", prov, spec)
}

func deployment(app *model.ResolvedComponent, prov provenance, env *yaml.Node) Manifest {
	specKV := []any{}
	if !autoscaling(app) {
		// With an HPA the replicas field is owned by the autoscaler; setting
		// both makes apply and autoscaling fight.
		specKV = append(specKV, "replicas", app.Replicas.Min)
	}
	specKV = append(specKV,
		"selector", mapNode("matchLabels", selectorLabels(prov)),
		"template", podTemplate(app, prov, env),
	)
	return baseManifest("apps/v1", "Deployment", prov, mapNode(specKV...))
}

func autoscaling(app *model.ResolvedComponent) bool {
	return app.Replicas.Max > app.Replicas.Min
}

func autoscaler(app *model.ResolvedComponent, prov provenance) (Manifest, bool) {
	if !autoscaling(app) || app.Kind == model.ComponentCron {
		return Manifest{}, false
	}
	spec := mapNode(
		"scaleTargetRef", mapNode(
			"apiVersion", "apps/v1",
			"kind", "Deployment",
			"name", app.Name,
		),
		"minReplicas", app.Replicas.Min,
		"maxReplicas", app.Replicas.Max,
		"metrics", seqNode(mapNode(
			"type", "Resource",
			"resource", mapNode(
				"name", "cpu",
				"target", mapNode("type", "Utilization", "averageUtilization", 80),
			),
		)),
	)
	return baseManifest("autoscaling/v2", "HorizontalPodAutoscaler", prov, spec), true
}

func cronJob(app *model.ResolvedComponent, prov provenance, env *yaml.Node) Manifest {
	spec := mapNode(
		"schedule", app.Schedule,
		"jobTemplate", mapNode(
			"spec", mapNode(
				"template", cronPodTemplate(app, prov, env),
			),
		),
	)
	return baseManifest("batch/v1", "CronJob", prov, spec)
}

// podTemplate names the component's own ServiceAccount on every kind. A pod
// that falls back to `default` inherits whatever that namespace's default
// carries, which is the opposite of a blast radius anyone chose (ADR-0014).
func podTemplate(app *model.ResolvedComponent, prov provenance, env *yaml.Node) *yaml.Node {
	return mapNode(
		"metadata", mapNode("labels", prov.labels()),
		"spec", mapNode(
			"serviceAccountName", app.Name,
			"containers", seqNode(container(app, env)),
		),
	)
}

func cronPodTemplate(app *model.ResolvedComponent, prov provenance, env *yaml.Node) *yaml.Node {
	return mapNode(
		"metadata", mapNode("labels", prov.labels()),
		"spec", mapNode(
			"serviceAccountName", app.Name,
			"restartPolicy", "Never",
			"containers", seqNode(container(app, env)),
		),
	)
}

func container(app *model.ResolvedComponent, env *yaml.Node) *yaml.Node {
	kv := []any{
		"name", app.Name,
		"image", app.Image,
	}
	if len(app.Command) > 0 {
		kv = append(kv, "command", asNode(app.Command))
	}
	if app.Port != 0 {
		kv = append(kv, "ports", seqNode(mapNode("name", "http", "containerPort", app.Port)))
	}
	if env != nil {
		kv = append(kv, "env", env)
	}
	if r := resourcesNode(app.Resources); r != nil {
		kv = append(kv, "resources", r)
	}
	if app.Health != "" && app.Port != 0 {
		probe := mapNode("httpGet", mapNode("path", app.Health, "port", app.Port))
		kv = append(kv, "livenessProbe", probe, "readinessProbe", cloneNode(probe))
	}
	return mapNode(kv...)
}

// envList renders the merged environment (precedence rule P1 already applied
// by resolution) with keys sorted, so output never depends on map iteration.
//
// Three forms, two of which are references (ADR-0018). A secret reference
// becomes a secretKeyRef against the Secret the author named, which kelson
// never reads. A binding becomes a secretKeyRef against the Secret the
// service's operator generates, or — for a connection detail that is not a
// credential, such as a cache's host and port — the plain value it resolves to.
// A literal *secret* value can never appear here because validation rejects it
// (ADR-0009), and nothing in this path can produce one: a reference carries a
// name and a key, and bindingRef either names a Secret key or returns a fact
// the renderer derived from names it already had. That is the property issue
// #82 asks for; internal/renderer/secrets.go states its boundary.
//
// Every unresolvable binding is reported, not just the first: one run should
// list all the work.
func envList(app *model.ResolvedComponent, services map[string]boundService) (*yaml.Node, Errors) {
	if len(app.Env) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(app.Env))
	for k := range app.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var errs Errors
	items := []*yaml.Node{}
	for _, k := range keys {
		v := app.Env[k]
		entry := []any{"name", k}
		switch {
		case v.Secret != nil:
			entry = append(entry, "valueFrom", secretKeyRefNode(v.Secret.Name, v.Secret.Key))
		case v.From != nil:
			field, ref, err := bindingRef(app.Name, k, v.From, services)
			if err != nil {
				errs = append(errs, *err)
				continue
			}
			entry = append(entry, field, ref)
		default:
			entry = append(entry, "value", v.Literal)
		}
		items = append(items, mapNode(entry...))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return seqNode(items...), nil
}

func resourcesNode(r *model.Resources) *yaml.Node {
	if r == nil {
		return nil
	}
	kv := []any{}
	if rl := resourceListNode(r.Requests); rl != nil {
		kv = append(kv, "requests", rl)
	}
	if rl := resourceListNode(r.Limits); rl != nil {
		kv = append(kv, "limits", rl)
	}
	if len(kv) == 0 {
		return nil
	}
	return mapNode(kv...)
}

func resourceListNode(rl *model.ResourceList) *yaml.Node {
	if rl == nil {
		return nil
	}
	kv := []any{}
	if rl.CPU != "" {
		kv = append(kv, "cpu", rl.CPU)
	}
	if rl.Memory != "" {
		kv = append(kv, "memory", rl.Memory)
	}
	if len(kv) == 0 {
		return nil
	}
	return mapNode(kv...)
}
