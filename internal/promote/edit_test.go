package promote

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pinned image used throughout: a digest, which is what a promotion
// actually writes.
const promoted = "ghcr.io/acme/checkout@sha256:9f6ad2c1b1c1e1f1a1b1c1d1e1f1a1b1c1d1e1f1a1b1c1d1e1f1a1b1c1d1e1f1"

// TestPinReplacesAnExistingImageAndTouchesNothingElse is the core promise: the
// document that comes back differs from the one that went in by exactly the
// bytes of the pin.
func TestPinReplacesAnExistingImageAndTouchesNothingElse(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production

# The comment above spec, and the blank line above this one, are the point.
spec:
  project: checkout
  namespace: checkout-prod

  routing:
    domainSuffix: acme.com

  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:0000    # what production runs today
      replicas: { min: 3, max: 20 }
    - name: worker
      env:
        CONCURRENCY: "10"
`
	out, err := Pin([]byte(doc), "production", "web", promoted)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	want := strings.Replace(doc,
		"image: ghcr.io/acme/checkout@sha256:0000    # what production runs today",
		"image: "+promoted+"    # what production runs today", 1)
	if string(out) != want {
		t.Errorf("Pin rewrote more than the pin:\n--- got\n%s\n--- want\n%s", out, want)
	}
}

// TestPinAddsAnImageToAnOverrideThatHasNone: the override exists, carries
// other fields, and gains one line under its name.
func TestPinAddsAnImageToAnOverrideThatHasNone(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout

  components:
    - name: web
      replicas: { min: 3, max: 20 }   # unrelated, and it stays
      resources:
        requests: { cpu: 500m }
    - name: worker
      env:
        CONCURRENCY: "10"
`
	out, err := Pin([]byte(doc), "production", "web", promoted)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	want := strings.Replace(doc,
		"    - name: web\n",
		"    - name: web\n      image: "+promoted+"\n", 1)
	if string(out) != want {
		t.Errorf("Pin:\n--- got\n%s\n--- want\n%s", out, want)
	}
}

// TestPinAppendsAnOverrideForAComponentTheEnvironmentDoesNotMention.
func TestPinAppendsAnOverrideForAComponentTheEnvironmentDoesNotMention(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components:
    - name: web
      replicas: { min: 3 }
`
	out, err := Pin([]byte(doc), "production", "worker", promoted)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	want := doc + "    - name: worker\n      image: " + promoted + "\n"
	if string(out) != want {
		t.Errorf("Pin:\n--- got\n%q\n--- want\n%q", out, want)
	}
	assertPin(t, out, "production", "worker", promoted)
}

// TestPinCreatesAMinimalComponentsListWhenThereIsNone is the "gains one
// minimally" case: a document with no components list gets exactly three
// lines, appended after everything spec already carries.
func TestPinCreatesAMinimalComponentsListWhenThereIsNone(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production

spec:
  project: checkout
  namespace: checkout-prod

  routing:
    domainSuffix: acme.com   # kept
`
	out, err := Pin([]byte(doc), "production", "web", promoted)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	want := doc + "  components:\n    - name: web\n      image: " + promoted + "\n"
	if string(out) != want {
		t.Errorf("Pin:\n--- got\n%q\n--- want\n%q", out, want)
	}
	assertPin(t, out, "production", "web", promoted)
}

// TestPinFillsAComponentsKeyWithNoValue.
func TestPinFillsAComponentsKeyWithNoValue(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components:
  namespace: checkout-prod
`
	out, err := Pin([]byte(doc), "production", "web", promoted)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	assertPin(t, out, "production", "web", promoted)
	if !strings.Contains(string(out), "  namespace: checkout-prod\n") {
		t.Errorf("the key after components was disturbed:\n%s", out)
	}
}

// TestPinSelectsTheNamedEnvironmentInAMultiDocumentFile: a -f file may hold
// the Project and several Environments, and node line numbers must stay
// relative to the whole stream for the splice to land in the right one.
func TestPinSelectsTheNamedEnvironmentInAMultiDocumentFile(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  image: ghcr.io/acme/checkout:main
  components:
    - name: web
      port: 8080
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  project: checkout
  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:aaaa
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:bbbb
`
	out, err := Pin([]byte(doc), "production", "web", promoted)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if !strings.Contains(string(out), "image: ghcr.io/acme/checkout@sha256:aaaa") {
		t.Errorf("staging's pin was rewritten:\n%s", out)
	}
	if strings.Contains(string(out), "sha256:bbbb") {
		t.Errorf("production's pin was not rewritten:\n%s", out)
	}
	assertPin(t, out, "production", "web", promoted)
	assertPin(t, out, "staging", "web", "ghcr.io/acme/checkout@sha256:aaaa")
}

// TestPinRefusesAFlowStyleComponentsList: refusing is the honest answer, and
// the error names the taxonomy an agent branches on.
func TestPinRefusesAFlowStyleComponentsList(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components: [{ name: web }]
`
	_, err := Pin([]byte(doc), "production", "web", promoted)
	var pe Error
	if !errors.As(err, &pe) || pe.Code != ErrDocumentUnwritable {
		t.Fatalf("want promote/document-unwritable, got %v", err)
	}
}

// TestPinRefusesAFlowStyleOverride.
func TestPinRefusesAFlowStyleOverride(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components:
    - { name: web, replicas: { min: 2 } }
`
	_, err := Pin([]byte(doc), "production", "web", promoted)
	var pe Error
	if !errors.As(err, &pe) || pe.Code != ErrDocumentUnwritable {
		t.Fatalf("want promote/document-unwritable, got %v", err)
	}
}

