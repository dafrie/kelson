package controlstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/dafrie/kelson/internal/redact"
)

// The audit trail (issue #78, ADR-0026): what each principal actually did.
//
// # A bounded ring of ConfigMaps, one per UTC day
//
// ADR-0013 §1 says all server state is cluster state, and an audit trail that a
// restart forgot would be worse than none. So the records live where the specs
// and the history live: ConfigMaps in the state namespace, under the same
// provenance labels, readable with `kubectl get configmap -l
// kelson.dev/state=audit -o yaml` when kelson itself is the thing being
// investigated.
//
// A ConfigMap is a poor append log and this store does not pretend otherwise.
// etcd caps a value near 1 MiB, every append is a read-modify-write of the whole
// day, and a busy day would churn one object. Three decisions make the shape
// honest rather than merely convenient:
//
//   - **One object per UTC day.** The write set is one object, retention is a
//     delete of whole days, and a query for a time range reads only the days it
//     covers.
//   - **The day is a ring with a stated bound.** Past [AuditOptions.MaxPerDay]
//     records or [maxAuditDayBytes], the oldest records of that day are dropped
//     and the count of what was dropped is written onto the object. Nothing is
//     silently lost.
//   - **Truncation travels with the answer.** [AuditPage.Window] states the
//     retention boundary and how many records the ring dropped inside the range
//     queried, so a caller can never mistake a partial window for a complete one
//     (the repo's no-silent-caps rule).
//
// The recorded successor is the same one ADR-0013 names for the other stores —
// a CRD, or an operator-supplied external sink — and it lands behind this
// store's two methods.
//
// # Mutations are recorded, refusals are recorded, allowed reads are not
//
// A read changes nothing, and recording every Status poll would collapse the
// ring's window from days to minutes: the volume would evict the mutations the
// trail exists for. A *refused* request is recorded whatever its class,
// because a refusal is a security event and there are few of them. ADR-0026 §3
// records the trade and the way out (an external sink).
//
// # Nothing secret reaches a record
//
// The two free-text fields — the caller's reason and a failure message — go
// through [redact.Scrub] on the way in, and no field of a record holds a request
// payload. SetSecret registers its values with internal/redact before anything
// else happens to them (internal/api/secret.go), so a value that reached the
// process cannot reach a record even by way of an error message (issue #117).

// Audit error codes. Like every other Code here they pass to the wire verbatim
// (ADR-0013 §2).
const (
	// ErrAuditQuery is a query the store refuses: a page size above the
	// maximum, an unreadable page token, an outcome that is not one.
	ErrAuditQuery Code = "audit/invalid-query"
)

// AsAuditQuery reports whether err is an audit/invalid-query.
func AsAuditQuery(err error) bool { return hasCode(err, ErrAuditQuery) }

// AuditOutcome is what became of one request. Three values rather than the two
// an authorization decision has, because "the caller was allowed and the thing
// failed anyway" is the answer to "what did it actually do?" as often as either
// of the others.
type AuditOutcome string

const (
	// AuditAllowed is a request that was authorized and whose handler returned
	// without error.
	AuditAllowed AuditOutcome = "allowed"
	// AuditRefused is a request authorization turned away. Code carries which
	// refusal it was.
	AuditRefused AuditOutcome = "refused"
	// AuditFailed is a request that was authorized and then failed. Code
	// carries the failure's own taxonomy code where the error had one.
	AuditFailed AuditOutcome = "failed"
)

// ValidAuditOutcome reports whether v is one of the three outcomes. A filter
// naming anything else is refused rather than matching nothing, so a typo is
// not read as "no records".
func ValidAuditOutcome(v AuditOutcome) bool {
	switch v {
	case AuditAllowed, AuditRefused, AuditFailed:
		return true
	default:
		return false
	}
}

// AuditPrincipal is who acted, in the vocabulary internal/api's Principal uses.
// Type is "agent", "human" or "anonymous"; Name is the identity name of an
// agent or the display name of a logged-in human, and is empty for a
// bearer-password caller because a shared password names nobody.
type AuditPrincipal struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// String renders the principal the way an audit line reads: "agent:deploybot".
func (p AuditPrincipal) String() string {
	if p.Name == "" {
		return p.Type
	}
	return p.Type + ":" + p.Name
}

