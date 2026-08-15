package controlstore

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// The connection store, against the same fake client the spec store's tests
// use — plus core/v1, because this is the one store that reads a Secret the
// user wrote, and plus the status subresource, because TestConnection's
// observation is written through it.

const testConnection = "acme-github"

func newConnectionClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("registering kelson.dev/v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("registering core/v1: %v", err)
	}
	return crfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.GitConnection{}).
		Build()
}

func newConnectionStore(t *testing.T, c client.Client) *GitConnectionStore {
	t.Helper()
	store, err := NewGitConnectionStore(GitConnectionStoreOptions{Client: c, Namespace: testNamespace})
	if err != nil {
		t.Fatalf("NewGitConnectionStore: %v", err)
	}
	return store
}

func tokenSpec(secretRef string) model.GitConnectionSpec {
	return model.GitConnectionSpec{
		Provider: model.GitProviderGitHub,
		Auth:     model.GitConnectionAuth{Token: &model.TokenAuth{SecretRef: secretRef}},
	}
}

func tokenSecret(name string, data map[string]string) *corev1.Secret {
	values := make(map[string][]byte, len(data))
	for k, v := range data {
		values[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       values,
	}
}

func TestConnectionCreateWritesTheCustomResource(t *testing.T) {
	ctx := context.Background()
	c := newConnectionClient(t)
	store := newConnectionStore(t, c)

	created, err := store.Create(ctx, testConnection, tokenSpec("acme-git-token"), CreateConnectionOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != testConnection || created.Version == "" {
		t.Fatalf("create returned %+v, want a named connection with a resourceVersion", created)
	}

	var conn v1alpha1.GitConnection
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testConnection}, &conn); err != nil {
		t.Fatalf("read the stored connection: %v", err)
	}
	for key, want := range map[string]string{
		labelManagedBy:  managedByKelson,
		labelConnection: testConnection,
		labelState:      stateConnection,
	} {
		if got := conn.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
	if conn.Spec.Auth.Token == nil || conn.Spec.Auth.Token.SecretRef != "acme-git-token" {
		t.Errorf("the stored spec lost its Secret reference: %+v", conn.Spec)
	}
	// Nothing was observed yet, and the store must say so rather than report
	// two false booleans a reader cannot tell from a failed probe.
	if created.Status.Observed {
		t.Error("a connection nobody has probed reports Observed")
	}
}

// There is no UpdateConnection in the schema, so a create against a name that
// exists must not quietly become one: every project resolving through that name
// would be repointed at a different forge.
func TestConnectionCreateRefusesAnExistingName(t *testing.T) {
	ctx := context.Background()
	store := newConnectionStore(t, newConnectionClient(t))
	if _, err := store.Create(ctx, testConnection, tokenSpec("first"), CreateConnectionOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := store.Create(ctx, testConnection, tokenSpec("second"), CreateConnectionOptions{})
	if !AsVersionConflict(err) {
		t.Fatalf("re-creating answered %v, want a store/version-conflict", err)
	}
	stored, err := store.Get(ctx, testConnection)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.Auth.Token.SecretRef != "first" {
		t.Errorf("the refused create changed the stored Secret reference to %q", stored.Spec.Auth.Token.SecretRef)
	}
}

// A retry after a timeout must answer what the first attempt answered.
func TestConnectionCreateReplaysAnIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	store := newConnectionStore(t, newConnectionClient(t))
	opts := CreateConnectionOptions{IdempotencyKey: "req-1"}

	first, err := store.Create(ctx, testConnection, tokenSpec("acme-git-token"), opts)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := store.Create(ctx, testConnection, tokenSpec("acme-git-token"), opts)
	if err != nil {
		t.Fatalf("the replay was refused: %v", err)
	}
	if first.Version != second.Version {
		t.Errorf("the replay wrote again: versions %q and %q", first.Version, second.Version)
	}
	// A different key against the same name is a different request, and it is
	// the conflict the replay path must not hide.
	if _, err := store.Create(ctx, testConnection, tokenSpec("other"),
		CreateConnectionOptions{IdempotencyKey: "req-2"}); !AsVersionConflict(err) {
		t.Errorf("a second request answered %v, want a store/version-conflict", err)
	}
}

func TestConnectionCreateDryRunPersistsNothing(t *testing.T) {
	ctx := context.Background()
	c := newConnectionClient(t)
	store := newConnectionStore(t, c)

	got, err := store.Create(ctx, testConnection, tokenSpec("acme-git-token"), CreateConnectionOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run create: %v", err)
	}
	if got.Name != testConnection {
		t.Errorf("the dry run returned %+v, want the connection it validated", got)
	}
	var conn v1alpha1.GitConnection
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testConnection}, &conn); err == nil {
		t.Fatal("the dry run persisted the connection")
	}
}

