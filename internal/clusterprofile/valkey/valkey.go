// Package valkey judges what a cluster's Valkey operator installation can
// actually do, capability by capability, from a ClusterProfile alone (issue
// #98, ADR-0005, ADR-0015).
//
// # The operator kelson delegates to
//
// ADR-0015 selects valkey-io/valkey-operator, the Valkey project's own
// operator. What that buys and what it costs is recorded there; what matters
// here is the shape of the delegation. kelson writes one ValkeyCluster per
// component and nothing else:
// the topology, the failover, the pod scheduling and the config file all belong
// to the operator.
//
// # The capabilities, and why there are two
//
//	valkeyclusters.valkey.io   the API kelson writes
//	valkeynodes.valkey.io      the resource the operator materialises pods through
//
// They are separate answers because the API server can serve one and not the
// other. The operator renders every pod through a `ValkeyNode` CR rather than
// managing pods directly, so a cluster whose `ValkeyNode` CRD was never applied
// accepts a `ValkeyCluster`, reports no error on the manifest, and then never
// creates a pod. That is exactly the silent half-success a capability check
// exists to catch, and it is visible in discovery.
//
// No capability here carries a version floor of its own. The operator is young
// enough that its features have not yet split across releases the way
// CloudNativePG's declarative surface did, and inventing floors that upstream
// never documented would be a guess wearing a number. The single floor is the
// declared support matrix's `valkey-operator` row
// (internal/clusterprofile/support), which is the release whose CRD shape
// kelson writes.
//
// # Adopt, never duplicate
//
// The operator's CRDs are cluster-scoped and only one installation may own
// them. Every remediation here therefore says *upgrade the operator you have*;
// the one message that says "install" is the one for a cluster that has none.
// Installing is not this package's job and is not implemented at all — ADR-0005
// requires the prerequisite to be visible and explicit, never a silent side
// effect.
//
// # Capable, not capable, and unknown are three different things
//
// The verdict is a [clusterprofile.Outcome] (issue #144), with the same reading
// as internal/clusterprofile/postgres:
//
//   - OutcomeYes — the operator is present, meets the floor, and serves the CRD.
//   - OutcomeNo — checked and found wanting: no operator at all, a version below
//     the floor, or a CRD the API server does not serve.
//   - OutcomeUnknown — the operator's version could not be read, or the operator
//     sat behind a detection Gap. Never silently fine, never a failure.
//
// The package is pure: its whole input is a ClusterProfile value, no cluster
// access, no clock (issue #20), so preview and the renderer may import it.
package valkey

