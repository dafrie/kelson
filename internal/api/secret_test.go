package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/secret"
)

// SecretService served (issue #116). The seam is faked for the reason every
// seam here is — the api plane's lint rule forbids the Kubernetes clients that
// internal/secret's own tests drive — so what these assert is the handler:
// which requests reach the store, which never do, how a `secret/*` refusal maps
// onto a ConnectRPC code, and that no response can carry a value.

// fakeSecrets is an in-memory SecretStore reproducing the parts of
// internal/secret's contract the handler depends on: merge-on-set, the managed
// rule, and the not-found delete.
type fakeSecrets struct {
	mu sync.Mutex
	// items is namespace/name -> keys -> value. The values are held so a test
	// can assert one arrived; nothing in the handler can read them back.
	items map[string]map[string]string
	sets  []secret.SetRequest
	dels  []secret.DeleteRequest
	err   error
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{items: map[string]map[string]string{}}
}

func (f *fakeSecrets) key(namespace, name string) string { return namespace + "/" + name }

func (f *fakeSecrets) Set(_ context.Context, req secret.SetRequest) (secret.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, req)
	if f.err != nil {
		return secret.Secret{}, f.err
	}
	namespace, err := req.Resolve()
	if err != nil {
		return secret.Secret{}, err
	}
	if req.DryRun {
		// A server-side dry run computes the merge and persists nothing, which
		// is what the fake reproduces: the read-back is what would result.
		return secret.Secret{Name: req.Name, Namespace: namespace,
			Keys: mergedKeys(f.items[f.key(namespace, req.Name)], req.Values)}, nil
	}
	held := f.items[f.key(namespace, req.Name)]
	if held == nil {
		held = map[string]string{}
		f.items[f.key(namespace, req.Name)] = held
	}
	for k, v := range req.Values {
		held[k] = v
	}
	return secret.Secret{Name: req.Name, Namespace: namespace, Keys: sortedMapKeys(held),
		CreatedAt: fakeSecretCreatedAt()}, nil
}

