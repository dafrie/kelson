package install

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The one component whose manifest kelson writes rather than fetches.
//
// CNCF Distribution publishes a container image and no install.yaml — there
// is no Kubernetes manifest for ADR-0021 decision 2's pin-a-URL-and-a-digest
// rule to apply to. ADR-0030's 2026-08-14 amendment records the same
// exception decision 2 itself already carved for flux-aio: the objects below
// are kelson-authored, pinned by image digest rather than manifest digest,
// and Installer.load builds them in Go instead of downloading and decoding
// YAML (pins.go's registry row, install.go's composeAuthored).
//
// The shape mirrors hack/local/up.sh's own registry, which is the reason the
// name and the port are not new choices: one Deployment, one Service named
// kelson-registry on port 5000, reached at the same cluster-internal FQDN a
// local kind cluster already uses. The one difference is durability — a PVC
// here, an emptyDir there, because this registry is not torn down with the
// cluster.

const (
	// RegistryName is the Deployment and Service name the "registry" catalog
	// entry creates, and the host component of RegistryEndpoint.
	RegistryName = "kelson-registry"
	// RegistryPort is the registry's HTTP port: the container's, and the
	// Service's.
	RegistryPort = 5000
	// RegistryNamespace is where the entry lands — kelson-system, alongside
	// the control plane, not a namespace of its own. A cluster that already
	// ran the Helm chart has this namespace already; the entry adopts it
	// exactly as any other row adopts a pre-existing Namespace object.
	RegistryNamespace = "kelson-system"
	// RegistryEndpoint is the cluster-internal address every other workload
	// reaches the installed registry at: plain HTTP, so a pusher or puller
	// must mark it insecure — $KELSON_INSECURE_REGISTRIES /
	// --insecure-registries on `kelson build`, server.insecureRegistries on
	// the chart (docs/install.md).
	RegistryEndpoint = RegistryName + "." + RegistryNamespace + ".svc.cluster.local:5000"

	// RegistryStorageSize is the PersistentVolumeClaim's default request.
	// There is no command-line override: ADR-0021 §2 refuses pin overrides
	// from the CLI, on the ground that a different value is a decision the
	// pins table should carry so it is reviewed, not a flag. Needing more
	// after install is `kubectl edit pvc` — ordinary PVC resize, if the
	// StorageClass allows expansion.
	RegistryStorageSize = "10Gi"

	// registryDataVolume names the volume and the volumeMount that carry it,
	// so the two stay in sync by construction.
	registryDataVolume = "data"
	// registryPVCName is the PersistentVolumeClaim the Deployment claims.
	registryPVCName = "kelson-registry-data"
	// registrySelectorLabel is the one label the Deployment, its Pod template
	// and the Service all share — the same "app" convention
	// hack/local/up.sh's own copy of this registry uses.
	registrySelectorLabel = "app"
)

// registryImageRef is the full pull spec for a pin: the repository plus its
// digest, never a tag. Kubernetes itself then enforces the pin — a pull that
// found different bytes at that digest would fail closed, not silently drift.
func registryImageRef(c Component) string {
	return fmt.Sprintf("%s@sha256:%s", c.Image, c.ImageDigest)
}

// registryManifest is the "registry" row's whole install, in apply order: the
// namespace before anything that lives in it, the volume before the
// Deployment that claims it, exactly as a fetched manifest's own document
// order would read.
func registryManifest(c Component) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		registryNamespaceObject(c),
		registryPVCObject(c),
		registryDeploymentObject(c),
		registryServiceObject(c),
	}
}

func registryNamespaceObject(c Component) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": c.Namespace,
		},
	}}
}

// registryPVCObject requests the default size on whatever StorageClass the
// cluster marks default — no storageClassName is set, so this is ordinary PVC
// semantics, nothing registry-specific and nothing that assumes a cloud.
func registryPVCObject(c Component) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      registryPVCName,
			"namespace": c.Namespace,
		},
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": RegistryStorageSize,
				},
			},
		},
	}}
}

func registryDeploymentObject(c Component) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      RegistryName,
			"namespace": c.Namespace,
			"labels":    map[string]any{registrySelectorLabel: RegistryName},
		},
		"spec": map[string]any{
			// One replica: CNCF Distribution's filesystem storage driver
			// (what the PVC above backs) is not safe for two writers, and a
			// second replica would race the first over the same volume.
			"replicas": int64(1),
			"selector": map[string]any{
				"matchLabels": map[string]any{registrySelectorLabel: RegistryName},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{registrySelectorLabel: RegistryName},
				},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "registry",
							"image": registryImageRef(c),
							"ports": []any{
								map[string]any{"name": "http", "containerPort": int64(RegistryPort)},
							},
							"volumeMounts": []any{
								map[string]any{"name": registryDataVolume, "mountPath": "/var/lib/registry"},
							},
							"readinessProbe": registryProbe(),
							"livenessProbe":  registryProbe(),
						},
					},
					"volumes": []any{
						map[string]any{
							"name": registryDataVolume,
							"persistentVolumeClaim": map[string]any{
								"claimName": registryPVCName,
							},
						},
					},
				},
			},
		},
	}}
}

// registryProbe checks the distribution API's own root route, which answers
// 200 with an empty JSON object once the registry is serving — the same
// endpoint `docker login` and every registry client probe first.
func registryProbe() map[string]any {
	return map[string]any{
		"httpGet": map[string]any{
			"path": "/v2/",
			"port": int64(RegistryPort),
		},
		"initialDelaySeconds": int64(5),
		"periodSeconds":       int64(10),
	}
}

func registryServiceObject(c Component) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      RegistryName,
			"namespace": c.Namespace,
			"labels":    map[string]any{registrySelectorLabel: RegistryName},
		},
		"spec": map[string]any{
			"type":     "ClusterIP",
			"selector": map[string]any{registrySelectorLabel: RegistryName},
			"ports": []any{
				map[string]any{"name": "http", "port": int64(RegistryPort), "targetPort": int64(RegistryPort)},
			},
		},
	}}
}
