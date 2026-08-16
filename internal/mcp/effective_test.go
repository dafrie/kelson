package mcp

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// TestEffectiveConfigRendersSettingsAndShadows: the happy path — a winning
// value, its provenance sentence, and a shadowed value's "was:" line, in the
// wire's own outermost-first order (#268).
func TestEffectiveConfigRendersSettingsAndShadows(t *testing.T) {
	var request *kelsonv1alpha1.GetEffectiveConfigRequest
	h := start(t, &fakeServer{
		getEffectiveConfig: func(req *kelsonv1alpha1.GetEffectiveConfigRequest) (*kelsonv1alpha1.GetEffectiveConfigResponse, error) {
			request = req
			return &kelsonv1alpha1.GetEffectiveConfigResponse{
				Config: &kelsonv1alpha1.EffectiveConfig{
					Project:     "hello",
					Environment: "production",
					Settings: []*kelsonv1alpha1.EffectiveSetting{
						{
							Name:  "secrets.backend",
							Group: kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENVIRONMENT,
							Value: &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Literal{Literal: "cluster"}},
							SetAt: &kelsonv1alpha1.SetAt{Level: kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_BUILT_IN},
						},
					},
					Components: []*kelsonv1alpha1.EffectiveComponent{
						{
							Name: "web",
							Kind: "service",
							Settings: []*kelsonv1alpha1.EffectiveSetting{
								{
									Name:  "LOG_LEVEL",
									Group: kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV,
									Value: &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Literal{Literal: "info"}},
									SetAt: &kelsonv1alpha1.SetAt{
										Level:    kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_ENVIRONMENT_COMPONENT,
										Document: "Environment", Environment: "production", Component: "web",
										Field: "$.spec.components[0].env.LOG_LEVEL",
									},
									Shadowed: []*kelsonv1alpha1.ShadowedValue{
										{
											Value: &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Literal{Literal: "debug"}},
											SetAt: &kelsonv1alpha1.SetAt{
												Level: kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_COMPONENT,
												Field: "$.spec.components[0].env.LOG_LEVEL",
											},
										},
									},
								},
							},
						},
					},
				},
			}, nil
		},
	})

	out := h.call(t, "effective_config", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"hello/production: effective configuration.",
		"secrets.backend = cluster",
		"kelson's default",
		"web (service)",
		"LOG_LEVEL = info",
		"set on production, for this component",
		"was: debug — set on the component, overridden for production",
	)
	if request.GetProject() != "hello" || request.GetEnvironment() != "production" {
		t.Errorf("request = %+v, want project=hello environment=production", request)
	}
}

// TestEffectiveConfigNeverReturnsASecretValue: a {secret, key} reference must
// render as the reference it is, current or shadowed, and never as a value —
// kelson does not read the Secret at any point.
func TestEffectiveConfigNeverReturnsASecretValue(t *testing.T) {
	h := start(t, &fakeServer{
		getEffectiveConfig: func(*kelsonv1alpha1.GetEffectiveConfigRequest) (*kelsonv1alpha1.GetEffectiveConfigResponse, error) {
			return &kelsonv1alpha1.GetEffectiveConfigResponse{
				Config: &kelsonv1alpha1.EffectiveConfig{
					Project: "hello", Environment: "production",
					Components: []*kelsonv1alpha1.EffectiveComponent{
						{
							Name: "web",
							Kind: "service",
							Settings: []*kelsonv1alpha1.EffectiveSetting{
								{
									Name:  "DATABASE_URL",
									Group: kelsonv1alpha1.SettingGroup_SETTING_GROUP_ENV,
									Value: &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Secret{
										Secret: &kelsonv1alpha1.SecretKeyReference{Secret: "checkout-db", Key: "url"},
									}},
									SetAt: &kelsonv1alpha1.SetAt{Level: kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_PROJECT},
									Shadowed: []*kelsonv1alpha1.ShadowedValue{
										{
											Value: &kelsonv1alpha1.EffectiveValue{Value: &kelsonv1alpha1.EffectiveValue_Binding{
												Binding: &kelsonv1alpha1.ServiceBindingReference{Service: "db", Key: "url"},
											}},
											SetAt: &kelsonv1alpha1.SetAt{Level: kelsonv1alpha1.SetAtLevel_SET_AT_LEVEL_BUILT_IN},
										},
									},
								},
							},
						},
					},
				},
			}, nil
		},
	})

	out := h.call(t, "effective_config", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"DATABASE_URL = {secret: checkout-db, key: url}",
		"was: {from: {service: db, key: url}}",
	)
	// The literal never appears anywhere in the answer — this tool has no field
	// a secret value could have arrived in even if the fake staged one.
	mustNotContain(t, out, "postgres://", "hunter2")
}

// TestEffectiveConfigReportsUnresolvableSpecAsAnAnswer: a spec that does not
// resolve is answered with the validation errors, not a transport failure —
// GetEffectiveConfig's own contract, relayed rather than turned into a tool
// error.
func TestEffectiveConfigReportsUnresolvableSpecAsAnAnswer(t *testing.T) {
	h := start(t, &fakeServer{
		getEffectiveConfig: func(*kelsonv1alpha1.GetEffectiveConfigRequest) (*kelsonv1alpha1.GetEffectiveConfigResponse, error) {
			return &kelsonv1alpha1.GetEffectiveConfigResponse{Errors: []*kelsonv1alpha1.Error{
				{Code: "image/unresolved", Application: "worker", Message: "no image and no source", Remediation: "set spec.image or spec.source"},
			}}, nil
		},
	})

	out := h.call(t, "effective_config", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"the spec does not resolve",
		"code: image/unresolved",
		"application: worker",
		"remediation: set spec.image or spec.source",
	)
}

// TestEffectiveConfigRefusesAnUnknownComponent: the server's own refusal (a
// component the project does not declare) arrives as a ConnectRPC error and
// the tool relays it, exactly as [clients.fail] does for every other tool —
// no MCP-side re-interpretation of the API's own taxonomy.
func TestEffectiveConfigRefusesAnUnknownComponent(t *testing.T) {
	h := start(t, &fakeServer{
		getEffectiveConfig: func(*kelsonv1alpha1.GetEffectiveConfigRequest) (*kelsonv1alpha1.GetEffectiveConfigResponse, error) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("api: project %q declares no component %q (declares: web, worker)", "hello", "ghost"))
		},
	})

	out := h.callErr(t, "effective_config", map[string]any{"project": "hello", "environment": "production", "component": "ghost"})
	mustContain(t, out,
		"kelson.v1alpha1.SpecService.GetEffectiveConfig failed: invalid_argument",
		"declares no component",
	)
}