// AuditTarget is the (project, environment) the request acted on, as the
// authorization table derived it from the request. Both are empty for a request
// that names none — a cluster profile capture — and Project alone is set for a
// request that names no environment.
type AuditTarget struct {
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
}

// String renders the target for a listing.
func (t AuditTarget) String() string {
	switch {
	case t.Project == "":
		return ""
	case t.Environment == "":
		return t.Project
	default:
		return t.Project + "/" + t.Environment
	}
}

// Sources of an [AuditChange]'s numbers. They are separate values because a
// count of what was applied and a count of what changed are different claims,
// and a record that blurred them would be a record that lies about the blast
// radius.
const (
	// ChangeFromDiff means Added/Modified/Removed come from a computed
	// comparison — a server-side dry-run preview, a promotion's diff, a
	// rollback's preview.
	ChangeFromDiff = "diff"
	// ChangeFromRendered means no comparison was computed and the numbers
	// describe the set that was applied: Resources and Kinds only.
	ChangeFromRendered = "rendered"
)

// AuditChange is the resulting-diff half of the acceptance criterion, bounded.
//
// Revision is the pointer: it names the entry in the environment's rendered
// history, from which the exact manifests and any diff against them can be
// reconstructed (DeployService.History, rollback's preview). The numbers beside
// it are the summary a reader scans without following the pointer. Manifests
// are deliberately not here — they are already stored once, under a name, and a
// second unbounded copy per record would blow the ring's budget in one deploy.
type AuditChange struct {
	// Revision is what the operation recorded, empty when it recorded nothing.
	// For a deploy or a rollback it is the delivery revision, which the
	// environment's rendered history is keyed by. For a spec write — PutSpec,
	// Promote — it is the spec store's version, which is what a later Diff
	// would compare against. The two vocabularies are deliberately in one
	// field: it is "the name of the thing this call produced", and a record
	// that split it would make "what did it produce?" two questions.
	Revision string `json:"revision,omitempty"`
	// From is what the change was computed against or restored from: the
	// source revision of a promotion, the revision a rollback returned to.
	From string `json:"from,omitempty"`
	// Source says how to read the counts: [ChangeFromDiff] or
	// [ChangeFromRendered].
	Source string `json:"source,omitempty"`
	Added  int    `json:"added,omitempty"`
	// Modified is "changed" in the issue's vocabulary; it is named for
	// diff.Summary's field so the two do not drift.
	Modified int `json:"modified,omitempty"`
	Removed  int `json:"removed,omitempty"`
	// Resources is how many resources the applied set held. Meaningful for
	// [ChangeFromRendered]; zero otherwise.
	Resources int `json:"resources,omitempty"`
	// Kinds are the Kubernetes kinds touched, sorted, deduplicated and capped
	// at [maxAuditKinds] with a trailing "…" when there were more.
	Kinds []string `json:"kinds,omitempty"`
	// MaxRisk is diff.Summary.MaxRisk where a comparison was computed.
	MaxRisk string `json:"maxRisk,omitempty"`
}

