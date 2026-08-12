package direct

// Rendered-history store for direct mode (issue #38).
//
// # Storage choice: an append-only JSONL journal, not an embedded git repo
//
// Two designs were considered for "direct mode keeps diffs, history and
// rollback" (docs/architecture.md, Delivery adapters):
//
//  1. An embedded bare git repository under the data dir. Every deploy is a
//     commit of the rendered tree. It gives `kelson eject --to-git` an almost
//     free implementation (push the repo), and diffs come from git itself.
//     The cost is a hard dependency on a git implementation (go-git or the
//     git binary), a second on-disk format whose failure modes — index locks,
//     corrupt packfiles, ref races — are far richer than the History() and
//     Rollback() semantics that actually need them, and tests that either
//     shell out or carry a git library just to assert "the last three entries
//     are these".
//
//  2. An append-only JSONL journal plus one rendered blob per revision. One
//     line per deploy, one file per revision's rendered output, retention by
//     count. Reading history is a bounded file read; rollback is "read blob N
//     and re-apply it"; eject stays possible because the rendered output is
//     kept verbatim, in apply order, with the same provenance the Git modes
//     commit — replaying it into commits is a loop over the journal.
//
// This package implements (2). The deciding argument is that the journal is
// deterministic and testable against a temp dir with no external binary, and
// that the eject path only needs the rendered bytes plus their order, which
// the journal already keeps. Nothing in the on-disk layout forecloses (1): a
// future ejector reads the journal and writes commits.
//
// Durability model: the rendered blob is written before the journal line that
// references it, so a crash can leave an orphan blob (harmless, pruned by
// retention) but never a journal entry pointing at missing output. A Store is
// safe for concurrent use within one process; the data dir is assumed to be
// owned by a single kelson-server, matching the direct-mode deployment shape.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dafrie/kelson/internal/delivery"
)

// DefaultKeep is the retention default: the newest N revisions are kept and
// older journal lines and their rendered blobs are pruned.
const DefaultKeep = 20

const (
	journalFile   = "journal.jsonl"
	renderedDir   = "rendered"
	renderedExt   = ".yaml"
	revisionPre   = "rev-"
	revisionWidth = 8
)

// RecordType distinguishes a deploy from a rollback so history reads like an
// audit trail rather than a list of indistinguishable revisions.
type RecordType string

const (
	TypeDeploy   RecordType = "deploy"
	TypeRollback RecordType = "rollback"
)

// ResourceRef is the identity of one applied resource. The journal keeps these
// alongside the rendered blob so pruning knows what the previous revision owned
// without re-parsing YAML.
type ResourceRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

func (r ResourceRef) key() string {
	return r.APIVersion + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

func (r ResourceRef) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Record is one journal line: a delivery.Entry plus the direct-mode detail the
// adapter needs to prune and to replay.
type Record struct {
	Revision     string        `json:"revision"`
	SpecHash     string        `json:"specHash"`
	CommittedAt  string        `json:"committedAt"`
	Type         RecordType    `json:"type"`
	Message      string        `json:"message,omitempty"`
	Author       string        `json:"author,omitempty"`
	RolledBackTo string        `json:"rolledBackTo,omitempty"`
	Resources    []ResourceRef `json:"resources"`
}

// Entry projects a Record onto the mode-independent history shape callers see
// (delivery.Entry), so direct and Git modes are indistinguishable from the
// CLI/UI/API side.
func (r Record) Entry() delivery.Entry {
	return delivery.Entry{
		Revision:    r.Revision,
		SpecHash:    r.SpecHash,
		CommittedAt: r.CommittedAt,
		Message:     r.Message,
		Author:      r.Author,
	}
}

// StoreOptions configures a Store.
type StoreOptions struct {
	// Dir is the data dir the journal lives under. Required.
	Dir string
	// Keep is the retention count; <= 0 selects DefaultKeep.
	Keep int
	// Now is injectable so tests get deterministic timestamps.
	Now func() time.Time
}

// Store is the on-disk rendered-history for direct mode. Layout:
//
//	<dir>/<project>/<environment>/journal.jsonl
//	<dir>/<project>/<environment>/rendered/<revision>.yaml
type Store struct {
	dir  string
	keep int
	now  func() time.Time
	mu   sync.Mutex
}

// OpenStore prepares the data dir and returns a Store.
func OpenStore(opts StoreOptions) (*Store, error) {
	if opts.Dir == "" {
		return nil, errors.New("direct: history store requires a data dir")
	}
	keep := opts.Keep
	if keep <= 0 {
		keep = DefaultKeep
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("direct: create history dir: %w", err)
	}
	return &Store{dir: opts.Dir, keep: keep, now: now}, nil
}

// Append records one revision: the rendered output first, then the journal
// line that makes it visible to History().
func (s *Store) Append(project, environment string, rec Record, rendered []byte) (delivery.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validSegment("revision", rec.Revision); err != nil {
		return delivery.Entry{}, err
	}
	dir, err := s.scope(project, environment)
	if err != nil {
		return delivery.Entry{}, err
	}
	if rec.Type == "" {
		rec.Type = TypeDeploy
	}
	if rec.CommittedAt == "" {
		rec.CommittedAt = s.now().UTC().Format(time.RFC3339)
	}
	if rec.Resources == nil {
		rec.Resources = []ResourceRef{}
	}

	if err := os.MkdirAll(filepath.Join(dir, renderedDir), 0o750); err != nil {
		return delivery.Entry{}, fmt.Errorf("direct: create rendered dir: %w", err)
	}
	if err := os.WriteFile(renderedPath(dir, rec.Revision), rendered, 0o600); err != nil {
		return delivery.Entry{}, fmt.Errorf("direct: write rendered output: %w", err)
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return delivery.Entry{}, fmt.Errorf("direct: encode history record: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, journalFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return delivery.Entry{}, fmt.Errorf("direct: open history journal: %w", err)
	}
	_, writeErr := f.Write(append(line, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return delivery.Entry{}, fmt.Errorf("direct: append history record: %w", writeErr)
	}
	if closeErr != nil {
		return delivery.Entry{}, fmt.Errorf("direct: close history journal: %w", closeErr)
	}

	if err := s.retain(dir); err != nil {
		return delivery.Entry{}, err
	}
	return rec.Entry(), nil
}

// List returns the recorded revisions newest first.
func (s *Store) List(project, environment string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.scope(project, environment)
	if err != nil {
		return nil, err
	}
	recs, err := readJournal(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		out = append(out, recs[i])
	}
	return out, nil
}

// Entries is List projected onto the adapter-facing history shape.
func (s *Store) Entries(project, environment string) ([]delivery.Entry, error) {
	recs, err := s.List(project, environment)
	if err != nil {
		return nil, err
	}
	out := make([]delivery.Entry, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Entry())
	}
	return out, nil
}

// Latest returns the newest record, or nil when nothing has been recorded.
func (s *Store) Latest(project, environment string) (*Record, error) {
	recs, err := s.List(project, environment)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	newest := recs[0]
	return &newest, nil
}

// Get returns the record for one revision, or nil when it is not retained.
func (s *Store) Get(project, environment, revision string) (*Record, error) {
	recs, err := s.List(project, environment)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].Revision == revision {
			return &recs[i], nil
		}
	}
	return nil, nil
}

