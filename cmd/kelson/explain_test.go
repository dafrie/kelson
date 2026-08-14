package main

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// `kelson explain` is the CLI half of ADR-0023. The causal machinery is
// internal/explain's and tested there; what these tests assert is that this
// command resolves all four sources — the adapter's status and history, the
// probe's verdicts and the rendered-history store's recorded manifests — and
// prints the answer whole.

// recordedDeployment renders the fixture's one Deployment with the given env
// entries, in the shape the direct-mode history store keeps.
func recordedDeployment(image, env string) []delivery.Manifest {
	yaml := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: hello-development\n" +
		"spec:\n  template:\n    spec:\n      containers:\n        - name: web\n          image: " + image + "\n"
	if env != "" {
		yaml += "          env:\n" + env
	}
	return []delivery.Manifest{{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Namespace: "hello-development", YAML: []byte(yaml),
	}}
}

// TestExplainNamesTheVariableAndTheRevision is issue #77's acceptance case at
// the CLI: the crash-looping container's own output names DATABASE_URL, the
// recorded manifests show the live revision removed it, and the printed answer
// carries both plus the confidence that says two signals agreed.
func TestExplainNamesTheVariableAndTheRevision(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase:    delivery.PhaseDegraded,
		Revision: "rev-00000043",
		Cause:    "Deployment/hello-development/web: unhealthy",
	}}
	adapter.history = []delivery.Entry{
		{Revision: "rev-00000043", CommittedAt: "2026-08-13T09:20:00Z", Message: "deploy hello development"},
		{Revision: "rev-00000042", CommittedAt: "2026-08-12T17:02:00Z", Message: "deploy hello development"},
	}
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
	recorded := fakeRecorded{byRev: map[string][]delivery.Manifest{
		"rev-00000043": recordedDeployment("ghcr.io/acme/hello:1.4.3", ""),
		"rev-00000042": recordedDeployment("ghcr.io/acme/hello:1.4.2",
			"            - name: DATABASE_URL\n              value: postgres://db/app\n"),
	}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, probe, recorded),
		"explain", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("explain must exit 0 even when the workload is broken; got %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"CAUSES",
		"explain/missing-env-var",
		"[high]",
		"DATABASE_URL",
		"introduced by: rev-00000043",
		"KeyError: 'DATABASE_URL'",
		"RECENT CHANGE",
		"rev-00000042 → rev-00000043",
		"ghcr.io/acme/hello:1.4.2 → ghcr.io/acme/hello:1.4.3",
		"fix:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestExplainWithoutHistoryStillExplains: the correlation is a bonus, not a
// prerequisite. An environment with nothing recorded gets the verdict-derived
// causes and a note saying why there is no change to correlate with.
func TestExplainWithoutHistoryStillExplains(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseDegraded, Revision: "rev-00000001"}}
	probe := fakeProbe{verdicts: map[string]observation.Verdict{"web": {
		Healthy:  false,
		Code:     observation.CodeImagePullBackOff,
		Reason:   `Back-off pulling image "ghcr.io/acme/hello:1.0.0": 401 Unauthorized`,
		Resource: "Deployment/hello-development/web",
	}}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, probe, nil),
		"explain", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{"explain/image-pull", "ghcr.io/acme/hello:1.0.0", "401 Unauthorized", "NOTES"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestExplainWithNoProbeSaysSo: a plane that could not build an observation
// probe read no workload health, and the report says that rather than printing
// an empty CAUSES section that reads as "nothing is wrong".
func TestExplainWithNoProbeSaysSo(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseApplied, Revision: "rev-00000001"}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"explain", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "no observation probe") {
		t.Fatalf("a report with no health signal must say so:\n%s", stdout)
	}
}
