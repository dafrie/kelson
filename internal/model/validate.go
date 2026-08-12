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

func (v *validator) envMap(field string, env map[string]EnvValue, services map[string]Service) {
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

func (v *validator) serviceRef(field string, b *ServiceBinding, services map[string]Service) {
	svc, ok := services[b.Service]
	if !ok {
		names := make([]string, 0, len(services))
		for n := range services {
			names = append(names, n)
		}
		v.err(ErrUnknownService, field+".from.service",
			fmt.Sprintf("service %q is not declared in the Project", b.Service),
			fmt.Sprintf("declare it under spec.services, or use one of: %s", strings.Join(names, ", ")))
		return
	}
	if b.Key == "" {
		v.err(ErrMissingRequired, field+".from.key",
			"binding key is required",
			"valid keys for "+svc.Type+": "+strings.Join(ServiceKeys[svc.Type], ", "))
		return
	}
	if !slices.Contains(ServiceKeys[svc.Type], b.Key) {
		v.err(ErrUnknownServiceKey, field+".from.key",
			fmt.Sprintf("key %q does not exist for service type %q", b.Key, svc.Type),
			"valid keys for "+svc.Type+": "+strings.Join(ServiceKeys[svc.Type], ", "))
	}
}

// secretLiteral rejects values that look like credentials (ADR-0009): URLs
// embedding passwords, and literals for secret-shaped variable names.
func (v *validator) secretLiteral(field, name, literal string) {
	if literal == "" {
		return
	}
	if u, err := url.Parse(literal); err == nil && u != nil && u.User != nil && u.Scheme != "" && u.Host != "" {
		if _, hasPassword := u.User.Password(); hasPassword {
			v.err(ErrSecretLiteral, field,
				fmt.Sprintf("%q contains a credential (URL with embedded password)", name),
				// kelson has no command to write secret values yet (M8 · Secrets,
				// ADR-0009) — a declared service is the only way to keep a
				// credential-bearing value out of the spec today (issue #142).
				fmt.Sprintf("declare a service and reference it, e.g. %s: {from: {service: <name>, key: uri}}", name))
			return
		}
	}
	if secretNameRE.MatchString(name) {
		v.err(ErrSecretLiteral, field,
			fmt.Sprintf("%q looks like a secret but is a plaintext literal", name),
			"the spec carries references, never values (ADR-0009); kelson does not yet have a command to set secret "+
				"values (tracked by the M8 · Secrets milestone) — until then, remove this variable or bind it to a "+
				"declared service with {from: {service: <name>, key: <key>}}")
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

func (v *validator) applications(field string, apps []Application, services map[string]Service, projectImage string) {
	seen := map[string]int{}
	for i, a := range apps {
		f := fmt.Sprintf("%s[%d]", field, i)
		v.name(f+".name", a.Name, "application")
		if prev, dup := seen[a.Name]; dup {
			v.err(ErrDuplicateName, f+".name",
				fmt.Sprintf("duplicate application name %q (first at applications[%d])", a.Name, prev),
				"application names must be unique within the Project")
		}
		seen[a.Name] = i

		if a.Port != 0 && (a.Port < 1 || a.Port > 65535) {
			v.err(ErrOutOfRange, f+".port",
				fmt.Sprintf("port must be 1-65535, got %d", a.Port),
				"set a valid TCP port, or omit port for a worker")
		}
		if a.Health != "" && !strings.HasPrefix(a.Health, "/") {
			v.err(ErrInvalidFormat, f+".health",
				fmt.Sprintf("health path %q must start with /", a.Health),
				"use a URL path such as /healthz")
		}
		for j, d := range a.Domains {
			v.domain(fmt.Sprintf("%s.domains[%d]", f, j), d)
		}
		if a.Schedule != "" {
			v.cron(f+".schedule", a.Schedule)
			switch {
			case a.Port != 0:
				v.err(ErrMutuallyExclusive, f,
					fmt.Sprintf("application %q sets both schedule and port", a.Name),
					"split it into two applications: a cron application with schedule, and a service with port")
			case len(a.Domains) > 0 || a.Health != "":
				v.err(ErrMutuallyExclusive, f,
					fmt.Sprintf("cron application %q sets domains or health", a.Name),
					"cron jobs are not routed or health-checked; remove domains/health")
			}
		}
		v.replicas(f+".replicas", a.Replicas)
		v.resources(f+".resources", a.Resources)
		v.envMap(f+".env", a.Env, services)

		hasImage := a.Image != "" || projectImage != ""
		if !hasImage {
			v.err(ErrNoImageSource, f,
				fmt.Sprintf("application %q has no image source", a.Name),
				"set image on the application or the Project, or configure spec.source + spec.build with a strategy other than none")
		}
	}
}

func validateProject(p *Project, v *validator) {
	v.name("$.metadata.name", p.Metadata.Name, "project")

	s := &p.Spec
	if len(s.Applications) == 0 {
		v.err(ErrMissingRequired, "$.spec.applications",
			"a Project declares at least one application",
			"add spec.applications with at least one entry; a cron or worker counts")
	}

	services := map[string]Service{}
	for i, svc := range s.Services {
		f := fmt.Sprintf("$.spec.services[%d]", i)
		v.name(f+".name", svc.Name, "service")
		switch svc.Type {
		case "postgres", "valkey":
		case "":
			v.err(ErrMissingRequired, f+".type",
				fmt.Sprintf("service %q has no type", svc.Name),
				"valid types: postgres, valkey — new engines land via ADR, not ad hoc strings")
		default:
			v.err(ErrInvalidEnum, f+".type",
				fmt.Sprintf("unknown service type %q", svc.Type),
				"valid types: postgres, valkey")
		}
		switch svc.Plan {
		case "", PlanShared, PlanSmall, PlanHASmall, PlanHAMedium, PlanBranch:
		default:
			v.err(ErrInvalidEnum, f+".plan",
				fmt.Sprintf("unknown plan %q", svc.Plan),
				"valid plans: shared, small, ha-small, ha-medium, branch (docs/architecture.md, ADR-0007)")
		}
		if _, dup := services[svc.Name]; dup {
			v.err(ErrDuplicateName, f+".name",
				fmt.Sprintf("duplicate service name %q", svc.Name),
				"service names must be unique within the Project")
		}
		services[svc.Name] = svc
	}

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
	v.applications("$.spec.applications", s.Applications, services, projectImage)

	if d := s.Defaults; d != nil {
		switch d.DeliveryMode {
		case "", DeliveryDirect, DeliveryFlux, DeliveryArgoCD:
		default:
			v.err(ErrInvalidEnum, "$.spec.defaults.deliveryMode",
				fmt.Sprintf("unknown delivery mode %q", d.DeliveryMode),
				"valid modes: direct, flux, argocd")
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

	if r := s.Routing; r != nil {
		if r.DomainSuffix != "" {
			v.domain("$.spec.routing.domainSuffix", r.DomainSuffix)
		}
		if r.IngressClass != "" && r.GatewayClass != "" {
			v.err(ErrMutuallyExclusive, "$.spec.routing",
				"ingressClass and gatewayClass are mutually exclusive",
				"set the one matching your cluster; the ClusterProfile lists available classes")
		}
	}

	v.delivery("$.spec.delivery", s.Delivery)
	v.policy("$.spec.policy", s.Policy)
	v.secrets("$.spec.secrets", s.Secrets)

	seen := map[string]int{}
	for i, ov := range s.Applications {
		f := fmt.Sprintf("$.spec.applications[%d]", i)
		v.name(f+".name", ov.Name, "application")
		if prev, dup := seen[ov.Name]; dup {
			v.err(ErrDuplicateName, f+".name",
				fmt.Sprintf("duplicate override for application %q (first at applications[%d])", ov.Name, prev),
				"one override block per application")
		}
		seen[ov.Name] = i
		v.replicas(f+".replicas", ov.Replicas)
		v.resources(f+".resources", ov.Resources)
		v.envMap(f+".env", ov.Env, nil) // binding targets re-checked against the Project in ValidateEnvironment
	}

	seenSvc := map[string]int{}
	for i, ov := range s.Services {
		f := fmt.Sprintf("$.spec.services[%d]", i)
		v.name(f+".name", ov.Name, "service")
		if prev, dup := seenSvc[ov.Name]; dup {
			v.err(ErrDuplicateName, f+".name",
				fmt.Sprintf("duplicate override for service %q (first at services[%d])", ov.Name, prev),
				"one override block per service")
		}
		seenSvc[ov.Name] = i
		switch ov.Plan {
		case PlanShared, PlanSmall, PlanHASmall, PlanHAMedium, PlanBranch:
		default:
			v.err(ErrInvalidEnum, f+".plan",
				fmt.Sprintf("unknown plan %q", ov.Plan),
				"valid plans: shared, small, ha-small, ha-medium, branch")
		}
	}

	for i, o := range s.Overlays {
		v.overlay(fmt.Sprintf("$.spec.overlays[%d]", i), o)
	}
}

// validateServiceRefs re-checks an environment's binding targets against the
// Project's declared services.
func validateServiceRefs(e *Environment, services map[string]Service, v *validator) {
	for i, ov := range e.Spec.Applications {
		v.envMap(fmt.Sprintf("$.spec.applications[%d].env", i), ov.Env, services)
	}
}

// ValidateEnvironment validates an Environment against its Project: shape,
// plus every cross-document reference (application overrides, service
// overrides, binding targets). The Environment's spec.project must equal the
// Project's name.
func ValidateEnvironment(e *Environment, p *Project) Errors {
	v := validator{resource: fmt.Sprintf("%s/%s", KindEnvironment, e.Metadata.Name)}
	validateEnvironmentShape(e, &v)

	if p == nil {
		return v.errs
	}
	if e.Spec.Project != "" && e.Spec.Project != p.Metadata.Name {
		v.err(ErrUnknownApplication, "$.spec.project",
			fmt.Sprintf("environment targets project %q but was validated against project %q", e.Spec.Project, p.Metadata.Name),
			fmt.Sprintf("set spec.project to %q, or validate against the %q project", p.Metadata.Name, e.Spec.Project))
	}

	services := map[string]Service{}
	for _, svc := range p.Spec.Services {
		services[svc.Name] = svc
	}
	apps := map[string]bool{}
	for _, a := range p.Spec.Applications {
		apps[a.Name] = true
	}

	for i, ov := range e.Spec.Applications {
		if ov.Name != "" && !apps[ov.Name] {
			v.err(ErrUnknownApplication, fmt.Sprintf("$.spec.applications[%d].name", i),
				fmt.Sprintf("no application %q in project %q", ov.Name, p.Metadata.Name),
				"override an application the project declares, or add it to the project")
		}
	}
	for i, ov := range e.Spec.Services {
		if ov.Name != "" {
			if _, ok := services[ov.Name]; !ok {
				v.err(ErrUnknownService, fmt.Sprintf("$.spec.services[%d].name", i),
					fmt.Sprintf("no service %q in project %q", ov.Name, p.Metadata.Name),
					"override a service the project declares, or add it to the project")
			}
		}
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
	vp := validator{resource: fmt.Sprintf("%s/%s", KindProject, p.Metadata.Name)}
	validateProject(p, &vp)
	errs := vp.errs
	for _, e := range envs {
		errs = append(errs, ValidateEnvironment(e, p)...)
	}
	return errs
}