// AuditRecord is one durable record of one request.
type AuditRecord struct {
	// ID is `<13-digit unix millis>-<8 hex>`: unique, and lexicographically
	// ordered by time, so it is both the ConfigMap data key and the pagination
	// cursor without a second index.
	ID   string    `json:"id"`
	Time time.Time `json:"time"`

	Principal AuditPrincipal `json:"principal"`
	// Scope is the credential's scope at the moment it acted, rendered as the
	// same one-line summary the refusal messages use
	// ("projects=shop environments=development operations=mutate"). Empty for a
	// principal that has no scope. It is recorded rather than looked up later
	// because an identity's scope can be re-issued and a record must say what
	// was true then.
	Scope string `json:"scope,omitempty"`

	// Procedure is the full ConnectRPC procedure,
	// "/kelson.v1alpha1.DeployService/Deploy".
	Procedure string `json:"procedure"`
	// Operation is the class the scope table filed the procedure under: read,
	// mutate or admin.
	Operation Operation   `json:"operation,omitempty"`
	Target    AuditTarget `json:"target,omitempty"`

	Outcome AuditOutcome `json:"outcome"`
	// Code is the refusal or failure code, empty on an allowed outcome.
	Code string `json:"code,omitempty"`
	// Message is bounded, scrubbed free text about a refusal or failure.
	Message string `json:"message,omitempty"`

	// DryRun is the rung the request asked for: "none", "render" or "server".
	// Empty for a procedure with no dry-run ladder.
	DryRun string `json:"dryRun,omitempty"`
	// DryRunSummary is what the dry run found, bounded, when the handler
	// computed one.
	DryRunSummary string `json:"dryRunSummary,omitempty"`

	Change *AuditChange `json:"change,omitempty"`

	// Reason is the caller's own statement of why, from the Kelson-Reason
	// header or a mutating MCP tool's `reason` parameter. Bounded and scrubbed;
	// absent when the caller supplied none, and never invented.
	Reason string `json:"reason,omitempty"`

	// IdempotencyKey ties a retry to the operation it repeats (issue #71).
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// Field bounds. Every one of them is enforced on the way in, and every one
// leaves a visible mark when it bites — a record that silently lost half a
// reason would be a record you could not reason from.
const (
	// MaxAuditReason bounds the caller-supplied reason. Long enough for a
	// sentence explaining an action, short enough that a thousand of them fit
	// the day's budget.
	MaxAuditReason = 512
	// maxAuditMessage bounds a refusal or failure message.
	maxAuditMessage = 512
	// maxAuditScope bounds the recorded scope summary.
	maxAuditScope = 256
	// maxAuditKinds bounds the kind list on a change.
	maxAuditKinds = 24
	// maxAuditField bounds the short identifier fields — procedure, project,
	// revision, idempotency key — against a caller that sends a megabyte where
	// a name goes.
	maxAuditField = 256
	// truncationMark is appended to anything this store shortened, so a reader
	// can tell a bounded value from a complete one.
	truncationMark = "…[truncated]"
)

// Store bounds and defaults.
const (
	// DefaultAuditRetentionDays is how many UTC days of records the store
	// keeps. A month is long enough to answer "what happened during that
	// incident" after the incident review is scheduled, and short enough that
	// the namespace does not accumulate objects nobody prunes.
	DefaultAuditRetentionDays = 30

	// MaxAuditRetentionDays is the ceiling, and it is a consequence of the
	// storage rather than a policy: one ConfigMap per day is one object per
	// day, and a year of them in one namespace is a listing nobody wants.
	// Longer retention is an external sink, which ADR-0026 records as the way
	// out.
	MaxAuditRetentionDays = 120

	// DefaultAuditRecordsPerDay bounds one day's ring. With mutations and
	// refusals only (see this file's header), a busy team deploying every few
	// minutes stays far below it.
	DefaultAuditRecordsPerDay = 2000

	// maxAuditDayBytes is the byte ceiling on one day's ConfigMap. etcd caps a
	// value near 1 MiB and the object carries labels, annotations and
	// managed-fields metadata besides the records; 768 KiB leaves that
	// headroom. Reaching it drops the oldest records of the day, visibly.
	maxAuditDayBytes = 768 * 1024

	// DefaultAuditPageSize is what a query with no page size returns.
	DefaultAuditPageSize = 100
	// MaxAuditPageSize is the largest page. A query above it is refused rather
	// than clamped, so a caller that asked for ten thousand records learns it
	// did not get them — the same rule MaxAgentTTL follows.
	MaxAuditPageSize = 500
)

// Audit object layout.
const (
	auditNamePrefix = "kelson-audit-"
	stateAudit      = "audit"
	labelAuditDay   = "kelson.dev/audit-day"

	// annAuditDropped counts the records the ring dropped from this day. It is
	// an annotation rather than a record so it survives the drop that produced
	// it.
	annAuditDropped = "kelson.dev/audit-dropped"

	// auditDayLayout is the day key, which is also the object-name suffix and
	// the label value.
	auditDayLayout = "20060102"
)

// AuditOptions configures an [AuditStore].
type AuditOptions struct {
	// Client is the typed client for the namespace the server runs against.
	Client kubernetes.Interface
	// Namespace is where state objects live.
	Namespace string
	// RetainDays is how many UTC days to keep. Zero selects
	// [DefaultAuditRetentionDays]; anything above [MaxAuditRetentionDays] is
	// refused at construction.
	RetainDays int
	// MaxPerDay bounds one day's ring. Zero selects
	// [DefaultAuditRecordsPerDay].
	MaxPerDay int
	// Now is the clock. Nil selects time.Now.
	Now func() time.Time
}

// AuditStore is the cluster-backed audit trail.
type AuditStore struct {
	client     kubernetes.Interface
	namespace  string
	retainDays int
	maxPerDay  int
	now        func() time.Time
}

// NewAuditStore returns a store over the day ConfigMaps in one namespace.
func NewAuditStore(opts AuditOptions) (*AuditStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("controlstore: a Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("controlstore: a namespace is required")
	}
	retain := opts.RetainDays
	switch {
	case retain == 0:
		retain = DefaultAuditRetentionDays
	case retain < 0 || retain > MaxAuditRetentionDays:
		return nil, fmt.Errorf("controlstore: audit retention of %d days is outside the permitted range (0 < days <= %d); "+
			"longer retention is an external sink, not a longer ring (ADR-0026)", retain, MaxAuditRetentionDays)
	}
	perDay := opts.MaxPerDay
	if perDay <= 0 {
		perDay = DefaultAuditRecordsPerDay
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &AuditStore{
		client:     opts.Client,
		namespace:  opts.Namespace,
		retainDays: retain,
		maxPerDay:  perDay,
		now:        now,
	}, nil
}

// RetainDays reports the store's retention window in whole UTC days. The API
// layer reports it on every query so a reader knows what the answer could not
// have covered.
func (s *AuditStore) RetainDays() int { return s.retainDays }

// Append writes one record.
//
// It is the only writer, and it is deliberately allowed to fail: the caller
// (internal/api) counts and logs a failure and does not fail the request it was
// recording. An audit write that could take a deployment down would be a new
// way to take a deployment down.
func (s *AuditStore) Append(ctx context.Context, rec AuditRecord) error {
	rec = s.normalise(rec)
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("controlstore: encode audit record: %w", err)
	}
	if len(blob) > maxAuditDayBytes/4 {
		// A single record a quarter of the day's budget wide is a bug in a
		// caller, not a record. Refusing it keeps one malformed write from
		// evicting a day's history.
		return fmt.Errorf("controlstore: audit record for %s is %d bytes, over the %d-byte per-record ceiling",
			rec.Procedure, len(blob), maxAuditDayBytes/4)
	}

	day := rec.Time.UTC().Format(auditDayLayout)
	cms := s.client.CoreV1().ConfigMaps(s.namespace)
	for attempt := 0; attempt < writeAttempts; attempt++ {
		existing, err := cms.Get(ctx, auditName(day), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, err := cms.Create(ctx, s.dayConfigMap(day, map[string]string{rec.ID: string(blob)}, 0), metav1.CreateOptions{}); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue // Another writer created the day between the Get and the Create.
				}
				return fmt.Errorf("controlstore: create audit day %s: %w", day, err)
			}
			return s.retain(ctx)
		}
		if err != nil {
			return fmt.Errorf("controlstore: read audit day %s: %w", day, err)
		}

		data := map[string]string{}
		for k, v := range existing.Data {
			data[k] = v
		}
		data[rec.ID] = string(blob)
		dropped := auditDropped(existing) + s.trim(data)

		updated := s.dayConfigMap(day, data, dropped)
		updated.ResourceVersion = existing.ResourceVersion
		if _, err := cms.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				continue // Another request appended first; re-read and append again.
			}
			return fmt.Errorf("controlstore: append audit record to %s: %w", day, err)
		}
		return s.retain(ctx)
	}
	return VersionConflict("audit/"+day,
		fmt.Sprintf("the audit record for %s is being written concurrently and did not settle in %d attempts", day, writeAttempts),
		"retry; a lost audit record is reported by the server rather than swallowed (ADR-0026 §3)")
}

