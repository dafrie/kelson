package kube_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/delivery/kube"
)

func dockerSecret(name, namespace string, cfg []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: cfg},
	}
}

// TestSecretResolverHappyPath resolves a real dockerconfigjson Secret and
// returns the credential for the requested registry.
func TestSecretResolverHappyPath(t *testing.T) {
	const (
		ns   = "apps"
		name = "reg-creds"
		user = "robot"
		pass = "hunter2"
	)
	cfg := []byte(`{"auths":{"ghcr.io":{"username":"` + user + `","password":"` + pass + `"}}}`)
	cli := fake.NewSimpleClientset(dockerSecret(name, ns, cfg))

	res, err := kube.NewSecretResolver(cli).Resolve(context.Background(), registry.SecretRef{
		Name: name, Namespace: ns, Registry: "ghcr.io",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Username != user || res.Password != pass {
		t.Errorf("credential = %+v, want user=%q pass=%q", res, user, pass)
	}
	if want := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)); res.Auth != want {
		t.Errorf("Auth = %q, want %q", res.Auth, want)
	}
}

func TestSecretResolverMissingSecret(t *testing.T) {
	cli := fake.NewSimpleClientset()
	_, err := kube.NewSecretResolver(cli).Resolve(context.Background(), registry.SecretRef{
		Name: "absent", Namespace: "apps", Registry: "ghcr.io",
	})
	if err == nil {
		t.Fatal("expected an error for a missing Secret")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing-secret error should say not found: %v", err)
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("missing-secret error should name the secret: %v", err)
	}
}

func TestSecretResolverMalformedSecret(t *testing.T) {
	// An unparseable dockerconfigjson is a resolve failure, not a crash.
	cli := fake.NewSimpleClientset(dockerSecret("reg-creds", "apps", []byte(`{"auths":`)))
	_, err := kube.NewSecretResolver(cli).Resolve(context.Background(), registry.SecretRef{
		Name: "reg-creds", Namespace: "apps", Registry: "ghcr.io",
	})
	if err == nil {
		t.Fatal("expected an error for a malformed Secret")
	}
}

func TestSecretResolverWrongType(t *testing.T) {
	sec := dockerSecret("reg-creds", "apps", []byte(`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`))
	sec.Type = corev1.SecretTypeOpaque
	cli := fake.NewSimpleClientset(sec)
	_, err := kube.NewSecretResolver(cli).Resolve(context.Background(), registry.SecretRef{
		Name: "reg-creds", Namespace: "apps", Registry: "ghcr.io",
	})
	if err == nil {
		t.Fatal("expected an error for a non-dockerconfigjson Secret")
	}
	if !strings.Contains(err.Error(), "want") {
		t.Errorf("wrong-type error should state the expected type: %v", err)
	}
}

// TestSecretResolverFailureNeverLeaksCredential closes the ADR-0009 hole for
// this resolver: a resolve failure's error text must carry neither the secret
// password nor its base64 auth form, even when the underlying dockerconfigjson
// is malformed or names a different registry. The secret is never interpolated
// into an error; only names, keys and types are.
func TestSecretResolverFailureNeverLeaksCredential(t *testing.T) {
	const (
		user = "robot"
		pass = "s3cr3t-password-value"
	)
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))

	// A secret that resolves for a *different* registry holds the password, so
	// a resolve for ghcr.io must fail without echoing it.
	cfg := []byte(`{"auths":{"gcr.io":{"username":` + quoted(user) + `,"password":` + quoted(pass) + `}}}`)
	cli := fake.NewSimpleClientset(dockerSecret("reg-creds", "apps", cfg))
	_, err := kube.NewSecretResolver(cli).Resolve(context.Background(), registry.SecretRef{
		Name: "reg-creds", Namespace: "apps", Registry: "ghcr.io",
	})
	if err == nil {
		t.Fatal("expected a resolve failure")
	}
	for _, forbidden := range []string{pass, auth} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error text leaked %q: %v", forbidden, err)
		}
	}

	// Same guarantee for a malformed payload that could tempt a wrapper into
	// dumping the decoded secret bytes into the error.
	bad := []byte(`{"auths":` + quoted(pass))
	cli = fake.NewSimpleClientset(dockerSecret("reg-creds", "apps", bad))
	_, err = kube.NewSecretResolver(cli).Resolve(context.Background(), registry.SecretRef{
		Name: "reg-creds", Namespace: "apps", Registry: "ghcr.io",
	})
	if err == nil {
		t.Fatal("expected a resolve failure for a malformed payload")
	}
	for _, forbidden := range []string{pass, auth} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error text leaked %q: %v", forbidden, err)
		}
	}
}

func quoted(s string) string { return `"` + s + `"` }
