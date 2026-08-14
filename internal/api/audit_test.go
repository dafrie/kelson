package api

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/serverstate"
)

// The capture-point tests of issue #78. Like the #74 tests they go through the
// real HTTP gate, the real interceptor and the generated clients, because what
// is being asserted is that a request leaves a record — and a record written
// from inside the handler under test would prove nothing about the path a real
// request takes.

// --- the fake sink -----------------------------------------------------------

// fakeAuditSink is an in-memory AuditSink. The ConfigMap-backed store has its
// own tests in internal/serverstate; what matters here is what the API layer
// hands it, and (through failNext) that it can fail without taking the request
// with it.
type fakeAuditSink struct {
	mu       sync.Mutex
	records  []serverstate.AuditRecord
	failWith error
	window   serverstate.AuditWindow
}

func newFakeAuditSink() *fakeAuditSink {
	return &fakeAuditSink{window: serverstate.AuditWindow{
		Complete:     true,
		RetainDays:   serverstate.DefaultAuditRetentionDays,
		RetainedFrom: testNow.AddDate(0, 0, -serverstate.DefaultAuditRetentionDays),
	}}
}

func (f *fakeAuditSink) Append(_ context.Context, rec serverstate.AuditRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("%013d-%04x", len(f.records), len(f.records))
	}
	if rec.Time.IsZero() {
		rec.Time = testNow
	}
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeAuditSink) Query(_ context.Context, q serverstate.AuditQuery) (serverstate.AuditPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.Limit > serverstate.MaxAuditPageSize {
		return serverstate.AuditPage{}, serverstate.Error{Code: serverstate.ErrAuditQuery, Resource: "audit"}
	}
	limit := q.Limit
	if limit == 0 {
		limit = serverstate.DefaultAuditPageSize
	}
	var matched []serverstate.AuditRecord
	for i := len(f.records) - 1; i >= 0; i-- {
		rec := f.records[i]
		if q.Principal != "" && rec.Principal.Name != q.Principal {
			continue
		}
		if q.Project != "" && rec.Target.Project != q.Project {
			continue
		}
		if q.Outcome != "" && rec.Outcome != q.Outcome {
			continue
		}
		if q.PageToken != "" && rec.ID >= q.PageToken {
			continue
		}
		matched = append(matched, rec)
	}
	page := serverstate.AuditPage{Window: f.window}
	if len(matched) > limit {
		page.NextPageToken = matched[limit-1].ID
		matched = matched[:limit]
	}
	page.Records = matched
	return page, nil
}

func (f *fakeAuditSink) all() []serverstate.AuditRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]serverstate.AuditRecord(nil), f.records...)
}

// only returns the single record, failing when there is not exactly one. Most
// assertions here are "one request, one record", and the count is half the
// property.
func (f *fakeAuditSink) only(t *testing.T) serverstate.AuditRecord {
	t.Helper()
	records := f.all()
	if len(records) != 1 {
		t.Fatalf("the trail holds %d records, want exactly one: %+v", len(records), records)
	}
	return records[0]
}

func (f *fakeAuditSink) find(t *testing.T, procedure string) serverstate.AuditRecord {
	t.Helper()
	for _, rec := range f.all() {
		if strings.Contains(rec.Procedure, procedure) {
			return rec
		}
	}
	t.Fatalf("no record for %s in the trail: %+v", procedure, f.all())
	return serverstate.AuditRecord{}
}

// --- the harness -------------------------------------------------------------

// auditedServer is the #74 gated stack plus a trail, a stored spec and a
// delivery plane, so a mutation can actually run to a revision.
type auditedServer struct {
	*gatedServer
	sink    *fakeAuditSink
	adapter *fakeAdapter
	specs   *fakeSpecStore
}

// gatedFor mounts an already-built Server behind the same HTTP gate
// newGatedServer uses, for a test that needs the *Server itself.
func gatedFor(t *testing.T, server *Server, agents *fakeAgentStore) *gatedServer {
	t.Helper()
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

// findMessage is recordingHandler.find keyed on the log message rather than the
// principal, which is what the lost-record line has to be found by: it is
// written by the auditor, not by the attribution seam.
func (h *recordingHandler) findMessage(msg string) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, record := range h.records {
		if record["msg"] == msg {
			return record
		}
	}
	return nil
}

