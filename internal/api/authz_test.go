package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/serverstate"
)

// The enforcement tests of issue #74. Every one of them goes through the real
// HTTP gate, the real interceptor and the generated clients, because the thing
// under test is a boundary and a boundary tested from the inside is not one.
//
// The acceptance criterion the issue states is
// TestScopedToDevelopmentCannotMutateProduction; the rest are the properties
// that make it mean something.

const (
	devEnv  = "development"
	prodEnv = "production"
)

// --- the fake identity store -------------------------------------------------

// fakeAgentStore is an in-memory AgentStore. The real one is tested against a
// fake clientset in internal/serverstate; what matters here is the gate and the
// interceptor, so this one mints a token whose only job is to be looked up.
type fakeAgentStore struct {
	mu     sync.Mutex
	agents map[string]serverstate.Agent
	tokens map[string]string
	seq    int
}

func newFakeAgentStore() *fakeAgentStore {
	return &fakeAgentStore{agents: map[string]serverstate.Agent{}, tokens: map[string]string{}}
}

func (f *fakeAgentStore) Create(_ context.Context, spec serverstate.AgentSpec) (serverstate.Agent, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.agents[spec.Name]; exists {
		return serverstate.Agent{}, "", serverstate.Error{Code: serverstate.ErrAgentExists, Resource: "agent/" + spec.Name}
	}
	ttl := spec.TTL
	if ttl == 0 {
		ttl = serverstate.DefaultAgentTTL
	}
	limit := spec.Limit
	if limit.RequestsPerMinute == 0 {
		limit.RequestsPerMinute = serverstate.DefaultRequestsPerMinute
	}
	if limit.Burst == 0 {
		limit.Burst = serverstate.DefaultBurst
	}
	agent := serverstate.Agent{
		Name:    spec.Name,
		Created: testNow,
		Expires: testNow.Add(ttl),
		Scope:   spec.Scope,
		Limit:   limit,
	}
	f.seq++
	token := serverstate.AgentTokenPrefix + spec.Name + ".fake-secret"
	f.agents[spec.Name] = agent
	f.tokens[token] = spec.Name
	return agent, token, nil
}

func (f *fakeAgentStore) List(context.Context) ([]serverstate.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serverstate.Agent, 0, len(f.agents))
	for _, agent := range f.agents {
		out = append(out, agent)
	}
	return out, nil
}

func (f *fakeAgentStore) Revoke(_ context.Context, name string) (serverstate.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	agent, ok := f.agents[name]
	if !ok {
		return serverstate.Agent{}, serverstate.NotFound("agent/"+name, "no such identity", "list them")
	}
	agent.Revoked = true
	agent.RevokedAt = testNow
	f.agents[name] = agent
	return agent, nil
}

func (f *fakeAgentStore) Authenticate(_ context.Context, token string) (serverstate.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.tokens[token]
	if !ok {
		return serverstate.Agent{}, serverstate.Error{Code: serverstate.ErrAgentCredential, Resource: "agent/credential"}
	}
	return f.agents[name], nil
}

// expire rewrites one identity's expiry, which is how the expiry test reaches
// the past without a fake clock in three places.
func (f *fakeAgentStore) expire(name string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	agent := f.agents[name]
	agent.Expires = at
	f.agents[name] = agent
}

var testNow = time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)

// --- the harness -------------------------------------------------------------

// gatedServer is the whole stack: the api handlers, the authorization
// interceptor Register mounts, and the HTTP gate in front of them.
type gatedServer struct {
	url    string
	client *http.Client
	agents *fakeAgentStore
}

