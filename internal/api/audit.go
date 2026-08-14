package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/serverstate"
)

// The audit trail's capture point (issue #78, ADR-0026).
//
// # One record per request, written once, at the end
//
// #74 left a seam: the authorization interceptor already sees every request,
// knows the principal, the procedure and the decision, and writes one slog line
// per call. This file turns that line into a durable record — and moves the
// write to *after* the handler, so the record can also carry what only the
// handler knows: the revision an apply produced, the diff a promotion computed,
// whether the call failed after being allowed.
//
// The mechanism is an [auditEntry] parked in the request's context. The
// interceptor creates it, authorization stamps a refusal onto it, the handler
// enriches it through the small setters below, and exactly one write happens
// when the call returns. One request is one record: a handler that forgets to
// enrich produces a thinner record, never a second one and never none.
//
// # A failed audit write never fails the request
//
// It is counted and logged at ERROR with the record's identity, and the request
// proceeds. The alternative — failing a deploy because a ConfigMap update
// conflicted — would make the audit trail a new way to take a deployment down.
// The counter is the visibility half: "the trail is silently incomplete" is the
// failure mode this must not have, so a lost record is loud even though it is
// not fatal (ADR-0026 §3).
//
// # What is not captured here
//
// A request that never authenticates is refused by the HTTP gate (auth.go)
// before any interceptor runs, so it leaves no audit record. That is an honest
// gap and ADR-0026 §7 records it: the gate has no principal to attribute the
// attempt to, and a record naming nobody is a log line, which is what it
// already writes.

// ReasonHeader carries the caller's own statement of why it is doing this. It
// is a header rather than a field on every mutating request message because it
// belongs to the *call*, not to any one operation: adding it to the schema
// would mean adding it to eight messages and to every message added later, and
// the one that got forgotten would be the one that mattered.
//
// internal/mcp sets it from the `reason` parameter of the mutating tools, which
// is how an agent states its intent (ADR-0026 §4). Nothing is invented when the
// caller sends none: the record's reason is simply absent.
const ReasonHeader = "Kelson-Reason"

// auditWriteTimeout bounds one record's write. It is short: the write is one
// ConfigMap read-modify-write, and a record that has not landed in this long is
// better counted as lost than left holding a goroutine.
const auditWriteTimeout = 5 * time.Second

// AuditSink is the audit-trail seam. *serverstate.AuditStore implements it; a
// nil one is a server with no trail, which QueryAudit answers CodeUnimplemented
// for and which makes every capture point below a no-op.
//
// Both halves are on one interface deliberately. A deployment that recorded
// through one implementation and answered queries from another would produce a
// trail that disagreed with itself, which is worse than no trail at all.
type AuditSink interface {
	Append(ctx context.Context, rec serverstate.AuditRecord) error
	Query(ctx context.Context, q serverstate.AuditQuery) (serverstate.AuditPage, error)
}

// auditor owns the sink, the failure counter and the logger. It is held by the
// authorization interceptor because that is the one place every request passes
// through.
type auditor struct {
	sink   AuditSink
	logger *slog.Logger
	// failures counts records this process could not write. It is exported
	// through [auditor.Failures] for the test that proves a failing sink does
	// not fail a request, and it rides on every failure log line so the number
	// is visible without a metrics endpoint kelson does not have yet.
	failures atomic.Int64
}

func newAuditor(sink AuditSink, logger *slog.Logger) *auditor {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &auditor{sink: sink, logger: logger}
}

// enabled reports whether there is anywhere to write.
func (a *auditor) enabled() bool { return a != nil && a.sink != nil }

// Failures is how many records this process failed to write.
func (a *auditor) Failures() int64 { return a.failures.Load() }

// auditEntry is the record under construction for one request. Every field is
// guarded because a streaming handler's enrichment and the interceptor's
// finalisation are not guaranteed to be the same goroutine.
type auditEntry struct {
	mu sync.Mutex
	// rec is the record so far.
	rec serverstate.AuditRecord
	// refused is set when authorization turned the request away, so finish
	// does not overwrite an authorization code with a transport one.
	refused bool
	// row is the scope table's rule for this procedure, kept so the target can
	// be read off the request message once it is decoded (which for a
	// server-streaming RPC is after the interceptor has already run).
	row methodScope
	// written guards against a double write on a path that both returns an
	// error and completes.
	written bool
}

