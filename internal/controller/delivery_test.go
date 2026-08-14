package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// These drive the real FluxDeliverer against controller-runtime's fake client
// and a fake pusher. What is worth pinning is everything the deliverer decides:
// what goes in the artifact, what the two Flux objects say, when a publish is
// skipped, when a name is refused, and what a teardown deletes in which order.

// fakePusher records the artifacts it was handed instead of uploading them.
type fakePusher struct {
	pushed   []artifact.Artifact
	cred     registry.Credential
	insecure bool
	err      error
}

func (f *fakePusher) connect(cred registry.Credential, insecure bool) ArtifactPusher {
	f.cred, f.insecure = cred, insecure
	return f
}

func (f *fakePusher) Push(_ context.Context, a artifact.Artifact) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.pushed = append(f.pushed, a)
	return a.Reference(), nil
}

func (f *fakePusher) last(t *testing.T) artifact.Artifact {
	t.Helper()
	if len(f.pushed) == 0 {
		t.Fatal("nothing was published")
	}
	return f.pushed[len(f.pushed)-1]
}

const fluxNamespace = "kelson-system"

func testDeliverer(t *testing.T, push *fakePusher, objects ...client.Object) *FluxDeliverer {
	t.Helper()
	return &FluxDeliverer{
		Client:        newClient(t, objects...),
		Registry:      "ghcr.io/acme",
		FluxNamespace: fluxNamespace,
		Pusher:        push.connect,
		// A path that cannot exist, so the credential lookup takes the
		// anonymous branch instead of reading the test machine's docker login.
		RegistryConfig: t.TempDir() + "/absent/config.json",
	}
}

// testRevision is a rendered checkout/production at generation 7.
func testRevision(t *testing.T) Revision {
	t.Helper()
	mp, me := modelProject(validProject()), modelEnvironment(validEnvironment())
	resolved, errs := model.Resolve(mp, me)
	if len(errs) > 0 {
		t.Fatalf("resolving the fixture: %v", errs)
	}
	manifests, err := renderer.Render(resolved, clusterprofile.ClusterProfile{}, nil)
	if err != nil {
		t.Fatalf("rendering the fixture: %v", err)
	}
	hash, err := model.SpecHash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	return Revision{
		Project:              "checkout",
		Environment:          "production",
		EnvironmentNamespace: testNamespace,
		TargetNamespace:      resolved.Environment.Namespace,
		Generation:           7,
		SpecHash:             hash,
		Manifests:            manifests,
		Resolved:             resolved,
		FluxPresent:          true,
	}
}

func liveObject(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	key := types.NamespacedName{Namespace: fluxNamespace, Name: name}
	if err := c.Get(context.Background(), key, u); err != nil {
		t.Fatalf("reading %s %s back: %v", gvk.Kind, name, err)
	}
	return u
}

func nested(t *testing.T, u *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s has no %s", u.GetKind(), strings.Join(path, "."))
		return nil
	}
	return v
}

