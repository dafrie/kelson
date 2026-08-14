// Package postgres judges what a cluster's CloudNativePG installation can
// actually do, capability by capability, from a ClusterProfile alone (issue
// #90, ADR-0005, ADR-0007).
//
// # kelson targets the latest CloudNativePG
//
// The baseline is the newest CNPG release (owner decision, 2026-08-13). The
// database rendering built on this (#89, #92, #93) may rely on the declarative
// surface — managed roles for application credentials, the Database CRD for the
// `shared` preset, schemas and extensions on that Database — without keeping an
// imperative fallback path alive beside it.
//
// That baseline is a posture, not a refusal. An operator at 1.24 runs dedicated
// clusters perfectly well and simply cannot serve `shared`, so what an older
// version costs is a *per-capability* answer: "supported from 1.25, detected
// 1.24.1", naming the capability the operator lacks, rather than one boolean
// that would either refuse a working cluster or promise a feature it cannot
// serve. The declared support matrix keeps its own, much lower `cnpg` floor
// (internal/clusterprofile/support): that floor governs whether the Cluster API
// kelson writes is servable at all, and refusing everything below the
// declarative baseline would be a different, wrong decision.
//
// # The capabilities, and the release each arrived in
//
//	managed roles           Cluster .spec.managed.roles              CNPG 1.20
//	declarative databases   the Database CRD                         CNPG 1.25
//	declarative schemas     Database .spec.schemas / .spec.extensions CNPG 1.26
//
// Versions verified against CloudNativePG's own release notes and documentation
// history; the newest release at the time of writing is 1.30.
//
// # Adopt, never duplicate
//
// CNPG's resources are cluster-scoped and only one operator may run per
// cluster. Every remediation message here therefore says *upgrade the operator
// you have*; the one message that says "install" is the one for a cluster that
// has none. Installing is not this package's job and is not implemented at all
// yet — ADR-0005 requires the prerequisite to be visible and explicit (a
// disabled option with a one-click install, never a silent side effect), and
// that path is the remaining scope of issue #90.
//
// # Capable, not capable, and unknown are three different things
//
// The verdict is a [clusterprofile.Outcome] (issue #144), so:
//
//   - OutcomeYes — the operator is present and meets the capability's floor,
//     or serves the CRD that *is* the capability.
//   - OutcomeNo — checked and found wanting: no CNPG at all, a version below
//     the floor, or a CRD the API server does not serve.
//   - OutcomeUnknown — the operator's version could not be read, or CNPG sat
//     behind a detection Gap. Never silently fine, never a failure.
//
// The package is pure: its whole input is a ClusterProfile value, no cluster
// access, no clock (issue #20), so preview and the renderer may import it.
package postgres

