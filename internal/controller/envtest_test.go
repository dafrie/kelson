//go:build envtest

// The envtest suite: the generated CRDs against a real API server.
//
// # How it is gated
//
// Two gates, the same shape test/e2e uses and for the same reasons. The
// `envtest` build tag keeps this file out of `go test ./...` entirely, so a
// checkout with no control-plane binaries still runs a green suite. KELSON_ENVTEST=1
// is required at run time, so `go test -tags envtest ./...` on a machine that
// happens to compile it still skips rather than fails. Opting in is a promise
// that the assets exist: from there a missing etcd is a failure, never a skip,
// because a suite that silently skips is a suite that silently stops proving
// anything.
//
// Run it with:
//
//	make envtest
//
// which downloads the assets with setup-envtest, exports KUBEBUILDER_ASSETS and
// sets KELSON_ENVTEST=1.
//
// # What it covers that the fake client cannot
//
// The fake client stores whatever it is handed. An API server does not: it
// parses deploy/crds/*.yaml, rejects a schema that is not structural, prunes
// fields the schema does not declare, enforces the CEL rules, and refuses a
// status write against a resource with no status subresource. Every one of
// those is a property of the generated CRDs rather than of the Go code, and
// this is the only place they are actually exercised.
//
// # Why Flux's own CRDs are installed beside kelson's
//
// The delivery spine's fifth step server-side applies an OCIRepository and a
// Kustomization (ADR-0028 decision 3, fluxobjects.go), and against a fake
// client that apply proves only that kelson built a map with the keys the test
// then reads back out of it. Flux's schemas are where the interesting refusals
// live — `spec.url` must match `^oci://`, `spec.interval` must be a Go
// duration, `sourceRef.kind` is an enum of four, `targetNamespace` has a length
// bound, and every field kelson does not declare is pruned. Installing the real
// CRDs (testdata/flux-crds, pinned and drift-tested by fluxcrds_test.go) is
// what makes envtest_delivery_test.go's assertions statements about the objects
// Flux will actually receive.

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// enableEnv must be "1" for this suite to run.
const enableEnv = "KELSON_ENVTEST"

var testEnv *envtest.Environment

func TestMain(m *testing.M) {
	if os.Getenv(enableEnv) != "1" {
		os.Exit(0)
	}
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "deploy", "crds"),
			// Flux's own, pinned and committed (fluxcrds_test.go). Second in the
			// list rather than merged into the first directory because
			// deploy/crds/ is an install surface — the chart copies it and
			// `kubectl apply -f deploy/crds/` is a documented step — and kelson
			// must never install another project's API behind a user's back
			// (ADR-0003).
			fluxCRDDir,
		},
		ErrorIfCRDPathMissing: true,
	}
	if _, err := testEnv.Start(); err != nil {
		panic("starting envtest (is KUBEBUILDER_ASSETS set? run `make envtest`): " + err.Error())
	}
	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

func envtestClient(t *testing.T) client.Client {
	t.Helper()
	s := testScheme(t)
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("registering core/v1: %v", err)
	}
	// apps/v1 for the workload readback's fixtures (issue #240). The readback
	// itself reads unstructured and needs no scheme entry; the tests that seed
	// a Deployment want the typed struct rather than a hand-built map.
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("registering apps/v1: %v", err)
	}
	c, err := client.New(testEnv.Config, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	return c
}

// namespace creates a namespace of its own for one test, so the objects one
// test applies cannot be seen by another.
func namespace(t *testing.T, c client.Client) string {
	t.Helper()
	return createNamespace(t, c, nsName(t.Name(), ""))
}

// nsName turns a test's name into a legal namespace, leaving room for a suffix
// so one test can hold two (see the delivery suite: the custom resources and
// kelson's Flux objects deliberately do not share a namespace).
func nsName(name, suffix string) string {
	name = strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	name = strings.ReplaceAll(name, "/", "-")
	if limit := 60 - len(suffix); len(name) > limit {
		name = name[:limit]
	}
	return strings.Trim(name, "-") + suffix
}

func createNamespace(t *testing.T, c client.Client, name string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
	return name
}

// TestCRDsInstall is the first thing worth knowing: the generated manifests are
// accepted by a real API server as structural schemas. TestMain has already
// failed if they were not — this states it as an assertion so the failure has a
// name.
func TestCRDsInstall(t *testing.T) {
	c := envtestClient(t)
	var list v1alpha1.ProjectList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("listing projects: %v (the CRD did not install)", err)
	}
	var envs v1alpha1.EnvironmentList
	if err := c.List(context.Background(), &envs); err != nil {
		t.Fatalf("listing environments: %v (the CRD did not install)", err)
	}
}