// TestDeliverPublishesAndEnsures is the happy path end to end: one artifact,
// two Flux objects, and a phase read back.
func TestDeliverPublishesAndEnsures(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)

	out, err := d.Deliver(context.Background(), testRevision(t))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !out.Published || out.Revision == "" || !strings.HasPrefix(out.Revision, "7-") {
		t.Fatalf("outcome = %+v, want a published revision at generation 7", out)
	}
	if out.Digest == "" || !strings.HasPrefix(out.Digest, "sha256:") {
		t.Errorf("digest = %q, want the artifact's own digest", out.Digest)
	}
	if len(out.Images) == 0 || out.Images[0] != "ghcr.io/acme/checkout:1.0.0" {
		t.Errorf("images = %v, want what the spec resolved to", out.Images)
	}
	// Nothing has been reconciled yet, so the honest phase is Committed.
	if out.Phase != v1alpha1.PhaseCommitted {
		t.Errorf("phase = %q, want %q before Flux has looked", out.Phase, v1alpha1.PhaseCommitted)
	}

	a := push.last(t)
	if a.Repository != "ghcr.io/acme/kelson/checkout-production" || a.Tag != out.Revision {
		t.Errorf("published %s:%s, want the ADR-0028 decision 2 shape", a.Repository, a.Tag)
	}
	if len(a.Files) == 0 {
		t.Error("the artifact holds no files")
	}

	// The OCIRepository is pinned to the tag just pushed and points at the
	// repository it was pushed to.
	repo := liveObject(t, d.Client, ociRepositoryGVK, "checkout-production")
	if got := nested(t, repo, "spec", "url"); got != "oci://ghcr.io/acme/kelson/checkout-production" {
		t.Errorf("OCIRepository url = %v", got)
	}
	if got := nested(t, repo, "spec", "ref", "tag"); got != out.Revision {
		t.Errorf("OCIRepository is pinned to %v, want the tag just pushed (%s)", got, out.Revision)
	}
	if _, found, _ := unstructured.NestedBool(repo.Object, "spec", "insecure"); found {
		t.Error("insecure must not be set for a registry nobody named as insecure")
	}
	if _, found, _ := unstructured.NestedMap(repo.Object, "spec", "secretRef"); found {
		t.Error("secretRef must not be set without --pull-secret")
	}

	ks := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	if got := nested(t, ks, "spec", "path"); got != "./" {
		t.Errorf("Kustomization path = %v, want the artifact root", got)
	}
	if got := nested(t, ks, "spec", "prune"); got != true {
		t.Errorf("prune = %v, want true", got)
	}
	// wait: true is what makes Ready mean healthy rather than applied, which is
	// what lets R1 report on workloads with no RBAC over them.
	if got := nested(t, ks, "spec", "wait"); got != true {
		t.Errorf("wait = %v, want true", got)
	}
	if got := nested(t, ks, "spec", "targetNamespace"); got != "checkout-production" {
		t.Errorf("targetNamespace = %v, want the resolved namespace", got)
	}
	if got := nested(t, ks, "spec", "sourceRef", "kind"); got != "OCIRepository" {
		t.Errorf("sourceRef kind = %v", got)
	}
	if got := nested(t, ks, "spec", "sourceRef", "name"); got != "checkout-production" {
		t.Errorf("sourceRef name = %v; the pair shares one name so it cannot be mismatched", got)
	}
	if _, found, _ := unstructured.NestedMap(ks.Object, "spec", "decryption"); found {
		t.Error("decryption must not be set for the cluster secret backend")
	}

	// Both objects carry the provenance, including the label that makes the
	// reverse lookup and the name-conflict check possible.
	for _, u := range []*unstructured.Unstructured{repo, ks} {
		labels := u.GetLabels()
		if labels[delivery.LabelManagedBy] != delivery.ManagedByKelson {
			t.Errorf("%s is not marked as kelson's: %v", u.GetKind(), labels)
		}
		if labels[delivery.LabelProject] != "checkout" || labels[delivery.LabelEnvironment] != "production" {
			t.Errorf("%s provenance = %v", u.GetKind(), labels)
		}
		if labels[delivery.LabelEnvironmentNamespace] != testNamespace {
			t.Errorf("%s environment-namespace = %q, want %q",
				u.GetKind(), labels[delivery.LabelEnvironmentNamespace], testNamespace)
		}
	}
}

// TestDeliverAnnotatesTheArtifact: an artifact in a registry has to answer
// "whose is this, and of what?" without being unpacked.
func TestDeliverAnnotatesTheArtifact(t *testing.T) {
	push := &fakePusher{}
	rev := testRevision(t)
	if _, err := testDeliverer(t, push).Deliver(context.Background(), rev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	manifest := string(push.last(t).Manifest)
	for _, want := range []string{
		`"kelson.dev/project":"checkout"`,
		`"kelson.dev/environment":"production"`,
		`"kelson.dev/spec-hash":"` + rev.SpecHash + `"`,
		`"kelson.dev/generation":"7"`,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the artifact manifest does not carry %s:\n%s", want, manifest)
		}
	}
}

// TestDeliverSopsWritesTheDecryptionBlock is ADR-0028 decision 7: kelson now
// owns the Kustomization, so the decryption block is not skippable and the
// "committed but never decrypted" failure is structurally unreachable.
func TestDeliverSopsWritesTheDecryptionBlock(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)
	rev := testRevision(t)
	rev.Resolved.Environment.Secrets = model.SecretBackend{
		Backend:      model.SecretsSOPS,
		AgeKeySecret: "sops-age",
	}

	if _, err := d.Deliver(context.Background(), rev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	ks := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	if got := nested(t, ks, "spec", "decryption", "provider"); got != "sops" {
		t.Errorf("decryption provider = %v", got)
	}
	if got := nested(t, ks, "spec", "decryption", "secretRef", "name"); got != "sops-age" {
		t.Errorf("decryption secretRef = %v, want the environment's ageKeySecret", got)
	}
}

