//go:build envtest

// The delivery path against Flux's own schemas (issue #243).
//
// Everything else in this package's tests applies the OCIRepository and the
// Kustomization to controller-runtime's fake client, which stores whatever map
// it is handed and hands the same map back. That is the right harness for
// "which fields does the deliverer decide", and it is worth almost nothing for
// "will Flux accept this object": a fake client has no schema, so a misspelt
// key, a duration in the wrong format, a `sourceRef.kind` outside the enum and
// a URL with no scheme all round-trip perfectly and fail for the first time in
// somebody's cluster.
//
// These tests apply the same objects, built by the same [FluxDeliverer], to a
// real API server serving Flux's real CustomResourceDefinitions — pinned at the
// version ADR-0030's FluxInstance installs, committed under
// testdata/flux-crds/, drift-tested in fluxcrds_test.go. The API server prunes
// what the schema does not declare, refuses what its patterns and enums do not
// allow, defaults what upstream defaults, and treats an identical re-apply as
// the no-op it is. None of those is a property of kelson's Go code, and this is
// the only place any of them is exercised.

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
)

// envtestDeliverer is [testDeliverer] against the API server instead of the
// fake client: the same struct, the same fake pusher, a real apply.
func envtestDeliverer(t *testing.T, c client.Client, fluxNS string, push *fakePusher) *FluxDeliverer {
	t.Helper()
	return &FluxDeliverer{
		Client:        c,
		Registry:      "ghcr.io/acme",
		FluxNamespace: fluxNS,
		Pusher:        push.connect,
		// A path that cannot exist, so the credential lookup takes the
		// anonymous branch instead of reading the test machine's docker login.
		RegistryConfig: t.TempDir() + "/absent/config.json",
	}
}

// deliveryNamespaces creates the two namespaces a delivery involves and returns
// them in the order the code names them: where the custom resources live, and
// where kelson's Flux objects live. They are separate on purpose — that split
// is what the environment-namespace label and the name-conflict check exist for
// (ADR-0028 decision 3, fluxobjects.go).
func deliveryNamespaces(t *testing.T, c client.Client) (crNS, fluxNS string) {
	t.Helper()
	return createNamespace(t, c, nsName(t.Name(), "-app")),
		createNamespace(t, c, nsName(t.Name(), "-flux"))
}

func liveFluxObject(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u); err != nil {
		t.Fatalf("reading %s %s/%s back from the API server: %v", gvk.Kind, ns, name, err)
	}
	return u
}

func nestedString(t *testing.T, u *unstructured.Unstructured, path ...string) string {
	t.Helper()
	v, found, err := unstructured.NestedString(u.Object, path...)
	if err != nil {
		t.Fatalf("reading %s off the %s: %v", strings.Join(path, "."), u.GetKind(), err)
	}
	if !found {
		t.Fatalf("%s is not set on the %s — the API server pruned it, which means the schema does not declare it",
			strings.Join(path, "."), u.GetKind())
	}
	return v
}

// TestFluxCRDsAreServed is the first thing worth knowing, and it is the one
// assertion the rest depend on: the API server serves both kinds at exactly the
// coordinates fluxobjects.go applies to. An apply against a group the cluster
// does not serve fails in the RESTMapper as a *meta.NoKindMatchError — which is
// what kindNotServed exists to recognise — so getting this wrong would report
// every test below as "Flux is not installed" rather than as a schema failure.
func TestFluxCRDsAreServed(t *testing.T) {
	c := envtestClient(t)
	for _, gvk := range []schema.GroupVersionKind{ociRepositoryGVK, kustomizationGVK} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := c.List(context.Background(), list); err != nil {
			t.Errorf("listing %s: %v (the CRD fixture did not install; run `make flux-crds`)", gvk, err)
		}
	}
}

