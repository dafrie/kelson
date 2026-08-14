package direct

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T, keep int) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	s, err := OpenStore(StoreOptions{Dir: dir, Keep: keep, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s, dir
}

func TestStoreRoundTrip(t *testing.T) {
	s, _ := testStore(t, 5)
	rendered := []byte("---\napiVersion: v1\nkind: ConfigMap\n")

	entry, err := s.Append(testProject, testEnv, Record{
		Revision: "rev-00000001",
		SpecHash: "sha256:one",
		Message:  "deploy",
		Author:   "agent",
		Resources: []ResourceRef{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "flags", Namespace: testNS},
		},
	}, rendered)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if entry.Revision != "rev-00000001" || entry.SpecHash != "sha256:one" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.CommittedAt != "2026-08-12T09:00:00Z" {
		t.Fatalf("committedAt = %q", entry.CommittedAt)
	}

	entries, err := s.Entries(testProject, testEnv)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v (err %v)", entries, err)
	}
	if entries[0].Author != "agent" || entries[0].Message != "deploy" {
		t.Fatalf("entry = %+v", entries[0])
	}

	got, err := s.Rendered(testProject, testEnv, "rev-00000001")
	if err != nil {
		t.Fatalf("rendered: %v", err)
	}
	if string(got) != string(rendered) {
		t.Fatalf("rendered output must be stored verbatim, got %q", got)
	}

	rec, err := s.Get(testProject, testEnv, "rev-00000001")
	if err != nil || rec == nil {
		t.Fatalf("get = %+v (err %v)", rec, err)
	}
	if rec.Type != TypeDeploy || len(rec.Resources) != 1 || rec.Resources[0].Name != "flags" {
		t.Fatalf("record = %+v", rec)
	}

	missing, err := s.Get(testProject, testEnv, "rev-00000099")
	if err != nil || missing != nil {
		t.Fatalf("unknown revision = %+v (err %v), want nil", missing, err)
	}
}

func TestStoreEmptyHistory(t *testing.T) {
	s, _ := testStore(t, 5)
	entries, err := s.Entries(testProject, testEnv)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries = %+v (err %v)", entries, err)
	}
	latest, err := s.Latest(testProject, testEnv)
	if err != nil || latest != nil {
		t.Fatalf("latest = %+v (err %v)", latest, err)
	}
	next, err := s.NextRevision(testProject, testEnv)
	if err != nil || next != "rev-00000001" {
		t.Fatalf("next revision = %q (err %v)", next, err)
	}
}

