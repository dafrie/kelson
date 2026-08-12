package observation

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	testNS   = "prod"
	testApp  = "web"
	testProj = "shop"
)

// deployment builds a Deployment object standing in for a rendered kelson
// workload. By default it looks satisfied: generation observed, Available and
// Progressing both True. A test mutates status to inject a failure.
func deployment(ns, name string, mutate func(m map[string]any)) *unstructured.Unstructured {
	m := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":       name,
			"namespace":  ns,
			"generation": int64(1),
		},
		"spec": map[string]any{
			"selector": map[string]any{
				"matchLabels": map[string]any{
					"kelson.dev/application": testApp,
					"kelson.dev/project":     testProj,
				},
			},
		},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"availableReplicas":  int64(1),
			"readyReplicas":      int64(1),
			"replicas":           int64(2),
			"conditions": []any{
				cond("Available", "True", "MinimumReplicasAvailable", ""),
				cond("Progressing", "True", "NewReplicaSetAvailable", ""),
			},
		},
	}
	if mutate != nil {
		mutate(m)
	}
	return &unstructured.Unstructured{Object: m}
}

// pod builds a Pod object. By default it is Running, scheduled and ready with
// one healthy container. A test mutates status to inject a failure.
func pod(ns, name string, mutate func(m map[string]any)) *unstructured.Unstructured {
	m := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels": map[string]any{
				"kelson.dev/application": testApp,
				"kelson.dev/project":     testProj,
			},
		},
		"status": map[string]any{
			"phase": "Running",
			"conditions": []any{
				cond("Ready", "True", "", ""),
				cond("PodScheduled", "True", "", ""),
			},
			"containerStatuses": []any{
				container("web", true, 0, map[string]any{
					"running": map[string]any{},
				}),
			},
		},
	}
	if mutate != nil {
		mutate(m)
	}
	return &unstructured.Unstructured{Object: m}
}

func cond(typ, status, reason, message string) map[string]any {
	var condVal string
	if message == "" {
		condVal = typ + " condition"
	}
	c := map[string]any{
		"type":   typ,
		"status": status,
		"reason": reason,
	}
	if message != "" {
		c["message"] = message
	} else if condVal != "" {
		c["message"] = condVal
	}
	return c
}

// container builds a containerStatus map. state is an arbitrary map such as
// {"running": {}} or {"waiting": {"reason": "CrashLoopBackOff"}}.
func container(name string, ready bool, restarts int64, state map[string]any) map[string]any {
	return map[string]any{
		"name":         name,
		"ready":        ready,
		"restartCount": restarts,
		"state":        state,
	}
}
