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

// twoGitHubConnections is the instance host matching cannot answer for: both
// cover github.com, so `previews.repo` alone ties and the materializer writes
// nothing. `source.connection` is what the author writes to break it (ADR-0033
// decision 4).
func twoGitHubConnections() *fakeConnStore {
	conn := func(name string) controlstore.StoredConnection {
		return controlstore.StoredConnection{
			Name: name,
			Spec: model.GitConnectionSpec{
				Provider: model.GitProviderGitHub,
				Auth:     model.GitConnectionAuth{Token: &model.TokenAuth{SecretRef: name + "-token"}},
			},
		}
	}
	return &fakeConnStore{
		conns: []controlstore.StoredConnection{conn("acme-github"), conn("contractor-github")},
		material: map[string]controlstore.AuthMaterial{
			"acme-github":       {Token: previewToken},
			"contractor-github": {Token: "ghp_contractor"},
		},
	}
}

// withSource puts the Project's source block on a revision, the way resolution
// carries it now.
func withSource(rev Revision, git, connection string) Revision {
	rev.Resolved.Source = &model.ResolvedSource{Git: git, Connection: connection}
	return rev
}

// Two connections covering one host is a refusal, and a refusal materializes
// nothing rather than failing the deploy.
func TestAmbiguousConnectionsMaterializeNothing(t *testing.T) {
	client := newFakeSecretClient()
	name, _, err := materializer(twoGitHubConnections(), client).Ensure(context.Background(), previewRevision(""))
	if err != nil {
		t.Fatalf("an ambiguous resolution failed the deploy: %v", err)
	}
	if name != "" || client.creates != 0 {
		t.Error("kelson picked between two connections covering the same repository")
	}
}

// The override reaches the previews path now that model.Resolved carries it:
// the author named a connection for this repository, and the previews credential
// is that connection's.
func TestExplicitSourceConnectionBreaksTheTie(t *testing.T) {
	client := newFakeSecretClient()
	rev := withSource(previewRevision(""), "https://github.com/acme/checkout", "contractor-github")

	name, shape, err := materializer(twoGitHubConnections(), client).Ensure(context.Background(), rev)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "checkout-staging-previews" || shape != forgeconn.ShapeBasicAuth {
		t.Fatalf("Ensure = (%q, %q), want the derived name and basic auth", name, shape)
	}
	secret := client.objects["checkout-staging/checkout-staging-previews"]
	if got := string(secret.Data[forgeconn.FluxPasswordKey]); got != "ghp_contractor" {
		t.Errorf("password = %q, want the connection the author named", got)
	}
}

// The edge the rule is written for: `previews.repo` may be a different
// repository from `source.git`, and `source.connection` is a sentence about the
// source one. Where the two are on different forges the override is ignored and
// host matching against previews.repo answers instead — here, ambiguously, so
// nothing is written. Honouring it would authenticate a github.com poll with a
// connection the author chose for a self-hosted forge.
func TestASourceConnectionOnAnotherForgeIsIgnored(t *testing.T) {
	client := newFakeSecretClient()
	rev := withSource(previewRevision(""), "https://git.acme.internal/acme/checkout", "contractor-github")

	name, _, err := materializer(twoGitHubConnections(), client).Ensure(context.Background(), rev)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "" || client.creates != 0 {
		t.Error("an override written for another forge was applied to previews.repo")
	}
}

// And the same override does travel to a *sibling* repository on the same
// forge, which is the case host granularity buys: a project whose previews
// watch a different repo on the same host wants the same credential.
func TestTheOverrideTravelsToASiblingRepositoryOnTheSameForge(t *testing.T) {
	client := newFakeSecretClient()
	rev := previewRevision("")
	rev.Resolved.Environment.Previews.Repo = "https://github.com/acme/checkout-web"
	rev = withSource(rev, "https://github.com/acme/checkout", "contractor-github")

	name, _, err := materializer(twoGitHubConnections(), client).Ensure(context.Background(), rev)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "checkout-staging-previews" {
		t.Fatalf("name = %q, want the derived name", name)
	}
	secret := client.objects["checkout-staging/checkout-staging-previews"]
	if got := string(secret.Data[forgeconn.FluxPasswordKey]); got != "ghp_contractor" {
		t.Errorf("password = %q, want the connection the author named", got)
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