// Rendered returns the rendered output recorded for one revision, verbatim and
// in apply order — the bytes that were applied, not a re-render.
func (s *Store) Rendered(project, environment, revision string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.scope(project, environment)
	if err != nil {
		return nil, err
	}
	if err := validSegment("revision", revision); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(renderedPath(dir, revision))
	if err != nil {
		return nil, fmt.Errorf("direct: read rendered output for %s: %w", revision, err)
	}
	return b, nil
}

// NextRevision allocates the next direct-mode revision id. Revisions are a
// zero-padded sequence ("rev-00000007") so they sort lexicographically and read
// as an obvious counter next to the Git modes' shas.
func (s *Store) NextRevision(project, environment string) (string, error) {
	recs, err := s.List(project, environment)
	if err != nil {
		return "", err
	}
	highest := 0
	for _, r := range recs {
		if n, ok := parseRevision(r.Revision); ok && n > highest {
			highest = n
		}
	}
	return fmt.Sprintf("%s%0*d", revisionPre, revisionWidth, highest+1), nil
}

// retain enforces the keep-last-N policy: the journal is rewritten atomically
// with the newest keep records and the dropped revisions' blobs are removed.
func (s *Store) retain(dir string) error {
	recs, err := readJournal(dir)
	if err != nil {
		return err
	}
	if len(recs) <= s.keep {
		return nil
	}
	dropped := recs[:len(recs)-s.keep]
	kept := recs[len(recs)-s.keep:]

	var buf strings.Builder
	for _, r := range kept {
		line, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("direct: re-encode history record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := filepath.Join(dir, journalFile+".tmp")
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o600); err != nil {
		return fmt.Errorf("direct: write pruned journal: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, journalFile)); err != nil {
		return fmt.Errorf("direct: replace journal: %w", err)
	}
	for _, r := range dropped {
		if err := os.Remove(renderedPath(dir, r.Revision)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("direct: prune rendered output for %s: %w", r.Revision, err)
		}
	}
	return nil
}

// scope resolves and creates the per-(project, environment) directory.
func (s *Store) scope(project, environment string) (string, error) {
	if err := validSegment("project", project); err != nil {
		return "", err
	}
	if err := validSegment("environment", environment); err != nil {
		return "", err
	}
	dir := filepath.Join(s.dir, project, environment)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("direct: create history dir: %w", err)
	}
	return dir, nil
}

func renderedPath(dir, revision string) string {
	return filepath.Join(dir, renderedDir, revision+renderedExt)
}

// readJournal returns the records oldest first. A missing journal is an empty
// history, not an error.
func readJournal(dir string) ([]Record, error) {
	f, err := os.Open(filepath.Join(dir, journalFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("direct: open history journal: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("direct: corrupt history record: %w", err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("direct: read history journal: %w", err)
	}
	return out, nil
}

func parseRevision(rev string) (int, bool) {
	if !strings.HasPrefix(rev, revisionPre) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(rev, revisionPre))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// validSegment keeps caller-supplied identifiers from escaping the data dir.
func validSegment(what, v string) error {
	if v == "" {
		return fmt.Errorf("direct: history %s must not be empty", what)
	}
	if v == "." || v == ".." || strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") {
		return fmt.Errorf("direct: history %s %q must be a single path-safe segment", what, v)
	}
	return nil
}
