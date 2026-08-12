package main

import (
	"errors"
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
