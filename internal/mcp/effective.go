package mcp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

const effectiveConfigDescription = `Answer "what does this component actually run with, and why" for one (project, environment) — the winning value of every setting, and the block that set it.

READ-ONLY. Changes nothing. Composes SpecService.GetEffectiveConfig, itself a read of the stored spec: the same two documents get_spec's underlying RPC returns, merged by the same code the renderer's input is merged by, with the winning scope recorded per setting. Nothing here re-derives a merge — a client that did would drift from internal/model in exactly the direction that makes a provenance claim wrong.

Each row states the setting's name, its winning value, and a plain-language provenance sentence: kelson's default, set on the project, set on the component, set on <environment>, or set on <environment>, for this component. Below a row that shadowed something, a "was:" line names the losing value and where it came from and where it went — "was: debug, set on the component, overridden for production" — outermost first, the order the merge applied the scopes in.

A {secret, key} or a {from: {service, key}} reference is reported as the reference it is, never a value: kelson does not read the Secret at any point, so there is never a value here to print, current or shadowed.

A helm component reports no settings of its own — no precedence rule reaches a chart, and that is the answer rather than an omission. A component whose image cannot be resolved reports no image row rather than a guess; the server's own errors say why when the whole spec fails to resolve.

Preconditions: the project must be stored (put_spec) and declare the named environment; environment may be omitted only when the project declares exactly one. Naming a component the project does not declare is a caller error, answered as one, not an empty table. A spec that does not currently resolve answers with the validation errors instead of a config — the RPC succeeds and says so, the same way put_spec and deploy report an invalid spec as an answer rather than a transport failure.`

type effectiveConfigInput struct {
	Project     string `json:"project" jsonschema:"the stored project name, as reported by list_components"`
	Environment string `json:"environment,omitempty" jsonschema:"the environment to answer for; may be omitted only when the project declares exactly one"`
	Component   string `json:"component,omitempty" jsonschema:"one component name; omitted means every component the project declares"`
}

func effectiveConfigTool(c *clients) tool {
	def := readOnlyTool("effective_config", "What a component runs with, and why", effectiveConfigDescription)
	return tool{
		def:  def,
		rpcs: []rpc{rpcGetEffectiveConfig},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in effectiveConfigInput) (*mcpsdk.CallToolResult, any, error) {
				return c.effectiveConfig(ctx, in)
			})
		},
	}
}

// effectiveConfig composes one RPC and presents its answer the way the table
// does: per setting, the winning value, a plain-language provenance sentence,
// and the shadowed values it replaced (#268). Nothing here merges anything —
// the merge is GetEffectiveConfig's, computed by internal/model's walk, and
// this tool relays it exactly as ui/src/config/effective.ts does for the UI,
// in prose rather than rows.
func (c *clients) effectiveConfig(ctx context.Context, in effectiveConfigInput) (*mcpsdk.CallToolResult, any, error) {
	res, err := c.spec.GetEffectiveConfig(ctx, connect.NewRequest(&kelsonv1alpha1.GetEffectiveConfigRequest{
		Project:     in.Project,
		Environment: in.Environment,
		Component:   in.Component,
	}))
	if err != nil {
		return nil, nil, c.fail(rpcGetEffectiveConfig, err)
	}
	msg := res.Msg

	var r report
	if errs := msg.GetErrors(); len(errs) > 0 {
		r.addf("%s/%s: the spec does not resolve, so there is no effective configuration to report.", in.Project, orDash(in.Environment))
		shown, dropped := limit(errs, maxSpecErrors)
		r.section("ERRORS")
		for _, e := range shown {
			r.wireError("  ", e)
		}
		r.truncated(dropped, "errors")
		return text(&r)
	}

	config := msg.GetConfig()
	if config == nil {
		r.addf("%s/%s: the server returned no effective configuration.", in.Project, orDash(in.Environment))
		return text(&r)
	}

	r.addf("%s/%s: effective configuration.", config.GetProject(), config.GetEnvironment())

	if settings := config.GetSettings(); len(settings) > 0 {
		r.section("THIS ENVIRONMENT (policy, secret backend — not per component)")
		writeEffectiveSettings(&r, settings)
	}

	components := config.GetComponents()
	if len(components) == 0 {
		r.addf("\n(no component matched)")
		return text(&r)
	}
	for _, comp := range components {
		r.section(fmt.Sprintf("%s (%s)", comp.GetName(), orDash(comp.GetKind())))
		if len(comp.GetSettings()) == 0 {
			r.addf("  (no settings kelson resolves for this component — a helm chart's values are the chart's own, not the spec's)")
			continue
		}
		writeEffectiveSettings(&r, comp.GetSettings())
	}
	return text(&r)
}

