// Package version holds build-time metadata for kelson binaries.
//
// Version and Commit are injected at link time via -ldflags; defaults apply
// for plain `go build`/`go test` so the package is usable from an unchecked
// checkout. See the Makefile and .goreleaser.yml for the injection sites,
// and docs/release-policy.md for the versioning contract. Issue #21.
package version

import "fmt"

var (
	// Version is the release version. Default "0.0.0-dev" for unchecked
	// builds; overridden by ldflags during release builds.
	Version = "0.0.0-dev"

	// Commit is the short git sha the binary was built from. Default "none".
	Commit = "none"
)

// String renders the version for the CLI `--version` output.
func String() string {
	return fmt.Sprintf("%s (commit %s)", Version, Commit)
}
