package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forge"
	"github.com/dafrie/kelson/internal/model"
)

// GitConnectionService served (ADR-0033, issue #248). The store is faked for
// the reason every seam here is faked — the api plane's lint rule forbids the
// Kubernetes clients internal/controlstore's own tests drive — so what these
// assert is the handler: which requests reach the store, which never do, how a
// probe's causes become messages, and that no response can carry a value.

// --- the store seam ----------------------------------------------------------

// fakeConnections is an in-memory [ConnectionStore]. It reproduces the parts of
// the real store's contract the handlers depend on — create refuses a name that
// exists, delete is replayable, UpdateStatus records — and it records every
// call so a test can assert what the handler did *not* do.
type fakeConnections struct {
	mu      sync.Mutex
	items   map[string]*controlstore.StoredConnection
	secrets map[string]controlstore.AuthMaterial
	// secretReads counts ReadAuthSecret calls, which is how the no-secret
	// invariant is asserted from the outside: a List or a Get that touched the
	// Secret would show up here.
	secretReads int
	// observed is every ConnectionObservation UpdateStatus was handed.
	observed  []controlstore.ConnectionObservation
	err       error
	statusErr error
}

func newFakeConnections() *fakeConnections {
	return &fakeConnections{
		items:   map[string]*controlstore.StoredConnection{},
		secrets: map[string]controlstore.AuthMaterial{},
	}
}

// with stores a connection directly, as though it had always been there.
func (f *fakeConnections) with(conn controlstore.StoredConnection) *fakeConnections {
	if conn.Version == "" {
		conn.Version = "1"
	}
	f.items[conn.Name] = &conn
	return f
}

// withSecret gives a connection material for ReadAuthSecret to answer with.
func (f *fakeConnections) withSecret(name string, material controlstore.AuthMaterial) *fakeConnections {
	f.secrets[name] = material
	return f
}

func (f *fakeConnections) List(context.Context) ([]controlstore.StoredConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]controlstore.StoredConnection, 0, len(f.items))
	for _, c := range f.items {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeConnections) Get(_ context.Context, name string) (controlstore.StoredConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return controlstore.StoredConnection{}, f.err
	}
	c, ok := f.items[name]
	if !ok {
		return controlstore.StoredConnection{}, controlstore.NotFound("connection/"+name,
			fmt.Sprintf("no connection named %q is stored", name), "list the connections with ListConnections")
	}
	return *c, nil
}

func (f *fakeConnections) Create(_ context.Context, name string, spec model.GitConnectionSpec, opts controlstore.CreateConnectionOptions) (controlstore.StoredConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return controlstore.StoredConnection{}, f.err
	}
	if _, ok := f.items[name]; ok {
		return controlstore.StoredConnection{}, controlstore.VersionConflict("connection/"+name,
			fmt.Sprintf("a connection named %q already exists", name), "delete it and create yours")
	}
	stored := controlstore.StoredConnection{Name: name, Spec: spec, Version: "1"}
	if !opts.DryRun {
		f.items[name] = &stored
	}
	return stored, nil
}

func (f *fakeConnections) Delete(_ context.Context, name string, opts controlstore.DeleteConnectionOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if _, ok := f.items[name]; !ok {
		if opts.IdempotencyKey != "" {
			return nil
		}
		return controlstore.NotFound("connection/"+name, "no connection is stored", "list the connections")
	}
	if !opts.DryRun {
		delete(f.items, name)
	}
	return nil
}

func (f *fakeConnections) UpdateStatus(_ context.Context, name string, obs controlstore.ConnectionObservation) (controlstore.StoredConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, obs)
	if f.statusErr != nil {
		return controlstore.StoredConnection{}, f.statusErr
	}
	c, ok := f.items[name]
	if !ok {
		return controlstore.StoredConnection{}, controlstore.NotFound("connection/"+name, "no connection is stored", "list them")
	}
	c.Status = controlstore.ConnectionStatus{
		Ready: obs.Ready, Reachable: obs.Reachable, Account: obs.Account,
		Repositories: obs.Repositories, Message: obs.Message, Observed: true,
	}
	return *c, nil
}

func (f *fakeConnections) ReadAuthSecret(_ context.Context, conn controlstore.StoredConnection) (controlstore.AuthMaterial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secretReads++
	material, ok := f.secrets[conn.Name]
	if !ok {
		return controlstore.AuthMaterial{}, controlstore.NotFound("connection/"+conn.Name,
			"the Secret this connection references does not exist",
			"create it, or point the connection at the Secret that holds the credential")
	}
	return material, nil
}

func (f *fakeConnections) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secretReads
}

// --- the forge seam ----------------------------------------------------------

// fakeForge is a [forge.Provider] with scripted answers. It implements
// RepoBrowser only when repos or listErr is set, which is how the capability
// split of ADR-0033 decision 3 is driven from a test: a provider without a
// browser is a *different type*, not a flag.
type fakeForge struct {
	name    string
	cred    forge.Credential
	mintErr error
}

func (f fakeForge) Name() string { return f.name }

func (f fakeForge) MintCloneCredential(context.Context, forge.Conn, string) (forge.Credential, error) {
	if f.mintErr != nil {
		return forge.Credential{}, f.mintErr
	}
	return f.cred, nil
}

type browsingForge struct {
	fakeForge
	repos    []forge.Repo
	branches []string
	listErr  error
	// conns records the forge.Conn each call was handed, so a test can assert
	// the material reached the adapter.
	conns *[]forge.Conn
	// repositories records the repository each ListBranches was asked about.
	repositories *[]string
}

