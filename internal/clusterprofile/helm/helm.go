// Package helm judges what a cluster's Flux installation can do for a
// `kind: helm` component, capability by capability, from a ClusterProfile alone
// (ADR-0016 decision 4, ADR-0005).
//
// # The controllers kelson delegates to
//
// A helm component renders two resources and reconciles neither: a chart source
// (HelmRepository or OCIRepository) that source-controller fetches, and a
// HelmRelease that helm-controller installs and upgrades. That is the same
// delegation every managed data type gets — kelson writes a CR, kelson does not
// template an engine — and it means the same thing for detection: presence of
// the API is the prerequisite, and the version is part of the answer.
//
// # The capabilities, and why there are two
//
//	helmreleases.helm.toolkit.fluxcd.io    the API kelson writes
//	source.toolkit.fluxcd.io               the controller that fetches the chart
//
// They are separate answers because a cluster can have one and not the other,
// and this is not a hypothetical: flux-operator's FluxInstance installs a
// *subset* of the Flux controllers, and "source-controller and helm-controller
// only" is a supported and commonly chosen shape (issue #60). The failure modes
// differ too — with no helm-controller a HelmRelease is accepted and nothing
// ever installs the chart; with no source-controller the release is created and
// stalls with an unresolved chart reference — and neither is a failure kelson
// would otherwise surface.
//
// # What this package does NOT decide
//
// Whether a helm component may render at all. That is the environment's
// delivery mode, which is spec data, and the gate lives in the pure renderer
// (internal/renderer/helm.go) so the same document renders the same way against
// every cluster. This package answers the question a capability panel or a
// `kelson profile` run asks — *can this cluster host a chart component, and if
// not, what is missing* — and answers it as a finding rather than a refusal.
//
// # Capable, not capable, and unknown are three different things
//
// The verdict is a [clusterprofile.Outcome] (issue #144), read exactly as
// internal/clusterprofile/postgres and internal/clusterprofile/valkey read it:
//
//   - OutcomeYes — installed, at or above the floor, and serving what it needs.
//   - OutcomeNo — checked and found wanting: nothing installed, a version below
//     the floor, or a CRD the API server does not serve.
//   - OutcomeUnknown — the version could not be read, or the component sat
//     behind a detection Gap. Never silently fine, never a failure.
//
// The package is pure: its whole input is a ClusterProfile value, no cluster
// access and no clock (issue #20).
package helm