type auditKey struct{}

// withAuditEntry parks the entry in the request context.
func withAuditEntry(ctx context.Context, e *auditEntry) context.Context {
	return context.WithValue(ctx, auditKey{}, e)
}

// auditFrom returns the entry the context carries, or nil. Every enrichment
// helper below tolerates nil, so a handler driven directly by a test — with no
// interceptor in front of it — is not a special case anywhere.
func auditFrom(ctx context.Context) *auditEntry {
	e, _ := ctx.Value(auditKey{}).(*auditEntry)
	return e
}

// begin opens a record for one request. It returns a context carrying the entry
// and the entry itself; a nil entry (no sink) makes every later call a no-op.
func (a *auditor) begin(ctx context.Context, p Principal, procedure string, header http.Header) (context.Context, *auditEntry) {
	if !a.enabled() {
		return ctx, nil
	}
	row, _ := scopeFor(procedure)
	e := &auditEntry{
		row: row,
		rec: serverstate.AuditRecord{
			Principal: serverstate.AuditPrincipal{Type: string(p.Type), Name: p.Name},
			Procedure: procedure,
			Operation: row.Operation,
			Reason:    reasonFrom(header),
		},
	}
	if p.Type == PrincipalAgent {
		e.rec.Scope = describeScope(p.Agent.Scope)
	}
	return withAuditEntry(ctx, e), e
}

// reasonFrom reads the caller's stated reason off the request headers. It is
// bounded here as well as in the store: a header a thousand times the limit
// should not be carried around the process just to be cut at the end.
func reasonFrom(header http.Header) string {
	if header == nil {
		return ""
	}
	reason := strings.TrimSpace(header.Get(ReasonHeader))
	if len(reason) > serverstate.MaxAuditReason*2 {
		reason = reason[:serverstate.MaxAuditReason*2]
	}
	return reason
}

// target reads the (project, environment) off the decoded request message,
// using the same extractor the scope check uses. A handler that knows better —
// Promote acts on two environments and PutSpec's project is inside the YAML —
// overrides it with [auditTarget].
func (e *auditEntry) target(msg any) {
	if e == nil || e.row.Targets == nil {
		return
	}
	targets, ok := e.row.Targets(msg)
	if !ok || len(targets) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rec.Target.Project == "" {
		e.rec.Target = serverstate.AuditTarget{Project: targets[0].Project, Environment: targets[0].Environment}
	}
}

// refuse stamps an authorization refusal. The code is the structured one the
// caller receives, so a record and the error the caller saw name the same thing.
func (e *auditEntry) refuse(err authzError) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refused = true
	e.rec.Outcome = serverstate.AuditRefused
	e.rec.Code = err.Code
	e.rec.Message = err.Message
}

// finish resolves the outcome and writes the record, once.
//
// The write happens on a context detached from the request's, because the two
// cases where a record matters most are exactly the cases where the request's
// context is already dead: a cancelled deploy stream and a client that hung up
// mid-call.
func (a *auditor) finish(ctx context.Context, e *auditEntry, err error) {
	if !a.enabled() || e == nil {
		return
	}
	e.mu.Lock()
	if e.written {
		e.mu.Unlock()
		return
	}
	e.written = true
	if !e.refused {
		if err == nil {
			e.rec.Outcome = serverstate.AuditAllowed
		} else {
			e.rec.Outcome = serverstate.AuditFailed
			e.rec.Code, e.rec.Message = failureCode(err)
		}
	}
	rec := e.rec
	record := e.recordable()
	e.mu.Unlock()

	if !record {
		return
	}
	// Scrub here, not at begin: the values SetSecret carries are registered
	// with internal/redact by the handler, which has only just run. A reason
	// scrubbed before the handler executed would be scrubbed against a registry
	// that did not yet know the credential (issue #117).
	//
	// The store scrubs again on the way in. That is deliberate duplication, the
	// same kind internal/api/secret.go and internal/secret keep between them: a
	// record must not be able to carry a value through a sink that forgot.
	rec.Reason = redact.Scrub(rec.Reason)
	rec.Message = redact.Scrub(rec.Message)
	rec.DryRunSummary = redact.Scrub(rec.DryRunSummary)
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := a.sink.Append(write, rec); err != nil {
		// Counted and loud, and the request it was recording is already
		// answered. A trail with a hole in it must never be a trail that looks
		// whole (ADR-0026 §3).
		a.logger.Error("audit record lost",
			slog.String("principal", rec.Principal.String()),
			slog.String("procedure", rec.Procedure),
			slog.String("outcome", string(rec.Outcome)),
			slog.Int64("lost_total", a.failures.Add(1)),
			slog.String("error", err.Error()),
		)
	}
}