// TestAPIServerAcceptsAValidPair is the round trip the CLI's documents will
// take: the same structs, through the real schema, stored and read back.
func TestAPIServerAcceptsAValidPair(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	p := validProject()
	p.Namespace = ns
	p.Generation = 0
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("creating the project: %v", err)
	}
	e := validEnvironment()
	e.Namespace = ns
	e.Generation = 0
	if err := c.Create(ctx, e); err != nil {
		t.Fatalf("creating the environment: %v", err)
	}

	var got v1alpha1.Project
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatalf("reading the project back: %v", err)
	}
	if len(got.Spec.Components) != 1 || got.Spec.Components[0].Port != 8080 {
		t.Fatalf("the spec did not survive the round trip: %+v", got.Spec)
	}
	if got.Generation != 1 {
		t.Errorf("generation is %d, want 1 — it is the revision number (ADR-0028 decision 2)", got.Generation)
	}
}

// TestEnvValueSurvivesTheAPIServer is the one the union node earns. `env` is
// x-kubernetes-preserve-unknown-fields because a structural schema cannot
// express a oneOf (internal/schemagen/crd.go), and the whole point of that
// marking is that the API server stores the mapping instead of pruning it.
func TestEnvValueSurvivesTheAPIServer(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	p := project("checkout", model.ProjectSpec{
		Image: "ghcr.io/acme/checkout:1.0.0",
		Env: map[string]model.EnvValue{
			"LOG_LEVEL": {Literal: "info"},
			"TOKEN":     {Secret: &model.SecretRef{Name: "api", Key: "token"}},
			"DB":        {From: &model.ServiceBinding{Service: "db", Key: "uri"}},
		},
		Components: []model.Component{{Name: "web", Port: 8080}},
	})
	p.Namespace = ns
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("creating: %v", err)
	}

	var got v1alpha1.Project
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if v := got.Spec.Env["LOG_LEVEL"]; v.Literal != "info" {
		t.Errorf("LOG_LEVEL came back as %s", v.String())
	}
	if v := got.Spec.Env["TOKEN"]; v.Secret == nil || v.Secret.Key != "token" {
		t.Errorf("TOKEN came back as %s — the union arm was pruned", v.String())
	}
	if v := got.Spec.Env["DB"]; v.From == nil || v.From.Service != "db" {
		t.Errorf("DB came back as %s — the union arm was pruned", v.String())
	}
}

// TestCELRulesRefuseAtTheAPIServer is the thing ADR-0027 decision 5 buys: a
// malformed document fails at the author's terminal rather than thirty seconds
// later in a controller they may not be watching.
func TestCELRulesRefuseAtTheAPIServer(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	cases := []struct {
		name  string
		spec  model.ProjectSpec
		wants string
	}{
		{
			name: "port and schedule together",
			spec: model.ProjectSpec{
				Image:      "ghcr.io/acme/checkout:1.0.0",
				Components: []model.Component{{Name: "web", Port: 8080, Schedule: "0 3 * * *"}},
			},
			wants: "never both",
		},
		{
			name: "replicas max below min",
			spec: model.ProjectSpec{
				Image: "ghcr.io/acme/checkout:1.0.0",
				Components: []model.Component{
					{Name: "web", Port: 8080, Replicas: &model.Replicas{Min: 5, Max: 2}},
				},
			},
			wants: "at least replicas.min",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := project("refused-"+strconv.Itoa(i), tc.spec)
			p.Namespace = ns
			err := c.Create(ctx, p)
			if err == nil {
				t.Fatalf("the API server accepted %s; the CEL rule did not fire", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("refusal %q does not quote the rule's message (%q)", err, tc.wants)
			}
		})
	}
}

