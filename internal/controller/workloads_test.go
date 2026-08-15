package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
)

// The workload readback (issue #240): ClusterWorkloads against a fake reader,
// and the phase refinement against the real deliverer.
//
// What is worth pinning here is everything the readback *decides*: which
// objects it asks for, what it counts, what it puts in the bounded list and in
// which order, what it refuses to carry across from a verdict, and — the half
// most likely to be got wrong by a later edit — when it is allowed to move the
// phase and when it is not.

// fakeWorkloads is a [WorkloadReader] over fixtures, recording what it was
// asked for so a test can assert the *query* and not only the answer.
type fakeWorkloads struct {
	deployments []*unstructured.Unstructured
	// pods is keyed by the selector the caller passes, rendered as a sorted
	// string, so a test can give different pods to different Deployments.
	pods map[string][]*unstructured.Unstructured
	err  error

	calls []workloadCall
}

type workloadCall struct {
	kind      string
	namespace string
	match     map[string]string
}

func (f *fakeWorkloads) List(_ context.Context, listGVK schema.GroupVersionKind, namespace string, match map[string]string) ([]*unstructured.Unstructured, error) {
	f.calls = append(f.calls, workloadCall{kind: listGVK.Kind, namespace: namespace, match: match})
	if f.err != nil {
		return nil, f.err
	}
	if listGVK.Kind == deploymentGVK.Kind {
		return f.deployments, nil
	}
	return f.pods[selectorKey(match)], nil
}

func selectorKey(match map[string]string) string {
	parts := make([]string, 0, len(match))
	for k, v := range match {
		parts = append(parts, k+"="+v)
	}
	// One label per fixture selector, so no sort is needed; a fixture with two
	// would be the test's own bug and this makes it visible.
	if len(parts) > 1 {
		panic("fixture selectors carry one label")
	}
	return strings.Join(parts, ",")
}

func fixtureDeployment(namespace, name, selectorValue string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":       name,
			"namespace":  namespace,
			"generation": int64(1),
			"labels": map[string]any{
				delivery.LabelManagedBy:   delivery.ManagedByKelson,
				delivery.LabelProject:     "checkout",
				delivery.LabelEnvironment: "production",
			},
		},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": selectorValue}},
		},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"conditions": []any{
				map[string]any{"type": "Available", "status": "True"},
			},
		},
	}}
	return u
}

// fixturePod builds a pod whose single container is in the given waiting state,
// or ready when reason is empty.
func fixturePod(namespace, name, container, waitingReason string) *unstructured.Unstructured {
	status := map[string]any{
		"conditions": []any{
			map[string]any{"type": "PodScheduled", "status": "True"},
			map[string]any{"type": "Ready", "status": "True"},
		},
		"containerStatuses": []any{
			map[string]any{
				"name":  container,
				"ready": true,
				"state": map[string]any{"running": map[string]any{}},
			},
		},
	}
	if waitingReason != "" {
		status["conditions"] = []any{
			map[string]any{"type": "PodScheduled", "status": "True"},
			map[string]any{"type": "Ready", "status": "False"},
		}
		status["containerStatuses"] = []any{
			map[string]any{
				"name":  container,
				"ready": false,
				"state": map[string]any{"waiting": map[string]any{
					"reason":  waitingReason,
					"message": waitingReason + " detail",
				}},
			},
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"status":     status,
	}}
}