func newGatedServer(t *testing.T, opts Options) *gatedServer {
	t.Helper()
	agents := newFakeAgentStore()
	opts.Agents = agents
	opts.Now = func() time.Time { return testNow }
	server := New(opts)

	mux := http.NewServeMux()
	server.Register(mux)
	auth, err := NewAuthWith(AuthOptions{
		Password: testPassword,
		Agents:   agents,
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("NewAuthWith: %v", err)
	}
	srv := httptest.NewServer(auth.Middleware(mux))
	t.Cleanup(srv.Close)
	return &gatedServer{url: srv.URL, client: srv.Client(), agents: agents}
}

// as returns clients that send the given bearer credential on every call.
func (g *gatedServer) as(credential string) clients {
	opts := []connect.ClientOption{connect.WithInterceptors(bearerCredential(credential))}
	return clients{
		spec:     kelsonv1alpha1connect.NewSpecServiceClient(g.client, g.url, opts...),
		render:   kelsonv1alpha1connect.NewRenderServiceClient(g.client, g.url, opts...),
		profile:  kelsonv1alpha1connect.NewProfileServiceClient(g.client, g.url, opts...),
		deploy:   kelsonv1alpha1connect.NewDeployServiceClient(g.client, g.url, opts...),
		logs:     kelsonv1alpha1connect.NewLogServiceClient(g.client, g.url, opts...),
		events:   kelsonv1alpha1connect.NewEventServiceClient(g.client, g.url, opts...),
		builds:   kelsonv1alpha1connect.NewBuildServiceClient(g.client, g.url, opts...),
		secrets:  kelsonv1alpha1connect.NewSecretServiceClient(g.client, g.url, opts...),
		previews: kelsonv1alpha1connect.NewPreviewServiceClient(g.client, g.url, opts...),
	}
}

func (g *gatedServer) agentsClient(credential string) kelsonv1alpha1connect.AgentServiceClient {
	return kelsonv1alpha1connect.NewAgentServiceClient(g.client, g.url,
		connect.WithInterceptors(bearerCredential(credential)))
}

// bearerCredential is the client-side header setter, the same shape
// internal/mcp uses.
type bearerCredential string

var _ connect.Interceptor = bearerCredential("")

func (b bearerCredential) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearerCredential) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}

func (b bearerCredential) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// mint issues an identity through the fake store and returns its token.
func (g *gatedServer) mint(t *testing.T, name string, scope serverstate.Scope) string {
	t.Helper()
	_, token, err := g.agents.Create(t.Context(), serverstate.AgentSpec{Name: name, Scope: scope})
	if err != nil {
		t.Fatalf("minting %s: %v", name, err)
	}
	return token
}

// deployTo drives a Deploy stream to its first answer. Deploy is
// server-streaming, so the authorization refusal arrives when the handler
// receives the request — which is exactly the path the acceptance criterion
// runs through.
func deployTo(t *testing.T, c clients, project, environment string) error {
	t.Helper()
	stream, err := c.deploy.Deploy(t.Context(), connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec:        specRefFor(project),
		Environment: environment,
	}))
	if err != nil {
		return err
	}
	defer stream.Close() //nolint:errcheck // the test only wants the first error
	for stream.Receive() {
	}
	return stream.Err()
}

func specRefFor(project string) *kelsonv1alpha1.SpecRef {
	return &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: project}}
}

// authCode is the ConnectRPC code of an error, or CodeUnknown-as-success.
func authCode(err error) connect.Code {
	if err == nil {
		return 0
	}
	return connect.CodeOf(err)
}

// detailCodes returns the structured error codes riding on a refusal, which is
// what an agent branches on.
func detailCodes(err error) []string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return nil
	}
	var out []string
	for _, detail := range cerr.Details() {
		value, derr := detail.Value()
		if derr != nil {
			continue
		}
		if wire, ok := value.(*kelsonv1alpha1.Error); ok {
			out = append(out, wire.GetCode())
		}
	}
	return out
}

func hasCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

// --- the acceptance criterion ------------------------------------------------

// TestScopedToDevelopmentCannotMutateProduction is issue #74's acceptance
// criterion, enforced server-side: the same credential, the same method, two
// environments, two answers.
func TestScopedToDevelopmentCannotMutateProduction(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	token := g.mint(t, "deploybot", serverstate.Scope{
		Environments: []string{devEnv},
		Operations:   []serverstate.Operation{serverstate.OpMutate},
	})
	c := g.as(token)

	err := deployTo(t, c, "shop", prodEnv)
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("deploying to production = %v (code %s), want permission-denied", err, authCode(err))
	}
	if !hasCode(detailCodes(err), ErrOutOfScope) {
		t.Errorf("the refusal does not carry %s as a structured code: %v", ErrOutOfScope, detailCodes(err))
	}

	// The same credential against the environment it owns must get past
	// authorization. It fails afterwards for want of a delivery seam, which is
	// a different code and proves the refusal above was the scope and not the
	// server refusing everything.
	err = deployTo(t, c, "shop", devEnv)
	if authCode(err) == connect.CodePermissionDenied {
		t.Fatalf("deploying to the environment the identity owns was refused: %v", err)
	}
}

