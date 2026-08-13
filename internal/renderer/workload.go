package renderer

import (
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// appManifests renders one application in a fixed resource order:
// ServiceAccount, Service, workload (Deployment|CronJob), HPA, HTTPRoute,
// Certificate, ServiceMonitor. Kinds that do not apply are simply absent.
func appManifests(
	resolved *model.Resolved,
	app *model.ResolvedApplication,
	profile clusterprofile.ClusterProfile,
	services map[string]boundService,
) ([]Manifest, error) {
	hash, err := specHash(resolved, app)
	if err != nil {
		return nil, Errors{{Code: ErrInternal, Message: err.Error()}}
	}
	env, berrs := envList(app, services)
	if len(berrs) > 0 {
		return nil, berrs
	}
	prov := provenance{
		project:     resolved.Project,
		environment: resolved.Environment.Name,
		application: app.Name,
		namespace:   resolved.Environment.Namespace,
		specHash:    hash,
	}

	var out []Manifest
	if app.Kind == model.WorkloadService {
		out = append(out, serviceAccount(prov), service(app, prov))
	}
	switch app.Kind {
	case model.WorkloadService, model.WorkloadWorker:
		out = append(out, deployment(app, prov, env))
		if hpa, ok := autoscaler(app, prov); ok {
			out = append(out, hpa)
		}
	case model.WorkloadCron:
		out = append(out, cronJob(app, prov, env))
	}
	routes, err := routingResources(resolved, app, profile, prov)
	if err != nil {
		return nil, err
	}
	out = append(out, routes...)
	if app.Kind == model.WorkloadService && profile.Prometheus != nil {
		out = append(out, serviceMonitor(app, prov))
	}
	return out, nil
}

func serviceAccount(prov provenance) Manifest {
	// A ServiceAccount is metadata-only for now: it exists so workloads have
	// a per-application identity to hang future policy (IRSA, image pull)
	// on without re-keying pods later.
	root := mapNode(
		"apiVersion", "v1",
		"kind", "ServiceAccount",
		"metadata", metadataNode(prov),
	)
	return Manifest{APIVersion: "v1", Kind: "ServiceAccount", Name: prov.name(), Namespace: prov.namespace, doc: docNode(root)}
}

func service(app *model.ResolvedApplication, prov provenance) Manifest {
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

func deployment(app *model.ResolvedApplication, prov provenance, env *yaml.Node) Manifest {
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

func autoscaling(app *model.ResolvedApplication) bool {
	return app.Replicas.Max > app.Replicas.Min
}

func autoscaler(app *model.ResolvedApplication, prov provenance) (Manifest, bool) {
	if !autoscaling(app) || app.Kind == model.WorkloadCron {
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

func cronJob(app *model.ResolvedApplication, prov provenance, env *yaml.Node) Manifest {
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

func podTemplate(app *model.ResolvedApplication, prov provenance, env *yaml.Node) *yaml.Node {
	specKV := []any{}
	if app.Kind == model.WorkloadService {
		specKV = append(specKV, "serviceAccountName", app.Name)
	}
	specKV = append(specKV, "containers", seqNode(container(app, env)))
	return mapNode(
		"metadata", mapNode("labels", prov.labels()),
		"spec", mapNode(specKV...),
	)
}

func cronPodTemplate(app *model.ResolvedApplication, prov provenance, env *yaml.Node) *yaml.Node {
	c := container(app, env)
	return mapNode(
		"metadata", mapNode("labels", prov.labels()),
		"spec", mapNode(
			"restartPolicy", "Never",
			"containers", seqNode(c),
		),
	)
}

func container(app *model.ResolvedApplication, env *yaml.Node) *yaml.Node {
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
// Bindings become secretKeyRefs against the Secret the service's operator
// generates; a literal secret value can never appear here because validation
// rejects it (ADR-0009).
//
// Every unresolvable binding is reported, not just the first: one run should
// list all the work.
func envList(app *model.ResolvedApplication, services map[string]boundService) (*yaml.Node, Errors) {
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
		if v.From != nil {
			ref, err := bindingRef(app.Name, k, v.From, services)
			if err != nil {
				errs = append(errs, *err)
				continue
			}
			entry = append(entry, "valueFrom", ref)
		} else {
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