func (f browsingForge) ListRepositories(_ context.Context, c forge.Conn) ([]forge.Repo, error) {
	if f.conns != nil {
		*f.conns = append(*f.conns, c)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.repos, nil
}

func (f browsingForge) ListBranches(_ context.Context, c forge.Conn, repoFullName string) ([]string, error) {
	if f.conns != nil {
		*f.conns = append(*f.conns, c)
	}
	if f.repositories != nil {
		*f.repositories = append(*f.repositories, repoFullName)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.branches, nil
}

func forgeLookup(p forge.Provider) ForgeLookup {
	return func(string) (forge.Provider, bool) { return p, p != nil }
}

// --- fixtures ----------------------------------------------------------------

func tokenConnection(name, secretRef string) controlstore.StoredConnection {
	return controlstore.StoredConnection{
		Name: name,
		Spec: model.GitConnectionSpec{
			Provider: model.GitProviderGitHub,
			Auth:     model.GitConnectionAuth{Token: &model.TokenAuth{SecretRef: secretRef}},
		},
	}
}

func appConnection(name, secretRef string) controlstore.StoredConnection {
	return controlstore.StoredConnection{
		Name: name,
		Spec: model.GitConnectionSpec{
			Provider: model.GitProviderGitHub,
			Auth: model.GitConnectionAuth{GitHubApp: &model.GitHubAppAuth{
				AppID: 12345, InstallationID: 678910, SecretRef: secretRef,
			}},
		},
	}
}

// --- reads -------------------------------------------------------------------

func TestListConnectionsProjectsTheStore(t *testing.T) {
	store := newFakeConnections().
		with(tokenConnection("zeta", "zeta-token")).
		with(appConnection("acme-github", "acme-github-app"))
	store.items["acme-github"].Status = controlstore.ConnectionStatus{
		Ready: true, Reachable: true, Account: "acme", Repositories: 42,
		Message: "github.com answered", Observed: true,
	}
	c := serve(t, Options{Connections: store})

	res, err := c.connections.ListConnections(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatalf("ListConnections: %v", err)
	}
	got := res.Msg.GetConnections()
	if len(got) != 2 || got[0].GetName() != "acme-github" || got[1].GetName() != "zeta" {
		t.Fatalf("connections came back as %+v, want acme-github then zeta", got)
	}

	app := got[0]
	if app.GetAuthKind() != kelsonv1alpha1.GitAuthKind_GIT_AUTH_KIND_GITHUB_APP {
		t.Errorf("auth kind = %v, want GITHUB_APP", app.GetAuthKind())
	}
	if app.GetAppId() != 12345 || app.GetInstallationId() != 678910 {
		t.Errorf("the app identifiers came back as %d/%d", app.GetAppId(), app.GetInstallationId())
	}
	if app.GetSecretRef() != "acme-github-app" {
		t.Errorf("secret_ref = %q, want the Secret's *name*", app.GetSecretRef())
	}
	// An omitted host means github.com, and every surface must answer that the
	// same way or host-match resolution and the UI would disagree.
	if app.GetHost() != model.DefaultGitHubHost {
		t.Errorf("host = %q, want the provider default %q", app.GetHost(), model.DefaultGitHubHost)
	}
	if app.GetOwner().GetKind() != kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_INSTANCE {
		t.Errorf("owner kind = %v, want INSTANCE for a connection that named none", app.GetOwner().GetKind())
	}
	if !app.GetReady() || !app.GetReachable() || app.GetAccount() != "acme" || app.GetRepositories() != 42 {
		t.Errorf("the observed half came back as %+v", app)
	}

	// The never-probed one must be distinguishable from a failed probe. Both
	// booleans are false either way; the message is what separates them.
	if unprobed := got[1]; !strings.Contains(unprobed.GetMessage(), "not yet observed") {
		t.Errorf("an unprobed connection reports %q, which a client cannot tell from a failure", unprobed.GetMessage())
	}
}

// The invariant gitconnection.proto states as a schema property, asserted as a
// behavioural one: the reads never open the Secret. The handler could not do it
// even if it tried — `Server.connections` has no such method — and this is the
// test that fails if that narrowing is ever widened.
func TestReadingConnectionsNeverTouchesTheSecret(t *testing.T) {
	store := newFakeConnections().
		with(tokenConnection("acme-github", "acme-git-token")).
		withSecret("acme-github", controlstore.AuthMaterial{Token: "ghp-not-a-real-token"})
	c := serve(t, Options{Connections: store})

	if _, err := c.connections.ListConnections(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListConnectionsRequest{})); err != nil {
		t.Fatalf("ListConnections: %v", err)
	}
	res, err := c.connections.GetConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.GetConnectionRequest{Name: "acme-github"}))
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if store.reads() != 0 {
		t.Fatalf("a read path called ReadAuthSecret %d times", store.reads())
	}

	// And the response itself: every string field, checked against the value
	// the Secret holds. There is no field it could be in, and this is what
	// would notice if one were ever added.
	conn := res.Msg.GetConnection()
	for _, field := range []string{
		conn.GetName(), conn.GetProvider(), conn.GetHost(), conn.GetSecretRef(),
		conn.GetAccount(), conn.GetMessage(), conn.GetOwner().GetName(),
	} {
		if strings.Contains(field, "ghp-not-a-real-token") {
			t.Fatalf("a connection field carried the credential: %q", field)
		}
	}
	if conn.GetSecretRef() != "acme-git-token" {
		t.Errorf("secret_ref = %q, want the reference", conn.GetSecretRef())
	}
}

func TestGetConnectionNotFound(t *testing.T) {
	c := serve(t, Options{Connections: newFakeConnections()})
	_, err := c.connections.GetConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.GetConnectionRequest{Name: "ghost"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetConnection on a missing connection = %v (code %s), want not-found", err, connect.CodeOf(err))
	}
}