import (
	"strings"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// gapField is the ClusterProfile.Incomplete root that hides CNPG detection.
// Gap.Covers matches anything beneath it, so a gap on "cnpg.version" or
// "cnpg.crds" hides a judgement about "cnpg" too — which is right: a version
// nobody could read cannot be judged against a floor.
const gapField = "cnpg"

// Capability is one declarative CloudNativePG feature kelson's rendering
// relies on. They are separate answers because they arrived in separate
// releases and an operator can have some and not others.
type Capability string

const (
	// CapabilityCluster is the Cluster CR itself: the API every preset writes.
	// Its floor is the declared support matrix's `cnpg` row, not a declarative
	// baseline, because it governs whether the manifest is servable at all.
	CapabilityCluster Capability = "cluster"
	// CapabilityManagedRoles is declarative role management via
	// Cluster .spec.managed.roles (CNPG 1.20), which is how kelson renders
	// per-component database users and credentials without imperative SQL.
	CapabilityManagedRoles Capability = "managed-roles"
	// CapabilityDeclarativeDatabases is the Database CRD (CNPG 1.25): many
	// databases in one Postgres cluster, which is what ADR-0007's `shared`
	// preset is built on.
	CapabilityDeclarativeDatabases Capability = "declarative-databases"
	// CapabilityDeclarativeSchemas is schema and extension management on the
	// Database resource, .spec.schemas and .spec.extensions (CNPG 1.26).
	CapabilityDeclarativeSchemas Capability = "declarative-schemas"
)

// requirement is what a capability needs from the cluster: a version floor,
// and — when discovery can see it — a served resource.
type requirement struct {
	capability Capability
	// what is the human phrase naming the capability in a message.
	what string
	// since is the CNPG release the capability arrived in. Empty means "the
	// declared support matrix's cnpg floor", which is resolved at judgement
	// time so the matrix stays the single source of truth for that number.
	since string
	// crd is the plural resource in postgresql.cnpg.io the capability needs the
	// API server to serve. A served set that lacks it is a No regardless of the
	// version, because the manifest would be rejected.
	crd string
	// crdProves records whether serving that resource is itself proof of the
	// capability. It is for the CRDs that *are* the feature; a capability
	// expressed as fields inside a CR (managed roles, schemas) is invisible to
	// discovery, so an unreadable version stays Unknown rather than being
	// inferred from a resource name.
	crdProves bool
}

// requirements is the maintained capability table, in report order. Each floor
// is CloudNativePG's own documented introduction release.
var requirements = []requirement{
	{
		capability: CapabilityCluster,
		what:       "the Cluster API kelson renders databases against",
		crd:        "clusters",
		crdProves:  true,
	},
	{
		capability: CapabilityManagedRoles,
		what:       "declarative role management (Cluster .spec.managed.roles)",
		since:      "1.20.0",
		crd:        "clusters",
	},
	{
		capability: CapabilityDeclarativeDatabases,
		what:       "declarative databases (the Database CRD)",
		since:      "1.25.0",
		crd:        "databases",
		crdProves:  true,
	},
	{
		capability: CapabilityDeclarativeSchemas,
		what:       "declarative schemas and extensions (Database .spec.schemas, .spec.extensions)",
		since:      "1.26.0",
		crd:        "databases",
	},
}

// Result is the verdict for one capability on one cluster.
type Result struct {
	Capability Capability
	Outcome    clusterprofile.Outcome
	// Found is the detected operator version, "" when none was readable.
	Found string
	// Since is the CNPG release the capability was introduced in.
	Since string
	// Message reads "supported from X, detected Y" for a too-old operator, and
	// names the remediation — always an upgrade when CNPG is present, because
	// a second operator must never be installed beside the first.
	Message string
}

// Report is every capability's verdict, in table order. It is the shape a UI
// needs to show `type: postgres` with the reason attached (ADR-0005).
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
		Message:    "no CloudNativePG capability named " + quoted(string(c)) + " is tracked",
	}
}

// Preset is an ADR-0007 topology preset name. The strings match
// model.ServicePreset's values deliberately, but the type is redeclared here so
// the capability plane does not depend on the authoring plane: what travels
// between them is the preset *name*, which is also what a user writes in a spec.
type Preset string

const (
	PresetShared   Preset = "shared"
	PresetSmall    Preset = "small"
	PresetHASmall  Preset = "ha-small"
	PresetHAMedium Preset = "ha-medium"
	PresetBranch   Preset = "branch"
)

// presetNeeds maps each preset to the capabilities it cannot be rendered
// without. `shared` is the only one that needs the Database CRD — it is the
// preset that puts many databases in one Postgres cluster — and every preset
// needs managed roles because that is how application credentials are rendered.
//
// Storage capability is deliberately absent: whether a `branch` is a thin clone
// or a full copy is a storage question, answered by
// internal/clusterprofile/storage.Branching (issue #91), not by the operator.
var presetNeeds = map[Preset][]Capability{
	PresetShared:   {CapabilityCluster, CapabilityManagedRoles, CapabilityDeclarativeDatabases},
	PresetSmall:    {CapabilityCluster, CapabilityManagedRoles},
	PresetHASmall:  {CapabilityCluster, CapabilityManagedRoles},
	PresetHAMedium: {CapabilityCluster, CapabilityManagedRoles},
	PresetBranch:   {CapabilityCluster, CapabilityManagedRoles},
}

// Verdict is the answer to "can this cluster host this preset", with the
// capability that decided it.
type Verdict struct {
	Preset  Preset
	Outcome clusterprofile.Outcome
	// Blocking is the capabilities that were not answered yes, in table order:
	// the ones a No or an Unknown rests on.
	Blocking []Capability
	Message  string
}