// recordable is the "what is worth a durable record" rule, in one place.
//
// Everything that changed something, and everything that was refused whatever
// its class. An allowed read is not recorded: it changed nothing, and the
// volume of status polls would evict the mutations the trail exists for from a
// bounded store. ADR-0026 §3 states the trade and the way out.
//
// A procedure with no row in the scope table is recorded: it was refused, and a
// refusal nobody can see is the failure mode the table exists to prevent.
func (e *auditEntry) recordable() bool {
	if e.rec.Outcome != serverstate.AuditAllowed {
		return true
	}
	return e.rec.Operation == serverstate.OpMutate || e.rec.Operation == serverstate.OpAdmin
}

// failureCode projects a handler failure onto the record's code and message. A
// structured plane error keeps its own code — the record then speaks the same
// vocabulary an agent branched on — and anything else falls back to the
// transport code, prefixed so the two cannot be confused.
func failureCode(err error) (code, message string) {
	if wire := wireErrors(err); len(wire) > 0 {
		return wire[0].GetCode(), wire[0].GetMessage()
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return "rpc/" + connect.CodeOf(err).String(), cerr.Message()
	}
	return "rpc/" + connect.CodeOf(err).String(), redact.Scrub(err.Error())
}

// --- enrichment, called by handlers ------------------------------------------

// auditTarget records which project and environment a call acted on, for the
// handlers whose request shape the scope table cannot read one out of: PutSpec
// carries the project inside the YAML, and Promote names two environments of
// which the destination is the one that changed.
func auditTarget(ctx context.Context, project, environment string) {
	e := auditFrom(ctx)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec.Target = serverstate.AuditTarget{Project: project, Environment: environment}
}

// auditDryRun records which rung of the dry-run ladder the call asked for, so a
// preview and an apply are never confused in the trail.
func auditDryRun(ctx context.Context, dry kelsonv1alpha1.DryRun) {
	e := auditFrom(ctx)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec.DryRun = dryRunName(dry)
}

// auditDryRunResult records what a dry run found, in the handler's own words.
func auditDryRunResult(ctx context.Context, summary string) {
	e := auditFrom(ctx)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec.DryRunSummary = summary
}

// auditIdempotencyKey records the key a mutating call carried (issue #71), so a
// retry and the operation it repeats are visibly the same one.
func auditIdempotencyKey(ctx context.Context, key string) {
	e := auditFrom(ctx)
	if e == nil || key == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec.IdempotencyKey = key
}

// auditChange records the resulting change: the revision it produced and a
// bounded summary of what it touched. It merges rather than replaces, because
// the revision and the shape of the change are learned at different moments —
// the set is rendered before the apply, the revision comes back from it.
func auditChange(ctx context.Context, change serverstate.AuditChange) {
	e := auditFrom(ctx)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rec.Change == nil {
		e.rec.Change = &serverstate.AuditChange{}
	}
	merged := e.rec.Change
	if change.Revision != "" {
		merged.Revision = change.Revision
	}
	if change.From != "" {
		merged.From = change.From
	}
	if change.Source != "" {
		merged.Source = change.Source
	}
	if change.Added != 0 || change.Modified != 0 || change.Removed != 0 {
		merged.Added, merged.Modified, merged.Removed = change.Added, change.Modified, change.Removed
	}
	if change.Resources != 0 {
		merged.Resources = change.Resources
	}
	if len(change.Kinds) > 0 {
		merged.Kinds = change.Kinds
	}
	if change.MaxRisk != "" {
		merged.MaxRisk = change.MaxRisk
	}
}