func newAuditedServer(t *testing.T) *auditedServer {
	t.Helper()
	sink := newFakeAuditSink()
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "hello", serverstate.Documents{
		Project:      []byte(projectDoc),
		Environments: map[string][]byte{"development": []byte(developmentDoc)},
	}, serverstate.PutOptions{}); err != nil {
		t.Fatalf("seeding the spec store: %v", err)
	}
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseHealthy, Revision: "rev-00000001"}}
	connector, _ := connectorFor(adapter, nil, nil)

	g := newGatedServer(t, Options{
		Specs:    specs,
		Delivery: connector,
		Audit:    sink,
		Secrets:  newFakeSecrets(),
	})
	return &auditedServer{gatedServer: g, sink: sink, adapter: adapter, specs: specs}
}

// deployStored drives a Deploy of the seeded spec to the end of its stream,
// optionally stating a reason the way an agent would.
func deployStored(t *testing.T, c clients, environment, reason string) error {
	t.Helper()
	req := connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec:           specRefFor("hello"),
		Environment:    environment,
		Profile:        profileRef(),
		DryRun:         kelsonv1alpha1.DryRun_DRY_RUN_NONE,
		IdempotencyKey: "key-42",
	})
	if reason != "" {
		req.Header().Set(ReasonHeader, reason)
	}
	stream, err := c.deploy.Deploy(t.Context(), req)
	if err != nil {
		return err
	}
	defer stream.Close() //nolint:errcheck // the outcome is the stream's error
	for stream.Receive() {
	}
	return stream.Err()
}

func (a *auditedServer) auditClient(credential string) kelsonv1alpha1connect.AuditServiceClient {
	return kelsonv1alpha1connect.NewAuditServiceClient(a.client, a.url,
		connect.WithInterceptors(bearerCredential(credential)))
}

// --- the acceptance criterion ------------------------------------------------

// TestAnAgentMutationIsTraceableToIdentityScopeAndDiff is issue #78's
// acceptance criterion, end to end: an agent deploys, and the record names who,
// under what scope, against what, with what result and pointing at the revision
// the apply produced.
func TestAnAgentMutationIsTraceableToIdentityScopeAndDiff(t *testing.T) {
	a := newAuditedServer(t)
	token := a.mint(t, "deploybot", serverstate.Scope{
		Projects:     []string{"hello"},
		Environments: []string{devEnv},
		Operations:   []serverstate.Operation{serverstate.OpMutate},
	})

	if err := deployStored(t, a.as(token), devEnv, "shipping the checkout fix"); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	rec := a.sink.only(t)
	if rec.Principal.Type != string(PrincipalAgent) || rec.Principal.Name != "deploybot" {
		t.Errorf("principal = %+v, want the agent identity that called", rec.Principal)
	}
	if !strings.Contains(rec.Scope, "projects=hello") || !strings.Contains(rec.Scope, "environments="+devEnv) {
		t.Errorf("scope = %q, want the credential's scope at the time it acted", rec.Scope)
	}
	if rec.Procedure != kelsonv1alpha1connect.DeployServiceDeployProcedure {
		t.Errorf("procedure = %q", rec.Procedure)
	}
	if rec.Operation != serverstate.OpMutate {
		t.Errorf("operation = %q, want mutate", rec.Operation)
	}
	if rec.Target.Project != "hello" || rec.Target.Environment != devEnv {
		t.Errorf("target = %+v, want the project and environment it acted on", rec.Target)
	}
	if rec.Outcome != serverstate.AuditAllowed {
		t.Errorf("outcome = %q (%s), want allowed", rec.Outcome, rec.Message)
	}
	if rec.Change == nil || rec.Change.Revision != "rev-00000001" {
		t.Fatalf("change = %+v, want the revision the apply recorded", rec.Change)
	}
	if rec.Change.Resources == 0 || len(rec.Change.Kinds) == 0 {
		t.Errorf("change = %+v, want the shape of what was applied beside the revision", rec.Change)
	}
	if rec.DryRun != "none" {
		t.Errorf("dryRun = %q, want none: this one actually applied", rec.DryRun)
	}
	if rec.Reason != "shipping the checkout fix" {
		t.Errorf("reason = %q, want the caller's own words", rec.Reason)
	}
	if rec.IdempotencyKey != "key-42" {
		t.Errorf("idempotencyKey = %q, want the key the call carried", rec.IdempotencyKey)
	}
}

