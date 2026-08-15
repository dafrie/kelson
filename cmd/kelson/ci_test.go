package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// runCI executes the root command and returns stderr as well, which the other
// helpers drop. `kelson ci report-build` splits its answer across both streams
// on purpose — identifiers on stdout, explanation on stderr — so a test that
// could only see one of them could not tell the split had happened.
func runCI(t *testing.T, args ...string) (stdout, stderr string, code int, msg string) {
	t.Helper()
	cmd := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), errBuf.String(), code, msg
}

const reportSHA = "0123456789abcdef0123456789abcdef01234567"

func TestReportBuildSendsWhatCIKnowsAndPrintsTriggered(t *testing.T) {
	fake := &fakeBuildService{
		report: func(_ context.Context, req *kelsonv1alpha1.ReportBuildRequest) (*kelsonv1alpha1.ReportBuildResponse, error) {
			if req.GetProject() != "checkout" || req.GetSha() != reportSHA {
				t.Errorf("unexpected join key: project=%q sha=%q", req.GetProject(), req.GetSha())
			}
			if req.GetPr() != 412 {
				t.Errorf("pr = %d, want 412", req.GetPr())
			}
			if req.GetRef() != "refs/heads/feature" {
				t.Errorf("ref = %q", req.GetRef())
			}
			want := map[string]string{
				"web":    "ghcr.io/acme/checkout-web@sha256:abc",
				"worker": "ghcr.io/acme/checkout-worker@sha256:def",
			}
			for component, reference := range want {
				if got := req.GetImages()[component]; got != reference {
					t.Errorf("images[%q] = %q, want %q", component, got, reference)
				}
			}
			if len(req.GetImages()) != len(want) {
				t.Errorf("images = %v, want exactly %v", req.GetImages(), want)
			}
			// A report's effect is not a function of its arguments, so a retry
			// has to be recognisable as the same report (#71).
			if req.GetIdempotencyKey() == "" {
				t.Error("the report carries no idempotency key")
			}
			return &kelsonv1alpha1.ReportBuildResponse{
				Accepted:  true,
				Triggered: []string{"checkout-staging-pr412", "checkout-qa-pr412"},
				Message:   "published into two environments",
			}, nil
		},
	}
	addr := serveFakeBuildService(t, fake)

	stdout, stderr, code, msg := runCI(t, "ci", "report-build",
		"--project", "checkout", "--sha", reportSHA, "--pr", "412",
		"--ref", "refs/heads/feature",
		"--image", "web=ghcr.io/acme/checkout-web@sha256:abc",
		"--image", "worker=ghcr.io/acme/checkout-worker@sha256:def",
		"--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s\n%s", code, msg, stdout, stderr)
	}
	// stdout is the machine's half and nothing else: one triggered identifier
	// per line, in the order the server listed them.
	if stdout != "checkout-staging-pr412\ncheckout-qa-pr412\n" {
		t.Errorf("stdout is not the triggered identifiers alone:\n%q", stdout)
	}
	if !strings.Contains(stderr, "published into two environments") {
		t.Errorf("the server's explanation did not reach stderr:\n%s", stderr)
	}
}

// TestReportBuildNoPreviewCandidateExitsZero is the case the whole exit-code
// contract exists for: a repository no environment previews must not turn a
// pipeline red.
func TestReportBuildNoPreviewCandidateExitsZero(t *testing.T) {
	fake := &fakeBuildService{
		report: func(context.Context, *kelsonv1alpha1.ReportBuildRequest) (*kelsonv1alpha1.ReportBuildResponse, error) {
			return &kelsonv1alpha1.ReportBuildResponse{
				Accepted: true,
				Message:  "no environment of checkout declares previews for this repository",
			}, nil
		},
	}
	addr := serveFakeBuildService(t, fake)

	stdout, stderr, code, msg := runCI(t, "ci", "report-build",
		"--project", "checkout", "--sha", reportSHA, "--pr", "7",
		"--image", "web=ghcr.io/acme/checkout-web@sha256:abc",
		"--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0: nothing to preview is not a failure\n%s", code, msg, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty when nothing was triggered, got %q", stdout)
	}
	if !strings.Contains(stderr, "nothing was triggered") ||
		!strings.Contains(stderr, "no environment of checkout declares previews") {
		t.Errorf("stderr does not say what happened and why:\n%s", stderr)
	}
}

// TestReportBuildDeclinedFails covers `accepted: false`, the schema's one
// meaning: kelson understood the report and will not act on it, because this
// project's images come from kelson's own build plane.
func TestReportBuildDeclinedFails(t *testing.T) {
	declined := "Project checkout declares spec.build.by: kelson, so kelson's own build plane produces its images"
	fake := &fakeBuildService{
		report: func(context.Context, *kelsonv1alpha1.ReportBuildRequest) (*kelsonv1alpha1.ReportBuildResponse, error) {
			return &kelsonv1alpha1.ReportBuildResponse{Message: declined}, nil
		},
	}
	addr := serveFakeBuildService(t, fake)

	stdout, _, code, msg := runCI(t, "ci", "report-build",
		"--project", "checkout", "--sha", reportSHA, "--pr", "412",
		"--image", "web=ghcr.io/acme/checkout-web@sha256:abc",
		"--server", addr)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d for a declined report", code, exitErr)
	}
	if !strings.Contains(msg, declined) {
		t.Errorf("the refusal does not carry the server's reason: %s", msg)
	}
	if stdout != "" {
		t.Errorf("a declined report printed to stdout: %q", stdout)
	}
}