// auditRevision is the one-field case of [auditChange]: the apply landed and
// this is what it was recorded as.
func auditRevision(ctx context.Context, revision string) {
	auditChange(ctx, serverstate.AuditChange{Revision: revision})
}

// changeFromDiff summarises a computed comparison for a record.
func changeFromDiff(d *diff.Diff) serverstate.AuditChange {
	if d == nil {
		return serverstate.AuditChange{}
	}
	kinds := make([]string, 0, len(d.Resources))
	for _, r := range d.Resources {
		kinds = append(kinds, r.Kind)
	}
	return serverstate.AuditChange{
		Source:   serverstate.ChangeFromDiff,
		Added:    d.Summary.Added,
		Modified: d.Summary.Modified,
		Removed:  d.Summary.Removed,
		Kinds:    kinds,
		MaxRisk:  string(d.Summary.MaxRisk),
	}
}

// changeFromSet summarises the set an apply carried, for the case where no
// comparison was computed. It reports what was applied and says so through
// Source, rather than reporting zeros for added/modified/removed as if nothing
// had changed — a record that claimed a deploy touched nothing would be the
// worst kind of wrong.
func changeFromSet(set delivery.ManifestSet) serverstate.AuditChange {
	kinds := make([]string, 0, len(set.Manifests))
	for _, m := range set.Manifests {
		kinds = append(kinds, m.Kind)
	}
	return serverstate.AuditChange{
		Source:    serverstate.ChangeFromRendered,
		Resources: len(set.Manifests),
		Kinds:     kinds,
	}
}

// --- the query handler --------------------------------------------------------

// QueryAudit answers the audit trail. It is administrative: the scope table
// files it beside AgentService, so an agent credential is refused whatever its
// scope (ADR-0026 §5).
//
// The bounds are the store's, reported rather than hidden: a page size above
// the maximum is refused, and every response carries the window the store could
// actually have covered.
func (s *Server) QueryAudit(ctx context.Context, req *connect.Request[kelsonv1alpha1.QueryAuditRequest]) (*connect.Response[kelsonv1alpha1.QueryAuditResponse], error) {
	if s.audit == nil || !s.audit.enabled() {
		return nil, unimplemented("the audit trail")
	}
	msg := req.Msg
	page, err := s.audit.sink.Query(ctx, serverstate.AuditQuery{
		Principal:     msg.GetPrincipal(),
		PrincipalType: msg.GetPrincipalType(),
		Project:       msg.GetProject(),
		Environment:   msg.GetEnvironment(),
		Outcome:       auditOutcomeOf(msg.GetOutcome()),
		Procedure:     msg.GetProcedure(),
		Since:         auditSince(msg.GetSinceUnixMs()),
		Until:         auditSince(msg.GetUntilUnixMs()),
		Limit:         int(msg.GetPageSize()),
		PageToken:     msg.GetPageToken(),
	})
	if err != nil {
		if serverstate.AsAuditQuery(err) {
			return nil, fail(connect.CodeInvalidArgument, err)
		}
		return nil, failRequest(err)
	}

	out := &kelsonv1alpha1.QueryAuditResponse{
		NextPageToken: page.NextPageToken,
		Window: &kelsonv1alpha1.AuditWindow{
			Complete:             page.Window.Complete,
			Dropped:              int32(page.Window.Dropped), //nolint:gosec // a day's ring is bounded far below int32
			RetainedFromUnixMs:   auditMillis(page.Window.RetainedFrom),
			RetainDays:           int32(page.Window.RetainDays), //nolint:gosec // bounded by MaxAuditRetentionDays
			OldestRecordedUnixMs: auditMillis(page.Window.OldestRecorded),
			Reason:               page.Window.Reason,
		},
	}
	for i := range page.Records {
		out.Records = append(out.Records, wireAuditRecord(page.Records[i]))
	}
	return connect.NewResponse(out), nil
}