func (f *fakeSecrets) List(_ context.Context, t secret.Target) ([]secret.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	namespace, err := t.Resolve()
	if err != nil {
		return nil, err
	}
	var out []secret.Secret
	for key, held := range f.items {
		ns, name, _ := strings.Cut(key, "/")
		if ns != namespace {
			continue
		}
		out = append(out, secret.Secret{Name: name, Namespace: ns, Keys: sortedMapKeys(held),
			CreatedAt: fakeSecretCreatedAt()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeSecrets) Delete(_ context.Context, req secret.DeleteRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dels = append(f.dels, req)
	if f.err != nil {
		return f.err
	}
	namespace, err := req.Resolve()
	if err != nil {
		return err
	}
	if _, ok := f.items[f.key(namespace, req.Name)]; !ok {
		return secret.Error{Code: secret.ErrNotFound, Resource: "Secret/" + f.key(namespace, req.Name),
			Message: "no such Secret"}
	}
	if !req.DryRun {
		delete(f.items, f.key(namespace, req.Name))
	}
	return nil
}

func (f *fakeSecrets) held(namespace, name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.items[f.key(namespace, name)]
}

func mergedKeys(held, adding map[string]string) []string {
	merged := map[string]string{}
	for k, v := range held {
		merged[k] = v
	}
	for k, v := range adding {
		merged[k] = v
	}
	return sortedMapKeys(merged)
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fakeSecretCreatedAt is the creation timestamp the fake stamps. It is
// relative rather than a fixed date so the age the handler computes is
// positive whenever the suite runs, which a hard-coded date stops being on
// whichever side of it the clock happens to fall.
func fakeSecretCreatedAt() time.Time { return time.Now().Add(-90 * time.Minute) }

func secretTargetOf(project, environment string) *kelsonv1alpha1.SecretTarget {
	return &kelsonv1alpha1.SecretTarget{Project: project, Environment: environment}
}

// --- the happy paths ------------------------------------------------------

// TestSetSecretWritesAndReportsKeysOnly is the whole RPC: the value goes to the
// store, the response reports keys, and no field of the response could hold a
// value even if a handler wanted to send one.
func TestSetSecretWritesAndReportsKeysOnly(t *testing.T) {
	store := newFakeSecrets()
	c := serve(t, Options{Secrets: store})

	res, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("checkout", "production"),
		Name:   "checkout-db",
		Values: map[string]string{"url": "postgres://user:pw@db/checkout", "api-key": "sk-live-0001"},
	}))
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if got := res.Msg.GetSecret().GetNamespace(); got != "checkout-production" {
		t.Errorf("namespace = %q, want the derived one", got)
	}
	if got := res.Msg.GetWrittenKeys(); len(got) != 2 || got[0] != "api-key" || got[1] != "url" {
		t.Errorf("written_keys = %v, want them sorted and complete", got)
	}
	if res.Msg.GetDryRun() {
		t.Error("a plain set reported itself as a dry run")
	}
	if got := store.held("checkout-production", "checkout-db")["url"]; got != "postgres://user:pw@db/checkout" {
		t.Errorf("the value did not reach the store: %q", got)
	}
	// The masking is a schema property: SecretSummary has no value field, so
	// the assertion is that the response's whole encoded form does not contain
	// one — which it cannot, and this states it.
	assertNoSentinel(t, "SetSecret response", []byte(res.Msg.String()), "postgres://user:pw@db/checkout")
}

// TestListSecretsMasksAndReportsAges: keys and metadata, never a value.
func TestListSecretsMasksAndReportsAges(t *testing.T) {
	store := newFakeSecrets()
	c := serve(t, Options{Secrets: store})
	for name, values := range map[string]map[string]string{
		"checkout-db": {"url": "postgres://list-sentinel-0001"},
		"payments":    {"api-key": "sk-live-list-0002", "webhook": "whsec-list-0003"},
	} {
		if _, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
			Target: secretTargetOf("checkout", "production"), Name: name, Values: values,
		})); err != nil {
			t.Fatalf("SetSecret %s: %v", name, err)
		}
	}

	res, err := c.secrets.ListSecrets(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListSecretsRequest{
		Target: secretTargetOf("checkout", "production"),
	}))
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if got := res.Msg.GetNamespace(); got != "checkout-production" {
		t.Errorf("namespace = %q", got)
	}
	if len(res.Msg.GetSecrets()) != 2 {
		t.Fatalf("listed %d, want 2", len(res.Msg.GetSecrets()))
	}
	if got := res.Msg.GetSecrets()[1].GetKeys(); len(got) != 2 || got[0] != "api-key" {
		t.Errorf("keys = %v, want them sorted", got)
	}
	if res.Msg.GetSecrets()[0].GetCreatedAt() == "" || res.Msg.GetSecrets()[0].GetAgeSeconds() <= 0 {
		t.Errorf("the listing should carry creation time and age: %+v", res.Msg.GetSecrets()[0])
	}
	for _, value := range []string{"postgres://list-sentinel-0001", "sk-live-list-0002", "whsec-list-0003"} {
		assertNoSentinel(t, "ListSecrets response", []byte(res.Msg.String()), value)
	}
}

// TestDeleteSecretRemovesAndThenReportsNotFound.
func TestDeleteSecretRemovesAndThenReportsNotFound(t *testing.T) {
	store := newFakeSecrets()
	c := serve(t, Options{Secrets: store})
	if _, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://delete-0001"},
	})); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	res, err := c.secrets.DeleteSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.DeleteSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
	}))
	if err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if !res.Msg.GetDeleted() || res.Msg.GetNamespace() != "checkout-production" {
		t.Errorf("response = %+v", res.Msg)
	}

	_, err = c.secrets.DeleteSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.DeleteSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("deleting it twice = %v, want CodeNotFound", connect.CodeOf(err))
	}
}

// --- dry run ---------------------------------------------------------------