// TestScopeCoversEveryDimension: project, environment and operation class are
// three separate refusals, and each one alone is enough.
func TestScopeCoversEveryDimension(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	scoped := g.as(g.mint(t, "scoped", serverstate.Scope{
		Projects:     []string{"shop"},
		Environments: []string{devEnv},
		Operations:   []serverstate.Operation{serverstate.OpMutate},
	}))
	readOnly := g.as(g.mint(t, "reporter", serverstate.Scope{
		Operations: []serverstate.Operation{serverstate.OpRead},
	}))

	if code := authCode(deployTo(t, scoped, "billing", devEnv)); code != connect.CodePermissionDenied {
		t.Errorf("a project outside the scope = %s, want permission-denied", code)
	}
	if code := authCode(deployTo(t, readOnly, "shop", devEnv)); code != connect.CodePermissionDenied {
		t.Errorf("a mutation by a read-only identity = %s, want permission-denied", code)
	}

	// A read-only identity may still read.
	_, err := readOnly.spec.GetSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "shop"}))
	if authCode(err) == connect.CodePermissionDenied {
		t.Errorf("a read-only identity was refused a read: %v", err)
	}
}

// TestARestrictedCredentialCannotReachWhatItCannotBeCheckedAgainst: the honest
// half of the design. An inline spec and a cross-project list are refused
// rather than half-served.
func TestARestrictedCredentialCannotReachWhatItCannotBeCheckedAgainst(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	scoped := g.as(g.mint(t, "scoped", serverstate.Scope{
		Projects:   []string{"shop"},
		Operations: []serverstate.Operation{serverstate.OpMutate},
	}))
	unscoped := g.as(g.mint(t, "wide", serverstate.Scope{
		Operations: []serverstate.Operation{serverstate.OpMutate},
	}))

	// ListSpecs spans every project.
	_, err := scoped.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{}))
	if authCode(err) != connect.CodePermissionDenied {
		t.Errorf("ListSpecs by a project-scoped identity = %s, want permission-denied", authCode(err))
	}
	if _, err := unscoped.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{})); err != nil {
		t.Errorf("ListSpecs by an unrestricted identity was refused: %v", err)
	}

	// An inline spec carries the project inside the document.
	inline := &kelsonv1alpha1.RenderRequest{
		Spec: &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Documents{
			Documents: &kelsonv1alpha1.SpecDocuments{Project: []byte("apiVersion: kelson.dev/v1alpha1\n")},
		}},
		Environment: devEnv,
	}
	_, err = scoped.render.Render(t.Context(), connect.NewRequest(inline))
	if authCode(err) != connect.CodePermissionDenied {
		t.Errorf("an inline spec from a scoped identity = %s, want permission-denied", authCode(err))
	}
	if !hasCode(detailCodes(err), ErrTargetUnknown) {
		t.Errorf("the refusal does not carry %s: %v", ErrTargetUnknown, detailCodes(err))
	}
}

// TestLogsFollowTheNamespaceConvention pins the documented approximation: a
// namespace that is not `<project>-<environment>` for an allowed pair is
// refused, which is the direction that fails closed.
func TestLogsFollowTheNamespaceConvention(t *testing.T) {
	g := newGatedServer(t, Options{})
	c := g.as(g.mint(t, "reporter", serverstate.Scope{
		Projects:     []string{"shop"},
		Environments: []string{devEnv},
		Operations:   []serverstate.Operation{serverstate.OpRead},
	}))

	query := func(namespace string) connect.Code {
		_, err := c.logs.QueryLogs(t.Context(), connect.NewRequest(&kelsonv1alpha1.QueryLogsRequest{
			Selector: &kelsonv1alpha1.LogSelector{Namespace: namespace, Application: "web"},
			Tail:     10,
		}))
		return authCode(err)
	}
	if code := query("shop-production"); code != connect.CodePermissionDenied {
		t.Errorf("logs from another environment's namespace = %s, want permission-denied", code)
	}
	if code := query("kube-system"); code != connect.CodePermissionDenied {
		t.Errorf("logs from an unrelated namespace = %s, want permission-denied", code)
	}
	if code := query("shop-development"); code == connect.CodePermissionDenied {
		t.Error("logs from the identity's own namespace were refused")
	}
}