import (
	"strings"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// Group is helm-controller's API group. It is exported because the renderer
// writes it into every HelmRelease and the messages here name it; one constant
// beats two string literals that can drift apart.
const Group = "helm.toolkit.fluxcd.io"

// SourceGroup is source-controller's API group: where the chart source kinds
// this package requires live.
const SourceGroup = "source.toolkit.fluxcd.io"

// Capability is one thing kelson's chart rendering relies on Flux for.
type Capability string

const (
	// CapabilityRelease is the HelmRelease CR: the API every helm component
	// writes, served by helm-controller.
	CapabilityRelease Capability = "release"
	// CapabilitySource is source-controller, which fetches the chart the
	// release names. Without it the release is created and never resolves.
	CapabilitySource Capability = "source"
)

// Capabilities is the table in report order.
var Capabilities = []Capability{CapabilityRelease, CapabilitySource}

// Result is the verdict for one capability on one cluster.
type Result struct {
	Capability Capability
	Outcome    clusterprofile.Outcome
	// Found is the detected controller version, "" when none was readable.
	Found string
	// Since is the release the capability requires.
	Since string
	// Message names the finding and, when it is a No, the remediation — always
	// in terms of the Flux installation the cluster already has, because a
	// second Flux beside the first is never the answer (ADR-0005).
	Message string
}

// Report is every capability's verdict, in table order. It is the shape a
// capability panel needs to show `kind: helm` with the reason attached.
func Report(p clusterprofile.ClusterProfile) []Result {
	out := make([]Result, 0, len(Capabilities))
	for _, c := range Capabilities {
		out = append(out, Supports(p, c))
	}
	return out
}

// Supports answers one capability's question. An unknown capability name is
// Unknown rather than a panic: a caller asking about something this table does
// not track has not been told "no".
func Supports(p clusterprofile.ClusterProfile, c Capability) Result {
	switch c {
	case CapabilityRelease:
		return judgeRelease(p)
	case CapabilitySource:
		return judgeSource(p)
	}
	return Result{
		Capability: c,
		Outcome:    clusterprofile.OutcomeUnknown,
		Message:    "no helm capability named " + quoted(string(c)) + " is tracked",
	}
}

// Verdict is the answer to "can this cluster host a helm component", with the
// capabilities that decided it.
type Verdict struct {
	Outcome clusterprofile.Outcome
	// Blocking is the capabilities that were not answered yes, in table order.
	Blocking []Capability
	Message  string
}

// Supported decides whether a cluster can host a `kind: helm` component and
// says why not in terms a human can act on. A single No decides the verdict;
// otherwise a single Unknown does, because "we could not tell" must not be
// rounded up to a promise.
//
// This is the sentence the profile output and the UI's capability panel are
// after: helm components need helm-controller, and here is whether this cluster
// has one.
func Supported(p clusterprofile.ClusterProfile) Verdict {
	v := Verdict{Outcome: clusterprofile.OutcomeYes}
	var reasons []string
	worst := clusterprofile.OutcomeYes
	for _, c := range Capabilities {
		r := Supports(p, c)
		if r.Outcome == clusterprofile.OutcomeYes {
			continue
		}
		v.Blocking = append(v.Blocking, c)
		reasons = append(reasons, r.Message)
		if worst != clusterprofile.OutcomeNo {
			worst = r.Outcome
		}
	}
	v.Outcome = worst

	if v.Outcome == clusterprofile.OutcomeYes {
		return Verdict{Outcome: clusterprofile.OutcomeYes,
			Message: "this cluster can host helm components: helm-controller is present and a chart source " +
				"controller with it"}
	}
	v.Message = "this cluster cannot host helm components: " + strings.Join(reasons, "; ")
	return v
}

// supportComponent is helm-controller's row in the declared support matrix.
const supportComponent = "helm-controller"

// releaseCRD is the plural resource a HelmRelease is written as.
const releaseCRD = "helmreleases"

// judgeRelease answers the HelmRelease capability, in the order the sibling
// packages use: a gap first (we could not look), then absence (we looked and
// there is none), then the version floor, then the served CRD, and only then
// the fallback for a controller whose version is present but unreadable.
func judgeRelease(p clusterprofile.ClusterProfile) Result {
	comp, _ := support.Lookup(supportComponent)
	since := comp.Minimum
	res := Result{Capability: CapabilityRelease, Since: since}
	const what = "the HelmRelease API kelson renders chart components against"

	if gap, hidden := p.GapFor("helmController"); hidden {
		res.Outcome = clusterprofile.OutcomeUnknown
		res.Message = "cannot judge " + what + " (hidden by a detection gap: " + gap.Reason + ")"
		return res
	}
	if p.HelmController == nil {
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = "helm-controller is not installed, so " + what + " is unavailable; install it as part of " +
			"the cluster's Flux installation — a FluxInstance may name a components subset, and " +
			"source-controller with helm-controller is enough for chart components (issue #60). kelson never " +
			"installs a controller as a side effect (ADR-0005, ADR-0016)"
		return res
	}

	hc := *p.HelmController
	res.Found = hc.Version

	switch support.AtLeast(hc.Version, since) {
	case clusterprofile.OutcomeNo:
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = what + " is supported from helm-controller " + since + ", detected " + hc.Version +
			" — the detected controller serves the older HelmRelease betas instead of " + Group + "/v2; " +
			"upgrade the Flux installation this cluster already has"
		return res
	case clusterprofile.OutcomeYes:
		if len(hc.CRDs) > 0 && !hc.ServesCRD(releaseCRD) {
			res.Outcome = clusterprofile.OutcomeNo
			res.Message = "helm-controller " + hc.Version + " is new enough for " + what +
				", but the API server does not serve " + quoted(releaseCRD+"."+Group) + "; reapply Flux's CRDs"
			return res
		}
		res.Outcome = clusterprofile.OutcomeYes
		res.Message = "helm-controller " + hc.Version + " supports " + what + " (supported from " + since + ")"
		return res
	}

	// The version could not be read or parsed. Discovery still settles the
	// capability when the served set is known, because the capability *is* a
	// served resource rather than evidence about one.
	if len(hc.CRDs) > 0 {
		if !hc.ServesCRD(releaseCRD) {
			res.Outcome = clusterprofile.OutcomeNo
			res.Message = "the API server does not serve " + quoted(releaseCRD+"."+Group) + ", so " + what +
				" is unavailable (supported from helm-controller " + since + ")"
			return res
		}
		res.Outcome = clusterprofile.OutcomeYes
		res.Message = "the API server serves " + quoted(releaseCRD+"."+Group) + ", so " + what +
			" is available; the controller version could not be read, so this is judged from the served CRD"
		return res
	}

	res.Outcome = clusterprofile.OutcomeUnknown
	res.Message = "cannot judge " + what + " (" + unreadableVersion(hc.Version) +
		"); supported from helm-controller " + since
	return res
}

// judgeSource answers the chart-source capability from the Flux finding.
//
// It reads presence and not a served-CRD set, and that is a deliberate
// difference from judgeRelease. source-controller owns its whole API group and
// kelson's use of it is the group's oldest, most stable corner: a cluster that
// registers source.toolkit.fluxcd.io at all serves HelmRepository and
// OCIRepository. The interesting partial install is the other one — Flux
// without helm-controller — and that is what the release capability catches.
func judgeSource(p clusterprofile.ClusterProfile) Result {
	comp, _ := support.Lookup(fluxComponent)
	since := comp.Minimum
	res := Result{Capability: CapabilitySource, Since: since}
	const what = "the chart source APIs (HelmRepository, OCIRepository) a release fetches its chart through"

	if gap, hidden := p.GapFor("flux"); hidden {
		res.Outcome = clusterprofile.OutcomeUnknown
		res.Message = "cannot judge " + what + " (hidden by a detection gap: " + gap.Reason + ")"
		return res
	}
	if p.Flux == nil {
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = "source-controller is not installed (nothing serves " + SourceGroup + "), so " + what +
			" is unavailable; a helm component needs a source controller to fetch the chart and a " +
			"helm-controller to release it"
		return res
	}

	res.Found = p.Flux.Version
	switch support.AtLeast(p.Flux.Version, since) {
	case clusterprofile.OutcomeNo:
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = what + " is supported from Flux " + since + ", detected " + p.Flux.Version +
			" — upgrade the Flux installation this cluster already has"
	case clusterprofile.OutcomeYes:
		res.Outcome = clusterprofile.OutcomeYes
		res.Message = "Flux " + p.Flux.Version + " supports " + what + " (supported from " + since + ")"
	default:
		res.Outcome = clusterprofile.OutcomeUnknown
		res.Message = "cannot judge " + what + " (" + unreadableFluxVersion(p.Flux.Version) +
			"); supported from Flux " + since
	}
	return res
}

// fluxComponent is source-controller's row in the declared support matrix.
// Flux ships its controllers as one release, so the suite's version is the
// version of any of them that has no row of its own.
const fluxComponent = "flux"

func unreadableVersion(found string) string {
	if found == "" {
		return "helm-controller is installed but its version could not be read"
	}
	return "the detected helm-controller version " + quoted(found) + " could not be parsed"
}

func unreadableFluxVersion(found string) string {
	if found == "" {
		return "Flux is installed but its version could not be read"
	}
	return "the detected Flux version " + quoted(found) + " could not be parsed"
}

func quoted(s string) string { return `"` + s + `"` }
