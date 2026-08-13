package model

import (
	"fmt"
	"strings"
)

// ImageUnresolved is the image a ResolvedComponent carries when the spec
// builds it from source and no build result has been supplied yet. It is a
// sentinel, not a reference: it must never reach a manifest, and the renderer
// refuses to render a component still carrying it (issue #136).
const ImageUnresolved = "@"

// Resolved is the precedence-resolved output for one (Project, Environment)
// pair — the concrete input the renderer consumes (issue #26). Resolution
// applies every rule from docs/model.md (P1–P6) and the built-in defaults.
//
// The spec's one components list (ADR-0014) resolves into two slices, because
// the two halves are consumed differently and by different rules: a workload
// carries the P1/P2/P3 merge and renders pods, a data component carries the P5
// preset override and renders an operator's resources. Keeping them apart here
// is what lets resolve() read as one rule per paragraph, and lets Render state
// its ordering contract — data services before the workloads that bind to
// them — as two loops rather than two passes over one list with a kind test in
// each.
type Resolved struct {
	Project      string
	Environment  ResolvedEnvironment
	Components   []ResolvedComponent
	DataServices []ResolvedDataService
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

// ResolvedComponent is one workload-kind component after resolution:
// service, worker, cron or agent.
type ResolvedComponent struct {
	Name      string
	Kind      ComponentKind
	Image     string
	Command   []string
	Port      int
	Health    string
	Schedule  string
	Domains   []string            // explicit domains, or the derived default hostname
	Replicas  Replicas            // after P2 defaults
	Resources *Resources          // after P2; nil if unset at both scopes
	Env       map[string]EnvValue // P1 merge: project < component < environment
}

// ResolvedDataService is one data-kind component after resolution.
type ResolvedDataService struct {
	Name   string
	Kind   ComponentKind // postgres | valkey
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
// because issue #141 gates fields the resolver still resolves — policy and
// secret backends among them. Keeping resolution reachable without the gate
// means P4 stays under test, and means landing M7/M8 is a matter of deleting a
// gate row rather than rebuilding precedence. M9 already proved it: data
// components and their P5 preset override left the gate table when the
// renderer began emitting CloudNativePG resources (issue #89), and nothing
// here changed.
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

	// P6: project overlays first.
	r.Overlays = append(r.Overlays, p.Spec.Overlays...)
	r.Overlays = append(r.Overlays, e.Spec.Overlays...)

	// One spec list, two resolutions: P5 for data components, P1/P2/P3 for
	// workloads. Spec order is preserved within each.
	overrides := map[string]ComponentOverride{}
	for _, ov := range e.Spec.Components {
		overrides[ov.Name] = ov
	}
	builtFromSource := p.Spec.Source != nil && (p.Spec.Build == nil || p.Spec.Build.Strategy != BuildNone)
	for _, c := range p.Spec.Components {
		if kind := c.EffectiveKind(); kind.IsData() {
			r.DataServices = append(r.DataServices, resolveDataService(c, kind, overrides[c.Name]))
			continue
		}
		r.Components = append(r.Components, resolveComponent(p, r, c, overrides[c.Name], builtFromSource))
	}

	return r
}

// resolveDataService applies P5: the Environment's preset override, else the
// component's own preset, else the built-in `shared`.
func resolveDataService(c Component, kind ComponentKind, ov ComponentOverride) ResolvedDataService {
	preset := c.Preset
	if preset == "" {
		preset = PresetShared
	}
	if ov.Preset != "" {
		preset = ov.Preset
	}
	return ResolvedDataService{Name: c.Name, Kind: kind, Preset: preset}
}

// resolveComponent applies P1 (env merge), P2 (replicas/resources) and P3
// (image/command) to one workload component, then the domain default.
func resolveComponent(p *Project, r *Resolved, c Component, ov ComponentOverride, builtFromSource bool) ResolvedComponent {
	rc := ResolvedComponent{
		Name:     c.Name,
		Kind:     c.EffectiveKind(),
		Port:     c.Port,
		Health:   c.Health,
		Schedule: c.Schedule,
		Replicas: Replicas{Min: 1},
	}
	if c.Replicas != nil {
		rc.Replicas = *c.Replicas
	}
	rc.Env = map[string]EnvValue{}
	for k, ev := range p.Spec.Env {
		rc.Env[k] = ev
	}
	for k, ev := range c.Env {
		rc.Env[k] = ev
	}
	rc.Image, rc.Command = c.Image, c.Command
	if rc.Image == "" {
		rc.Image = p.Spec.Image
	}
	if rc.Image == "" && builtFromSource {
		// The build plane fills the digest in; until it does, the image is
		// explicitly unresolved rather than blank, so a consumer can tell
		// "waiting on a build" from "the spec named nothing" (issue #136).
		rc.Image = ImageUnresolved
	}
	rc.Resources = c.Resources
	rc.Domains = c.Domains

	if ov.Replicas != nil {
		rc.Replicas = *ov.Replicas
	}
	if ov.Resources != nil {
		rc.Resources = ov.Resources
	}
	for k, ev := range ov.Env {
		rc.Env[k] = ev
	}

	// Domain defaulting.
	if len(rc.Domains) == 0 && rc.Port != 0 && r.Environment.Routing.DomainSuffix != "" {
		rc.Domains = []string{fmt.Sprintf("%s.%s", c.Name, strings.TrimPrefix(r.Environment.Routing.DomainSuffix, "."))}
	}
	return rc
}
