package install

import (
	"context"
	"testing"
)

// The substrate seam (owner decision 2026-08-16: installing kelson installs the
// delivery substrate). It exists so a caller that needs Flux and only Flux gets
// the same row a `--all-missing` sweep would pick, without also getting
// cert-manager and CloudNativePG — and the tests below are written against that
// pair of properties, because a drift in either one is silent.

// TestSubstrateNamePrefersFluxAIOWhenTheSnapshotIsThere: ADR-0030 decision 1's
// default offer, read through the seam instead of through a sweep.
func TestSubstrateNamePrefersFluxAIOWhenTheSnapshotIsThere(t *testing.T) {
	aio := renderedFixture(t, FluxAIOName, "flux-system", "flux", fluxAIOFixture)
	operator, _ := pinnedFixture(FluxOperatorName, "flux-system", "flux", []byte(certManagerFixture))
	withComponents(t, aio, operator)

	if got := SubstrateName(); got != FluxAIOName {
		t.Fatalf("SubstrateName() = %q, want %q: one pod of Flux is the default offer on a cluster with none",
			got, FluxAIOName)
	}
	c, ok := Substrate()
	if !ok || c.Name != FluxAIOName {
		t.Fatalf("Substrate() = (%+v, %v), want the flux-aio row", c, ok)
	}
}

// TestSubstrateNameFallsBackWithoutASnapshot is the same guard
// [TestSweepFallsBackToFluxOperatorWithoutASnapshot] puts on the sweep, on the
// seam a chart hook reads. A build that cannot install flux-aio must name
// flux-operator rather than name a row it would then refuse: the caller here
// installs unattended, so a name it cannot act on is a cluster left with no
// reconciler and a failed release to explain it.
func TestSubstrateNameFallsBackWithoutASnapshot(t *testing.T) {
	aio := renderedFixture(t, FluxAIOName, "flux-system", "flux", fluxAIOFixture)
	withRendered(t, nil)
	operator, _ := pinnedFixture(FluxOperatorName, "flux-system", "flux", []byte(certManagerFixture))
	withComponents(t, aio, operator)

	if got := SubstrateName(); got != FluxOperatorName {
		t.Fatalf("SubstrateName() = %q, want %q: with no snapshot the fallback is full Flux, not nothing",
			got, FluxOperatorName)
	}
}

// TestSubstrateNameIsInstallableAsANamedRequest closes the loop the seam exists
// for: whatever SubstrateName returns must survive Plan as a NAMED component,
// which is a different path from the sweep the preference was written for. A
// named flux-operator is not declined in favour of flux-aio (that exception is
// `--all-missing`-only), and a named row plans exactly one component — the
// substrate, and nothing else in the catalog.
func TestSubstrateNameIsInstallableAsANamedRequest(t *testing.T) {
	aio := renderedFixture(t, FluxAIOName, "flux-system", "flux", fluxAIOFixture)
	operator, fetcher := pinnedFixture(FluxOperatorName, "flux-system", "flux", []byte(certManagerFixture))
	certManager, certFetcher := pinnedFixture("cert-manager", "cert-manager", "certManager", []byte(certManagerFixture))
	for url, body := range certFetcher.bodies {
		fetcher.bodies[url] = body
	}
	withComponents(t, aio, operator, certManager)
	installer := newInstaller(t, newCluster(), fetcher)

	plan, err := installer.Plan(context.Background(), Request{Components: []string{SubstrateName()}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Component.Name != SubstrateName() {
		var got []string
		for _, item := range plan.Items {
			got = append(got, item.Component.Name)
		}
		t.Fatalf("items = %v, want [%s] alone: the substrate is mandatory (ADR-0028) and every other row is "+
			"a decision somebody makes by name (ADR-0021)", got, SubstrateName())
	}
}
