package model

import (
	"fmt"
	"strings"
	"time"
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
// The spec's one components list (ADR-0014) resolves into three slices, because
// each part is consumed differently and by different rules: a workload carries
// the P1/P2/P3 merge and renders pods, a data component carries the P5 preset
// override and renders an operator's resources, and a helm component carries no
// rule at all and renders a HelmRelease (ADR-0016). Keeping them apart here is
// what lets resolve() read as one rule per paragraph, and lets Render state its
// ordering contract — the things workloads depend on before the workloads — as
// separate loops rather than passes over one list with a kind test in each.
type Resolved struct {
	Project string

	// Source is the Project's *default* source: what a component that names none
	// builds from (ADR-0035 decision 3), carried through so a plane holding a
	// resolved spec can resolve its git connection the way the server does
	// (ADR-0033 decision 4). Nil for a project that deploys a pre-built image
	// and declares no source at all — and for one that declares several and
	// names no default, which is legal exactly while every component that builds
	// names its own.
	//
	// It carries an explicit json tag with omitempty where its siblings carry
	// none, for the reason [ResolvedComponent.Release] does: this struct is
	// hashed into the artifact tag, so a nil pointer must marshal to nothing or
	// adding the field would have retagged every environment in every cluster.
	// A project that *does* have a source republishes once, which is correct —
	// where kelson reads the code from is part of what a revision is.
	Source *ResolvedSource `json:"source,omitempty"`

	// Sources are the per-component bindings: which source each component that
	// builds was resolved to, in spec order (ADR-0035 decision 3). It is empty
	// for a project that builds nothing.
	//
	// They live on the envelope rather than on [ResolvedComponent] deliberately.
	// Nothing about a source reaches a manifest — ADR-0035 decision 4 leaves
	// delivery unchanged, and the renderer never reads one — so a binding on the
	// component would enter `kelson.dev/spec-hash`, the per-resource annotation,
	// and churn every workload in every cluster over a field that changes no
	// rendered byte. Here it enters the artifact tag instead, which is the right
	// scope: the *revision* is what a repository and a ref are part of.
	Sources []ResolvedComponentSource `json:"sources,omitempty"`

	Environment  ResolvedEnvironment
	Components   []ResolvedComponent
	DataServices []ResolvedDataService
	// Charts are the `kind: helm` components, in spec order. They resolve into
	// a third slice for the same reason data services do: nothing in P1–P5
	// reaches them (a chart has no image, no env merge and no preset), and the
	// renderer emits a different pair of resources for them.
	Charts   []ResolvedChart
	Overlays []Overlay
}

// ResolvedSource is one source after binding: where the code lives, at which
// ref, and which connection kelson reads it with.
//
// There is no per-environment override and no default chain over a source's
// *fields* — what resolution decides is which declared source a component gets,
// not what is in it — so the values are carried rather than computed.
type ResolvedSource struct {
	// Name is the source's name: an entry of the Project's list, a GitSource the
	// instance declares, or [DefaultSourceName] for the singular spelling. It is
	// carried so a consumer can say *which* source it built from, and so a
	// shadowed global name is legible as the local one that won.
	Name string `json:"name,omitempty"`

	Git string `json:"git"`

	// Ref is the branch, tag or commit this source is read at (ADR-0035
	// decision 1). It is per source rather than per project, which is the whole
	// of what "a source" adds over a repository URL.
	Ref string `json:"ref,omitempty"`

	// Connection is `connection`: the GitConnection the author named, or empty
	// for the host match that is the common case (ADR-0033 decision 4).
	Connection string `json:"connection,omitempty"`
}

// ResolvedComponentSource is one component's binding: which component, and the
// source it builds from. A component that builds nothing — a data component, a
// chart, one pinned to an image — has no entry.
type ResolvedComponentSource struct {
	Component string         `json:"component"`
	Source    ResolvedSource `json:"source"`
}

// SourceFor returns the source a component builds from, or nil for one that
// builds nothing. It is how a plane holding a resolved spec asks the question
// ADR-0035 decision 4 puts to the build plane — clone *this* component's
// repository at *this* component's ref — without re-deriving the binding from
// the document and getting a different answer.
func (r *Resolved) SourceFor(component string) *ResolvedSource {
	for i := range r.Sources {
		if r.Sources[i].Component == component {
			return &r.Sources[i].Source
		}
	}
	return nil
}

type ResolvedEnvironment struct {
	Name      string
	Cluster   string
	Namespace string
	Routing   ResolvedRouting
	Delivery  Delivery      // git always present for flux
	Mode      DeliveryMode  // after the P4 default chain
	Policy    Policy        // after the P4 default chain
	Secrets   SecretBackend // after the P4 default chain

	// Previews is nil unless the Environment declares one. It carries kelson's
	// defaults already applied, so the renderer never has to know what an
	// unset interval or an unset limit means.
	Previews *ResolvedPreviews
}

// ResolvedPreviews is the previews declaration with defaults filled in
// (ADR-0017). Nothing merges into it — there is no per-project preview default
// and no override — so resolution is defaulting and nothing else.
//
// The environment's delivery mode is deliberately not copied here, for the
// same reason ResolvedChart does not copy it: it lives once, on
// ResolvedEnvironment, and the renderer's Flux-only gate reads it there.
type ResolvedPreviews struct {
	Provider  PreviewProvider       `json:"provider"`
	Repo      string                `json:"repo"`
	SecretRef string                `json:"secretRef"`
	Interval  string                `json:"interval"`
	Filter    ResolvedPreviewFilter `json:"filter"`
	// Skip is the CI-gating label list, flattened: PreviewSkip has one field
	// and a struct with one field buys nothing downstream.
	Skip      []string         `json:"skip,omitempty"`
	Artifacts PreviewArtifacts `json:"artifacts"`
}

// ResolvedPreviewFilter is the filter with Limit defaulted. Limit is never
// zero here: kelson always writes a ceiling (ADR-0017).
type ResolvedPreviewFilter struct {
	Labels        []string `json:"labels,omitempty"`
	IncludeBranch string   `json:"includeBranch,omitempty"`
	ExcludeBranch string   `json:"excludeBranch,omitempty"`
	Limit         int      `json:"limit"`
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
	Image     string // after P3: environment pin, else component, else project
	Command   []string
	Port      int
	Health    string
	Schedule  string
	Domains   []string            // explicit domains, or the derived default hostname
	Replicas  Replicas            // after P2 defaults
	Resources *Resources          // after P2; nil if unset at both scopes
	Env       map[string]EnvValue // P1 merge: project < component < environment

	// Release is the component's release-command hook with its timeout already
	// turned into seconds, so the pure renderer never has to parse a duration
	// (it may not import `time` at all — ADR-0001, issue #20).
	//
	// It carries an explicit json tag with omitempty where its siblings carry
	// none, for the reason ResolvedDataService.Auth does: this struct is hashed
	// into kelson.dev/spec-hash, so a nil pointer must marshal to nothing or
	// adding the field would have changed the annotation of every workload that
	// does not use it.
	Release *ResolvedRelease `json:"release,omitempty"`
}

// ResolvedRelease is a release hook after resolution: the command as written,
// and the timeout as a number of seconds. No precedence rule reaches it — an
// Environment override carries image, replicas, resources and env, and a
// release command is none of those — so resolution is defaulting and nothing
// else.
type ResolvedRelease struct {
	Command []string `json:"command"`
	// TimeoutSeconds is always positive: resolution fills DefaultReleaseTimeout
	// in, so the renderer never has to know what an unset timeout means.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// ResolvedDataService is one data-kind component after resolution.
type ResolvedDataService struct {
	Name   string
	Kind   ComponentKind // postgres | valkey
	Preset ServicePreset // after the P5 environment override
	// Auth is the Secret the component's password is read from, or nil for a
	// component that declares none. No precedence rule reaches it: P5 overrides
	// a data component's preset and nothing else, and the same Secret *name*
	// resolves per namespace anyway, so one reference on the Project is a
	// different Secret in each environment without the spec saying so twice.
	//
	// It carries an explicit json tag with omitempty where its siblings carry
	// none, because this struct is hashed into kelson.dev/spec-hash: a nil
	// pointer must marshal to nothing, or adding the field would have changed
	// the annotation of every data service that does not use it.
	Auth *SecretRef `json:"auth,omitempty"`
}

// ResolvedChart is one `kind: helm` component after resolution. There is
// nothing to resolve — no precedence rule reaches a chart — so it is the spec's
// own values, carried through in the shape the renderer writes.
//
// The environment's delivery mode is deliberately not copied here. It lives
// once, on ResolvedEnvironment, and the renderer's Flux-only gate reads it
// there: two copies of a mode would be two chances for them to disagree.
type ResolvedChart struct {
	Name    string         `json:"name"`
	Chart   string         `json:"chart"`
	Version string         `json:"version"`
	Source  ChartSource    `json:"source"`
	Values  map[string]any `json:"values,omitempty"`
	// ValuesFrom keeps spec order: helm-controller merges these in the order
	// they are listed, so it is meaning, not presentation.
	ValuesFrom []ValuesFrom `json:"valuesFrom,omitempty"`
}

// Resolve validates the (Project, Environment) pair and returns the effective
// spec with all precedence rules applied. Any validation error aborts
// resolution — a spec that does not validate does not render.
//
// `globals` are the sources the instance offers to every project — the
// GitSources of ADR-0035 decision 2, passed in rather than read. Resolution is
// a pure function of what it is given, exactly as rendering is of its
// ClusterProfile (ADR-0001, ADR-0029): the planes with cluster access list the
// GitSources and hand them here, and `kelson render` against a file with none is
// a resolution against an empty global tier rather than a resolution that went
// looking for one.
//
// It is variadic so that the caller with nothing global to say says nothing —
// the shape [ValidateSet] already uses for its environments. A component naming
// a source that is in neither the project's list nor `globals` is refused with
// ref/unknown-source listing both, so a forgotten list is an error naming what
// was in scope and never a silently different build.
func Resolve(p *Project, e *Environment, globals ...Source) (*Resolved, Errors) {
	if errs := ValidateSet(p, e); len(errs) > 0 {
		return nil, errs
	}
	r, errs := resolve(p, e, globals)
	if len(errs) > 0 {
		return nil, errs
	}
	return r, nil
}

// resolve applies the precedence rules without validating. It is split out
// because issue #141 gates fields the resolver still resolves — policy and
// secret backends among them. Keeping resolution reachable without the gate
// means P4 stays under test, and means landing M7/M8 is a matter of deleting a
// gate row rather than rebuilding precedence. M9 already proved it: data
// components and their P5 preset override left the gate table when the
// renderer began emitting CloudNativePG resources (issue #89), and nothing
// here changed.
func resolve(p *Project, e *Environment, globals []Source) (*Resolved, Errors) {
	r := &Resolved{Project: p.Metadata.Name}

	// Environment identity and target.
	ns := e.Spec.Namespace
	if ns == "" {
		ns = DefaultNamespace(p.Metadata.Name, e.Metadata.Name)
	}
	r.Environment.Name = e.Metadata.Name
	r.Environment.Cluster = e.Spec.Cluster
	r.Environment.Namespace = ns

	bindErrs := resolveSources(p, r, globals)

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
	r.Environment.Policy = EffectivePolicy(p, e)
	r.Environment.Secrets = SecretBackend{Backend: SecretsCluster}
	if d := p.Spec.Defaults; d != nil {
		if d.DeliveryMode != "" {
			r.Environment.Mode = d.DeliveryMode
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
	if sb := e.Spec.Secrets; sb != nil {
		r.Environment.Secrets = *sb
	}
	// The externalSecrets refresh interval is defaulted here, once, so the pure
	// renderer never has to know what an unset interval means — the same reason
	// resolvePreviews fills its own defaults in. It may not parse one either:
	// the renderer cannot import `time` (ADR-0001, issue #20), which is why
	// validation owns the duration check and this owns the default.
	if r.Environment.Secrets.Backend == SecretsExternalSecrets && r.Environment.Secrets.RefreshInterval == "" {
		r.Environment.Secrets.RefreshInterval = DefaultSecretRefreshInterval
	}
	// The sops backend's key-Secret name is defaulted in the same place and
	// for the same reason: the renderer writes it into a Kustomization's
	// spec.decryption.secretRef and must never have to decide what an empty
	// one means (ADR-0022).
	if r.Environment.Secrets.Backend == SecretsSOPS && r.Environment.Secrets.AgeKeySecret == "" {
		r.Environment.Secrets.AgeKeySecret = DefaultAgeKeySecret
	}
	r.Environment.Previews = resolvePreviews(e.Spec.Previews)

	// P6: project overlays first.
	r.Overlays = append(r.Overlays, p.Spec.Overlays...)
	r.Overlays = append(r.Overlays, e.Spec.Overlays...)

	// One spec list, three resolutions: P5 for data components, P1/P2/P3 for
	// workloads, and none at all for charts. Spec order is preserved within each.
	overrides := map[string]ComponentOverride{}
	for _, ov := range e.Spec.Components {
		overrides[ov.Name] = ov
	}
	// Whether a component's image comes from a build is now a per-component
	// question, because its source is (ADR-0035 decision 3): it is built when
	// something bound it to a source and the project's strategy is not `none`.
	// For the single-source project that is every workload, which is what it was
	// before this became a question at all.
	builds := p.Spec.Build == nil || p.Spec.Build.Strategy != BuildNone
	for _, c := range p.Spec.Components {
		switch kind := c.EffectiveKind(); {
		case kind.IsData():
			r.DataServices = append(r.DataServices, resolveDataService(c, kind, overrides[c.Name]))
		case kind.IsChart():
			r.Charts = append(r.Charts, resolveChart(c))
		default:
			builtFromSource := builds && r.SourceFor(c.Name) != nil
			r.Components = append(r.Components, resolveComponent(p, r, c, overrides[c.Name], builtFromSource))
		}
	}

	return r, bindErrs
}

// resolveSources binds every component that builds to the source it builds
// from, and records the Project's default (ADR-0035 decision 3).
//
// Resolution order is the project's own list, then the instance's: a
// project-local name shadows a global one, which is the same instinct as the
// P1–P3 precedence rules — the innermost scope wins. Shadowing rather than
// clashing is deliberate (ADR-0035): a global name is a convenience, not a
// claim, and a project must be able to redefine `tools` without asking the
// instance.
//
// A name in neither scope is refused here rather than in validate.go, because
// only here is the second half of the scope in hand. The refusal lists what was
// in scope, both halves labelled, so "I forgot to declare it" and "the instance
// does not offer it" are told apart without a second lookup.
func resolveSources(p *Project, r *Resolved, globals []Source) Errors {
	scope := make(map[string]Source, len(globals)+len(p.Spec.Sources))
	var globalNames []string
	for _, g := range globals {
		if g.Name == "" {
			continue
		}
		if _, dup := scope[g.Name]; !dup {
			globalNames = append(globalNames, g.Name)
		}
		scope[g.Name] = g
	}
	local := p.Spec.EffectiveSources()
	localNames := make([]string, 0, len(local))
	for _, src := range local {
		scope[src.Name] = src // project shadows global
		localNames = append(localNames, src.Name)
	}

	if def, ok := p.Spec.DefaultSource(); ok {
		bound := resolvedSource(def)
		r.Source = &bound
	}

	var errs Errors
	for i, c := range p.Spec.Components {
		if !c.EffectiveKind().IsWorkload() {
			continue
		}
		name := c.SourceName()
		if name == "" {
			if r.Source == nil {
				continue // an image-only component: nothing to bind
			}
			r.Sources = append(r.Sources, ResolvedComponentSource{Component: c.Name, Source: *r.Source})
			continue
		}
		src, ok := scope[name]
		if !ok {
			errs = append(errs, sourceNotInScope(p, i, c, localNames, globalNames))
			continue
		}
		r.Sources = append(r.Sources, ResolvedComponentSource{Component: c.Name, Source: resolvedSource(src)})
	}
	return errs
}

// resolvedSource carries one declared source into its resolved form. It exists
// so the default binding and a named one are built in one place: two copies
// would eventually disagree about what an empty ref means.
//
// The two structs are field-identical, so the conversion is the whole function:
// a binding *is* a declared source, with nothing computed on the way through —
// which is the same claim [ResolvedSource]'s doc comment makes. A field added to
// one and not the other stops compiling here rather than going quietly missing.
func resolvedSource(src Source) ResolvedSource { return ResolvedSource(src) }

// sourceNotInScope is the refusal of a component bound to a name nothing
// declares. It names both halves of the scope it searched, because the fix
// differs: declare it on the Project, or ask for a GitSource on the instance.
func sourceNotInScope(p *Project, index int, c Component, local, globals []string) Error {
	e := Error{
		Code:     ErrUnknownSource,
		Resource: fmt.Sprintf("%s/%s", KindProject, p.Metadata.Name),
		Field:    fmt.Sprintf("$.spec.components[%d].source", index),
		Message: fmt.Sprintf("component %q builds from source %q, which neither project %q nor this instance declares",
			c.Name, c.SourceName(), p.Metadata.Name),
		Remediation: fmt.Sprintf("declare it under spec.sources, or bind to one that is in scope. "+
			"This project declares: %s. This instance offers: %s (ADR-0035 decision 3)",
			nameList(local), nameList(globals)),
		DocsURL: docsURL(ErrUnknownSource),
	}
	return e
}

// nameList renders a scope for a remediation, saying "nothing" rather than
// printing an empty list — an author reading "in scope: " learns nothing.
func nameList(names []string) string {
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

// EffectivePolicy is the P4 chain for `policy:` alone: the Environment's block
// whole, else the Project's default block whole, else the built-in.
//
// It is exported and separate from [resolve] because kelson-server enforces
// agent policy against the *stored* spec on requests that never reach a full
// resolution — a spec store write, a secret write, a rollback — and enforcing a
// second copy of the precedence rule would be enforcing a different policy
// (ADR-0025 §2). Either argument may be nil, which is how a caller holding only
// one of the two documents asks.
//
// The built-in is `allow`, and an unset field inside a declared block means the
// same thing. Every restriction here is opt-in: the credential an operator
// issued is the grant (ADR-0024 §3), and this narrows it where an environment
// says so. A default that narrowed silently would refuse work an operator had
// already deliberately authorised, in an environment whose spec says nothing
// about agents at all.
func EffectivePolicy(p *Project, e *Environment) Policy {
	var pol Policy
	if p != nil && p.Spec.Defaults != nil && p.Spec.Defaults.Policy != nil {
		pol = *p.Spec.Defaults.Policy
	}
	if e != nil && e.Spec.Policy != nil {
		pol = *e.Spec.Policy
	}
	if pol.Agents == "" {
		pol.Agents = AgentsAllow
	}
	return pol
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
	return ResolvedDataService{Name: c.Name, Kind: kind, Preset: preset, Auth: c.Auth}
}

// resolvePreviews fills in the two defaults kelson has an opinion about, so
// the rendered ResourceSetInputProvider always states its polling interval and
// its ceiling rather than inheriting flux-operator's (ADR-0017).
func resolvePreviews(p *Previews) *ResolvedPreviews {
	if p == nil {
		return nil
	}
	rp := &ResolvedPreviews{
		Provider:  p.Provider,
		Repo:      p.Repo,
		SecretRef: p.SecretRef,
		Interval:  p.Interval,
		Artifacts: p.Artifacts,
		Filter:    ResolvedPreviewFilter{Limit: PreviewDefaultLimit},
	}
	if rp.Interval == "" {
		rp.Interval = PreviewDefaultInterval
	}
	if f := p.Filter; f != nil {
		rp.Filter.Labels = f.Labels
		rp.Filter.IncludeBranch = f.IncludeBranch
		rp.Filter.ExcludeBranch = f.ExcludeBranch
		if f.Limit != nil {
			rp.Filter.Limit = *f.Limit
		}
	}
	if s := p.Skip; s != nil {
		rp.Skip = s.Labels
	}
	return rp
}

// resolveChart carries a helm component through unchanged. It takes no
// override argument because there is none to take: an Environment override on a
// helm component is a validation error, not a merge (ADR-0016).
func resolveChart(c Component) ResolvedChart {
	rc := ResolvedChart{
		Name:       c.Name,
		Chart:      c.Chart,
		Version:    c.ChartVersion,
		Values:     c.Values,
		ValuesFrom: c.ValuesFrom,
	}
	if chart := c.ChartSourceOf(); chart != nil {
		rc.Source = *chart
	}
	return rc
}

// resolveRelease fills the one default a release hook has. The duration string
// is turned into seconds here, where `time` is available, rather than in the
// renderer, where it deliberately is not (issue #20). A duration that does not
// parse cannot reach this function: validation rejects it, and resolution only
// runs on a document that validated.
func resolveRelease(r *Release) *ResolvedRelease {
	if r == nil {
		return nil
	}
	timeout := DefaultReleaseTimeout
	if d, err := time.ParseDuration(r.Timeout); err == nil && d > 0 {
		timeout = d
	}
	return &ResolvedRelease{
		Command:        r.Command,
		TimeoutSeconds: int(timeout.Round(time.Second) / time.Second),
	}
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
	// P3, innermost first: the Environment's pin, the component's image, the
	// Project's. The pin is the promotion primitive (ADR-0016) and sits at the
	// top of that chain on purpose — `--image` stands in for the Project's
	// image, so a pinned environment stays where it was pinned until someone
	// promotes it.
	rc.Command = c.Command
	rc.Image = ov.Image
	if rc.Image == "" {
		rc.Image = c.Image
	}
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
	rc.Release = resolveRelease(c.Release)

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
