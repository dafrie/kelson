package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/forgeconn"
	"github.com/dafrie/kelson/internal/model"
)

// Materializing the previews credential (ADR-0033 decision 4), against fake
// stores and a fake Secret client — no cluster and no forge.

const previewToken = "ghp_preview_materialization_probe"

// fakeConnStore is the forgeconn.Store half.
type fakeConnStore struct {
	conns    []controlstore.StoredConnection
	material map[string]controlstore.AuthMaterial
}

func (s *fakeConnStore) List(context.Context) ([]controlstore.StoredConnection, error) {
	return s.conns, nil
}

func (s *fakeConnStore) ReadAuthSecret(_ context.Context, c controlstore.StoredConnection) (controlstore.AuthMaterial, error) {
	return s.material[c.Name], nil
}

func tokenConnections() *fakeConnStore {
	conn := controlstore.StoredConnection{
		Name: "acme-github",
		Spec: model.GitConnectionSpec{
			Provider: model.GitProviderGitHub,
			Auth:     model.GitConnectionAuth{Token: &model.TokenAuth{SecretRef: "acme-git-token"}},
		},
	}
	return &fakeConnStore{
		conns:    []controlstore.StoredConnection{conn},
		material: map[string]controlstore.AuthMaterial{"acme-github": {Token: previewToken}},
	}
}

// fakeSecretClient is the three-method slice ConnectionPreviewSecrets writes
// through.
type fakeSecretClient struct {
	objects map[string]*corev1.Secret
	creates int
	updates int
}

func newFakeSecretClient(existing ...*corev1.Secret) *fakeSecretClient {
	c := &fakeSecretClient{objects: map[string]*corev1.Secret{}}
	for _, s := range existing {
		c.objects[s.Namespace+"/"+s.Name] = s
	}
	return c
}

func (c *fakeSecretClient) Get(_ context.Context, key types.NamespacedName, obj *corev1.Secret) error {
	found, ok := c.objects[key.String()]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	*obj = *found.DeepCopy()
	return nil
}

func (c *fakeSecretClient) Create(_ context.Context, obj *corev1.Secret) error {
	c.creates++
	c.objects[obj.Namespace+"/"+obj.Name] = obj.DeepCopy()
	return nil
}

func (c *fakeSecretClient) Update(_ context.Context, obj *corev1.Secret) error {
	c.updates++
	c.objects[obj.Namespace+"/"+obj.Name] = obj.DeepCopy()
	return nil
}

// previewRevision is an environment previewing a repository the fake connection
// covers, in its own namespace.
func previewRevision(secretRef string) Revision {
	return Revision{
		Project:         "checkout",
		Environment:     "staging",
		TargetNamespace: "checkout-staging",
		Resolved: &model.Resolved{
			Project: "checkout",
			Environment: model.ResolvedEnvironment{
				Name:      "staging",
				Namespace: "checkout-staging",
				Previews: &model.ResolvedPreviews{
					Provider:  model.PreviewGitHub,
					Repo:      "https://github.com/acme/checkout",
					SecretRef: secretRef,
				},
			},
		},
	}
}

func materializer(store *fakeConnStore, client *fakeSecretClient) ConnectionPreviewSecrets {
	return ConnectionPreviewSecrets{
		Sources: &forgeconn.Resolver{Store: store},
		Client:  client,
	}
}

