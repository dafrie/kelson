package uninstall

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// TestPlanOrdersByTier is the ordering contract: routes stop traffic first,
// workloads go before the data they read, data goes last of the namespaced
// resources, and the Namespace goes last of all.
func TestPlanOrdersByTier(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
		object("v1", "Service", "web", testNS),
		object("gateway.networking.k8s.io/v1", "HTTPRoute", "web", testNS),
		object("postgresql.cnpg.io/v1", "Cluster", "checkout-production-db", testNS),
		object("batch/v1", "CronJob", "nightly", testNS),
	)

	plan := planFor(t, u, testScope())
	got := refs(plan.Targets)
	want := []string{
		"HTTPRoute/checkout-production/web",
		"CronJob/checkout-production/nightly",
		"Deployment/checkout-production/web",
		"Service/checkout-production/web",
		"Cluster/checkout-production/checkout-production-db",
		"Namespace/checkout-production",
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("deletion order is wrong\n got: %v\nwant: %v\n"+
			"the order is the contract: a route deleted after its workload serves 503s, and a data service "+
			"deleted before the workloads that read it breaks them on the way out", got, want)
	}
}

// TestPlanSelectsOnlyWhatKelsonOwns is the additivity claim as a unit test: the
// plan is exactly the labelled set for this scope and nothing else. The
// bystanders here are the same ones test/e2e/additivity_test.go plants.
func TestPlanSelectsOnlyWhatKelsonOwns(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
		// No kelson labels at all.
		object("v1", "ConfigMap", "bystander", testNS, withLabels(map[string]string{
			delivery.LabelManagedBy:   "",
			delivery.LabelProject:     "",
			delivery.LabelEnvironment: "",
		})),
		// Another project.
		object("v1", "ConfigMap", "other-project", testNS, withLabels(map[string]string{
			delivery.LabelProject: "somebody-else",
		})),
		// Same project, another environment.
		object("v1", "ConfigMap", "other-env", testNS, withLabels(map[string]string{
			delivery.LabelEnvironment: "staging",
		})),
		// Labelled for kelson but managed by something else — the case a
		// selector that dropped managed-by would get wrong.
		object("v1", "ConfigMap", "other-tool", testNS, withLabels(map[string]string{
			delivery.LabelManagedBy: "helm",
		})),
	)

	plan := planFor(t, u, testScope())
	got := refs(plan.Targets)
	for _, unwanted := range []string{
		"ConfigMap/checkout-production/bystander",
		"ConfigMap/checkout-production/other-project",
		"ConfigMap/checkout-production/other-env",
		"ConfigMap/checkout-production/other-tool",
	} {
		if has(got, unwanted) {
			t.Errorf("the plan includes %s, which kelson does not own\nplan: %v\n"+
				"this is the additivity claim failing at the planning stage", unwanted, got)
		}
	}
	if !has(got, "Deployment/checkout-production/web") {
		t.Errorf("the plan does not include the Deployment kelson deployed\nplan: %v", got)
	}
}

// TestPlanSkipsDerivedResources: Pods and ReplicaSets carry the same provenance
// labels (the renderer stamps them onto pod templates) and are garbage-collected
// by the workload that owns them. Listing them as separate deletions would
// misreport the size of the operation.
func TestPlanSkipsDerivedResources(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
		object("apps/v1", "ReplicaSet", "web-5d9f", testNS),
		object("v1", "Pod", "web-5d9f-abcde", testNS),
	)

	plan := planFor(t, u, testScope())
	for _, ref := range refs(plan.Targets) {
		if strings.HasPrefix(ref, "Pod/") || strings.HasPrefix(ref, "ReplicaSet/") {
			t.Errorf("the plan includes %s; derived resources go with their owner, not as separate deletions", ref)
		}
	}
}