func TestConnectionServiceWithoutAStoreIsUnimplemented(t *testing.T) {
	c := serve(t, Options{})
	calls := map[string]func() error{
		"ListConnections": func() error {
			_, err := c.connections.ListConnections(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListConnectionsRequest{}))
			return err
		},
		"GetConnection": func() error {
			_, err := c.connections.GetConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.GetConnectionRequest{Name: "x"}))
			return err
		},
		"CreateConnection": func() error {
			_, err := c.connections.CreateConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateConnectionRequest{
				Name: "acme-github", Provider: "github", Host: "https://github.com", SecretRef: "acme-git-token",
			}))
			return err
		},
		"DeleteConnection": func() error {
			_, err := c.connections.DeleteConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "x"}))
			return err
		},
		"TestConnection": func() error {
			_, err := c.connections.TestConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.TestConnectionRequest{Name: "x"}))
			return err
		},
	}
	for name, call := range calls {
		if code := connect.CodeOf(call()); code != connect.CodeUnimplemented {
			t.Errorf("%s on a server with no connection store = %s, want unimplemented", name, code)
		}
	}
}

// --- create ------------------------------------------------------------------

func TestCreateConnectionStoresATokenConnection(t *testing.T) {
	store := newFakeConnections()
	c := serve(t, Options{Connections: store})

	res, err := c.connections.CreateConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateConnectionRequest{
		Name:      "acme-github",
		Provider:  "github",
		Host:      "https://github.com",
		SecretRef: "acme-git-token",
	}))
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if res.Msg.GetDryRun() {
		t.Error("a real create reported dry_run")
	}
	conn := res.Msg.GetConnection()
	if conn.GetAuthKind() != kelsonv1alpha1.GitAuthKind_GIT_AUTH_KIND_TOKEN {
		t.Errorf("auth kind = %v, want TOKEN: this RPC makes token connections and nothing else", conn.GetAuthKind())
	}
	stored, err := store.Get(t.Context(), "acme-github")
	if err != nil {
		t.Fatalf("the connection was not stored: %v", err)
	}
	if stored.Spec.Auth.Token == nil || stored.Spec.Auth.Token.SecretRef != "acme-git-token" {
		t.Errorf("the stored spec is %+v, want token auth naming the Secret", stored.Spec)
	}
	if stored.Spec.Owner != nil {
		t.Errorf("an unset owner was stored as %+v, want nil — the instance is the default", stored.Spec.Owner)
	}
}

// Validation runs before the store is touched, through the same model
// validator the CLI runs. The refusals are the model's own codes.
func TestCreateConnectionRefusesAnInvalidRequest(t *testing.T) {
	cases := []struct {
		name    string
		req     *kelsonv1alpha1.CreateConnectionRequest
		wantsIn string
	}{{
		name:    "no name",
		req:     &kelsonv1alpha1.CreateConnectionRequest{Provider: "github", SecretRef: "acme-git-token"},
		wantsIn: "needs a name",
	}, {
		name:    "no secret reference",
		req:     &kelsonv1alpha1.CreateConnectionRequest{Name: "acme-github", Provider: "github"},
		wantsIn: "secret_ref",
	}, {
		name:    "no provider",
		req:     &kelsonv1alpha1.CreateConnectionRequest{Name: "acme-github", SecretRef: "acme-git-token"},
		wantsIn: "provider",
	}, {
		name: "a provider no adapter serves",
		req: &kelsonv1alpha1.CreateConnectionRequest{
			Name: "acme-gitlab", Provider: "gitlab", Host: "https://gitlab.com", SecretRef: "t"},
		wantsIn: "gitlab",
	}, {
		// Only `provider: github` may omit the host, because only it has a
		// default worth writing down.
		name:    "a generic connection with no host",
		req:     &kelsonv1alpha1.CreateConnectionRequest{Name: "acme", Provider: "generic", SecretRef: "t"},
		wantsIn: "host",
	}, {
		name: "a name that is not a DNS label",
		req: &kelsonv1alpha1.CreateConnectionRequest{
			Name: "Acme GitHub", Provider: "github", SecretRef: "t"},
		wantsIn: "name",
	}, {
		name: "an instance-owned connection naming a principal",
		req: &kelsonv1alpha1.CreateConnectionRequest{
			Name: "acme-github", Provider: "github", SecretRef: "t",
			Owner: &kelsonv1alpha1.GitOwner{
				Kind: kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_INSTANCE, Name: "alice"}},
		wantsIn: "names no principal",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeConnections()
			c := serve(t, Options{Connections: store})
			_, err := c.connections.CreateConnection(t.Context(), connect.NewRequest(tc.req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("err = %v (code %s), want invalid-argument", err, connect.CodeOf(err))
			}
			if !strings.Contains(err.Error(), tc.wantsIn) {
				t.Errorf("the refusal does not mention %q: %v", tc.wantsIn, err)
			}
			if len(store.items) != 0 {
				t.Errorf("a refused create stored %d connections", len(store.items))
			}
		})
	}
}

func TestCreateConnectionDryRunStoresNothing(t *testing.T) {
	for name, dry := range map[string]kelsonv1alpha1.DryRun{
		"render": kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
		"server": kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeConnections()
			c := serve(t, Options{Connections: store})
			res, err := c.connections.CreateConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateConnectionRequest{
				Name: "acme-github", Provider: "github", SecretRef: "acme-git-token", DryRun: dry,
			}))
			if err != nil {
				t.Fatalf("dry-run create: %v", err)
			}
			if !res.Msg.GetDryRun() {
				t.Error("dry_run is false on a dry run")
			}
			if res.Msg.GetConnection().GetName() != "acme-github" {
				t.Errorf("the dry run answered %+v, want the connection it validated", res.Msg.GetConnection())
			}
			if len(store.items) != 0 {
				t.Errorf("a dry run stored %d connections", len(store.items))
			}
		})
	}
	// A RENDER dry run validates and never reaches the store, which is what
	// makes it answerable by a server with none.
	c := serve(t, Options{})
	if _, err := c.connections.CreateConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateConnectionRequest{
		Name: "acme-github", Provider: "github", SecretRef: "acme-git-token",
		DryRun: kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	})); err != nil {
		t.Errorf("a render dry run on a server with no store = %v, want an answer", err)
	}
}

