package forgehttp

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dafrie/kelson/internal/redact"
)

// The one Kubernetes write this package makes: the Secret a freshly created
// GitHub App's material goes into (ADR-0033 decision 1 — the CR carries
// identifiers, the Secret carries the material).
//
// # Why it is not internal/secret's Store
//
// That package is ADR-0009's *authoring* half: `kelson secret set`, values a
// user names, in an application namespace, labelled `kelson.dev/managed-secret`
// so `kelson secret list` can enumerate them. This is none of those. Nobody
// authored this value — GitHub generated it and handed it back exactly once —
// it lives in kelson's own namespace beside the connection that references it,
// and it must never appear in the surface that lists a namespace's secrets to a
// user. Reusing that Store would have put an app's private key in the output of
// `kelson secret list`.
//
// # Why it is not internal/controlstore
//
// The connection store reads this Secret at use time and deliberately has no
// path from a name to a value in the other direction (its header says the api
// plane must not be able to reach one). Giving it a writer would give every
// holder of the store a way to overwrite a credential; the writer lives with
// the one flow that legitimately produces material instead.

// KubeSecrets writes app credentials with a typed clientset.
type KubeSecrets struct {
	// Client is the typed clientset. The same one the state stores ride: this
	// only ever gets, creates and updates Secrets in one namespace, so no
	// discovery mapper is involved.
	Client kubernetes.Interface
	// Namespace is kelson's own — where connections and their Secrets live,
	// which is the only place ADR-0033 decision 1 puts them.
	Namespace string
}

var _ SecretWriter = KubeSecrets{}

// Labels every Secret this writer creates carries, so `kubectl get secret -l
// kelson.dev/managed-by=kelson` in kelson's namespace answers "what did kelson
// put here" the same way every other object it owns does.
const (
	labelManagedBy  = "kelson.dev/managed-by"
	managedByKelson = "kelson"
	labelPurpose    = "kelson.dev/purpose"
	purposeForgeApp = "forge-app"
)

// Write creates the Secret, or replaces the keys it carries if it already
// exists.
//
// Replace and not merge: the two keys are one app's credentials, and a Secret
// holding a new app's private key beside an old app's webhook secret would
// verify deliveries it cannot act on. A name collision is the flow being run
// twice for the same app slug, and the second run's material is the live one.
func (k KubeSecrets) Write(ctx context.Context, name string, data map[string][]byte) error {
	if k.Client == nil || k.Namespace == "" {
		return fmt.Errorf("forgehttp: no Kubernetes client or namespace to write Secret %q into", name)
	}
	// Registered before the write, not after: from here the private key and the
	// webhook secret cannot reach a log line or an error message anywhere in
	// this process, including the errors below (issue #117).
	for _, v := range data {
		if len(v) > 0 {
			redact.Register(string(v))
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.Namespace,
			Labels: map[string]string{
				labelManagedBy: managedByKelson,
				labelPurpose:   purposeForgeApp,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	_, err := k.Client.CoreV1().Secrets(k.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("forgehttp: writing Secret %s/%s: %w", k.Namespace, name, err)
	}
	if _, err := k.Client.CoreV1().Secrets(k.Namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("forgehttp: replacing Secret %s/%s: %w", k.Namespace, name, err)
	}
	return nil
}
