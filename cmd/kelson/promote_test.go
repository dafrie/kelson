package main

import (
	"context"
	"strings"
	"testing"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// `kelson promote` addresses a project already stored on kelson-server (R2,
// issue #225): there is no `-f` form, because a promotion writes a spec
// document and an inline write has nowhere to land
// (proto/kelson/v1alpha1/deploy.proto, PromoteRequest). What is asserted here
// is the plan/confirm/write sequence and the two refusals that must never
// reach the server at all.

func promotedComponent(name, from, to string) *kelsonv1alpha1.PromotedComponent {
	return &kelsonv1alpha1.PromotedComponent{
		Component: name, FromImage: from, ToImage: to,
		Status: kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED,
	}
}

func TestPromoteWritesPinsAfterConfirmation(t *testing.T) {
	var renderCalls, writeCalls int
	fake := &fakeDeployService{
		promote: func(_ context.Context, req *kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			if req.GetProject() != "hello" || req.GetFromEnvironment() != "staging" || req.GetToEnvironment() != "production" {
				t.Fatalf("unexpected request: %+v", req)
			}
			res := &kelsonv1alpha1.PromoteResponse{
				FromRevision: "9-deadbeef",
				Components:   []*kelsonv1alpha1.PromotedComponent{promotedComponent("web", "ghcr.io/acme/hello:1.0.0", "ghcr.io/acme/hello:1.1.0")},
			}
			switch req.GetDryRun() {
			case kelsonv1alpha1.DryRun_DRY_RUN_RENDER:
				renderCalls++
			case kelsonv1alpha1.DryRun_DRY_RUN_NONE:
				writeCalls++
				res.Version = "42"
			}
			return res, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "promote", "--project", "hello", "--from", "staging", "--to", "production", "--yes", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if renderCalls != 1 || writeCalls != 1 {
		t.Fatalf("render calls = %d, write calls = %d, want 1 and 1", renderCalls, writeCalls)
	}
	for _, want := range []string{"web", "pinned", "1.0.0", "1.1.0", "Wrote 1 pin", "spec version 42", "kelson deploy --project hello --env production"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestPromoteDryRunWritesNothing(t *testing.T) {
	var writeCalls int
	fake := &fakeDeployService{
		promote: func(_ context.Context, req *kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			if req.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_NONE {
				writeCalls++
			}
			return &kelsonv1alpha1.PromoteResponse{
				Components: []*kelsonv1alpha1.PromotedComponent{promotedComponent("web", "", "ghcr.io/acme/hello:1.1.0")},
			}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "promote", "--project", "hello", "--from", "staging", "--to", "production", "--dry-run", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if writeCalls != 0 {
		t.Errorf("--dry-run made a write call")
	}
	if !strings.Contains(stdout, "Dry run: nothing was written.") {
		t.Errorf("stdout does not report a dry run:\n%s", stdout)
	}
}

// A promotion the server refuses (validation/render findings) must write
// nothing — the plan call surfaces the errors and the command stops there,
// never reaching a second, writing call.
func TestPromoteRefusedByServerWritesNothing(t *testing.T) {
	var writeCalls int
	fake := &fakeDeployService{
		promote: func(_ context.Context, req *kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			if req.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_NONE {
				writeCalls++
			}
			return &kelsonv1alpha1.PromoteResponse{
				Errors: []*kelsonv1alpha1.Error{{Code: "promote/not-in-revision", Message: "staging has not deployed web"}},
			}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	_, code, msg := runRootStdin(t, "", "promote", "--project", "hello", "--from", "staging", "--to", "production", "--yes", "--server", addr)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d (%s)", code, exitErr, msg)
	}
	if writeCalls != 0 {
		t.Errorf("a refused promotion made a write call")
	}
}

// The refusals that are about the request rather than about the server come
// first, and never reach the server at all: promoting an environment to
// itself is a mistake the caller can fix without an RPC round trip.
func TestPromoteToItselfIsRefusedBeforeAnyCall(t *testing.T) {
	called := false
	fake := &fakeDeployService{
		promote: func(context.Context, *kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			called = true
			return &kelsonv1alpha1.PromoteResponse{}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	_, code, msg := runRootStdin(t, "", "promote", "--project", "hello", "--from", "staging", "--to", "staging", "--yes", "--server", addr)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "itself") {
		t.Errorf("error %q does not explain the contradiction", msg)
	}
	if called {
		t.Errorf("promoting to itself reached the server; it should be refused client-side")
	}
}