func TestCreateConnectionRefusesAnExistingName(t *testing.T) {
	store := newFakeConnections().with(tokenConnection("acme-github", "first"))
	c := serve(t, Options{Connections: store})
	_, err := c.connections.CreateConnection(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateConnectionRequest{
		Name: "acme-github", Provider: "github", SecretRef: "second",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v (code %s), want failed-precondition", err, connect.CodeOf(err))
	}
}

// projectDocument is a Project as PutSpec would have accepted one: valid,
// with a source and optionally the explicit connection override.
func projectDocument(name, git, connection string) []byte {
	doc := fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: %s}
spec:
  image: ghcr.io/acme/%s:v1
  components:
    - name: web
      kind: service
      port: 8080
  source:
    git: %s
    ref: main
`, name, name, git)
	if connection != "" {
		doc += "    connection: " + connection + "\n"
	}
	return []byte(doc)
}

// --- delete ------------------------------------------------------------------

// A project reaches a connection two ways, and both must be named: by explicit
// `source.connection`, and by host match. The explicit one wins where they
// disagree, which is what this fixture is built to show.
func TestDeleteConnectionNamesTheProjectsItServed(t *testing.T) {
	specs := newFakeSpecStore()
	for _, p := range []struct{ name, git, connection string }{
		{"checkout", "https://github.com/acme/checkout", ""},
		{"billing", "https://github.com/globex/billing", ""},
		{"shop", "https://github.com/globex/shop", "acme-github"},
		{"site", "https://gitlab.com/acme/site", ""},
	} {
		doc := projectDocument(p.name, p.git, p.connection)
		if _, err := specs.Put(t.Context(), p.name,
			controlstore.Documents{Project: doc}, controlstore.PutOptions{}); err != nil {
			t.Fatalf("storing %s: %v", p.name, err)
		}
	}
	// One project whose document is no longer readable. It must not stop the
	// scan: a single broken spec that made every connection un-deletable would
	// be the worst possible way to learn a credential had leaked.
	if _, err := specs.Put(t.Context(), "broken",
		controlstore.Documents{Project: []byte("this: is: not: a document")}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the broken project: %v", err)
	}

	store := newFakeConnections().
		with(tokenConnection("acme-github", "acme-git-token")).
		with(tokenConnection("globex-github", "globex-git-token"))
	store.items["acme-github"].Status = controlstore.ConnectionStatus{Account: "acme", Observed: true}
	store.items["globex-github"].Status = controlstore.ConnectionStatus{Account: "globex", Observed: true}

	c := serve(t, Options{Connections: store, Specs: specs})
	res, err := c.connections.DeleteConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "acme-github"}))
	if err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	if !res.Msg.GetDeleted() {
		t.Error("deleted is false on a real delete")
	}
	want := []string{"checkout", "shop"}
	if got := res.Msg.GetAffectedProjects(); !reflect.DeepEqual(got, want) {
		t.Fatalf("affected_projects = %v, want %v (checkout by host, shop by name)", got, want)
	}
	if _, err := store.Get(t.Context(), "acme-github"); err == nil {
		t.Error("the connection survived the delete")
	}
}

// pluralProjectDocument declares the same thing the other way: `spec.sources`,
// one name per entry (ADR-0035 decision 1). A project spelling it this way used
// to be invisible to the scan, so revoking a credential said "no projects
// affected" while every build in it was about to fail.
func pluralProjectDocument(name string, sources ...model.Source) []byte {
	doc := fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: %s}
spec:
  image: ghcr.io/acme/%s:v1
  components:
    - name: web
      kind: service
      port: 8080
      source: %s
  sources:
`, name, name, sources[0].Name)
	for _, src := range sources {
		doc += fmt.Sprintf("    - name: %s\n      git: %s\n      ref: main\n", src.Name, src.Git)
		if src.Connection != "" {
			doc += "      connection: " + src.Connection + "\n"
		}
	}
	return []byte(doc)
}

// boundProjectDocument declares no sources at all: its component binds by name
// to whatever the instance offers (ADR-0035 decisions 2 and 3).
func boundProjectDocument(name, source string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: %s}
spec:
  image: ghcr.io/acme/%s:v1
  components:
    - name: web
      kind: service
      port: 8080
      source: %s