// TestSetSecretRenderDryRunTouchesNothing is #69's RENDER rung: validate and
// touch nothing. "Nothing" includes the seam — a render dry run that called the
// store to answer would not be one.
func TestSetSecretRenderDryRunTouchesNothing(t *testing.T) {
	store := newFakeSecrets()
	c := serve(t, Options{Secrets: store})

	res, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://render-dry-0001"},
		DryRun: kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if !res.Msg.GetDryRun() {
		t.Error("a render dry run did not say so")
	}
	if got := res.Msg.GetWrittenKeys(); len(got) != 1 || got[0] != "url" {
		t.Errorf("written_keys = %v, want the keys it would write", got)
	}
	if keys := res.Msg.GetSecret().GetKeys(); len(keys) != 0 {
		t.Errorf("a render dry run reported the Secret's keys (%v); it never looked", keys)
	}
	if len(store.sets) != 0 {
		t.Errorf("a render dry run reached the store: %+v", store.sets)
	}
	if store.held("checkout-production", "checkout-db") != nil {
		t.Error("a render dry run wrote")
	}
}

// TestSetSecretServerDryRunReachesTheStoreButStoresNothing is the SERVER rung,
// which is the meaningful one here: the request goes to the API server as a
// server-side dry-run apply, so admission runs and the merge is computed.
func TestSetSecretServerDryRunReachesTheStoreButStoresNothing(t *testing.T) {
	store := newFakeSecrets()
	c := serve(t, Options{Secrets: store})

	res, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://server-dry-0001"},
		DryRun: kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}))
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if !res.Msg.GetDryRun() {
		t.Error("a server dry run did not say so")
	}
	if len(store.sets) != 1 || !store.sets[0].DryRun {
		t.Fatalf("the store saw %+v, want one dry-run set", store.sets)
	}
	if store.held("checkout-production", "checkout-db") != nil {
		t.Error("a server dry run persisted something")
	}
}

// TestDeleteSecretRenderDryRunTouchesNothing.
func TestDeleteSecretRenderDryRunTouchesNothing(t *testing.T) {
	store := newFakeSecrets()
	c := serve(t, Options{Secrets: store})
	if _, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://delete-dry-0001"},
	})); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	res, err := c.secrets.DeleteSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.DeleteSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
		DryRun: kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if res.Msg.GetDeleted() {
		t.Error("a render dry run reported a delete")
	}
	if len(store.dels) != 0 {
		t.Errorf("a render dry run reached the store: %+v", store.dels)
	}
	if store.held("checkout-production", "checkout-db") == nil {
		t.Error("a render dry run deleted the Secret")
	}
}

// --- refusals ---------------------------------------------------------------

