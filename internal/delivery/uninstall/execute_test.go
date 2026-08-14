package uninstall

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// TestExecuteDeletesTheWholePlanInOrder is the happy path: every target goes,
// in the plan's order, and the report says so per object.
func TestExecuteDeletesTheWholePlanInOrder(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("gateway.networking.k8s.io/v1", "HTTPRoute", "web", testNS),
		object("apps/v1", "Deployment", "web", testNS),
		object("v1", "Service", "web", testNS),
		object("postgresql.cnpg.io/v1", "Cluster", "db", testNS),
	)
	deletes := recordDeletes(client)

	plan := planFor(t, u, testScope())
	report, err := u.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.Deleted != len(plan.Targets) || report.Left != 0 || report.Failed != 0 {
		t.Fatalf("report = %+v, want every target deleted", report)
	}
	want := []string{"httproutes/web", "deployments/web", "services/web", "clusters/db", "namespaces/" + testNS}
	if strings.Join(*deletes, ", ") != strings.Join(want, ", ") {
		t.Fatalf("deletes reached the API in the wrong order\n got: %v\nwant: %v", *deletes, want)
	}
}

// TestExecuteRefusesWhatStoppedBeingKelsons is the refuse-unmanaged rule at the
// moment it matters. A plan is a snapshot; between the preview and the delete
// somebody relabelled the ConfigMap. kelson re-reads, sees it is no longer its
// own, and leaves it — reported, not deleted.
func TestExecuteRefusesWhatStoppedBeingKelsons(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("v1", "ConfigMap", "settings", testNS),
	)
	plan := planFor(t, u, testScope())

	// Somebody takes the ConfigMap over after the preview was printed.
	relabel(t, client, "configmaps", testNS, "settings", map[string]string{delivery.LabelProject: "somebody-else"})

	report, err := u.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	bystanders := report.Bystanders()
	if len(bystanders) != 1 || bystanders[0].Ref.Name != "settings" {
		t.Fatalf("bystanders = %+v, want the relabelled ConfigMap left alone and reported", bystanders)
	}
	if bystanders[0].Detail == "" {
		t.Errorf("the refusal gives no reason; a resource silently skipped reads as a resource silently deleted")
	}
	if _, err := client.Resource(gvrFor(t, "ConfigMap")).Namespace(testNS).
		Get(context.Background(), "settings", metav1.GetOptions{}); err != nil {
		t.Fatalf("the relabelled ConfigMap was deleted anyway: %v", err)
	}
}

// TestExecuteRefusesANamespaceThatStoppedBeingKelsons: the namespace check is
// re-made at delete time too. It is the one deletion that cascades, so it gets
// the check twice.
func TestExecuteRefusesANamespaceThatStoppedBeingKelsons(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
	)
	plan := planFor(t, u, testScope())
	if !has(refs(plan.Targets), "Namespace/"+testNS) {
		t.Fatalf("fixture is wrong: the namespace should be in the plan")
	}

	// A second environment adopts the namespace between preview and delete.
	annotate(t, client, "namespaces", "", testNS, map[string]string{
		delivery.AnnNamespaceOwnership: delivery.NamespaceOwnershipAdopted,
	})

	report, err := u.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.Left != 1 {
		t.Fatalf("report = %+v, want the namespace left alone", report)
	}
	if _, err := client.Resource(namespacesGVR).Get(context.Background(), testNS, metav1.GetOptions{}); err != nil {
		t.Fatalf("the adopted namespace was deleted anyway: %v", err)
	}
}

// TestExecuteIsIdempotent: the second run of an uninstall finds everything gone
// and succeeds. "Nothing to do" is an outcome, not a failure — a command that
// errored the second time would be one nobody could safely put in a script.
func TestExecuteIsIdempotent(t *testing.T) {
	u, _ := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("apps/v1", "Deployment", "web", testNS),
		object("v1", "Service", "web", testNS),
	)
	plan := planFor(t, u, testScope())
	if _, err := u.Execute(context.Background(), plan); err != nil {
		t.Fatalf("first execute: %v", err)
	}

	// Re-plan against the emptied cluster.
	second := planFor(t, u, testScope())
	if !second.Empty() {
		t.Fatalf("the second plan still has targets: %v", refs(second.Targets))
	}
	// And replaying the FIRST plan — the case where two operators run the same
	// preview — reports gone, not failed.
	report, err := u.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("replaying the first plan: %v", err)
	}
	if report.Gone != len(plan.Targets) || report.Failed != 0 {
		t.Fatalf("report = %+v, want everything reported as already gone", report)
	}
}