`, name, name, source))
}

// Both spellings are one declaration, so both must be seen. The miss case is
// asserted in the same fixture: a project whose sources are all on another
// forge is not named, which is what makes the two that *are* named evidence.
func TestDeleteConnectionSeesBothSourceSpellings(t *testing.T) {
	specs := newFakeSpecStore()
	docs := map[string][]byte{
		// The singular spelling, matched by host.
		"checkout": projectDocument("checkout", "https://github.com/acme/checkout", ""),
		// The plural spelling: the connection serves the second entry, which the
		// old scan never looked at.
		"platform": pluralProjectDocument("platform",
			model.Source{Name: "default", Git: "https://gitlab.com/acme/site"},
			model.Source{Name: "tools", Git: "https://github.com/acme/build-tools"}),
		// The plural spelling with an explicit connection, against a repository
		// the host match would have given to nobody.
		"billing": pluralProjectDocument("billing",
			model.Source{Name: "app", Git: "https://git.acme.internal/acme/billing", Connection: "acme-github"}),
		// The miss: declared, plural, and nothing here is on this connection's
		// host.
		"site": pluralProjectDocument("site",
			model.Source{Name: "default", Git: "https://gitlab.com/acme/site"}),
	}
	for name, doc := range docs {
		if _, err := specs.Put(t.Context(), name,
			controlstore.Documents{Project: doc}, controlstore.PutOptions{}); err != nil {
			t.Fatalf("storing %s: %v", name, err)
		}
	}

	store := newFakeConnections().with(tokenConnection("acme-github", "acme-git-token"))
	c := serve(t, Options{Connections: store, Specs: specs})

	res, err := c.connections.DeleteConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "acme-github"}))
	if err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	want := []string{"billing", "checkout", "platform"}
	if got := res.Msg.GetAffectedProjects(); !reflect.DeepEqual(got, want) {
		t.Fatalf("affected_projects = %v, want %v — the plural spelling declares repositories too", got, want)
	}
}

// A component bound to a GitSource reaches that repository, so the project is
// named. The counting rule is narrower than for a project's own sources — an
// instance's tier is offered to everybody, so it counts where a component is
// *bound* — and a project-local name shadows the global one it hides.
func TestDeleteConnectionNamesProjectsBoundToAGitSource(t *testing.T) {
	specs := newFakeSpecStore()
	docs := map[string][]byte{
		// Binds to the instance's `tools`, which lives on the connection's host.
		"worker": boundProjectDocument("worker", "tools"),
		// Declares its own `tools` on another forge, which shadows the global
		// one: this project never reaches the connection.
		"shadow": pluralProjectDocument("shadow",
			model.Source{Name: "tools", Git: "https://gitlab.com/acme/our-own-tools"}),
	}
	for name, doc := range docs {
		if _, err := specs.Put(t.Context(), name,
			controlstore.Documents{Project: doc}, controlstore.PutOptions{}); err != nil {
			t.Fatalf("storing %s: %v", name, err)
		}
	}

	store := newFakeConnections().with(tokenConnection("acme-github", "acme-git-token"))
	c := serve(t, Options{
		Connections: store,
		Specs:       specs,
		GitSources: fakeGitSources{sources: []model.Source{
			{Name: "tools", Git: "https://github.com/acme/build-tools", Ref: "v2"},
		}},
	})

	res, err := c.connections.DeleteConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "acme-github"}))
	if err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	if got := res.Msg.GetAffectedProjects(); !reflect.DeepEqual(got, []string{"worker"}) {
		t.Fatalf("affected_projects = %v, want [worker] — shadow declares its own tools elsewhere", got)
	}
}

// A GitSource listing that failed is not an instance with no GitSources.
// Answering "nothing is affected" out of a read that did not happen would be
// the warning lying in the one direction that matters.
func TestDeleteConnectionRefusesWhenTheGlobalTierCannotBeRead(t *testing.T) {
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "worker", controlstore.Documents{
		Project: boundProjectDocument("worker", "tools"),
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	store := newFakeConnections().with(tokenConnection("acme-github", "acme-git-token"))
	c := serve(t, Options{
		Connections: store,
		Specs:       specs,
		GitSources:  fakeGitSources{err: errors.New("the API server said no")},
	})

	_, err := c.connections.DeleteConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "acme-github"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v (code %s), want unavailable", err, connect.CodeOf(err))
	}
	if _, err := store.Get(t.Context(), "acme-github"); err != nil {
		t.Errorf("the connection was deleted despite the refusal: %v", err)
	}
}

// The delete is not blocked by the projects it names: a connection is deleted
// because it is wrong, and refusing until every project is edited would make a
// leaked credential harder to revoke than to keep.
func TestDeleteConnectionIsNotBlockedByAffectedProjects(t *testing.T) {
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project: projectDocument("checkout", "https://github.com/acme/checkout", ""),
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	store := newFakeConnections().with(tokenConnection("acme-github", "acme-git-token"))
	c := serve(t, Options{Connections: store, Specs: specs})

	res, err := c.connections.DeleteConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "acme-github"}))
	if err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	if !res.Msg.GetDeleted() || len(res.Msg.GetAffectedProjects()) != 1 {
		t.Fatalf("the delete answered %+v, want deleted with one project named", res.Msg)
	}
}

// A dry run previews the blast radius and removes nothing. Withholding
// affected_projects would make the rehearsal useless, so both rungs report it.
func TestDeleteConnectionDryRunReportsTheBlastRadius(t *testing.T) {
	for name, dry := range map[string]kelsonv1alpha1.DryRun{
		"render": kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
		"server": kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	} {
		t.Run(name, func(t *testing.T) {
			specs := newFakeSpecStore()
			if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
				Project: projectDocument("checkout", "https://github.com/acme/checkout", ""),
			}, controlstore.PutOptions{}); err != nil {
				t.Fatalf("storing the project: %v", err)
			}
			store := newFakeConnections().with(tokenConnection("acme-github", "acme-git-token"))
			c := serve(t, Options{Connections: store, Specs: specs})

			res, err := c.connections.DeleteConnection(t.Context(),
				connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "acme-github", DryRun: dry}))
			if err != nil {
				t.Fatalf("dry-run delete: %v", err)
			}
			if res.Msg.GetDeleted() {
				t.Error("deleted is true on a dry run")
			}
			if got := res.Msg.GetAffectedProjects(); !reflect.DeepEqual(got, []string{"checkout"}) {
				t.Errorf("affected_projects = %v, want [checkout] — a dry run that hid it would rehearse nothing", got)
			}
			if _, err := store.Get(t.Context(), "acme-github"); err != nil {
				t.Errorf("the dry run removed the connection: %v", err)
			}
		})
	}
}

func TestDeleteConnectionNotFound(t *testing.T) {
	c := serve(t, Options{Connections: newFakeConnections()})
	_, err := c.connections.DeleteConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.DeleteConnectionRequest{Name: "ghost"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v (code %s), want not-found", err, connect.CodeOf(err))
	}
}

// --- the probe ---------------------------------------------------------------

func TestTestConnectionReportsWhatTheForgeSaid(t *testing.T) {
	var seen []forge.Conn
	store := newFakeConnections().
		with(appConnection("acme-github", "acme-github-app")).
		withSecret("acme-github", controlstore.AuthMaterial{
			SecretRef: "acme-github-app", PrivateKeyPEM: []byte("-----BEGIN RSA PRIVATE KEY-----")})
	c := serve(t, Options{
		Connections: store,
		Forges: forgeLookup(browsingForge{
			fakeForge: fakeForge{name: "github", cred: forge.Credential{Username: "x-access-token", Password: "ghs-minted"}},
			repos: []forge.Repo{
				{FullName: "acme/checkout"}, {FullName: "acme/billing"}, {FullName: "acme/site"},
			},
			conns: &seen,
		}),
	})

	res, err := c.connections.TestConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.TestConnectionRequest{Name: "acme-github"}))
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !res.Msg.GetReachable() || res.Msg.GetAccount() != "acme" || res.Msg.GetRepositories() != 3 {
		t.Fatalf("the probe answered %+v, want reachable acme with 3 repositories", res.Msg)
	}
	if !strings.Contains(res.Msg.GetMessage(), "acme") {
		t.Errorf("the summary does not name the account: %q", res.Msg.GetMessage())
	}
	// The minted credential is used and never returned.
	if strings.Contains(res.Msg.GetMessage(), "ghs-minted") {
		t.Fatal("the probe's message carried the minted credential")
	}

	// The material reached the adapter, which is the only place it may go.
	if len(seen) != 1 || len(seen[0].PrivateKeyPEM) == 0 || seen[0].AppID != 12345 || seen[0].InstallationID != 678910 {
		t.Fatalf("the adapter was handed %+v, want the app's key and identifiers", seen)
	}

	// And the outcome was written to the CR's status, so `kubectl get
	// gitconnections` agrees with what the caller was just told.
	if len(store.observed) != 1 {
		t.Fatalf("UpdateStatus was called %d times, want once", len(store.observed))
	}
	obs := store.observed[0]
	if !obs.Ready || !obs.Reachable || obs.Account != "acme" || obs.Repositories != 3 {
		t.Errorf("the recorded observation is %+v", obs)
	}
	stored, err := store.Get(t.Context(), "acme-github")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !stored.Status.Reachable || stored.Status.Account != "acme" {
		t.Errorf("the connection's status is %+v", stored.Status)
	}
}