// TestARefusalIsRecordedWithTheCodeTheCallerGot: the refusal an agent sees and
// the record an operator reads must name the same thing, or the trail cannot be
// used to explain a refusal.
func TestARefusalIsRecordedWithTheCodeTheCallerGot(t *testing.T) {
	a := newAuditedServer(t)
	token := a.mint(t, "deploybot", serverstate.Scope{
		Environments: []string{devEnv},
		Operations:   []serverstate.Operation{serverstate.OpMutate},
	})

	err := deployStored(t, a.as(token), prodEnv, "")
	if authCode(err) != connect.CodePermissionDenied {
		t.Fatalf("deploying to production = %v, want permission-denied", err)
	}

	rec := a.sink.only(t)
	if rec.Outcome != serverstate.AuditRefused {
		t.Errorf("outcome = %q, want refused", rec.Outcome)
	}
	if rec.Code != ErrOutOfScope {
		t.Errorf("code = %q, want %q — the same code the caller received", rec.Code, ErrOutOfScope)
	}
	if !hasCode(detailCodes(err), rec.Code) {
		t.Errorf("the record's code %q is not among the caller's %v", rec.Code, detailCodes(err))
	}
	if rec.Change != nil {
		t.Errorf("a refused deploy recorded a change: %+v", rec.Change)
	}
	if rec.Principal.Name != "deploybot" {
		t.Errorf("a refusal must still name who was refused, got %+v", rec.Principal)
	}
	// And what it tried to reach. "deploybot was refused" is half an answer;
	// "deploybot was refused production" is the one an operator acts on.
	if rec.Target.Project != "hello" || rec.Target.Environment != prodEnv {
		t.Errorf("target = %+v, want the target the refused call named", rec.Target)
	}
}

// TestAReasonIsAbsentWhenNoneWasSupplied: kelson records the caller's words and
// never invents them, so an empty reason is the honest answer rather than a
// guess reconstructed from the request.
func TestAReasonIsAbsentWhenNoneWasSupplied(t *testing.T) {
	a := newAuditedServer(t)
	token := a.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})
	if err := deployStored(t, a.as(token), devEnv, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := a.sink.only(t).Reason; got != "" {
		t.Errorf("reason = %q, want empty", got)
	}
}

// TestAnAllowedReadLeavesNoRecordButARefusedOneDoes is the recording rule
// (ADR-0026 §3), asserted as the pair it is: reads are not recorded because
// they change nothing and their volume would evict the mutations, but a
// *refused* read is a security event and is always recorded.
func TestAnAllowedReadLeavesNoRecordButARefusedOneDoes(t *testing.T) {
	a := newAuditedServer(t)
	reader := a.as(a.mint(t, "reporter", serverstate.Scope{
		Projects:   []string{"hello"},
		Operations: []serverstate.Operation{serverstate.OpRead},
	}))

	if _, err := reader.spec.GetSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "hello"})); err != nil {
		t.Fatalf("an in-scope read was refused: %v", err)
	}
	if records := a.sink.all(); len(records) != 0 {
		t.Fatalf("an allowed read left %d records: %+v", len(records), records)
	}

	if _, err := reader.spec.GetSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "billing"})); err == nil {
		t.Fatal("a read outside the scope was allowed")
	}
	rec := a.sink.only(t)
	if rec.Outcome != serverstate.AuditRefused || rec.Code != ErrOutOfScope {
		t.Errorf("the refused read was recorded as %q/%q", rec.Outcome, rec.Code)
	}
}

