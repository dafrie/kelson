package build

import (
	"fmt"
	"strings"
)

// How a build pod gets its source, shared by every driver.
//
// Cloning in-pod is a decision of the build plane, not of one strategy: an
// in-cluster build starts with an empty workspace, and cloning there keeps the
// control plane out of the data path — nothing uploads a tarball through
// kelson. Both drivers render the same clone, so the script lives here rather
// than once per driver, where the two copies would drift and only one of them
// would be the one anybody read.
//
// These are strings, not containers: the shape of the init container is the
// driver's (its image, its security context, its volume names), and only the
// script and the path convention are common.

// Workspace is where the source tree lives inside a build pod. Every driver
// mounts the same path, so a build's context path means the same thing
// whichever strategy renders it.
const Workspace = "/workspace"

// CloneScript is the shell script that fetches exactly one commit into
// [Workspace].
//
// A ref that names a commit is fetched directly at depth 1, which is both the
// fastest path and the only one that guarantees the build matches Revision. A
// branch or tag is fetched at depth 1 too; that is a moving target, and this
// comment says so rather than pretending the result is pinned.
func CloneScript(req Request) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "git init -q %s\n", Workspace)
	fmt.Fprintf(&b, "cd %s\n", Workspace)
	fmt.Fprintf(&b, "git remote add origin %s\n", ShellQuote(req.SourceGit))
	ref := req.SourceRef
	if ref == "" {
		ref = "HEAD"
	}
	fmt.Fprintf(&b, "git fetch --depth 1 origin %s\n", ShellQuote(ref))
	b.WriteString("git checkout -q FETCH_HEAD\n")
	return b.String()
}

// ContextPath is the build context inside the cloned workspace. A monorepo
// clones whole and builds one subdirectory, so ContextDir selects the subtree
// rather than changing what is fetched.
func ContextPath(req Request) string {
	dir := strings.Trim(req.ContextDir, "/")
	if dir == "" || dir == "." {
		return Workspace
	}
	return Workspace + "/" + dir
}

// ShellQuote wraps a value in single quotes for a generated shell command.
// The values here come from the spec, not from a build's own output, but a
// repository URL is still user input reaching a shell — quoting it is the
// difference between a config error and a command injection.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
