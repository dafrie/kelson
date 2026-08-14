package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/sops"
)

// testRecipient is a real age *public* key generated for these tests. Its
// identity was discarded: nothing here decrypts, because nothing in kelson
// decrypts. internal/sops owns the round-trip proof.
const testRecipient = "age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw"

const (
	testDBURL  = "postgres://checkout:hunter2@db.internal:5432/checkout"
	testAPIKey = "sk_live_51NotARealStripeKey"
)

func sopsTarget() Target {
	return Target{Project: "checkout", Environment: "production"}
}

// sopsStore builds a store over a bare repository standing in for a delivery
// repo. Nothing here reaches the network: go-git clones a local path.
func sopsStore(t *testing.T, recipients ...string) (*SOPSStore, string) {
	t.Helper()
	remote := t.TempDir()
	if _, err := gogit.PlainInit(remote, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}
	if len(recipients) == 0 {
		recipients = []string{testRecipient}
	}
	store, err := NewSOPS(SOPSConfig{
		Writer:     newSOPSWriterTo(t, remote),
		Recipients: recipients,
		Now:        func() time.Time { return time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewSOPS: %v", err)
	}
	return store, remote
}

// newSOPSWriter is a writer over a fresh bare repository.
func newSOPSWriter(t *testing.T) *git.Writer {
	t.Helper()
	remote := t.TempDir()
	if _, err := gogit.PlainInit(remote, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}
	return newSOPSWriterTo(t, remote)
}

// newSOPSWriterTo is a writer over a named remote, which may deliberately not
// be a repository at all.
func newSOPSWriterTo(t *testing.T, remote string) *git.Writer {
	t.Helper()
	w, err := git.New(git.Config{
		Target:   git.Target{Repo: remote, Branch: "main", Path: "clusters/prod"},
		Mode:     git.ModeCommit,
		Identity: git.Identity{Name: "Test", Email: "test@example.test"},
		Now:      func() time.Time { return time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("git writer: %v", err)
	}
	return w
}

// remoteFile returns one committed file's bytes, or nil when it is absent.
func remoteFile(t *testing.T, remote, path string) []byte {
	t.Helper()
	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		return nil
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	f, err := commit.File(path)
	if err != nil {
		return nil
	}
	body, err := f.Contents()
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	return []byte(body)
}

func remotePaths(t *testing.T, remote string) []string {
	t.Helper()
	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		return nil
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	var out []string
	if err := tree.Files().ForEach(func(f *object.File) error {
		out = append(out, f.Name)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// TestSOPSSetCommitsCiphertext is issue #81's acceptance criterion on this
// side of the seam: what reaches the repository is encrypted, and no spelling
// of the plaintext is anywhere in the bytes that were committed.
func TestSOPSSetCommitsCiphertext(t *testing.T) {
	store, remote := sopsStore(t)
	written, err := store.Set(context.Background(), SetRequest{
		Target: sopsTarget(),
		Name:   "checkout-db",
		Values: map[string]string{"url": testDBURL, "token": testAPIKey},
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if written.Namespace != "checkout-production" || strings.Join(written.Keys, ",") != "token,url" {
		t.Errorf("read-back = %+v", written)
	}

	body := remoteFile(t, remote, "clusters/prod/secrets/checkout-db.enc.yaml")
	if body == nil {
		t.Fatalf("nothing was committed; the tree holds %v", remotePaths(t, remote))
	}
	for _, plaintext := range []string{testDBURL, testAPIKey} {
		if bytes.Contains(body, []byte(plaintext)) {
			t.Errorf("the committed file contains a plaintext value")
		}
		if bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString([]byte(plaintext)))) {
			t.Errorf("the committed file contains a base64-encoded value")
		}
	}

	f, err := sops.Inspect(body)
	if err != nil {
		t.Fatalf("the committed file is not a SOPS document: %v", err)
	}
	if f.Namespace != "checkout-production" {
		t.Errorf("namespace = %q; a Secret in the wrong namespace is one nothing reads", f.Namespace)
	}
	if !f.EncryptedTo([]string{testRecipient}) {
		t.Errorf("recipients = %v", f.Recipients)
	}
}

// TestSOPSSetWritesNothingElse: a secret write is additive. The manifests a
// deploy put in the same directory are not this command's to touch, and the
// git plane's test asserts the prune side of the same property.
func TestSOPSSetWritesNothingElse(t *testing.T) {
	store, remote := sopsStore(t)
	if _, err := store.Set(context.Background(), SetRequest{
		Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": testDBURL},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	paths := remotePaths(t, remote)
	if len(paths) != 1 || paths[0] != "clusters/prod/secrets/checkout-db.enc.yaml" {
		t.Errorf("a set wrote %v", paths)
	}
}

// TestSOPSSetRefusesToDropKeys is the one behaviour this backend does not
// share with `cluster`, and the reason is the guarantee: kelson cannot carry
// the other keys forward without the identity it never holds, so it says so
// instead of quietly writing a Secret with fewer keys than it had.
func TestSOPSSetRefusesToDropKeys(t *testing.T) {
	store, _ := sopsStore(t)
	ctx := context.Background()
	if _, err := store.Set(ctx, SetRequest{
		Target: sopsTarget(), Name: "checkout-db",
		Values: map[string]string{"url": testDBURL, "token": testAPIKey},
	}); err != nil {
		t.Fatalf("first Set: %v", err)
	}

	_, err := store.Set(ctx, SetRequest{
		Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": "postgres://new"},
	})
	if !AsCode(err, ErrSOPSPartialSet) {
		t.Fatalf("want %s, got %v", ErrSOPSPartialSet, err)
	}
	if !strings.Contains(err.(Error).Message, `"token"`) {
		t.Errorf("the refusal must name the key that would be lost, got %q", err.(Error).Message)
	}

	// Naming every key is the ordinary case and simply works, as is adding one.
	if _, err := store.Set(ctx, SetRequest{
		Target: sopsTarget(), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://new", "token": testAPIKey, "extra": "x"},
	}); err != nil {
		t.Fatalf("a superset write must succeed: %v", err)
	}
}

func TestSOPSListAndDelete(t *testing.T) {
	store, remote := sopsStore(t)
	ctx := context.Background()
	for _, name := range []string{"payments", "checkout-db"} {
		if _, err := store.Set(ctx, SetRequest{
			Target: sopsTarget(), Name: name, Values: map[string]string{"url": testDBURL},
		}); err != nil {
			t.Fatalf("Set %s: %v", name, err)
		}
	}

	list, err := store.List(ctx, sopsTarget())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "checkout-db" || list[1].Name != "payments" {
		t.Fatalf("List = %+v", list)
	}
	if strings.Join(list[0].Keys, ",") != "url" {
		t.Errorf("keys = %v", list[0].Keys)
	}

	if err := store.Delete(ctx, DeleteRequest{Target: sopsTarget(), Name: "payments"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if remoteFile(t, remote, "clusters/prod/secrets/payments.enc.yaml") != nil {
		t.Errorf("the deleted Secret is still committed")
	}
	if remoteFile(t, remote, "clusters/prod/secrets/checkout-db.enc.yaml") == nil {
		t.Errorf("the other Secret was deleted too")
	}

	// A delete of something that is not there names the path rather than
	// succeeding silently: under this backend "it is gone" and "it was never
	// committed" are different answers to `did my rotation land`.
	err = store.Delete(ctx, DeleteRequest{Target: sopsTarget(), Name: "payments"})
	if !AsCode(err, ErrNotFound) {
		t.Fatalf("want %s, got %v", ErrNotFound, err)
	}
}

// TestSOPSDryRunWritesNothing: the dry run still encrypts, so it answers the
// question it was asked rather than a cheaper one, and commits nothing.
func TestSOPSDryRunWritesNothing(t *testing.T) {
	store, remote := sopsStore(t)
	if _, err := store.Set(context.Background(), SetRequest{
		Target: sopsTarget(), Name: "checkout-db",
		Values: map[string]string{"url": testDBURL}, DryRun: true,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if paths := remotePaths(t, remote); len(paths) != 0 {
		t.Errorf("a dry run committed %v", paths)
	}
}

// TestSOPSDriftReportsRecipientChanges is what `kelson secret rotate` runs. It
// needs no key material at all: SOPS stores every recipient in the clear
// beside the data key it wrapped.
func TestSOPSDriftReportsRecipientChanges(t *testing.T) {
	const second = "age1cnnsmzvurnpxzzggcwm4rvk44hgskzhrz92nryk6879kf8zll3jqqjhvt3"
	store, _ := sopsStore(t)
	ctx := context.Background()
	for _, name := range []string{"checkout-db", "payments"} {
		if _, err := store.Set(ctx, SetRequest{
			Target: sopsTarget(), Name: name, Values: map[string]string{"url": testDBURL},
		}); err != nil {
			t.Fatalf("Set %s: %v", name, err)
		}
	}

	drift, err := store.Drift(ctx, sopsTarget())
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(drift) != 0 {
		t.Fatalf("freshly written files must not be stale, got %+v", drift)
	}

	// The spec gains a recipient: every existing file is now unreadable by
	// whoever holds the new identity, and that is exactly what rotation has to
	// surface before the old key is destroyed.
	rotated, err := NewSOPS(SOPSConfig{
		Writer:     storeWriter(store),
		Recipients: []string{testRecipient, second},
	})
	if err != nil {
		t.Fatalf("NewSOPS: %v", err)
	}
	drift, err = rotated.Drift(ctx, sopsTarget())
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(drift) != 2 {
		t.Fatalf("want both files stale, got %+v", drift)
	}
	d := drift[0]
	if d.Name != "checkout-db" || d.Path != "clusters/prod/secrets/checkout-db.enc.yaml" {
		t.Errorf("drift entry = %+v", d)
	}
	if len(d.Missing) != 1 || d.Missing[0] != second {
		t.Errorf("Missing = %v, want the recipient the file cannot be read by", d.Missing)
	}
	if len(d.Extra) != 0 {
		t.Errorf("Extra = %v", d.Extra)
	}
	if strings.Join(d.Keys, ",") != "url" {
		t.Errorf("Keys = %v; the report must be enough to reconstruct the set command", d.Keys)
	}

	// Re-encrypting to the new set clears the drift — the rotation actually
	// completing, in one command per Secret.
	if _, err := rotated.Set(ctx, SetRequest{
		Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": testDBURL},
	}); err != nil {
		t.Fatalf("re-encrypt: %v", err)
	}
	drift, err = rotated.Drift(ctx, sopsTarget())
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(drift) != 1 || drift[0].Name != "payments" {
		t.Fatalf("only payments should still be stale, got %+v", drift)
	}
}

// TestSOPSRefusesAPlaintextFile: a hand-committed Secret where an encrypted
// one belongs is the failure this backend exists to prevent. kelson reports it
// rather than overwriting it, because the remediation starts with treating the
// contents as compromised.
func TestSOPSRefusesAPlaintextFile(t *testing.T) {
	store, _ := sopsStore(t)
	session, err := storeWriter(store).Open(context.Background(), git.Message{Subject: "by hand"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := session.Put([]git.File{{
		Path: git.SecretPath("checkout-db"),
		Data: []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: checkout-db\nstringData:\n  url: " + testDBURL + "\n"),
	}}, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := session.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err = store.Set(context.Background(), SetRequest{
		Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": testDBURL},
	})
	if !AsCode(err, ErrSOPSNotEncrypted) {
		t.Fatalf("want %s, got %v", ErrSOPSNotEncrypted, err)
	}
	if err := store.Delete(context.Background(), DeleteRequest{Target: sopsTarget(), Name: "checkout-db"}); !AsCode(err, ErrSOPSNotEncrypted) {
		t.Fatalf("delete must refuse it too, got %v", err)
	}
}

func TestNewSOPSRefusesAnIncompleteConfig(t *testing.T) {
	if _, err := NewSOPS(SOPSConfig{Recipients: []string{testRecipient}}); !AsCode(err, ErrSOPSNoTarget) {
		t.Errorf("want %s, got %v", ErrSOPSNoTarget, err)
	}
	w, err := git.New(git.Config{Target: git.Target{Repo: t.TempDir()}})
	if err != nil {
		t.Fatalf("git writer: %v", err)
	}
	if _, err := NewSOPS(SOPSConfig{Writer: w}); !AsCode(err, ErrSOPSNoRecipients) {
		t.Errorf("want %s, got %v", ErrSOPSNoRecipients, err)
	}
}

// TestSOPSSetValidatesLikeTheClusterBackend: the shared request type means the
// two backends refuse the same names and keys, so a Secret kelson would let
// you write in one is one you can write in the other.
func TestSOPSSetValidatesLikeTheClusterBackend(t *testing.T) {
	store, _ := sopsStore(t)
	cases := []struct {
		name string
		req  SetRequest
		want Code
	}{
		{"no name", SetRequest{Target: sopsTarget(), Values: map[string]string{"k": "v"}}, ErrInvalidName},
		{"bad name", SetRequest{Target: sopsTarget(), Name: "Not A Label", Values: map[string]string{"k": "v"}}, ErrInvalidName},
		{"no values", SetRequest{Target: sopsTarget(), Name: "checkout-db"}, ErrNoValues},
		{"bad key", SetRequest{Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"a key": "v"}}, ErrInvalidKey},
		{"no target", SetRequest{Name: "checkout-db", Values: map[string]string{"k": "v"}}, ErrInvalidTarget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Set(context.Background(), tc.req); !AsCode(err, tc.want) {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
		})
	}
}

// storeWriter reaches the store's git writer, so a test can build a second
// store against the same repository or commit something behind kelson's back.
func storeWriter(s *SOPSStore) *git.Writer { return s.cfg.Writer }