// TestAFailedMutationIsRecordedAsFailed: "allowed and then broke" is the answer
// to "what did it actually do?" as often as either of the other two, so it is a
// third outcome rather than an allowed record that quietly implies success.
func TestAFailedMutationIsRecordedAsFailed(t *testing.T) {
	a := newAuditedServer(t)
	a.adapter.applyErr = delivery.Error{
		Code:     delivery.ErrApplyFailed,
		Resource: "Deployment/hello-development/web",
		Message:  "the apply was rejected",
	}
	token := a.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})

	if err := deployStored(t, a.as(token), devEnv, ""); err == nil {
		t.Fatal("the deploy reported success with a failing adapter")
	}
	rec := a.sink.only(t)
	if rec.Outcome != serverstate.AuditFailed {
		t.Fatalf("outcome = %q, want failed", rec.Outcome)
	}
	if rec.Code != string(delivery.ErrApplyFailed) {
		t.Errorf("code = %q, want the failing plane's own code %q", rec.Code, delivery.ErrApplyFailed)
	}
	if rec.Change != nil && rec.Change.Revision != "" {
		t.Errorf("a failed apply recorded a revision: %+v", rec.Change)
	}
}

// TestAHumanMutationIsRecordedToo: #12 wants a general log and #78 wants the
// agent trail. One implementation, principal-typed — which only holds if a
// human's mutation lands in the same place with its type on it.
func TestAHumanMutationIsRecordedToo(t *testing.T) {
	a := newAuditedServer(t)
	if err := deployStored(t, a.as(testPassword), devEnv, "hand-rolled by an operator"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	rec := a.sink.only(t)
	if rec.Principal.Type != string(PrincipalHuman) {
		t.Errorf("principal type = %q, want human", rec.Principal.Type)
	}
	if rec.Scope != "" {
		t.Errorf("scope = %q, want empty: a shared password has no scope to record", rec.Scope)
	}
	if rec.Outcome != serverstate.AuditAllowed || rec.Change == nil {
		t.Errorf("the human mutation was recorded as %q with change %+v", rec.Outcome, rec.Change)
	}
}

// TestADryRunIsRecordedAsAPreview: a preview and an apply must never be
// confused in the trail, which is the difference between "it looked at
// production" and "it changed production".
func TestADryRunIsRecordedAsAPreview(t *testing.T) {
	a := newAuditedServer(t)
	token := a.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})

	stream, err := a.as(token).deploy.Deploy(t.Context(), connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec:        specRefFor("hello"),
		Environment: devEnv,
		Profile:     profileRef(),
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	_ = stream.Close()

	rec := a.sink.only(t)
	if rec.DryRun != "render" {
		t.Errorf("dryRun = %q, want render", rec.DryRun)
	}
	if rec.Change != nil && rec.Change.Revision != "" {
		t.Errorf("a render dry run recorded a revision: %+v", rec.Change)
	}
	if calls := a.adapter.callLog(); len(calls) != 0 {
		t.Errorf("a render dry run reached the adapter: %v", calls)
	}
}

// --- the failure that must not become a failure ------------------------------

