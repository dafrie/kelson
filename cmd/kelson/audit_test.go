package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dafrie/kelson/internal/serverstate"
)

// `kelson audit`'s tests (issue #78, ADR-0026).
//
// The command is thin — it builds a query, prints a page and pages an export —
// so what is worth asserting is the part that is not: that the filters reach the
// store as the flags promised, that the export round-trips, and that a truncated
// window is impossible to miss.

var auditNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// fakeAuditStore records the query it was asked and answers from a fixed set,
// paging the way the real store does.
type fakeAuditStore struct {
	queries []serverstate.AuditQuery
	records []serverstate.AuditRecord
	window  serverstate.AuditWindow
	err     error
	// pageSize caps a page below whatever the query asked for, so the export's
	// paging loop is actually exercised rather than answered in one round trip.
	pageSize int
}

func (f *fakeAuditStore) Query(_ context.Context, q serverstate.AuditQuery) (serverstate.AuditPage, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return serverstate.AuditPage{}, f.err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = serverstate.DefaultAuditPageSize
	}
	if f.pageSize > 0 && f.pageSize < limit {
		limit = f.pageSize
	}
	var matched []serverstate.AuditRecord
	for _, rec := range f.records {
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

func auditRecords(n int) []serverstate.AuditRecord {
	out := make([]serverstate.AuditRecord, 0, n)
	for i := range n {
		out = append(out, serverstate.AuditRecord{
			// Descending, as the store returns them.
			ID:        string(rune('z'-i)) + "-id",
			Time:      auditNow.Add(-time.Duration(i) * time.Minute),
			Principal: serverstate.AuditPrincipal{Type: "agent", Name: "deploybot"},
			Scope:     "projects=shop environments=production operations=mutate",
			Procedure: "/kelson.v1alpha1.DeployService/Deploy",
			Operation: serverstate.OpMutate,
			Target:    serverstate.AuditTarget{Project: "shop", Environment: "production"},
			Outcome:   serverstate.AuditAllowed,
			Change: &serverstate.AuditChange{
				Revision:  "rev-0000000" + string(rune('1'+i)),
				Source:    serverstate.ChangeFromRendered,
				Resources: 4,
				Kinds:     []string{"Deployment", "Service"},
			},
		})
	}
	return out
}

func completeWindow() serverstate.AuditWindow {
	return serverstate.AuditWindow{
		Complete:     true,
		RetainDays:   serverstate.DefaultAuditRetentionDays,
		RetainedFrom: auditNow.AddDate(0, 0, -serverstate.DefaultAuditRetentionDays),
	}
}

func runAuditCmd(t *testing.T, store *fakeAuditStore, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newAuditCmdWith(func(string, string) (auditStore, error) { return store, nil })
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// TestAuditPrintsIdentityTargetAndRevision: the listing has to answer #78's
// question at a glance — who, against what, and what came of it.
func TestAuditPrintsIdentityTargetAndRevision(t *testing.T) {
	store := &fakeAuditStore{records: auditRecords(1), window: completeWindow()}
	out, _, err := runAuditCmd(t, store)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, want := range []string{
		"agent:deploybot",
		"DeployService.Deploy",
		"target=shop/production",
		"revision=rev-00000001",
		"4 resources applied",
		"kinds=Deployment,Service",
		"scope: projects=shop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not mention %q:\n%s", want, out)
		}
	}
}

// TestAuditAlwaysStatesTheWindow is the no-silent-caps rule: the horizon is
// printed whether or not anything was lost, because a reader who never sees it
// cannot know it exists.
func TestAuditAlwaysStatesTheWindow(t *testing.T) {
	store := &fakeAuditStore{records: auditRecords(1), window: completeWindow()}
	out, _, err := runAuditCmd(t, store)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(out, "retains 30 days") {
		t.Errorf("a complete window did not state the retention:\n%s", out)
	}
	if !strings.Contains(out, "this window is complete") {
		t.Errorf("a complete window did not say so:\n%s", out)
	}
}