func observeFixture(t *testing.T, reader *fakeWorkloads) *v1alpha1.WorkloadsStatus {
	t.Helper()
	got, err := ClusterWorkloads{Reader: reader}.Observe(context.Background(), Revision{
		Project: "checkout", Environment: "production", TargetNamespace: "checkout-production",
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return got
}

// TestObserveSelectsKelsonsOwnWorkloads pins the query, which is the half a
// reader of the status can never check. An environment's target namespace may
// hold workloads kelson did not apply — adoption is the whole premise of
// ADR-0003 — and a readback that listed the namespace would report a
// neighbour's broken Deployment as this environment's.
func TestObserveSelectsKelsonsOwnWorkloads(t *testing.T) {
	reader := &fakeWorkloads{
		deployments: []*unstructured.Unstructured{fixtureDeployment("checkout-production", "web", "web")},
		pods: map[string][]*unstructured.Unstructured{
			"app=web": {fixturePod("checkout-production", "web-0", "app", "")},
		},
	}
	observeFixture(t, reader)

	if len(reader.calls) != 2 {
		t.Fatalf("made %d list calls, want one for the Deployments and one for the pods: %+v",
			len(reader.calls), reader.calls)
	}
	deployments := reader.calls[0]
	if deployments.namespace != "checkout-production" {
		t.Errorf("listed Deployments in %q, want the environment's target namespace", deployments.namespace)
	}
	for key, want := range map[string]string{
		delivery.LabelManagedBy:   delivery.ManagedByKelson,
		delivery.LabelProject:     "checkout",
		delivery.LabelEnvironment: "production",
	} {
		if deployments.match[key] != want {
			t.Errorf("the Deployment list selector has %s=%q, want %q — without kelson's own provenance "+
				"labels the readback classifies workloads this environment did not apply",
				key, deployments.match[key], want)
		}
	}
	if pods := reader.calls[1]; pods.match["app"] != "web" {
		t.Errorf("the pod list selector is %v, want the Deployment's own spec.selector.matchLabels", pods.match)
	}
}

// TestObserveClassifiesAndCounts is the shape a `kubectl get environment -o
// yaml` reader gets: complete counts, and a list that names the failure in
// internal/observation's own vocabulary.
func TestObserveClassifiesAndCounts(t *testing.T) {
	reader := &fakeWorkloads{
		deployments: []*unstructured.Unstructured{
			fixtureDeployment("checkout-production", "web", "web"),
			fixtureDeployment("checkout-production", "worker", "worker"),
		},
		pods: map[string][]*unstructured.Unstructured{
			"app=web":    {fixturePod("checkout-production", "web-0", "app", "")},
			"app=worker": {fixturePod("checkout-production", "worker-0", "app", "CrashLoopBackOff")},
		},
	}
	got := observeFixture(t, reader)

	if got.Checked != 2 || got.Healthy != 1 || got.Degraded != 1 || got.Progressing != 0 {
		t.Fatalf("counts = checked %d healthy %d degraded %d progressing %d, want 2/1/1/0",
			got.Checked, got.Healthy, got.Degraded, got.Progressing)
	}
	if len(got.Unhealthy) != 1 {
		t.Fatalf("got %d unhealthy entries, want 1: %+v", len(got.Unhealthy), got.Unhealthy)
	}
	entry := got.Unhealthy[0]
	if entry.Code != v1alpha1.WorkloadCrashLoopBackOff {
		t.Errorf("code = %q, want %q", entry.Code, v1alpha1.WorkloadCrashLoopBackOff)
	}
	if !strings.Contains(entry.Resource, "worker") {
		t.Errorf("resource = %q, want it to name the worker Deployment", entry.Resource)
	}
	if entry.Remediation == "" {
		t.Error("no remediation on a failure entry; a status a human reads should say what to do")
	}
	if len(entry.Containers) != 1 || entry.Containers[0].Pod != "worker-0" || entry.Containers[0].Name != "app" {
		t.Errorf("containers = %+v, want the failing container named by pod and name — that "+
			"\"which pod, which container\" is the whole reason this readback exists", entry.Containers)
	}
	if got.Unavailable != "" {
		t.Errorf("unavailable = %q on a readback that worked", got.Unavailable)
	}
}

// TestObserveCarriesNoContainerOutput is the security property stated in
// v1alpha1.WorkloadsStatus, asserted where it could be broken: the status shape
// has no field for logs, so the only way one arrives is a future edit adding
// one. This test is the thing that argues with that edit.
func TestObserveCarriesNoContainerOutput(t *testing.T) {
	for _, f := range []string{"Logs", "LogError", "Output", "Message"} {
		if _, ok := reflectFieldNames(v1alpha1.UnhealthyContainer{})[f]; ok {
			t.Errorf("v1alpha1.UnhealthyContainer has a %q field. A container's output must not reach "+
				"an Environment's status: it is readable by anyone who can read the Environment, and a "+
				"crash dump is where a connection string appears. `kelson logs` reads them instead.", f)
		}
	}
}

func reflectFieldNames(v any) map[string]struct{} {
	rt := reflect.TypeOf(v)
	out := make(map[string]struct{}, rt.NumField())
	for i := range rt.NumField() {
		out[rt.Field(i).Name] = struct{}{}
	}
	return out
}

// TestObserveBoundsTheUnhealthyList: the counts are complete and the list is
// not, and the list has to be the *same* prefix every reconcile or a status
// under `kubectl get -w` churns while nothing changes.
func TestObserveBoundsTheUnhealthyList(t *testing.T) {
	reader := &fakeWorkloads{pods: map[string][]*unstructured.Unstructured{}}
	total := v1alpha1.MaxUnhealthyWorkloads + 3
	// Built in reverse name order, so a readback that simply preserved the
	// reader's order would keep the wrong ones.
	for i := total - 1; i >= 0; i-- {
		name := fmt.Sprintf("web-%02d", i)
		reader.deployments = append(reader.deployments, fixtureDeployment("checkout-production", name, name))
		reader.pods["app="+name] = []*unstructured.Unstructured{
			fixturePod("checkout-production", name+"-0", "app", "ImagePullBackOff"),
		}
	}

	got := observeFixture(t, reader)

	if int(got.Degraded) != total {
		t.Errorf("degraded = %d, want the complete count %d — the count is what says how bad it is",
			got.Degraded, total)
	}
	if len(got.Unhealthy) != v1alpha1.MaxUnhealthyWorkloads {
		t.Fatalf("unhealthy has %d entries, want the bound %d", len(got.Unhealthy), v1alpha1.MaxUnhealthyWorkloads)
	}
	for i := 1; i < len(got.Unhealthy); i++ {
		if got.Unhealthy[i-1].Resource >= got.Unhealthy[i].Resource {
			t.Fatalf("unhealthy is not in resource-name order: %q then %q",
				got.Unhealthy[i-1].Resource, got.Unhealthy[i].Resource)
		}
	}
	if !strings.Contains(got.Unhealthy[0].Resource, "web-00") {
		t.Errorf("the first entry is %q, want the alphabetically first — the bound must take a stable "+
			"prefix, not whatever the API server listed first", got.Unhealthy[0].Resource)
	}
}

// TestObserveSkipsSelectorlessDeployments: an empty label selector matches
// every pod in the namespace, not none of them. A Deployment with no
// spec.selector must therefore list nothing rather than be classified against
// its neighbours' failures.
func TestObserveSkipsSelectorlessDeployments(t *testing.T) {
	dep := fixtureDeployment("checkout-production", "web", "web")
	unstructured.RemoveNestedField(dep.Object, "spec", "selector")
	reader := &fakeWorkloads{deployments: []*unstructured.Unstructured{dep}}

	got := observeFixture(t, reader)

	if len(reader.calls) != 2 {
		t.Fatalf("made %d calls, want the Deployment list and one pod list: %+v", len(reader.calls), reader.calls)
	}
	if len(reader.calls[1].match) != 0 {
		t.Errorf("the pod list selector is %v, want empty — and ClientWorkloads.List refuses an empty one",
			reader.calls[1].match)
	}
	if got.Checked != 1 || got.Degraded != 0 {
		t.Errorf("counts = checked %d degraded %d, want the workload counted and not diagnosed",
			got.Checked, got.Degraded)
	}
}

// TestObserveReportsAReadFailureAsUnavailable is the checked-versus-
// could-not-check discipline: a listing that failed and an environment with
// nothing to look at produce identical counts and mean opposite things.
func TestObserveReportsAReadFailureAsUnavailable(t *testing.T) {
	d := &FluxDeliverer{Workloads: ClusterWorkloads{
		Reader: &fakeWorkloads{err: errors.New("pods is forbidden")},
	}}
	got := d.readWorkloads(context.Background(), Revision{
		Project: "checkout", Environment: "production", TargetNamespace: "checkout-production",
	})
	if got == nil {
		t.Fatal("a failed readback reported nothing at all; it must report that it could not look")
	}
	if !strings.Contains(got.Unavailable, "forbidden") {
		t.Errorf("unavailable = %q, want the reason the read failed", got.Unavailable)
	}
	if got.Checked != 0 || got.Healthy != 0 {
		t.Errorf("counts = %+v on a failed readback; zero counts beside an empty `unavailable` would "+
			"read as \"we looked and all is well\"", got)
	}
}

// --- the phase refinement ---------------------------------------------------

// TestRefineDowngradesASettledHealthy is issue #53 at the environment level:
// `wait: true` makes Flux's Ready mean "the applied set converged", which stays
// true after a pod starts crash-looping ten minutes later, because nothing the
// Kustomization applied changed.
func TestRefineDowngradesASettledHealthy(t *testing.T) {
	workloads := &v1alpha1.WorkloadsStatus{
		Checked: 1, Degraded: 1,
		Unhealthy: []v1alpha1.UnhealthyWorkload{{
			Resource: "Deployment/checkout-production/web",
			Code:     v1alpha1.WorkloadCrashLoopBackOff,
			Reason:   "CrashLoopBackOff",
			Containers: []v1alpha1.UnhealthyContainer{
				{Pod: "web-0", Name: "app", Code: v1alpha1.WorkloadCrashLoopBackOff},
			},
		}},
	}
	for _, from := range []string{v1alpha1.PhaseHealthy, v1alpha1.PhaseApplied} {
		phase, cause := refine(from, "applied", workloads)
		if phase != v1alpha1.PhaseDegraded {
			t.Errorf("refine(%s) = %q, want Degraded: a status that reports healthy over a "+
				"CrashLoopBackOff is the conflation the observation plane exists to prevent", from, phase)
		}
		if !strings.Contains(cause, "web-0") || !strings.Contains(cause, "crash-loop-back-off") {
			t.Errorf("cause = %q, want it to name the pod and the code", cause)
		}
	}
}

// TestRefineLeavesAnUnsettledPhaseAlone: while Flux is still working, the pods
// on the cluster are the *previous* revision's. Reporting them as this one's
// failure would make every rolling update flash Degraded on the way through,
// and Degraded is a state that pages people.
func TestRefineLeavesAnUnsettledPhaseAlone(t *testing.T) {
	workloads := &v1alpha1.WorkloadsStatus{Checked: 1, Degraded: 1,
		Unhealthy: []v1alpha1.UnhealthyWorkload{{Resource: "r", Code: v1alpha1.WorkloadCrashLoopBackOff}}}
	for _, from := range []string{
		v1alpha1.PhaseCommitted, v1alpha1.PhaseReconciling, v1alpha1.PhaseRejected, v1alpha1.PhaseDegraded,
	} {
		if phase, _ := refine(from, "waiting", workloads); phase != "" {
			t.Errorf("refine(%s) moved the phase to %q; only a settled Applied or Healthy may be "+
				"refined", from, phase)
		}
	}
}

// TestRefineNeverUpgrades: healthy-looking workloads cannot argue a
// Kustomization out of its verdict. A set that was never applied has no
// workloads of this revision to look at, so an all-green readback beside a
// failed apply is a readback of the *previous* revision.
func TestRefineNeverUpgrades(t *testing.T) {
	green := &v1alpha1.WorkloadsStatus{Checked: 3, Healthy: 3}
	for _, from := range []string{v1alpha1.PhaseRejected, v1alpha1.PhaseDegraded, v1alpha1.PhaseCommitted} {
		if phase, _ := refine(from, "flux said no", green); phase != "" {
			t.Errorf("refine(%s) with a healthy readback returned %q; the readback may only downgrade",
				from, phase)
		}
	}
}

// TestObserveWiresTheReadbackIntoDeliver is the seam end to end: a
// [WorkloadObserver] on the deliverer reaches Outcome.Workloads, and its verdict
// reaches the phase. Everything above this line tests a piece.
func TestObserveWiresTheReadbackIntoDeliver(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)
	first, err := d.Deliver(context.Background(), testRevision(t))
	if err != nil {
		t.Fatal(err)
	}

	ks := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	revision := first.Revision + "@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	if err := unstructured.SetNestedMap(ks.Object, map[string]any{
		"lastAppliedRevision":   revision,
		"lastAttemptedRevision": revision,
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "message": "applied"},
		},
	}, "status"); err != nil {
		t.Fatal(err)
	}
	if err := d.Client.Update(context.Background(), ks); err != nil {
		t.Fatal(err)
	}

	rev := testRevision(t)
	rev.Observed = first.Revision

	// Without an observer: Flux's verdict, and no readback section at all.
	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != v1alpha1.PhaseHealthy || out.Workloads != nil {
		t.Fatalf("phase = %q workloads = %+v, want Healthy with no readback when none is wired",
			out.Phase, out.Workloads)
	}

	// With one that finds a crash loop: Degraded, and the detail on the status.
	d.Workloads = ClusterWorkloads{Reader: &fakeWorkloads{
		deployments: []*unstructured.Unstructured{fixtureDeployment(rev.TargetNamespace, "web", "web")},
		pods: map[string][]*unstructured.Unstructured{
			"app=web": {fixturePod(rev.TargetNamespace, "web-0", "app", "CrashLoopBackOff")},
		},
	}}
	out, err = d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("a readback must never fail the delivery it observed: %v", err)
	}
	if out.Phase != v1alpha1.PhaseDegraded {
		t.Errorf("phase = %q, want Degraded once the readback found a crash loop", out.Phase)
	}
	if out.Workloads == nil || out.Workloads.Degraded != 1 {
		t.Fatalf("workloads = %+v, want one degraded workload on the outcome", out.Workloads)
	}
	if !strings.Contains(out.Cause, "web-0") {
		t.Errorf("cause = %q, want the pod named — that sentence is what reaches the Ready condition", out.Cause)
	}
}

