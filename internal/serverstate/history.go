package serverstate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
)

// History-store layout (ADR-0013 §1): one ConfigMap per revision.
const (
	historyNamePrefix = "kelson-hist-"

	// recordKey holds the journal record as JSON in Data, so
	// `kubectl get configmap -o yaml` shows what was deployed and when.
	recordKey = "record"
	// renderedKey holds the rendered manifests in BinaryData.
	//
	// ConfigMap Data values must be valid UTF-8 strings, which gzip output is
	// not, and base64-in-Data would cost a third of the byte budget for
	// nothing. BinaryData takes raw bytes and the API server base64-encodes it
	// on the wire itself, so the stored payload is gzip and only gzip.
	renderedKey = "rendered"
)

// revisionPrefix and revisionWidth mirror the JSONL store's "rev-%08d" scheme
// (internal/delivery/direct/history.go). They are duplicated rather than
// imported because direct keeps them unexported; the two stores must agree,
// since a revision id crosses the direct.History seam in both directions.
const (
	revisionPrefix = "rev-"
	revisionWidth  = 8
)

// maxRenderedBytes is the compressed-payload ceiling for one revision.
//
// etcd caps a value at ~1 MiB, and the ConfigMap carries a record, labels,
// annotations and managed-fields metadata besides the rendered bytes. 900 KiB
// leaves that headroom. Exceeding it fails the append loudly with
// store/too-large rather than storing a truncated revision: rollback replays
// recorded bytes, so a history that lies is worse than none (ADR-0013 §1).
const maxRenderedBytes = 900 * 1024

// appendAttempts bounds the revision-collision retry. See Append.
const appendAttempts = 5

// HistoryOptions configures a HistoryStore.
type HistoryOptions struct {
	// Client is the typed client for the namespace the server runs against.
	Client kubernetes.Interface
	// Namespace is where state ConfigMaps live.
	Namespace string
	// Keep is the retention count; <= 0 selects direct.DefaultKeep, so the
	// server and the CLI retain the same depth of history.
	Keep int
	// Now is injectable so recorded timestamps are deterministic in tests.
	Now func() time.Time
}

// HistoryStore is the cluster-backed rendered-history: kelson-server's
// implementation of direct.History (ADR-0013 §1). The CLI keeps its local
// JSONL journal; the two are separate stores by design, so a deploy made
// through one is not visible in the other's history.
type HistoryStore struct {
	client    kubernetes.Interface
	namespace string
	keep      int
	now       func() time.Time

	// ctx is bound at construction because direct.History is context-free: the
	// adapter's history calls are bookkeeping around an apply that carries its
	// own context. A server handler calls WithContext to hand the store the
	// request's context so a cancelled RPC does not leave a call in flight.
	ctx context.Context
}

var _ direct.History = (*HistoryStore)(nil)