// TestDeliverInsecureAndPullSecret: plain HTTP is an operator's named exception
// and never a guess, and the pull credential is a different one from the push
// credential because a different process uses it.
func TestDeliverInsecureAndPullSecret(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)
	d.Registry = "localhost:5000"
	d.InsecureRegistries = []string{"localhost:5000"}
	d.PullSecret = "ghcr-pull"

	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !push.insecure {
		t.Error("the push was not told the registry speaks plain HTTP")
	}
	repo := liveObject(t, d.Client, ociRepositoryGVK, "checkout-production")
	if got := nested(t, repo, "spec", "insecure"); got != true {
		t.Errorf("OCIRepository insecure = %v, want true for a named plain-HTTP host", got)
	}
	if got := nested(t, repo, "spec", "secretRef", "name"); got != "ghcr-pull" {
		t.Errorf("OCIRepository secretRef = %v, want --pull-secret", got)
	}

	// And a registry nobody named is never downgraded.
	d.InsecureRegistries = nil
	push.insecure = true
	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if push.insecure {
		t.Error("a registry nobody named as insecure was pushed to over plain HTTP")
	}
}

// TestDeliverIsIdempotent: a second reconcile of an unchanged spec must not
// publish again, and must not change either object.
func TestDeliverIsIdempotent(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)

	first, err := d.Deliver(context.Background(), testRevision(t))
	if err != nil {
		t.Fatalf("first Deliver: %v", err)
	}
	before := specOf(t, liveObject(t, d.Client, kustomizationGVK, "checkout-production"))

	rev := testRevision(t)
	rev.Observed = first.Revision
	second, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if len(push.pushed) != 1 {
		t.Errorf("the second reconcile pushed again (%d pushes total)", len(push.pushed))
	}
	if second.Published {
		t.Error("the second reconcile claims a new publish; history would grow on a timer")
	}
	if second.Revision != first.Revision {
		t.Errorf("revision moved from %s to %s with no spec change", first.Revision, second.Revision)
	}
	if after := specOf(t, liveObject(t, d.Client, kustomizationGVK, "checkout-production")); after != before {
		t.Errorf("the re-apply changed the Kustomization:\n before %v\n after  %v", before, after)
	}
}

// TestDeliverRepublishesWhenTheLiveObjectDisagrees: the skip is guarded on the
// *cluster*, not on the status. A status claiming a revision only proves a
// previous reconcile got as far as writing the status, and an OCIRepository
// that was deleted or edited would otherwise leave the environment stuck
// forever on a claim nothing backs.
func TestDeliverRepublishesWhenTheLiveObjectDisagrees(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)

	first, err := d.Deliver(context.Background(), testRevision(t))
	if err != nil {
		t.Fatal(err)
	}
	repo := liveObject(t, d.Client, ociRepositoryGVK, "checkout-production")
	if err := d.Client.Delete(context.Background(), repo); err != nil {
		t.Fatalf("deleting the OCIRepository: %v", err)
	}

	rev := testRevision(t)
	rev.Observed = first.Revision
	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !out.Published || len(push.pushed) != 2 {
		t.Errorf("a missing OCIRepository did not trigger a republish: published=%v pushes=%d",
			out.Published, len(push.pushed))
	}
	if got := nested(t, liveObject(t, d.Client, ociRepositoryGVK, "checkout-production"), "spec", "ref", "tag"); got != out.Revision {
		t.Errorf("the OCIRepository was not recreated pinned to %s, got %v", out.Revision, got)
	}
}

// TestDeliverRollbackSkipsPublishing is ADR-0028 decision 5: steps 3 and 4 do
// not run, so the current spec cannot be republished over the thing you just
// rolled back to. All that moves is the pointer.
func TestDeliverRollbackSkipsPublishing(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)

	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatal(err)
	}
	rev := testRevision(t)
	rev.Generation = 8
	rev.PinnedTo = "6-9f0a1b2c"
	rev.Manifests = nil // the reconciler does not render under a rollback

	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(push.pushed) != 1 {
		t.Errorf("a rollback published something (%d pushes)", len(push.pushed))
	}
	if !out.RolledBack || out.Revision != "6-9f0a1b2c" || out.Published {
		t.Errorf("outcome = %+v, want a pinned rollback with nothing published", out)
	}
	if got := nested(t, liveObject(t, d.Client, ociRepositoryGVK, "checkout-production"), "spec", "ref", "tag"); got != "6-9f0a1b2c" {
		t.Errorf("the OCIRepository is pinned to %v, want the rollback target", got)
	}
}

