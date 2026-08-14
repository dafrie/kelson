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

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "deploy", "crds")},
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
	name := strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	name = strings.ReplaceAll(name, "/", "-")
	if len(name) > 60 {
		name = name[:60]
	}
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