// trim enforces the ring on one day, dropping the oldest records until the day
// is inside both bounds. It returns how many it dropped, which the caller adds
// to the day's running total — the number a query reports so a truncated window
// can never read as a complete one.
func (s *AuditStore) trim(data map[string]string) int {
	ids := make([]string, 0, len(data))
	size := 0
	for id, blob := range data {
		ids = append(ids, id)
		size += len(id) + len(blob)
	}
	sort.Strings(ids) // Oldest first: IDs lead with the timestamp.

	dropped := 0
	for i := 0; i < len(ids)-1 && (len(data) > s.maxPerDay || size > maxAuditDayBytes); i++ {
		id := ids[i]
		size -= len(id) + len(data[id])
		delete(data, id)
		dropped++
	}
	return dropped
}

// dayConfigMap builds the object one day is stored as.
func (s *AuditStore) dayConfigMap(day string, data map[string]string, dropped int) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      auditName(day),
			Namespace: s.namespace,
			Labels: map[string]string{
				labelManagedBy: managedByKelson,
				labelState:     stateAudit,
				labelAuditDay:  day,
			},
		},
		Data: data,
	}
	if dropped > 0 {
		cm.Annotations = map[string]string{annAuditDropped: strconv.Itoa(dropped)}
	}
	return cm
}