// --- the reconciler ---------------------------------------------------------

// TestReadbackReachesTheStatus is the last link: what a Deliverer observed has
// to arrive on the custom resource, or the whole readback is a computation
// nobody can see. It is the `kubectl get environment -o yaml` bar from issue
// #240, asserted at the only place that bar is met.
func TestReadbackReachesTheStatus(t *testing.T) {
	workloads := &v1alpha1.WorkloadsStatus{
		Checked: 2, Healthy: 1, Degraded: 1,
		Unhealthy: []v1alpha1.UnhealthyWorkload{{
			Resource:    "Deployment/checkout-production/worker",
			Code:        v1alpha1.WorkloadImagePullBackOff,
			Reason:      "ImagePullBackOff",
			Remediation: "check the image name and tag",
			Containers: []v1alpha1.UnhealthyContainer{
				{Pod: "worker-0", Name: "app", Code: v1alpha1.WorkloadImagePullBackOff},
			},
		}},
	}
	spy := &spyDeliverer{outcome: Outcome{
		Revision: "8-99887766", Phase: v1alpha1.PhaseDegraded, Published: true, Workloads: workloads,
	}}
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := readEnvironment(t, c, "production")
	if got.Status.Workloads == nil {
		t.Fatal("status.workloads is absent after a reconcile that observed workloads")
	}
	if got.Status.Workloads.Degraded != 1 || got.Status.Workloads.Checked != 2 {
		t.Errorf("status.workloads = %+v, want the counts the deliverer reported", got.Status.Workloads)
	}
	if len(got.Status.Workloads.Unhealthy) != 1 ||
		got.Status.Workloads.Unhealthy[0].Code != v1alpha1.WorkloadImagePullBackOff {
		t.Errorf("status.workloads.unhealthy = %+v, want the failing workload named in the "+
			"observation vocabulary", got.Status.Workloads.Unhealthy)
	}
	if containers := got.Status.Workloads.Unhealthy[0].Containers; len(containers) != 1 ||
		containers[0].Pod != "worker-0" {
		t.Errorf("containers = %+v, want the pod named", containers)
	}

	// And the next reconcile that observes nothing clears it, rather than
	// leaving a readback from a previous minute beside a phase from this one.
	spy.outcome = Outcome{Revision: "8-99887766", Phase: v1alpha1.PhaseHealthy}
	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := readEnvironment(t, c, "production"); got.Status.Workloads != nil {
		t.Errorf("status.workloads survived a reconcile that did not look: %+v. A readback is a "+
			"snapshot; keeping the last good one produces a status whose sections describe "+
			"different moments.", got.Status.Workloads)
	}
}
