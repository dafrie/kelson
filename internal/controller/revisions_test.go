package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/build/registry"
)

// The durable record (ADR-0028 decision 4, issue #241): reading the registry's
// tag list back into revisions, and refusing to turn a registry that would not
// answer into a revision that does not exist.

// fakeReader is the registry, without one.
type fakeReader struct {
	tags       []string
	digest     string
	found      bool
	err        error
	repository string
}

func (f *fakeReader) Tags(_ context.Context, repository string) ([]string, error) {
	f.repository = repository
	return f.tags, f.err
}

func (f *fakeReader) Resolve(_ context.Context, repository, _ string) (string, bool, error) {
	f.repository = repository
	return f.digest, f.found, f.err
}

func (f *fakeReader) connect(registry.Credential, bool) RegistryReader { return f }

func TestParseRevisionReadsTheTagGrammarBack(t *testing.T) {
	for _, tc := range []struct {
		tag        string
		generation int64
		short      string
		ok         bool
	}{
		{tag: "7-a1b2c3d4", generation: 7, short: "a1b2c3d4", ok: true},
		{tag: "142-00000000", generation: 142, short: "00000000", ok: true},
		// Tags a repository may hold that kelson did not write. They are not
		// errors and they are not revisions; guessing at either would put
		// something in a history that was never deployed.
		{tag: "latest"},
		{tag: "sha-9f0a1b2c"},
		{tag: "7-a1b2c3"},        // too short to be a spec hash
		{tag: "7-A1B2C3D4"},      // the tag is written lower-case
		{tag: "07-a1b2c3d4"},     // a leading zero is a second spelling of one revision
		{tag: "0-a1b2c3d4"},      // generations start at 1
		{tag: "-a1b2c3d4"},       //
		{tag: "7.1-a1b2c3d4"},    //
		{tag: "7-a1b2c3d4-rc.1"}, // the hash half is exactly eight characters
	} {
		generation, short, ok := ParseRevision(tc.tag)
		if ok != tc.ok || generation != tc.generation || short != tc.short {
			t.Errorf("ParseRevision(%q) = %d, %q, %v; want %d, %q, %v",
				tc.tag, generation, short, ok, tc.generation, tc.short, tc.ok)
		}
	}
}

// A registry returns tags in lexical order, where "10-…" precedes "9-…".
// Sorting by generation is what makes a tag list a history.
func TestSortRevisionsIsNewestFirst(t *testing.T) {
	got := SortRevisions([]string{"1-aaaaaaaa", "10-cccccccc", "9-bbbbbbbb", "latest"})
	if want := "10-cccccccc,9-bbbbbbbb,1-aaaaaaaa"; strings.Join(got, ",") != want {
		t.Errorf("SortRevisions = %v, want %s", got, want)
	}
}

func TestRegistryRevisionsListsOneEnvironmentsRepository(t *testing.T) {
	reader := &fakeReader{tags: []string{"2-0badc0de", "11-a1b2c3d4"}}
	record := RegistryRevisions{Registry: "ghcr.io/acme", Reader: reader.connect}

	revisions, err := record.Revisions(context.Background(), "shop", "production")
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if want := "11-a1b2c3d4,2-0badc0de"; strings.Join(revisions, ",") != want {
		t.Errorf("revisions = %v, want %s", revisions, want)
	}
	// The repository is derived by the one function that derives it for a push,
	// so the two processes cannot look in different places (ADR-0028
	// decision 2).
	if want := "ghcr.io/acme/kelson/shop-production"; reader.repository != want {
		t.Errorf("read %q, want %q", reader.repository, want)
	}
}

// A registry that refused the read is not an environment with no revisions.
// The reason names the credential because that is what has to change.
func TestRegistryReadRefusalIsItsOwnReason(t *testing.T) {
	reader := &fakeReader{err: &artifact.DeniedError{StatusCode: 403, Status: "403 Forbidden", Doing: "listing"}}
	record := RegistryRevisions{Registry: "ghcr.io/acme", Reader: reader.connect}

	_, err := record.Revisions(context.Background(), "shop", "production")
	var refusal *DeliveryError
	if !errors.As(err, &refusal) || refusal.Reason != v1alpha1.ReasonRegistryReadDenied {
		t.Fatalf("Revisions error = %v, want %s", err, v1alpha1.ReasonRegistryReadDenied)
	}
}

func TestRegistryUnreachableTakesBackoff(t *testing.T) {
	reader := &fakeReader{err: &artifact.UnreachableError{Doing: "GET tags", Err: errors.New("no route to host")}}
	record := RegistryRevisions{Registry: "ghcr.io/acme", Reader: reader.connect}

	_, _, err := record.Resolve(context.Background(), "shop", "production", "2-0badc0de")
	var refusal *DeliveryError
	if !errors.As(err, &refusal) || refusal.Reason != v1alpha1.ReasonRegistryUnreachable || !refusal.Backoff {
		t.Fatalf("Resolve error = %v, want %s with backoff", err, v1alpha1.ReasonRegistryUnreachable)
	}
}

// An instance with no registry configured refuses before it asks anything,
// under the reason that names the missing flag rather than the missing tag.
func TestRegistryRevisionsWithoutARegistryRefuses(t *testing.T) {
	_, err := RegistryRevisions{}.Revisions(context.Background(), "shop", "production")
	var refusal *DeliveryError
	if !errors.As(err, &refusal) || refusal.Reason != v1alpha1.ReasonRegistryNotConfigured {
		t.Fatalf("Revisions error = %v, want %s", err, v1alpha1.ReasonRegistryNotConfigured)
	}
}

func TestDescribeRevisionsSaysHowMuchRecordThereIs(t *testing.T) {
	if got := describeRevisions(nil); !strings.Contains(got, "no revisions") {
		t.Errorf("describeRevisions(none) = %q", got)
	}
	got := describeRevisions([]string{"9-aaaaaaaa", "3-bbbbbbbb"})
	if !strings.Contains(got, "2 revisions") || !strings.Contains(got, "9-aaaaaaaa") ||
		!strings.Contains(got, "3-bbbbbbbb") {
		t.Errorf("describeRevisions = %q, want the count and both ends", got)
	}
}