// TestDeliverRefusesAConflictingName: two Environments of the same name binding
// Projects of the same name, in two namespaces, resolve to one object name in
// one Flux namespace. Applying over it would silently redirect somebody else's
// deployment.
func TestDeliverRefusesAConflictingName(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)

	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatal(err)
	}
	before := specOf(t, liveObject(t, d.Client, ociRepositoryGVK, "checkout-production"))

	other := testRevision(t)
	other.EnvironmentNamespace = "somebody-else"
	_, err := d.Deliver(context.Background(), other)
	if err == nil {
		t.Fatal("a second environment took the first one's objects")
	}
	de, ok := asDeliveryError(err)
	if !ok || de.Reason != v1alpha1.ReasonNameConflict {
		t.Fatalf("error = %v, want %s", err, v1alpha1.ReasonNameConflict)
	}
	if !strings.Contains(de.Message, testNamespace) || !strings.Contains(de.Message, "somebody-else") {
		t.Errorf("the refusal does not name both namespaces: %s", de.Message)
	}
	if de.Retry != 0 || de.Backoff {
		t.Error("a name conflict must not requeue: nothing changes on its own")
	}
	if after := specOf(t, liveObject(t, d.Client, ociRepositoryGVK, "checkout-production")); after != before {
		t.Error("the refused reconcile wrote to the first environment's objects anyway")
	}
}

// TestDeliverAdoptsAnUnlabelledObject: an object kelson wrote before the label
// existed, or one an operator hand-wrote, must be converged on rather than
// refused — the alternative is a controller that will not reconcile an object
// it would otherwise have created identically.
func TestDeliverAdoptsAnUnlabelledObject(t *testing.T) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(ociRepositoryGVK)
	existing.SetName("checkout-production")
	existing.SetNamespace(fluxNamespace)

	push := &fakePusher{}
	d := testDeliverer(t, push, existing)
	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatalf("an unlabelled object was refused instead of adopted: %v", err)
	}
}

// TestDeliverRefusesWithoutFlux: a cluster with no Flux has nothing that would
// reconcile what kelson published, so publishing would be a push into an empty
// room. It is the expected state of a fresh cluster, so it waits on a timer.
func TestDeliverRefusesWithoutFlux(t *testing.T) {
	push := &fakePusher{}
	rev := testRevision(t)
	rev.FluxPresent = false

	_, err := testDeliverer(t, push).Deliver(context.Background(), rev)
	de, ok := asDeliveryError(err)
	if !ok || de.Reason != v1alpha1.ReasonFluxNotInstalled {
		t.Fatalf("error = %v, want %s", err, v1alpha1.ReasonFluxNotInstalled)
	}
	if len(push.pushed) != 0 {
		t.Error("something was published into a cluster that cannot reconcile it")
	}
	if de.Backoff {
		t.Error("a missing Flux must never crash-loop the controller")
	}
	if !strings.Contains(de.Message, "kelson install") {
		t.Errorf("the refusal does not name the fix: %s", de.Message)
	}
}

// TestDeliverClassifiesPushFailures is the taxonomy at the registry boundary:
// the registry refused the caller, the registry never answered, or the
// repository is not one anything can push to.
func TestDeliverClassifiesPushFailures(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{
			name:   "a credential the registry refused",
			err:    &artifact.DeniedError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Doing: "pushing"},
			reason: v1alpha1.ReasonPushDenied,
		},
		{
			name:   "a registry that never answered",
			err:    &artifact.UnreachableError{Doing: "PUT https://ghcr.io/v2/", Err: errors.New("connection refused")},
			reason: v1alpha1.ReasonRegistryUnreachable,
		},
		{
			name:   "a repository nothing can push to",
			err:    artifact.Error{Reason: artifact.ReasonRepositoryInvalid, Message: "carries a tag"},
			reason: v1alpha1.ReasonArtifactRefInvalid,
		},
		{
			// Unclassified is treated as transient: a retry costs a request,
			// and not retrying something that would have worked costs a deploy.
			name:   "something nobody anticipated",
			err:    errors.New("the moon is in the wrong phase"),
			reason: v1alpha1.ReasonRegistryUnreachable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			push := &fakePusher{err: tc.err}
			_, err := testDeliverer(t, push).Deliver(context.Background(), testRevision(t))
			de, ok := asDeliveryError(err)
			if !ok {
				t.Fatalf("error %v is not part of the taxonomy", err)
			}
			if de.Reason != tc.reason {
				t.Errorf("reason = %s, want %s", de.Reason, tc.reason)
			}
		})
	}
}