// TestAFailedAuditWriteDoesNotFailTheRequestAndIsVisible is ADR-0026 §3. A
// trail that could take a deployment down would be a new way to take a
// deployment down; a trail with a silent hole in it would be worse than none.
// Both halves are asserted at once, because either alone is the wrong design.
func TestAFailedAuditWriteDoesNotFailTheRequestAndIsVisible(t *testing.T) {
	var records recordingHandler
	sink := newFakeAuditSink()
	sink.failWith = errors.New("the audit ConfigMap is being written concurrently")

	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "hello", serverstate.Documents{
		Project:      []byte(projectDoc),
		Environments: map[string][]byte{"development": []byte(developmentDoc)},
	}, serverstate.PutOptions{}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseHealthy, Revision: "rev-00000001"}}
	connector, _ := connectorFor(adapter, nil, nil)

	server := New(Options{
		Specs:    specs,
		Delivery: connector,
		Audit:    sink,
		Logger:   slog.New(&records),
		Now:      func() time.Time { return testNow },
	})
	g := gatedFor(t, server, newFakeAgentStore())

	if err := deployStored(t, g.as(testPassword), devEnv, ""); err != nil {
		t.Fatalf("a deploy failed because its audit record could not be written: %v", err)
	}
	if calls := adapter.callLog(); len(calls) == 0 || calls[0] != "apply" {
		t.Errorf("the deploy did not reach the adapter: %v", calls)
	}

	lost := records.findMessage("audit record lost")
	if lost == nil {
		t.Fatal("a lost audit record left no log line; the trail would be silently incomplete")
	}
	if lost["lost_total"] != "1" {
		t.Errorf("the loss line reports lost_total=%q, want 1 — the count is the visibility", lost["lost_total"])
	}
	if lost["procedure"] != kelsonv1alpha1connect.DeployServiceDeployProcedure {
		t.Errorf("the loss line does not say which call was lost: %v", lost)
	}
	if server.audit.Failures() != 1 {
		t.Errorf("the lost-record counter reads %d, want 1", server.audit.Failures())
	}
}

// --- query and export ---------------------------------------------------------

// TestQueryAuditFiltersAndPages drives the RPC the CLI and the export both use.
func TestQueryAuditFiltersAndPages(t *testing.T) {
	a := newAuditedServer(t)
	for i := range 5 {
		rec := serverstate.AuditRecord{
			ID:        fmt.Sprintf("%013d-0000", i),
			Time:      testNow.Add(time.Duration(i) * time.Second),
			Principal: serverstate.AuditPrincipal{Type: "agent", Name: "deploybot"},
			Procedure: kelsonv1alpha1connect.DeployServiceDeployProcedure,
			Target:    serverstate.AuditTarget{Project: "hello", Environment: devEnv},
			Outcome:   serverstate.AuditAllowed,
			Change:    &serverstate.AuditChange{Revision: fmt.Sprintf("rev-%d", i)},
		}
		if err := a.sink.Append(t.Context(), rec); err != nil {
			t.Fatalf("seeding the trail: %v", err)
		}
	}

	client := a.auditClient(testPassword)
	res, err := client.QueryAudit(t.Context(), connect.NewRequest(&kelsonv1alpha1.QueryAuditRequest{
		Principal: "deploybot",
		Project:   "hello",
		PageSize:  2,
	}))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Msg.GetRecords()) != 2 {
		t.Fatalf("page holds %d records, want 2", len(res.Msg.GetRecords()))
	}
	if res.Msg.GetNextPageToken() == "" {
		t.Fatal("a full page carried no next page token, so an export could not continue")
	}
	if got := res.Msg.GetRecords()[0].GetChange().GetRevision(); got != "rev-4" {
		t.Errorf("first record = %q, want the newest (rev-4)", got)
	}
	if window := res.Msg.GetWindow(); window == nil || window.GetRetainDays() == 0 {
		t.Errorf("the response carries no window; a reader cannot tell what it could not have covered: %v", window)
	}

	// Following the token to exhaustion is what `--export jsonl` does.
	seen := len(res.Msg.GetRecords())
	token := res.Msg.GetNextPageToken()
	for token != "" {
		next, err := client.QueryAudit(t.Context(), connect.NewRequest(&kelsonv1alpha1.QueryAuditRequest{
			Principal: "deploybot",
			Project:   "hello",
			PageSize:  2,
			PageToken: token,
		}))
		if err != nil {
			t.Fatalf("paging: %v", err)
		}
		seen += len(next.Msg.GetRecords())
		token = next.Msg.GetNextPageToken()
		if seen > 5 {
			t.Fatal("paging did not terminate")
		}
	}
	if seen != 5 {
		t.Errorf("paging to exhaustion saw %d records, want 5", seen)
	}
}