// TestAuditShoutsWhenTheWindowIsIncomplete: a truncated answer must be
// impossible to read as a whole one.
func TestAuditShoutsWhenTheWindowIsIncomplete(t *testing.T) {
	store := &fakeAuditStore{
		records: auditRecords(1),
		window: serverstate.AuditWindow{
			Complete:     false,
			Dropped:      17,
			RetainDays:   30,
			RetainedFrom: auditNow.AddDate(0, 0, -30),
			Reason:       "17 record(s) were dropped from the days this query covers",
		},
	}
	out, _, err := runAuditCmd(t, store)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(out, "THIS WINDOW IS INCOMPLETE") {
		t.Errorf("an incomplete window was not called out:\n%s", out)
	}
	if !strings.Contains(out, "17 record(s) were dropped") {
		t.Errorf("the reason the window is incomplete was not printed:\n%s", out)
	}
}

// TestAuditFlagsReachTheStore: every filter the help promises has to arrive.
func TestAuditFlagsReachTheStore(t *testing.T) {
	store := &fakeAuditStore{window: completeWindow()}
	if _, _, err := runAuditCmd(t, store,
		"--project", "shop",
		"--env", "production",
		"--agent", "deploybot",
		"--procedure", "Deploy",
		"--outcome", "refused",
		"--limit", "7",
	); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(store.queries) != 1 {
		t.Fatalf("the store was queried %d times", len(store.queries))
	}
	q := store.queries[0]
	switch {
	case q.Project != "shop":
		t.Errorf("project = %q", q.Project)
	case q.Environment != "production":
		t.Errorf("environment = %q", q.Environment)
	case q.Principal != "deploybot" || q.PrincipalType != "agent":
		t.Errorf("--agent produced principal %q of type %q", q.Principal, q.PrincipalType)
	case q.Procedure != "Deploy":
		t.Errorf("procedure = %q", q.Procedure)
	case q.Outcome != serverstate.AuditRefused:
		t.Errorf("outcome = %q", q.Outcome)
	case q.Limit != 7:
		t.Errorf("limit = %d", q.Limit)
	}
}

// TestAuditSinceIsATimeBound: --since is a duration for a person and an
// absolute bound for the store.
func TestAuditSinceIsATimeBound(t *testing.T) {
	opts := &auditOptions{since: 24 * time.Hour}
	q, err := opts.query(auditNow)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if want := auditNow.Add(-24 * time.Hour); !q.Since.Equal(want) {
		t.Errorf("since = %s, want %s", q.Since, want)
	}

	unbounded, err := (&auditOptions{}).query(auditNow)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !unbounded.Since.IsZero() {
		t.Errorf("omitting --since produced a bound of %s; it must leave the range open so the "+
			"window can report what the store could not have covered", unbounded.Since)
	}
}

// TestAuditRefusesAnUnknownOutcome: a typo must not silently match nothing and
// read as "no records".
func TestAuditRefusesAnUnknownOutcome(t *testing.T) {
	store := &fakeAuditStore{window: completeWindow()}
	_, _, err := runAuditCmd(t, store, "--outcome", "denied")
	if err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
	if !strings.Contains(err.Error(), "allowed, refused or failed") {
		t.Errorf("the refusal does not name the values that work: %v", err)
	}
	if len(store.queries) != 0 {
		t.Error("an invalid query still reached the store")
	}
}

// TestAuditRefusesAnUnknownExportFormat, for the same reason.
func TestAuditRefusesAnUnknownExportFormat(t *testing.T) {
	_, _, err := runAuditCmd(t, &fakeAuditStore{window: completeWindow()}, "--export", "csv")
	if err == nil || !strings.Contains(err.Error(), "jsonl") {
		t.Fatalf("an unknown export format = %v, want a refusal naming jsonl", err)
	}
}