// TestReportBuildRefOnlyIsReportedAsAGap is the `ref`-only path: the RPC is
// Unimplemented today, and the command must say so as a gap in kelson rather
// than as something the pipeline got wrong — while passing the server's own
// sentence and its taxonomy code through untouched.
func TestReportBuildRefOnlyIsReportedAsAGap(t *testing.T) {
	fake := &fakeBuildService{
		report: func(context.Context, *kelsonv1alpha1.ReportBuildRequest) (*kelsonv1alpha1.ReportBuildResponse, error) {
			cerr := connect.NewError(connect.CodeUnimplemented,
				errors.New("kelson accepts this report's images but has nowhere to send them"))
			detail, err := connect.NewErrorDetail(&kelsonv1alpha1.Error{
				Code:        "delivery/not-implemented",
				Message:     "Environment.spec.autoDeploy is not in the model yet",
				Remediation: "this capability returns with issue #248",
			})
			if err != nil {
				t.Fatalf("building the error detail: %v", err)
			}
			cerr.AddDetail(detail)
			return nil, cerr
		},
	}
	addr := serveFakeBuildService(t, fake)

	stdout, _, code, msg := runCI(t, "ci", "report-build",
		"--project", "checkout", "--sha", reportSHA, "--ref", "refs/heads/main",
		"--image", "web=ghcr.io/acme/checkout-web@sha256:abc",
		"--server", addr)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "nothing is wrong with this report") {
		t.Errorf("an unimplemented half reads as a pipeline mistake: %s", msg)
	}
	for _, want := range []string{
		"nowhere to send them",
		"delivery/not-implemented",
		"issue #248",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the server's own answer did not ride through: %q missing from %s", want, msg)
		}
	}
	if stdout != "" {
		t.Errorf("a failed report printed to stdout: %q", stdout)
	}
}

// TestReportBuildCarriesTheAgentCredential is ADR-0034 decision 6 at this
// command's boundary: --token is what a pipeline is given, and it has to arrive.
func TestReportBuildCarriesTheAgentCredential(t *testing.T) {
	fake := &fakeBuildService{
		report: func(context.Context, *kelsonv1alpha1.ReportBuildRequest) (*kelsonv1alpha1.ReportBuildResponse, error) {
			return &kelsonv1alpha1.ReportBuildResponse{Accepted: true, Triggered: []string{"checkout-staging-pr412"}}, nil
		},
	}
	addr := serveFakeBuildService(t, fake)

	_, _, code, msg := runCI(t, "ci", "report-build",
		"--project", "checkout", "--sha", reportSHA, "--pr", "412",
		"--image", "web=ghcr.io/acme/checkout-web@sha256:abc",
		"--server", addr, "--token", "agent-token", "--password", "shared-password")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	// The agent identity wins over the shared password, for the reason
	// serverOptions.credential states: an identity configured with a token must
	// act as itself, never as whoever also knows the password.
	if fake.authorization != "Bearer agent-token" {
		t.Errorf("Authorization = %q, want the agent credential", fake.authorization)
	}
}

func TestReportBuildUnreachableServerNamesTheFlag(t *testing.T) {
	_, _, code, msg := runCI(t, "ci", "report-build",
		"--project", "checkout", "--sha", reportSHA, "--pr", "412",
		"--image", "web=ghcr.io/acme/checkout-web@sha256:abc",
		"--server", unreachableServerAddr(t))
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "--server") {
		t.Errorf("an unreachable server must name the flag that fixes it: %s", msg)
	}
}

// TestReportBuildImageSpelling covers the one judgement this command makes
// about a report: `component=reference` exists only at this boundary, so a
// malformed one has to be refused here or nowhere.
func TestReportBuildImageSpelling(t *testing.T) {
	tests := []struct {
		name   string
		images []string
		want   string
	}{
		{"no separator", []string{"ghcr.io/acme/web@sha256:abc"}, "component=reference"},
		{"no component", []string{"=ghcr.io/acme/web@sha256:abc"}, "component=reference"},
		{"no reference", []string{"web="}, "component=reference"},
		{
			"one component twice",
			[]string{"web=ghcr.io/acme/web@sha256:abc", "web=ghcr.io/acme/web@sha256:def"},
			"twice",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"ci", "report-build", "--project", "checkout", "--sha", reportSHA, "--pr", "1"}
			for _, image := range tc.images {
				args = append(args, "--image", image)
			}
			// No --server: a refusal that needed one would mean the request was
			// built before it was judged.
			_, _, code, msg := runCI(t, args...)
			if code != exitErr {
				t.Fatalf("exit = %d, want %d", code, exitErr)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error should contain %q: %s", tc.want, msg)
			}
		})
	}
}

func TestReportBuildRequiresItsJoinKey(t *testing.T) {
	for _, missing := range []string{"project", "sha", "image"} {
		t.Run(missing, func(t *testing.T) {
			args := []string{"ci", "report-build"}
			if missing != "project" {
				args = append(args, "--project", "checkout")
			}
			if missing != "sha" {
				args = append(args, "--sha", reportSHA)
			}
			if missing != "image" {
				args = append(args, "--image", "web=ghcr.io/acme/web@sha256:abc")
			}
			_, _, code, msg := runCI(t, args...)
			if code != exitErr {
				t.Fatalf("exit = %d, want %d", code, exitErr)
			}
			if !strings.Contains(msg, missing) {
				t.Errorf("the error should name --%s: %s", missing, msg)
			}
		})
	}
}

// TestCIGroupIsNotItselfAVerb keeps the parent a grouping rather than something
// runnable, the same shape `kelson preview` and `kelson agent` have.
func TestCIGroupIsNotItselfAVerb(t *testing.T) {
	stdout, _, code, msg := runCI(t, "ci")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	if !strings.Contains(stdout, "report-build") {
		t.Errorf("`kelson ci` should list its subcommands:\n%s", stdout)
	}
}