// TestAnOversizedPageIsRefused: the store refuses rather than clamps, and the
// handler must pass that through as invalid-argument rather than as a server
// failure — a caller that asked for too much is wrong, not unlucky.
func TestAnOversizedPageIsRefused(t *testing.T) {
	a := newAuditedServer(t)
	_, err := a.auditClient(testPassword).QueryAudit(t.Context(), connect.NewRequest(&kelsonv1alpha1.QueryAuditRequest{
		PageSize: serverstate.MaxAuditPageSize + 1,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("an oversized page = %v (%s), want invalid-argument", err, connect.CodeOf(err))
	}
}

// TestAnAgentMayNotReadTheAuditTrail is ADR-0026 §5. An agent that could read
// the trail could read what its reviewer will see, and the trail spans every
// project, so there is no scoped version of the answer that would be safe.
func TestAnAgentMayNotReadTheAuditTrail(t *testing.T) {
	a := newAuditedServer(t)
	token := a.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})

	_, err := a.auditClient(token).QueryAudit(t.Context(), connect.NewRequest(&kelsonv1alpha1.QueryAuditRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("an agent reading the trail = %v (%s), want permission-denied", err, connect.CodeOf(err))
	}
	if !hasCode(detailCodes(err), ErrHumanOnly) {
		t.Errorf("the refusal does not carry %s: %v", ErrHumanOnly, detailCodes(err))
	}

	// And a human with the password may.
	if _, err := a.auditClient(testPassword).QueryAudit(t.Context(),
		connect.NewRequest(&kelsonv1alpha1.QueryAuditRequest{})); err != nil {
		t.Errorf("a human was refused the trail: %v", err)
	}
}

// TestQueryAuditIsUnimplementedWithoutASink: a server keeping no trail says so
// rather than answering an empty list, which would be indistinguishable from
// "nothing happened".
func TestQueryAuditIsUnimplementedWithoutASink(t *testing.T) {
	server := New(Options{})
	_, err := server.QueryAudit(t.Context(), connect.NewRequest(&kelsonv1alpha1.QueryAuditRequest{}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("a server with no trail answered %v (%s), want unimplemented", err, connect.CodeOf(err))
	}
}

// --- the #117 property --------------------------------------------------------

// TestNoSecretValueReachesTheTrail: the one RPC that carries secret values must
// not put one in a record, even when it fails and even in the reason the caller
// chose to send.
func TestNoSecretValueReachesTheTrail(t *testing.T) {
	const value = "pk_live_5UP3Rs3cr3t_stripe_key"

	a := newAuditedServer(t)
	token := a.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})

	req := connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: &kelsonv1alpha1.SecretTarget{Project: "hello", Environment: devEnv},
		Name:   "stripe",
		Values: map[string]string{"api-key": value},
	})
	// The caller puts the value in the one free-text field it controls, which
	// is the mistake this property exists to survive.
	req.Header().Set(ReasonHeader, "rotating the key to "+value)
	if _, err := a.as(token).secrets.SetSecret(t.Context(), req); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	rec := a.sink.find(t, "SetSecret")
	blob := fmt.Sprintf("%+v", rec)
	if strings.Contains(blob, value) {
		t.Fatalf("a secret value reached the audit record: %s", blob)
	}
	if !strings.Contains(rec.Reason, redact.Sentinel) {
		t.Errorf("the reason was not scrubbed: %q", rec.Reason)
	}
	// The record still says what happened — which keys, on what — so scrubbing
	// did not cost the trail its usefulness.
	if rec.Change == nil || rec.Change.Resources != 1 {
		t.Errorf("change = %+v, want the key count", rec.Change)
	}
	if rec.Target.Project != "hello" {
		t.Errorf("target = %+v", rec.Target)
	}
}

// TestTheReasonIsBoundedOnTheWayIn: a header is caller-controlled, so a
// megabyte of it must not be carried around the process or stored.
func TestTheReasonIsBoundedOnTheWayIn(t *testing.T) {
	a := newAuditedServer(t)
	token := a.mint(t, "deploybot", serverstate.Scope{Operations: []serverstate.Operation{serverstate.OpMutate}})
	if err := deployStored(t, a.as(token), devEnv, strings.Repeat("x", 64<<10)); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := len(a.sink.only(t).Reason); got > serverstate.MaxAuditReason*2 {
		t.Errorf("the handler passed %d bytes of reason to the store", got)
	}
}