// TestAuditExportRoundTrips is the export's whole contract: every matching
// record, one JSON object per line, decodable back into the record it came from.
func TestAuditExportRoundTrips(t *testing.T) {
	const total = 7
	store := &fakeAuditStore{records: auditRecords(total), window: completeWindow()}

	out, errOut, err := runAuditCmd(t, store, "--export", "jsonl")
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != total {
		t.Fatalf("the export wrote %d lines for %d records", len(lines), total)
	}
	var decoded []serverstate.AuditRecord
	for i, line := range lines {
		var rec serverstate.AuditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not a JSON object: %v\n%s", i, err, line)
		}
		decoded = append(decoded, rec)
	}
	first := decoded[0]
	if first.Principal.Name != "deploybot" || first.Target.Project != "shop" {
		t.Errorf("the exported record lost its identity or target: %+v", first)
	}
	if first.Change == nil || first.Change.Revision != "rev-00000001" {
		t.Errorf("the exported record lost the revision it points at: %+v", first.Change)
	}
	if first.Scope == "" {
		t.Errorf("the exported record lost the scope it acted under: %+v", first)
	}

	// stdout is the data; the window note goes to stderr so a pipeline is not
	// handed a line it cannot parse.
	if strings.Contains(out, "retains") {
		t.Errorf("the export wrote a human note onto stdout, corrupting the stream:\n%s", out)
	}
	if !strings.Contains(errOut, "exported 7 records") {
		t.Errorf("the export did not report what it wrote, on stderr: %q", errOut)
	}
}

// TestAuditExportPagesToExhaustion: the export must not stop at one page, and
// it must ask for the store's largest so it does so in as few round trips as
// the bound allows.
func TestAuditExportPagesToExhaustion(t *testing.T) {
	// The store hands back one record at a time whatever is asked for, so an
	// export that stopped at the first page would return one line instead of
	// five.
	store := &fakeAuditStore{records: auditRecords(5), window: completeWindow(), pageSize: 1}

	out, _, err := runAuditCmd(t, store, "--export", "jsonl", "--limit", "2")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != 5 {
		t.Fatalf("the export wrote %d lines, want all 5 records — --limit must not bound an export", lines)
	}
	if len(store.queries) != 5 {
		t.Errorf("the export made %d queries for 5 single-record pages", len(store.queries))
	}
	if store.queries[0].Limit != serverstate.MaxAuditPageSize {
		t.Errorf("the export asked for a page of %d, want the store's maximum %d so it pages in as few "+
			"round trips as the bound allows", store.queries[0].Limit, serverstate.MaxAuditPageSize)
	}
	if store.queries[1].PageToken == "" {
		t.Error("the export did not carry the page token forward")
	}
}

// TestAuditExportSaysWhenItIsIncomplete: an export is the artifact people keep,
// so a partial one must say so where a reader will see it.
func TestAuditExportSaysWhenItIsIncomplete(t *testing.T) {
	store := &fakeAuditStore{
		records: auditRecords(1),
		window: serverstate.AuditWindow{
			RetainDays:   30,
			RetainedFrom: auditNow.AddDate(0, 0, -30),
			Dropped:      4,
			Reason:       "4 record(s) were dropped",
		},
	}
	_, errOut, err := runAuditCmd(t, store, "--export", "jsonl")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(errOut, "THIS EXPORT IS INCOMPLETE") {
		t.Errorf("an incomplete export did not say so: %q", errOut)
	}
}

// TestAuditSaysSoWhenNothingMatches: an empty answer must read as "nothing
// matched", never as an error or as silence.
func TestAuditSaysSoWhenNothingMatches(t *testing.T) {
	out, _, err := runAuditCmd(t, &fakeAuditStore{window: completeWindow()}, "--agent", "nobody")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(out, "no audit records match") {
		t.Errorf("an empty result printed nothing intelligible:\n%s", out)
	}
	if !strings.Contains(out, "window:") {
		t.Errorf("an empty result skipped the window, so a reader cannot tell "+
			"'nothing happened' from 'it was pruned':\n%s", out)
	}
}

// TestAuditRefusesTwoDifferentPrincipals: --agent and --principal naming
// different things is a contradiction, and answering one of them silently would
// be the wrong half of the time.
func TestAuditRefusesTwoDifferentPrincipals(t *testing.T) {
	_, err := (&auditOptions{agent: "deploybot", principal: "ada"}).query(auditNow)
	if err == nil {
		t.Fatal("--agent and --principal naming different principals was accepted")
	}
}