// NewHistoryStore returns a HistoryStore reading and writing ConfigMaps in one
// namespace.
func NewHistoryStore(opts HistoryOptions) (*HistoryStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("serverstate: a Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("serverstate: a namespace is required")
	}
	keep := opts.Keep
	if keep <= 0 {
		keep = direct.DefaultKeep
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &HistoryStore{client: opts.Client, namespace: opts.Namespace, keep: keep, now: now, ctx: context.Background()}, nil
}

// WithContext returns a shallow copy of the store whose cluster calls run under
// ctx. It is how a request-scoped handler propagates cancellation through the
// context-free direct.History seam.
func (h *HistoryStore) WithContext(ctx context.Context) *HistoryStore {
	scoped := *h
	scoped.ctx = ctx
	return &scoped
}

// Append records one revision as a ConfigMap: the journal record as JSON and
// the rendered manifests gzipped into BinaryData.
//
// Concurrency: two servers can compute the same NextRevision before either has
// written it, and the loser's Create comes back AlreadyExists. Rather than
// failing the deploy on a name collision the append recomputes the next
// revision and retries, bounded by appendAttempts — the revision is a local
// sequence id, not a promise about ordering between two writers. The returned
// delivery.Entry therefore carries the revision that was actually recorded,
// which under a genuine race is not the one the caller passed in.
func (h *HistoryStore) Append(project, environment string, rec direct.Record, rendered []byte) (delivery.Entry, error) {
	if err := h.validScope(project, environment); err != nil {
		return delivery.Entry{}, err
	}
	if err := validRevision(rec.Revision); err != nil {
		return delivery.Entry{}, err
	}
	if rec.Type == "" {
		rec.Type = direct.TypeDeploy
	}
	if rec.CommittedAt == "" {
		rec.CommittedAt = h.now().UTC().Format(time.RFC3339)
	}
	if rec.Resources == nil {
		rec.Resources = []direct.ResourceRef{}
	}

	blob, err := compress(rendered)
	if err != nil {
		return delivery.Entry{}, err
	}
	if len(blob) > maxRenderedBytes {
		return delivery.Entry{}, TooLarge(historyRef(project, environment, rec.Revision),
			fmt.Sprintf("the rendered output is %d bytes compressed, over the %d-byte ConfigMap budget", len(blob), maxRenderedBytes),
			"split the environment into fewer components so each deploy renders less; the etcd value limit binds the CRD store too, and kelson will not record a truncated revision")
	}

	cms := h.client.CoreV1().ConfigMaps(h.namespace)
	for attempt := 0; attempt < appendAttempts; attempt++ {
		cm, err := h.configMap(project, environment, rec, blob)
		if err != nil {
			return delivery.Entry{}, err
		}
		_, err = cms.Create(h.ctx, cm, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			next, nextErr := h.NextRevision(project, environment)
			if nextErr != nil {
				return delivery.Entry{}, nextErr
			}
			rec.Revision = next
			continue
		}
		if err != nil {
			return delivery.Entry{}, fmt.Errorf("serverstate: record revision %s: %w",
				historyRef(project, environment, rec.Revision), err)
		}
		if err := h.retain(project, environment); err != nil {
			return delivery.Entry{}, err
		}
		return rec.Entry(), nil
	}
	return delivery.Entry{}, fmt.Errorf("serverstate: could not allocate a free revision for %s/%s in %d attempts; another writer is appending in a loop",
		project, environment, appendAttempts)
}

// List returns the recorded revisions newest first.
func (h *HistoryStore) List(project, environment string) ([]direct.Record, error) {
	if err := h.validScope(project, environment); err != nil {
		return nil, err
	}
	cms, err := h.list(project, environment)
	if err != nil {
		return nil, err
	}
	out := make([]direct.Record, 0, len(cms))
	for i := range cms {
		rec, err := decodeRecord(&cms[i])
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// Latest returns the newest record, or nil when nothing has been recorded.
func (h *HistoryStore) Latest(project, environment string) (*direct.Record, error) {
	recs, err := h.List(project, environment)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	newest := recs[0]
	return &newest, nil
}

// Get returns the record for one revision, or nil when retention has dropped
// it. A pruned revision is (nil, nil), matching the JSONL store: the adapter
// distinguishes "not retained" from "the store failed".
func (h *HistoryStore) Get(project, environment, revision string) (*direct.Record, error) {
	cm, err := h.revision(project, environment, revision)
	if err != nil || cm == nil {
		return nil, err
	}
	rec, err := decodeRecord(cm)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// Rendered returns the rendered output recorded for one revision, verbatim and
// in apply order.
func (h *HistoryStore) Rendered(project, environment, revision string) ([]byte, error) {
	cm, err := h.revision(project, environment, revision)
	if err != nil {
		return nil, err
	}
	if cm == nil {
		return nil, NotFound(historyRef(project, environment, revision),
			fmt.Sprintf("revision %q of %s/%s is not in the retained history", revision, project, environment),
			"list the history for the revisions that still exist; older ones are pruned by the retention policy")
	}
	return decompress(historyRef(project, environment, revision), cm.BinaryData[renderedKey])
}

// NextRevision allocates the next revision id: the highest recorded sequence
// plus one, zero-padded so revisions sort lexicographically and read as an
// obvious counter next to the Git modes' shas.
func (h *HistoryStore) NextRevision(project, environment string) (string, error) {
	if err := h.validScope(project, environment); err != nil {
		return "", err
	}
	cms, err := h.list(project, environment)
	if err != nil {
		return "", err
	}
	highest := 0
	for i := range cms {
		if n, ok := parseRevision(cms[i].Labels[labelRevision]); ok && n > highest {
			highest = n
		}
	}
	return formatRevision(highest + 1), nil
}

// --- cluster access ---------------------------------------------------------

// list returns the revision ConfigMaps of one environment, newest first.
func (h *HistoryStore) list(project, environment string) ([]corev1.ConfigMap, error) {
	list, err := h.client.CoreV1().ConfigMaps(h.namespace).List(h.ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			labelManagedBy:   managedByKelson,
			labelState:       stateHistory,
			labelProject:     project,
			labelEnvironment: environment,
		}).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("serverstate: list history for %s/%s: %w", project, environment, err)
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		a, aok := parseRevision(items[i].Labels[labelRevision])
		b, bok := parseRevision(items[j].Labels[labelRevision])
		if aok && bok {
			return a > b
		}
		return items[i].Name > items[j].Name
	})
	return items, nil
}

// revision reads one revision's ConfigMap, or nil when it is not retained.
func (h *HistoryStore) revision(project, environment, revision string) (*corev1.ConfigMap, error) {
	if err := h.validScope(project, environment); err != nil {
		return nil, err
	}
	if err := validRevision(revision); err != nil {
		return nil, err
	}
	cm, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(h.ctx, historyName(project, environment, revision), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("serverstate: read revision %s: %w", historyRef(project, environment, revision), err)
	}
	return cm, nil
}

// retain enforces keep-last-N by deleting the revisions past the keep window.
func (h *HistoryStore) retain(project, environment string) error {
	cms, err := h.list(project, environment)
	if err != nil {
		return err
	}
	if len(cms) <= h.keep {
		return nil
	}
	for i := h.keep; i < len(cms); i++ {
		err := h.client.CoreV1().ConfigMaps(h.namespace).Delete(h.ctx, cms[i].Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("serverstate: prune revision %s: %w", cms[i].Name, err)
		}
	}
	return nil
}

// configMap builds the object one Append writes.
func (h *HistoryStore) configMap(project, environment string, rec direct.Record, blob []byte) (*corev1.ConfigMap, error) {
	record, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("serverstate: encode history record: %w", err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      historyName(project, environment, rec.Revision),
			Namespace: h.namespace,
			Labels: map[string]string{
				labelManagedBy:   managedByKelson,
				labelState:       stateHistory,
				labelProject:     project,
				labelEnvironment: environment,
				labelRevision:    rec.Revision,
			},
		},
		Data:       map[string]string{recordKey: string(record)},
		BinaryData: map[string][]byte{renderedKey: blob},
	}, nil
}

