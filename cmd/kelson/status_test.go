package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// `kelson status` answers "is my change live, and if not, why" in two halves.
//
// The observation half — whether it WORKS — needs no adapter and no server; it
// reads the cluster directly and is what most of these tests cover. The
// delivery half — whether the change ARRIVED — is a ConnectRPC client of
// DeployService.Status (R2, #225), tested the same way deploy/rollback/
// promote/history are (facade_test.go's fakeDeployService), plus the one
// property a missing server adds: an unreachable --server degrades that half
// to a "not reported" line naming the flag, rather than failing the command —
// the workload half is the one people run this command in an incident to ask.

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
	spec := deploySpec(t)
	probe := fakeProbe{verdicts: map[string]observation.Verdict{"web": crashLoopVerdict()}}

	stdout, code, msg := runDelivery(t, planeOf(probe), "status", "-f", spec, "--env", "development")
	if code != exitOK {
		t.Fatalf("status must exit 0 even when the workload is broken; got %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"CrashLoopBackOff",
		"crash-loop-back-off",
		"web-6d4f9c8b7-abcde",
		"panic: missing DATABASE_URL",
		"degraded:  1",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// The delivery-phase line is printed whether or not the workload half is
// wrong, and — with no reachable server, the default in these tests — it
// degrades to "not reported" naming --server rather than failing the command.
// A gap that only announces itself on failure is a gap the reader meets at
// the worst possible moment.
func TestStatusDeliveryPhaseDegradesWithoutServer(t *testing.T) {
	spec := deploySpec(t)
	for _, tc := range []struct {
		name  string
		probe observation.Evaluator
	}{
		{"healthy", fakeProbe{}},
		{"degraded", fakeProbe{verdicts: map[string]observation.Verdict{"web": crashLoopVerdict()}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, code, msg := runDelivery(t, planeOf(tc.probe), "status", "-f", spec, "--env", "development",
				"--server", unreachableServerAddr(t))
			if code != exitOK {
				t.Fatalf("exit = %d (%s)", code, msg)
			}
			for _, want := range []string{"delivery phase: not reported", "--server", "KELSON_SERVER"} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("stdout missing %q:\n%s", want, stdout)
				}
			}
		})
	}
}