// TestDeliveryStatusFieldsRoundTrip is what the fake client cannot prove: the
// API server prunes any field the structural schema does not declare, so a
// status field added to the Go type without a line in
// internal/schemagen/status.go is silently dropped here and nowhere else.
//
// Every field ADR-0028 asks the spine to record is written, read back, and
// compared — including the ones inside a history entry, which are the ones a
// promotion and a rollback are answered from.
func TestDeliveryStatusFieldsRoundTrip(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	e := validEnvironment()
	e.Namespace = ns
	e.Generation = 0
	if err := c.Create(ctx, e); err != nil {
		t.Fatalf("creating the environment: %v", err)
	}

	want := v1alpha1.EnvironmentStatus{
		ObservedGeneration: 1,
		Phase:              v1alpha1.PhaseHealthy,
		Revision:           "7-1a2b3c4d",
		RollbackRevision:   "6-9f0a1b2c",
		RollbackGeneration: 7,
		History: []v1alpha1.HistoryEntry{{
			Revision: "7-1a2b3c4d",
			Digest:   "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			SpecHash: "1a2b3c4d5e6f7788990011223344556677889900aabbccddeeff001122334455",
			//nolint:staticcheck // the deprecated mirror must still round-trip for one release.
			Images: []string{"ghcr.io/acme/checkout@sha256:abc", "ghcr.io/acme/worker:1.2.3"},
			ComponentImages: []v1alpha1.ComponentImage{
				{Component: "web", Image: "ghcr.io/acme/checkout@sha256:abc"},
				{Component: "worker", Image: "ghcr.io/acme/worker:1.2.3"},
			},
			Outcome:   v1alpha1.PhaseHealthy,
			Timestamp: metav1.NewTime(time.Now().Truncate(time.Second)),
		}},
	}
	e.Status = want
	setReady(&e.Status.Conditions, 1, metav1.ConditionTrue, v1alpha1.ReasonRolledBack, "serving 6-9f0a1b2c")
	setProgressing(&e.Status.Conditions, 1, metav1.ConditionFalse, v1alpha1.ReasonRollbackPinned, "pinned")
	if err := c.Status().Update(ctx, e); err != nil {
		t.Fatalf("writing the status: %v", err)
	}

	var got v1alpha1.Environment
	if err := c.Get(ctx, client.ObjectKeyFromObject(e), &got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got.Status.Revision != want.Revision || got.Status.Phase != want.Phase {
		t.Errorf("revision/phase came back as %q/%q", got.Status.Revision, got.Status.Phase)
	}
	if got.Status.RollbackRevision != want.RollbackRevision || got.Status.RollbackGeneration != want.RollbackGeneration {
		t.Errorf("the rollback fields were pruned: %q at %d",
			got.Status.RollbackRevision, got.Status.RollbackGeneration)
	}
	if len(got.Status.History) != 1 {
		t.Fatalf("history came back with %d entries", len(got.Status.History))
	}
	entry, wantEntry := got.Status.History[0], want.History[0]
	if entry.Revision != wantEntry.Revision || entry.Digest != wantEntry.Digest || entry.SpecHash != wantEntry.SpecHash {
		t.Errorf("history entry = %+v, want %+v", entry, wantEntry)
	}
	if entry.Outcome != wantEntry.Outcome {
		t.Errorf("outcome came back as %q; a field with no schema line is pruned silently", entry.Outcome)
	}
	//nolint:staticcheck // asserting the deprecated mirror is the point of this block.
	if len(entry.Images) != 2 || entry.Images[1] != wantEntry.Images[1] {
		t.Errorf("images came back as %v", entry.Images)
	}
	// The attributed list is an object array, which is the shape a structural
	// schema prunes hardest: a missing property line silently drops the whole
	// field, and a missing `required` would let a half-pair through.
	if len(entry.ComponentImages) != 2 {
		t.Fatalf("componentImages came back as %+v, want both pairs", entry.ComponentImages)
	}
	for i, want := range wantEntry.ComponentImages {
		if entry.ComponentImages[i] != want {
			t.Errorf("componentImages[%d] = %+v, want %+v", i, entry.ComponentImages[i], want)
		}
	}
	if entry.Timestamp.IsZero() {
		t.Error("the timestamp was pruned")
	}
	// Two conditions, and the API server merges them by type rather than
	// clobbering — which is what the list-map-key marking in the schema buys.
	if len(got.Status.Conditions) != 2 {
		t.Fatalf("conditions came back as %v", got.Status.Conditions)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing); c == nil ||
		c.Reason != v1alpha1.ReasonRollbackPinned {
		t.Errorf("the Progressing condition did not survive: %v", got.Status.Conditions)
	}
}

// TestHistoryBoundIsEnforcedByTheAPIServer: the maxItems in the schema is the
// bound, not just the controller's discipline. A status that could grow without
// limit would put the whole deployment history into every watch event every
// controller in the cluster receives.
func TestHistoryBoundIsEnforcedByTheAPIServer(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	e := validEnvironment()
	e.Namespace = ns
	e.Generation = 0
	if err := c.Create(ctx, e); err != nil {
		t.Fatalf("creating: %v", err)
	}
	// The entries carry the attributed images too: a schema addition inside the
	// item must not be a way to talk the API server out of the bound.
	for i := range v1alpha1.MaxHistoryEntries + 1 {
		e.Status.History = append(e.Status.History, v1alpha1.HistoryEntry{
			Revision: strconv.Itoa(i) + "-1a2b3c4d",
			ComponentImages: []v1alpha1.ComponentImage{
				{Component: "web", Image: "ghcr.io/acme/checkout:" + strconv.Itoa(i)},
			},
		})
	}
	if err := c.Status().Update(ctx, e); err == nil {
		t.Fatalf("the API server accepted %d history entries; maxItems is not enforced",
			v1alpha1.MaxHistoryEntries+1)
	}

	// And the bound is the only thing refused: the same shape at the bound is
	// accepted, so a red test above means the bound and not the new field.
	e.Status.History = e.Status.History[:v1alpha1.MaxHistoryEntries]
	if err := c.Status().Update(ctx, e); err != nil {
		t.Fatalf("the API server refused %d history entries carrying componentImages: %v",
			v1alpha1.MaxHistoryEntries, err)
	}
}

// TestFinalizerSurvivesTheAPIServer: the deletion blocker is what keeps an
// Environment alive until its Kustomization is gone, and an object with a
// finalizer must go to Terminating rather than disappear.
func TestFinalizerSurvivesTheAPIServer(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	e := validEnvironment()
	e.Namespace = ns
	e.Generation = 0
	e.Finalizers = []string{Finalizer}
	if err := c.Create(ctx, e); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := c.Delete(ctx, e); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	var got v1alpha1.Environment
	if err := c.Get(ctx, client.ObjectKeyFromObject(e), &got); err != nil {
		t.Fatalf("the object vanished despite its finalizer: %v", err)
	}
	if got.DeletionTimestamp.IsZero() {
		t.Error("the object was not marked for deletion")
	}

	// And removing it lets the object go.
	got.Finalizers = nil
	if err := c.Update(ctx, &got); err != nil {
		t.Fatalf("removing the finalizer: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(e), &got); !apierrors.IsNotFound(err) {
		t.Errorf("the object survived the finalizer's removal: %v", err)
	}
}

// TestRollbackAnnotationIsAcceptedVerbatim: a rollback is porcelain over an
// annotation, so the API server has to store it exactly as written — including
// the slash in the key, which is a domain-prefixed name and not a path.
func TestRollbackAnnotationIsAcceptedVerbatim(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	e := validEnvironment()
	e.Namespace = ns
	e.Generation = 0
	e.Annotations = map[string]string{v1alpha1.AnnotationRollbackTo: "6-9f0a1b2c"}
	if err := c.Create(ctx, e); err != nil {
		t.Fatalf("creating: %v", err)
	}
	var got v1alpha1.Environment
	if err := c.Get(ctx, client.ObjectKeyFromObject(e), &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[v1alpha1.AnnotationRollbackTo] != "6-9f0a1b2c" {
		t.Errorf("the annotation came back as %q", got.Annotations[v1alpha1.AnnotationRollbackTo])
	}
}

// TestStatusSubresourceIsSeparate: a status write must not touch the spec, and
// a spec write must not touch the status. That separation is the whole reason
// ADR-0027 calls the subresource the prize.
func TestStatusSubresourceIsSeparate(t *testing.T) {
	c := envtestClient(t)
	ns := namespace(t, c)
	ctx := context.Background()

	p := validProject()
	p.Namespace = ns
	p.Generation = 0
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("creating: %v", err)
	}

	r := &ProjectReconciler{Client: c}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got v1alpha1.Project
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration %d does not describe generation %d",
			got.Status.ObservedGeneration, got.Generation)
	}
	before := got.Generation
	// A status write must not bump the generation: generation counts spec
	// changes, and ADR-0028 decision 2 makes it the revision number.
	time.Sleep(10 * time.Millisecond)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got.Generation != before {
		t.Errorf("a status write bumped the generation from %d to %d", before, got.Generation)
	}
}
