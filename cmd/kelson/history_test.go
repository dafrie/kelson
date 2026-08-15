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
				{Revision: "3-cccc0000", CommittedAt: "2026-08-15T09:00:00Z", Message: "healthy · serving · ghcr.io/acme/hello:1.2.0"},
				{Revision: "2-bbbb0000", CommittedAt: "2026-08-14T09:00:00Z", Message: "healthy · ghcr.io/acme/hello:1.1.0"},
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
	if !strings.Contains(stdout, "1.2.0") {
		t.Errorf("stdout does not carry the entry's detail message:\n%s", stdout)
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
