package model

import (
	"fmt"
	"strings"
)

// Gating spec fields no plane consumes yet (issue #141).
//
// The model has always validated more than the rest of kelson implements. A
// spec declaring data services, `policy:` or `secrets:` passed validation,
// resolved into Resolved, and then reached a renderer that reads none of it.
// The author got a clean deploy and no signal that part of their spec did
// nothing at all. For an agent that silence is indistinguishable from success,
// which makes it the worst failure shape this project can ship — so a field
// that renders nothing is now an error that says so and says where the work is
// tracked.
//
// The gate is validation-level only, deliberately. The types stay and the
// resolver keeps resolving these fields, so landing a milestone means deleting
// a row from the table below and its call site, not rebuilding the feature.
// M9 · Data services is the first proof of that: the data-service fields and
// the `from:` bindings left this table when the renderer began emitting
// CloudNativePG resources for them (issue #89), and nothing else had to move.
//
// ADR-0025 is the second proof and the clearest one: `policy:` was gated whole
// until kelson-server began enforcing it against agent principals (issue #75).
// The row did not disappear — it narrowed to `policy.deployers`, the one field
// that is about humans and that nothing can resolve yet — and the resolver, the
// precedence rule and the validation it already had were reused unchanged.
//
// A row may also *narrow* rather than disappear, and then disappear later.
// `secrets:` was gated whole until ADR-0018, which narrowed it to `store` alone
// once `backend` began deciding the rendered shape; ADR-0020 then removed that
// last row when `store` and `refreshInterval` became the ExternalSecret's own
// `secretStoreRef` and `spec.refreshInterval` (issue #80). ADR-0022 finished
// the block: `ageRecipients` is what `kelson secret set` encrypts to and
// `ageKeySecret` is a Kustomization's `spec.decryption.secretRef`, so `sops`
// stopped being a render refusal and became a mechanism (issue #81). Nothing
// under `secrets:` is gated any more.
//
// What replaced their rows is *not* silence. A data component the renderer
// cannot emit — `preset: branch`, `preset: shared`, or a preset that is not a
// topology of the component's kind — is a structured render error naming its
// issue, and so is a preset the ClusterProfile says the cluster cannot host.
// That check needs a cluster profile, which validation deliberately does not
// have (ADR-0001), so it lives in the renderer; every surface that can reach a
// cluster goes through it (docs/data-services.md).

// notImplemented is one gated field: where it lives in the schema and where
// the work that would make it real is tracked.
type notImplemented struct {
	// Kind is KindProject or KindEnvironment: the two documents share field
	// names (`$.spec.components` exists on both) but not their gate status.
	Kind string

	// Path is the canonical spec path, with `[]` for a sequence entry and `*`
	// for a map key — the same shape the coverage test derives from the yaml
	// tags. A gate covers the path and everything beneath it.
	Path string

	// What names the feature in prose, for the error message.
	What string

	// TrackedBy is the milestone, and the epic issue where one is certain.
	// Milestone names come from docs/roadmap.md; a wrong issue number is worse
	// than no issue number, so this stays coarse where the tracker is unclear.
	TrackedBy string
}

// notImplementedFields is the gate table (issue #141). Each entry is one
// feature that validates today and renders nothing. Delete a row when the
// milestone that implements it lands — the coverage test in
// coverage_test.go then requires the field to be on the rendered allow-list
// instead, so a feature cannot quietly go back to being silent.
var notImplementedFields = []notImplemented{
	{
		Kind: KindProject,
		Path: "$.spec.components[].tools",
		What: "per-agent tool policy",
		// ADR-0014 lands `kind: agent` thin: the identity is real today (every
		// component renders its own ServiceAccount) and the capability policy
		// is not. An allow-list nothing enforces is the silent success this
		// gate exists to prevent, so the field is validated and refused.
		TrackedBy: "milestone M7 · Agent surface & MCP, issue #75",
	},
	{
		Kind: KindProject,
		Path: "$.spec.defaults.policy.deployers",
		What: "human deployer lists",
		// ADR-0025 narrowed this row rather than deleting it. Everything else
		// under `policy:` is agent policy and kelson-server enforces it as of
		// issue #75; `deployers` is about *humans*, and kelson has no notion of
		// a human subject beyond "holds the password" (ADR-0013 §3, ADR-0024
		// §8). A list of names nothing can resolve is exactly the silence this
		// table exists to prevent.
		TrackedBy: "milestone M11 · Teams, RBAC & multi-tenancy",
	},
	{
		Kind: KindProject,
		Path: "$.spec.build.by",
		What: "the CI build hand-off",
		// `by: ci` is a promise that kelson waits for a ReportBuild instead of
		// building, and there is no ReportBuild yet — so accepting the field
		// would tell an author their pipeline is wired up when nothing is
		// listening for it (ADR-0034 decision 3).
		TrackedBy: "ADR-0034 · forge-driven delivery, the BuildService.ReportBuild slice after R2 (issue #225)",
	},
	{
		Kind:      KindEnvironment,
		Path:      "$.spec.cluster",
		What:      "multi-cluster targeting",
		TrackedBy: "milestone M10 · Environments & promotion",
	},
	{
		Kind:      KindEnvironment,
		Path:      "$.spec.policy.deployers",
		What:      "human deployer lists",
		TrackedBy: "milestone M11 · Teams, RBAC & multi-tenancy",
	},
}

// gateFor returns the gate covering a canonical path for a document kind.
func gateFor(kind, canonical string) (notImplemented, bool) {
	for _, g := range notImplementedFields {
		if g.Kind == kind && g.Path == canonical {
			return g, true
		}
	}
	return notImplemented{}, false
}

// gatedBy reports the gate covering a canonical path or any of its ancestors,
// which is how the coverage test decides a leaf is accounted for.
func gatedBy(kind, canonical string) (notImplemented, bool) {
	for _, g := range notImplementedFields {
		if g.Kind != kind {
			continue
		}
		if canonical == g.Path || strings.HasPrefix(canonical, g.Path+".") || strings.HasPrefix(canonical, g.Path+"[") {
			return g, true
		}
	}
	return notImplemented{}, false
}

// gate reports a gated field as not implemented. canonical selects the gate
// row; field is the concrete path the author wrote, with real indices and map
// keys, so the error points at their line. Calling gate on an ungated path is
// a no-op, and TestGateTableIsEnforced pins every row to a live call site.
func (v *validator) gate(canonical, field string) {
	g, ok := gateFor(v.kind, canonical)
	if !ok {
		return
	}
	v.err(ErrNotImplemented, field,
		fmt.Sprintf("%s is not implemented: kelson validates %s but renders nothing for it", trimRoot(g.Path), g.What),
		fmt.Sprintf("remove %s until %s lands; kelson rejects a field it cannot render rather than accepting it and doing nothing (issue #141)",
			trimRoot(g.Path), g.TrackedBy))
}

// trimRoot turns "$.spec.components" into "spec.components" for prose.
func trimRoot(path string) string {
	return strings.TrimPrefix(path, "$.")
}
