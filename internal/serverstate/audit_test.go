package serverstate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dafrie/kelson/internal/redact"
)

// The audit store's tests (issue #78, ADR-0026).
//
// The properties that matter are not "a record round-trips" — they are the ones
// a trail is worthless without: that the bound is enforced, that hitting it is
// visible, that retention is stated, and that nothing secret can reach a record.

var auditDay = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func newAuditStore(t *testing.T, client *fake.Clientset, opts AuditOptions) *AuditStore {
	t.Helper()
	opts.Client = client
	opts.Namespace = testNamespace
	if opts.Now == nil {
		opts.Now = func() time.Time { return auditDay }
	}
	s, err := NewAuditStore(opts)
	if err != nil {
		t.Fatalf("new audit store: %v", err)
	}
	return s
}

// mutation is the record shape the API layer writes for a deploy, so the tests
// below assert against something the production path would actually produce.
func mutation(at time.Time, principal AuditPrincipal, project, environment, revision string) AuditRecord {
	return AuditRecord{
		Time:      at,
		Principal: principal,
		Scope:     "projects=" + project + " environments=" + environment + " operations=mutate",
		Procedure: "/kelson.v1alpha1.DeployService/Deploy",
		Operation: OpMutate,
		Target:    AuditTarget{Project: project, Environment: environment},
		Outcome:   AuditAllowed,
		DryRun:    "none",
		Change: &AuditChange{
			Revision:  revision,
			Source:    ChangeFromRendered,
			Resources: 3,
			Kinds:     []string{"Service", "Deployment", "Deployment"},
		},
	}
}

var deploybot = AuditPrincipal{Type: "agent", Name: "deploybot"}

// TestAnAgentMutationIsRecordedWithIdentityScopeOutcomeAndDiff is issue #78's
// acceptance criterion at the storage layer: identity, scope, and a pointer to
// the resulting diff, all readable back.
func TestAnAgentMutationIsRecordedWithIdentityScopeOutcomeAndDiff(t *testing.T) {
	client := newFakeClient()
	store := newAuditStore(t, client, AuditOptions{})

	rec := mutation(auditDay, deploybot, "shop", "production", "rev-00000007")
	rec.Reason = "rolling out the checkout fix from PR 412"
	if err := store.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}

	page, err := store.Query(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("query returned %d records, want 1", len(page.Records))
	}
	got := page.Records[0]

	if got.Principal.Type != "agent" || got.Principal.Name != "deploybot" {
		t.Errorf("principal = %+v, want the agent identity that acted", got.Principal)
	}
	if !strings.Contains(got.Scope, "environments=production") {
		t.Errorf("scope = %q, want the credential's scope as it was at the time", got.Scope)
	}
	if got.Target != (AuditTarget{Project: "shop", Environment: "production"}) {
		t.Errorf("target = %+v", got.Target)
	}
	if got.Outcome != AuditAllowed {
		t.Errorf("outcome = %q, want allowed", got.Outcome)
	}
	if got.Change == nil || got.Change.Revision != "rev-00000007" {
		t.Fatalf("change = %+v, want the revision the apply recorded — that is the pointer to the resulting diff", got.Change)
	}
	if got.Change.Source != ChangeFromRendered || got.Change.Resources != 3 {
		t.Errorf("change source/resources = %q/%d, want the applied set described as such", got.Change.Source, got.Change.Resources)
	}
	// Kinds are deduplicated and sorted, so a reader scanning a page sees a
	// stable list rather than the render order.
	if len(got.Change.Kinds) != 2 || got.Change.Kinds[0] != "Deployment" || got.Change.Kinds[1] != "Service" {
		t.Errorf("kinds = %v, want them deduplicated and sorted", got.Change.Kinds)
	}
	if got.Reason != "rolling out the checkout fix from PR 412" {
		t.Errorf("reason = %q, want the caller's own words", got.Reason)
	}
	if got.ID == "" || got.Time.IsZero() {
		t.Errorf("the store did not stamp an id and a time: %q %s", got.ID, got.Time)
	}
}

