package main

import (
	"context"
	"strings"
	"testing"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

func TestHistoryListsRevisionsNewestFirst(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		history: func(_ context.Context, req *kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error) {
			if req.GetEnvironment() != "development" {
				t.Fatalf("unexpected environment %q", req.GetEnvironment())
			}
			return &kelsonv1alpha1.HistoryResponse{Entries: []*kelsonv1alpha1.HistoryEntry{
				{
					Revision: "3-cccc0000", CommittedAt: "2026-08-15T09:00:00Z",
					Outcome: "Healthy", Digest: "sha256:" + strings.Repeat("a", 64),
					Images: []*kelsonv1alpha1.ComponentImage{
						{Component: "web", Image: "ghcr.io/acme/hello:1.2.0"},
					},
					// The prose the server still fills for older clients. This
					// one is deliberately a lie, because nothing may read it.
					Message: "nonsense · that · must · not · be · printed",
				},
				{
					Revision: "2-bbbb0000", CommittedAt: "2026-08-14T09:00:00Z",
					Outcome: "Healthy",
					// An entry recorded before the controller attributed
					// images: the image with no component claimed for it.
					Images: []*kelsonv1alpha1.ComponentImage{{Image: "ghcr.io/acme/hello:1.1.0"}},
				},
			}}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "history", "-f", spec, "--env", "development", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	revIdx3 := strings.Index(stdout, "3-cccc0000")
	revIdx2 := strings.Index(stdout, "2-bbbb0000")
	if revIdx3 < 0 || revIdx2 < 0 || revIdx3 > revIdx2 {
		t.Errorf("revisions are not printed newest first:\n%s", stdout)
	}
	// Every column comes from a field of its own: the outcome, the digest at
	// reading length, and each image under the component that resolved it.
	if !strings.Contains(stdout, "web=ghcr.io/acme/hello:1.2.0") {
		t.Errorf("stdout does not label the image with its component:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Healthy") || !strings.Contains(stdout, "sha256:"+strings.Repeat("a", 12)) {
		t.Errorf("stdout does not carry the recorded outcome and short digest:\n%s", stdout)
	}
	// A pre-attribution entry prints the image alone rather than under a
	// fabricated label.
	if !strings.Contains(stdout, " ghcr.io/acme/hello:1.1.0") || strings.Contains(stdout, "=ghcr.io/acme/hello:1.1.0") {
		t.Errorf("an unattributed image was labelled:\n%s", stdout)
	}
	// The deprecated prose is not a source for anything on screen.
	if strings.Contains(stdout, "nonsense") {
		t.Errorf("the entry's deprecated message reached stdout:\n%s", stdout)
	}
}

// TestHistorySeparatesWhatOnlyTheRegistryRemembers is issue #241 at the CLI. A
// revision past the cluster's mirror fills one column and no others, so putting
// it in the table would print dashes that read as "this deployment had no
// outcome" instead of "nothing recorded one".
func TestHistorySeparatesWhatOnlyTheRegistryRemembers(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		history: func(context.Context, *kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error) {
			return &kelsonv1alpha1.HistoryResponse{Entries: []*kelsonv1alpha1.HistoryEntry{
				{
					Revision: "3-cccc0000", CommittedAt: "2026-08-15T09:00:00Z",
					Outcome: "Healthy", Digest: "sha256:" + strings.Repeat("a", 64),
				},
				{Revision: "1-aaaa0000", BeyondWindow: true, Message: "older than the 20 entries…"},
			}}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "history", "-f", spec, "--env", "development", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	table, older, split := strings.Cut(stdout, "Older than the cluster's mirror")
	if !split {
		t.Fatalf("the aged-out revision was not called out:\n%s", stdout)
	}
	if strings.Contains(table, "1-aaaa0000") {
		t.Errorf("a registry-only revision was printed in the table:\n%s", stdout)
	}
	if !strings.Contains(older, "1-aaaa0000") {
		t.Errorf("the registry-only revision is missing from its own list:\n%s", stdout)
	}
	// And the reader is told it is still restorable, which is the whole point
	// of listing it.
	if !strings.Contains(older, "rollback --to") {
		t.Errorf("the note does not say the revision can still be restored:\n%s", stdout)
	}
}

func TestHistoryEmptyIsReportedExplicitly(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		history: func(context.Context, *kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error) {
			return &kelsonv1alpha1.HistoryResponse{}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "history", "-f", spec, "--env", "development", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "no revisions recorded") {
		t.Errorf("stdout does not report the empty history explicitly:\n%s", stdout)
	}
}

func TestHistoryProjectRequiresEnv(t *testing.T) {
	_, code, msg := runRoot(t, "history", "--project", "hello")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "--env") {
		t.Errorf("the error should name --env: %s", msg)
	}
}