// retain deletes whole days that have fallen out of the retention window. A
// failure to prune is not a failure to record, so it is reported but the record
// it followed is already written.
func (s *AuditStore) retain(ctx context.Context) error {
	cutoff := s.retainedFrom().Format(auditDayLayout)
	days, err := s.days(ctx)
	if err != nil {
		return err
	}
	for i := range days {
		day := days[i].Labels[labelAuditDay]
		if day >= cutoff {
			continue
		}
		err := s.client.CoreV1().ConfigMaps(s.namespace).Delete(ctx, days[i].Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("controlstore: prune audit day %s: %w", day, err)
		}
	}
	return nil
}

// retainedFrom is the start of the oldest UTC day the store still keeps.
func (s *AuditStore) retainedFrom() time.Time {
	today := s.now().UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -(s.retainDays - 1))
}

// --- query -------------------------------------------------------------------

// AuditQuery filters the trail. Every field is optional and an empty one means
// "any"; a zero Since or Until means the range is open at that end, which
// [AuditWindow] then reports as a window the store cannot promise to have
// covered.
type AuditQuery struct {
	// Principal matches the identity name exactly ("deploybot").
	Principal string
	// PrincipalType matches "agent", "human" or "anonymous".
	PrincipalType string
	Project       string
	Environment   string
	Outcome       AuditOutcome
	// Procedure matches case-insensitively as a substring, so "Deploy" finds
	// /kelson.v1alpha1.DeployService/Deploy without the caller spelling the
	// package out.
	Procedure string
	// Since and Until bound the record time, Since inclusive and Until
	// exclusive.
	Since time.Time
	Until time.Time
	// Limit is the page size. Zero selects [DefaultAuditPageSize]; above
	// [MaxAuditPageSize] is refused.
	Limit int
	// PageToken continues a previous page. It is the ID of the last record of
	// that page; records are returned newest first, so the next page is
	// everything older than it.
	PageToken string
}

// AuditWindow is what the answer could not have covered. It rides on every
// page, always populated, because a query result that does not say where the
// store's memory ends is a query result a reader will over-trust.
type AuditWindow struct {
	// Complete is true only when the queried range lies inside the retention
	// window and no record inside it was dropped by the ring.
	Complete bool
	// Dropped is how many records the ring evicted from the days this query
	// covered. It is a count of records, not of days.
	Dropped int
	// RetainedFrom is the start of the oldest day the store keeps.
	RetainedFrom time.Time
	// RetainDays is the store's retention in whole days.
	RetainDays int
	// OldestRecorded is the time of the oldest record actually present in the
	// days this query covered, zero when there were none.
	OldestRecorded time.Time
	// Reason states in words why Complete is false, and is empty when it is
	// true.
	Reason string
}

// AuditPage is one page of records, newest first.
type AuditPage struct {
	Records []AuditRecord
	// NextPageToken continues the query, empty when this was the last page.
	NextPageToken string
	Window        AuditWindow
}