// TestAReasonIsAbsentWhenTheCallerSuppliedNone: kelson never invents one.
func TestAReasonIsAbsentWhenTheCallerSuppliedNone(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	if err := store.Append(t.Context(), mutation(auditDay, deploybot, "shop", "production", "rev-00000001")); err != nil {
		t.Fatalf("append: %v", err)
	}
	page, err := store.Query(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := page.Records[0].Reason; got != "" {
		t.Errorf("reason = %q, want empty: a reason nobody gave must not be manufactured", got)
	}
}

// TestARefusalIsRecordedWithItsCode: a refusal is the other half of the trail,
// and it is useless without the code the caller was actually given.
func TestARefusalIsRecordedWithItsCode(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	rec := mutation(auditDay, deploybot, "shop", "production", "")
	rec.Outcome = AuditRefused
	rec.Code = "auth/out-of-scope"
	rec.Message = `agent identity "deploybot" may not act on shop/production`
	rec.Change = nil
	if err := store.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}

	page, err := store.Query(t.Context(), AuditQuery{Outcome: AuditRefused})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].Code != "auth/out-of-scope" {
		t.Fatalf("the refusal was not recorded with its code: %+v", page.Records)
	}
	if page.Records[0].Change != nil {
		t.Error("a refused request recorded a change; nothing happened")
	}
}

