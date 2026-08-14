package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// The tests here drive the real MCP server over a real transport against a real
// ConnectRPC server, because everything this package does is assembly and the
// seams are where assembly breaks: the tool's input schema, the request it
// builds, the stream it consumes and the text it produces.
//
// The kelson-server behind it is a fake — a set of connect handlers over
// httptest, the shape internal/api's own tests use — for two reasons. It stages
// answers no in-memory kelson-server could produce (a crash-looping workload, a
// resync, a settled-unhealthy deployment), and it records every procedure
// called, which is how [harness.assertComposed] proves each tool used only the
// RPCs it declares. A tool that grew a capability of its own would fail that
// assertion, which is the enforcement ADR-0008 says the composition rule needs.

// fakeServer stages one kelson-server. An unset handler answers Unimplemented,
// so a test wires only what its tool should touch.
type fakeServer struct {
	mu    sync.Mutex
	calls []string
	// creds is the Authorization header of every request, in call order, so a
	// test can assert the shared password rode along (#84's interim cut).
	creds []string
	// reasons is the Kelson-Reason header of every request, in call order, so a
	// test can assert an agent's stated reason reached the server rather than
	// being accepted by the tool and dropped (#78).
	reasons []string

	listSpecs func(*kelsonv1alpha1.ListSpecsRequest) (*kelsonv1alpha1.ListSpecsResponse, error)
	getSpec   func(*kelsonv1alpha1.GetSpecRequest) (*kelsonv1alpha1.GetSpecResponse, error)
	putSpec   func(*kelsonv1alpha1.PutSpecRequest) (*kelsonv1alpha1.PutSpecResponse, error)
	status    func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error)
	history   func(*kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error)
	queryLogs func(*kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error)
	deploy    func(*kelsonv1alpha1.DeployRequest, *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error
	rollback  func(*kelsonv1alpha1.RollbackRequest, *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error
	promote   func(*kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error)
	watch     func(*kelsonv1alpha1.WatchRequest, *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error

	setSecret   func(*kelsonv1alpha1.SetSecretRequest) (*kelsonv1alpha1.SetSecretResponse, error)
	listSecrets func(*kelsonv1alpha1.ListSecretsRequest) (*kelsonv1alpha1.ListSecretsResponse, error)

	getProfile func(*kelsonv1alpha1.GetProfileRequest) (*kelsonv1alpha1.GetProfileResponse, error)

	explain func(*kelsonv1alpha1.ExplainRequest) (*kelsonv1alpha1.ExplainResponse, error)
}

var (
	_ kelsonv1alpha1connect.SpecServiceHandler    = (*fakeServer)(nil)
	_ kelsonv1alpha1connect.DeployServiceHandler  = (*fakeServer)(nil)
	_ kelsonv1alpha1connect.LogServiceHandler     = (*fakeServer)(nil)
	_ kelsonv1alpha1connect.EventServiceHandler   = (*fakeServer)(nil)
	_ kelsonv1alpha1connect.SecretServiceHandler  = (*fakeServer)(nil)
	_ kelsonv1alpha1connect.ProfileServiceHandler = (*fakeServer)(nil)
	_ kelsonv1alpha1connect.ExplainServiceHandler = (*fakeServer)(nil)
)

func (f *fakeServer) Explain(_ context.Context, req *connect.Request[kelsonv1alpha1.ExplainRequest]) (*connect.Response[kelsonv1alpha1.ExplainResponse], error) {
	if f.explain == nil {
		return nil, notWired("Explain")
	}
	msg, err := f.explain(req.Msg)
	return respond(msg, err)
}

func notWired(what string) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("this test did not wire %s", what))
}

