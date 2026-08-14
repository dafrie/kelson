// Package gitref answers "what commit does this ref name?" against a remote
// repository, for the build plane.
package gitref

import (
	"context"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/dafrie/kelson/internal/delivery"
)

// RemoteResolver turns a branch, tag or "HEAD" into the commit it currently
// names, by asking the remote — the `git ls-remote` question, with no clone
// and no working tree.
//
// # Why this package still exists after ADR-0028
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) deleted the git writer and said
// choosing OCI "removes go-git" from kelson. That is true of the *transport*:
// nothing commits, pushes, opens a pull request or keeps an in-memory worktree
// any more. It was never true of this, which is the BUILD plane's question and
// not the delivery plane's — it only lived beside the writer because go-git
// did, and it survives the writer for the same reason it existed before it.
//
// # Why the build needs it at all
//
// A build records what it built (build.Request.Revision) and tags the image
// from it. A branch name is a moving target: recording "main" says nothing
// about which commit is in the image, and re-running the build later would
// produce a different image under the same tag. Resolving the ref once, in the
// CLI, means the tag, the recorded revision and the commit the build pod
// checks out are all the same 40-hex string.
//
// The command plane may not import the git libraries (.golangci.yml), so
// `kelson build` consumes this through a one-method interface the same way it
// consumes the cluster — production wires this type in, tests wire a fake.
type RemoteResolver struct {
	// Auth authenticates the remote read. Nil means anonymous, which is
	// correct for public repositories and local paths.
	Auth Auth
}

// Resolve returns the commit hash that ref names in repo. An empty ref (or
// "HEAD") resolves the repository's default branch.
//
// Resolution order for a short name is branch, then tag — the same precedence
// git itself uses — and an annotated tag resolves to the commit it points at
// (the peeled "^{}" entry), never to the tag object, because the tag object's
// hash is not something a build can check out as a tree.
func (r RemoteResolver) Resolve(ctx context.Context, repo, ref string) (string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", delivery.ApplyFailed("git/revision", "repo",
			"cannot resolve a git revision without a repository URL",
			"set spec.source.git on the Project")
	}

	auth := Auth(Anonymous{})
	if r.Auth != nil {
		auth = r.Auth
	}
	method, err := auth.GitAuth()
	if err != nil {
		return "", err
	}

	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: gogit.DefaultRemoteName,
		URLs: []string{repo},
	})
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{Auth: method})
	if err != nil {
		e := delivery.ApplyFailed("git/revision", "repo",
			"could not read the refs of the source repository",
			"check that the repository URL is reachable and that the credential (e.g. KELSON_GIT_TOKEN) can read it")
		e.Cause = err.Error()
		return "", e
	}

	hash, ok := resolveRef(refs, ref)
	if !ok {
		return "", delivery.ApplyFailed("git/revision", "ref",
			"the source repository has no branch or tag named "+displayRef(ref),
			"pass --ref with a branch, tag or 40-character commit that exists in the repository")
	}
	return hash, nil
}

// resolveRef picks the commit for ref out of an ls-remote listing. It is a
// pure function of the listing so its precedence is unit-testable without a
// remote.
func resolveRef(refs []*plumbing.Reference, ref string) (string, bool) {
	byName := make(map[plumbing.ReferenceName]*plumbing.Reference, len(refs))
	for _, r := range refs {
		byName[r.Name()] = r
	}

	ref = strings.TrimSpace(ref)
	var candidates []plumbing.ReferenceName
	switch {
	case ref == "" || ref == "HEAD":
		candidates = []plumbing.ReferenceName{plumbing.HEAD}
	case strings.HasPrefix(ref, "refs/"):
		// A fully-qualified ref is taken as written; the peeled entry first so
		// an annotated tag still yields a commit.
		candidates = []plumbing.ReferenceName{plumbing.ReferenceName(ref + peeledSuffix), plumbing.ReferenceName(ref)}
	default:
		candidates = []plumbing.ReferenceName{
			plumbing.NewBranchReferenceName(ref),
			plumbing.ReferenceName(string(plumbing.NewTagReferenceName(ref)) + peeledSuffix),
			plumbing.NewTagReferenceName(ref),
		}
	}

	for _, name := range candidates {
		if hash, ok := follow(byName, name); ok {
			return hash, true
		}
	}
	return "", false
}

// peeledSuffix marks the entry an ls-remote listing carries for the commit an
// annotated tag points at.
const peeledSuffix = "^{}"

// follow walks symbolic references (HEAD → refs/heads/main) to the hash. The
// hop limit is a guard against a malformed listing, not an expected case: real
// listings are one hop deep.
func follow(byName map[plumbing.ReferenceName]*plumbing.Reference, name plumbing.ReferenceName) (string, bool) {
	r, ok := byName[name]
	for hops := 0; ok && r.Type() == plumbing.SymbolicReference && hops < 4; hops++ {
		r, ok = byName[r.Target()]
	}
	if !ok || r.Type() != plumbing.HashReference {
		return "", false
	}
	return r.Hash().String(), true
}

func displayRef(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return "HEAD (the default branch)"
	}
	return `"` + ref + `"`
}