// TestSecretRefusalsMapOntoTheirCodes: a caller must be able to tell "your
// request is wrong" from "the cluster's state refuses it", because the two lead
// to different next steps.
func TestSecretRefusalsMapOntoTheirCodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		storeIn error
		request *kelsonv1alpha1.SetSecretRequest
		want    connect.Code
		code    secret.Code
	}{
		{
			name: "no environment",
			request: &kelsonv1alpha1.SetSecretRequest{
				Target: &kelsonv1alpha1.SecretTarget{Project: "checkout"}, Name: "db",
				Values: map[string]string{"url": "x-0001"}},
			want: connect.CodeInvalidArgument, code: secret.ErrInvalidTarget,
		},
		{
			name: "key outside the alphabet",
			request: &kelsonv1alpha1.SetSecretRequest{
				Target: secretTargetOf("checkout", "production"), Name: "db",
				Values: map[string]string{"db/url": "x-0001"}},
			want: connect.CodeInvalidArgument, code: secret.ErrInvalidKey,
		},
		{
			name:    "not managed by kelson",
			storeIn: secret.Error{Code: secret.ErrNotManaged, Resource: "Secret/checkout-production/tls", Message: "not kelson's"},
			request: &kelsonv1alpha1.SetSecretRequest{
				Target: secretTargetOf("checkout", "production"), Name: "tls",
				Values: map[string]string{"url": "x-0001"}},
			want: connect.CodeFailedPrecondition, code: secret.ErrNotManaged,
		},
		{
			name:    "namespace missing",
			storeIn: secret.Error{Code: secret.ErrNamespaceMissing, Resource: "Secret/checkout-production/db", Message: "no namespace"},
			request: &kelsonv1alpha1.SetSecretRequest{
				Target: secretTargetOf("checkout", "production"), Name: "db",
				Values: map[string]string{"url": "x-0001"}},
			want: connect.CodeFailedPrecondition, code: secret.ErrNamespaceMissing,
		},
		{
			name:    "the API server would not answer",
			storeIn: secret.Error{Code: secret.ErrWriteFailed, Resource: "Secret/checkout-production/db", Message: "refused"},
			request: &kelsonv1alpha1.SetSecretRequest{
				Target: secretTargetOf("checkout", "production"), Name: "db",
				Values: map[string]string{"url": "x-0001"}},
			want: connect.CodeUnavailable, code: secret.ErrWriteFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeSecrets()
			store.err = tc.storeIn
			c := serve(t, Options{Secrets: store})
			_, err := c.secrets.SetSecret(context.Background(), connect.NewRequest(tc.request))
			if err == nil {
				t.Fatal("the request was accepted")
			}
			if got := connect.CodeOf(err); got != tc.want {
				t.Errorf("code = %v, want %v (%v)", got, tc.want, err)
			}
			assertWireCode(t, err, string(tc.code))
		})
	}
}

// assertWireCode checks that the structured detail carries the plane's own
// code, which is what an agent branches on.
func assertWireCode(t *testing.T, err error, want string) {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("want a *connect.Error, got %T", err)
	}
	for _, detail := range cerr.Details() {
		value, derr := detail.Value()
		if derr != nil {
			continue
		}
		if wire, ok := value.(*kelsonv1alpha1.Error); ok && wire.GetCode() == want {
			return
		}
	}
	t.Errorf("no structured detail carried code %q: %v", want, cerr.Details())
}

// TestSecretServiceWithoutASeamIsUnimplemented: a server started without a
// cluster says so rather than panicking, the same as every other seam here.
func TestSecretServiceWithoutASeamIsUnimplemented(t *testing.T) {
	c := serve(t, Options{})
	_, err := c.secrets.ListSecrets(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListSecretsRequest{
		Target: secretTargetOf("checkout", "production"),
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("code = %v, want CodeUnimplemented (%v)", connect.CodeOf(err), err)
	}
}

// --- auth --------------------------------------------------------------------

// TestSecretServiceIsGatedByTheSharedPassword is #84's interim gate applied to
// the one RPC that carries credentials. The middleware matches on the route
// prefix, so a new service is covered by construction — this test is what says
// so rather than leaving it to be assumed.
func TestSecretServiceIsGatedByTheSharedPassword(t *testing.T) {
	auth, err := NewAuth(testPassword)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	mux := http.NewServeMux()
	New(Options{Secrets: newFakeSecrets()}).Register(mux)
	auth.Register(mux)
	srv := httptest.NewServer(auth.Middleware(mux))
	t.Cleanup(srv.Close)

	request := connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: secretTargetOf("checkout", "production"), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://gated-0001"},
	})

	anonymous := kelsonv1alpha1connect.NewSecretServiceClient(srv.Client(), srv.URL)
	if _, err := anonymous.SetSecret(context.Background(), request); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("an anonymous SetSecret = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}

	authenticated := kelsonv1alpha1connect.NewSecretServiceClient(srv.Client(), srv.URL,
		connect.WithInterceptors(bearerFor(testPassword)))
	if _, err := authenticated.SetSecret(context.Background(), connect.NewRequest(request.Msg)); err != nil {
		t.Fatalf("an authenticated SetSecret: %v", err)
	}
}

// bearerFor sends the shared password the way a non-browser client does.
type bearerFor string

func (b bearerFor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearerFor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (b bearerFor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