// SupportsPreset decides whether a cluster can host a preset, and says why not
// in terms a human can act on. A single No decides the verdict; otherwise a
// single Unknown does, because "we could not tell" must not be rounded up to a
// promise.
func SupportsPreset(p clusterprofile.ClusterProfile, preset Preset) Verdict {
	needs, tracked := presetNeeds[preset]
	if !tracked {
		return Verdict{
			Preset:   preset,
			Outcome:  clusterprofile.OutcomeUnknown,
			Message:  "unknown preset " + quoted(string(preset)) + "; valid presets are shared, small, ha-small, ha-medium, branch (ADR-0007)",
			Blocking: nil,
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
		// A definite No outranks an Unknown, which outranks a Yes: the preset
		// is refused if any capability is missing, and only "we could not tell"
		// otherwise.
		if worst != clusterprofile.OutcomeNo {
			worst = r.Outcome
		}
	}
	v.Outcome = worst

	switch v.Outcome {
	case clusterprofile.OutcomeYes:
		v.Message = "this cluster can host the " + quoted(string(preset)) + " preset"
	default:
		v.Message = "this cluster cannot host the " + quoted(string(preset)) + " preset: " + strings.Join(reasons, "; ")
	}
	return v
}

// judge answers one requirement. The order is the point: a gap first (we could
// not look), then absence (we looked and there is none), then the version floor
// (the "too old for this preset" answer, naming both numbers), then the served
// CRD, and only then the fallbacks for an operator whose version is present but
// unreadable.
func judge(p clusterprofile.ClusterProfile, r requirement) Result {
	since := r.since
	if since == "" {
		comp, _ := support.Lookup("cnpg")
		since = comp.Minimum
	}
	res := Result{Capability: r.capability, Since: since}

	if gap, hidden := p.GapFor(gapField); hidden {
		res.Outcome = clusterprofile.OutcomeUnknown
		res.Message = "cannot judge " + r.what + " (hidden by a detection gap: " + gap.Reason + ")"
		return res
	}
	if p.CloudNativePG == nil {
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = "CloudNativePG is not installed, so " + r.what + " is unavailable; " +
			"install the operator before using a managed postgres service — kelson never installs it as a side effect (ADR-0005, issue #90)"
		return res
	}

	cnpg := *p.CloudNativePG
	res.Found = cnpg.Version

	switch support.AtLeast(cnpg.Version, since) {
	case clusterprofile.OutcomeNo:
		res.Outcome = clusterprofile.OutcomeNo
		res.Message = r.what + " is supported from CloudNativePG " + since + ", detected " + cnpg.Version +
			" — the detected operator is too old; upgrade the CloudNativePG you have (kelson targets the latest release, " +
			"and a second operator must never be installed beside the first)"
		return res
	case clusterprofile.OutcomeYes:
		if len(cnpg.CRDs) > 0 && r.crd != "" && !cnpg.ServesCRD(r.crd) {
			res.Outcome = clusterprofile.OutcomeNo
			res.Message = "CloudNativePG " + cnpg.Version + " is new enough for " + r.what +
				", but the API server does not serve " + quoted(r.crd+"."+"postgresql.cnpg.io") +
				"; reapply the operator's CRDs"
			return res
		}
		res.Outcome = clusterprofile.OutcomeYes
		res.Message = "CloudNativePG " + cnpg.Version + " supports " + r.what + " (supported from " + since + ")"
		return res
	}

	// The version could not be read or parsed. Discovery can still settle the
	// capabilities that *are* a CRD, because a served Database resource is the
	// feature itself rather than evidence about it.
	if len(cnpg.CRDs) > 0 && r.crd != "" {
		switch {
		case !cnpg.ServesCRD(r.crd):
			res.Outcome = clusterprofile.OutcomeNo
			res.Message = "the API server does not serve " + quoted(r.crd+".postgresql.cnpg.io") +
				", so " + r.what + " is unavailable (supported from CloudNativePG " + since + ")"
			return res
		case r.crdProves:
			res.Outcome = clusterprofile.OutcomeYes
			res.Message = "the API server serves " + quoted(r.crd+".postgresql.cnpg.io") + ", so " + r.what +
				" is available; the operator version could not be read, so this is judged from the served CRD"
			return res
		}
	}

	res.Outcome = clusterprofile.OutcomeUnknown
	res.Message = "cannot judge " + r.what + " (" + unreadableVersion(cnpg.Version) +
		"); supported from CloudNativePG " + since
	return res
}

// unreadableVersion phrases why a version could not be compared: absent and
// unparseable are different facts and a human fixing it needs to know which.
func unreadableVersion(found string) string {
	if found == "" {
		return "the CloudNativePG operator is installed but its version could not be read"
	}
	return "the detected CloudNativePG version " + quoted(found) + " could not be parsed"
}

func quoted(s string) string { return `"` + s + `"` }