// TestStoreNewestFirstAndSequence: History() is newest first (the shape Git
// modes expose) and revisions are a monotonic sequence.
func TestStoreNewestFirstAndSequence(t *testing.T) {
	s, _ := testStore(t, 10)
	for i := 0; i < 3; i++ {
		rev, err := s.NextRevision(testProject, testEnv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(testProject, testEnv, Record{Revision: rev}, []byte(rev)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.Entries(testProject, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rev-00000003", "rev-00000002", "rev-00000001"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v", entries)
	}
	for i := range want {
		if entries[i].Revision != want[i] {
			t.Fatalf("entries = %+v, want newest first %v", entries, want)
		}
	}
	latest, err := s.Latest(testProject, testEnv)
	if err != nil || latest == nil || latest.Revision != "rev-00000003" {
		t.Fatalf("latest = %+v (err %v)", latest, err)
	}
}

// TestStoreRetention: keep-last-N prunes journal lines and their rendered
// blobs, and the revision sequence keeps counting past what was pruned.
func TestStoreRetention(t *testing.T) {
	s, dir := testStore(t, 3)
	for i := 0; i < 5; i++ {
		rev, err := s.NextRevision(testProject, testEnv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(testProject, testEnv, Record{Revision: rev}, []byte(rev)); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := s.Entries(testProject, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Revision != "rev-00000005" || entries[2].Revision != "rev-00000003" {
		t.Fatalf("retention kept %+v, want the newest 3", entries)
	}

	renderedIn := filepath.Join(dir, testProject, testEnv, renderedDir)
	files, err := os.ReadDir(renderedIn)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Fatalf("rendered blobs = %v, want the newest 3", names)
	}
	if _, err := s.Rendered(testProject, testEnv, "rev-00000001"); err == nil {
		t.Fatal("a pruned revision's rendered output must be gone")
	}

	next, err := s.NextRevision(testProject, testEnv)
	if err != nil || next != "rev-00000006" {
		t.Fatalf("next revision after pruning = %q (err %v)", next, err)
	}
}

// TestStoreScopedPerEnvironment: two environments of one project keep
// independent histories and independent revision sequences.
func TestStoreScopedPerEnvironment(t *testing.T) {
	s, _ := testStore(t, 5)
	if _, err := s.Append(testProject, "development", Record{Revision: "rev-00000001"}, []byte("dev")); err != nil {
		t.Fatal(err)
	}
	next, err := s.NextRevision(testProject, testEnv)
	if err != nil || next != "rev-00000001" {
		t.Fatalf("production sequence = %q, want its own", next)
	}
	entries, err := s.Entries(testProject, testEnv)
	if err != nil || len(entries) != 0 {
		t.Fatalf("production history = %+v, want empty", entries)
	}
}

// TestStoreForget is `kelson uninstall`'s half of the store (issue #59): an
// environment whose resources are gone must not leave a journal describing them
// behind, because the next deploy of the same name would inherit its revision
// numbers and its prune baseline from a deployment that no longer exists.
func TestStoreForget(t *testing.T) {
	s, dir := testStore(t, 5)
	for _, rev := range []string{"rev-00000001", "rev-00000002"} {
		if _, err := s.Append(testProject, testEnv, Record{Revision: rev}, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(testProject, "development", Record{Revision: "rev-00000001"}, []byte("x")); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.Forget(testProject, testEnv)
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("forget reported %d revisions, want 2 — the count is what the uninstall prints", dropped)
	}
	if _, err := os.Stat(filepath.Join(dir, testProject, testEnv)); !os.IsNotExist(err) {
		t.Fatalf("the environment's history directory survived: %v", err)
	}

	// The project's other environments are untouched: uninstalling one
	// environment is not uninstalling the project.
	entries, err := s.Entries(testProject, "development")
	if err != nil || len(entries) != 1 {
		t.Fatalf("development history = %+v (%v), want its own single entry", entries, err)
	}

	// Forgetting what was never recorded is the outcome the caller asked for.
	again, err := s.Forget(testProject, testEnv)
	if err != nil || again != 0 {
		t.Fatalf("forget of an already-forgotten environment = (%d, %v), want (0, nil)", again, err)
	}
}

func TestStoreForgetProject(t *testing.T) {
	s, dir := testStore(t, 5)
	for _, env := range []string{testEnv, "development", "staging"} {
		if _, err := s.Append(testProject, env, Record{Revision: "rev-00000001"}, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append("other", testEnv, Record{Revision: "rev-00000001"}, []byte("x")); err != nil {
		t.Fatal(err)
	}

	envs, err := s.Environments(testProject)
	if err != nil {
		t.Fatalf("environments: %v", err)
	}
	if strings.Join(envs, ",") != "development,production,staging" {
		t.Fatalf("environments = %v, want every recorded environment, sorted", envs)
	}

	dropped, err := s.ForgetProject(testProject)
	if err != nil {
		t.Fatalf("forget project: %v", err)
	}
	if dropped != 3 {
		t.Fatalf("forget project reported %d revisions, want 3", dropped)
	}
	if _, err := os.Stat(filepath.Join(dir, testProject)); !os.IsNotExist(err) {
		t.Fatalf("the project's history directory survived: %v", err)
	}
	entries, err := s.Entries("other", testEnv)
	if err != nil || len(entries) != 1 {
		t.Fatalf("another project's history = %+v (%v), want untouched", entries, err)
	}
}

func TestStoreForgetRejectsUnsafeSegments(t *testing.T) {
	s, _ := testStore(t, 5)
	for _, tc := range []struct{ project, env string }{
		{"../escape", testEnv},
		{testProject, ".."},
		{"", testEnv},
	} {
		if _, err := s.Forget(tc.project, tc.env); err == nil {
			t.Errorf("forget(%q, %q) must be refused: it would delete outside the data dir", tc.project, tc.env)
		}
	}
	if _, err := s.ForgetProject("../escape"); err == nil {
		t.Error("forgetProject must refuse a path-escaping project name")
	}
	if _, err := s.Environments(".."); err == nil {
		t.Error("environments must refuse a path-escaping project name")
	}
}

func TestStoreRejectsUnsafeSegments(t *testing.T) {
	s, dir := testStore(t, 5)
	for _, tc := range []struct{ project, env, revision string }{
		{"../escape", testEnv, "rev-00000001"},
		{testProject, "..", "rev-00000001"},
		{testProject, testEnv, "../../etc/passwd"},
		{"", testEnv, "rev-00000001"},
	} {
		if _, err := s.Append(tc.project, tc.env, Record{Revision: tc.revision}, []byte("x")); err == nil {
			t.Fatalf("append(%q, %q, %q) must be refused", tc.project, tc.env, tc.revision)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("nothing must be written outside a valid scope, found %v", entries)
	}
}

func TestStoreRejectsCorruptJournal(t *testing.T) {
	s, dir := testStore(t, 5)
	if _, err := s.Append(testProject, testEnv, Record{Revision: "rev-00000001"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, testProject, testEnv, journalFile)
	if err := os.WriteFile(journal, []byte("{not json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Entries(testProject, testEnv)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt history must be reported, got %v", err)
	}
}

func TestOpenStoreRequiresDir(t *testing.T) {
	if _, err := OpenStore(StoreOptions{}); err == nil {
		t.Fatal("a data dir is required")
	}
	s, err := OpenStore(StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s.keep != DefaultKeep {
		t.Fatalf("keep = %d, want the default %d", s.keep, DefaultKeep)
	}
}