// TestObserveMapsTheKustomization: the phase comes from
// internal/delivery/flux's mapping and not a second one, so what the controller
// says about its own Kustomization is what `kelson status` says about any other.
func TestObserveMapsTheKustomization(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)
	first, err := d.Deliver(context.Background(), testRevision(t))
	if err != nil {
		t.Fatal(err)
	}

	// Flux applies it and reports Ready, with the OCI revision spelling.
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
		t.Fatalf("updating the Kustomization status: %v", err)
	}

	rev := testRevision(t)
	rev.Observed = first.Revision
	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Phase != v1alpha1.PhaseHealthy {
		t.Fatalf("phase = %q (%s), want %q", out.Phase, out.Cause, v1alpha1.PhaseHealthy)
	}
}

// TestObserveReportsARejection: a change Flux processed and refused is a
// different user action from one that is live and unhealthy, and the cause is
// Flux's own words.
func TestObserveReportsARejection(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)
	first, err := d.Deliver(context.Background(), testRevision(t))
	if err != nil {
		t.Fatal(err)
	}

	ks := liveObject(t, d.Client, kustomizationGVK, "checkout-production")
	if err := unstructured.SetNestedMap(ks.Object, map[string]any{
		"lastAttemptedRevision": first.Revision + "@sha256:abc",
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed",
				"message": "kustomize build failed: accumulating resources"},
		},
	}, "status"); err != nil {
		t.Fatal(err)
	}
	if err := d.Client.Update(context.Background(), ks); err != nil {
		t.Fatal(err)
	}

	rev := testRevision(t)
	rev.Observed = first.Revision
	out, err := d.Deliver(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != v1alpha1.PhaseRejected {
		t.Fatalf("phase = %q, want %q", out.Phase, v1alpha1.PhaseRejected)
	}
	if !strings.Contains(out.Cause, "accumulating resources") {
		t.Errorf("the cause does not relay Flux's own message: %q", out.Cause)
	}
}

// TestTeardownDeletesTheKustomizationFirst is the whole of the deletion order
// decision: deleting the Kustomization is what prunes the workloads, and doing
// it second would leave it pointing at a source that no longer exists, so it
// would stop reconciling and prune nothing.
func TestTeardownDeletesTheKustomizationFirst(t *testing.T) {
	push := &fakePusher{}
	d := testDeliverer(t, push)
	if _, err := d.Deliver(context.Background(), testRevision(t)); err != nil {
		t.Fatal(err)
	}

	order := &deleteOrder{}
	d.Client = interceptDeletes(t, d.Client, order)
	if err := d.Teardown(context.Background(), "checkout", "production"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(order.kinds) != 2 || order.kinds[0] != "Kustomization" || order.kinds[1] != "OCIRepository" {
		t.Fatalf("deleted %v, want the Kustomization first", order.kinds)
	}

	// And it is idempotent: teardown runs on every reconcile of a deleting
	// Environment until the finalizer clears, so "already gone" is success.
	if err := d.Teardown(context.Background(), "checkout", "production"); err != nil {
		t.Fatalf("a second Teardown must be a no-op, got %v", err)
	}
}

// TestTeardownOfSomethingNeverCreated is the same property from the other side:
// an Environment refused before it ever published still has to be deletable.
func TestTeardownOfSomethingNeverCreated(t *testing.T) {
	d := testDeliverer(t, &fakePusher{})
	if err := d.Teardown(context.Background(), "checkout", "production"); err != nil {
		t.Fatalf("Teardown of a pair that was never created must succeed, got %v", err)
	}
}

// specOf is an object's spec and labels as a comparable string. The fake
// client bumps resourceVersion on every apply, no-op or not, where a real API
// server does not — so what an idempotent re-apply is actually asserted on is
// the content, which is the property that matters either way.
func specOf(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	return fmt.Sprintf("%v|%v", u.Object["spec"], u.GetLabels())
}

type deleteOrder struct{ kinds []string }

// interceptDeletes wraps a client so the deletion *order* is observable, which
// is the property the teardown is actually about.
func interceptDeletes(t *testing.T, base client.Client, order *deleteOrder) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(base.Scheme()).
		WithObjects(existingFluxObjects(t, base)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				order.kinds = append(order.kinds, obj.GetObjectKind().GroupVersionKind().Kind)
				return nil
			},
		}).
		Build()
}

func existingFluxObjects(t *testing.T, c client.Client) []client.Object {
	t.Helper()
	var out []client.Object
	for _, gvk := range []schema.GroupVersionKind{kustomizationGVK, ociRepositoryGVK} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		key := types.NamespacedName{Namespace: fluxNamespace, Name: "checkout-production"}
		if err := c.Get(context.Background(), key, u); err != nil {
			t.Fatalf("reading %s back: %v", gvk.Kind, err)
		}
		u.SetResourceVersion("")
		out = append(out, u)
	}
	return out
}