// writeEffectiveSettings renders one group of settings: the winner and, when
// it shadowed anything, a "was:" line per loser — outermost first, exactly the
// order EffectiveSetting.shadowed carries on the wire (effectiveconfig.proto).
func writeEffectiveSettings(r *report, settings []*kelsonv1alpha1.EffectiveSetting) {
	for _, s := range settings {
		r.addf("  %s = %s", s.GetName(), effectiveValueText(s.GetValue()))
		r.addf("    %s", setAtSentence(s.GetSetAt()))
		for _, sh := range s.GetShadowed() {
			r.addf("    was: %s — %s", effectiveValueText(sh.GetValue()), shadowSentence(sh.GetSetAt(), s.GetSetAt()))
		}
	}
}

// effectiveValueText renders a winning or shadowed value. A secret or a
// binding reference renders as the reference it is — kelson never reads the
// Secret, so there is no value here to render instead (ADR-0009, ADR-0018).
func effectiveValueText(v *kelsonv1alpha1.EffectiveValue) string {
	switch {
	case v.GetSecret() != nil:
		return fmt.Sprintf("{secret: %s, key: %s}", v.GetSecret().GetSecret(), v.GetSecret().GetKey())
	case v.GetBinding() != nil:
		return fmt.Sprintf("{from: {service: %s, key: %s}}", v.GetBinding().GetService(), v.GetBinding().GetKey())
	}
	if literal := v.GetLiteral(); literal != "" {
		return literal
	}
	return `""`
}

// setAtSentence is the plain-language answer to "set at:" — the same five
// levels ui/src/config/effective.ts words for the UI, said as facts about the
// two documents rather than the rule numbers that govern them (docs/model.md
// P1–P6). A level this build does not know reads "not stated" rather than
// falling back to something plausible.
func setAtSentence(at *kelsonv1alpha1.SetAt) string {
	environment := orThisEnvironment(at.GetEnvironment())
	switch at.GetLevel() {
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_BUILT_IN:
		return "kelson's default"
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_PROJECT:
		return "set on the project"
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_COMPONENT:
		return "set on the component"
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT:
		return fmt.Sprintf("set on %s", environment)
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT_COMPONENT:
		return fmt.Sprintf("set on %s, for this component", environment)
	default:
		return "not stated"
	}
}

// overriddenBy is the second half of a shadow's sentence, said from the
// winner's side — "overridden for production" — because the first half
// already names where the lost value was written, and the reader's question
// is where it went.
func overriddenBy(at *kelsonv1alpha1.SetAt) string {
	environment := orThisEnvironment(at.GetEnvironment())
	switch at.GetLevel() {
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_PROJECT:
		return "overridden on the project"
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_COMPONENT:
		return "overridden on the component"
	case kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT, kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT_COMPONENT:
		return fmt.Sprintf("overridden for %s", environment)
	default:
		return "overridden"
	}
}

// shadowSentence is one shadowed value's whole sentence: where it was
// written, and where it was taken away — "set on the component, overridden
// for production".
func shadowSentence(shadow, winner *kelsonv1alpha1.SetAt) string {
	return setAtSentence(shadow) + ", " + overriddenBy(winner)
}

func orThisEnvironment(name string) string {
	if name == "" {
		return "this environment"
	}
	return name
}
