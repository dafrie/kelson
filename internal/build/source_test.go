package build_test

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/build"
)

// The clone script is shared by every driver, so what it guarantees is
// asserted once here rather than per driver: one commit where it can, a
// POSIX-only script, and a repository URL that cannot escape into the shell.
func TestCloneScript(t *testing.T) {
	script := build.CloneScript(build.Request{
		SourceGit: "https://github.com/acme/shop.git",
		SourceRef: "9f1c0dea",
	})
	for _, want := range []string{
		"git init -q /workspace",
		"git remote add origin 'https://github.com/acme/shop.git'",
		"git fetch --depth 1 origin '9f1c0dea'",
		"git checkout -q FETCH_HEAD",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("clone script must contain %q\n%s", want, script)
		}
	}
	// `set -o pipefail` is a bashism, and the shells these scripts run under
	// (busybox ash in the git image, dash in an Ubuntu-based builder) are not
	// all bash — dash exits on it before the clone starts.
	if strings.Contains(script, "pipefail") {
		t.Errorf("the clone script must stay POSIX\n%s", script)
	}

	// No ref means the remote's HEAD, not a failure.
	if !strings.Contains(build.CloneScript(build.Request{SourceGit: "x"}), "origin 'HEAD'") {
		t.Error("an empty ref must fetch HEAD")
	}
}

func TestCloneScriptQuotesTheRepositoryURL(t *testing.T) {
	script := build.CloneScript(build.Request{SourceGit: "https://example.com/x.git; touch /pwned"})
	if !strings.Contains(script, "'https://example.com/x.git; touch /pwned'") {
		t.Errorf("the repository URL must be single-quoted\n%s", script)
	}
	if strings.Contains(script, "origin https://") {
		t.Errorf("the repository URL reached the shell unquoted\n%s", script)
	}
}

func TestContextPath(t *testing.T) {
	cases := map[string]string{
		"":          "/workspace",
		".":         "/workspace",
		"/":         "/workspace",
		"apps/web":  "/workspace/apps/web",
		"/apps/web": "/workspace/apps/web",
	}
	for dir, want := range cases {
		if got := build.ContextPath(build.Request{ContextDir: dir}); got != want {
			t.Errorf("ContextPath(%q) = %q, want %q", dir, got, want)
		}
	}
}