// TestDeliverAppliesThePairAgainstFluxSchemas is the delivery path itself, run
// against the real thing.
//
// Every field asserted here had to survive a structural schema to be readable
// at all: the API server drops any key `spec` does not declare, so a field that
// comes back is a field Flux's own CRD names. That is the difference between
// this and the fake-client test of the same shape in delivery_test.go, which
// would pass just as happily if kelson wrote `spec.targetNS`.
func TestDeliverAppliesThePairAgainstFluxSchemas(t *testing.T) {
	c := envtestClient(t)
	crNS, fluxNS := deliveryNamespaces(t, c)
	ctx := context.Background()

	push := &fakePusher{}
	d := envtestDeliverer(t, c, fluxNS, push)
	rev := testRevision(t)
	rev.EnvironmentNamespace = crNS

	out, err := d.Deliver(ctx, rev)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !out.Published || out.Revision == "" || out.Digest == "" {
		t.Fatalf("nothing was published: %+v", out)
	}
	// A Kustomization that exists and has never reconciled is Committed, not a
	// failure — there is no kustomize-controller in an envtest cluster, and
	// reporting a missing status as broken would make every first deploy in a
	// real cluster look broken too (delivery.go's observe).
	if out.Phase != v1alpha1.PhaseCommitted {
		t.Errorf("phase = %q, want %q for a pair nothing has reconciled yet", out.Phase, v1alpha1.PhaseCommitted)
	}

	name := ObjectName("checkout", "production")

	oci := liveFluxObject(t, c, ociRepositoryGVK, fluxNS, name)
	// The URL keeps its scheme because source-controller's schema pins it with
	// `pattern: ^oci://.*$`: a repository written without one is refused at the
	// API server rather than at the first pull.
	if got := nestedString(t, oci, "spec", "url"); got != "oci://ghcr.io/acme/kelson/checkout-production" {
		t.Errorf("spec.url = %q", got)
	}
	if got := nestedString(t, oci, "spec", "ref", "tag"); got != out.Revision {
		t.Errorf("spec.ref.tag = %q, want the published revision %q", got, out.Revision)
	}
	if got := nestedString(t, oci, "spec", "ref", "digest"); got != out.Digest {
		t.Errorf("spec.ref.digest = %q, want %q — both halves of the reference are pinned (fluxobjects.go)",
			got, out.Digest)
	}
	// spec.interval is `pattern: ^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`, which is
	// what makes this assertion worth making: Go's Duration.String() produces
	// "5m0s", and a format the pattern rejected would have failed the apply.
	if got := nestedString(t, oci, "spec", "interval"); got != DefaultInterval.String() {
		t.Errorf("spec.interval = %q, want %q", got, DefaultInterval.String())
	}
	// And the API server defaulted a field kelson never wrote, which no fake
	// client does: proof that a real schema and not a map is in play.
	if got := nestedString(t, oci, "spec", "timeout"); got == "" {
		t.Error("the OCIRepository's spec.timeout was not defaulted; that default is upstream's, not kelson's")
	}

	ks := liveFluxObject(t, c, kustomizationGVK, fluxNS, name)
	if got := nestedString(t, ks, "spec", "path"); got != "./" {
		t.Errorf("spec.path = %q, want the artifact root (ADR-0017 decision 10)", got)
	}
	if got := nestedString(t, ks, "spec", "targetNamespace"); got != rev.TargetNamespace {
		t.Errorf("spec.targetNamespace = %q, want %q", got, rev.TargetNamespace)
	}
	// sourceRef.kind is an enum of four in the schema. kelson writes one of
	// them, and a rename on either side of flux.KindOCIRepository would be
	// refused here instead of silently pointing the Kustomization at nothing.
	if got := nestedString(t, ks, "spec", "sourceRef", "kind"); got != KindOCIRepository {
		t.Errorf("spec.sourceRef.kind = %q, want %q", got, KindOCIRepository)
	}
	if got := nestedString(t, ks, "spec", "sourceRef", "name"); got != name {
		t.Errorf("spec.sourceRef.name = %q, want %q — the pair shares one name so it cannot be mismatched", got, name)
	}
	for _, field := range []string{"prune", "wait"} {
		v, found, err := unstructured.NestedBool(ks.Object, "spec", field)
		if err != nil || !found || !v {
			t.Errorf("spec.%s is %v (found=%v, err=%v); prune is what deletes workloads on teardown and "+
				"wait is what makes Ready mean healthy", field, v, found, err)
		}
	}

	// The provenance labels are how a watch event maps back to an Environment
	// and how checkConflict tells two environments of the same name apart.
	labels := ks.GetLabels()
	if labels[delivery.LabelManagedBy] != delivery.ManagedByKelson ||
		labels[delivery.LabelProject] != "checkout" ||
		labels[delivery.LabelEnvironment] != "production" ||
		labels[delivery.LabelEnvironmentNamespace] != crNS {
		t.Errorf("the Kustomization's labels are %v", labels)
	}
}

