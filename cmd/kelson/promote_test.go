package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// `kelson promote` is gated (issue #224).
//
// The tests that asserted the byte-faithful splice, the plan table and the
// promotion diff went with the input those all hang off: the deployed digest
// came from the rendered-history journal, which ADR-0027 decision 7 deleted.
// internal/promote — the plan, the splice and their own tests — is untouched
// and still covered in its own package.
//
// What is asserted here is the two properties the gate itself has to have, and
// they are the ones a silently-broken promotion would violate: it refuses with
// the tracked code, and it writes nothing to the file it would have edited.

const promoteWebImage = "ghcr.io/acme/hello@sha256:aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"

// promoteFiles writes the three-document fixture as three files, because that
// is what makes the write target unambiguous: promote edits the file that
// declares the target environment and no other.
func promoteFiles(t *testing.T) (project, staging, production string) {
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
		"  image: ghcr.io/acme/hello:main\n\n  components:\n    - name: web\n      port: 8080\n")
	staging = write("staging.yaml", "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: staging\n\nspec:\n  project: hello\n")
	production = write("production.yaml", "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: production\n\n"+
		"# Production is conservative, and this comment must survive a promotion.\n"+
		"spec:\n  project: hello\n\n  components:\n    - name: web\n      replicas: { min: 3 }   # the front door\n")
	return project, staging, production
}

// A gated promotion must leave the target document exactly as it found it. A
// refusal that had already written half the pins would be worse than either
// outcome it is between.
func TestPromoteRefusesAndWritesNothing(t *testing.T) {
	project, staging, production := promoteFiles(t)
	before, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}

	_, code, msg := runRoot(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "production", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d (%s)", code, exitErr, msg)
	}
	if !strings.Contains(msg, string(delivery.ErrNotImplemented)) {
		t.Errorf("the refusal must carry %s, got: %s", delivery.ErrNotImplemented, msg)
	}
	if !strings.Contains(msg, "staging") {
		t.Errorf("the refusal should name the environment whose deployed images it cannot read: %s", msg)
	}

	after, err := os.ReadFile(production)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the target document was modified by a refused promotion:\n%s", after)
	}
	if strings.Contains(string(after), promoteWebImage) {
		t.Error("a pin was written")
	}
}

// The refusals that are about the request rather than about kelson still come
// first: promoting an environment to itself is a mistake the caller can fix,
// and telling them about the gate instead would send them to an issue tracker
// for a typo.
func TestPromoteToItselfIsRefusedBeforeTheGate(t *testing.T) {
	project, staging, production := promoteFiles(t)
	_, code, msg := runRoot(t, "promote", "-f", project, "-f", staging, "-f", production,
		"--from", "staging", "--to", "staging", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if strings.Contains(msg, "#224") {
		t.Errorf("a request mistake must not be reported as kelson's gap: %s", msg)
	}
	if !strings.Contains(msg, "itself") {
		t.Errorf("error %q does not explain the contradiction", msg)
	}
}