func TestConnectionListIsSortedAndDeleteRemoves(t *testing.T) {
	ctx := context.Background()
	store := newConnectionStore(t, newConnectionClient(t))
	for _, name := range []string{"zeta", "alpha", "mu"} {
		if _, err := store.Create(ctx, name, tokenSpec(name+"-token"), CreateConnectionOptions{}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "alpha,mu,zeta" {
		t.Fatalf("list order = %v, want alpha,mu,zeta", names)
	}

	if err := store.Delete(ctx, "mu", DeleteConnectionOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, "mu"); !AsNotFound(err) {
		t.Fatalf("reading the deleted connection answered %v, want a store/not-found", err)
	}
	// Without a key a second delete is a not-found; with one it is the recorded
	// outcome of a completed delete, which is [DeleteOptions]'s contract for a
	// spec and this store keeps it identical.
	if err := store.Delete(ctx, "mu", DeleteConnectionOptions{}); !AsNotFound(err) {
		t.Errorf("a second delete answered %v, want a store/not-found", err)
	}
	if err := store.Delete(ctx, "mu", DeleteConnectionOptions{IdempotencyKey: "req-1"}); err != nil {
		t.Errorf("a replayed delete answered %v, want success", err)
	}
}

func TestConnectionDeleteDryRunKeepsTheConnection(t *testing.T) {
	ctx := context.Background()
	store := newConnectionStore(t, newConnectionClient(t))
	if _, err := store.Create(ctx, testConnection, tokenSpec("acme-git-token"), CreateConnectionOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Delete(ctx, testConnection, DeleteConnectionOptions{DryRun: true}); err != nil {
		t.Fatalf("dry-run delete: %v", err)
	}
	if _, err := store.Get(ctx, testConnection); err != nil {
		t.Fatalf("the dry run deleted the connection: %v", err)
	}
}

func TestConnectionUpdateStatusRecordsTheProbe(t *testing.T) {
	ctx := context.Background()
	store := newConnectionStore(t, newConnectionClient(t))
	if _, err := store.Create(ctx, testConnection, tokenSpec("acme-git-token"), CreateConnectionOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.UpdateStatus(ctx, testConnection, ConnectionObservation{
		Ready:        true,
		Reachable:    true,
		Account:      "acme",
		Repositories: 42,
		Message:      "github.com answered",
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !got.Status.Ready || !got.Status.Reachable {
		t.Errorf("status = %+v, want both conditions true", got.Status)
	}
	if got.Status.Account != "acme" || got.Status.Repositories != 42 {
		t.Errorf("the provider-reported pair came back as %q/%d", got.Status.Account, got.Status.Repositories)
	}
	if !got.Status.Observed {
		t.Error("a probed connection reports Observed false, which a client reads as 'not looked at yet'")
	}

	// A failed probe turns Reachable off and keeps a reason a caller can branch
	// on, which is the whole reason the conditions are separate.
	after, err := store.UpdateStatus(ctx, testConnection, ConnectionObservation{
		Ready:   true,
		Message: "the token was rejected",
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if after.Status.Reachable || after.Status.Message != "the token was rejected" {
		t.Errorf("status after the failed probe = %+v", after.Status)
	}

	var conn v1alpha1.GitConnection
	if err := store.client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testConnection}, &conn); err != nil {
		t.Fatalf("read back: %v", err)
	}
	reasons := map[string]string{}
	for _, cond := range conn.Status.Conditions {
		reasons[cond.Type] = cond.Reason
	}
	if reasons[v1alpha1.ConditionReachable] != ReasonUnreachable {
		t.Errorf("Reachable reason = %q, want %q", reasons[v1alpha1.ConditionReachable], ReasonUnreachable)
	}
	if reasons[v1alpha1.ConditionReady] != ReasonConnectionReady {
		t.Errorf("Ready reason = %q, want %q", reasons[v1alpha1.ConditionReady], ReasonConnectionReady)
	}
	// The spec must survive a status write untouched: an apply of a status-only
	// object under the spec's field manager would have deleted it.
	if conn.Spec.Auth.Token == nil || conn.Spec.Auth.Token.SecretRef != "acme-git-token" {
		t.Errorf("the status write damaged the spec: %+v", conn.Spec)
	}
}

func TestConnectionUpdateStatusOnAMissingConnection(t *testing.T) {
	store := newConnectionStore(t, newConnectionClient(t))
	_, err := store.UpdateStatus(context.Background(), "ghost", ConnectionObservation{Reachable: true})
	if !AsNotFound(err) {
		t.Fatalf("probing a connection that does not exist answered %v, want a store/not-found", err)
	}
}

func TestReadAuthSecretReturnsTheMaterialAndRegistersIt(t *testing.T) {
	ctx := context.Background()
	c := newConnectionClient(t, tokenSecret("acme-git-token", map[string]string{
		model.TokenKey:         "ghp-not-a-real-token-9f0a1b2c",
		model.TokenUsernameKey: "acme-bot",
	}))
	store := newConnectionStore(t, c)
	if _, err := store.Create(ctx, testConnection, tokenSpec("acme-git-token"), CreateConnectionOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	conn, err := store.Get(ctx, testConnection)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	material, err := store.ReadAuthSecret(ctx, conn)
	if err != nil {
		t.Fatalf("read the auth secret: %v", err)
	}
	if material.Token != "ghp-not-a-real-token-9f0a1b2c" || material.Username != "acme-bot" {
		t.Fatalf("material = %+v, want the Secret's token and username", material)
	}

	// The registration is the point of the method existing here at all: from
	// this moment the value cannot reach a log line or a wire error, whichever
	// plane writes it (#117).
	if got := redact.Scrub("cloning with " + material.Token); strings.Contains(got, material.Token) {
		t.Fatalf("the token was not registered with internal/redact: %q", got)
	}
	// The username is deliberately not registered: it is an identity, and
	// scrubbing an account name out of unrelated text helps nobody.
	if got := redact.Scrub("acting as " + material.Username); !strings.Contains(got, material.Username) {
		t.Errorf("the username was registered as a secret: %q", got)
	}
}

func TestReadAuthSecretNamesWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	c := newConnectionClient(t, tokenSecret("empty", map[string]string{"unrelated": "value"}))
	store := newConnectionStore(t, c)

	cases := []struct {
		name    string
		spec    model.GitConnectionSpec
		wants   []string
		wantErr bool
	}{{
		name:    "no Secret of that name",
		spec:    tokenSpec("nowhere"),
		wants:   []string{"nowhere", "does not exist"},
		wantErr: true,
	}, {
		name:    "the Secret exists without the key",
		spec:    tokenSpec("empty"),
		wants:   []string{"empty", model.TokenKey},
		wantErr: true,
	}, {
		name: "the connection references no credential at all",
		spec: model.GitConnectionSpec{Provider: model.GitProviderGeneric,
			Host: "https://git.acme.internal"},
		wants:   []string{"references no credential"},
		wantErr: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := StoredConnection{Name: testConnection, Spec: tc.spec}
			_, err := store.ReadAuthSecret(ctx, conn)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !AsNotFound(err) {
				t.Fatalf("err = %v, want a store/not-found", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// A connection's Ref is what the matcher consumes, and the effective host is
// what makes a `provider: github` connection that named no host match
// github.com.
func TestStoredConnectionRefUsesTheEffectiveHost(t *testing.T) {
	conn := StoredConnection{
		Name:   testConnection,
		Spec:   tokenSpec("acme-git-token"),
		Status: ConnectionStatus{Account: "acme"},
	}
	ref := conn.Ref()
	if ref.Host != model.DefaultGitHubHost || ref.Account != "acme" || ref.Name != testConnection {
		t.Fatalf("ref = %+v, want the default GitHub host and the observed account", ref)
	}
}