func (f *fakeServer) ListSpecs(_ context.Context, req *connect.Request[kelsonv1alpha1.ListSpecsRequest]) (*connect.Response[kelsonv1alpha1.ListSpecsResponse], error) {
	if f.listSpecs == nil {
		return nil, notWired("ListSpecs")
	}
	msg, err := f.listSpecs(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) GetSpec(_ context.Context, req *connect.Request[kelsonv1alpha1.GetSpecRequest]) (*connect.Response[kelsonv1alpha1.GetSpecResponse], error) {
	if f.getSpec == nil {
		return nil, notWired("GetSpec")
	}
	msg, err := f.getSpec(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) PutSpec(_ context.Context, req *connect.Request[kelsonv1alpha1.PutSpecRequest]) (*connect.Response[kelsonv1alpha1.PutSpecResponse], error) {
	if f.putSpec == nil {
		return nil, notWired("PutSpec")
	}
	msg, err := f.putSpec(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) DeleteSpec(context.Context, *connect.Request[kelsonv1alpha1.DeleteSpecRequest]) (*connect.Response[kelsonv1alpha1.DeleteSpecResponse], error) {
	return nil, notWired("DeleteSpec")
}

func (f *fakeServer) Status(_ context.Context, req *connect.Request[kelsonv1alpha1.StatusRequest]) (*connect.Response[kelsonv1alpha1.StatusResponse], error) {
	if f.status == nil {
		return nil, notWired("Status")
	}
	msg, err := f.status(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) History(_ context.Context, req *connect.Request[kelsonv1alpha1.HistoryRequest]) (*connect.Response[kelsonv1alpha1.HistoryResponse], error) {
	if f.history == nil {
		return nil, notWired("History")
	}
	msg, err := f.history(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) Promote(_ context.Context, req *connect.Request[kelsonv1alpha1.PromoteRequest]) (*connect.Response[kelsonv1alpha1.PromoteResponse], error) {
	if f.promote == nil {
		return nil, notWired("Promote")
	}
	msg, err := f.promote(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) QueryLogs(_ context.Context, req *connect.Request[kelsonv1alpha1.QueryLogsRequest]) (*connect.Response[kelsonv1alpha1.QueryLogsResponse], error) {
	if f.queryLogs == nil {
		return nil, notWired("QueryLogs")
	}
	msg, err := f.queryLogs(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) FollowLogs(context.Context, *connect.Request[kelsonv1alpha1.FollowLogsRequest], *connect.ServerStream[kelsonv1alpha1.FollowLogsResponse]) error {
	return notWired("FollowLogs")
}

func (f *fakeServer) Deploy(_ context.Context, req *connect.Request[kelsonv1alpha1.DeployRequest], stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
	if f.deploy == nil {
		return notWired("Deploy")
	}
	return f.deploy(req.Msg, stream)
}

func (f *fakeServer) Rollback(_ context.Context, req *connect.Request[kelsonv1alpha1.RollbackRequest], stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
	if f.rollback == nil {
		return notWired("Rollback")
	}
	return f.rollback(req.Msg, stream)
}

func (f *fakeServer) Watch(_ context.Context, req *connect.Request[kelsonv1alpha1.WatchRequest], stream *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
	if f.watch == nil {
		return notWired("Watch")
	}
	return f.watch(req.Msg, stream)
}

func (f *fakeServer) SetSecret(_ context.Context, req *connect.Request[kelsonv1alpha1.SetSecretRequest]) (*connect.Response[kelsonv1alpha1.SetSecretResponse], error) {
	if f.setSecret == nil {
		return nil, notWired("SetSecret")
	}
	msg, err := f.setSecret(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) ListSecrets(_ context.Context, req *connect.Request[kelsonv1alpha1.ListSecretsRequest]) (*connect.Response[kelsonv1alpha1.ListSecretsResponse], error) {
	if f.listSecrets == nil {
		return nil, notWired("ListSecrets")
	}
	msg, err := f.listSecrets(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) GetProfile(_ context.Context, req *connect.Request[kelsonv1alpha1.GetProfileRequest]) (*connect.Response[kelsonv1alpha1.GetProfileResponse], error) {
	if f.getProfile == nil {
		return nil, notWired("GetProfile")
	}
	msg, err := f.getProfile(req.Msg)
	return respond(msg, err)
}

func (f *fakeServer) DeleteSecret(context.Context, *connect.Request[kelsonv1alpha1.DeleteSecretRequest]) (*connect.Response[kelsonv1alpha1.DeleteSecretResponse], error) {
	// Deliberately never wired: no tool composes DeleteSecret. Deleting a
	// credential an agent cannot see and cannot restore is not a task that
	// belongs on this surface (ADR-0008), and the assertion that no tool
	// reaches it is [harness.assertComposed] on every call.
	return nil, notWired("DeleteSecret")
}

func respond[T any](msg *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(msg), nil
}

// harness is a running MCP server over a running fake kelson-server, plus the
// client session a test calls tools through.
type harness struct {
	fake    *fakeServer
	session *mcpsdk.ClientSession
	address string
	// client is the fake server's own HTTP client, for the rare test that has
	// to make a call the tool surface does not expose.
	client connect.HTTPClient
}

func start(t *testing.T, fake *fakeServer) *harness {
	t.Helper()
	return startWith(t, fake, "")
}

// startWith is start with a credential. A password makes the MCP server send
// it as a bearer token on every call; empty is the no-authentication server.
func startWith(t *testing.T, fake *fakeServer, password string) *harness {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(kelsonv1alpha1connect.NewSpecServiceHandler(fake))
	mux.Handle(kelsonv1alpha1connect.NewDeployServiceHandler(fake))
	mux.Handle(kelsonv1alpha1connect.NewLogServiceHandler(fake))
	mux.Handle(kelsonv1alpha1connect.NewEventServiceHandler(fake))
	mux.Handle(kelsonv1alpha1connect.NewSecretServiceHandler(fake))
	mux.Handle(kelsonv1alpha1connect.NewProfileServiceHandler(fake))
	mux.Handle(kelsonv1alpha1connect.NewExplainServiceHandler(fake))
	srv := httptest.NewServer(record(fake, mux))
	t.Cleanup(srv.Close)

	server := New(Options{Server: srv.URL, HTTPClient: srv.Client(), Version: "test", Password: password})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Run(ctx, serverTransport)
	}()
	t.Cleanup(func() { cancel(); <-done })

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting to the MCP server: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return &harness{fake: fake, session: session, address: srv.URL, client: srv.Client()}
}

// record notes every procedure the tools call, which is what proves a tool
// composed the API rather than reaching past it.
func record(fake *fakeServer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.calls = append(fake.calls, strings.TrimPrefix(strings.ReplaceAll(r.URL.Path, "/", "."), "."))
		fake.creds = append(fake.creds, r.Header.Get("Authorization"))
		fake.reasons = append(fake.reasons, r.Header.Get(ReasonHeader))
		fake.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (f *fakeServer) procedures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeServer) credentials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.creds...)
}

func (f *fakeServer) statedReasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reasons...)
}

// call invokes one tool and returns its text, failing the test if the tool
// reported an error.
func (h *harness) call(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	out, isError := h.callRaw(t, name, args)
	if isError {
		t.Fatalf("%s reported an error:\n%s", name, out)
	}
	h.assertComposed(t, name)
	return out
}

// callErr invokes one tool expecting a tool error, and returns its text.
func (h *harness) callErr(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	out, isError := h.callRaw(t, name, args)
	if !isError {
		t.Fatalf("%s was expected to fail but returned:\n%s", name, out)
	}
	h.assertComposed(t, name)
	return out
}

func (h *harness) callRaw(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	var b strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(*mcpsdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String(), res.IsError
}

// assertComposed is the capability-parity check at runtime: every procedure the
// tool actually called must be one it declares in the surface table.
func (h *harness) assertComposed(t *testing.T, name string) {
	t.Helper()
	var declared []rpc
	found := false
	for _, tool := range surface(&clients{}) {
		if tool.def.Name == name {
			declared, found = tool.rpcs, true
		}
	}
	if !found {
		t.Fatalf("no tool named %q is in the surface", name)
	}
	allowed := map[string]bool{}
	for _, r := range declared {
		allowed[r.String()] = true
	}
	for _, called := range h.fake.procedures() {
		if !allowed[called] {
			t.Errorf("%s called %s, which it does not declare in its RPC list (declared: %v)", name, called, declared)
		}
	}
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not mention %q:\n%s", want, out)
		}
	}
}

func mustNotContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if strings.Contains(out, want) {
			t.Errorf("answer mentions %q and should not:\n%s", want, out)
		}
	}
}

// --- fixtures ---------------------------------------------------------------

const projectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello
spec:
  image: ghcr.io/acme/hello:1.4.2
  components:
    - name: web
      port: 8080
      health: /healthz
    - name: worker
`

func healthyStatus() *kelsonv1alpha1.StatusResponse {
	return &kelsonv1alpha1.StatusResponse{
		Phase:     "Healthy",
		Revision:  "42",
		Namespace: "hello-production",
		Verdicts: []*kelsonv1alpha1.WorkloadVerdict{
			{Resource: "Deployment/hello-production/web", Code: "healthy", Healthy: true},
		},
	}
}

func crashLoopStatus() *kelsonv1alpha1.StatusResponse {
	return &kelsonv1alpha1.StatusResponse{
		Phase:     "Degraded",
		Revision:  "43",
		Namespace: "hello-production",
		Cause:     "ReplicaSet hello-web-7f6 has not progressed",
		Detail:    map[string]string{"resources": "4", "degraded": "1"},
		Verdicts: []*kelsonv1alpha1.WorkloadVerdict{
			{Resource: "Deployment/hello-production/worker", Code: "healthy", Healthy: true},
			{
				Resource:    "Deployment/hello-production/web",
				Code:        "crash-loop-back-off",
				Degraded:    true,
				Message:     "web: back-off restarting failed container",
				Remediation: "check the container command and the logs before termination",
			},
		},
	}
}

func logLines(n int) *kelsonv1alpha1.QueryLogsResponse {
	res := &kelsonv1alpha1.QueryLogsResponse{}
	for i := range n {
		res.Lines = append(res.Lines, &kelsonv1alpha1.LogLine{
			TimestampUnixMs: 1_755_000_000_000 + int64(i)*1000,
			Pod:             "hello-web-7f6-abcde",
			Container:       "web",
			Message:         fmt.Sprintf("line %d", i),
		})
	}
	return res
}