func TestPreviewSecretIsMaterializedAtTheDerivedName(t *testing.T) {
	client := newFakeSecretClient()
	name, shape, err := materializer(tokenConnections(), client).Ensure(context.Background(), previewRevision(""))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "checkout-staging-previews" {
		t.Fatalf("name = %q, want the lifecycle pair's stem", name)
	}
	if shape != forgeconn.ShapeBasicAuth {
		t.Errorf("shape = %q, want basic auth for a token connection", shape)
	}

	secret := client.objects["checkout-staging/checkout-staging-previews"]
	if secret == nil {
		t.Fatalf("no Secret was written: %v", client.objects)
	}
	if string(secret.Data[forgeconn.FluxPasswordKey]) != previewToken {
		t.Errorf("the Secret does not carry the credential: %v", secret.Data)
	}
	// Owned and labelled like every other object this controller writes, so
	// `kubectl get secret -l kelson.dev/managed-by=kelson` finds it.
	for key, want := range map[string]string{
		delivery.LabelManagedBy:   delivery.ManagedByKelson,
		delivery.LabelProject:     "checkout",
		delivery.LabelEnvironment: "staging",
	} {
		if got := secret.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
}

// An author who named their own Secret keeps it: ADR-0033 decision 4, "the
// field stays for anyone bringing their own Secret; nothing breaks".
func TestAnExplicitSecretRefIsLeftAlone(t *testing.T) {
	client := newFakeSecretClient()
	name, _, err := materializer(tokenConnections(), client).Ensure(context.Background(), previewRevision("my-own-forge-auth"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "" {
		t.Errorf("name = %q, want nothing materialized", name)
	}
	if client.creates != 0 {
		t.Error("kelson wrote a Secret for an environment that named its own")
	}
}

// A Secret at kelson's name that kelson does not own is a refusal, not an
// overwrite: adopting objects by guessing a name is the failure internal/secret
// refuses from the other side.
func TestAForeignSecretAtTheDerivedNameIsRefused(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-staging-previews", Namespace: "checkout-staging"},
		Data:       map[string][]byte{"username": []byte("hand-made")},
	}
	client := newFakeSecretClient(foreign)

	_, _, err := materializer(tokenConnections(), client).Ensure(context.Background(), previewRevision(""))
	if err == nil {
		t.Fatal("kelson overwrote a Secret it does not own")
	}
	if client.updates != 0 {
		t.Error("the foreign Secret was written to")
	}
	if string(client.objects["checkout-staging/checkout-staging-previews"].Data["username"]) != "hand-made" {
		t.Error("the foreign Secret's contents changed")
	}
}

// Refreshed when stale, and silent when it is not: an Update per reconcile
// would bump the resourceVersion of a Secret every watcher of the namespace
// sees.
func TestTheSecretIsRefreshedOnlyWhenItChanges(t *testing.T) {
	store := tokenConnections()
	client := newFakeSecretClient()
	m := materializer(store, client)
	ctx := context.Background()

	if _, _, err := m.Ensure(ctx, previewRevision("")); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if _, _, err := m.Ensure(ctx, previewRevision("")); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if client.updates != 0 {
		t.Errorf("updates = %d, want 0: nothing changed", client.updates)
	}

	// The user rotates the token in the Secret the connection names.
	store.material["acme-github"] = controlstore.AuthMaterial{Token: "ghp_rotated"}
	if _, _, err := m.Ensure(ctx, previewRevision("")); err != nil {
		t.Fatalf("Ensure after rotation: %v", err)
	}
	if client.updates != 1 {
		t.Errorf("updates = %d, want 1 after the credential changed", client.updates)
	}
	if got := string(client.objects["checkout-staging/checkout-staging-previews"].Data[forgeconn.FluxPasswordKey]); got != "ghp_rotated" {
		t.Errorf("password = %q, want the rotated token", got)
	}
}

// A repository no connection covers is the ordinary public case: nothing is
// written and nothing fails. flux-operator polls unauthenticated, which works
// at a lower rate limit.
func TestAnUnmatchedRepositoryMaterializesNothing(t *testing.T) {
	client := newFakeSecretClient()
	rev := previewRevision("")
	rev.Resolved.Environment.Previews.Repo = "https://gitlab.com/acme/checkout"

	name, _, err := materializer(tokenConnections(), client).Ensure(context.Background(), rev)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "" || client.creates != 0 {
		t.Error("a repository no connection covers produced a Secret")
	}
}

// An app nobody has installed yet is the connection's Ready condition, not this
// environment's deploy: failing here would put a revoked forge installation
// between a user and production.
func TestAnUninstalledAppDoesNotFailTheDeploy(t *testing.T) {
	store := &fakeConnStore{
		conns: []controlstore.StoredConnection{{
			Name: "acme-github",
			Spec: model.GitConnectionSpec{
				Provider: model.GitProviderGitHub,
				Auth: model.GitConnectionAuth{GitHubApp: &model.GitHubAppAuth{
					AppID: 12345, SecretRef: "acme-github-app",
				}},
			},
		}},
		material: map[string]controlstore.AuthMaterial{
			"acme-github": {PrivateKeyPEM: []byte("-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n")},
		},
	}
	client := newFakeSecretClient()

	name, _, err := materializer(store, client).Ensure(context.Background(), previewRevision(""))
	if err != nil {
		t.Fatalf("an uninstalled app failed the deploy: %v", err)
	}
	if name != "" || client.creates != 0 {
		t.Error("a Secret was written for an app that can mint nothing")
	}
}

// An environment with no previews block asks for nothing.
func TestNoPreviewsMaterializesNothing(t *testing.T) {
	client := newFakeSecretClient()
	rev := previewRevision("")
	rev.Resolved.Environment.Previews = nil

	if name, _, err := materializer(tokenConnections(), client).Ensure(context.Background(), rev); err != nil || name != "" {
		t.Errorf("Ensure = (%q, %v), want nothing", name, err)
	}
}

// A nil materializer is the pre-ADR-0033 behaviour, and a deliverer without one
// must not change.
func TestNilMaterializerIsTheOldBehaviour(t *testing.T) {
	var p PreviewSecrets
	d := &FluxDeliverer{PreviewSecrets: p}
	if d.PreviewSecrets != nil {
		t.Error("a nil PreviewSecrets must stay nil")
	}
}
