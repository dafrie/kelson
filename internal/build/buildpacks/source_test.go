package buildpacks

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/build"
)

func srcReq() build.Request {
	return build.Request{
		Project:     testProject,
		Application: "web",
		SourceGit:   "https://github.com/acme/shop.git",
		SourceRef:   "9f1c0de",
		Revision:    "9f1c0de",
		Image:       "ghcr.io/acme/web",
		Tag:         "shop-web-9f1c0de",
	}
}

// TestCloneInitContainerFetchesTheSource: the lifecycle reads a tree that is
// already in the workspace, and an in-cluster build starts with an empty one.
// Without the clone the creator detects nothing and reports "no buildpack
// groups passed detection", which says nothing about the real cause (#49).
func TestCloneInitContainerFetchesTheSource(t *testing.T) {
	out, err := Config{Namespace: testNS}.Workload(srcReq())
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
	if strings.Contains(s, "pipefail") {
		t.Errorf("the generated scripts must stay POSIX: the builder image's /bin/sh has no `set -o pipefail`\n%s", s)
	}
}

// TestCloneInitContainerIsNotPrivileged: an init container is an easy place to
// quietly regain root, which would defeat the guarantee #48 and #49 both make.
func TestCloneInitContainerIsNotPrivileged(t *testing.T) {
	out, err := Config{Namespace: testNS}.Workload(srcReq())
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
	out, err := Config{Namespace: testNS}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	if strings.Contains(string(out), "initContainers:") {
		t.Error("init container rendered with no source configured")
	}
}

// TestContextDirSelectsMonorepoSubtree: the repository clones whole and the
// application the lifecycle builds is a subdirectory of it.
func TestContextDirSelectsMonorepoSubtree(t *testing.T) {
	req := srcReq()
	req.ContextDir = "apps/web"
	out, err := Config{Namespace: testNS}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	if !strings.Contains(string(out), "-app /workspace/apps/web") {
		t.Errorf("the lifecycle's app directory is not the subtree\n%s", out)
	}
}

// TestCloneURLIsShellQuoted: the repository URL is user input reaching a
// generated shell command.
func TestCloneURLIsShellQuoted(t *testing.T) {
	req := srcReq()
	req.SourceGit = "https://example.com/x.git; touch /pwned"
	out, err := Config{Namespace: testNS}.Workload(req)
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

// TestWorkloadReportsThePushedDigest is the half of the build the executor
// depends on: the lifecycle states the pushed digest only in report.toml,
// which nobody can read once the Job is deleted, so the build echoes it in the
// `<repository>@sha256:…` shape the executor parses. Without this line a
// successful build returns no reference at all.
func TestWorkloadReportsThePushedDigest(t *testing.T) {
	cmd := buildContainer(workloadFor(t, baseRequest(), Config{Namespace: testNS})).Command[2]
	for _, want := range []string{
		"-report /tmp/report.toml",
		"/tmp/report.toml | head -n 1",
		"kelson: pushed " + testImage + "@${digest}",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("build command must contain %q\n%s", want, cmd)
		}
	}
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("a report with no digest must fail the build rather than succeed with nothing to deploy\n%s", cmd)
	}
	if strings.Contains(cmd, "exec /cnb/lifecycle/creator") {
		t.Errorf("the creator must not be exec'd: the digest is reported after it\n%s", cmd)
	}
}

// TestWorkloadAnnotatesTheDestination: the executor turns the build into a
// digest-pinned reference from these annotations, not by re-reading the
// lifecycle's command line (build.AnnotationImage).
func TestWorkloadAnnotatesTheDestination(t *testing.T) {
	w := workloadFor(t, baseRequest(), Config{Namespace: testNS})
	if got := w.Metadata.Annotations[build.AnnotationImage]; got != testImage {
		t.Errorf("%s = %q, want %q", build.AnnotationImage, got, testImage)
	}
	if got := w.Metadata.Annotations[build.AnnotationTag]; got != "deadbeefabcd1234" {
		t.Errorf("%s = %q", build.AnnotationTag, got)
	}

	req := baseRequest()
	req.Tag = ""
	w = workloadFor(t, req, Config{Namespace: testNS})
	if _, ok := w.Metadata.Annotations[build.AnnotationTag]; ok {
		t.Error("an untagged build must not annotate a tag")
	}
}

// TestPushSecretIsProjectedAsADockerConfig: the lifecycle authenticates a push
// through a docker config file, so the credential is a projected file and a
// $DOCKER_CONFIG pointing at it — never a value in the manifest (ADR-0009).
func TestPushSecretIsProjectedAsADockerConfig(t *testing.T) {
	w := workloadFor(t, baseRequest(), Config{Namespace: testNS, PushSecret: "ghcr-push"})

	var vol *volume
	for i := range w.Spec.Template.Spec.Volumes {
		if w.Spec.Template.Spec.Volumes[i].Secret != nil {
			vol = &w.Spec.Template.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatalf("no projected credential volume: %+v", w.Spec.Template.Spec.Volumes)
	}
	if vol.Secret.SecretName != "ghcr-push" {
		t.Errorf("secretName = %q", vol.Secret.SecretName)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Path != dockerConfigFile {
		t.Errorf("projected items = %+v, want %s -> %s", vol.Secret.Items, dockerConfigJSONKey, dockerConfigFile)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != pushSecretMode {
		t.Errorf("defaultMode = %v, want %v — a Secret volume's files are root-owned, so the build user needs a readable mode",
			vol.Secret.DefaultMode, pushSecretMode)
	}

	ctr := buildContainer(w)
	var mountedReadOnly bool
	for _, m := range ctr.VolumeMounts {
		if m.Name == vol.Name {
			mountedReadOnly = m.ReadOnly
		}
	}
	if !mountedReadOnly {
		t.Errorf("the credential must be mounted read-only, got %+v", ctr.VolumeMounts)
	}
	if envOf(ctr, "DOCKER_CONFIG") != dockerConfigDir {
		t.Errorf("DOCKER_CONFIG = %q, want %q", envOf(ctr, "DOCKER_CONFIG"), dockerConfigDir)
	}

	// And with no push secret, none of it appears.
	plain := workloadFor(t, baseRequest(), Config{Namespace: testNS})
	for _, v := range plain.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			t.Errorf("a build with no push secret must project none: %+v", v)
		}
	}
	if envOf(buildContainer(plain), "DOCKER_CONFIG") != "" {
		t.Error("DOCKER_CONFIG must not be set without a projected credential")
	}
}

func envOf(ctr container, name string) string {
	for _, e := range ctr.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
