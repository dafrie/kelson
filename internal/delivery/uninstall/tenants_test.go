package uninstall

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// TestPlanKeepsACreatedNamespaceAnotherDeploymentLivesIn is issue #215.
//
// The namespace name is overridable, so a namespace kelson created for one
// environment can be the namespace a second deployment was later pointed at.
// The ownership annotation cannot see that: it records who created the
// namespace, not who is in it now. Deleting it anyway evicts the other tenant,
// data included — so the created-namespace licence is withdrawn, and the
// preview says whose resources are keeping it standing.
func TestPlanKeepsACreatedNamespaceAnotherDeploymentLivesIn(t *testing.T) {
	cases := []struct {
		name      string
		tenant    map[string]string
		wantOwner string
	}{
		{
			name:      "another project",
			tenant:    map[string]string{delivery.LabelProject: "grocery", delivery.LabelEnvironment: "production"},
			wantOwner: "grocery/production",
		},
		{
			name:      "another environment of the same project",
			tenant:    map[string]string{delivery.LabelEnvironment: "staging"},
			wantOwner: testProject + "/staging",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := newUninstaller(t, false,
				namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
				object("apps/v1", "Deployment", "web", testNS),
				object("v1", "ConfigMap", "tenant-settings", testNS, withLabels(tc.tenant)),
			)

			plan := planFor(t, u, testScope())
			if has(refs(plan.Targets), "Namespace/"+testNS) {
				t.Fatalf("the plan deletes a namespace another deployment lives in\nplan: %v\n"+
					"a namespace delete cascades, so this is the tenant's whole environment going with it", refs(plan.Targets))
			}
			if !has(refs(plan.Targets), "Deployment/"+testNS+"/web") {
				t.Errorf("the sweep stopped deleting this deployment's own resources\nplan: %v", refs(plan.Targets))
			}
			if has(refs(plan.Targets), "ConfigMap/"+testNS+"/tenant-settings") {
				t.Errorf("the plan deletes the other deployment's ConfigMap\nplan: %v", refs(plan.Targets))
			}
			if len(plan.Namespaces) != 1 {
				t.Fatalf("want exactly one swept namespace, got %d", len(plan.Namespaces))
			}
			reason := plan.Namespaces[0].Reason
			for _, want := range []string{"left behind", "1 resource(s)", tc.wantOwner} {
				if !strings.Contains(reason, want) {
					t.Errorf("the namespace stays but the preview does not say %q\nreason: %s\n"+
						"a survivor nobody is named for reads as a bug rather than a decision", want, reason)
				}
			}
		})
	}
}

// TestPlanDeletesACreatedNamespaceWhoseNeighboursAreNotKelsons keeps the new
// check from over-reaching. A namespace kelson created has always taken its
// unlabelled neighbours with it, and that is documented, previewed and
// unchanged: what is new is only the refusal to evict ANOTHER KELSON
// DEPLOYMENT. A resource carrying a project label but managed by something else
// is not one.
func TestPlanDeletesACreatedNamespaceWhoseNeighboursAreNotKelsons(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
		// No kelson labels at all.
		object("v1", "ConfigMap", "bystander", testNS, withLabels(map[string]string{
			delivery.LabelManagedBy:   "",
			delivery.LabelProject:     "",
			delivery.LabelEnvironment: "",
		})),
		// Another project's label, but not kelson's to begin with — the case
		// test/e2e plants as `other-project`.
		object("v1", "ConfigMap", "other-tool", testNS, withLabels(map[string]string{
			delivery.LabelManagedBy: "helm",
			delivery.LabelProject:   "grocery",
		})),
	)

	plan := planFor(t, u, testScope())
	if !has(refs(plan.Targets), "Namespace/"+testNS) {
		t.Fatalf("the namespace kelson created stays for neighbours that are not kelson deployments\nreason: %s\n"+
			"that would make every uninstall leave a namespace behind", plan.Namespaces[0].Reason)
	}
}

// TestPlanAllEnvironmentsIsNotItsOwnTenant: --all-environments removes every
// environment of the project, so the project's other environment in a shared
// namespace is not somebody else — it is part of this uninstall.
func TestPlanAllEnvironmentsIsNotItsOwnTenant(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
		object("v1", "ConfigMap", "staging-settings", testNS,
			withLabels(map[string]string{delivery.LabelEnvironment: "staging"})),
	)

	plan := planFor(t, u, Scope{Project: testProject, AllEnvironments: true})
	if !has(refs(plan.Targets), "Namespace/"+testNS) {
		t.Fatalf("--all-environments treated the project's own staging environment as another tenant\nreason: %s",
			plan.Namespaces[0].Reason)
	}
}

