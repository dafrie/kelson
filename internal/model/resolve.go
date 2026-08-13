package model

import (
	"fmt"
	"strings"
)

// ImageUnresolved is the image a ResolvedApplication carries when the spec
// builds it from source and no build result has been supplied yet. It is a
// sentinel, not a reference: it must never reach a manifest, and the renderer
// refuses to render an application still carrying it (issue #136).
const ImageUnresolved = "@"

// Resolved is the precedence-resolved output for one (Project, Environment)
// pair — the concrete input the renderer consumes (issue #26). Resolution
// applies every rule from docs/model.md (P1–P6) and the built-in defaults.
type Resolved struct {
	Project      string
	Environment  ResolvedEnvironment
	Applications []ResolvedApplication
	Services     []ResolvedService
	Overlays     []Overlay
}

type ResolvedEnvironment struct {
	Name      string
	Cluster   string
	Namespace string
	Routing   ResolvedRouting
	Delivery  Delivery      // git always present for flux/argocd
	Mode      DeliveryMode  // after the P4 default chain
	Policy    Policy        // after the P4 default chain
	Secrets   SecretBackend // after the P4 default chain
}

type ResolvedRouting struct {
	DomainSuffix string
	GatewayClass string
	TLS          bool // defaulted to true
}

type ResolvedApplication struct {
	Name      string
	Kind      WorkloadKind
	Image     string
	Command   []string
	Port      int
	Health    string
	Schedule  string
	Domains   []string            // explicit domains, or the derived default hostname
	Replicas  Replicas            // after P2 defaults
	Resources *Resources          // after P2; nil if unset at both scopes
	Env       map[string]EnvValue // P1 merge: project < application < environment
}

type ResolvedService struct {
	Name   string
	Type   string
	Preset ServicePreset // after the P5 environment override
}

// Resolve validates the (Project, Environment) pair and returns the effective
// spec with all precedence rules applied. Any validation error aborts
// resolution — a spec that does not validate does not render.
func Resolve(p *Project, e *Environment) (*Resolved, Errors) {
	if errs := ValidateSet(p, e); len(errs) > 0 {
		return nil, errs
	}
	return resolve(p, e), nil
}

// resolve applies the precedence rules without validating. It is split out
// because issue #141 gates fields the resolver still resolves — services,
// policy and secret backends among them. Keeping resolution reachable without
// the gate means P4 and P5 stay under test, and means landing M7/M8/M9 is a
// matter of deleting a gate row rather than rebuilding precedence.
func resolve(p *Project, e *Environment) *Resolved {
	r := &Resolved{Project: p.Metadata.Name}

	// Environment identity and target.
	ns := e.Spec.Namespace
	if ns == "" {
		ns = fmt.Sprintf("%s-%s", p.Metadata.Name, e.Metadata.Name)
	}
	r.Environment.Name = e.Metadata.Name
	r.Environment.Cluster = e.Spec.Cluster
	r.Environment.Namespace = ns

	if routing := e.Spec.Routing; routing != nil {
		r.Environment.Routing.DomainSuffix = routing.DomainSuffix
		r.Environment.Routing.GatewayClass = routing.GatewayClass
		r.Environment.Routing.TLS = true
		if routing.TLS != nil {
			r.Environment.Routing.TLS = *routing.TLS
		}
	} else {
		r.Environment.Routing.TLS = true
	}

	// P4: Environment value whole, else Project default, else built-in.
	r.Environment.Mode = DeliveryDirect
	r.Environment.Policy = Policy{Agents: AgentsProposeOnly}
	r.Environment.Secrets = SecretBackend{Backend: SecretsCluster}
	if d := p.Spec.Defaults; d != nil {
		if d.DeliveryMode != "" {
			r.Environment.Mode = d.DeliveryMode
		}
		if d.Policy != nil {
			r.Environment.Policy = *d.Policy
		}
		if d.Secrets != nil {
			r.Environment.Secrets = *d.Secrets
		}
	}
	if d := e.Spec.Delivery; d != nil {
		r.Environment.Delivery = *d
		if d.Mode != "" {
			r.Environment.Mode = d.Mode
		}
	}
	r.Environment.Delivery.Mode = r.Environment.Mode
	if pol := e.Spec.Policy; pol != nil {
		r.Environment.Policy = *pol
	}
	if sb := e.Spec.Secrets; sb != nil {
		r.Environment.Secrets = *sb
	}

	// P5: service preset overrides by name.
	svcPresets := map[string]ServicePreset{}
	for _, ov := range e.Spec.Services {
		svcPresets[ov.Name] = ov.Preset
	}
	for _, svc := range p.Spec.Services {
		preset := svc.Preset
		if preset == "" {
			preset = PresetShared
		}
		if ov, ok := svcPresets[svc.Name]; ok {
			preset = ov
		}
		r.Services = append(r.Services, ResolvedService{Name: svc.Name, Type: svc.Type, Preset: preset})
	}

	// P6: project overlays first.
	r.Overlays = append(r.Overlays, p.Spec.Overlays...)
	r.Overlays = append(r.Overlays, e.Spec.Overlays...)

	// Applications: P1 (env), P2 (replicas/resources), P3 (image/command).
	overrides := map[string]AppOverride{}
	for _, ov := range e.Spec.Applications {
		overrides[ov.Name] = ov
	}
	builtFromSource := p.Spec.Source != nil && (p.Spec.Build == nil || p.Spec.Build.Strategy != BuildNone)
	for _, app := range p.Spec.Applications {
		ra := ResolvedApplication{
			Name:     app.Name,
			Kind:     app.Workload(),
			Port:     app.Port,
			Health:   app.Health,
			Schedule: app.Schedule,
			Replicas: Replicas{Min: 1},
		}
		if app.Replicas != nil {
			ra.Replicas = *app.Replicas
		}
		ra.Env = map[string]EnvValue{}
		for k, ev := range p.Spec.Env {
			ra.Env[k] = ev
		}
		for k, ev := range app.Env {
			ra.Env[k] = ev
		}
		ra.Image, ra.Command = app.Image, app.Command
		if ra.Image == "" {
			ra.Image = p.Spec.Image
		}
		if ra.Image == "" && builtFromSource {
			// The build plane fills the digest in; until it does, the image is
			// explicitly unresolved rather than blank, so a consumer can tell
			// "waiting on a build" from "the spec named nothing" (issue #136).
			ra.Image = ImageUnresolved
		}
		ra.Resources = app.Resources
		ra.Domains = app.Domains

		if ov, ok := overrides[app.Name]; ok {
			if ov.Replicas != nil {
				ra.Replicas = *ov.Replicas
			}
			if ov.Resources != nil {
				ra.Resources = ov.Resources
			}
			for k, ev := range ov.Env {
				ra.Env[k] = ev
			}
		}

		// Domain defaulting.
		if len(ra.Domains) == 0 && ra.Port != 0 && r.Environment.Routing.DomainSuffix != "" {
			ra.Domains = []string{fmt.Sprintf("%s.%s", app.Name, strings.TrimPrefix(r.Environment.Routing.DomainSuffix, "."))}
		}
		r.Applications = append(r.Applications, ra)
	}

	return r
}