// Query returns the records matching q, newest first.
//
// The whole of each covered day is decoded and filtered in memory. That is
// affordable precisely because the day is a bounded ring: the ceiling on what
// one Query reads is [AuditOptions.MaxPerDay] per day covered, which is the
// same bound that keeps the ConfigMap inside etcd's value limit.
func (s *AuditStore) Query(ctx context.Context, q AuditQuery) (AuditPage, error) {
	limit, err := auditLimit(q.Limit)
	if err != nil {
		return AuditPage{}, err
	}
	if q.Outcome != "" && !ValidAuditOutcome(q.Outcome) {
		return AuditPage{}, newError(ErrAuditQuery, "audit",
			fmt.Sprintf("%q is not an audit outcome", q.Outcome),
			"filter on allowed, refused or failed; an unknown outcome is refused rather than matched against nothing")
	}

	days, err := s.days(ctx)
	if err != nil {
		return AuditPage{}, err
	}

	page := AuditPage{Window: AuditWindow{
		RetainedFrom: s.retainedFrom(),
		RetainDays:   s.retainDays,
	}}
	var matched []AuditRecord
	for i := range days {
		cm := &days[i]
		if !coversDay(cm.Labels[labelAuditDay], q.Since, q.Until) {
			continue
		}
		page.Window.Dropped += auditDropped(cm)
		for id, blob := range cm.Data {
			var rec AuditRecord
			if err := json.Unmarshal([]byte(blob), &rec); err != nil {
				return AuditPage{}, fmt.Errorf("controlstore: corrupt audit record %s in %s/%s: %w",
					id, cm.Namespace, cm.Name, err)
			}
			if rec.ID == "" {
				rec.ID = id
			}
			if page.Window.OldestRecorded.IsZero() || rec.Time.Before(page.Window.OldestRecorded) {
				page.Window.OldestRecorded = rec.Time
			}
			if q.matches(rec) {
				matched = append(matched, rec)
			}
		}
	}
	// Newest first, and the ID is time-ordered, so one sort answers both the
	// ordering and the cursor.
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })

	if len(matched) > limit {
		page.NextPageToken = matched[limit-1].ID
		matched = matched[:limit]
	}
	page.Records = matched
	page.Window.describe(q, s.retainDays)
	return page, nil
}

// matches is the filter. Every named field must match; an empty one matches
// everything.
func (q AuditQuery) matches(rec AuditRecord) bool {
	switch {
	case q.PageToken != "" && rec.ID >= q.PageToken:
		return false
	case q.Principal != "" && rec.Principal.Name != q.Principal:
		return false
	case q.PrincipalType != "" && rec.Principal.Type != q.PrincipalType:
		return false
	case q.Project != "" && rec.Target.Project != q.Project:
		return false
	case q.Environment != "" && rec.Target.Environment != q.Environment:
		return false
	case q.Outcome != "" && rec.Outcome != q.Outcome:
		return false
	case q.Procedure != "" && !strings.Contains(strings.ToLower(rec.Procedure), strings.ToLower(q.Procedure)):
		return false
	case !q.Since.IsZero() && rec.Time.Before(q.Since):
		return false
	case !q.Until.IsZero() && !rec.Time.Before(q.Until):
		return false
	default:
		return true
	}
}

// describe fills in whether the window is complete, and says why when it is
// not. Both halves are stated: a range reaching past what the store keeps, and
// records the ring dropped from inside the range.
func (w *AuditWindow) describe(q AuditQuery, retainDays int) {
	var reasons []string
	if q.Since.IsZero() || q.Since.Before(w.RetainedFrom) {
		reasons = append(reasons, fmt.Sprintf(
			"this query reaches before %s, and the store keeps %d days: anything older was pruned",
			w.RetainedFrom.Format("2006-01-02"), retainDays))
	}
	if w.Dropped > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d record(s) were dropped from the days this query covers because a day exceeded the ring's bound",
			w.Dropped))
	}
	w.Complete = len(reasons) == 0
	w.Reason = strings.Join(reasons, "; ")
}

// auditLimit resolves and bounds a page size.
func auditLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return DefaultAuditPageSize, nil
	case limit < 0 || limit > MaxAuditPageSize:
		return 0, newError(ErrAuditQuery, "audit",
			fmt.Sprintf("a page size of %d is outside the permitted range (0 < n <= %d)", limit, MaxAuditPageSize),
			"ask for a smaller page and follow the next page token; the export path pages rather than asking for everything at once")
	default:
		return limit, nil
	}
}

// coversDay reports whether a day key can hold a record inside [since, until).
// The comparison is on the day string, which sorts chronologically.
func coversDay(day string, since, until time.Time) bool {
	if day == "" {
		return false
	}
	if !since.IsZero() && day < since.UTC().Format(auditDayLayout) {
		return false
	}
	if !until.IsZero() && day > until.UTC().Format(auditDayLayout) {
		return false
	}
	return true
}