// TestExecuteContinuesPastAFailure: one kind the credentials may not delete must
// not strand everything after it, and the run must still exit non-zero.
func TestExecuteContinuesPastAFailure(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("gateway.networking.k8s.io/v1", "HTTPRoute", "web", testNS),
		object("apps/v1", "Deployment", "web", testNS),
		object("v1", "Service", "web", testNS),
	)
	client.PrependReactor("delete", "httproutes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "gateway.networking.k8s.io", Resource: "httproutes"}, "web", errors.New("no delete on httproutes"))
	})

	plan := planFor(t, u, testScope())
	report, err := u.Execute(context.Background(), plan)
	if err == nil {
		t.Fatalf("a refused delete produced no error; the exit code would claim success")
	}
	var de delivery.Error
	if !errors.As(err, &de) || de.Code != delivery.ErrApplyFailed {
		t.Errorf("the failure is not a structured delivery error: %v", err)
	}
	if report.Failed != 1 {
		t.Errorf("report = %+v, want exactly one failure", report)
	}
	if report.Deleted != len(plan.Targets)-1 {
		t.Errorf("report = %+v, want everything after the failure still deleted", report)
	}
}

// TestExecuteRefusesAReplacedObject: the UID precondition. A resource deleted
// and re-created between the plan and the delete is a different object, and
// deleting it on the strength of a check against its predecessor is the race
// the precondition exists to lose safely.
func TestExecuteRefusesAReplacedObject(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("v1", "ConfigMap", "settings", testNS),
	)
	plan := planFor(t, u, testScope())
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "configmaps"}, "settings", errors.New("UID precondition failed"))
	})

	report, err := u.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.Left != 1 || report.Failed != 0 {
		t.Fatalf("report = %+v, want the replaced object left alone rather than failed", report)
	}
}

// TestExecuteSendsAUIDPrecondition proves the precondition is actually on the
// wire, not merely handled when the server sends a conflict back.
func TestExecuteSendsAUIDPrecondition(t *testing.T) {
	u, client := newUninstaller(t, false,
		namespaceObject(testNS, delivery.NamespaceOwnershipCreated),
		object("v1", "ConfigMap", "settings", testNS),
	)
	var seen *metav1.DeleteOptions
	client.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if da, ok := action.(k8stesting.DeleteActionImpl); ok {
			opts := da.DeleteOptions
			seen = &opts
		}
		return false, nil, nil
	})

	plan := planFor(t, u, testScope())
	if _, err := u.Execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if seen == nil || seen.Preconditions == nil || seen.Preconditions.UID == nil || *seen.Preconditions.UID == "" {
		t.Fatalf("the delete carried no UID precondition: %+v", seen)
	}
}

// --- helpers ----------------------------------------------------------------

// recordDeletes returns a pointer to the "<resource>/<name>" log of deletes, in
// the order they reached the API.
func recordDeletes(client *dynamicfake.FakeDynamicClient) *[]string {
	log := &[]string{}
	client.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		da, ok := action.(k8stesting.DeleteActionImpl)
		if !ok {
			return false, nil, nil
		}
		*log = append(*log, da.GetResource().Resource+"/"+da.Name)
		return false, nil, nil
	})
	return log
}

func relabel(t *testing.T, client *dynamicfake.FakeDynamicClient, resource, namespace, name string, kv map[string]string) {
	t.Helper()
	mutateLive(t, client, resource, namespace, name, func(obj *unstructured.Unstructured) {
		withLabels(kv)(obj)
	})
}

func annotate(t *testing.T, client *dynamicfake.FakeDynamicClient, resource, namespace, name string, kv map[string]string) {
	t.Helper()
	mutateLive(t, client, resource, namespace, name, func(obj *unstructured.Unstructured) {
		withAnnotations(kv)(obj)
	})
}

func mutateLive(t *testing.T, client *dynamicfake.FakeDynamicClient, resource, namespace, name string, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	gvr := resourceGVR(t, resource)
	ctx := context.Background()
	obj, err := client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s/%s: %v", resource, name, err)
	}
	mutate(obj)
	if _, err := client.Resource(gvr).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating %s/%s: %v", resource, name, err)
	}
}

// resourceGVR maps a plural resource name back to its GVR for the test kinds.
func resourceGVR(t *testing.T, resource string) schema.GroupVersionResource {
	t.Helper()
	for _, k := range testKinds {
		gvr, _ := meta.UnsafeGuessKindToResource(k.gvk)
		if gvr.Resource == resource {
			return gvr
		}
	}
	t.Fatalf("no test kind with resource %q", resource)
	return schema.GroupVersionResource{}
}