// TestPinQuotesAReferenceThatNeedsIt: yaml.Marshal decides, not a hand rule.
func TestPinQuotesAReferenceThatNeedsIt(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components:
    - name: web
      image: old
`
	out, err := Pin([]byte(doc), "production", "web", "yes")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if !strings.Contains(string(out), `image: "yes"`) {
		t.Errorf("an ambiguous scalar was written unquoted:\n%s", out)
	}
	assertPin(t, out, "production", "web", "yes")
}

// TestPinRefusesABlankImage.
func TestPinRefusesABlankImage(t *testing.T) {
	doc := "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: production\nspec:\n  project: checkout\n"
	if _, err := Pin([]byte(doc), "production", "web", "   "); err == nil {
		t.Fatal("a blank pin was accepted")
	}
}

// TestPinLeavesEveryRepositoryExampleByteFaithful walks the checked-in example
// Environments: each is pinned, and every line except the one carrying the pin
// must come back identical. It is the guard the ADR-0013 byte-fidelity promise
// needs against a future "just re-serialize it" simplification.
func TestPinLeavesEveryRepositoryExampleByteFaithful(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, file := range files {
		data, err := os.ReadFile(file) //nolint:gosec // a checked-in example path
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "kind: Environment") {
			continue
		}
		out, err := Pin(data, "", "web", promoted)
		if err != nil {
			// Not every example declares a `web` component; a refusal that
			// names the document shape is a different test's subject.
			continue
		}
		checked++
		assertOnlyPinChanged(t, file, string(data), string(out))
	}
	if checked == 0 {
		t.Fatal("no example Environment was exercised; the glob no longer finds them")
	}
}

// assertOnlyPinChanged asserts that the two documents differ only by lines the
// pin is allowed to write: the pin itself, the `- name:` entry that carries it
// and the `components:` key that holds them, added; and at most the image line
// the pin replaced, removed.
func assertOnlyPinChanged(t *testing.T, file, before, after string) {
	t.Helper()
	counts := map[string]int{}
	for _, line := range strings.Split(before, "\n") {
		counts[line]++
	}
	for _, line := range strings.Split(after, "\n") {
		counts[line]--
	}
	for line, n := range counts {
		switch {
		case n == 0:
		case n < 0: // added by the pin
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(line, promoted) && trimmed != "components:" && trimmed != "- name: web" {
				t.Errorf("%s: the pin added %q, which is not part of a pin", file, line)
			}
		default: // removed by the pin
			if !strings.Contains(line, "image:") {
				t.Errorf("%s: the pin removed %q, which is not the image it replaced", file, line)
			}
		}
	}
}

func assertPin(t *testing.T, doc []byte, environment, component, want string) {
	t.Helper()
	if err := verifyPin(doc, environment, component, want, false); err != nil {
		t.Errorf("%v\n%s", err, doc)
	}
}

/* ------------------------------------------- the marker (ADR-0036 decision 5) */

// A tracked pin writes two lines rather than one, and the second is what tells
// the next push that this image is a starting point rather than a hold. The
// image sits above the marker, next to the name it belongs to.
func TestTrackedPinWritesTheMarkerBesideTheImage(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  project: checkout
  autoDeploy: true
  components:
    - name: web
      replicas: { min: 2 }   # unrelated, and it stays
`
	out, err := Pin([]byte(doc), "staging", "web", promoted, Tracked())
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	want := strings.Replace(doc,
		"    - name: web\n",
		"    - name: web\n      image: "+promoted+"\n      imageTracked: true\n", 1)
	if string(out) != want {
		t.Errorf("Pin:\n--- got\n%s\n--- want\n%s", out, want)
	}
	if err := verifyPin(out, "staging", "web", promoted, true); err != nil {
		t.Error(err)
	}
}

// The second push is the one the marker exists for: it replaces the image the
// first one wrote and leaves the marker exactly where it is, so the document
// after two pushes differs from the document after one by the digest alone.
func TestTrackedPinReplacesItsOwnPin(t *testing.T) {
	doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: checkout
  autoDeploy: true
  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:0000
      imageTracked: true
`
	out, err := Pin([]byte(doc), "staging", "web", promoted, Tracked())
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	want := strings.Replace(doc, "ghcr.io/acme/checkout@sha256:0000", promoted, 1)
	if string(out) != want {
		t.Errorf("a second tracked pin rewrote more than the digest:\n--- got\n%s\n--- want\n%s", out, want)
	}
}

// A person pinning over a trigger's pin takes the component back: the marker is
// cleared rather than inherited, or the next push would overwrite a promotion.
// Nothing is written where no marker was — a promotion into a document that has
// never auto-deployed is still one image line and nothing else.
func TestAnUnmarkedPinClearsAMarkerAndOtherwiseWritesNone(t *testing.T) {
	t.Run("clears a marker it finds", func(t *testing.T) {
		doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: checkout
  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:0000
      imageTracked: true
`
		out, err := Pin([]byte(doc), "staging", "web", promoted)
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		want := strings.Replace(
			strings.Replace(doc, "ghcr.io/acme/checkout@sha256:0000", promoted, 1),
			"imageTracked: true", "imageTracked: false", 1)
		if string(out) != want {
			t.Errorf("Pin:\n--- got\n%s\n--- want\n%s", out, want)
		}
	})

	t.Run("writes none where there was none", func(t *testing.T) {
		doc := `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:0000
`
		out, err := Pin([]byte(doc), "production", "web", promoted)
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		if strings.Contains(string(out), "imageTracked") {
			t.Errorf("a promotion wrote a marker to say what its absence already says:\n%s", out)
		}
	})
}