// days lists the audit day ConfigMaps, newest first.
func (s *AuditStore) days(ctx context.Context) ([]corev1.ConfigMap, error) {
	list, err := s.client.CoreV1().ConfigMaps(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			labelManagedBy: managedByKelson,
			labelState:     stateAudit,
		}).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("controlstore: list audit days in %s: %w", s.namespace, err)
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		return items[i].Labels[labelAuditDay] > items[j].Labels[labelAuditDay]
	})
	return items, nil
}

func auditDropped(cm *corev1.ConfigMap) int {
	n, err := strconv.Atoi(cm.Annotations[annAuditDropped])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func auditName(day string) string { return auditNamePrefix + day }

// --- normalisation -----------------------------------------------------------

// normalise stamps the fields the store owns and enforces every bound. It is
// applied on the way in rather than trusted from the caller, because the caller
// is the API layer and the API layer's inputs come off the wire.
func (s *AuditStore) normalise(rec AuditRecord) AuditRecord {
	if rec.Time.IsZero() {
		rec.Time = s.now()
	}
	rec.Time = rec.Time.UTC().Truncate(time.Millisecond)
	if rec.ID == "" {
		rec.ID = newAuditID(rec.Time)
	}
	if rec.Outcome == "" {
		rec.Outcome = AuditAllowed
	}
	if rec.Principal.Type == "" {
		rec.Principal.Type = "anonymous"
	}

	rec.Principal.Name = clip(rec.Principal.Name, maxAuditField)
	rec.Principal.Type = clip(rec.Principal.Type, maxAuditField)
	rec.Procedure = clip(rec.Procedure, maxAuditField)
	rec.Target.Project = clip(rec.Target.Project, maxAuditField)
	rec.Target.Environment = clip(rec.Target.Environment, maxAuditField)
	rec.Code = clip(rec.Code, maxAuditField)
	rec.DryRun = clip(rec.DryRun, maxAuditField)
	rec.IdempotencyKey = clip(rec.IdempotencyKey, maxAuditField)
	rec.Scope = clip(rec.Scope, maxAuditScope)

	// The two free-text fields are the ones a secret could ride in on, so they
	// are scrubbed as well as bounded (issue #117).
	rec.Message = clip(redact.Scrub(rec.Message), maxAuditMessage)
	rec.DryRunSummary = clip(redact.Scrub(rec.DryRunSummary), maxAuditMessage)
	rec.Reason = clip(redact.Scrub(rec.Reason), MaxAuditReason)

	if rec.Change != nil {
		change := *rec.Change
		change.Revision = clip(change.Revision, maxAuditField)
		change.From = clip(change.From, maxAuditField)
		change.Source = clip(change.Source, maxAuditField)
		change.MaxRisk = clip(change.MaxRisk, maxAuditField)
		change.Kinds = boundKinds(change.Kinds)
		rec.Change = &change
	}
	return rec
}

// boundKinds sorts, deduplicates and caps a kind list, marking the cap.
func boundKinds(kinds []string) []string {
	if len(kinds) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kind = clip(kind, maxAuditField)
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	sort.Strings(out)
	if len(out) > maxAuditKinds {
		out = append(out[:maxAuditKinds:maxAuditKinds], truncationMark)
	}
	return out
}

// clip bounds a string and says so when it had to. The mark is what makes the
// bound honest: a reader can tell a short value from a shortened one.
func clip(v string, max int) string {
	if len(v) <= max {
		return v
	}
	return v[:max] + truncationMark
}

// newAuditID mints the record id: the millisecond timestamp zero-padded to 13
// digits — enough until the year 2286 — and 4 random bytes to separate records
// minted in the same millisecond by two replicas.
func newAuditID(at time.Time) string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any supported platform. If it somehow
		// does, a nanosecond suffix still orders and still separates; it is
		// only weaker against a deliberate collision, which is not the threat
		// an id is defending against here.
		return fmt.Sprintf("%013d-%08x", at.UnixMilli(), at.Nanosecond())
	}
	return fmt.Sprintf("%013d-%s", at.UnixMilli(), hex.EncodeToString(buf[:]))
}
