package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// --- fixtures ---------------------------------------------------------------

const (
	promoteWebImage    = "ghcr.io/acme/hello@sha256:aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	promoteDigestImage = "ghcr.io/acme/hello@sha256:bbbb1111cccc2222dddd3333eeee4444ffff5555aaaa6666bbbb7777cccc8888"
)

// promoteFiles writes the three-document fixture as three files, because that
// is what makes the write target unambiguous: promote edits the file that
// declares the target environment and no other.
func promoteFiles(t *testing.T) (project, staging, production, history string) {
	t.Helper()
	dir := t.TempDir()

	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	project = write("project.yaml", "apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata:\n  name: hello\n\nspec:\n"+
		"  image: ghcr.io/acme/hello:main\n\n  components:\n    - name: web\n      port: 8080\n"+
		"    - name: digest\n      schedule: 30 6 * * 1-5\n")
	staging = write("staging.yaml", "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: staging\n\nspec:\n  project: hello\n")
	production = write("production.yaml", "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: production\n\n"+
		"# Production is conservative, and this comment must survive a promotion.\n"+
		"spec:\n  project: hello\n\n  components:\n    - name: web\n      replicas: { min: 3 }   # the front door\n")
	return project, staging, production, filepath.Join(dir, "history")
}

func promoteManifest(kind, component, image string) delivery.Manifest {
	var body string
	if kind == "CronJob" {
		body = "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: " + component +
			"\n  namespace: hello-staging\n  labels:\n    kelson.dev/application: " + component +
			"\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n" +
			"            - name: " + component + "\n              image: " + image + "\n"
	} else {
		body = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + component +
			"\n  namespace: hello-staging\n  labels:\n    kelson.dev/application: " + component +
			"\nspec:\n  template:\n    spec:\n      containers:\n        - name: " + component +
			"\n          image: " + image + "\n"
	}
	return delivery.Manifest{Kind: kind, Name: component, Namespace: "hello-staging", YAML: []byte(body)}
}

// stagingDeployed is the history the promotion reads: one revision, two
// workloads, each running a digest-pinned image.
func stagingDeployed() (*fakeAdapter, fakeRecorded) {
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{{Revision: "000004", CommittedAt: "2026-08-13T09:00:00Z"}}
	src := fakeRecorded{
		byRev: map[string][]delivery.Manifest{
			"000004": {
				promoteManifest("Deployment", "web", promoteWebImage),
				promoteManifest("CronJob", "digest", promoteDigestImage),
			},
		},
	}
	return adapter, src
}

func runPromoteCmd(t *testing.T, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	adapter, src := stagingDeployed()
	return runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, src), args...)
}

// --- tests ------------------------------------------------------------------

// TestPromoteWritesThePinsAndKeepsTheDocument is the whole command: the target
// file gains one image line per component and loses nothing.
func TestPromoteWritesThePinsAndKeepsTheDocument(t *testing.T) {
	project, staging, production, history := promoteFiles(t)
	before, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}

	stdout, code, msg := runPromoteCmd(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--history", history, "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}

	after, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(after)
	for _, want := range []string{
		"image: " + promoteWebImage,
		"image: " + promoteDigestImage,
		"# Production is conservative, and this comment must survive a promotion.",
		"replicas: { min: 3 }   # the front door",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the promoted document lost %q:\n%s", want, doc)
		}
	}
	if len(after) <= len(before) {
		t.Errorf("the document did not grow:\n%s", doc)
	}
	if !strings.Contains(stdout, "Next: kelson deploy") {
		t.Errorf("the follow-up hint was not printed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "2 pinned, 0 unchanged, 0 skipped") {
		t.Errorf("the summary was not printed:\n%s", stdout)
	}
}