// TestRepeatedDeliveryOfOneRevisionStopsWriting is the property
// delivery_test.go's specOf helper says out loud it cannot assert: "the fake
// client bumps resourceVersion on every apply, no-op or not, where a real API
// server does not".
//
// It matters beyond tidiness. Both Flux objects are watched by this controller,
// so an apply that always wrote would make every reconcile produce two watch
// events, which would schedule another reconcile, which would apply again — a
// controller that never goes idle for an environment nobody is touching. What
// stops that is the API server recognising an apply that changes nothing and
// declining to write, and that recognition is exactly what a fake client cannot
// model.
//
// The measurement starts at the *second* delivery on purpose, and the reason is
// worth recording rather than papering over: an object created by a
// server-side apply is written once more by the next identical apply — the
// stored object settles into the form the apply path produces — and is
// completely stable from then on. So the invariant this asserts is convergence,
// which is the one the watch loop needs, and not "an apply never writes", which
// is false for the first re-apply and would make this test a lie about the
// second run.
func TestRepeatedDeliveryOfOneRevisionStopsWriting(t *testing.T) {
	c := envtestClient(t)
	crNS, fluxNS := deliveryNamespaces(t, c)
	ctx := context.Background()

	push := &fakePusher{}
	d := envtestDeliverer(t, c, fluxNS, push)
	rev := testRevision(t)
	rev.EnvironmentNamespace = crNS
	name := ObjectName("checkout", "production")

	versions := func() map[string]string {
		return map[string]string{
			KindOCIRepository: liveFluxObject(t, c, ociRepositoryGVK, fluxNS, name).GetResourceVersion(),
			KindKustomization: liveFluxObject(t, c, kustomizationGVK, fluxNS, name).GetResourceVersion(),
		}
	}

	for i := range 2 {
		if _, err := d.Deliver(ctx, rev); err != nil {
			t.Fatalf("Deliver %d: %v", i, err)
		}
	}
	settled := versions()

	if _, err := d.Deliver(ctx, rev); err != nil {
		t.Fatalf("the third Deliver: %v", err)
	}
	for kind, was := range settled {
		if now := versions()[kind]; now != was {
			t.Errorf("re-delivering an unchanged revision moved the %s's resourceVersion from %s to %s; "+
				"every such write is a watch event that schedules the reconcile that produces it", kind, was, now)
		}
	}
}

// TestTheAPIServerRefusesWhatFluxWouldRefuse is the fixture proving itself.
//
// If these applies succeeded, every assertion in this file would be worthless:
// it would mean the CRDs installed as permissive shells rather than as the
// schemas Flux ships, and the suite would be back to storing whatever it is
// handed. Each case is a constraint the deliverer's own output satisfies, so
// each is also a statement about why that output is the way it is.
func TestTheAPIServerRefusesWhatFluxWouldRefuse(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	cases := []struct {
		name  string
		gvk   schema.GroupVersionKind
		spec  map[string]any
		wants string
	}{
		{
			name: "an OCI URL with no scheme",
			gvk:  ociRepositoryGVK,
			spec: map[string]any{
				"interval": "5m0s",
				"url":      "ghcr.io/acme/kelson/checkout-production",
			},
			wants: "spec.url",
		},
		{
			name: "an interval that is not a Go duration",
			gvk:  ociRepositoryGVK,
			spec: map[string]any{
				"interval": "5 minutes",
				"url":      "oci://ghcr.io/acme/kelson/checkout-production",
			},
			wants: "spec.interval",
		},
		{
			name: "a sourceRef kind outside the enum",
			gvk:  kustomizationGVK,
			spec: map[string]any{
				"interval":  "5m0s",
				"path":      "./",
				"prune":     true,
				"sourceRef": map[string]any{"kind": "OCIRepositor", "name": "checkout-production"},
			},
			wants: "spec.sourceRef.kind",
		},
		{
			name: "an empty targetNamespace",
			gvk:  kustomizationGVK,
			spec: map[string]any{
				"interval":        "5m0s",
				"path":            "./",
				"prune":           true,
				"targetNamespace": "",
				"sourceRef":       map[string]any{"kind": KindOCIRepository, "name": "checkout-production"},
			},
			wants: "spec.targetNamespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{}}
			u.SetGroupVersionKind(tc.gvk)
			u.SetNamespace(ns)
			u.SetName("refused-" + strings.ToLower(strings.ReplaceAll(tc.name, " ", "-")))
			u.Object["spec"] = tc.spec

			err := c.Create(ctx, u)
			if err == nil {
				t.Fatalf("the API server accepted %s; the fixture is not enforcing Flux's schema", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the refusal does not name %s: %v", tc.wants, err)
			}
		})
	}
}

