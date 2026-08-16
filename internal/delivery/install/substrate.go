package install

// The delivery substrate, named once so every caller that must not guess reads
// the same answer.
//
// ADR-0028 makes Flux the only reconciliation path, so a cluster without it
// deploys nothing at all: an Environment applies, validates, and then sits
// reporting FluxNotInstalled forever. ADR-0030 answers that with two catalog
// rows and a preference — flux-aio, every Flux controller in one pod, on a
// cluster with none; flux-operator when this build carries no flux-aio
// snapshot to install.
//
// That preference already exists, in [refuse]: a `--all-missing` sweep installs
// flux-aio and declines flux-operator by name, guarded on the snapshot actually
// being there so a build without one falls back rather than offering a cluster
// nothing. What this file adds is a way to ask the same question WITHOUT
// sweeping. A caller that only needs the substrate — the chart's
// ensure-substrate hook, which runs because somebody installed kelson and not
// because they asked for a platform — must not also install cert-manager,
// CloudNativePG and Envoy Gateway. Those are decisions a person makes by
// naming them (ADR-0021 §1); the substrate is the one component ADR-0028 has
// already decided is mandatory.
//
// It is a function over the table rather than a constant for the reason
// [fluxAIOInstallable] is: the table is swappable in tests, the snapshot is
// present in a released build and absent in a fresh clone, and a constant would
// be wrong in one of those worlds.

// SubstrateName is the catalog row that installs the delivery substrate on a
// cluster that has none: [FluxAIOName] when this build carries its rendered
// snapshot, [FluxOperatorName] otherwise.
//
// It says nothing about whether a substrate SHOULD be installed. That is
// detection's answer and only detection's answer ([Component.Presence]), and a
// cluster that already runs Flux is adopted whatever installed it.
func SubstrateName() string {
	if fluxAIOInstallable() {
		return FluxAIOName
	}
	return FluxOperatorName
}

// Substrate is [SubstrateName]'s row, and whether the table still has one. The
// second return is not ceremony: both Flux rows are ordinary table entries and
// a caller that finds neither should say so rather than install something else.
func Substrate() (Component, bool) {
	return Lookup(SubstrateName())
}