import (
	"strings"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// Group is the operator's API group. It is exported because the renderer writes
// it into every manifest and the error messages name it; one constant beats two
// string literals that can drift apart.
const Group = "valkey.io"

// gapField is the ClusterProfile.Incomplete root that hides operator detection.
// Gap.Covers matches anything beneath it, so a gap on "valkey.version" or
// "valkey.crds" hides a judgement about "valkey" too.
const gapField = "valkey"

// Capability is one thing kelson's cache rendering relies on the operator for.
type Capability string

const (
	// CapabilityCluster is the ValkeyCluster CR: the API every preset writes.
	CapabilityCluster Capability = "cluster"
	// CapabilityNodes is the ValkeyNode CR the operator materialises each pod
	// through. Without it a ValkeyCluster is accepted and never runs.
	CapabilityNodes Capability = "nodes"
)

// requirement is what a capability needs from the cluster: a version floor, and
// the served resource that proves it.
type requirement struct {
	capability Capability
	// what is the human phrase naming the capability in a message.
	what string
	// crd is the plural resource in valkey.io the capability needs the API
	// server to serve. Both of kelson's capabilities *are* a CRD, so serving the
	// resource is itself the proof — unlike CNPG's managed roles or declarative
	// schemas, which are fields inside a CR and invisible to discovery.
	crd string
}

// requirements is the maintained capability table, in report order. The floor
// for both is the declared support matrix's `valkey-operator` row, resolved at
// judgement time so the matrix stays the single source of truth for that number.
var requirements = []requirement{
	{
		capability: CapabilityCluster,
		what:       "the ValkeyCluster API kelson renders caches against",
		crd:        "valkeyclusters",
	},
	{
		capability: CapabilityNodes,
		what:       "the ValkeyNode resource the operator runs each cache pod through",
		crd:        "valkeynodes",
	},
}

// Result is the verdict for one capability on one cluster.
type Result struct {
	Capability Capability
	Outcome    clusterprofile.Outcome
	// Found is the detected operator version, "" when none was readable.
	Found string
	// Since is the operator release the capability requires.
	Since string
	// Message reads "supported from X, detected Y" for a too-old operator, and
	// names the remediation — always an upgrade when the operator is present,
	// because a second operator must never be installed beside the first.
	Message string
}

// Report is every capability's verdict, in table order. It is the shape a UI
// needs to show `kind: valkey` with the reason attached (ADR-0005).
func Report(p clusterprofile.ClusterProfile) []Result {
	out := make([]Result, 0, len(requirements))
	for _, r := range requirements {
		out = append(out, judge(p, r))
	}
	return out
}

// Supports answers one capability's question. An unknown capability name is
// Unknown rather than a panic: a caller asking about something this table does
// not track has not been told "no".
func Supports(p clusterprofile.ClusterProfile, c Capability) Result {
	for _, r := range requirements {
		if r.capability == c {
			return judge(p, r)
		}
	}
	return Result{
		Capability: c,
		Outcome:    clusterprofile.OutcomeUnknown,
		Message:    "no Valkey operator capability named " + quoted(string(c)) + " is tracked",
	}
}

// Preset is an ADR-0007 topology preset name. The strings match
// model.ServicePreset's values deliberately, but the type is redeclared here so
// the capability plane does not depend on the authoring plane — the same
// arrangement internal/clusterprofile/postgres uses.
type Preset string

const (
	PresetShared   Preset = "shared"
	PresetSmall    Preset = "small"
	PresetHASmall  Preset = "ha-small"
	PresetHAMedium Preset = "ha-medium"
	PresetBranch   Preset = "branch"
)

// presetNeeds maps each preset kelson renders to the capabilities it cannot be
// rendered without. Every valkey preset needs the same two, because every one of
// them is a ValkeyCluster whose pods are ValkeyNodes — the presets differ only
// in shard count, replica count and sizing, none of which is a capability.
//
// `shared` and `branch` are absent on purpose: they are not valkey topologies at
// all (docs/data-services.md) and the renderer refuses them before asking this
// package anything. An untracked preset is Unknown, which is the honest answer
// to a question about a topology this operator has no notion of.
var presetNeeds = map[Preset][]Capability{
	PresetSmall:    {CapabilityCluster, CapabilityNodes},
	PresetHASmall:  {CapabilityCluster, CapabilityNodes},
	PresetHAMedium: {CapabilityCluster, CapabilityNodes},
}

// Verdict is the answer to "can this cluster host this preset", with the
// capability that decided it.
type Verdict struct {
	Preset  Preset
	Outcome clusterprofile.Outcome
	// Blocking is the capabilities that were not answered yes, in table order.
	Blocking []Capability
	Message  string
}

// SupportsPreset decides whether a cluster can host a valkey preset, and says
// why not in terms a human can act on. A single No decides the verdict;
// otherwise a single Unknown does, because "we could not tell" must not be
// rounded up to a promise.
func SupportsPreset(p clusterprofile.ClusterProfile, preset Preset) Verdict {
	needs, tracked := presetNeeds[preset]
	if !tracked {
		return Verdict{
			Preset:  preset,
			Outcome: clusterprofile.OutcomeUnknown,
			Message: "unknown valkey preset " + quoted(string(preset)) +
				"; valid presets are small, ha-small, ha-medium (docs/data-services.md)",
		}
	}

	v := Verdict{Preset: preset, Outcome: clusterprofile.OutcomeYes}
	var reasons []string
	worst := clusterprofile.OutcomeYes
	for _, c := range needs {
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

	switch v.Outcome {
	case clusterprofile.OutcomeYes:
		v.Message = "this cluster can host the " + quoted(string(preset)) + " valkey preset"
	default:
		v.Message = "this cluster cannot host the " + quoted(string(preset)) + " valkey preset: " + strings.Join(reasons, "; ")
	}
	return v
}

// judge answers one requirement, in the same order internal/clusterprofile/
// postgres does: a gap first (we could not look), then absence (we looked and
// there is none), then the version floor, then the served CRD, and only then the
// fallback for an operator whose version is present but unreadable.
func judge(p clusterprofile.ClusterProfile, r requirement) Result {
	comp, _ := support.Lookup(supportComponent)
	since := comp.Minimum
	res := Result{Capability: r.capability, Since: since}

	if gap, hidden := p.GapFor(gapField); hidden {
		res.Outcome = clusterprofile.OutcomeUnknown
		res.Message = "cannot judge " + r.what + " (hidden by a detection gap: " + gap.Reason + ")"
		return res
	}
	if p.Valkey == nil {
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = "the Valkey operator is not installed, so " + r.what + " is unavailable; " +
			"install valkey-io/valkey-operator before using a managed valkey component — kelson never " +
			"installs it as a side effect (ADR-0005, ADR-0015)"
		return res
	}

	op := *p.Valkey
	res.Found = op.Version

	switch support.AtLeast(op.Version, since) {
	case clusterprofile.OutcomeNo:
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = r.what + " is supported from Valkey operator " + since + ", detected " + op.Version +
			" — the detected operator is too old; upgrade the operator you have (its CRDs are cluster-scoped, " +
			"and a second operator must never be installed beside the first)"
		return res
	case clusterprofile.OutcomeYes:
		if len(op.CRDs) > 0 && !op.ServesCRD(r.crd) {
			res.Outcome = clusterprofile.OutcomeNo
			res.Message = "the Valkey operator " + op.Version + " is new enough for " + r.what +
				", but the API server does not serve " + quoted(r.crd+"."+Group) + "; reapply the operator's CRDs"
			return res
		}
		res.Outcome = clusterprofile.OutcomeYes
		res.Message = "the Valkey operator " + op.Version + " supports " + r.what + " (supported from " + since + ")"
		return res
	}

	// The version could not be read or parsed. Discovery still settles both
	// capabilities here, because each of them *is* a served resource rather than
	// evidence about one.
	if len(op.CRDs) > 0 {
		if !op.ServesCRD(r.crd) {
			res.Outcome = clusterprofile.OutcomeNo
			res.Message = "the API server does not serve " + quoted(r.crd+"."+Group) + ", so " + r.what +
				" is unavailable (supported from Valkey operator " + since + ")"
			return res
		}
		res.Outcome = clusterprofile.OutcomeYes
		res.Message = "the API server serves " + quoted(r.crd+"."+Group) + ", so " + r.what +
			" is available; the operator version could not be read, so this is judged from the served CRD"
		return res
	}

	res.Outcome = clusterprofile.OutcomeUnknown
	res.Message = "cannot judge " + r.what + " (" + unreadableVersion(op.Version) +
		"); supported from Valkey operator " + since
	return res
}

// supportComponent is this operator's row in the declared support matrix.
const supportComponent = "valkey-operator"

// unreadableVersion phrases why a version could not be compared: absent and
// unparseable are different facts and a human fixing it needs to know which.
func unreadableVersion(found string) string {
	if found == "" {
		return "the Valkey operator is installed but its version could not be read"
	}
	return "the detected Valkey operator version " + quoted(found) + " could not be parsed"
}

func quoted(s string) string { return `"` + s + `"` }