// TestPlanDeletesNamespaceOnlyWhenKelsonCreatedIt is the namespace rule. Only
// the delivery plane's recorded "created" is licence; every other reading leaves
// the namespace standing with a stated reason.
func TestPlanDeletesNamespaceOnlyWhenKelsonCreatedIt(t *testing.T) {
	cases := []struct {
		name      string
		ownership string
		want      bool
	}{
		{"created", delivery.NamespaceOwnershipCreated, true},
		{"adopted", delivery.NamespaceOwnershipAdopted, false},
		{"declared only", delivery.NamespaceOwnershipDeclared, false},
		{"no annotation", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := namespaceObject(testNS, tc.ownership)
			if tc.ownership == "" {
				ns.SetAnnotations(nil)
			}
			u, _ := newUninstaller(t, false, ns, object("apps/v1", "Deployment", "web", testNS))

			plan := planFor(t, u, testScope())
			got := has(refs(plan.Targets), "Namespace/"+testNS)
			if got != tc.want {
				t.Fatalf("namespace deletion = %v, want %v for ownership %q\nplan: %v",
					got, tc.want, tc.ownership, refs(plan.Targets))
			}
			if len(plan.Namespaces) != 1 {
				t.Fatalf("want exactly one swept namespace, got %d", len(plan.Namespaces))
			}
			if !tc.want && plan.Namespaces[0].Reason == "" {
				t.Errorf("the namespace stays but the plan gives no reason; an unexplained survivor reads as a bug")
			}
		})
	}
}

// TestPlanKeepDataExcludesDataAndTheNamespace: --keep-data has to keep the
// namespace too, because deleting the namespace would delete the data in it.
func TestPlanKeepDataExcludesDataAndTheNamespace(t *testing.T) {
	u, _ := newUninstaller(t, true,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
		object("postgresql.cnpg.io/v1", "Cluster", "checkout-production-db", testNS),
		object("valkey.io/v1alpha1", "ValkeyCluster", "checkout-production-cache", testNS),
		object("v1", "PersistentVolumeClaim", "uploads", testNS),
	)

	plan := planFor(t, u, testScope())
	got := refs(plan.Targets)
	if has(got, "Namespace/"+testNS) {
		t.Errorf("--keep-data deleted the namespace, which would delete the data it kept\nplan: %v", got)
	}
	for _, kept := range []string{
		"Cluster/checkout-production/checkout-production-db",
		"ValkeyCluster/checkout-production/checkout-production-cache",
		"PersistentVolumeClaim/checkout-production/uploads",
	} {
		if has(got, kept) {
			t.Errorf("--keep-data plans to delete %s", kept)
		}
		if !has(refs(plan.Kept), kept) {
			t.Errorf("--keep-data does not report %s as kept; silence would read as 'it was not there'", kept)
		}
	}
	if plan.Namespaces[0].Reason == "" {
		t.Errorf("--keep-data keeps the namespace without saying why")
	}
}

// TestPlanNamesDataCollateral: a CloudNativePG Cluster's PVCs are destroyed by
// the operator when the Cluster goes. kelson does not delete them — they are not
// kelson's — but a preview that does not name them understates the loss.
func TestPlanNamesDataCollateral(t *testing.T) {
	pvc := object("v1", "PersistentVolumeClaim", "checkout-production-db-1", testNS,
		withLabels(map[string]string{
			// The operator's labels, not kelson's: this PVC is CloudNativePG's.
			delivery.LabelManagedBy:   "cloudnative-pg",
			delivery.LabelProject:     "",
			delivery.LabelEnvironment: "",
			labelCNPGCluster:          "checkout-production-db",
		}))
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("postgresql.cnpg.io/v1", "Cluster", "checkout-production-db", testNS),
		pvc,
	)

	plan := planFor(t, u, testScope())
	if has(refs(plan.Targets), "PersistentVolumeClaim/checkout-production/checkout-production-db-1") {
		t.Fatalf("kelson plans to delete the operator's PVC; it carries no kelson provenance and is the operator's to remove")
	}
	var cluster *Target
	for i := range plan.Targets {
		if plan.Targets[i].Ref.Kind == "Cluster" {
			cluster = &plan.Targets[i]
		}
	}
	if cluster == nil {
		t.Fatalf("the CloudNativePG Cluster is not in the plan\nplan: %v", refs(plan.Targets))
	}
	if cluster.Note == "" {
		t.Errorf("the Cluster carries no irreversibility note; the data tier is the only step nothing can undo")
	}
	if !has(cluster.Collateral, "PersistentVolumeClaim/checkout-production-db-1") {
		t.Errorf("the Cluster does not name the volume that goes with it: %v", cluster.Collateral)
	}
}

