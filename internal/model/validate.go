package model

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// validator accumulates structured errors against one resource document.
type validator struct {
	resource string // e.g. "Project/checkout"
	kind     string // KindProject or KindEnvironment; selects the #141 gate rows
	pos      positions
	errs     Errors
}

func (v *validator) err(code Code, field, msg, remediation string) {
	e := Error{
		Code:        code,
		Resource:    v.resource,
		Field:       field,
		Message:     msg,
		Remediation: remediation,
		DocsURL:     docsURL(code),
	}
	if v.pos != nil {
		p := v.pos.at(field)
		e.Line, e.Column = p.Line, p.Column
	}
	v.errs = append(v.errs, e)
}

var (
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsDomainRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	cpuRE        = regexp.MustCompile(`^(\d+|\d*\.\d+)m?$`)
	memoryRE     = regexp.MustCompile(`^\d+(Ei|Pi|Ti|Gi|Mi|Ki|E|P|T|G|M|K)?$`)
	cronFieldRE  = regexp.MustCompile(`^[\d*,\-/]+$|^[A-Za-z]{3}$`)
	cronNameRE   = regexp.MustCompile(`^[A-Za-z]{3}$`)
	secretNameRE = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API[_-]?KEY|PRIVATE[_-]?KEY|CREDENTIAL|_AUTH)`)
	envVarNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (v *validator) name(field, s, what string) {
	if s == "" {
		v.err(ErrMissingRequired, field, what+" name is required",
			"set "+field+" to a DNS-1123 label, e.g. my-"+what)
		return
	}
	if len(s) > 63 || !dnsLabelRE.MatchString(s) {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not a DNS-1123 label", s),
			"use lowercase letters, digits and dashes, e.g. "+strings.ToLower(s)+"-1")
	}
}

func (v *validator) domain(field, s string) {
	if len(s) > 253 || !dnsDomainRE.MatchString(s) {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not a well-formed domain", s),
			"use a DNS name such as checkout.acme.com")
	}
}

// envMap validates one env map: variable names, secret literals (ADR-0009) and
// service bindings. Passing services nil checks a binding's shape only, for the
// Environment pass whose targets are re-checked against the Project later.
func (v *validator) envMap(field string, env map[string]EnvValue, services map[string]Component) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		f := field + "." + k
		ev := env[k]
		if !envVarNameRE.MatchString(k) {
			v.err(ErrInvalidFormat, f,
				fmt.Sprintf("%q is not a valid environment variable name", k),
				"use letters, digits and underscores, not starting with a digit")
		}
		if ev.From != nil {
			if services != nil {
				v.serviceRef(f, ev.From, services)
			} else if ev.From.Service == "" || ev.From.Key == "" {
				v.err(ErrMissingRequired, f+".from",
					"a service binding needs both service and key",
					"set from: {service: <service name>, key: <well-known key>}")
			}
			continue
		}
		v.secretLiteral(f, k, ev.Literal)
	}
}

// serviceRef checks a binding against the Project's data components. `services`
// holds only those: binding to a worker is as wrong as binding to a name
// nothing declares, and the remediation says which names are bindable.
func (v *validator) serviceRef(field string, b *ServiceBinding, services map[string]Component) {
	svc, ok := services[b.Service]
	if !ok {
		names := make([]string, 0, len(services))
		for n := range services {
			names = append(names, n)
		}
		slices.Sort(names)
		v.err(ErrUnknownService, field+".from.service",
			fmt.Sprintf("service %q is not a data component of the Project", b.Service),
			fmt.Sprintf("declare it under spec.components with kind: postgres or kind: valkey, or bind to one of: %s",
				strings.Join(names, ", ")))
		return
	}
	kind := svc.EffectiveKind()
	if b.Key == "" {
		v.err(ErrMissingRequired, field+".from.key",
			"binding key is required",
			"valid keys for "+string(kind)+": "+strings.Join(ServiceKeys[kind], ", "))
		return
	}
	if !slices.Contains(ServiceKeys[kind], b.Key) {
		v.err(ErrUnknownServiceKey, field+".from.key",
			fmt.Sprintf("key %q does not exist for component kind %q", b.Key, kind),
			"valid keys for "+string(kind)+": "+strings.Join(ServiceKeys[kind], ", "))
	}
}

// secretLiteral rejects values that look like credentials (ADR-0009): URLs
// embedding passwords, and literals for secret-shaped variable names.
//
// The remediation names only what kelson can actually do today. It used to
// send authors to a `kelson secret set` that does not exist (issue #142), and
// then — while issue #141 gated bindings — to an overlay only. A binding to a
// managed service now renders end to end (issue #89), so it is named first: it
// is the answer for the credential this check catches most often, a database
// URL. Everything else is still an overlay against a Secret the user manages.
const secretRemediation = "the spec carries references, never values (ADR-0009). For a managed service, declare it " +
	"under spec.components with kind: postgres and bind: {from: {service: <name>, key: uri}} — kelson renders a " +
	"secretKeyRef against the " +
	"credentials the operator generates. For anything else kelson cannot hold the value yet (there is no command to " +
	"set one, milestone M8 · Secrets): remove this variable and inject it with an overlay patch (spec.overlays) that " +
	"references a Secret you manage."

func (v *validator) secretLiteral(field, name, literal string) {
	if literal == "" {
		return
	}
	if u, err := url.Parse(literal); err == nil && u != nil && u.User != nil && u.Scheme != "" && u.Host != "" {
		if _, hasPassword := u.User.Password(); hasPassword {
			v.err(ErrSecretLiteral, field,
				fmt.Sprintf("%q contains a credential (URL with embedded password)", name),
				secretRemediation)
			return
		}
	}
	if secretNameRE.MatchString(name) {
		v.err(ErrSecretLiteral, field,
			fmt.Sprintf("%q looks like a secret but is a plaintext literal", name),
			secretRemediation)
	}
}

func (v *validator) replicas(field string, r *Replicas) {
	if r == nil {
		return
	}
	if r.Min < 0 {
		v.err(ErrOutOfRange, field+".min",
			fmt.Sprintf("replicas.min must be >= 0, got %d", r.Min),
			"set min to 0 (scale-to-zero) or more")
	}
	if r.Max < 0 {
		v.err(ErrOutOfRange, field+".max",
			fmt.Sprintf("replicas.max must be >= 0, got %d", r.Max),
			"omit max for a fixed replica count of min")
	}
	if r.Max != 0 && r.Max < r.Min {
		v.err(ErrOutOfRange, field+".max",
			fmt.Sprintf("replicas.max (%d) must be >= replicas.min (%d)", r.Max, r.Min),
			"raise max, or omit it for fixed scaling at min")
	}
}

func (v *validator) resources(field string, r *Resources) {
	if r == nil {
		return
	}
	check := func(f string, rl *ResourceList) {
		if rl == nil {
			return
		}
		if rl.CPU != "" && !cpuRE.MatchString(rl.CPU) {
			v.err(ErrInvalidFormat, f+".cpu",
				fmt.Sprintf("%q is not a valid CPU quantity", rl.CPU),
				"use millicores (500m) or cores (2)")
		}
		if rl.Memory != "" && !memoryRE.MatchString(rl.Memory) {
			v.err(ErrInvalidFormat, f+".memory",
				fmt.Sprintf("%q is not a valid memory quantity", rl.Memory),
				"use a Kubernetes quantity such as 512Mi or 2Gi")
		}
	}
	check(field+".requests", r.Requests)
	check(field+".limits", r.Limits)
}

// cronField names one of the five cron positions and its inclusive numeric
// bounds. Kubernetes CronJob parses the schedule with robfig/cron's standard
// 5-field parser, so these mirror that parser's bounds rather than inventing
// our own (issue #143) — day-of-week keeps both 0 and 7 as Sunday, matching
// crontab(5) rather than robfig's stricter 0-6.
type cronField struct {
	name     string
	min, max int
}

var cronFields = [5]cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7},
}

// cron validates a five-field cron expression field by field: shape first
// (cronFieldRE), then numeric bounds. A shape-valid field like "99" used to
// reach the CronJob unchecked and fail only when applied to the cluster
// (issue #143), instead of at validation time where the field path and a fix
// are available.
func (v *validator) cron(field, s string) {
	fields := strings.Fields(s)
	if len(fields) != 5 {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("schedule %q must have 5 fields (minute hour day-of-month month day-of-week), got %d", s, len(fields)),
			"use a five-field cron expression, e.g. \"0 3 * * *\"")
		return
	}
	for i, f := range fields {
		if !cronFieldRE.MatchString(f) {
			v.err(ErrInvalidFormat, field,
				fmt.Sprintf("cron field %d %q is not valid", i+1, f),
				"use numbers, ranges, steps and *, e.g. \"*/15 2-4 * * 1-5\"; month/day names (JAN, MON) are allowed")
			continue
		}
		if cronNameRE.MatchString(f) {
			continue // a bare 3-letter name (JAN, MON, ...) has no numeric bound to check
		}
		spec := cronFields[i]
		if reason := cronRangeError(f, spec); reason != "" {
			v.err(ErrOutOfRange, field,
				fmt.Sprintf("cron field %d (%s) %q %s", i+1, spec.name, f, reason),
				fmt.Sprintf("%s must be between %d and %d", spec.name, spec.min, spec.max))
		}
	}
}

// cronRangeError checks one already shape-valid cron field against its
// numeric bounds. It covers every form cronFieldRE accepts: *, a bare number,
// a range (a-b), a step (base/n, including */n), and a comma-separated list
// of any of those. Returns "" when every item is in bounds.
func cronRangeError(field string, spec cronField) string {
	for _, item := range strings.Split(field, ",") {
		if reason := cronItemRangeError(item, spec); reason != "" {
			return reason
		}
	}
	return ""
}

// cronItemRangeError checks one list item (no commas) of a cron field.
func cronItemRangeError(item string, spec cronField) string {
	base, step, hasStep := strings.Cut(item, "/")
	if hasStep {
		n, err := strconv.Atoi(step)
		if err != nil || n <= 0 {
			return fmt.Sprintf("has step %q, which must be a positive integer", step)
		}
	}
	if base == "*" {
		return ""
	}
	if lo, hi, isRange := strings.Cut(base, "-"); isRange {
		start, errStart := strconv.Atoi(lo)
		end, errEnd := strconv.Atoi(hi)
		if errStart != nil || errEnd != nil {
			return fmt.Sprintf("has a malformed range %q", base)
		}
		if start < spec.min || start > spec.max {
			return fmt.Sprintf("has range start %d outside %d-%d", start, spec.min, spec.max)
		}
		if end < spec.min || end > spec.max {
			return fmt.Sprintf("has range end %d outside %d-%d", end, spec.min, spec.max)
		}
		if start > end {
			return fmt.Sprintf("has range %d-%d with start after end", start, end)
		}
		return ""
	}
	n, err := strconv.Atoi(base)
	if err != nil {
		return fmt.Sprintf("has a malformed value %q", base)
	}
	if n < spec.min || n > spec.max {
		return fmt.Sprintf("value %d is outside %d-%d", n, spec.min, spec.max)
	}
	return ""
}

func (v *validator) overlay(field string, o Overlay) {
	if (o.Patch == "") == (o.Manifest == "") {
		v.err(ErrMutuallyExclusive, field,
			"an overlay must set exactly one of patch or manifest",
			"set patch for a merge against rendered resources, or manifest for an extra raw manifest")
	}
	for _, p := range []string{o.Patch, o.Manifest} {
		if p == "" {
			continue
		}
		if strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
			v.err(ErrInvalidFormat, field,
				fmt.Sprintf("overlay path %q must be relative to the spec file and stay within its directory", p),
				"use a relative path without .. segments, e.g. ./k8s/patches/affinity.yaml")
		}
	}
}

func (v *validator) policy(field string, p *Policy) {
	if p == nil {
		return
	}
	switch p.Agents {
	case "", AgentsAllow, AgentsProposeOnly:
	default:
		v.err(ErrInvalidEnum, field+".agents",
			fmt.Sprintf("unknown agents policy %q", p.Agents),
			fmt.Sprintf("valid values: %s, %s", AgentsAllow, AgentsProposeOnly))
	}
	for i, req := range p.Require {
		if req != "dry-run" {
			v.err(ErrInvalidEnum, fmt.Sprintf("%s.require[%d]", field, i),
				fmt.Sprintf("unknown requirement %q", req),
				"only dry-run is defined today; add new requirements via the schema, not ad hoc strings")
		}
	}
}

func (v *validator) secrets(field string, s *SecretBackend) {
	if s == nil {
		return
	}
	switch s.Backend {
	case SecretsCluster, SecretsSOPS:
	case SecretsExternalSecrets:
		if s.Store == "" {
			v.err(ErrMissingRequired, field+".store",
				"backend externalSecrets requires store",
				"set store to a ClusterSecretStore name from the ClusterProfile, e.g. vault-backend")
		}
	case "":
		v.err(ErrMissingRequired, field+".backend",
			"secrets.backend is required when secrets is set",
			"valid backends: cluster, externalSecrets, sops")
	default:
		v.err(ErrInvalidEnum, field+".backend",
			fmt.Sprintf("unknown secret backend %q", s.Backend),
			"valid backends: cluster, externalSecrets, sops")
	}
	if s.Store != "" && s.Backend != "" && s.Backend != SecretsExternalSecrets {
		v.err(ErrMutuallyExclusive, field+".store",
			fmt.Sprintf("store is only meaningful with backend externalSecrets, not %q", s.Backend),
			"remove store, or set backend to externalSecrets")
	}
}

func (v *validator) delivery(field string, d *Delivery) {
	if d == nil {
		return
	}
	switch d.Mode {
	case DeliveryDirect:
		if d.Git != nil {
			v.err(ErrMutuallyExclusive, field+".git",
				"delivery.git is meaningless with mode direct",
				"remove git, or change mode to flux or argocd")
		}
	case DeliveryFlux, DeliveryArgoCD:
		if d.Git == nil || d.Git.Repo == "" {
			v.err(ErrGitTargetMissing, field+".git",
				fmt.Sprintf("delivery mode %q requires a git target", d.Mode),
				"set delivery.git.repo (and optionally branch, path) to the deployment repository")
		}
	case "":
		// Mode inherited from a Project default (P4); the git requirement is
		// checked on the effective mode in ValidateEnvironment.
	default:
		v.err(ErrInvalidEnum, field+".mode",
			fmt.Sprintf("unknown delivery mode %q", d.Mode),
			"valid modes: direct, flux, argocd")
	}
}

// components validates the one leaf list of ADR-0014. The kind decides which
// half of the rules a component is held to; a field belonging to the other
// half is an error, never a no-op (issue #141).
func (v *validator) components(field string, comps []Component, services map[string]Component, projectImage string) {
	seen := map[string]int{}
	for i, c := range comps {
		f := fmt.Sprintf("%s[%d]", field, i)
		v.name(f+".name", c.Name, "component")
		if prev, dup := seen[c.Name]; dup {
			v.err(ErrDuplicateName, f+".name",
				fmt.Sprintf("duplicate component name %q (first at components[%d])", c.Name, prev),
				"component names must be unique within the Project; they name a workload or a database in one namespace")
		}
		seen[c.Name] = i

		kind, ok := v.componentKind(f, c)
		if !ok {
			continue // nothing below is meaningful against a kind we cannot name
		}
		if kind.IsData() {
			v.dataComponent(f, c, kind)
			continue
		}
		v.workloadComponent(f, c, kind, services, projectImage)
	}
}

// componentKind resolves a component's kind and reports the ways a written
// `kind:` can be wrong: not a member of the enum, or contradicting the shape
// the component actually has. An explicit kind is allowed to *state* what a
// component is; it is not allowed to change it silently.
func (v *validator) componentKind(field string, c Component) (ComponentKind, bool) {
	if c.Kind == "" {
		return c.DerivedKind(), true
	}
	if !c.Kind.Valid() {
		v.err(ErrInvalidEnum, field+".kind",
			fmt.Sprintf("unknown component kind %q", c.Kind),
			"valid kinds: "+strings.Join(kindNames(), ", ")+" — new kinds land via ADR, not ad hoc strings")
		return "", false
	}
	if c.Kind.IsData() {
		return c.Kind, true
	}
	if c.Port != 0 && c.Schedule != "" {
		// The shape contradicts itself; workloadComponent reports that, and a
		// second complaint about the kind would only obscure it.
		return c.Kind, true
	}
	derived := c.DerivedKind()
	want := c.Kind
	if c.Kind == ComponentAgent {
		// An agent is worker-shaped: it has an image and no inbound traffic.
		want = ComponentWorker
	}
	if derived != want {
		v.err(ErrMutuallyExclusive, field+".kind",
			fmt.Sprintf("component %q declares kind %q but its shape is %q", c.Name, c.Kind, derived),
			kindShapeRemediation(c.Kind))
		return "", false
	}
	return c.Kind, true
}

// kindShapeRemediation names the field that would make a written kind true.
func kindShapeRemediation(kind ComponentKind) string {
	switch kind {
	case ComponentService:
		return "a service is the component that serves traffic: set port, or drop kind: and let the shape derive it"
	case ComponentCron:
		return "a cron component is defined by its schedule: set schedule, or drop kind:"
	case ComponentWorker:
		return "a worker has neither port nor schedule: remove them, or drop kind: and let the shape derive it"
	case ComponentAgent:
		return "an agent is worker-shaped: remove port and schedule. An agent that also serves traffic is two components"
	}
	return "drop kind: and let the shape derive it"
}

func kindNames() []string {
	out := make([]string, 0, len(ComponentKinds))
	for _, k := range ComponentKinds {
		out = append(out, string(k))
	}
	return out
}

// dataComponent validates a postgres/valkey component: its preset, and the
// absence of everything that only means something for a workload.
func (v *validator) dataComponent(field string, c Component, kind ComponentKind) {
	switch c.Preset {
	case "", PresetShared, PresetSmall, PresetHASmall, PresetHAMedium, PresetBranch:
	default:
		v.err(ErrInvalidEnum, field+".preset",
			fmt.Sprintf("unknown preset %q", c.Preset),
			"valid presets: shared, small, ha-small, ha-medium, branch (docs/data-services.md, ADR-0007)")
	}
	for _, set := range workloadOnlyFields(c) {
		v.err(ErrMutuallyExclusive, field+"."+set,
			fmt.Sprintf("component %q has kind %q, which renders a managed data service, but sets %s", c.Name, kind, set),
			"remove "+set+"; a data component's whole configuration is its preset, because its topology belongs to the "+
				"operator (ADR-0005). Bind a workload to it with {from: {service: "+c.Name+", key: uri}}")
	}
}

// workloadOnlyFields lists the workload fields a component actually sets, in
// spec order, so a data component's errors name every offending key at once.
func workloadOnlyFields(c Component) []string {
	var out []string
	if c.Image != "" {
		out = append(out, "image")
	}
	if len(c.Command) > 0 {
		out = append(out, "command")
	}
	if c.Port != 0 {
		out = append(out, "port")
	}
	if c.Health != "" {
		out = append(out, "health")
	}
	if c.Schedule != "" {
		out = append(out, "schedule")
	}
	if len(c.Domains) > 0 {
		out = append(out, "domains")
	}
	if c.Replicas != nil {
		out = append(out, "replicas")
	}
	if c.Resources != nil {
		out = append(out, "resources")
	}
	if len(c.Env) > 0 {
		out = append(out, "env")
	}
	if len(c.Tools) > 0 {
		out = append(out, "tools")
	}
	return out
}

// workloadComponent validates a service/worker/cron/agent component.
func (v *validator) workloadComponent(
	field string,
	c Component,
	kind ComponentKind,
	services map[string]Component,
	projectImage string,
) {
	if c.Port != 0 && (c.Port < 1 || c.Port > 65535) {
		v.err(ErrOutOfRange, field+".port",
			fmt.Sprintf("port must be 1-65535, got %d", c.Port),
			"set a valid TCP port, or omit port for a worker")
	}
	if c.Health != "" && !strings.HasPrefix(c.Health, "/") {
		v.err(ErrInvalidFormat, field+".health",
			fmt.Sprintf("health path %q must start with /", c.Health),
			"use a URL path such as /healthz")
	}
	for j, d := range c.Domains {
		v.domain(fmt.Sprintf("%s.domains[%d]", field, j), d)
	}
	if c.Schedule != "" {
		v.cron(field+".schedule", c.Schedule)
		switch {
		case c.Port != 0:
			v.err(ErrMutuallyExclusive, field,
				fmt.Sprintf("component %q sets both schedule and port", c.Name),
				"split it into two components: a cron component with schedule, and a service with port")
		case len(c.Domains) > 0 || c.Health != "":
			v.err(ErrMutuallyExclusive, field,
				fmt.Sprintf("cron component %q sets domains or health", c.Name),
				"cron jobs are not routed or health-checked; remove domains/health")
		}
	}
	if c.Preset != "" {
		v.err(ErrMutuallyExclusive, field+".preset",
			fmt.Sprintf("component %q has kind %q, and a preset is the topology of a data component", c.Name, kind),
			"remove preset, or set kind: postgres if this was meant to be a database")
	}
	if len(c.Tools) > 0 {
		if kind != ComponentAgent {
			v.err(ErrMutuallyExclusive, field+".tools",
				fmt.Sprintf("component %q has kind %q, and tools is the capability policy of an agent", c.Name, kind),
				"remove tools, or set kind: agent — a tool allow-list on anything else attaches to nothing (ADR-0014)")
		} else {
			v.tools(field+".tools", c.Tools)
			v.gate("$.spec.components[].tools", field+".tools")
		}
	}
	v.replicas(field+".replicas", c.Replicas)
	v.resources(field+".resources", c.Resources)
	v.envMap(field+".env", c.Env, services)

	if c.Image == "" && projectImage == "" {
		v.err(ErrNoImageSource, field,
			fmt.Sprintf("component %q has no image source", c.Name),
			"set image on the component or the Project, or configure spec.source + spec.build with a strategy other than none")
	}
}

// tools validates an agent's tool allow-list. It runs even though the field is
// gated: the gate is about what kelson implements, not about what the author
// wrote, and an author fixing the shape of a list should not discover a second
// problem in it after #75 lands.
func (v *validator) tools(field string, tools []string) {
	seen := map[string]int{}
	for i, t := range tools {
		f := fmt.Sprintf("%s[%d]", field, i)
		if strings.TrimSpace(t) == "" {
			v.err(ErrMissingRequired, f,
				"a tool name is required",
				"name the tool the agent may call, e.g. search; remove the entry if it is a leftover")
			continue
		}
		if prev, dup := seen[t]; dup {
			v.err(ErrDuplicateName, f,
				fmt.Sprintf("duplicate tool %q (first at %s[%d])", t, field, prev),
				"a tool is allowed or it is not; listing it twice says nothing more")
			continue
		}
		seen[t] = i
	}
}

func validateProject(p *Project, v *validator) {
	v.name("$.metadata.name", p.Metadata.Name, "project")

	s := &p.Spec
	if len(s.Components) == 0 {
		v.err(ErrMissingRequired, "$.spec.components",
			"a Project declares at least one component",
			"add spec.components with at least one entry; a cron, a worker or a database counts")
	}

	// Bindable names come first: an env map anywhere in the document may
	// reference a data component declared further down the same list.
	services := dataComponents(s.Components)

	if s.Source != nil && s.Source.Git == "" {
		v.err(ErrMissingRequired, "$.spec.source.git",
			"source.git is required when source is set",
			"set source.git to the repository URL, or remove source and use pre-built images")
	}
	if s.Build != nil {
		switch s.Build.Strategy {
		case "", BuildAuto, BuildDockerfile, BuildBuildpacks, BuildNone:
		default:
			v.err(ErrInvalidEnum, "$.spec.build.strategy",
				fmt.Sprintf("unknown build strategy %q", s.Build.Strategy),
				"valid strategies: auto, dockerfile, buildpacks, none")
		}
	}
	projectImage := s.Image
	if s.Source != nil && s.Build != nil && s.Build.Strategy != BuildNone {
		projectImage = "(built from source)"
	}

	v.envMap("$.spec.env", s.Env, services)
	v.components("$.spec.components", s.Components, services, projectImage)

	if d := s.Defaults; d != nil {
		switch d.DeliveryMode {
		case "", DeliveryDirect, DeliveryFlux, DeliveryArgoCD:
		default:
			v.err(ErrInvalidEnum, "$.spec.defaults.deliveryMode",
				fmt.Sprintf("unknown delivery mode %q", d.DeliveryMode),
				"valid modes: direct, flux, argocd")
		}
		if d.Policy != nil {
			v.gate("$.spec.defaults.policy", "$.spec.defaults.policy")
		}
		if d.Secrets != nil {
			v.gate("$.spec.defaults.secrets", "$.spec.defaults.secrets")
		}
		v.policy("$.spec.defaults.policy", d.Policy)
		v.secrets("$.spec.defaults.secrets", d.Secrets)
	}

	for i, o := range s.Overlays {
		v.overlay(fmt.Sprintf("$.spec.overlays[%d]", i), o)
	}
}

// validateEnvironmentShape checks an Environment in isolation: everything
// that does not need the Project. Cross-document checks run separately in
// ValidateEnvironment.
func validateEnvironmentShape(e *Environment, v *validator) {
	v.name("$.metadata.name", e.Metadata.Name, "environment")
	s := &e.Spec

	if s.Project == "" {
		v.err(ErrMissingRequired, "$.spec.project",
			"an Environment binds to a Project by name",
			"set spec.project to the Project's metadata.name")
	}
	if s.Namespace != "" {
		v.name("$.spec.namespace", s.Namespace, "namespace")
	}
	if s.Cluster != "" {
		v.gate("$.spec.cluster", "$.spec.cluster")
	}

	if r := s.Routing; r != nil {
		if r.DomainSuffix != "" {
			v.domain("$.spec.routing.domainSuffix", r.DomainSuffix)
		}
	}

	v.delivery("$.spec.delivery", s.Delivery)
	if s.Policy != nil {
		v.gate("$.spec.policy", "$.spec.policy")
	}
	if s.Secrets != nil {
		v.gate("$.spec.secrets", "$.spec.secrets")
	}
	v.policy("$.spec.policy", s.Policy)
	v.secrets("$.spec.secrets", s.Secrets)

	seen := map[string]int{}
	for i, ov := range s.Components {
		f := fmt.Sprintf("$.spec.components[%d]", i)
		v.name(f+".name", ov.Name, "component")
		if prev, dup := seen[ov.Name]; dup {
			v.err(ErrDuplicateName, f+".name",
				fmt.Sprintf("duplicate override for component %q (first at components[%d])", ov.Name, prev),
				"one override block per component")
		}
		seen[ov.Name] = i
		v.replicas(f+".replicas", ov.Replicas)
		v.resources(f+".resources", ov.Resources)
		v.envMap(f+".env", ov.Env, nil) // binding targets re-checked against the Project in ValidateEnvironment
		switch ov.Preset {
		case "", PresetShared, PresetSmall, PresetHASmall, PresetHAMedium, PresetBranch:
		default:
			v.err(ErrInvalidEnum, f+".preset",
				fmt.Sprintf("unknown preset %q", ov.Preset),
				"valid presets: shared, small, ha-small, ha-medium, branch")
		}
	}

	for i, o := range s.Overlays {
		v.overlay(fmt.Sprintf("$.spec.overlays[%d]", i), o)
	}
}

// validateServiceRefs re-checks an environment's binding targets against the
// Project's data components.
func validateServiceRefs(e *Environment, services map[string]Component, v *validator) {
	for i, ov := range e.Spec.Components {
		// No canonical path: this is a second pass over env maps the shape
		// check already walked, and the #141 gate fired there.
		v.envMap(fmt.Sprintf("$.spec.components[%d].env", i), ov.Env, services)
	}
}

// overrideShape holds an Environment override to the half of the model its
// target belongs to. Which half that is only becomes knowable once the Project
// is in hand, so this is a cross-document check even though it reads like a
// shape one.
func (v *validator) overrideShape(field string, ov ComponentOverride, kind ComponentKind) {
	if !kind.IsData() {
		if ov.Preset != "" {
			v.err(ErrMutuallyExclusive, field+".preset",
				fmt.Sprintf("component %q has kind %q, and a preset is the topology of a data component", ov.Name, kind),
				"remove preset; a workload is overridden with replicas, resources and env")
		}
		return
	}
	var set []string
	if ov.Replicas != nil {
		set = append(set, "replicas")
	}
	if ov.Resources != nil {
		set = append(set, "resources")
	}
	if len(ov.Env) > 0 {
		set = append(set, "env")
	}
	for _, f := range set {
		v.err(ErrMutuallyExclusive, field+"."+f,
			fmt.Sprintf("component %q has kind %q, which renders a managed data service, but the override sets %s", ov.Name, kind, f),
			"remove "+f+"; a data component is overridden per environment with preset only (rule P5)")
	}
	if ov.Preset == "" {
		v.err(ErrMissingRequired, field+".preset",
			fmt.Sprintf("the override for data component %q sets nothing", ov.Name),
			"set preset to the topology this environment wants (shared, small, ha-small, ha-medium, branch), or remove the block")
	}
}

// dataComponents indexes the components a binding may target, by name.
func dataComponents(comps []Component) map[string]Component {
	out := map[string]Component{}
	for _, c := range comps {
		if c.EffectiveKind().IsData() {
			out[c.Name] = c
		}
	}
	return out
}

// ValidateEnvironment validates an Environment against its Project: shape,
// plus every cross-document reference (application overrides, service
// overrides, binding targets). The Environment's spec.project must equal the
// Project's name.
func ValidateEnvironment(e *Environment, p *Project) Errors {
	v := validator{resource: fmt.Sprintf("%s/%s", KindEnvironment, e.Metadata.Name), kind: KindEnvironment}
	validateEnvironmentShape(e, &v)

	if p == nil {
		return v.errs
	}
	if e.Spec.Project != "" && e.Spec.Project != p.Metadata.Name {
		v.err(ErrUnknownComponent, "$.spec.project",
			fmt.Sprintf("environment targets project %q but was validated against project %q", e.Spec.Project, p.Metadata.Name),
			fmt.Sprintf("set spec.project to %q, or validate against the %q project", p.Metadata.Name, e.Spec.Project))
	}

	services := dataComponents(p.Spec.Components)
	kinds := map[string]ComponentKind{}
	for _, c := range p.Spec.Components {
		kinds[c.Name] = c.EffectiveKind()
	}

	for i, ov := range e.Spec.Components {
		if ov.Name == "" {
			continue
		}
		f := fmt.Sprintf("$.spec.components[%d]", i)
		kind, ok := kinds[ov.Name]
		if !ok {
			v.err(ErrUnknownComponent, f+".name",
				fmt.Sprintf("no component %q in project %q", ov.Name, p.Metadata.Name),
				"override a component the project declares, or add it to the project")
			continue
		}
		v.overrideShape(f, ov, kind)
	}
	validateServiceRefs(e, services, &v)

	// The git requirement applies to the *effective* delivery mode: an
	// Environment may inherit flux/argocd from a Project default (P4), and
	// then it must carry the git target itself.
	mode := DeliveryDirect
	if p.Spec.Defaults != nil && p.Spec.Defaults.DeliveryMode != "" {
		mode = p.Spec.Defaults.DeliveryMode
	}
	if d := e.Spec.Delivery; d != nil && d.Mode != "" {
		mode = d.Mode
	}
	if (mode == DeliveryFlux || mode == DeliveryArgoCD) &&
		(e.Spec.Delivery == nil || e.Spec.Delivery.Git == nil || e.Spec.Delivery.Git.Repo == "") {
		v.err(ErrGitTargetMissing, "$.spec.delivery.git",
			fmt.Sprintf("effective delivery mode is %q (environment or project default) but no git target is set", mode),
			"set delivery.git.repo (and optionally branch, path) on this environment")
	}
	return v.errs
}

// ValidateSet validates a Project and all its Environments as a set. This is
// the entry point the API, CLI and renderer share.
func ValidateSet(p *Project, envs ...*Environment) Errors {
	vp := validator{resource: fmt.Sprintf("%s/%s", KindProject, p.Metadata.Name), kind: KindProject}
	validateProject(p, &vp)
	errs := vp.errs
	for _, e := range envs {
		errs = append(errs, ValidateEnvironment(e, p)...)
	}
	return errs
}