// TestPromotePrintsThePlanAndTheDiffBeforeWriting: what is about to happen is
// on screen before the file changes, which is the same order rollback keeps.
func TestPromotePrintsThePlanAndTheDiffBeforeWriting(t *testing.T) {
	project, staging, production, history := promoteFiles(t)

	stdout, code, msg := runPromoteCmd(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--history", history, "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	plan := strings.Index(stdout, "COMPONENT")
	wrote := strings.Index(stdout, "Wrote 2 pin(s)")
	switch {
	case plan < 0:
		t.Fatalf("the promotion plan was not printed:\n%s", stdout)
	case wrote < 0:
		t.Fatalf("the write was not reported:\n%s", stdout)
	case plan > wrote:
		t.Fatalf("the plan must precede the write:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Deployment/web") {
		t.Errorf("the diff of the target environment was not printed:\n%s", stdout)
	}
}

// TestPromoteDryRunWritesNothing.
func TestPromoteDryRunWritesNothing(t *testing.T) {
	project, staging, production, history := promoteFiles(t)
	before, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}

	stdout, code, msg := runPromoteCmd(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--history", history, "--dry-run")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	after, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("a dry run modified the document:\n%s", after)
	}
	if !strings.Contains(stdout, "Dry run:") {
		t.Errorf("a dry run did not say so:\n%s", stdout)
	}
	if !strings.Contains(stdout, promoteWebImage) {
		t.Errorf("a dry run must still say what it would pin:\n%s", stdout)
	}
}

// TestPromoteWithoutConfirmationWritesNothing: stdin is closed, so the prompt
// gets no answer, which is not a yes.
func TestPromoteWithoutConfirmationWritesNothing(t *testing.T) {
	project, staging, production, history := promoteFiles(t)
	before, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}

	stdout, code, msg := runPromoteCmd(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--history", history)
	if code != exitErr {
		t.Fatalf("exit = %d, want 1\n%s", code, stdout)
	}
	if !strings.Contains(msg, "cancelled") {
		t.Errorf("message = %q, want a cancellation", msg)
	}
	after, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a cancelled promotion wrote to the document")
	}
}

// TestPromoteRefusesWhenNothingHasBeenDeployed.
func TestPromoteRefusesWhenNothingHasBeenDeployed(t *testing.T) {
	project, staging, production, history := promoteFiles(t)
	adapter := newFakeAdapter("direct")

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, fakeRecorded{}),
		"promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--history", history, "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want 1\n%s", code, stdout)
	}
	if !strings.Contains(msg, "promote/nothing-deployed") {
		t.Errorf("message = %q, want the structured refusal", msg)
	}
}

// TestPromoteHonoursTheComponentFilter.
func TestPromoteHonoursTheComponentFilter(t *testing.T) {
	project, staging, production, history := promoteFiles(t)

	stdout, code, msg := runPromoteCmd(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--component", "web", "--history", history, "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	doc, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), promoteDigestImage) {
		t.Errorf("a filtered-out component was pinned:\n%s", doc)
	}
	if !strings.Contains(string(doc), promoteWebImage) {
		t.Errorf("the selected component was not pinned:\n%s", doc)
	}
}

// TestPromoteToItselfIsRefused.
func TestPromoteToItselfIsRefused(t *testing.T) {
	project, staging, production, history := promoteFiles(t)

	_, code, msg := runPromoteCmd(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "staging", "--history", history, "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(msg, "itself") {
		t.Errorf("message = %q", msg)
	}
}

// TestPromoteASecondTimeChangesNothing: the pins are already what staging runs,
// so the command reports no-ops and leaves the file alone.
func TestPromoteASecondTimeChangesNothing(t *testing.T) {
	project, staging, production, history := promoteFiles(t)
	args := []string{"promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--history", history, "--yes"}

	if _, code, msg := runPromoteCmd(t, args...); code != exitOK {
		t.Fatalf("first promote: exit = %d (%s)", code, msg)
	}
	first, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}

	stdout, code, msg := runPromoteCmd(t, args...)
	if code != exitOK {
		t.Fatalf("second promote: exit = %d (%s)\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "0 pinned, 2 unchanged, 0 skipped") {
		t.Errorf("the second promotion was not a no-op:\n%s", stdout)
	}
	second, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Errorf("a no-op promotion rewrote the document:\n%s", second)
	}
}