// TestPlanFlagsManagedSecrets: a Secret written by `kelson secret set` is
// removed with everything else and gets its own line, because kelson stores no
// copy of the value (ADR-0009) — the cluster was it.
func TestPlanFlagsManagedSecrets(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("v1", "Secret", "checkout-db", testNS, withLabels(map[string]string{LabelManagedSecret: "true"})),
		object("v1", "Secret", "rendered", testNS),
	)

	plan := planFor(t, u, testScope())
	managed, plain := 0, 0
	for _, target := range plan.Targets {
		if target.Ref.Kind != "Secret" {
			continue
		}
		if target.ManagedSecret {
			managed++
			continue
		}
		plain++
	}
	if managed != 1 || plain != 1 {
		t.Fatalf("managed=%d plain=%d, want 1 and 1: both are deleted, and only the managed one is flagged", managed, plain)
	}
}

// TestPlanWarnsAboutFluxReconciledResources. Deleting what a reconciler owns is
// temporary — it puts it back — and saying so before the delete is the whole
// value of the preview.
func TestPlanWarnsAboutFluxReconciledResources(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS, withLabels(map[string]string{
			"kustomize.toolkit.fluxcd.io/name":      "checkout",
			"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
		})),
	)

	plan := planFor(t, u, testScope())
	got := plan.Reconcilers()
	if len(got) != 1 || !strings.Contains(got[0], "flux-system/checkout") {
		t.Fatalf("reconcilers = %v, want the Flux Kustomization named; a deleted resource a reconciler owns comes back", got)
	}
}

// TestPlanAllEnvironments drops the environment from the selector and sweeps
// every namespace the project labelled.
func TestPlanAllEnvironments(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		namespaceObject("checkout-staging", delivery.NamespaceOwnershipCreated,
			withLabels(map[string]string{delivery.LabelEnvironment: "staging"})),
		object("apps/v1", "Deployment", "web", testNS),
		object("apps/v1", "Deployment", "web", "checkout-staging",
			withLabels(map[string]string{delivery.LabelEnvironment: "staging"})),
		object("apps/v1", "Deployment", "web", "other-production",
			withLabels(map[string]string{delivery.LabelProject: "somebody-else"})),
	)

	plan := planFor(t, u, Scope{Project: testProject, AllEnvironments: true})
	got := refs(plan.Targets)
	for _, want := range []string{
		"Deployment/checkout-production/web",
		"Deployment/checkout-staging/web",
		"Namespace/checkout-production",
		"Namespace/checkout-staging",
	} {
		if !has(got, want) {
			t.Errorf("--all-environments missed %s\nplan: %v", want, got)
		}
	}
	if has(got, "Deployment/other-production/web") {
		t.Errorf("--all-environments reached another project\nplan: %v", got)
	}
	if sel := (Scope{Project: testProject, AllEnvironments: true}).Selector(); strings.Contains(sel, delivery.LabelEnvironment) {
		t.Errorf("the --all-environments selector still pins an environment: %s", sel)
	}
}