// Every failure is a *cause*, not one generic string: the remedies have nothing
// in common, so the message must say which one applies.
func TestTestConnectionExplainsEachCause(t *testing.T) {
	cases := []struct {
		name          string
		conn          controlstore.StoredConnection
		secret        *controlstore.AuthMaterial
		provider      forge.Provider
		wantReachable bool
		wantReady     bool
		wantsIn       []string
	}{{
		name:      "the forge rejected the credential",
		conn:      tokenConnection("acme-github", "acme-git-token"),
		secret:    &controlstore.AuthMaterial{Token: "expired"},
		provider:  fakeForge{name: "github", mintErr: fmt.Errorf("401: %w", forge.ErrAuthFailed)},
		wantReady: true,
		wantsIn:   []string{"rejected the credential", "acme-git-token"},
	}, {
		name:      "the app is not installed",
		conn:      appConnection("acme-github", "acme-github-app"),
		secret:    &controlstore.AuthMaterial{PrivateKeyPEM: []byte("key")},
		provider:  fakeForge{name: "github", mintErr: fmt.Errorf("404: %w", forge.ErrNotInstalled)},
		wantReady: true,
		wantsIn:   []string{"no installation", "Install it"},
	}, {
		name:      "the forge did not answer",
		conn:      tokenConnection("acme-github", "acme-git-token"),
		secret:    &controlstore.AuthMaterial{Token: "fine"},
		provider:  fakeForge{name: "github", mintErr: context.DeadlineExceeded},
		wantReady: true,
		wantsIn:   []string{"did not answer", "not a credential fault"},
	}, {
		name:     "the Secret is missing",
		conn:     tokenConnection("acme-github", "acme-git-token"),
		provider: fakeForge{name: "github"},
		wantsIn:  []string{"does not exist"},
	}, {
		name:     "no adapter serves the provider",
		conn:     controlstore.StoredConnection{Name: "acme-gitlab", Spec: model.GitConnectionSpec{Provider: "gitlab", Host: "https://gitlab.com"}},
		provider: nil,
		wantsIn:  []string{"no adapter", "gitlab"},
	}, {
		// The credential worked; the listing did not. The deploy path needs
		// only the first, so this stays reachable.
		name:   "the listing failed after the credential worked",
		conn:   tokenConnection("acme-github", "acme-git-token"),
		secret: &controlstore.AuthMaterial{Token: "fine"},
		provider: browsingForge{
			fakeForge: fakeForge{name: "github", cred: forge.Credential{Username: "x", Password: "p"}},
			listErr:   errors.New("connection reset by peer"),
		},
		wantReachable: true,
		wantReady:     true,
		wantsIn:       []string{"Clones and builds through this connection are unaffected"},
	}, {
		// A generic token connection has no repository browser, which is a
		// capability this forge does not offer and not a fault.
		name:          "the provider has no repository browser",
		conn:          tokenConnection("acme-git", "acme-git-token"),
		secret:        &controlstore.AuthMaterial{Token: "fine"},
		provider:      fakeForge{name: "generic", cred: forge.Credential{Username: "x", Password: "p"}},
		wantReachable: true,
		wantReady:     true,
		wantsIn:       []string{"no repository browser", "not a fault"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeConnections().with(tc.conn)
			if tc.secret != nil {
				store.withSecret(tc.conn.Name, *tc.secret)
			}
			c := serve(t, Options{Connections: store, Forges: forgeLookup(tc.provider)})

			res, err := c.connections.TestConnection(t.Context(),
				connect.NewRequest(&kelsonv1alpha1.TestConnectionRequest{Name: tc.conn.Name}))
			if err != nil {
				t.Fatalf("TestConnection: %v", err)
			}
			if res.Msg.GetReachable() != tc.wantReachable {
				t.Errorf("reachable = %v, want %v (%q)", res.Msg.GetReachable(), tc.wantReachable, res.Msg.GetMessage())
			}
			for _, want := range tc.wantsIn {
				if !strings.Contains(res.Msg.GetMessage(), want) {
					t.Errorf("the message does not say %q: %q", want, res.Msg.GetMessage())
				}
			}
			if len(store.observed) != 1 {
				t.Fatalf("UpdateStatus was called %d times, want once — a probe that answered must be recorded",
					len(store.observed))
			}
			if store.observed[0].Ready != tc.wantReady {
				t.Errorf("Ready was recorded as %v, want %v", store.observed[0].Ready, tc.wantReady)
			}
		})
	}
}

