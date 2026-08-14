package install

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// TestPlanRefusesWhatDetectionFound is the first half of "never modify a
// component kelson did not install": never double-install one either.
func TestPlanRefusesWhatDetectionFound(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	cl := newCluster()
	installer := newInstaller(t, cl, fetcher)

	plan, err := installer.Plan(context.Background(), Request{
		Components: []string{"cert-manager"},
		Profile: clusterprofile.ClusterProfile{
			CertManager: &clusterprofile.CertManager{Version: "v1.20.0"},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Empty() {
		t.Fatalf("plan has %d items, want none for a component that is already present", len(plan.Items))
	}
	if len(plan.Refusals) != 1 {
		t.Fatalf("refusals = %d, want 1", len(plan.Refusals))
	}
	r := plan.Refusals[0]
	if r.Outcome != clusterprofile.OutcomeYes {
		t.Fatalf("refusal outcome = %v, want yes", r.Outcome)
	}
	if !strings.Contains(r.Reason, "v1.20.0") {
		t.Fatalf("refusal reason %q does not name the detected version", r.Reason)
	}
	if len(fetcher.asked) != 0 {
		t.Fatalf("fetched %v; detection must decide eligibility before anything is downloaded", fetcher.asked)
	}
}

// TestPlanRefusesOnDetectionGap: a component the probe could not look for is
// Unknown, and installing on an unknown is exactly the guess the tri-state
// exists to prevent (issue #144).
func TestPlanRefusesOnDetectionGap(t *testing.T) {
	c, fetcher := pinnedFixture("cnpg", "cnpg-system", "cnpg", []byte(cnpgFixture))
	withComponents(t, c)
	installer := newInstaller(t, newCluster(), fetcher)

	plan, err := installer.Plan(context.Background(), Request{
		Components: []string{"cnpg"},
		Profile: clusterprofile.ClusterProfile{
			Incomplete: []clusterprofile.Gap{{Field: "cnpg", Reason: "forbidden: needs get on /apis"}},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Empty() {
		t.Fatal("planned an install for a component detection could not read")
	}
	if len(plan.Refusals) != 1 || plan.Refusals[0].Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("refusals = %+v, want one unknown", plan.Refusals)
	}
	if !strings.Contains(plan.Refusals[0].Reason, "needs get on /apis") {
		t.Fatalf("refusal %q does not name the permission that would settle it", plan.Refusals[0].Reason)
	}
}

// TestPlanRefusesDeferredComponent: a deferred row is a refusal with a reason
// and a follow-up, not an "unknown component" error.
func TestPlanRefusesDeferredComponent(t *testing.T) {
	installer := newInstaller(t, newCluster(), &fakeFetcher{})
	plan, err := installer.Plan(context.Background(), Request{Components: []string{"external-secrets"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Empty() || len(plan.Refusals) != 1 {
		t.Fatalf("plan = %+v, want a single refusal", plan)
	}
	if !strings.Contains(plan.Refusals[0].Reason, "#60") {
		t.Fatalf("refusal %q does not name the follow-up issue", plan.Refusals[0].Reason)
	}
}

// TestPlanRejectsUnknownComponent names the real answers instead of shrugging.
func TestPlanRejectsUnknownComponent(t *testing.T) {
	installer := newInstaller(t, newCluster(), &fakeFetcher{})
	_, err := installer.Plan(context.Background(), Request{Components: []string{"nginx"}})
	if err == nil {
		t.Fatal("planned an install of a component that is not in the table")
	}
	if !strings.Contains(err.Error(), "cert-manager") {
		t.Fatalf("error %q does not list what kelson can install", err)
	}
}

// TestPlanVerifiesDigest: bytes that are not the pinned bytes are not the thing
// this repository reviewed, and nothing is applied.
func TestPlanVerifiesDigest(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	fetcher.bodies[c.ManifestURL] = []byte(certManagerFixture + "\n# something else\n")
	cl := newCluster()
	installer := newInstaller(t, cl, fetcher)

	_, err := installer.Plan(context.Background(), Request{Components: []string{"cert-manager"}})
	if err == nil {
		t.Fatal("planned an install from bytes that do not match the pin")
	}
	if !strings.Contains(err.Error(), "pinned digest") || !strings.Contains(err.Error(), c.SHA256) {
		t.Fatalf("error %q does not state both digests", err)
	}
	if got := cl.applyLog(); len(got) != 0 {
		t.Fatalf("applied %v after a digest mismatch", got)
	}
}

// TestPlanFailsLoudlyWithoutNetwork: the air-gap case names the URL and applies
// nothing, rather than half-installing.
func TestPlanFailsLoudlyWithoutNetwork(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	fetcher.err = errors.New("dial tcp: lookup github.com: no such host")
	installer := newInstaller(t, newCluster(), fetcher)

	_, err := installer.Plan(context.Background(), Request{Components: []string{"cert-manager"}})
	if err == nil {
		t.Fatal("planned an install with no network")
	}
	if !strings.Contains(err.Error(), c.ManifestURL) {
		t.Fatalf("error %q does not name the URL it could not reach", err)
	}
}

// TestExecuteStampsProvenanceAndPreservesUpstreamLabels.
func TestExecuteStampsProvenanceAndPreservesUpstreamLabels(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	cl := newCluster()
	installer := newInstaller(t, cl, fetcher)

	plan, err := installer.Plan(context.Background(), Request{Components: []string{"cert-manager"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.ObjectCount() != 6 {
		t.Fatalf("planned %d objects, want the fixture's 6", plan.ObjectCount())
	}
	report, err := installer.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(report.Components) != 1 || !report.Components[0].Installed {
		t.Fatalf("report = %+v, want one installed component", report.Components)
	}
	if report.Components[0].Created != 6 {
		t.Fatalf("created = %d, want 6", report.Components[0].Created)
	}

	ns := cl.get(t, "Namespace", "", "cert-manager")
	if ns == nil {
		t.Fatal("the namespace was not applied")
	}
	if got := ns.GetLabels()[LabelComponent]; got != "cert-manager" {
		t.Fatalf("%s = %q, want cert-manager", LabelComponent, got)
	}
	if got := ns.GetLabels()[LabelVersion]; got != "v9.9.9" {
		t.Fatalf("%s = %q, want the pinned version", LabelVersion, got)
	}
	if got := ns.GetAnnotations()[AnnOwnership]; got != OwnershipCreated {
		t.Fatalf("%s = %q, want %q", AnnOwnership, got, OwnershipCreated)
	}
	if got := ns.GetLabels()["app.kubernetes.io/name"]; got != "cert-manager" {
		t.Fatalf("upstream label app.kubernetes.io/name = %q, want it preserved", got)
	}
	// The project-scoped uninstall selector must never select a platform
	// component: it is not a project deployment.
	if _, claimed := ns.GetLabels()["app.kubernetes.io/managed-by"]; claimed {
		t.Fatal("an installed component carries app.kubernetes.io/managed-by, which is the project uninstall's selector")
	}
}

// TestExecutePreservesDocumentOrder: the manifest's order is a dependency order
// and kelson does not second-guess it.
func TestExecutePreservesDocumentOrder(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	cl := newCluster()
	installer := newInstaller(t, cl, fetcher)

	plan, err := installer.Plan(context.Background(), Request{Components: []string{"cert-manager"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := installer.Execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{
		"Namespace//cert-manager",
		"CustomResourceDefinition//clusterissuers.cert-manager.io",
		"ServiceAccount/cert-manager/cert-manager",
		"ClusterRole//cert-manager-controller",
		"Deployment/cert-manager/cert-manager",
		"ValidatingWebhookConfiguration//cert-manager-webhook",
	}
	got := cl.applyLog()
	if len(got) != len(want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("apply order:\n got %v\nwant %v", got, want)
		}
	}
}

// TestExecuteAdoptsWhatItDidNotCreate is the ownership claim at its sharpest: a
// namespace somebody made by hand is applied over and never claimed.
func TestExecuteAdoptsWhatItDidNotCreate(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	cl := newCluster()
	cl.seed(t, object("v1", "Namespace", "cert-manager", ""))
	installer := newInstaller(t, cl, fetcher)

	plan, err := installer.Plan(context.Background(), Request{Components: []string{"cert-manager"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Items[0].Objects[0].Exists {
		t.Fatal("the preview does not report that the namespace already exists")
	}
	report, err := installer.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := report.Components[0].Adopted; got != 1 {
		t.Fatalf("adopted = %d, want 1", got)
	}
	ns := cl.get(t, "Namespace", "", "cert-manager")
	if got := ns.GetAnnotations()[AnnOwnership]; got != OwnershipAdopted {
		t.Fatalf("%s = %q, want %q", AnnOwnership, got, OwnershipAdopted)
	}
	if adopted := report.Adopted(); len(adopted) != 1 {
		t.Fatalf("Adopted() = %+v, want the namespace", adopted)
	}
}

// TestExecuteIsIdempotent: re-running converges, and a re-run must not
// downgrade an object kelson created to adopted just because it now exists.
func TestExecuteIsIdempotent(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	cl := newCluster()
	installer := newInstaller(t, cl, fetcher)

	for run := 1; run <= 2; run++ {
		plan, err := installer.Plan(context.Background(), Request{Components: []string{"cert-manager"}})
		if err != nil {
			t.Fatalf("plan (run %d): %v", run, err)
		}
		if _, err := installer.Execute(context.Background(), plan); err != nil {
			t.Fatalf("execute (run %d): %v", run, err)
		}
	}
	ns := cl.get(t, "Namespace", "", "cert-manager")
	if got := ns.GetAnnotations()[AnnOwnership]; got != OwnershipCreated {
		t.Fatalf("after a second run %s = %q, want %q — the annotation records who created it, not who wrote last",
			AnnOwnership, got, OwnershipCreated)
	}
}

// TestExecuteStopsComponentOnFailure: a refused apply stops that component, is
// reported per object, and leaves the component reported as not installed.
func TestExecuteStopsComponentOnFailure(t *testing.T) {
	c, fetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	withComponents(t, c)
	cl := newCluster()
	cl.failApply("ServiceAccount/cert-manager/cert-manager", errors.New("serviceaccounts is forbidden"))
	installer := newInstaller(t, cl, fetcher)

	plan, err := installer.Plan(context.Background(), Request{Components: []string{"cert-manager"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	report, err := installer.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("execute succeeded despite a refused apply")
	}
	if report.Components[0].Installed {
		t.Fatal("a component with a failed apply reports as installed")
	}
	if got := len(report.Components[0].Results); got != 3 {
		t.Fatalf("results = %d, want the two applies before the failure plus the failure", got)
	}
	if last := report.Components[0].Results[2]; last.Outcome != OutcomeFailed {
		t.Fatalf("last result = %+v, want failed", last)
	}
	if got := cl.applyLog(); len(got) != 2 {
		t.Fatalf("applied %v, want to stop at the failure", got)
	}
}

// TestInstallFluxCreatesFluxInstance: installing Flux means installing
// flux-operator and creating one CR, with no Flux manifests vendored.
func TestInstallFluxCreatesFluxInstance(t *testing.T) {
	c, fetcher := pinnedFixture("flux", "flux-system", "flux", []byte(fluxOperatorFixture))
	withComponents(t, c)
	cl := newCluster()
	installer := newInstaller(t, cl, fetcher)

	plan, err := installer.Plan(context.Background(), Request{Components: []string{"flux"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	last := plan.Items[0].Objects[len(plan.Items[0].Objects)-1]
	if last.Ref.Kind != "FluxInstance" || !last.Authored {
		t.Fatalf("last planned object = %+v, want the authored FluxInstance", last.Ref)
	}
	if _, err := installer.Execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}

	fi := cl.get(t, "FluxInstance", "flux-system", "flux")
	if fi == nil {
		t.Fatal("no FluxInstance was created")
	}
	version, _, _ := unstructured.NestedString(fi.Object, "spec", "distribution", "version")
	if version != FluxDistributionVersion {
		t.Fatalf("spec.distribution.version = %q, want %q", version, FluxDistributionVersion)
	}
	components, _, _ := unstructured.NestedStringSlice(fi.Object, "spec", "components")
	if !contains(components, "helm-controller") {
		t.Fatalf("spec.components = %v, want helm-controller for `kind: helm` (ADR-0016)", components)
	}
	if _, found, _ := unstructured.NestedMap(fi.Object, "spec", "sync"); found {
		t.Fatal("the FluxInstance sets spec.sync; installing Flux must not start reconciling a repository")
	}
	// The CR goes last, after the CRD that serves it.
	log := cl.applyLog()
	if log[len(log)-1] != "FluxInstance/flux-system/flux" {
		t.Fatalf("apply order %v does not end with the FluxInstance", log)
	}
}

// TestInstallFluxWaitsForTheCRD: applying a CR into a group the API server does
// not yet serve is a race, and the timeout says so without leaving a mess.
func TestInstallFluxWaitsForTheCRD(t *testing.T) {
	c, fetcher := pinnedFixture("flux", "flux-system", "flux", []byte(fluxOperatorFixture))
	withComponents(t, c)
	cl := newCluster()
	cl.unestablished[fluxInstanceCRD] = true

	installer, err := New(Options{
		Client: cl.dyn, Mapper: testMapper(), Fetch: fetcher,
		EstablishTimeout: 10 * time.Millisecond, EstablishPoll: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new installer: %v", err)
	}
	plan, err := installer.Plan(context.Background(), Request{Components: []string{"flux"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	report, execErr := installer.Execute(context.Background(), plan)
	if execErr == nil {
		t.Fatal("execute succeeded with an unestablished CRD")
	}
	if !strings.Contains(execErr.Error(), "not established") {
		t.Fatalf("error %q does not say the CRD was never served", execErr)
	}
	if report.Components[0].Created != 3 {
		t.Fatalf("created = %d, want the operator's own three objects", report.Components[0].Created)
	}
}

// TestAllMissingSkipsWhatIsPresent: --all-missing is a plan over what detection
// reports absent, and says nothing about the rest.
func TestAllMissingSkipsWhatIsPresent(t *testing.T) {
	cm, cmFetch := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	cnpg, cnpgFetch := pinnedFixture("cnpg", "cnpg-system", "cnpg", []byte(cnpgFixture))
	withComponents(t, cm, cnpg)
	fetcher := &fakeFetcher{bodies: map[string][]byte{
		cm.ManifestURL:   cmFetch.bodies[cm.ManifestURL],
		cnpg.ManifestURL: cnpgFetch.bodies[cnpg.ManifestURL],
	}}
	installer := newInstaller(t, newCluster(), fetcher)

	plan, err := installer.Plan(context.Background(), Request{
		AllMissing: true,
		Profile: clusterprofile.ClusterProfile{
			CertManager: &clusterprofile.CertManager{Version: "v1.20.0"},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Component.Name != "cnpg" {
		t.Fatalf("plan items = %+v, want only cnpg", plan.Items)
	}
	if len(plan.Refusals) != 0 {
		t.Fatalf("refusals = %+v; --all-missing must not list every satisfied component", plan.Refusals)
	}
}

// TestRequestValidate covers the two ways a request addresses nothing coherent.
func TestRequestValidate(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"nothing", Request{}, "addressed by component"},
		{"both", Request{Components: []string{"flux"}, AllMissing: true}, "disagree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestNewRefusesIncompleteOptions: a fetcher is not optional, because a build
// without one would silently be a build that vendors manifests.
func TestNewRefusesIncompleteOptions(t *testing.T) {
	cl := newCluster()
	if _, err := New(Options{Mapper: testMapper(), Fetch: &fakeFetcher{}}); err == nil {
		t.Fatal("New accepted a nil client")
	}
	if _, err := New(Options{Client: cl.dyn, Fetch: &fakeFetcher{}}); err == nil {
		t.Fatal("New accepted a nil mapper")
	}
	if _, err := New(Options{Client: cl.dyn, Mapper: testMapper()}); err == nil {
		t.Fatal("New accepted a nil fetcher")
	}
}
