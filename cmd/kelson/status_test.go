package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// crashLoopVerdict is the shape observation.Probe produces for a container the
// kubelet named CrashLoopBackOff: the code is the typed value a machine reads,
// the reason is the kubelet's own string, and the containers carry the logs.
func crashLoopVerdict() observation.Verdict {
	return observation.Verdict{
		Healthy:     false,
		Code:        observation.CodeCrashLoopBackOff,
		Reason:      "CrashLoopBackOff",
		Resource:    "Deployment/development/web",
		Remediation: observation.CodeCrashLoopBackOff.String(),
		Containers: []observation.Container{{
			Name:   "web",
			Pod:    "web-6d4f9c8b7-abcde",
			Code:   observation.CodeCrashLoopBackOff,
			Reason: "CrashLoopBackOff",
			Logs:   "panic: missing DATABASE_URL\n",
		}},
	}
}

// TestStatusPrintsTheVerdict is the harness contract: status exits 0 and the
// verdict text names the kubelet's own reason, so "CrashLoopBackOff" appears
// verbatim rather than as a generic "deployment failed".
func TestStatusPrintsTheVerdict(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase:    delivery.PhaseDegraded,
		Revision: "000003",
		Cause:    "Deployment/development/web: unhealthy",
		Detail:   map[string]string{"resources": "3", "live": "3", "degraded": "1"},
	}}
	probe := fakeProbe{verdicts: map[string]observation.Verdict{"web": crashLoopVerdict()}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, probe, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("status must exit 0 even when the workload is broken; got %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"Degraded",
		"revision 000003",
		"CrashLoopBackOff",
		"crash-loop-back-off",
		"web-6d4f9c8b7-abcde",
		"panic: missing DATABASE_URL",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestStatusSummaryAgreesWithVerdicts is issue #151: a Deployment whose new
// pods crash-loop keeps the previous ReplicaSet alive, so the direct adapter
// reports the rollout as still in flight and counts no degraded resource. The
// summary printed "degraded: 0" directly above a verdict reading "degraded:
// crash-loop-back-off". The counter is now derived from the verdicts, so the
// two halves of the report cannot disagree.
func TestStatusSummaryAgreesWithVerdicts(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase:    delivery.PhaseApplied,
		Revision: "rev-00000007",
		Cause:    "Deployment/hello-development/web: 1 replicas of the previous revision are pending termination",
		Detail:   map[string]string{"resources": "3", "live": "3", "degraded": "0"},
	}}
	probe := fakeProbe{verdicts: map[string]observation.Verdict{"web": crashLoopVerdict()}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, probe, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if strings.Contains(stdout, "degraded:  0") {
		t.Fatalf("the summary must not count zero degraded while a verdict says degraded:\n%s", stdout)
	}
	if !strings.Contains(stdout, "degraded:  1") {
		t.Fatalf("summary should count the degraded workload:\n%s", stdout)
	}
	if got := summaryDegraded(t, stdout); got != countDegradedLines(stdout) {
		t.Fatalf("summary counts %d degraded, verdicts report %d:\n%s", got, countDegradedLines(stdout), stdout)
	}
}

// TestStatusSummaryKeepsAdapterDegradedCount: the adapter judges kinds the
// observation probe never classifies (CronJobs), so its count is a floor the
// summary must not lose when no workload verdict is failing.
func TestStatusSummaryKeepsAdapterDegradedCount(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase:    delivery.PhaseDegraded,
		Revision: "000009",
		Cause:    "CronJob/hello-development/report: no successful run",
		Detail:   map[string]string{"resources": "3", "live": "3", "degraded": "2"},
	}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, fakeProbe{}, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if got := summaryDegraded(t, stdout); got != 2 {
		t.Fatalf("summary degraded = %d, want the adapter's 2:\n%s", got, stdout)
	}
}

// TestStatusSummaryOmitsDegradedWithoutASignal: an adapter that reports no
// degraded count (a Proposed readback) with no failing verdict prints no
// counter, rather than inventing a zero the adapter never claimed.
func TestStatusSummaryOmitsDegradedWithoutASignal(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase:    delivery.PhaseProposed,
		Revision: "000001",
		Detail:   map[string]string{"resources": "3", "live": "0"},
	}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, fakeProbe{}, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if strings.Contains(stdout, "degraded:") {
		t.Fatalf("no plane reported a degraded resource, so no counter belongs in the summary:\n%s", stdout)
	}
}

// summaryDegraded reads the summary's degraded counter out of the report.
func summaryDegraded(t *testing.T, stdout string) int {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "degraded:") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "degraded:")))
		if err != nil {
			t.Fatalf("unparseable degraded counter %q: %v", line, err)
		}
		return n
	}
	t.Fatalf("no degraded counter in the summary:\n%s", stdout)
	return 0
}

// countDegradedLines counts the verdict lines that call a workload degraded.
// Verdict lines start with the resource they are about, which is what keeps the
// summary's own counter out of the count.
func countDegradedLines(stdout string) int {
	n := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "Deployment/") && strings.Contains(line, " degraded: ") {
			n++
		}
	}
	return n
}

// TestStatusHealthyReportsBothPlanes: the phase says the change arrived, the
// verdict says it works. Printing one without the other is the conflation
// issue #53 exists to prevent.
func TestStatusHealthyReportsBothPlanes(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseHealthy, Revision: "000007"}}

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, fakeProbe{}, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	if !strings.Contains(stdout, "Healthy") || !strings.Contains(stdout, "revision 000007") {
		t.Fatalf("stdout should report the delivery phase:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Deployment/hello-development/web healthy") {
		t.Fatalf("stdout should report the observation verdict:\n%s", stdout)
	}
}

// TestStatusProbesTheRenderedWorkloads: the rendered set is the correlation —
// status asks about exactly the workloads kelson declares for this project and
// environment, in the namespace it renders them into.
func TestStatusProbesTheRenderedWorkloads(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	set, err := func() (delivery.ManifestSet, error) {
		_, s, err := resolveDeliveryTarget([]string{spec}, "development", "", "", history, "")
		return s, err
	}()
	if err != nil {
		t.Fatalf("resolving the target: %v", err)
	}
	if got := deployments(set, "fallback"); len(got) != 1 || got[0].name != "web" || got[0].namespace != "hello-development" {
		t.Fatalf("expected the rendered Deployment web, got %+v", got)
	}

	stdout, code, _ := runDelivery(t, planeOf([]delivery.Adapter{adapter}, fakeProbe{}, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
}

// TestStatusAdapterErrorFails: a status readback that cannot reach the cluster
// is a real error, never a clean "unknown" that reads as fine.
func TestStatusAdapterErrorFails(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statusErr = errors.New("connection refused")
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, fakeProbe{}, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitErr || !strings.Contains(msg, "connection refused") {
		t.Fatalf("expected the readback error, got %d: %s", code, msg)
	}
}

// TestStatusWithoutProbeSaysSo: a missing observation probe is reported, not
// silently rendered as "no failing workloads".
func TestStatusWithoutProbeSaysSo(t *testing.T) {
	spec, history := deploySpec(t)
	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{newFakeAdapter("direct")}, nil, nil),
		"status", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	if !strings.Contains(stdout, "no observation probe available") {
		t.Fatalf("stdout should say health was not read:\n%s", stdout)
	}
}
