package api

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// GetEffectiveConfig answers what a component actually runs with in one
// environment, and which block of which document set each value (#260).
//
// It is a read of the spec store and nothing else: the same documents GetSpec
// returns, merged by the same code the renderer's input is merged by
// (internal/model's effective.go), with the winning scope recorded per setting.
// No cluster is touched, nothing is applied, and there is no new authorization
// question — this says strictly less than the documents GetSpec already hands
// back, so it takes the same read row in the scope table.
//
// A spec that does not validate is reported in `errors` with the RPC itself
// succeeding, exactly as PutSpec and Render report one: an unresolvable spec is
// an answer to "what does this run with", not a transport failure. A project or
// an environment that does not exist is the caller's argument being wrong and
// stays a ConnectRPC error.
func (s *Server) GetEffectiveConfig(ctx context.Context, req *connect.Request[kelsonv1alpha1.GetEffectiveConfigRequest]) (*connect.Response[kelsonv1alpha1.GetEffectiveConfigResponse], error) {
	msg := req.Msg
	ref := &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: msg.GetProject()}}
	spec, err := s.resolveSpec(ctx, ref)
	if err != nil {
		if wire := specFindings(err); len(wire) > 0 {
			return connect.NewResponse(&kelsonv1alpha1.GetEffectiveConfigResponse{Errors: wire}), nil
		}
		return nil, failRequest(err)
	}
	environment, err := selectEnvironment(spec.environments, msg.GetEnvironment())
	if err != nil {
		return nil, fail(connect.CodeInvalidArgument, err)
	}

	effective, errs := model.EffectiveConfig(spec.project, environment)
	if len(errs) > 0 {
		return connect.NewResponse(&kelsonv1alpha1.GetEffectiveConfigResponse{Errors: wireErrors(errs)}), nil
	}

	// A component filter that names nothing is the caller's argument being
	// wrong, and it is answered as such rather than by returning an empty table
	// — which would be indistinguishable from a chart, whose table is
	// legitimately empty.
	if name := msg.GetComponent(); name != "" {
		one := effective.ComponentNamed(name)
		if one == nil {
			return nil, fail(connect.CodeInvalidArgument, fmt.Errorf(
				"api: project %q declares no component %q (declares: %s)",
				effective.Project, name, strings.Join(componentNames(effective), ", ")))
		}
		effective.Components = []model.EffectiveComponent{*one}
	}

	return connect.NewResponse(&kelsonv1alpha1.GetEffectiveConfigResponse{
		Config: wireEffective(effective),
	}), nil
}

func componentNames(e *model.Effective) []string {
	names := make([]string, 0, len(e.Components))
	for _, c := range e.Components {
		names = append(names, c.Name)
	}
	return names
}

func wireEffective(e *model.Effective) *kelsonv1alpha1.EffectiveConfig {
	out := &kelsonv1alpha1.EffectiveConfig{
		Project:     e.Project,
		Environment: e.Environment,
		Settings:    wireSettings(e.Settings),
	}
	for _, c := range e.Components {
		out.Components = append(out.Components, &kelsonv1alpha1.EffectiveComponent{
			Name:     c.Name,
			Kind:     string(c.Kind),
			Settings: wireSettings(c.Settings),
		})
	}
	return out
}

func wireSettings(settings []model.EffectiveSetting) []*kelsonv1alpha1.EffectiveSetting {
	if len(settings) == 0 {
		return nil
	}
	out := make([]*kelsonv1alpha1.EffectiveSetting, 0, len(settings))
	for _, s := range settings {
		out = append(out, &kelsonv1alpha1.EffectiveSetting{
			Name:  s.Name,
			Group: wireSettingGroup(s.Group),
			Value: wireEffectiveValue(s.Value),
			SetAt: wireSetAt(s.SetAt),
		})
	}
	return out
}

// wireEffectiveValue keeps a reference a reference. The two mapping arms carry
// a Secret's name and a key and never a value — there is none in the spec, none
// in the resolution and none to be had here (ADR-0009, ADR-0018) — so the wire
// shape has no field one could arrive in.
func wireEffectiveValue(v model.EnvValue) *kelsonv1alpha1.EffectiveValue {
	switch {
	case v.Secret != nil:
		return &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Secret{
			Secret: &kelsonv1alpha1.SecretKeyReference{Secret: v.Secret.Name, Key: v.Secret.Key},
		}}
	case v.From != nil:
		return &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Binding{
			Binding: &kelsonv1alpha1.ServiceBindingReference{Service: v.From.Service, Key: v.From.Key},
		}}
	}
	return &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Literal{Literal: v.Literal}}
}

func wireSetAt(at model.SetAt) *kelsonv1alpha1.SetAt {
	return &kelsonv1alpha1.SetAt{
		Level:       wireSetAtLevel(at.Level),
		Document:    at.Document,
		Environment: at.Environment,
		Component:   at.Component,
		Field:       at.Field,
	}
}

// wireSetAtLevel and wireSettingGroup are exhaustive maps rather than string
// passthroughs, because the wire types are enums and an unmapped level would
// reach a client as UNSPECIFIED — a provenance answer that says nothing, which
// is worse than one that says the wrong thing because nothing fails. A level
// added to the model without a row here fails effectiveconfig_test.go's
// round-trip over every level.
func wireSetAtLevel(level model.SetAtLevel) kelsonv1alpha1.SetAtLevel {
	switch level {
	case model.SetAtBuiltIn:
		return kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_BUILT_IN
	case model.SetAtProject:
		return kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_PROJECT
	case model.SetAtComponent:
		return kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_COMPONENT
	case model.SetAtEnvironment:
		return kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT
	case model.SetAtEnvironmentComponent:
		return kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT_COMPONENT
	}
	return kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_UNSPECIFIED
}

func wireSettingGroup(group model.SettingGroup) kelsonv1alpha1.SettingGroup {
	switch group {
	case model.GroupEnv:
		return kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV
	case model.GroupWorkload:
		return kelsonv1alpha1.SettingGroup_SETTING_GROUP_WORKLOAD
	case model.GroupData:
		return kelsonv1alpha1.SettingGroup_SETTING_GROUP_DATA
	case model.GroupEnvironment:
		return kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENVIRONMENT
	}
	return kelsonv1alpha1.SettingGroup_SETTING_GROUP_UNSPECIFIED
}