func (h *HistoryStore) validScope(project, environment string) error {
	if err := validSegment("project", project); err != nil {
		return err
	}
	return validSegment("environment", environment)
}

// --- encoding ---------------------------------------------------------------

func decodeRecord(cm *corev1.ConfigMap) (direct.Record, error) {
	var rec direct.Record
	raw, ok := cm.Data[recordKey]
	if !ok {
		return direct.Record{}, fmt.Errorf("serverstate: history ConfigMap %s/%s has no %q key", cm.Namespace, cm.Name, recordKey)
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return direct.Record{}, fmt.Errorf("serverstate: corrupt history record in %s/%s: %w", cm.Namespace, cm.Name, err)
	}
	return rec, nil
}

func compress(rendered []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("serverstate: compress rendered output: %w", err)
	}
	if _, err := w.Write(rendered); err != nil {
		return nil, fmt.Errorf("serverstate: compress rendered output: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("serverstate: compress rendered output: %w", err)
	}
	return buf.Bytes(), nil
}

func decompress(ref string, blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("serverstate: revision %s recorded no rendered output", ref)
	}
	r, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("serverstate: read rendered output for %s: %w", ref, err)
	}
	defer r.Close() //nolint:errcheck // read-only handle
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("serverstate: read rendered output for %s: %w", ref, err)
	}
	return out, nil
}

// --- names ------------------------------------------------------------------

// historyName is the per-revision object name.
//
// Object names are capped at 253 characters. The bound is structural rather
// than checked here: validSegment requires project and environment to be
// DNS-1123 labels (63 characters each) and validRevision caps the revision, so
// the longest name this can produce is 13 + 63 + 1 + 63 + 1 + 63 = 204. No
// hashing is needed, and none is used — a name that reads as
// project/environment/revision is the point of storing history where kubectl
// can see it.
func historyName(project, environment, revision string) string {
	return historyNamePrefix + project + "-" + environment + "-" + revision
}

// historyRef is the resource identity carried on errors.
func historyRef(project, environment, revision string) string {
	return "history/" + project + "/" + environment + "@" + revision
}

// validRevision keeps a revision id safe as both a name component and a label
// value. Nothing here assumes the "rev-NNNNNNNN" sequence — a Git-mode sha is
// a DNS-1123 label too — only that the id survives being spliced into both.
func validRevision(revision string) error {
	return validSegment("revision", revision)
}

func formatRevision(n int) string {
	return fmt.Sprintf("%s%0*d", revisionPrefix, revisionWidth, n)
}

func parseRevision(rev string) (int, bool) {
	if !strings.HasPrefix(rev, revisionPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(rev, revisionPrefix))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