// TestReconcileDeliversTheFluxPair is the whole spine end to end, against real
// schemas on both sides: a Project and an Environment go in through the API
// server that validates kelson's CRDs, and an OCIRepository and a Kustomization
// come out through the API server that validates Flux's.
//
// It is the test issue #243 exists for. Until the fixtures landed, the only way
// to assert this was to hand the reconciler a fake Deliverer — which proves the
// reconciler called something — or a fake client, which proves the deliverer
// built a map. This asserts the thing the product actually does.
func TestReconcileDeliversTheFluxPair(t *testing.T) {
	c := envtestClient(t)
	crNS, fluxNS := deliveryNamespaces(t, c)
	ctx := context.Background()

	p := validProject()
	p.Namespace, p.Generation = crNS, 0
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("creating the project: %v", err)
	}
	e := validEnvironment()
	e.Namespace, e.Generation = crNS, 0
	if err := c.Create(ctx, e); err != nil {
		t.Fatalf("creating the environment: %v", err)
	}

	push := &fakePusher{}
	r := &EnvironmentReconciler{
		Client: c,
		// A cluster with Flux in it: without this finding the deliverer refuses
		// before it publishes, which is the correct behaviour and not what this
		// test is about (delivery.go's step 0).
		Profiles: StaticProfileSource{ClusterProfile: clusterprofile.ClusterProfile{
			Flux: &clusterprofile.Component{Version: "v2.9.4", Namespace: "flux-system"},
		}},
		Delivery: envtestDeliverer(t, c, fluxNS, push),
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	name := ObjectName("checkout", "production")
	oci := liveFluxObject(t, c, ociRepositoryGVK, fluxNS, name)
	ks := liveFluxObject(t, c, kustomizationGVK, fluxNS, name)

	var env v1alpha1.Environment
	if err := c.Get(ctx, req.NamespacedName, &env); err != nil {
		t.Fatalf("reading the environment back: %v", err)
	}
	if env.Status.Revision == "" {
		t.Fatal("the status records no revision, so nothing was published")
	}
	// The tag in the status and the tag the cluster is pinned to are the same
	// string. They are written by different steps through different clients,
	// and a status that named a revision the OCIRepository was not pinned to
	// would be the specific lie ADR-0027 gives the status subresource to avoid.
	if pinned := nestedString(t, oci, "spec", "ref", "tag"); pinned != env.Status.Revision {
		t.Errorf("the OCIRepository is pinned to %q and the status claims %q", pinned, env.Status.Revision)
	}
	if source := nestedString(t, ks, "spec", "sourceRef", "name"); source != name {
		t.Errorf("the Kustomization points at %q, not at its own OCIRepository", source)
	}
	if len(env.Status.History) != 1 {
		t.Errorf("history has %d entries after one publish", len(env.Status.History))
	}
	// The finalizer is added only once something was applied, and it is what
	// keeps the Environment alive until its Kustomization is gone.
	if !hasFinalizer(env.Finalizers) {
		t.Errorf("finalizers = %v, want %s", env.Finalizers, Finalizer)
	}

	// And deleting the Environment takes the pair with it: the finalizer runs
	// the teardown, and both objects leave the API server for real.
	if err := c.Delete(ctx, &env); err != nil {
		t.Fatalf("deleting the environment: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile of the deleting environment: %v", err)
	}
	for _, gvk := range []schema.GroupVersionKind{kustomizationGVK, ociRepositoryGVK} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		err := c.Get(ctx, types.NamespacedName{Namespace: fluxNS, Name: name}, u)
		if !apierrors.IsNotFound(err) {
			t.Errorf("the %s survived the Environment's deletion: %v", gvk.Kind, err)
		}
	}
	if err := c.Get(ctx, req.NamespacedName, &env); !apierrors.IsNotFound(err) {
		t.Errorf("the Environment is still there after its finalizer ran: %v", err)
	}
}

func hasFinalizer(finalizers []string) bool {
	for _, f := range finalizers {
		if f == Finalizer {
			return true
		}
	}
	return false
}