// --- credential lifetime -----------------------------------------------------

// TestAnExpiredCredentialIsRefused: expiry is checked on every request, so an
// identity that was live a moment ago stops working with no restart and no
// cache eviction.
func TestAnExpiredCredentialIsRefused(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	token := g.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})
	c := g.as(token)

	if code := authCode(deployTo(t, c, "shop", devEnv)); code == connect.CodeUnauthenticated {
		t.Fatalf("a live credential was refused as unauthenticated")
	}

	g.agents.expire("deploybot", testNow.Add(-time.Second))
	if code := authCode(deployTo(t, c, "shop", devEnv)); code != connect.CodeUnauthenticated {
		t.Errorf("an expired credential = %s, want unauthenticated", code)
	}
}

// TestRevocationIsImmediateAndLocksNoHumanOut is the second half of the
// acceptance criterion. The same token stops working on the very next request,
// and the password path is untouched.
func TestRevocationIsImmediateAndLocksNoHumanOut(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	token := g.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})
	agent := g.as(token)
	human := g.as(testPassword)

	if code := authCode(deployTo(t, agent, "shop", devEnv)); code == connect.CodeUnauthenticated {
		t.Fatal("a live credential was refused before revocation")
	}

	if _, err := g.agents.Revoke(t.Context(), "deploybot"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// No sleep, no restart, no cache to wait out: the next request is refused.
	if code := authCode(deployTo(t, agent, "shop", devEnv)); code != connect.CodeUnauthenticated {
		t.Errorf("a revoked credential = %s, want unauthenticated", code)
	}
	if code := authCode(deployTo(t, human, "shop", prodEnv)); code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied {
		t.Errorf("revoking an agent affected the human password path: %s", code)
	}
}

// TestTheHumanPasswordPathIsUnaffected: a password caller is not scoped, not
// rate-limited and not attributed to an identity — #84's posture, unchanged.
func TestTheHumanPasswordPathIsUnaffected(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	human := g.as(testPassword)

	if code := authCode(deployTo(t, human, "shop", prodEnv)); code == connect.CodePermissionDenied {
		t.Error("a password caller was refused a production deploy")
	}
	if _, err := human.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{})); err != nil {
		t.Errorf("a password caller was refused ListSpecs: %v", err)
	}
	// Far more requests than any agent budget allows.
	for i := range serverstate.DefaultBurst * 3 {
		if _, err := human.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{})); err != nil {
			t.Fatalf("a password caller was rate limited after %d requests: %v", i, err)
		}
	}
}

// --- the administrative boundary ---------------------------------------------

// TestAnAgentCannotMintOrRevokeAnAgent: the one refusal that keeps every other
// scope from being advisory.
func TestAnAgentCannotMintOrRevokeAnAgent(t *testing.T) {
	g := newGatedServer(t, Options{})
	token := g.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})
	agent := g.agentsClient(token)

	_, err := agent.CreateAgent(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateAgentRequest{
		Name:  "deploybot-2",
		Scope: &kelsonv1alpha1.AgentScope{Operations: []kelsonv1alpha1.AgentOperation{kelsonv1alpha1.AgentOperation_AGENT_OPERATION_MUTATE}},
	}))
	if authCode(err) != connect.CodePermissionDenied {
		t.Errorf("an agent minting an agent = %s, want permission-denied", authCode(err))
	}
	if !hasCode(detailCodes(err), ErrHumanOnly) {
		t.Errorf("the refusal does not carry %s: %v", ErrHumanOnly, detailCodes(err))
	}

	if _, err := agent.ListAgents(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListAgentsRequest{})); authCode(err) != connect.CodePermissionDenied {
		t.Errorf("an agent listing identities = %s, want permission-denied", authCode(err))
	}
	if _, err := agent.RevokeAgent(t.Context(), connect.NewRequest(&kelsonv1alpha1.RevokeAgentRequest{Name: "deploybot"})); authCode(err) != connect.CodePermissionDenied {
		t.Errorf("an agent revoking an identity = %s, want permission-denied", authCode(err))
	}

	// The human may do all three.
	human := g.agentsClient(testPassword)
	created, err := human.CreateAgent(t.Context(), connect.NewRequest(&kelsonv1alpha1.CreateAgentRequest{
		Name:  "minted-by-a-human",
		Scope: &kelsonv1alpha1.AgentScope{Operations: []kelsonv1alpha1.AgentOperation{kelsonv1alpha1.AgentOperation_AGENT_OPERATION_READ}},
	}))
	if err != nil {
		t.Fatalf("a human minting an identity: %v", err)
	}
	if created.Msg.GetToken() == "" {
		t.Error("CreateAgent returned no token, which is the only time it exists")
	}
	if _, err := human.ListAgents(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListAgentsRequest{})); err != nil {
		t.Errorf("a human listing identities: %v", err)
	}
}

