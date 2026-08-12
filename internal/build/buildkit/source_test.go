package buildkit

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/build"
)

func srcReq() build.Request {
	return build.Request{
		Project:     "shop",
		Application: "web",
		SourceGit:   "https://github.com/acme/shop.git",
		SourceRef:   "9f1c0de",
		Revision:    "9f1c0de",
		Image:       "ghcr.io/acme/web",
		Tag:         "shop-web-9f1c0de",
	}
}

// TestCloneInitContainerFetchesTheSource: an in-cluster build starts with an
// empty workspace, so the source must arrive via the clone init container
// (#48). Without it the build silently builds nothing.
func TestCloneInitContainerFetchesTheSource(t *testing.T) {
	out, err := Config{Namespace: "kelson"}.Workload(srcReq())
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "initContainers:") {
		t.Fatalf("no init container rendered\n%s", s)
	}
	for _, want := range []string{
		"https://github.com/acme/shop.git",
		"git fetch --depth 1 origin",
		"9f1c0de",
		"/workspace",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("clone command missing %q", want)
		}
	}
}

// TestCloneInitContainerIsNotPrivileged: the clone runs the same unprivileged
// posture as the build. An init container is an easy place to quietly regain
// root, which would defeat the guarantee #48 exists to make.
func TestCloneInitContainerIsNotPrivileged(t *testing.T) {
	out, err := Config{Namespace: "kelson"}.Workload(srcReq())
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "privileged: true") {
		t.Error("rendered workload contains privileged: true")
	}
	if strings.Contains(s, "SYS_ADMIN") {
		t.Error("rendered workload grants CAP_SYS_ADMIN")
	}
	if !strings.Contains(s, "runAsNonRoot: true") {
		t.Error("runAsNonRoot not set")
	}
}

// TestNoSourceMeansNoCloneContainer: a caller that populates the workspace
// itself must not get an init container that would overwrite it.
func TestNoSourceMeansNoCloneContainer(t *testing.T) {
	req := srcReq()
	req.SourceGit = ""
	out, err := Config{Namespace: "kelson"}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	if strings.Contains(string(out), "initContainers:") {
		t.Error("init container rendered with no source configured")
	}
}

// TestContextDirSelectsMonorepoSubtree: the repository clones whole and the
// build context is a subdirectory, which is what makes a monorepo buildable.
func TestContextDirSelectsMonorepoSubtree(t *testing.T) {
	req := srcReq()
	req.ContextDir = "apps/web"
	out, err := Config{Namespace: "kelson"}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	if !strings.Contains(string(out), "--local context=/workspace/apps/web") {
		t.Errorf("build context is not the subtree\n%s", out)
	}
}

// TestCloneURLIsShellQuoted: the repository URL is user input reaching a
// generated shell command. Quoting is the difference between a config error
// and command injection.
func TestCloneURLIsShellQuoted(t *testing.T) {
	req := srcReq()
	req.SourceGit = "https://example.com/x.git; touch /pwned"
	out, err := Config{Namespace: "kelson"}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "origin https://example.com/x.git; touch") {
		t.Errorf("URL interpolated unquoted into the shell command\n%s", s)
	}
	if !strings.Contains(s, "'https://example.com/x.git; touch /pwned'") {
		t.Errorf("URL not single-quoted\n%s", s)
	}
}