// TestWorkloadReadbackAgainstARealAPIServer is the readback's half of what this
// file exists for (issue #240).
//
// Against the fake client, [ClientWorkloads] proves that Go code called List
// with some arguments and got back whatever the fake was seeded with. Every
// interesting way it can be wrong survives that: an unstructured list needs the
// *List* kind ("DeploymentList", not "Deployment") or the RESTMapper never
// resolves it; a label selector is applied server-side, so a malformed one is a
// query that quietly returns everything; `apps/v1` versus anything else is a
// 404 only a real discovery document produces. None of those is a property of
// kelson's logic, and this is the only place any of them is exercised.
//
// It also pins the one property no schema can: the query is scoped by kelson's
// own provenance labels, so a workload in the same namespace that kelson did
// not apply is invisible to it.
func TestWorkloadReadbackAgainstARealAPIServer(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	ns := createNamespace(t, c, nsName(t.Name(), ""))

	// Two of kelson's own, and one the environment did not apply.
	createDeployment(t, c, ns, "web", "web", true)
	createDeployment(t, c, ns, "worker", "worker", true)
	createDeployment(t, c, ns, "someone-elses", "someone-elses", false)

	// web is up; worker's pod is in CrashLoopBackOff; the foreign workload's is
	// too, and must not appear anywhere in the answer.
	createPod(t, c, ns, "web-0", map[string]string{"app": "web"}, "")
	createPod(t, c, ns, "worker-0", map[string]string{"app": "worker"}, "CrashLoopBackOff")
	createPod(t, c, ns, "someone-elses-0", map[string]string{"app": "someone-elses"}, "CrashLoopBackOff")

	got, err := ClusterWorkloads{Reader: ClientWorkloads(c)}.Observe(ctx, Revision{
		Project: "checkout", Environment: "production", TargetNamespace: ns,
	})
	if err != nil {
		t.Fatalf("Observe against a real API server: %v", err)
	}

	if got.Checked != 2 {
		t.Fatalf("checked = %d, want 2 — the label selector is what keeps a workload kelson did not "+
			"apply out of this environment's status: %+v", got.Checked, got)
	}
	if got.Degraded != 1 || got.Healthy != 1 {
		t.Errorf("degraded = %d healthy = %d, want 1 and 1: %+v", got.Degraded, got.Healthy, got)
	}
	if len(got.Unhealthy) != 1 {
		t.Fatalf("unhealthy = %+v, want the worker alone", got.Unhealthy)
	}
	entry := got.Unhealthy[0]
	if entry.Code != v1alpha1.WorkloadCrashLoopBackOff {
		t.Errorf("code = %q, want %q", entry.Code, v1alpha1.WorkloadCrashLoopBackOff)
	}
	if !strings.Contains(entry.Resource, "worker") || strings.Contains(entry.Resource, "someone") {
		t.Errorf("resource = %q, want the worker Deployment and never the foreign one", entry.Resource)
	}
	if len(entry.Containers) != 1 || entry.Containers[0].Pod != "worker-0" {
		t.Errorf("containers = %+v, want the failing pod named. A pod selector that came back empty "+
			"would look exactly like this test passing with zero containers.", entry.Containers)
	}
}

// createDeployment applies a minimal but schema-valid Deployment. `mine` stamps
// kelson's provenance labels; without them the readback must not see it.
func createDeployment(t *testing.T, c client.Client, namespace, name, selector string, mine bool) {
	t.Helper()
	labels := map[string]string{}
	if mine {
		labels = map[string]string{
			delivery.LabelManagedBy:   delivery.ManagedByKelson,
			delivery.LabelProject:     "checkout",
			delivery.LabelEnvironment: "production",
		}
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": selector}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": selector}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}
	if err := c.Create(context.Background(), dep); err != nil {
		t.Fatalf("creating Deployment %s/%s: %v", namespace, name, err)
	}
	// envtest runs no controllers, so the Available condition has to be written
	// by hand — and through the status subresource, which is the write path a
	// real deployment-controller uses.
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: dep.Generation,
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
		},
	}
	if err := c.Status().Update(context.Background(), dep); err != nil {
		t.Fatalf("writing the Deployment status of %s/%s: %v", namespace, name, err)
	}
}

// createPod applies a pod and writes the container status the classifier reads.
// An empty waiting reason is a ready pod.
func createPod(t *testing.T, c client.Client, namespace, name string, labels map[string]string, waiting string) {
	t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatalf("creating Pod %s/%s: %v", namespace, name, err)
	}
	status := corev1.PodStatus{
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	if waiting != "" {
		status.Conditions[1].Status = corev1.ConditionFalse
		status.ContainerStatuses[0].Ready = false
		status.ContainerStatuses[0].State = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: waiting, Message: "back-off restarting failed container"},
		}
	}
	p.Status = status
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatalf("writing the Pod status of %s/%s: %v", namespace, name, err)
	}
}
