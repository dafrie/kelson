package gitref

import (
	"context"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	mainHash = "1111111111111111111111111111111111111111"
	tagHash  = "2222222222222222222222222222222222222222"
	tagObj   = "3333333333333333333333333333333333333333"
)

// lsRemote is an ls-remote listing in the shape go-git returns: a symbolic
// HEAD, branch heads, and — for an annotated tag — both the tag object and the
// peeled commit it points at.
func lsRemote() []*plumbing.Reference {
	return []*plumbing.Reference{
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(mainHash)),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("release"), plumbing.NewHash(tagHash)),
		plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0.0"), plumbing.NewHash(tagObj)),
		plumbing.NewHashReference(plumbing.ReferenceName("refs/tags/v1.0.0^{}"), plumbing.NewHash(tagHash)),
	}
}

func TestResolveRefPrecedence(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{name: "empty ref follows HEAD to the default branch", ref: "", want: mainHash},
		{name: "explicit HEAD", ref: "HEAD", want: mainHash},
		{name: "branch by short name", ref: "release", want: tagHash},
		{
			// An annotated tag's own hash is a tag object, not a tree a build
			// can check out; the peeled entry is the commit.
			name: "annotated tag resolves to the peeled commit",
			ref:  "v1.0.0",
			want: tagHash,
		},
		{name: "fully qualified branch ref", ref: "refs/heads/main", want: mainHash},
		{name: "fully qualified tag ref peels", ref: "refs/tags/v1.0.0", want: tagHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveRef(lsRemote(), tc.ref)
			if !ok {
				t.Fatalf("resolveRef(%q) did not resolve", tc.ref)
			}
			if got != tc.want {
				t.Fatalf("resolveRef(%q) = %s, want %s", tc.ref, got, tc.want)
			}
		})
	}
}

// A branch wins over a tag of the same name, which is the precedence git
// itself uses; silently preferring the tag would build the wrong commit.
func TestResolveRefPrefersABranchOverATag(t *testing.T) {
	refs := []*plumbing.Reference{
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("stable"), plumbing.NewHash(mainHash)),
		plumbing.NewHashReference(plumbing.NewTagReferenceName("stable"), plumbing.NewHash(tagHash)),
	}
	got, ok := resolveRef(refs, "stable")
	if !ok || got != mainHash {
		t.Fatalf("resolveRef(stable) = %q (ok %v), want the branch head %s", got, ok, mainHash)
	}
}

func TestResolveRefReportsAMissingRef(t *testing.T) {
	for _, ref := range []string{"nope", "refs/heads/nope", "v9.9.9"} {
		if got, ok := resolveRef(lsRemote(), ref); ok {
			t.Fatalf("resolveRef(%q) = %q, want no match", ref, got)
		}
	}
	// A listing with no HEAD at all must not resolve the empty ref to
	// something arbitrary.
	if got, ok := resolveRef(nil, ""); ok {
		t.Fatalf("resolveRef on an empty listing = %q, want no match", got)
	}
}

// A symbolic reference whose target is missing must fail rather than return a
// zero hash that would later be built as if it were a commit.
func TestResolveRefRejectsADanglingSymbolicRef(t *testing.T) {
	refs := []*plumbing.Reference{
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("gone")),
	}
	if got, ok := resolveRef(refs, ""); ok {
		t.Fatalf("resolveRef = %q, want no match for a dangling HEAD", got)
	}
}

// The end-to-end path against a real repository on disk: seed a bare remote,
// then read its default branch back the way the build command does.
func TestRemoteResolverReadsALiveRemote(t *testing.T) {
	remote := bareRemote(t)
	seedMain(t, remote)
	// gogit.PlainInit points a fresh bare repository's HEAD at
	// refs/heads/master; a real remote's HEAD names the branch that exists.
	setRemoteHEAD(t, remote, "main")

	r := RemoteResolver{}
	got, err := r.Resolve(context.Background(), remote, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("Resolve = %q, want a 40-character commit hash", got)
	}
	byName, err := r.Resolve(context.Background(), remote, "main")
	if err != nil {
		t.Fatalf("Resolve(main): %v", err)
	}
	if byName != got {
		t.Fatalf("HEAD resolved to %s but main resolved to %s", got, byName)
	}
}

func setRemoteHEAD(t *testing.T, dir, branch string) {
	t.Helper()
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	head := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch))
	if err := repo.Storer.SetReference(head); err != nil {
		t.Fatalf("set remote HEAD: %v", err)
	}
}

func TestRemoteResolverErrors(t *testing.T) {
	r := RemoteResolver{}
	if _, err := r.Resolve(context.Background(), "", "main"); err == nil {
		t.Fatal("an empty repository URL must be rejected")
	}

	remote := bareRemote(t)
	seedMain(t, remote)
	_, err := r.Resolve(context.Background(), remote, "does-not-exist")
	if err == nil {
		t.Fatal("an unknown ref must be reported, not silently resolved")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error should name the ref that was not found, got: %v", err)
	}
}

// bareRemote is an empty bare repository on disk: enough for `ls-remote` to
// answer, and no network.
func bareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}
	return dir
}

// seedMain gives the bare remote one commit on refs/heads/main, written
// straight through go-git's object store: this package reads refs and never
// writes any, so a test worktree would be machinery proving nothing.
func seedMain(t *testing.T, remote string) {
	t.Helper()
	repo, err := gogit.PlainOpen(remote)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	store := repo.Storer

	blob := store.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write([]byte("# deploy\n")); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}
	blobHash, err := store.SetEncodedObject(blob)
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}

	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: "README.md", Mode: filemode.Regular, Hash: blobHash},
	}}
	treeObj := store.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatalf("encode tree: %v", err)
	}
	treeHash, err := store.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatalf("store tree: %v", err)
	}

	when := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	commit := &object.Commit{
		Author:    object.Signature{Name: "Test User", Email: "test@example.com", When: when},
		Committer: object.Signature{Name: "Test User", Email: "test@example.com", When: when},
		Message:   "seed\n",
		TreeHash:  treeHash,
	}
	commitObj := store.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("encode commit: %v", err)
	}
	commitHash, err := store.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("store commit: %v", err)
	}
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commitHash)
	if err := store.SetReference(ref); err != nil {
		t.Fatalf("set refs/heads/main: %v", err)
	}
}