// --- the rate limit ----------------------------------------------------------

// TestAnAgentOverItsBudgetGetsResourceExhausted: the code matters as much as
// the refusal — an agent that read this as permission-denied would stop for
// good instead of backing off.
func TestAnAgentOverItsBudgetGetsResourceExhausted(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	_, token, err := g.agents.Create(t.Context(), serverstate.AgentSpec{
		Name:  "chatty",
		Scope: serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpRead}},
		Limit: serverstate.Limit{RequestsPerMinute: 60, Burst: 3},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := g.as(token)

	// The clock is frozen, so the bucket never refills: three requests are the
	// whole budget and the fourth is over it.
	for i := range 3 {
		if _, err := c.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{})); err != nil {
			t.Fatalf("request %d inside the budget was refused: %v", i+1, err)
		}
	}
	_, err = c.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{}))
	if authCode(err) != connect.CodeResourceExhausted {
		t.Fatalf("the request past the budget = %s, want resource-exhausted", authCode(err))
	}
	if !hasCode(detailCodes(err), ErrRateLimited) {
		t.Errorf("the refusal does not carry %s: %v", ErrRateLimited, detailCodes(err))
	}
}

// TestABudgetIsPerIdentity: one runaway agent must not starve another.
func TestABudgetIsPerIdentity(t *testing.T) {
	g := newGatedServer(t, Options{Specs: newFakeSpecStore()})
	small := serverstate.Limit{RequestsPerMinute: 60, Burst: 1}
	_, chatty, err := g.agents.Create(t.Context(), serverstate.AgentSpec{
		Name: "chatty", Scope: serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpRead}}, Limit: small,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, quiet, err := g.agents.Create(t.Context(), serverstate.AgentSpec{
		Name: "quiet", Scope: serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpRead}}, Limit: small,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	list := func(c clients) error {
		_, err := c.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{}))
		return err
	}
	chattyClient := g.as(chatty)
	if err := list(chattyClient); err != nil {
		t.Fatalf("the first request was refused: %v", err)
	}
	if code := authCode(list(chattyClient)); code != connect.CodeResourceExhausted {
		t.Fatalf("the second request = %s, want resource-exhausted", code)
	}
	if err := list(g.as(quiet)); err != nil {
		t.Errorf("one identity exhausting its budget refused another: %v", err)
	}
}

// --- failing closed ----------------------------------------------------------

// TestAnUnmappedMethodIsRefusedToEveryone: the fail-closed half of the table's
// contract, asserted directly because a method with no row cannot be reached
// through a generated client — the coverage test makes sure there is none.
func TestAnUnmappedMethodIsRefusedToEveryone(t *testing.T) {
	a := newAuthorizer(func() time.Time { return testNow }, nil, nil)
	const unmapped = "/kelson.v1alpha1.FutureService/DoSomething"

	principals := map[string]Principal{
		"an agent":            {Type: PrincipalAgent, Name: "deploybot", Agent: serverstate.Agent{Name: "deploybot", Expires: testNow.Add(time.Hour), Scope: serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}}}},
		"a human":             {Type: PrincipalHuman, Name: "ada"},
		"an anonymous caller": {Type: PrincipalAnonymous},
	}
	for who, p := range principals {
		_, err := a.admit(t.Context(), p, unmapped)
		if authCode(err) != connect.CodePermissionDenied {
			t.Errorf("%s calling an unmapped method = %v, want permission-denied", who, err)
		}
		if !hasCode(detailCodes(err), ErrUnmappedMethod) {
			t.Errorf("%s got a refusal without %s: %v", who, ErrUnmappedMethod, detailCodes(err))
		}
	}

	// And a mapped method is admitted for the same principals, so the test
	// above is about the missing row rather than about a gate that refuses
	// everything.
	for who, p := range principals {
		if _, err := a.admit(t.Context(), p, kelsonv1alpha1connect.SpecServiceGetSpecProcedure); err != nil {
			t.Errorf("%s was refused a mapped method: %v", who, err)
		}
	}
}