// wireAuditRecord projects one stored record onto the wire, verbatim: the
// vocabulary of codes, outcomes and sources is the store's and this layer does
// not translate it (ADR-0013 §2).
func wireAuditRecord(rec serverstate.AuditRecord) *kelsonv1alpha1.AuditRecord {
	out := &kelsonv1alpha1.AuditRecord{
		Id:         rec.ID,
		TimeUnixMs: auditMillis(rec.Time),
		Principal: &kelsonv1alpha1.AuditPrincipal{
			Type: rec.Principal.Type,
			Name: rec.Principal.Name,
		},
		Scope:          rec.Scope,
		Procedure:      rec.Procedure,
		Operation:      string(rec.Operation),
		Outcome:        wireAuditOutcome(rec.Outcome),
		Code:           rec.Code,
		Message:        rec.Message,
		DryRun:         rec.DryRun,
		DryRunSummary:  rec.DryRunSummary,
		Reason:         rec.Reason,
		IdempotencyKey: rec.IdempotencyKey,
	}
	if rec.Target != (serverstate.AuditTarget{}) {
		out.Target = &kelsonv1alpha1.AuditTarget{Project: rec.Target.Project, Environment: rec.Target.Environment}
	}
	if rec.Change != nil {
		out.Change = &kelsonv1alpha1.AuditChange{
			Revision:     rec.Change.Revision,
			FromRevision: rec.Change.From,
			Source:       rec.Change.Source,
			Added:     int32(rec.Change.Added),     //nolint:gosec // a rendered set is orders of magnitude below int32
			Modified:  int32(rec.Change.Modified),  //nolint:gosec // as above
			Removed:   int32(rec.Change.Removed),   //nolint:gosec // as above
			Resources: int32(rec.Change.Resources), //nolint:gosec // as above
			Kinds:     rec.Change.Kinds,
			MaxRisk:   rec.Change.MaxRisk,
		}
	}
	return out
}

func wireAuditOutcome(outcome serverstate.AuditOutcome) kelsonv1alpha1.AuditOutcome {
	switch outcome {
	case serverstate.AuditAllowed:
		return kelsonv1alpha1.AuditOutcome_AUDIT_OUTCOME_ALLOWED
	case serverstate.AuditRefused:
		return kelsonv1alpha1.AuditOutcome_AUDIT_OUTCOME_REFUSED
	case serverstate.AuditFailed:
		return kelsonv1alpha1.AuditOutcome_AUDIT_OUTCOME_FAILED
	default:
		return kelsonv1alpha1.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED
	}
}

// auditOutcomeOf maps the wire filter onto the store's vocabulary. UNSPECIFIED
// is "any", which is why it is not an error here — the store refuses a value it
// does not know, and the enum cannot carry one.
func auditOutcomeOf(outcome kelsonv1alpha1.AuditOutcome) serverstate.AuditOutcome {
	switch outcome {
	case kelsonv1alpha1.AuditOutcome_AUDIT_OUTCOME_ALLOWED:
		return serverstate.AuditAllowed
	case kelsonv1alpha1.AuditOutcome_AUDIT_OUTCOME_REFUSED:
		return serverstate.AuditRefused
	case kelsonv1alpha1.AuditOutcome_AUDIT_OUTCOME_FAILED:
		return serverstate.AuditFailed
	default:
		return ""
	}
}

// dryRunName is the ladder's rung as the record spells it. The names match the
// MCP surface's vocabulary ("render", "server", "none") rather than the enum's,
// because those are the words a person reading the trail typed.
func dryRunName(dry kelsonv1alpha1.DryRun) string {
	switch dry {
	case kelsonv1alpha1.DryRun_DRY_RUN_RENDER:
		return "render"
	case kelsonv1alpha1.DryRun_DRY_RUN_SERVER:
		return "server"
	default:
		return "none"
	}
}

func auditSince(unixMs int64) time.Time {
	if unixMs == 0 {
		return time.Time{}
	}
	return time.UnixMilli(unixMs).UTC()
}

func auditMillis(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}