// A status write that fails does not fail the probe: the answer was obtained,
// and the caller is told the record could not be kept rather than losing both.
func TestTestConnectionSurvivesAFailedStatusWrite(t *testing.T) {
	store := newFakeConnections().
		with(tokenConnection("acme-github", "acme-git-token")).
		withSecret("acme-github", controlstore.AuthMaterial{Token: "fine"})
	store.statusErr = errors.New("the API server said no")
	c := serve(t, Options{
		Connections: store,
		Forges:      forgeLookup(fakeForge{name: "generic", cred: forge.Credential{Username: "x", Password: "p"}}),
	})

	res, err := c.connections.TestConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.TestConnectionRequest{Name: "acme-github"}))
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !res.Msg.GetReachable() {
		t.Error("the probe reported unreachable because its result could not be recorded")
	}
	if !strings.Contains(res.Msg.GetMessage(), "could not be recorded") {
		t.Errorf("the caller was not told the status write failed: %q", res.Msg.GetMessage())
	}
}

func TestTestConnectionOnAMissingConnection(t *testing.T) {
	c := serve(t, Options{Connections: newFakeConnections()})
	_, err := c.connections.TestConnection(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.TestConnectionRequest{Name: "ghost"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v (code %s), want not-found", err, connect.CodeOf(err))
	}
}

// --- browsing ----------------------------------------------------------------
//
// The capability gate of ADR-0033 decision 3, on the wire. What these assert is
// the shape of the *refusal* as much as the shape of the answer: a connection
// that cannot browse has to be distinguishable from one that browsed and found
// nothing, and that distinction is the whole reason these two RPCs exist.

func TestListConnectionRepositoriesReportsWhatTheForgeSaw(t *testing.T) {
	store := newFakeConnections().
		with(appConnection("acme-github", "acme-github-app")).
		withSecret("acme-github", controlstore.AuthMaterial{PrivateKeyPEM: []byte("-----BEGIN PRIVATE KEY-----")})
	var seen []forge.Conn
	c := serve(t, Options{Connections: store, Forges: forgeLookup(browsingForge{
		fakeForge: fakeForge{name: "github"},
		conns:     &seen,
		repos: []forge.Repo{
			{FullName: "acme/checkout", HTMLURL: "https://github.com/acme/checkout", DefaultBranch: "main", Private: true},
			{FullName: "acme/site", HTMLURL: "https://github.com/acme/site", DefaultBranch: "trunk"},
		},
	})})

	res, err := c.connections.ListConnectionRepositories(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionRepositoriesRequest{Connection: "acme-github"}))
	if err != nil {
		t.Fatalf("ListConnectionRepositories: %v", err)
	}
	got := res.Msg.GetRepositories()
	if len(got) != 2 {
		t.Fatalf("got %d repositories, want the 2 the adapter reported", len(got))
	}
	// Every field of the seam's Repo has to survive the projection: a picker
	// that lost default_branch would preselect nothing, and one that lost
	// private would show a public badge on a private repository.
	first := got[0]
	if first.GetFullName() != "acme/checkout" || first.GetHtmlUrl() != "https://github.com/acme/checkout" ||
		first.GetDefaultBranch() != "main" || !first.GetPrivate() {
		t.Errorf("the first repository came back as %+v", first)
	}
	if got[1].GetPrivate() {
		t.Error("a public repository came back private")
	}

	// The material reached the adapter, joined onto the spec's identifiers —
	// which is what forgeConn exists to do and what a listing that silently
	// authenticated as nobody would get wrong.
	if len(seen) != 1 {
		t.Fatalf("the adapter was called %d times, want once", len(seen))
	}
	if len(seen[0].PrivateKeyPEM) == 0 || seen[0].AppID != 12345 || seen[0].InstallationID != 678910 {
		t.Errorf("the adapter was handed %+v, want the app's identifiers and its key", seen[0])
	}
}

func TestListConnectionBranchesNamesTheRepositoryItWasAsked(t *testing.T) {
	store := newFakeConnections().
		with(tokenConnection("acme-github", "acme-git-token")).
		withSecret("acme-github", controlstore.AuthMaterial{Token: "ghp-not-a-real-token"})
	var repositories []string
	c := serve(t, Options{Connections: store, Forges: forgeLookup(browsingForge{
		fakeForge:    fakeForge{name: "github"},
		repositories: &repositories,
		branches:     []string{"main", "release/1.4"},
	})})

	res, err := c.connections.ListConnectionBranches(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionBranchesRequest{
			Connection: "acme-github", Repository: "acme/checkout",
		}))
	if err != nil {
		t.Fatalf("ListConnectionBranches: %v", err)
	}
	if !reflect.DeepEqual(res.Msg.GetBranches(), []string{"main", "release/1.4"}) {
		t.Errorf("branches = %v, want the adapter's answer unchanged", res.Msg.GetBranches())
	}
	if !reflect.DeepEqual(repositories, []string{"acme/checkout"}) {
		t.Errorf("the adapter was asked about %v, want acme/checkout", repositories)
	}
	// The response is branch names, and a name is not a credential — but this
	// is the one browsing response built from a request the caller controls, so
	// the token is checked out of it explicitly.
	for _, branch := range res.Msg.GetBranches() {
		if strings.Contains(branch, "ghp-not-a-real-token") {
			t.Fatalf("a branch name carried the credential: %q", branch)
		}
	}
}