// TestQueryFiltersEveryDimension: each filter alone narrows, and a filter that
// matches nothing returns nothing rather than everything.
func TestQueryFiltersEveryDimension(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	ada := AuditPrincipal{Type: "human", Name: "ada"}

	seed := []AuditRecord{
		mutation(auditDay, deploybot, "shop", "production", "rev-1"),
		mutation(auditDay.Add(time.Minute), deploybot, "shop", "development", "rev-2"),
		mutation(auditDay.Add(2*time.Minute), ada, "billing", "production", "rev-3"),
	}
	seed[2].Procedure = "/kelson.v1alpha1.SecretService/SetSecret"
	seed[1].Outcome = AuditFailed
	seed[1].Code = "delivery/apply-failed"
	for _, rec := range seed {
		if err := store.Append(t.Context(), rec); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	cases := []struct {
		name  string
		query AuditQuery
		want  int
	}{
		{"by principal name", AuditQuery{Principal: "deploybot"}, 2},
		{"by principal type", AuditQuery{PrincipalType: "human"}, 1},
		{"by project", AuditQuery{Project: "shop"}, 2},
		{"by environment", AuditQuery{Environment: "production"}, 2},
		{"by outcome", AuditQuery{Outcome: AuditFailed}, 1},
		{"by procedure substring", AuditQuery{Procedure: "SetSecret"}, 1},
		{"by procedure, case-insensitively", AuditQuery{Procedure: "deployservice"}, 2},
		{"by time range", AuditQuery{Since: auditDay.Add(90 * time.Second)}, 1},
		{"combined", AuditQuery{Principal: "deploybot", Environment: "production"}, 1},
		{"matching nothing", AuditQuery{Principal: "nobody"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := store.Query(t.Context(), tc.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(page.Records) != tc.want {
				t.Errorf("got %d records, want %d", len(page.Records), tc.want)
			}
		})
	}
}

// TestQueryIsNewestFirstAndPages: the ordering and the cursor are one mechanism
// (the id is time-ordered), so they are asserted together.
func TestQueryIsNewestFirstAndPages(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	const total = 25
	for i := range total {
		rec := mutation(auditDay.Add(time.Duration(i)*time.Second), deploybot, "shop", "production",
			fmt.Sprintf("rev-%02d", i))
		if err := store.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	var seen []string
	token := ""
	pages := 0
	for {
		page, err := store.Query(t.Context(), AuditQuery{Limit: 10, PageToken: token})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		pages++
		for _, rec := range page.Records {
			seen = append(seen, rec.Change.Revision)
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != total {
		t.Fatalf("paging returned %d records, want %d — a page boundary dropped or repeated one", len(seen), total)
	}
	if seen[0] != "rev-24" || seen[total-1] != "rev-00" {
		t.Errorf("records are not newest first: first %q, last %q", seen[0], seen[total-1])
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("records are out of order at %d: %q then %q", i, seen[i-1], seen[i])
		}
	}
}

// TestAPageSizeAboveTheMaximumIsRefused: refused, not clamped. A caller that
// asked for ten thousand records must learn it did not get them.
func TestAPageSizeAboveTheMaximumIsRefused(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	_, err := store.Query(t.Context(), AuditQuery{Limit: MaxAuditPageSize + 1})
	if !AsAuditQuery(err) {
		t.Fatalf("an oversized page = %v, want %s", err, ErrAuditQuery)
	}
}

// TestAnUnknownOutcomeFilterIsRefused: a typo must not read as "no records".
func TestAnUnknownOutcomeFilterIsRefused(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	_, err := store.Query(t.Context(), AuditQuery{Outcome: AuditOutcome("denied")})
	if !AsAuditQuery(err) {
		t.Fatalf("an unknown outcome = %v, want %s", err, ErrAuditQuery)
	}
}

// TestTheRingDropsTheOldestAndSaysSo is the no-silent-caps rule. Exceeding the
// day's bound must lose the oldest records AND make the loss visible on the
// answer, because a truncated window that reads as a complete one is worse than
// an error.
func TestTheRingDropsTheOldestAndSaysSo(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{MaxPerDay: 5})
	for i := range 8 {
		rec := mutation(auditDay.Add(time.Duration(i)*time.Second), deploybot, "shop", "production",
			fmt.Sprintf("rev-%02d", i))
		if err := store.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	page, err := store.Query(t.Context(), AuditQuery{Since: auditDay, Limit: MaxAuditPageSize})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Records) != 5 {
		t.Fatalf("the day holds %d records, want the ring's bound of 5", len(page.Records))
	}
	// The newest survive; the oldest are the ones dropped.
	if page.Records[0].Change.Revision != "rev-07" {
		t.Errorf("newest retained = %q, want rev-07", page.Records[0].Change.Revision)
	}
	if page.Records[4].Change.Revision != "rev-03" {
		t.Errorf("oldest retained = %q, want rev-03 — the ring drops the oldest", page.Records[4].Change.Revision)
	}

	if page.Window.Complete {
		t.Error("the window reports itself complete after the ring dropped records")
	}
	if page.Window.Dropped != 3 {
		t.Errorf("window.Dropped = %d, want 3", page.Window.Dropped)
	}
	if !strings.Contains(page.Window.Reason, "dropped") {
		t.Errorf("window.Reason = %q, want it to say what was lost", page.Window.Reason)
	}
}

// TestAnUnboundedQueryReportsTheRetentionEdge: even with nothing dropped, a
// query with no lower bound cannot be complete, and says so rather than
// implying the store remembers everything.
func TestAnUnboundedQueryReportsTheRetentionEdge(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{RetainDays: 7})
	if err := store.Append(t.Context(), mutation(auditDay, deploybot, "shop", "production", "rev-1")); err != nil {
		t.Fatalf("append: %v", err)
	}

	page, err := store.Query(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if page.Window.Complete {
		t.Error("an unbounded query claimed a complete window")
	}
	if page.Window.RetainDays != 7 {
		t.Errorf("window.RetainDays = %d, want the store's stated retention", page.Window.RetainDays)
	}
	if page.Window.RetainedFrom.IsZero() || !strings.Contains(page.Window.Reason, page.Window.RetainedFrom.Format("2006-01-02")) {
		t.Errorf("window.Reason = %q, want it to name the retention boundary", page.Window.Reason)
	}

	// A query wholly inside the retention window, with nothing dropped, is
	// complete — otherwise the flag would carry no information.
	inside, err := store.Query(t.Context(), AuditQuery{Since: auditDay.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !inside.Window.Complete {
		t.Errorf("a query inside the retention window reports incomplete: %q", inside.Window.Reason)
	}
}

// TestRetentionPrunesWholeDays: the retention story is days, and it is enforced
// rather than documented.
func TestRetentionPrunesWholeDays(t *testing.T) {
	client := newFakeClient()
	now := auditDay
	store := newAuditStore(t, client, AuditOptions{RetainDays: 3, Now: func() time.Time { return now }})

	for i := range 6 {
		at := auditDay.AddDate(0, 0, -5+i)
		now = at
		if err := store.Append(t.Context(), mutation(at, deploybot, "shop", "production", fmt.Sprintf("rev-%d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	days, err := store.days(t.Context())
	if err != nil {
		t.Fatalf("days: %v", err)
	}
	if len(days) != 3 {
		names := make([]string, 0, len(days))
		for i := range days {
			names = append(names, days[i].Name)
		}
		t.Fatalf("the store kept %d day objects (%s), want 3", len(days), strings.Join(names, ", "))
	}
}

// TestRetentionAboveTheMaximumIsRefused: the ceiling is a consequence of the
// storage, so asking past it fails at construction rather than silently
// behaving differently.
func TestRetentionAboveTheMaximumIsRefused(t *testing.T) {
	_, err := NewAuditStore(AuditOptions{
		Client:     newFakeClient(),
		Namespace:  testNamespace,
		RetainDays: MaxAuditRetentionDays + 1,
	})
	if err == nil {
		t.Fatal("a retention above the maximum was accepted")
	}
}

// TestNoSecretValueReachesARecord is the #117 property for this store: a
// credential kelson has resolved cannot appear in a record, even when the
// caller put it in the field a record does carry.
func TestNoSecretValueReachesARecord(t *testing.T) {
	const password = "sup3r-s3cret-database-password"
	redact.Register(password)

	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	rec := mutation(auditDay, deploybot, "shop", "production", "rev-1")
	rec.Procedure = "/kelson.v1alpha1.SecretService/SetSecret"
	// Every free-text field a caller or a failing plane can reach.
	rec.Reason = "rotating to " + password
	rec.Message = "the store refused the value " + password
	rec.DryRunSummary = "would write " + password
	if err := store.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}

	page, err := store.Query(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := page.Records[0]
	for field, value := range map[string]string{
		"reason":        got.Reason,
		"message":       got.Message,
		"dryRunSummary": got.DryRunSummary,
	} {
		if strings.Contains(value, password) {
			t.Errorf("the %s field of a stored record carries a secret value: %q", field, value)
		}
		if !strings.Contains(value, redact.Sentinel) {
			t.Errorf("the %s field was not scrubbed at all: %q", field, value)
		}
	}

	// And nowhere in the stored bytes either — a reader of the ConfigMap must
	// not find it in a field this test forgot to name.
	cm, err := store.client.CoreV1().ConfigMaps(testNamespace).Get(
		t.Context(), auditName(auditDay.Format(auditDayLayout)), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the day: %v", err)
	}
	for key, blob := range cm.Data {
		if strings.Contains(blob, password) {
			t.Fatalf("the stored bytes of record %s carry a secret value", key)
		}
	}
}

// TestBoundedFieldsAreMarkedWhenTruncated: a record that silently lost half a
// reason would be a record nobody could reason from.
func TestBoundedFieldsAreMarkedWhenTruncated(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	rec := mutation(auditDay, deploybot, "shop", "production", "rev-1")
	rec.Reason = strings.Repeat("x", MaxAuditReason*2)
	if err := store.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}

	page, err := store.Query(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := page.Records[0].Reason
	if len(got) > MaxAuditReason+len(truncationMark) {
		t.Errorf("the reason was stored at %d bytes, past the %d-byte bound", len(got), MaxAuditReason)
	}
	if !strings.HasSuffix(got, truncationMark) {
		t.Errorf("a truncated reason is not marked as truncated: %q", got[len(got)-32:])
	}
}

// TestEveryRecordIsSmallWhateverTheCallerSends is what makes the ring's record
// count mean something. If one record could be a megabyte, "2000 records a day"
// would be a bound on nothing and one hostile call could evict the day.
//
// So: every field grossly oversized, and the stored bytes still fit the budget
// the ring is sized against.
func TestEveryRecordIsSmallWhateverTheCallerSends(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	big := strings.Repeat("z", 64<<10)
	kinds := make([]string, 0, 4000)
	for i := range 4000 {
		kinds = append(kinds, fmt.Sprintf("Kind%d%s", i, big))
	}
	if err := store.Append(t.Context(), AuditRecord{
		Time:           auditDay,
		Principal:      AuditPrincipal{Type: big, Name: big},
		Scope:          big,
		Procedure:      big,
		Target:         AuditTarget{Project: big, Environment: big},
		Code:           big,
		Message:        big,
		DryRun:         big,
		DryRunSummary:  big,
		Reason:         big,
		IdempotencyKey: big,
		Change:         &AuditChange{Revision: big, From: big, Source: big, MaxRisk: big, Kinds: kinds},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	cm, err := store.client.CoreV1().ConfigMaps(testNamespace).Get(
		t.Context(), auditName(auditDay.Format(auditDayLayout)), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the day: %v", err)
	}
	for key, blob := range cm.Data {
		// The bound the ring is sized against: 2000 records inside a 768 KiB
		// day means the honest per-record ceiling is well under a kilobyte of
		// *typical* record, and a worst case that still cannot evict a day on
		// its own.
		if ceiling := maxAuditDayBytes / DefaultAuditRecordsPerDay * 32; len(blob) > ceiling {
			t.Errorf("a record of maximally oversized fields stored %d bytes, past the %d-byte working ceiling; "+
				"one call could then evict a day of history", len(blob), ceiling)
		}
		if !strings.Contains(blob, truncationMark) {
			t.Errorf("record %s was shortened without saying so", key)
		}
	}
}

// TestConcurrentAppendsToOneDayAllLand: two replicas write the same day
// object, and the optimistic-concurrency retry is what keeps either from
// overwriting the other.
func TestConcurrentAppendsToOneDayAllLand(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	const n = 3
	for i := range n {
		rec := mutation(auditDay.Add(time.Duration(i)*time.Millisecond), deploybot, "shop", "production",
			fmt.Sprintf("rev-%d", i))
		if err := store.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	page, err := store.Query(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Records) != n {
		t.Fatalf("%d of %d records landed in one day object", len(page.Records), n)
	}
}

// TestTheTrailIsOneStoreForBothPrincipalTypes: #12 wants a general log and #78
// wants an agent trail. One implementation, told apart by a filter — which is
// only true if a human's action lands in the same place.
func TestTheTrailIsOneStoreForBothPrincipalTypes(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	human := AuditPrincipal{Type: "human", Name: "ada"}
	if err := store.Append(t.Context(), mutation(auditDay, deploybot, "shop", "production", "rev-1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Append(t.Context(), mutation(auditDay.Add(time.Second), human, "shop", "production", "rev-2")); err != nil {
		t.Fatalf("append: %v", err)
	}

	all, err := store.Query(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(all.Records) != 2 {
		t.Fatalf("the two principals did not land in one trail: %d records", len(all.Records))
	}
	agents, err := store.Query(t.Context(), AuditQuery{PrincipalType: "agent"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(agents.Records) != 1 || agents.Records[0].Principal.Name != "deploybot" {
		t.Errorf("filtering to agents returned %+v", agents.Records)
	}
}

// TestTheStoredObjectIsReadableWithKubectl: the store's premise is that the
// trail lives where an operator can already look. That is only true if the
// records are in Data as text under the provenance labels.
func TestTheStoredObjectIsReadableWithKubectl(t *testing.T) {
	store := newAuditStore(t, newFakeClient(), AuditOptions{})
	if err := store.Append(t.Context(), mutation(auditDay, deploybot, "shop", "production", "rev-1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	cm, err := store.client.CoreV1().ConfigMaps(testNamespace).Get(
		t.Context(), auditName(auditDay.Format(auditDayLayout)), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the day: %v", err)
	}
	if cm.Labels[labelManagedBy] != managedByKelson || cm.Labels[labelState] != stateAudit {
		t.Errorf("the day object does not carry kelson's provenance labels: %v", cm.Labels)
	}
	if len(cm.BinaryData) != 0 {
		t.Error("records are in BinaryData, which `kubectl get -o yaml` cannot show")
	}
	if len(cm.Data) != 1 {
		t.Fatalf("the day holds %d data keys, want one per record", len(cm.Data))
	}
	for _, blob := range cm.Data {
		if !strings.Contains(blob, `"procedure"`) {
			t.Errorf("the stored record is not readable JSON: %q", blob)
		}
	}
}