// --- attribution -------------------------------------------------------------

// TestEveryRequestIsAttributedToItsPrincipal is the seam #78 builds on: an
// action nobody can attribute is the first thing #74 says is impossible today,
// so the name and the type reach the log for an allowed call and a refused one
// alike — and the two are told apart by the outcome, not by the absence of a
// record.
func TestEveryRequestIsAttributedToItsPrincipal(t *testing.T) {
	var records recordingHandler
	a := newAuthorizer(func() time.Time { return testNow }, slog.New(&records), nil)

	agent := Principal{Type: PrincipalAgent, Name: "deploybot", Agent: serverstate.Agent{
		Name:    "deploybot",
		Expires: testNow.Add(time.Hour),
		Scope:   serverstate.Scope{Environments: []string{devEnv}, Operations: []serverstate.Operation{serverstate.OpMutate}},
		Limit:   serverstate.Limit{RequestsPerMinute: 60, Burst: 10},
	}}
	row, err := a.admit(t.Context(), agent, kelsonv1alpha1connect.DeployServiceDeployProcedure)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := a.checkTarget(t.Context(), agent, kelsonv1alpha1connect.DeployServiceDeployProcedure, row,
		&kelsonv1alpha1.DeployRequest{Spec: specRefFor("shop"), Environment: prodEnv}); err == nil {
		t.Fatal("a production deploy by a development-scoped identity was allowed")
	}

	human := Principal{Type: PrincipalHuman, Name: "ada"}
	if _, err := a.admit(t.Context(), human, kelsonv1alpha1connect.SpecServiceGetSpecProcedure); err != nil {
		t.Fatalf("admit a human: %v", err)
	}
	if err := a.checkTarget(t.Context(), human, kelsonv1alpha1connect.SpecServiceGetSpecProcedure, row, nil); err != nil {
		t.Fatalf("checkTarget for a human: %v", err)
	}
	a.record(human, kelsonv1alpha1connect.SpecServiceGetSpecProcedure, "allowed", nil)

	refused := records.find("agent:deploybot")
	if refused == nil {
		t.Fatal("the refused agent request left no audit record")
	}
	if refused["principal_type"] != string(PrincipalAgent) || refused["outcome"] != "out-of-scope" {
		t.Errorf("the refusal was recorded as %v", refused)
	}
	if refused["procedure"] != kelsonv1alpha1connect.DeployServiceDeployProcedure {
		t.Errorf("the record names procedure %q", refused["procedure"])
	}

	allowed := records.find("human:ada")
	if allowed == nil {
		t.Fatal("the allowed human request left no audit record")
	}
	if allowed["principal_type"] != string(PrincipalHuman) || allowed["outcome"] != "allowed" {
		t.Errorf("the allowed request was recorded as %v", allowed)
	}
}

// recordingHandler collects slog records as flat string maps.
type recordingHandler struct {
	mu      sync.Mutex
	records []map[string]string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, fields)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) find(principal string) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, record := range h.records {
		if record["principal"] == principal {
			return record
		}
	}
	return nil
}

// TestAGateWithNoAgentStoreSaysSo: a `kagt.` token against a server that has no
// identity store must not be reported as a wrong password, which would send an
// operator looking in the wrong place entirely.
func TestAGateWithNoAgentStoreSaysSo(t *testing.T) {
	auth, err := NewAuth(testPassword)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, apiPathPrefix+"SpecService/ListSpecs", nil)
	req.Header.Set("Authorization", "Bearer "+serverstate.AgentTokenPrefix+"deploybot.whatever")

	_, message := auth.principal(req)
	if message == "" {
		t.Fatal("an agent token was accepted by a server with no agent store")
	}
	if !strings.Contains(message, "no agent identity store") {
		t.Errorf("the refusal does not say the server has no identity store: %q", message)
	}
}