func TestListConnectionBranchesNeedsARepository(t *testing.T) {
	store := newFakeConnections().
		with(tokenConnection("acme-github", "acme-git-token")).
		withSecret("acme-github", controlstore.AuthMaterial{Token: "t"})
	c := serve(t, Options{Connections: store, Forges: forgeLookup(browsingForge{
		fakeForge: fakeForge{name: "github"},
	})})

	_, err := c.connections.ListConnectionBranches(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionBranchesRequest{Connection: "acme-github"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v (code %s), want invalid-argument", err, connect.CodeOf(err))
	}
	if store.reads() != 0 {
		t.Errorf("a request refused on its own shape read the Secret %d times", store.reads())
	}
}

// The capability gate itself: a provider with no RepoBrowser refuses, and the
// refusal is a *structured* one an agent and a browser can both act on.
//
// Three things are asserted about it and each is load-bearing. The connect code
// is Unimplemented and not InvalidArgument, because no rewrite of the request
// makes a `generic` connection grow a browser — an agent reading
// InvalidArgument would retry forever. The detail carries
// `connection/capability-unsupported`, which is what a client branches on. And
// the prose says what the connection *can* do and that pasting a URL works,
// because the connection is not broken and the user still has somewhere to go.
func TestBrowsingRefusesAConnectionWhoseProviderCannotBrowse(t *testing.T) {
	store := newFakeConnections().
		with(tokenConnection("internal-git", "internal-git-token")).
		withSecret("internal-git", controlstore.AuthMaterial{Token: "ghp-not-a-real-token"})
	// A fakeForge and not a browsingForge: the capability split is a difference
	// of type, exactly as it is in internal/forge.
	c := serve(t, Options{Connections: store, Forges: forgeLookup(fakeForge{name: "generic"})})

	calls := map[string]func() error{
		"ListConnectionRepositories": func() error {
			_, err := c.connections.ListConnectionRepositories(t.Context(),
				connect.NewRequest(&kelsonv1alpha1.ListConnectionRepositoriesRequest{Connection: "internal-git"}))
			return err
		},
		"ListConnectionBranches": func() error {
			_, err := c.connections.ListConnectionBranches(t.Context(),
				connect.NewRequest(&kelsonv1alpha1.ListConnectionBranchesRequest{
					Connection: "internal-git", Repository: "acme/checkout",
				}))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if code := connect.CodeOf(err); code != connect.CodeUnimplemented {
				t.Fatalf("%s = %v (code %s), want unimplemented", name, err, code)
			}
			assertWireCode(t, err, ErrConnectionCapabilityUnsupported)
			for _, want := range []string{"clone private repositories", "paste the repository's URL", "internal-git"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}

	// And the property the ordering of Server.browse exists for: a request that
	// was always going to be refused never opened the Secret.
	if store.reads() != 0 {
		t.Errorf("a capability refusal read the Secret %d times, want 0", store.reads())
	}
}

func TestBrowsingRefusesAConnectionWhoseProviderHasNoAdapter(t *testing.T) {
	store := newFakeConnections().with(tokenConnection("acme-gitlab", "acme-gitlab-token"))
	c := serve(t, Options{Connections: store, Forges: forgeLookup(nil)})

	_, err := c.connections.ListConnectionRepositories(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionRepositoriesRequest{Connection: "acme-gitlab"}))
	if code := connect.CodeOf(err); code != connect.CodeUnimplemented {
		t.Fatalf("err = %v (code %s), want unimplemented", err, code)
	}
	assertWireCode(t, err, ErrConnectionProviderUnknown)
	if store.reads() != 0 {
		t.Errorf("a connection with no adapter had its Secret read %d times", store.reads())
	}
}

func TestBrowsingAnUnknownConnection(t *testing.T) {
	c := serve(t, Options{Connections: newFakeConnections(), Forges: forgeLookup(browsingForge{
		fakeForge: fakeForge{name: "github"},
	})})

	if _, err := c.connections.ListConnectionRepositories(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionRepositoriesRequest{Connection: "ghost"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("ListConnectionRepositories on a missing connection = %v (code %s), want not-found", err, connect.CodeOf(err))
	}
	if _, err := c.connections.ListConnectionBranches(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionBranchesRequest{
			Connection: "ghost", Repository: "acme/checkout",
		})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("ListConnectionBranches on a missing connection = %v (code %s), want not-found", err, connect.CodeOf(err))
	}
}

// A forge that accepted the credential and then would not answer is this
// server's dependency failing, not the caller's request being wrong — and the
// refusal has to name the way around it, because a picker that cannot list
// leaves the pasted URL working.
func TestBrowsingAForgeThatWillNotAnswer(t *testing.T) {
	store := newFakeConnections().
		with(tokenConnection("acme-github", "acme-git-token")).
		withSecret("acme-github", controlstore.AuthMaterial{Token: "t"})
	c := serve(t, Options{Connections: store, Forges: forgeLookup(browsingForge{
		fakeForge: fakeForge{name: "github"},
		listErr:   forge.ErrAuthFailed,
	})})

	_, err := c.connections.ListConnectionRepositories(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionRepositoriesRequest{Connection: "acme-github"}))
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("err = %v (code %s), want unavailable", err, code)
	}
	if !strings.Contains(err.Error(), "paste the repository's URL") {
		t.Errorf("the failure does not name the path that still works: %v", err)
	}
}

func TestBrowsingWithoutAStoreIsUnimplemented(t *testing.T) {
	c := serve(t, Options{})
	if _, err := c.connections.ListConnectionRepositories(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionRepositoriesRequest{Connection: "x"})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("ListConnectionRepositories with no store = %s, want unimplemented", connect.CodeOf(err))
	}
	if _, err := c.connections.ListConnectionBranches(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.ListConnectionBranchesRequest{
			Connection: "x", Repository: "acme/checkout",
		})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("ListConnectionBranches with no store = %s, want unimplemented", connect.CodeOf(err))
	}
}