// TestPlanKeepsACreatedNamespaceItCannotSeeAllOf is the failure direction.
//
// A kind the credentials may not list could be holding another deployment's
// resources, and there is no way to find out. Deleting the namespace on the
// strength of an answer that was never complete is the one mistake here that
// cannot be undone, so blindness keeps the namespace and says where kelson
// could not look.
func TestPlanKeepsACreatedNamespaceItCannotSeeAllOf(t *testing.T) {
	t.Run("a kind that cannot be listed", func(t *testing.T) {
		client := newFakeClient(
			namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
			object("apps/v1", "Deployment", "web", testNS),
		)
		client.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("nope"))
		})
		u, err := New(Options{Client: client, Catalog: testCatalog{}})
		if err != nil {
			t.Fatalf("new uninstaller: %v", err)
		}

		plan := planFor(t, u, testScope())
		assertNamespaceKeptBlind(t, plan, "configmaps")
	})

	t.Run("an API group discovery could not resolve", func(t *testing.T) {
		client := newFakeClient(
			namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
			object("apps/v1", "Deployment", "web", testNS),
		)
		u, err := New(Options{Client: client, Catalog: testCatalog{gaps: []string{"postgresql.cnpg.io/v1"}}})
		if err != nil {
			t.Fatalf("new uninstaller: %v", err)
		}

		plan := planFor(t, u, testScope())
		assertNamespaceKeptBlind(t, plan, "postgresql.cnpg.io/v1")
	})
}

func assertNamespaceKeptBlind(t *testing.T, plan *Plan, want string) {
	t.Helper()
	if has(refs(plan.Targets), "Namespace/"+testNS) {
		t.Fatalf("the namespace was deleted on an answer kelson could not complete (%s unreadable)\nplan: %v", want, refs(plan.Targets))
	}
	if !has(refs(plan.Targets), "Deployment/"+testNS+"/web") {
		t.Errorf("an incomplete tenancy check stopped the sweep deleting kelson's own resources\nplan: %v", refs(plan.Targets))
	}
	reason := plan.Namespaces[0].Reason
	if !strings.Contains(reason, want) {
		t.Errorf("the namespace stays but the preview does not say where kelson could not look (%q)\nreason: %s", want, reason)
	}
}

// TestExecuteRefusesANamespaceAnotherDeploymentMovedInto is the race the
// plan-time check cannot cover: the tenant's first apply landed after the
// preview was printed. The delete refuses on the live reading, exactly as the
// re-labelled-object and replaced-object refusals do.
func TestExecuteRefusesANamespaceAnotherDeploymentMovedInto(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
	)
	plan := planFor(t, u, testScope())
	if !has(refs(plan.Targets), "Namespace/"+testNS) {
		t.Fatalf("fixture is wrong: the namespace should be in the plan")
	}

	// A second project is deployed into the shared namespace between the
	// preview and the delete.
	tenant := object("v1", "ConfigMap", "tenant-settings", testNS, withLabels(map[string]string{
		delivery.LabelProject:     "grocery",
		delivery.LabelEnvironment: "production",
	}))
	ctx := context.Background()
	if _, err := client.Resource(gvrFor(t, "ConfigMap")).Namespace(testNS).Create(ctx, tenant, metav1.CreateOptions{}); err != nil {
		t.Fatalf("planting the tenant: %v", err)
	}

	report, err := u.Execute(ctx, plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	bystanders := report.Bystanders()
	if len(bystanders) != 1 || bystanders[0].Ref.Kind != "Namespace" {
		t.Fatalf("bystanders = %+v, want the namespace left alone", bystanders)
	}
	for _, want := range []string{"left behind", "grocery/production"} {
		if !strings.Contains(bystanders[0].Detail, want) {
			t.Errorf("the refusal does not say %q\ndetail: %s", want, bystanders[0].Detail)
		}
	}
	if _, err := client.Resource(namespacesGVR).Get(ctx, testNS, metav1.GetOptions{}); err != nil {
		t.Fatalf("the namespace was deleted with another deployment in it: %v", err)
	}
	if _, err := client.Resource(gvrFor(t, "ConfigMap")).Namespace(testNS).
		Get(ctx, "tenant-settings", metav1.GetOptions{}); err != nil {
		t.Fatalf("the other deployment's ConfigMap is gone: %v", err)
	}
}

// TestExecuteRefusesANamespaceItCannotCheck: the same failure direction at
// delete time. A cluster whose API surface cannot be enumerated cannot answer
// "is anybody else in here", and an unanswered question is not a licence.
func TestExecuteRefusesANamespaceItCannotCheck(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
	)
	plan := planFor(t, u, testScope())

	// Discovery starts failing after the preview was computed.
	u.catalog = testCatalog{err: errors.New("the aggregated API is down")}

	report, err := u.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.Left != 1 {
		t.Fatalf("report = %+v, want the namespace left alone", report)
	}
	if _, err := client.Resource(namespacesGVR).Get(context.Background(), testNS, metav1.GetOptions{}); err != nil {
		t.Fatalf("the namespace was deleted without checking who was in it: %v", err)
	}
}