// TestStatusReportsTheDeliveryPhase: a reachable server's phase, revision and
// cause appear in the same line, even when the workload half is unrelated.
func TestStatusReportsTheDeliveryPhase(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		status: func(_ context.Context, req *kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			if req.GetEnvironment() != "development" {
				t.Fatalf("unexpected environment %q", req.GetEnvironment())
			}
			return &kelsonv1alpha1.StatusResponse{
				Phase:    "Healthy",
				Revision: "3-cccc0000",
				Cause:    "Ready",
			}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runDelivery(t, planeOf(fakeProbe{}), "status", "-f", spec, "--env", "development", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, msg)
	}
	for _, want := range []string{"delivery phase:", "Healthy", "3-cccc0000", "Ready"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestStatusDeliveryPhaseStaleReportsBothGenerations pins the server's own
// staleness wording (internal/api/deploy.go's reportDelivery, 43ccaf4)
// through unchanged: a status still describing an older generation than the
// spec says so, inline with the cause.
func TestStatusDeliveryPhaseStaleReportsBothGenerations(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		status: func(context.Context, *kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return &kelsonv1alpha1.StatusResponse{
				Phase:    "Progressing",
				Revision: "2-bbbb0000",
				Cause:    "Progressing (status is at generation 2, the spec is at 3)",
			}, nil
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runDelivery(t, planeOf(fakeProbe{}), "status", "-f", spec, "--env", "development", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, msg)
	}
	if !strings.Contains(stdout, "status is at generation 2, the spec is at 3") {
		t.Fatalf("stdout does not carry the server's staleness wording:\n%s", stdout)
	}
}

// TestStatusDeliveryPhaseServerErrorStillReportsWorkloads: a server that
// rejects the credential must not take the workload half down with it — the
// two questions fail independently (issue #53), and status still exits 0.
func TestStatusDeliveryPhaseServerErrorStillReportsWorkloads(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		status: func(context.Context, *kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("bad credential"))
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runDelivery(t, planeOf(fakeProbe{verdicts: map[string]observation.Verdict{"web": crashLoopVerdict()}}),
		"status", "-f", spec, "--env", "development", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, msg)
	}
	for _, want := range []string{"delivery phase: not reported", "--password", "CrashLoopBackOff"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestStatusProbesTheRenderedWorkloads: the rendered set is the correlation,
// and the probe is asked about exactly what the spec declares.
func TestStatusProbesTheRenderedWorkloads(t *testing.T) {
	spec := deploySpec(t)
	var seen observationTarget
	probe := fakeProbe{}
	stdout, code, msg := runDelivery(t, capturingConnector(planeOf(probe), &seen),
		"status", "-f", spec, "--env", "development")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if seen.project != "hello" || seen.environment != "development" {
		t.Errorf("target = %+v, want the spec's project and environment", seen)
	}
	if seen.namespace != "hello-development" {
		t.Errorf("namespace = %q, want the model's derived default", seen.namespace)
	}
}

// A probe that fails is a failed command: status reports what the cluster says,
// and a cluster it could not read is not an answer about the workloads.
func TestStatusProbeErrorFails(t *testing.T) {
	spec := deploySpec(t)
	probe := fakeProbe{err: errors.New("the API server is unreachable")}
	_, code, msg := runDelivery(t, planeOf(probe), "status", "-f", spec, "--env", "development")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "unreachable") {
		t.Errorf("error %q does not carry the probe's cause", msg)
	}
}

func TestStatusWithoutProbeSaysSo(t *testing.T) {
	spec := deploySpec(t)
	stdout, code, msg := runDelivery(t, planeOf(nil), "status", "-f", spec, "--env", "development")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	if !strings.Contains(stdout, "no observation probe available") {
		t.Fatalf("stdout should say health was not read:\n%s", stdout)
	}
}

// syncingProbe is a fakeProbe that also answers sync questions, standing in for
// the real observation.Probe's optional SecretSyncEvaluator capability.
type syncingProbe struct {
	fakeProbe
	sync map[string]observation.Verdict
	seen []string
}

func (s *syncingProbe) EvaluateSecretSync(_ context.Context, namespace, name string) (observation.Verdict, error) {
	s.seen = append(s.seen, name)
	if v, ok := s.sync[name]; ok {
		return v, nil
	}
	return observation.Verdict{
		Healthy:  true,
		Code:     observation.CodeHealthy,
		Resource: "external-secrets.io/ExternalSecret/" + namespace + "/" + name,
	}, nil
}

// TestStatusReportsSecretSyncFailures is issue #80's acceptance criterion at
// the surface a human reads: a Secret that failed to sync is an
// component-level problem with the cause named, and it is reported above the
// workloads it broke rather than left for someone to infer from a
// CreateContainerConfigError.
func TestStatusReportsSecretSyncFailures(t *testing.T) {
	set := delivery.ManifestSet{Manifests: []delivery.Manifest{
		{Kind: "ExternalSecret", Name: "payments", Namespace: "shop-prod"},
		{Kind: "Deployment", Name: "web", Namespace: "shop-prod"},
	}}
	probe := &syncingProbe{
		sync: map[string]observation.Verdict{"payments": {
			Healthy:     false,
			Code:        observation.CodeSecretSyncFailed,
			Reason:      `SecretSyncedError: cannot get secret "payments": permission denied`,
			Resource:    "external-secrets.io/ExternalSecret/shop-prod/payments",
			Remediation: "check the SecretStore",
		}},
	}

	verdicts, err := workloadVerdicts(context.Background(), &observationPlane{health: probe}, set, "fallback")
	if err != nil {
		t.Fatalf("workloadVerdicts: %v", err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("verdicts = %+v, want one per ExternalSecret and Deployment", verdicts)
	}
	if verdicts[0].Code != observation.CodeSecretSyncFailed {
		t.Fatalf("the sync verdict must come first — the cause above the symptom; got %+v", verdicts)
	}
	if !isDegraded(verdicts[0]) {
		t.Errorf("a failed sync must count as degraded, not as a wait state")
	}
	if !strings.Contains(verdicts[0].String(), "permission denied") {
		t.Errorf("the summary must carry the controller's cause: %q", verdicts[0].String())
	}
	if len(probe.seen) != 1 || probe.seen[0] != "payments" {
		t.Errorf("asked about %v, want exactly the rendered ExternalSecret", probe.seen)
	}
}

// TestStatusWithoutASyncEvaluatorStillWorks: the sync capability is optional,
// so an Evaluator that only classifies workloads keeps reporting exactly what
// it did before rather than failing the whole status readback.
func TestStatusWithoutASyncEvaluatorStillWorks(t *testing.T) {
	set := delivery.ManifestSet{Manifests: []delivery.Manifest{
		{Kind: "ExternalSecret", Name: "payments", Namespace: "shop-prod"},
		{Kind: "Deployment", Name: "web", Namespace: "shop-prod"},
	}}
	verdicts, err := workloadVerdicts(context.Background(), &observationPlane{health: fakeProbe{}}, set, "fallback")
	if err != nil {
		t.Fatalf("workloadVerdicts: %v", err)
	}
	if len(verdicts) != 1 || verdicts[0].Resource != "Deployment/shop-prod/web" {
		t.Fatalf("verdicts = %+v, want the Deployment alone", verdicts)
	}
}

// TestExternalSecretsPicksTheRenderedOnes: the rendered set is the correlation,
// and the fallback namespace applies only where the manifest carries none.
func TestExternalSecretsPicksTheRenderedOnes(t *testing.T) {
	set := delivery.ManifestSet{Manifests: []delivery.Manifest{
		{Kind: "Namespace", Name: "shop-prod"},
		{Kind: "ExternalSecret", Name: "payments", Namespace: "shop-prod"},
		{Kind: "ExternalSecret", Name: "mail-relay"},
		{Kind: "Deployment", Name: "web", Namespace: "shop-prod"},
	}}
	got := externalSecrets(set, "fallback")
	want := []workloadRef{
		{namespace: "shop-prod", name: "payments"},
		{namespace: "fallback", name: "mail-relay"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
