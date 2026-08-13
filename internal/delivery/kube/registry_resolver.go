package kube

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/redact"
)

// SecretResolver resolves a registry.SecretRef against a live cluster's
// kubernetes.io/dockerconfigjson Secrets (issue #51). It is the concrete,
// Kubernetes-backed half of the registry.Resolver interface, which lives in
// internal/build because that plane's lint allow-list forbids client-go
// (.golangci.yml) — the same split as the build executor.
//
// It never formats the raw secret or the resolved credential into an error or
// a log line: the only values that reach error text are names, key names and
// secret types. The credential is handed back for callers to format through
// registry.Credential.String, which redacts it.
type SecretResolver struct {
	clientset kubernetes.Interface
}

var _ registry.Resolver = (*SecretResolver)(nil)

// NewSecretResolver returns a resolver backed by the injected clientset, so
// its tests drive a fake and no test needs a real cluster.
func NewSecretResolver(clientset kubernetes.Interface) *SecretResolver {
	return &SecretResolver{clientset: clientset}
}

// Resolve reads the named kubernetes.io/dockerconfigjson Secret and returns
// the credential for ref.Registry. Parsing is delegated to
// registry.CredentialFromDockerConfigJSON, which is unit-tested; this method
// only handles the cluster read and the structural guarantees around it.
func (r *SecretResolver) Resolve(ctx context.Context, ref registry.SecretRef) (registry.Credential, error) {
	if ref.Name == "" || ref.Namespace == "" {
		return registry.Credential{}, errors.New("kube: a registry secret reference needs both Name and Namespace")
	}

	secret, err := r.clientset.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return registry.Credential{}, fmt.Errorf("kube: registry secret %s/%s not found", ref.Namespace, ref.Name)
		}
		return registry.Credential{}, fmt.Errorf("kube: reading registry secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return registry.Credential{}, fmt.Errorf("kube: registry secret %s/%s is type %q, want %q",
			ref.Namespace, ref.Name, secret.Type, corev1.SecretTypeDockerConfigJson)
	}
	payload, ok := secret.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return registry.Credential{}, fmt.Errorf("kube: registry secret %s/%s has no %q key",
			ref.Namespace, ref.Name, corev1.DockerConfigJsonKey)
	}

	cred, err := registry.CredentialFromDockerConfigJSON(payload, ref.Registry)
	if err != nil {
		// Only the reference and the registry are named — never the payload.
		return registry.Credential{}, fmt.Errorf("kube: parsing registry secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	// This is the moment kelson learns a credential, so it is the moment the
	// value becomes unprintable process-wide (issue #117). Registering here
	// rather than at each surface that might echo it is what makes "no secret
	// value reaches a log, an error or a diff" a property of the process instead
	// of a rule every future caller has to remember.
	redact.Register(cred.SecretValues()...)
	return cred, nil
}