// TestPlanListsByProvenanceSelector proves the selector is on the wire, not
// merely applied client-side afterwards. It is the difference between kelson
// asking the API server for its own resources and kelson reading every resource
// in the namespace and filtering — the second needs no more RBAC but does mean
// somebody else's Secret transits kelson's process for no reason, exactly as
// internal/secret argues for its own listing.
func TestPlanListsByProvenanceSelector(t *testing.T) {
	client := newFakeClient(
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
	)
	var selectors []string
	client.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if la, ok := action.(k8stesting.ListActionImpl); ok {
			selectors = append(selectors, la.ListRestrictions.Labels.String())
		}
		return false, nil, nil
	})
	u, err := New(Options{Client: client, Catalog: testCatalog{}})
	if err != nil {
		t.Fatalf("new uninstaller: %v", err)
	}

	planFor(t, u, testScope())
	if len(selectors) == 0 {
		t.Fatal("the sweep issued no list calls")
	}
	for _, sel := range selectors {
		// The CloudNativePG collateral query is the one list that is not a
		// provenance query, by design: those PVCs are the operator's.
		if strings.Contains(sel, labelCNPGCluster) {
			continue
		}
		for _, want := range []string{
			delivery.LabelManagedBy + "=" + delivery.ManagedByKelson,
			delivery.LabelProject + "=" + testProject,
			delivery.LabelEnvironment + "=" + testEnv,
		} {
			if !strings.Contains(sel, want) {
				t.Fatalf("a list went out with selector %q, which does not pin %q — kelson would be reading "+
					"resources that are not its own", sel, want)
			}
		}
	}
}

// TestPlanReportsUnreadableKinds: a sweep that could not read part of the API
// surface must say so. An incomplete sweep reported as a complete one is a lie
// about what is left in the cluster.
func TestPlanReportsUnreadableKinds(t *testing.T) {
	client := newFakeClient(
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
	)
	client.PrependReactor("list", "cronjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "cronjobs"}, "", errors.New("nope"))
	})
	u, err := New(Options{Client: client, Catalog: testCatalog{gaps: []string{"metrics.k8s.io/v1beta1"}}})
	if err != nil {
		t.Fatalf("new uninstaller: %v", err)
	}

	plan := planFor(t, u, testScope())
	joined := strings.Join(plan.Unreadable, "\n")
	if !strings.Contains(joined, "cronjobs") {
		t.Errorf("a forbidden kind was swept silently; unreadable: %q", joined)
	}
	if !strings.Contains(joined, "metrics.k8s.io/v1beta1") {
		t.Errorf("an undiscoverable API group was swept silently; unreadable: %q", joined)
	}
}

// TestScopeValidate holds the addressing rules: a scope that names nothing, or
// names two contradictory things, is refused before anything is read.
func TestScopeValidate(t *testing.T) {
	cases := []struct {
		name  string
		scope Scope
		want  string
	}{
		{"no project", Scope{Environment: "production"}, "addressed by project"},
		{"no environment", Scope{Project: "checkout"}, "addressed by environment"},
		{"both", Scope{Project: "checkout", Environment: "production", AllEnvironments: true}, "disagree"},
		{"env", Scope{Project: "checkout", Environment: "production"}, ""},
		{"all", Scope{Project: "checkout", AllEnvironments: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.scope.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("want valid, got %v", err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("want an error containing %q, got none", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			var de delivery.Error
			if !errors.As(err, &de) {
				t.Errorf("the refusal is not a structured delivery error: %T", err)
			}
		})
	}
}

// TestPlanEmptyWhenNothingIsDeployed: the second run of an uninstall, and the
// first run against an environment that was never deployed, are the same
// answer — nothing to do, no error.
func TestPlanEmptyWhenNothingIsDeployed(t *testing.T) {
	u, _ := newUninstaller(t, false)
	plan := planFor(t, u, testScope())
	if !plan.Empty() {
		t.Fatalf("want an empty plan against an empty cluster, got %v", refs(plan.Targets))
	}
	report, err := u.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("executing an empty plan: %v", err)
	}
	if report.Deleted != 0 || report.Failed != 0 {
		t.Fatalf("an empty plan deleted something: %+v", report)
	}
}
