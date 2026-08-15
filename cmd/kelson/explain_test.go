package main

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/observation"
)

// `kelson explain` is the CLI half of ADR-0023. The causal machinery is
// internal/explain's and tested there; what these tests assert is that this
// command resolves the sources it still has, prints the answer whole, and says
// out loud which sources it does not.
//
// It has four sources in principle and reads two: the workload verdicts and
// their logs, both read straight from the cluster. The delivery phase and the
// revision correlation are `Environment.status` (ADR-0028), and this command
// has no client for a stored `Environment` — it composes internal/explain
// locally against the cluster, by design (see explain.go's package comment) —
// so those two arrive as notes rather than as an empty CAUSES section that
// would read as "nothing is wrong". Issue #224 closed that gap for
// ExplainService (internal/api/explain_test.go covers the revision
// correlation this command still cannot make); the notes here say why this
// command is still the one to lose it, not that it is still missing anywhere.

// TestExplainNamesTheCauseFromTheVerdict is issue #77's acceptance case at what
// the CLI can still see: the crash-looping container's own output names
// DATABASE_URL, and the printed answer carries it with the code and the fix.
func TestExplainNamesTheCauseFromTheVerdict(t *testing.T) {
	spec := deploySpec(t)
	probe := fakeProbe{verdicts: map[string]observation.Verdict{"web": {
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Reason:   "CrashLoopBackOff",
		Resource: "Deployment/hello-development/web",
		Containers: []observation.Container{{
			Name: "web", Pod: "web-6d4f-abcde", Code: observation.CodeCrashLoopBackOff, Reason: "CrashLoopBackOff",
			Logs: "starting hello\nKeyError: 'DATABASE_URL'\n",
		}},
	}}}

	stdout, code, msg := runDelivery(t, planeOf(probe), "explain", "-f", spec, "--env", "development")
	if code != exitOK {
		t.Fatalf("explain must exit 0 even when the workload is broken; got %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"CAUSES",
		"DATABASE_URL",
		"KeyError: 'DATABASE_URL'",
		"fix:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// The two sources this command does not read must be named in the report. A
// diagnosis that quietly stopped consulting an input would read exactly like
// one that consulted it and found nothing, and the reader has no way to tell
// them apart.
func TestExplainNamesTheSourcesItNoLongerHas(t *testing.T) {
	spec := deploySpec(t)
	probe := fakeProbe{verdicts: map[string]observation.Verdict{"web": {
		Healthy:  false,
		Code:     observation.CodeImagePullBackOff,
		Reason:   `Back-off pulling image "ghcr.io/acme/hello:1.0.0": 401 Unauthorized`,
		Resource: "Deployment/hello-development/web",
	}}}

	stdout, code, msg := runDelivery(t, planeOf(probe), "explain", "-f", spec, "--env", "development")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"explain/image-pull", "ghcr.io/acme/hello:1.0.0", "401 Unauthorized",
		"NOTES", "delivery phase", "no change was correlated", "Environment.status",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "#224") {
		t.Fatalf("stdout still cites issue #224 as if the revision-correlation gate were unresolved:\n%s", stdout)
	}
}

// TestExplainWithNoProbeSaysSo: a plane that could not build an observation
// probe read no workload health, and the report says that rather than printing
// an empty CAUSES section that reads as "nothing is wrong".
func TestExplainWithNoProbeSaysSo(t *testing.T) {
	spec := deploySpec(t)
	stdout, code, msg := runDelivery(t, planeOf(nil), "explain", "-f", spec, "--env", "development")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "no observation probe") {
		t.Fatalf("a report with no health signal must say so:\n%s", stdout)
	}
}
