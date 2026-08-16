package model

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
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
	// secretKeyRE is Kubernetes' own alphabet for a key of a Secret's data
	// map. A reference kelson accepts must be one the kubelet could project.
	secretKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	// ageRecipientRE is an age X25519 public key: bech32 with the "age" human
	// readable part, which is `age1` plus 58 characters of the bech32
	// alphabet. The checksum is not verified here — validation may not import
	// filippo.io/age (the depguard `main` rule), and internal/sops rejects a
	// recipient that parses badly anyway. What this catches is the mistake
	// people actually make: pasting the AGE-SECRET-KEY half into the spec.
	ageRecipientRE = regexp.MustCompile(`^age1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]{58}$`)
)

// SecretShapedName reports whether a variable name is one people put
// credentials in. It is exported so the build plane can refuse the same names
// as build arguments (issue #117): a build arg ends up in image history and in
// the build log, so PASSWORD-as-a-build-arg is the same defect as
// PASSWORD-as-a-spec-literal and must not be caught by a second, drifting copy
// of this pattern.
//
// It is a heuristic and ADR-0009 says so in its own honesty note: a literal
// under a creatively named key passes. The structural replacement is #82.
func SecretShapedName(name string) bool {
	return secretNameRE.MatchString(name)
}

// ValidSecretName reports whether s can name a Kubernetes Secret: a DNS-1123
// label, which is what [validator.secretRef] holds `secret:` to.
//
// It is exported because the authoring path (internal/secret, issue #116) must
// refuse exactly the names a reference would refuse. A Secret kelson would let
// you create but not reference is a Secret nobody can use, and finding that out
// at render time rather than at `kelson secret set` time is the wrong order.
func ValidSecretName(s string) bool {
	return s != "" && len(s) <= 63 && dnsLabelRE.MatchString(s)
}

// ValidSecretKey reports whether s can be a key of a Secret's data map —
// Kubernetes' own alphabet (letters, digits, '-', '_', '.').
//
// Exported for the same reason as [ValidSecretName]: the writer and the
// reference must agree on the alphabet, and a second copy of the pattern would
// eventually drift from this one.
func ValidSecretKey(s string) bool {
	return secretKeyRE.MatchString(s)
}

// SecretKeyAlphabet describes [ValidSecretKey] in the form a remediation can
// use, so the writer and the reference explain the same rule in the same words.
const SecretKeyAlphabet = "letters, digits, '-', '_' and '.', which is the key alphabet Kubernetes enforces on Secret data"

// ValidAgeRecipient reports whether s is spelled like an age X25519 public
// key. It is exported so `kelson secret` can refuse a bad `--age-recipient`
// with the same rule the spec is held to.
func ValidAgeRecipient(s string) bool { return ageRecipientRE.MatchString(s) }

// AgeSecretKeyPrefix is how an age *identity* starts. It is named here so the
// one mistake that must never pass — pasting the private half into a spec or a
// flag — can be recognised and refused with a message that says what happened
// rather than "invalid format".
const AgeSecretKeyPrefix = "AGE-SECRET-KEY-"

// DefaultNamespace is the namespace an environment targets when its spec names
// none: `<project>-<environment>` (docs/model.md).
//
// The resolver applies it (resolve.go) and the secret-authoring path derives
// the same answer from a (project, environment) pair it was given without a
// spec (issue #116) — `kelson secret set --project p --env e` has no document
// to resolve. One function so the two cannot drift into writing Secrets into a
// namespace the render never targets.
func DefaultNamespace(project, environment string) string {
	return project + "-" + environment
}

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

// imageRef validates an image reference wherever the spec carries one: the
// Project's, a component's, and an Environment's per-component pin (rule P3).
// One function so the three fields cannot acquire different ideas of what a
// reference is — a pin written for a promotion is held to exactly what
// `spec.image` is held to.
//
// The check is deliberately shallow: it rejects what no registry could accept
// (blank, or embedded whitespace) and leaves tag/digest grammar to the
// registry, which is the only thing that can actually resolve it.
func (v *validator) imageRef(field, s string) {
	if s == "" {
		return // absence is precedence, not an error; no-image-source reports the real case
	}
	if strings.TrimSpace(s) == "" || strings.ContainsAny(s, " \t\n") {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not an image reference", s),
			"use a registry reference such as ghcr.io/acme/web:v1 or ghcr.io/acme/web@sha256:<digest>")
	}
}

func (v *validator) domain(field, s string) {
	if len(s) > 253 || !dnsDomainRE.MatchString(s) {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not a well-formed domain", s),
			"use a DNS name such as checkout.acme.com")
	}
}

// envMap validates one env map: variable names, secret literals (ADR-0009),
// secret references (ADR-0018) and service bindings. Passing services nil
// checks a binding's shape only, for the Environment pass whose targets are
// re-checked against the Project later.
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
		if ev.Secret != nil {
			v.secretRef(f, ev.Secret)
			continue
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

// secretRef checks a secret reference (ADR-0018). Both halves are required and
// each is held to what the mechanism it renders into can actually address: the
// name is a Secret's, so DNS-1123; the key is a Secret's data key, so
// Kubernetes' own key alphabet.
//
// Nothing here resolves anything. Whether the Secret exists, and what is in it,
// is the cluster's to know at apply time — kelson references it and never reads
// it, which is the whole point of the reference model.
func (v *validator) secretRef(field string, r *SecretRef) {
	if r.Name == "" {
		v.err(ErrMissingRequired, field+".secret",
			"a secret reference needs the name of a Secret",
			"set secret: <name of a Secret in the environment's namespace>, e.g. {secret: checkout-db, key: url}")
	} else {
		v.name(field+".secret", r.Name, "secret")
	}
	switch {
	case r.Key == "":
		v.err(ErrMissingRequired, field+".key",
			"a secret reference needs the key to read within that Secret",
			"set key: <key within the Secret>, e.g. {secret: "+refExample(r.Name)+", key: url}")
	case !ValidSecretKey(r.Key):
		v.err(ErrInvalidFormat, field+".key",
			fmt.Sprintf("%q is not a key a Kubernetes Secret can hold", r.Key),
			"use "+SecretKeyAlphabet)
	}
}

// refExample keeps a remediation's example concrete when the author already
// wrote half the reference.
func refExample(name string) string {
	if name == "" {
		return "<secret name>"
	}
	return name
}

// secretLiteral rejects values that look like credentials (ADR-0009): URLs
// embedding passwords, and literals for secret-shaped variable names.
//
// The remediation names only what kelson can actually do today. It used to
// send authors to a `kelson secret set` that does not exist (issue #142), and
// then — while issue #141 gated bindings — to an overlay only. Three things
// work end to end now: a secret reference (ADR-0018), which is the answer for
// every credential; the command that writes the Secret the reference names
// (issue #116); and a binding to a managed service (issue #89), which is the
// shorter path for the credential this check catches most often.
//
// `kelson secret set` leads the writing step and `kubectl` follows it as the
// alternative, because #116 landed the command ADR-0018 said the remediation
// would name once it existed. kubectl stays named rather than being dropped: an
// author reading this may be on a machine with kubectl and no kelson, and the
// two commands write the same object.
const secretRemediation = "the spec carries references, never values. Write the variable as a reference: " +
	"{secret: <secret name>, key: <key>}, which kelson renders as a valueFrom.secretKeyRef against a Secret in the " +
	"environment's namespace and never reads. Write that Secret with " +
	"`kelson secret set <secret name> --project <project> --env <environment> <key>=<value>` " +
	"(use --from-stdin <key> or --from-file <key>=<path> to keep the value out of your shell history), or with " +
	"`kubectl -n <namespace> create secret generic <secret name> --from-literal=<key>=…`. " +
	"For a managed service there is a shorter path: declare it under " +
	"spec.components with kind: postgres and bind {from: {service: <name>, key: uri}}, and kelson derives the " +
	"secretKeyRef from the credentials the operator generates. An overlay patch (spec.overlays) remains the escape " +
	"hatch for anything neither form expresses."

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
	if SecretShapedName(name) {
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

// policy checks the shape of a policy block: the enums, the ceiling and the
// names. Whether a protected component exists is a cross-document question and
// lives in [ValidateEnvironment] and [validateProject], where the Project's
// component list is in hand.
func (v *validator) policy(field string, p *Policy) {
	if p == nil {
		return
	}
	if len(p.Deployers) > 0 {
		v.gate(field+".deployers", field+".deployers")
	}
	switch p.Agents {
	case "", AgentsAllow, AgentsProposeOnly:
	default:
		v.err(ErrInvalidEnum, field+".agents",
			fmt.Sprintf("unknown agents policy %q", p.Agents),
			fmt.Sprintf("valid values: %s, %s", AgentsAllow, AgentsProposeOnly))
	}
	for i, req := range p.Require {
		if req != PolicyRequireDryRun {
			v.err(ErrInvalidEnum, fmt.Sprintf("%s.require[%d]", field, i),
				fmt.Sprintf("unknown requirement %q", req),
				"only dry-run is defined today; add new requirements via the schema, not ad hoc strings")
		}
	}
	// A ceiling of zero is refused rather than read as "no replicas": the one
	// spelling an author might reach for to mean "unlimited" would otherwise be
	// the strictest setting there is, and it would read as a ceiling while
	// acting as an off switch.
	if p.MaxReplicas != nil && *p.MaxReplicas < 1 {
		v.err(ErrOutOfRange, field+".maxReplicas",
			fmt.Sprintf("maxReplicas is %d; a ceiling below 1 forbids every deploy rather than capping one", *p.MaxReplicas),
			"set maxReplicas to the highest replica count an agent may deploy here, or remove it for no ceiling")
	}
	for i, name := range p.Protect {
		v.name(fmt.Sprintf("%s.protect[%d]", field, i), name, "component")
	}
	known := map[AgentOperation]bool{}
	for _, op := range AgentOperations() {
		known[op] = true
	}
	for i, op := range p.Forbid {
		if known[op] {
			continue
		}
		names := make([]string, 0, len(AgentOperations()))
		for _, valid := range AgentOperations() {
			names = append(names, string(valid))
		}
		v.err(ErrInvalidEnum, fmt.Sprintf("%s.forbid[%d]", field, i),
			fmt.Sprintf("unknown operation %q", op),
			"valid operations: "+strings.Join(names, ", "))
	}
}

// protectedComponents is the cross-document half of `policy.protect`: a
// protected name must be a component the Project declares. A typo there would
// protect nothing and say nothing, which is the silence issue #141 exists to
// prevent — and it would do it on the one field whose whole job is to stop an
// agent removing something.
func (v *validator) protectedComponents(field string, p *Policy, declared map[string]ComponentKind, project string) {
	if p == nil {
		return
	}
	for i, name := range p.Protect {
		if name == "" {
			continue
		}
		if _, ok := declared[name]; ok {
			continue
		}
		v.err(ErrUnknownComponent, fmt.Sprintf("%s.protect[%d]", field, i),
			fmt.Sprintf("policy protects component %q, which project %q does not declare", name, project),
			"protect a component the project declares, or remove the entry — a protected name that matches nothing protects nothing")
	}
}

// secrets validates the Environment-scoped backend selector (ADR-0009,
// ADR-0018, ADR-0020).
//
// What is checked here is shape: the enum, the applicability of the two
// externalSecrets-only fields, and that a refresh interval is a duration a
// controller will accept. What is deliberately NOT checked here is whether the
// named store exists — that is a ClusterProfile question, validation has no
// cluster (ADR-0001), and the renderer answers it where the profile is an
// input. `store` is optional for the same reason: with exactly one store on the
// cluster the renderer picks it, and only the renderer can know that.
func (v *validator) secrets(field string, s *SecretBackend) {
	if s == nil {
		return
	}
	switch s.Backend {
	case SecretsCluster, SecretsSOPS, SecretsExternalSecrets:
	case "":
		v.err(ErrMissingRequired, field+".backend",
			"secrets.backend is required when secrets is set",
			"valid backends: cluster, externalSecrets, sops")
	default:
		v.err(ErrInvalidEnum, field+".backend",
			fmt.Sprintf("unknown secret backend %q", s.Backend),
			"valid backends: cluster, externalSecrets, sops")
	}
	// The two externalSecrets-only fields are refused elsewhere rather than
	// ignored: a store name under `cluster` configures nothing, and silently
	// accepting it is the quiet success issue #141 exists to prevent.
	for _, f := range []struct{ name, value string }{
		{"store", s.Store},
		{"refreshInterval", s.RefreshInterval},
	} {
		if f.value != "" && s.Backend != "" && s.Backend != SecretsExternalSecrets {
			v.err(ErrMutuallyExclusive, field+"."+f.name,
				fmt.Sprintf("%s is only meaningful with backend externalSecrets, not %q", f.name, s.Backend),
				"remove "+f.name+", or set backend to externalSecrets")
		}
	}
	// The two sops-only fields, refused under the other backends for the same
	// reason and with the same code.
	if len(s.AgeRecipients) > 0 && s.Backend != "" && s.Backend != SecretsSOPS {
		v.err(ErrMutuallyExclusive, field+".ageRecipients",
			fmt.Sprintf("ageRecipients is only meaningful with backend sops, not %q", s.Backend),
			"remove ageRecipients, or set backend to sops")
	}
	if s.AgeKeySecret != "" && s.Backend != "" && s.Backend != SecretsSOPS {
		v.err(ErrMutuallyExclusive, field+".ageKeySecret",
			fmt.Sprintf("ageKeySecret is only meaningful with backend sops, not %q", s.Backend),
			"remove ageKeySecret, or set backend to sops")
	}
	if s.Backend == SecretsSOPS {
		v.ageRecipients(field, s.AgeRecipients)
	}
	if s.RefreshInterval != "" {
		v.secretRefreshInterval(field+".refreshInterval", s.RefreshInterval)
	}
}

// ageRecipients checks the sops backend's key list: at least one, each spelled
// like an age public key, none of them a private one, no duplicates.
//
// The list is required rather than defaulted because there is nothing to
// default it to. A sops environment with no recipient is one where
// `kelson secret set` has nobody to encrypt for, and failing at validation
// with the `age-keygen` line in the message is better than failing at the
// first `set` — which is the moment a human is holding a credential and least
// wants to go and read documentation.
func (v *validator) ageRecipients(field string, recipients []string) {
	if len(recipients) == 0 {
		v.err(ErrMissingRequired, field+".ageRecipients",
			"secrets.backend sops encrypts to age recipients and none are listed",
			"generate a key with `age-keygen -o age.key`, keep the AGE-SECRET-KEY line out of Git, and list "+
				"the public half here: secrets: { backend: sops, ageRecipients: [age1…] }. The private half goes "+
				"into a Kubernetes Secret the Kustomization decrypts with — kelson never holds it")
		return
	}
	seen := map[string]bool{}
	for i, r := range recipients {
		at := fmt.Sprintf("%s.ageRecipients[%d]", field, i)
		switch {
		case strings.HasPrefix(r, AgeSecretKeyPrefix):
			// The worst possible paste, so it gets its own message: everything
			// else here is a typo, and this one is a credential in Git.
			v.err(ErrInvalidFormat, at,
				"this is an age *private* key, not a recipient",
				"put the public half here — the age1… line `age-keygen` prints as \"Public key\". If this key "+
					"has been committed, treat it as compromised: generate a new one, re-encrypt with "+
					"`kelson secret set`, and rewrite or rotate what the old key could open")
		case !ValidAgeRecipient(r):
			v.err(ErrInvalidFormat, at,
				fmt.Sprintf("%q is not an age recipient", r),
				"an age X25519 recipient is `age1` followed by 58 characters, as printed by `age-keygen`. "+
					"SSH recipients are not supported here")
		case seen[r]:
			v.err(ErrDuplicateName, at,
				fmt.Sprintf("age recipient %q is listed twice", r),
				"remove the duplicate; a repeated recipient wraps the same data key twice and means nothing")
		}
		seen[r] = true
	}
}

// secretRefreshInterval checks that a refresh interval is a positive Go
// duration. The renderer may not import `time` (ADR-0001, issue #20), so the
// parse happens here and the renderer writes the string through verbatim.
//
// A non-positive interval is refused rather than passed on. external-secrets
// reads `0` as "sync once and never again", which is a real behaviour with a
// real use — and it is not one an author reaches by typing `0` into a field
// named refreshInterval, so accepting it would mean a credential that silently
// stops rotating. There is deliberately no spelling for it yet; ADR-0020 says
// so and names what would have to be decided to add one.
func (v *validator) secretRefreshInterval(field, value string) {
	d, err := time.ParseDuration(value)
	if err != nil {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not a duration", value),
			"write a Go duration: 30s, 15m, 1h, 24h")
		return
	}
	if d <= 0 {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("refreshInterval %q is not positive", value),
			"write a positive Go duration, e.g. 1h (the default when the field is omitted); "+
				"there is no spelling for \"never refresh\", because a credential that stops rotating "+
				"without anyone asking is the failure this field exists to prevent")
	}
}

// previews validates the per-pull-request child-environment declaration
// (ADR-0017).
//
// It used to leave the delivery mode to a render gate, because previews were
// Flux-only and the mode was an Environment's. There is one delivery path now
// (ADR-0028), so an environment that declares previews gets them and this is
// the only place they are judged.
func (v *validator) previews(field string, p *Previews) {
	if p == nil {
		return
	}
	switch p.Provider {
	case PreviewGitHub, PreviewGitLab:
	case "":
		v.err(ErrMissingRequired, field+".provider",
			"previews.provider is required when previews is set",
			"set provider to github or gitlab — it selects which forge API flux-operator polls for change requests")
	default:
		v.err(ErrInvalidEnum, field+".provider",
			fmt.Sprintf("unknown previews provider %q", p.Provider),
			"valid providers: github, gitlab")
	}

	if p.Repo == "" {
		v.err(ErrMissingRequired, field+".repo",
			"previews.repo is required when previews is set",
			"set repo to the HTTP(S) URL of the repository whose pull requests become previews, "+
				"e.g. https://github.com/acme/checkout — the repository the application is written in")
	} else {
		v.remoteURL(field+".repo", p.Repo, []string{"https", "http"},
			"use the HTTP(S) URL of the source repository, e.g. https://github.com/acme/checkout. "+
				"It is reached over the forge's HTTP API, so an SSH remote — the shape a clone URL "+
				"takes — is the wrong string in the right-looking field")
	}

	// An empty secretRef is a choice rather than an omission (ADR-0033
	// decision 4): kelson materializes the credential from the git connection
	// covering previews.repo, so there is nothing for an author to write. A
	// *named* one is still held to being a name, because the failure that
	// catches — a pasted token where a Secret name belongs — is the one
	// ADR-0009 exists to prevent.
	if p.SecretRef != "" && !ValidSecretName(p.SecretRef) {
		v.err(ErrInvalidFormat, field+".secretRef",
			fmt.Sprintf("%q is not a DNS-1123 label, so it cannot name a Secret", p.SecretRef),
			"set secretRef to the name of a Secret holding forge credentials — flux-operator reads "+
				"username/password, or the githubApp* keys, and the spec carries the name and never the "+
				"token — or leave it unset and kelson materializes the credential from the git "+
				"connection covering previews.repo")
	}

	if p.Interval != "" {
		v.duration(field+".interval", p.Interval)
	}

	if f := p.Filter; f != nil {
		v.previewLabels(field+".filter.labels", f.Labels, false)
		v.pattern(field+".filter.includeBranch", f.IncludeBranch)
		v.pattern(field+".filter.excludeBranch", f.ExcludeBranch)
		if f.Limit != nil && (*f.Limit < 1 || *f.Limit > previewMaxLimit) {
			v.err(ErrOutOfRange, field+".filter.limit",
				fmt.Sprintf("limit %d is outside 1..%d", *f.Limit, previewMaxLimit),
				fmt.Sprintf("set limit between 1 and %d, or remove it for kelson's default of %d "+
					"(deliberately below flux-operator's own 100 — the ceiling is a cost control)",
					previewMaxLimit, PreviewDefaultLimit))
		}
	}
	if s := p.Skip; s != nil {
		v.previewLabels(field+".skip.labels", s.Labels, true)
	}

	if p.Artifacts.Repository == "" {
		v.err(ErrMissingRequired, field+".artifacts.repository",
			"previews.artifacts.repository is required when previews is set",
			"set artifacts.repository to the oci:// repository the per-pull-request manifests are published to, "+
				"e.g. oci://ghcr.io/acme/checkout-previews. The tag is the pull request's head commit and kelson "+
				"chooses it")
	} else {
		v.remoteURL(field+".artifacts.repository", p.Artifacts.Repository, []string{"oci"},
			"use an oci:// repository URL without a tag, e.g. oci://ghcr.io/acme/checkout-previews")
		// A colon introduces a tag only in the last path segment; before the
		// first slash it is a registry port (oci://registry.internal:5000/x).
		trimmed := strings.TrimPrefix(p.Artifacts.Repository, "oci://")
		tagged := strings.Contains(p.Artifacts.Repository, "@")
		if i := strings.LastIndex(trimmed, "/"); i >= 0 && strings.Contains(trimmed[i+1:], ":") {
			tagged = true
		}
		if tagged {
			v.err(ErrInvalidFormat, field+".artifacts.repository",
				fmt.Sprintf("%q carries a tag or a digest", p.Artifacts.Repository),
				"drop the tag: each preview is pulled at its pull request's head commit SHA, so a tag here would "+
					"either be ignored or pin every preview to the same manifests")
		}
	}
	if p.Artifacts.SecretRef != "" {
		v.name(field+".artifacts.secretRef", p.Artifacts.SecretRef, "secret")
	}
}

// previewMaxLimit is flux-operator's own documented ceiling for
// `filter.limit`. kelson refuses above it rather than letting the operator
// reject the object after it is applied.
const previewMaxLimit = 10000

// previewLabels checks a forge label list. Skip lists allow a leading `!`,
// which flux-operator reads as "skip while this label is absent" — the shape a
// "tests passed" gate takes — and filter lists do not.
func (v *validator) previewLabels(field string, labels []string, allowNegation bool) {
	for i, l := range labels {
		f := fmt.Sprintf("%s[%d]", field, i)
		if strings.TrimSpace(l) == "" {
			v.err(ErrInvalidFormat, f, "a label must not be empty",
				"remove the entry, or name a label the forge carries, e.g. deploy/preview")
			continue
		}
		if !allowNegation && strings.HasPrefix(l, "!") {
			v.err(ErrInvalidFormat, f,
				fmt.Sprintf("label %q is negated, which only skip.labels understands", l),
				"remove the leading !, or move the entry to previews.skip.labels — a filter selects change "+
					"requests, a skip pauses updates to the ones already selected")
		}
	}
}

// pattern checks that a filter regular expression compiles. flux-operator
// evaluates it with Go's own regexp engine, so compiling it here is the same
// judgement the operator will make, taken while the author is still looking at
// the field.
func (v *validator) pattern(field, expr string) {
	if expr == "" {
		return
	}
	if _, err := regexp.Compile(expr); err != nil {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not a valid regular expression: %v", expr, err),
			`use a Go regular expression matched against the branch name, e.g. "^feat/.*"`)
	}
}

// durationRE is Kubernetes' and Flux's duration shape: one or more
// number+unit pairs, no fractions, no days.
var durationRE = regexp.MustCompile(`^([0-9]+(ms|s|m|h))+$`)

func (v *validator) duration(field, s string) {
	if !durationRE.MatchString(s) {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not a duration", s),
			"use a Go duration of whole units, e.g. 30s, 10m, 1h")
	}
}

// sourceScope is what the component checks need to know about the sources the
// Project declares: which names a component may bind to, whether one of them is
// the default a component that names none would get, and whether anything is
// built here at all.
//
// It is passed down rather than re-derived per component because the two
// questions it answers — "is this name spelled like one of ours" and "is there a
// default" — are properties of the document, and asking them once is what keeps
// the missing-default error able to name the candidates.
type sourceScope struct {
	// names are the declared source names, in spec order.
	names []string
	// hasDefault reports whether a component that names no source gets one.
	hasDefault bool
	// builds reports whether the Project's build strategy is anything but
	// `none`. Nothing binds a source in a project that builds nothing.
	builds bool
	// image is the Project's *authored* `image:`, not the "(built from source)"
	// marker the no-image-source check works with. A component covered by a
	// shared pre-built image is not built and therefore needs no binding, and
	// the marker cannot tell that case from the one this scope exists to refuse.
	image string
}

// needsDefault reports the case ADR-0035 decision 3 refuses: a component that
// would be built from a source, naming none, in a Project that declares several
// and names no `default`. Picking the first entry would make a list's order a
// decision nobody took.
//
// A component with an image of its own — or a Project image it inherits — is not
// built and therefore needs no binding, which is why the image is part of the
// question rather than a separate one.
func (s sourceScope) needsDefault(c Component, kind ComponentKind) bool {
	return s.builds && !s.hasDefault && len(s.names) > 1 &&
		kind.IsWorkload() && c.SourceName() == "" && c.Image == "" && s.image == ""
}

// components validates the one leaf list of ADR-0014. The kind decides which
// half of the rules a component is held to; a field belonging to the other
// half is an error, never a no-op (issue #141).
func (v *validator) components(
	field string,
	comps []Component,
	services map[string]Component,
	projectImage string,
	sources sourceScope,
) {
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
		if kind.IsChart() {
			v.chartComponent(f, c, kind)
			continue
		}
		v.workloadComponent(f, c, kind, services, projectImage, sources)
	}
}

// componentSourceName checks the binding arm of `source:` on a component that
// can build (ADR-0035 decision 3): that the name is spelled like one, and that
// a component needing a default has one.
//
// What it does *not* check is that the name resolves. The scope is the
// Project's list and the GitSources the instance offers, and a document can only
// see the first half — so a name nothing local declares may still be a global
// source, and refusing here would refuse a legal spec. That refusal belongs to
// the resolver, which holds both halves and lists them (ref/unknown-source).
func (v *validator) componentSourceName(field string, c Component, kind ComponentKind, sources sourceScope) {
	if name := c.SourceName(); name != "" {
		v.name(field+".source", name, "source")
		return
	}
	if !sources.needsDefault(c, kind) {
		return
	}
	v.err(ErrNoDefaultSource, field+".source",
		fmt.Sprintf("component %q names no source, and project sources declare %d of them with none named %q",
			c.Name, len(sources.names), DefaultSourceName),
		fmt.Sprintf("set source: to one of: %s — or rename one of them to %s, which is what a component "+
			"that names no source builds from. kelson refuses to pick for you: the order of a list is not "+
			"a decision", strings.Join(sources.names, ", "), DefaultSourceName))
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
	if c.Kind.IsData() || c.Kind.IsChart() {
		// Neither has a shape to contradict: a data component's topology is its
		// operator's, and a helm component's is the chart's.
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
			"valid presets: shared, small, ha-small, ha-medium, branch (docs/data-services.md)")
	}
	for _, set := range workloadOnlyFields(c) {
		v.err(ErrMutuallyExclusive, field+"."+set,
			fmt.Sprintf("component %q has kind %q, which renders a managed data service, but sets %s", c.Name, kind, set),
			"remove "+set+"; a data component's whole configuration is its preset, because its topology belongs to the "+
				"operator. Bind a workload to it with {from: {service: "+c.Name+", key: uri}}")
	}
	v.dataAuth(field, c, kind)
	v.chartOnlyFields(field, c, kind)
}

// dataAuth checks the `auth:` reference of a data component, and refuses it on
// the data kind that has no use for one.
//
// The split is not an implementation gap. `kind: postgres` gets its application
// credential from CloudNativePG's initdb bootstrap, which generates it and
// publishes it as <cluster>-app; an author-written Secret there would be a
// second credential the database never learns about, and every binding would
// keep resolving against the operator's. `kind: valkey` is the opposite case —
// the operator reads a user password from a Secret it never creates — which is
// what `auth:` exists to name (ADR-0015 amendment, 2026-08-14).
func (v *validator) dataAuth(field string, c Component, kind ComponentKind) {
	if c.Auth == nil {
		return
	}
	if kind != ComponentValkey {
		v.err(ErrMutuallyExclusive, field+".auth",
			fmt.Sprintf("component %q has kind %q, which manages its own credentials", c.Name, kind),
			"remove auth; CloudNativePG's initdb bootstrap generates the application user and its password and "+
				"publishes them as <cluster>-app, so a Secret you write would be a second credential the database "+
				"never accepts. Bind {from: {service: "+c.Name+", key: password}} and kelson points the "+
				"secretKeyRef at the one the operator made (docs/data-services.md)")
		return
	}
	v.secretRef(field+".auth", c.Auth)
}

// chartComponent validates a `kind: helm` component (ADR-0016 decision 4):
// the chart coordinates it must name, and the absence of everything belonging
// to the other two halves of the list.
//
// The delivery-mode gate that used to sit beside this in the renderer is gone
// (ADR-0028 decision 8): a chart delegates to helm-controller, and Flux is the
// only path there has been since. Whether helm-controller is *installed* is a
// ClusterProfile question and was never validation's.
func (v *validator) chartComponent(field string, c Component, kind ComponentKind) {
	if c.Chart == "" {
		v.err(ErrMissingRequired, field+".chart",
			fmt.Sprintf("component %q has kind %q and names no chart", c.Name, kind),
			"set chart to the chart's name within its source, e.g. chart: ingress-nginx")
	}
	if c.ChartVersion == "" {
		v.err(ErrMissingRequired, field+".chartVersion",
			fmt.Sprintf("component %q does not pin a chart version", c.Name),
			"set chartVersion to an exact version, e.g. chartVersion: 4.11.3. kelson refuses an unpinned chart "+
				"rather than resolving one at apply time: the same document would install different manifests on "+
				"different days, and the diff — which only ever shows the HelmRelease — would report no change "+
				"at all")
	}
	v.chartSource(field, c)
	v.chartValues(field+".values", c.Values)
	for i, vf := range c.ValuesFrom {
		v.valuesFrom(fmt.Sprintf("%s.valuesFrom[%d]", field, i), vf)
	}
	for _, set := range workloadOnlyFields(c) {
		v.err(ErrMutuallyExclusive, field+"."+set,
			fmt.Sprintf("component %q has kind %q, which delegates a chart to helm-controller, but sets %s", c.Name, kind, set),
			"remove "+set+"; what a chart runs is the chart's business, configured through values and valuesFrom. "+
				"kelson writes the HelmRelease and never templates the chart")
	}
	if c.Preset != "" {
		v.err(ErrMutuallyExclusive, field+".preset",
			fmt.Sprintf("component %q has kind %q, and a preset is the topology of a data component", c.Name, kind),
			"remove preset; a chart's topology is configured with values, not with a kelson preset")
	}
	v.workloadAuth(field, c, kind)
}

// workloadAuth refuses `auth:` on everything that is not a data component.
//
// The field configures an operator kelson delegates to; on a workload or a
// chart there is no operator on the other end of it, and the thing an author
// most likely meant has its own spelling — an env value written as
// {secret: <name>, key: <key>} (ADR-0018), which is the same two fields in the
// place they take effect.
func (v *validator) workloadAuth(field string, c Component, kind ComponentKind) {
	if c.Auth == nil {
		return
	}
	v.err(ErrMutuallyExclusive, field+".auth",
		fmt.Sprintf("component %q has kind %q, and auth configures the ACL user of a managed data service",
			c.Name, kind),
		"remove auth. To read a credential from a Secret here, write it where it is used — "+
			"env: {MY_VAR: {secret: "+refExample(c.Auth.Name)+", key: "+refKeyExample(c.Auth.Key)+"}}, which renders "+
			"as a valueFrom.secretKeyRef. auth belongs on a kind: valkey component, where it names the "+
			"Secret the cache's own password is read from")
}

// refKeyExample keeps the workloadAuth remediation concrete when the author
// already wrote the key half, the way refExample does for the name.
func refKeyExample(key string) string {
	if key == "" {
		return "<key>"
	}
	return key
}

// chartSource holds a helm component to exactly one chart source. Both set is
// as wrong as neither: they render different Flux source kinds, so kelson would
// have to pick one and the author would not know which.
func (v *validator) chartSource(field string, c Component) {
	if c.Source == nil {
		v.err(ErrMissingRequired, field+".source",
			fmt.Sprintf("component %q names no chart source", c.Name),
			"set source.repository to a classic Helm repository URL, or source.oci to an OCI registry URL")
		return
	}
	// The other arm of the union: a bare name is a build binding, and a chart is
	// fetched rather than built (componentsource.go, ADR-0035 decision 3).
	if chart := c.ChartSourceOf(); chart == nil {
		v.err(ErrMutuallyExclusive, field+".source",
			fmt.Sprintf("component %q has kind %q and names source %q, which is a source to build from",
				c.Name, ComponentHelm, c.SourceName()),
			"a helm component is not built from a repository — it installs a published chart. Write where "+
				"the chart is fetched from instead: source: {repository: <Helm repository URL>} or "+
				"source: {oci: <OCI registry URL>}. A source name binds a component kelson "+
				"builds")
		return
	}
	repo, oci := c.Source.Chart.Repository, c.Source.Chart.OCI
	switch {
	case repo == "" && oci == "":
		v.err(ErrMissingRequired, field+".source",
			fmt.Sprintf("component %q has an empty chart source", c.Name),
			"set exactly one of source.repository (a classic Helm repository URL) or source.oci (an OCI registry URL)")
		return
	case repo != "" && oci != "":
		v.err(ErrMutuallyExclusive, field+".source",
			fmt.Sprintf("component %q sets both source.repository and source.oci", c.Name),
			"keep one: a repository renders a HelmRepository and an OCI URL renders an OCIRepository, "+
				"and a chart is fetched from one of them")
		return
	}
	if repo != "" {
		v.remoteURL(field+".source.repository", repo, []string{"https", "http"},
			"use an https URL to the repository that serves index.yaml, e.g. https://kubernetes.github.io/ingress-nginx")
		return
	}
	v.remoteURL(field+".source.oci", oci, []string{"oci"},
		"use an oci:// URL to the registry path holding the chart, without the chart name, "+
			"e.g. oci://ghcr.io/acme/charts")
}

// remoteURL checks a URL for the one thing kelson can judge without a network:
// that it is a URL at all, with a host and a scheme this field accepts.
// Whether the other end answers is the other end's to say.
//
// It is shared by every remote address in the spec — chart sources, the
// previews source repository, the previews artifact repository — because the
// judgement is the same one and a second copy would drift on which of the
// three parts it checks.
func (v *validator) remoteURL(field, raw string, schemes []string, remediation string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !slices.Contains(schemes, u.Scheme) {
		v.err(ErrInvalidFormat, field,
			fmt.Sprintf("%q is not a %s URL", raw, strings.Join(schemes, "/")),
			remediation)
	}
}

// chartValues walks the inline values for the one property the renderer and the
// spec hash both need: string keys all the way down. YAML permits `1: true` as
// a mapping key and Helm itself does not, so refusing here costs an author
// nothing and keeps rendering and hashing total functions.
//
// Nothing else about a value is inspected. In particular kelson does not guess
// which key holds a credential: content-sniffing would be a heuristic that
// blocks legitimate configuration and misses the interesting cases, so the rule
// is documented rather than enforced (docs/model.md, ADR-0009).
func (v *validator) chartValues(field string, values map[string]any) {
	for _, k := range sortedMapKeys(values) {
		v.chartValue(field+"."+k, values[k])
	}
}

func (v *validator) chartValue(field string, value any) {
	switch t := value.(type) {
	case map[string]any:
		for _, k := range sortedMapKeys(t) {
			v.chartValue(field+"."+k, t[k])
		}
	case map[any]any:
		v.err(ErrInvalidFormat, field,
			"chart values must be keyed by strings",
			"quote the keys of this mapping; YAML allows non-string keys and Helm values do not")
	case []any:
		for i, item := range t {
			v.chartValue(fmt.Sprintf("%s[%d]", field, i), item)
		}
	}
}

// valuesFrom holds one valuesFrom entry to exactly one reference.
func (v *validator) valuesFrom(field string, vf ValuesFrom) {
	switch {
	case vf.SecretRef == "" && vf.ConfigMapRef == "":
		v.err(ErrMissingRequired, field,
			"a valuesFrom entry names a Secret or a ConfigMap",
			"set secretRef for credentials, or configMapRef for plain configuration held outside the spec")
	case vf.SecretRef != "" && vf.ConfigMapRef != "":
		v.err(ErrMutuallyExclusive, field,
			"a valuesFrom entry sets both secretRef and configMapRef",
			"keep one; use two entries if the release needs both")
	case vf.SecretRef != "":
		v.name(field+".secretRef", vf.SecretRef, "secret")
	default:
		v.name(field+".configMapRef", vf.ConfigMapRef, "configmap")
	}
}

// chartOnlyFields reports the helm fields set on a component that is not one.
// It is the mirror of workloadOnlyFields and exists for the same reason: a
// `chart:` on a worker configures nothing, and silence about it is the failure
// issue #141 named.
func (v *validator) chartOnlyFields(field string, c Component, kind ComponentKind) {
	var set []string
	if c.Chart != "" {
		set = append(set, "chart")
	}
	if c.ChartVersion != "" {
		set = append(set, "chartVersion")
	}
	// Only the chart arm of `source:` is a helm field. The other arm is a source
	// name, which is meaningful on every kind that builds — and refused on the
	// data kinds separately, because what a database runs is its operator's
	// (componentsource.go, ADR-0035 decision 3).
	if c.ChartSourceOf() != nil {
		set = append(set, "source")
	}
	if kind.IsData() && c.SourceName() != "" {
		v.err(ErrMutuallyExclusive, field+".source",
			fmt.Sprintf("component %q has kind %q, which runs an operator's image rather than one kelson builds",
				c.Name, kind),
			"remove source; a data component has no build, so a repository to build it from names nothing. "+
				"Bind a workload to it with {from: {service: "+c.Name+", key: uri}}")
	}
	if len(c.Values) > 0 {
		set = append(set, "values")
	}
	if len(c.ValuesFrom) > 0 {
		set = append(set, "valuesFrom")
	}
	for _, f := range set {
		v.err(ErrMutuallyExclusive, field+"."+f,
			fmt.Sprintf("component %q has kind %q, and %s configures a Helm chart", c.Name, kind, f),
			"remove "+f+", or set kind: helm — a chart field on any other kind attaches to nothing")
	}
}

// sortedMapKeys keeps every walk over an authored map deterministic, so a
// document with two problems reports them in the same order every run.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
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
	if c.Release != nil {
		out = append(out, "release")
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
	sources sourceScope,
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
	v.workloadAuth(field, c, kind)
	if len(c.Tools) > 0 {
		if kind != ComponentAgent {
			v.err(ErrMutuallyExclusive, field+".tools",
				fmt.Sprintf("component %q has kind %q, and tools is the capability policy of an agent", c.Name, kind),
				"remove tools, or set kind: agent — a tool allow-list on anything else attaches to nothing")
		} else {
			v.tools(field+".tools", c.Tools)
			v.gate("$.spec.components[].tools", field+".tools")
		}
	}
	v.release(field, c, kind)
	v.chartOnlyFields(field, c, kind)
	v.componentSourceName(field, c, kind, sources)
	v.imageRef(field+".image", c.Image)
	v.replicas(field+".replicas", c.Replicas)
	v.resources(field+".resources", c.Resources)
	v.envMap(field+".env", c.Env, services)

	// A component that names a source has an image source even when the Project
	// declares none: the name may be a GitSource the instance offers, which this
	// document cannot see (ADR-0035 decision 3). Whether it resolves is the
	// resolver's refusal, not this one's.
	if c.Image == "" && projectImage == "" && c.SourceName() == "" {
		v.err(ErrNoImageSource, field,
			fmt.Sprintf("component %q has no image source", c.Name),
			"set image on the component or the Project, name a source with source: <name>, or configure "+
				"spec.source (or spec.sources) + spec.build with a strategy other than none")
	}
}

// release validates a component's release-command hook (issue #104): the kinds
// it applies to, the command it must name, and the timeout's grammar.
//
// The hook is *gated* (ADR-0028 decision 8): the plane that waited for the Job
// is deleted, so the field renders nothing and the gate table refuses it by
// name. The shape checks below still run, for the reason [validator.tools]
// gives — the gate is about what kelson implements, not about what the author
// wrote, and an author who fixes the shape should not meet a second problem in
// it the day #227 lands.
func (v *validator) release(field string, c Component, kind ComponentKind) {
	if c.Release == nil {
		return
	}
	v.gate("$.spec.components[].release", field+".release")
	if kind == ComponentCron {
		v.err(ErrMutuallyExclusive, field+".release",
			fmt.Sprintf("component %q has kind %q, and a release command runs once per deploy", c.Name, kind),
			"remove release; a cron component already is a command on a schedule. Put the release hook on the "+
				"component whose rollout must wait for it — usually the service that talks to the database")
		return
	}
	if len(c.Release.Command) == 0 {
		v.err(ErrMissingRequired, field+".release.command",
			fmt.Sprintf("component %q declares a release hook with no command", c.Name),
			`set release.command to the argv to run, e.g. command: ["./manage.py", "migrate"]`)
	}
	for i, arg := range c.Release.Command {
		if strings.TrimSpace(arg) == "" {
			v.err(ErrMissingRequired, fmt.Sprintf("%s.release.command[%d]", field, i),
				"a release command argument is empty",
				"remove the empty entry; every argv element is passed to the container verbatim")
		}
	}
	if t := c.Release.Timeout; t != "" {
		// The same duration grammar every other kelson budget is written in.
		v.duration(field+".release.timeout", t)
		if d, err := time.ParseDuration(t); err == nil && d <= 0 {
			v.err(ErrOutOfRange, field+".release.timeout",
				fmt.Sprintf("timeout %q leaves the command no time to run", t),
				"give the command a budget it can finish in, or omit the field for the default of "+
					DefaultReleaseTimeout.String())
		}
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

// projectSources validates what a Project declares its components may build
// from, in either spelling, and returns the scope the component checks are held
// to (ADR-0035 decision 1).
//
// Everything here is answerable from the document: the two spellings are one
// list, so writing both is a refusal; a name is what a component binds to, so it
// must be unique and spelled like a name. What is deliberately *not* answered
// here is whether a repository exists or can be reached — that needs a network,
// and validation has none (ADR-0001).
func (v *validator) projectSources(s *ProjectSpec) sourceScope {
	scope := sourceScope{builds: s.Build == nil || s.Build.Strategy != BuildNone, image: s.Image}

	if s.Source != nil && len(s.Sources) > 0 {
		v.err(ErrMutuallyExclusive, "$.spec.sources",
			"the Project declares both spec.source and spec.sources, which are one list written two ways",
			"keep spec.sources and move the singular block into it as an entry named "+DefaultSourceName+
				" — `sources: [{name: "+DefaultSourceName+", git: <url>, ref: <ref>}, …]` — or delete "+
				"spec.sources and keep the shorthand")
	}

	if src := s.Source; src != nil {
		if src.Name != "" {
			v.err(ErrMutuallyExclusive, "$.spec.source.name",
				fmt.Sprintf("the singular spec.source names itself %q, and it is already named %q",
					src.Name, DefaultSourceName),
				"remove name; the shorthand declares exactly one source called "+DefaultSourceName+
					". To choose the name, write the list instead: sources: [{name: "+src.Name+
					", git: <url>}]")
		}
		v.source("$.spec.source", *src, false)
	}

	seen := map[string]int{}
	for i, src := range s.Sources {
		f := fmt.Sprintf("$.spec.sources[%d]", i)
		v.source(f, src, true)
		if src.Name == "" {
			continue
		}
		if prev, dup := seen[src.Name]; dup {
			v.err(ErrDuplicateName, f+".name",
				fmt.Sprintf("duplicate source name %q (first at sources[%d])", src.Name, prev),
				"source names must be unique within the Project: a component binds to one by name, "+
					"and two entries answering to it means the binding names nothing in particular")
			continue
		}
		seen[src.Name] = i
	}

	for _, src := range s.EffectiveSources() {
		scope.names = append(scope.names, src.Name)
	}
	_, scope.hasDefault = s.DefaultSource()
	return scope
}

// source validates one declared source in either spelling. `named` says whether
// this one carries its own name — a list entry does, and the shorthand is named
// by definition.
func (v *validator) source(field string, src Source, named bool) {
	if named {
		v.name(field+".name", src.Name, "source")
	}
	if src.Git == "" {
		v.err(ErrMissingRequired, field+".git",
			trimRoot(field)+".git is required",
			"set "+trimRoot(field)+".git to the repository URL, or remove the source and use pre-built images")
	}
	if src.Connection != "" {
		// No gate any more: the server resolves this to a credential and
		// projects it into the build pod's clone (ADR-0033 decisions 4 and 5,
		// internal/forgeconn). What is still not checked here is that the
		// connection *exists* — that is cluster state, and validation
		// deliberately has none (ADR-0001); a name nothing matches is a
		// resolution refusal naming this field.
		v.name(field+".connection", src.Connection, "connection")
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

	sources := v.projectSources(s)
	if s.Build != nil {
		switch s.Build.Strategy {
		case "", BuildAuto, BuildDockerfile, BuildBuildpacks, BuildNone:
		default:
			v.err(ErrInvalidEnum, "$.spec.build.strategy",
				fmt.Sprintf("unknown build strategy %q", s.Build.Strategy),
				"valid strategies: auto, dockerfile, buildpacks, none")
		}
		if s.Build.By != "" {
			switch s.Build.By {
			case BuildByKelson, BuildByCI:
			default:
				v.err(ErrInvalidEnum, "$.spec.build.by",
					fmt.Sprintf("unknown build owner %q", s.Build.By),
					"valid values: "+BuildByKelson+", "+BuildByCI+
						" — kelson builds the image itself and ci reports one its pipeline built")
			}
			// No gate any more: `BuildService.ReportBuild` reads this field
			// (ADR-0034 decision 3, internal/api/build.go). `ci` is what makes
			// the server act on a CI report and publish the change request's
			// preview; `kelson` is what makes it decline one, and the response
			// says which. The enum above is still all validation can judge here
			// — whether kelson could actually build the project is a question
			// about `source:`, and it is asked where the report is answered.
		}
	}
	projectImage := s.Image
	if len(sources.names) > 0 && sources.builds {
		projectImage = "(built from source)"
	}

	v.imageRef("$.spec.image", s.Image)
	v.envMap("$.spec.env", s.Env, services)
	v.components("$.spec.components", s.Components, services, projectImage, sources)

	if d := s.Defaults; d != nil {
		v.policy("$.spec.defaults.policy", d.Policy)
		// The Project's own components are in this document, so the
		// cross-reference `protect:` needs is available here rather than in
		// ValidateEnvironment.
		declared := map[string]ComponentKind{}
		for _, c := range s.Components {
			declared[c.Name] = c.EffectiveKind()
		}
		v.protectedComponents("$.spec.defaults.policy", d.Policy, declared, p.Metadata.Name)
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

	v.policy("$.spec.policy", s.Policy)
	v.secrets("$.spec.secrets", s.Secrets)
	v.previews("$.spec.previews", s.Previews)

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
		v.imageRef(f+".image", ov.Image)
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

// validateGitConnection checks a GitConnection (ADR-0033). There is no
// cross-document half to it: a connection names a Secret, and whether that
// Secret exists — or holds the keys kelson will look for — is the cluster's to
// know at use time. Validation refuses what it can judge from the document
// alone, which is the same line every other reference in this package draws
// (ADR-0009).
func validateGitConnection(g *GitConnection, v *validator) {
	v.name("$.metadata.name", g.Metadata.Name, "connection")
	s := &g.Spec

	switch {
	case s.Provider == "":
		v.err(ErrMissingRequired, "$.spec.provider",
			"a GitConnection names the forge it connects to",
			"set provider to one of: "+gitProviderNames()+
				". Use generic for any git host reachable with a token")
	case !s.Provider.Valid():
		v.err(ErrInvalidEnum, "$.spec.provider",
			fmt.Sprintf("unknown git provider %q", s.Provider),
			"valid providers: "+gitProviderNames()+
				". The enum grows one value per adapter, so a provider kelson has not written an "+
				"adapter for is spelled generic and gets private clones and nothing else")
	}

	v.connectionHost("$.spec.host", s)
	v.connectionAuth("$.spec.auth", s)
	v.owner("$.spec.owner", s.Owner)
}

// validateGitSource checks a GitSource (ADR-0035 decision 2). Like a
// connection it has no cross-document half: what it names is a repository and a
// connection, and whether either answers is the cluster's and the forge's to
// know at use time.
//
// It is held to exactly what a Project's own source is held to, because it is
// the same thing declared one tier out — the source of a document that cannot
// see the other tier must not be judged by a different rule.
func validateGitSource(g *GitSource, v *validator) {
	v.name("$.metadata.name", g.Metadata.Name, "source")
	s := &g.Spec

	if s.Git == "" {
		v.err(ErrMissingRequired, "$.spec.git",
			"a GitSource names the repository it offers",
			"set spec.git to the repository URL. A source is where code is read from; the credential it "+
				"is read with is a GitConnection, and it is spec.connection")
	}
	if s.Connection != "" {
		// Whether the connection *exists* is cluster state, and validation
		// deliberately has none (ADR-0001) — the same line a Project's
		// source.connection is held to.
		v.name("$.spec.connection", s.Connection, "connection")
	}
	v.owner("$.spec.owner", s.Owner)
}

// connectionHost holds the forge base URL to what kelson can judge without a
// network — the same judgement [validator.remoteURL] makes everywhere else —
// and decides when the field may be omitted.
//
// Only `provider: github` may omit it, because only it has a default worth
// writing down (DefaultGitHubHost). A provider whose whole point is that it
// runs somewhere else has nothing to fall back to, and a connection with no
// host would resolve against no repository.
func (v *validator) connectionHost(field string, s *GitConnectionSpec) {
	switch {
	case s.Host != "":
		v.remoteURL(field, s.Host, []string{"https", "http"},
			"use the forge's base URL over HTTP(S) — the host the API and the clones are reached at, "+
				"e.g. https://github.com or https://git.acme.internal. Everything here is HTTPS: an SSH "+
				"remote is a different credential class and is not this field")
	case s.Provider == GitProviderGitHub:
		// DefaultGitHubHost is what an omitted host means here, and it is the
		// common case: one connection to github.com, zero configuration.
	case s.Provider.Valid():
		v.err(ErrMissingRequired, field,
			fmt.Sprintf("provider %q names no default host", s.Provider),
			"set host to the forge's base URL, e.g. https://git.acme.internal. Only provider github "+
				"may omit it, because only it has a default worth writing down ("+DefaultGitHubHost+")")
		// A provider that is empty or unknown has already been reported; adding a
		// second error about the host it cannot default would name the wrong field.
	}
}

// connectionAuth holds a connection to exactly one credential kind, and then to
// what that kind needs.
//
// Neither arm carries a value, which is why the checks below are all about
// names and identifiers: the private key, the webhook secret and the token live
// in a Kubernetes Secret somebody else manages, and nothing in this package
// reads one (ADR-0009, ADR-0033 decision 1).
func (v *validator) connectionAuth(field string, s *GitConnectionSpec) {
	app, token := s.Auth.GitHubApp, s.Auth.Token
	switch {
	case app == nil && token == nil:
		v.err(ErrMissingRequired, field,
			"a GitConnection carries no credential reference",
			"set exactly one of auth.githubApp (this instance's own GitHub App) or "+
				"auth.token (a token held in a Secret). A connection that authenticates with neither "+
				"can reach nothing a public clone could not")
		return
	case app != nil && token != nil:
		v.err(ErrMutuallyExclusive, field,
			"a GitConnection sets both auth.githubApp and auth.token",
			"keep one: an installation token is scoped to chosen repositories and expires within the "+
				"hour, a token is a standing credential, and kelson would otherwise have to pick which "+
				"one it acts as without the author knowing which")
		return
	}

	if token != nil {
		v.connectionSecretRef(field+".token.secretRef", token.SecretRef,
			"the token, under key "+TokenKey+" (and optionally "+TokenUsernameKey+")")
		return
	}

	// GitHub App auth is the github adapter's shape: the manifest flow, the
	// installation, the RS256 JWT. No other provider has one to speak.
	if s.Provider != "" && s.Provider != GitProviderGitHub {
		v.err(ErrAuthProviderMismatch, field+".githubApp",
			fmt.Sprintf("auth.githubApp needs provider %q but the connection declares %q",
				GitProviderGitHub, s.Provider),
			fmt.Sprintf("set provider to %s if this really is a GitHub App, or authenticate with "+
				"auth.token instead — the app-manifest flow and installation tokens are GitHub's and "+
				"no other adapter can mint one", GitProviderGitHub))
	}
	if app.AppID < 1 {
		v.err(ErrOutOfRange, field+".githubApp.appID",
			fmt.Sprintf("appID is %d; an app has a positive numeric ID", app.AppID),
			"set appID to the ID GitHub reports when the app is created — the manifest flow writes it "+
				"here, and it is the subject of every JWT kelson signs with the app key")
	}
	if app.InstallationID < 0 {
		v.err(ErrOutOfRange, field+".githubApp.installationID",
			fmt.Sprintf("installationID is %d; an installation has a non-negative ID", app.InstallationID),
			"set installationID to the ID the installation webhook reports, or omit it: 0 is the valid "+
				"state between creating the app and installing it, and the connection reports "+
				"Ready=False until the installation arrives")
	}
	v.connectionSecretRef(field+".githubApp.secretRef", app.SecretRef,
		"the app private key and webhook secret, under keys "+
			GitHubAppPrivateKeyKey+" and "+GitHubAppWebhookSecretKey)
}

// connectionSecretRef holds one credential reference to a name kelson could
// actually address. `what` names the keys the reader will look for, so the
// remediation tells an operator what to put in the Secret it is asking for.
func (v *validator) connectionSecretRef(field, name, what string) {
	if name == "" {
		v.err(ErrMissingRequired, field,
			"a credential reference needs the name of a Secret",
			"set "+trimRoot(field)+" to the name of a Secret in kelson's namespace holding "+what+
				". The spec carries the name and never the value")
		return
	}
	v.name(field, name, "secret")
}

// owner checks the discriminated owner reference of ADR-0033 decision 6, on
// either kind that carries one — a GitConnection, and now a GitSource, which
// reuses the block rather than designing a second one (ADR-0035 decision 2).
//
// It is validated in full today and enforced by nothing, which is the decision
// and not an oversight: the semantics are fixed now so tenancy (#231) attaches
// to stored documents rather than migrating them. A malformed owner would
// otherwise be found for the first time by the code that finally enforces it.
func (v *validator) owner(field string, o *ConnectionOwner) {
	if o == nil {
		return
	}
	switch {
	case o.Kind == "":
		v.err(ErrMissingRequired, field+".kind",
			"an owner block names the kind of principal that owns this document",
			"set kind to one of: "+strings.Join(OwnerKinds, ", ")+
				", or remove owner entirely — an absent owner is instance-owned")
	case !slices.Contains(OwnerKinds, o.Kind):
		v.err(ErrInvalidEnum, field+".kind",
			fmt.Sprintf("unknown owner kind %q", o.Kind),
			"valid kinds: "+strings.Join(OwnerKinds, ", "))
	case o.Kind == OwnerInstance:
		if o.Name != "" {
			v.err(ErrMutuallyExclusive, field+".name",
				fmt.Sprintf("owner.kind is %q and names principal %q", o.Kind, o.Name),
				"remove name, or set kind to "+OwnerUser+" or "+OwnerTeam+" — an instance-owned "+
					"document belongs to the instance and there is no principal to name")
		}
	case o.Name == "":
		v.err(ErrMissingRequired, field+".name",
			fmt.Sprintf("owner.kind is %q and names no principal", o.Kind),
			"set name to the "+o.Kind+" that owns this document, or set kind to "+OwnerInstance+
				" — an owner kind with no name owns nothing")
	}
}

// gitProviderNames lists the provider enum the way a remediation reads it.
func gitProviderNames() string {
	out := make([]string, 0, len(GitProviders))
	for _, p := range GitProviders {
		out = append(out, string(p))
	}
	return strings.Join(out, ", ")
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
	if kind.IsChart() {
		v.chartOverrideShape(field, ov, kind)
		return
	}
	if !kind.IsData() {
		if ov.Preset != "" {
			v.err(ErrMutuallyExclusive, field+".preset",
				fmt.Sprintf("component %q has kind %q, and a preset is the topology of a data component", ov.Name, kind),
				"remove preset; a workload is overridden with image, replicas, resources and env")
		}
		return
	}
	set := workloadOverrideFields(ov)
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

// chartOverrideShape refuses every per-environment override on a helm
// component, because there is not one it could apply.
//
// The override block carries image, replicas, resources, env and preset, and a
// chart uses none of them: what it installs is decided by the chart version and
// the values, which live on the Project component. Per-environment values are a
// real ask and deliberately not in v0 (ADR-0016 scopes the kind to the
// HelmRelease and its source); until they exist, an override that silently did
// nothing would be the failure issue #141 is about.
func (v *validator) chartOverrideShape(field string, ov ComponentOverride, kind ComponentKind) {
	set := workloadOverrideFields(ov)
	if ov.Preset != "" {
		set = append(set, "preset")
	}
	for _, f := range set {
		v.err(ErrMutuallyExclusive, field+"."+f,
			fmt.Sprintf("component %q has kind %q, which delegates a chart to helm-controller, but the override sets %s", ov.Name, kind, f),
			"remove "+f+"; a helm component has no per-environment overrides in v0 — the chart version and its "+
				"values live on the Project component, and an environment that needs different values needs its "+
				"own component")
	}
	if len(set) == 0 {
		v.err(ErrMissingRequired, field,
			fmt.Sprintf("the override for helm component %q sets nothing", ov.Name),
			"remove the block; there is nothing a helm component can be overridden with per environment")
	}
}

// workloadOverrideFields lists the workload override fields an Environment
// block actually sets, in spec order. Both kinds that reject them — data and
// helm — name every offending key at once rather than one per pass.
func workloadOverrideFields(ov ComponentOverride) []string {
	var set []string
	if ov.Image != "" {
		set = append(set, "image")
	}
	// The marker qualifies an image, so it belongs to exactly the kinds an
	// image does: a data component runs what its operator runs and a chart runs
	// what helm-controller installs, and neither has an image for tracking to
	// advance (ADR-0036 decision 5).
	if ov.ImageTracked {
		set = append(set, "imageTracked")
	}
	if ov.Replicas != nil {
		set = append(set, "replicas")
	}
	if ov.Resources != nil {
		set = append(set, "resources")
	}
	if len(ov.Env) > 0 {
		set = append(set, "env")
	}
	// Tracking a source is a workload question too (ADR-0036 decision 1): the
	// kinds that build nothing of ours are bound to no source, so there is no
	// push that could move one and the flag would resolve into nothing.
	if ov.AutoDeploy != nil {
		set = append(set, "autoDeploy")
	}
	return set
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
// plus every cross-document reference (component overrides, service
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
	v.protectedComponents("$.spec.policy", e.Spec.Policy, kinds, p.Metadata.Name)

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
	return v.errs
}

// ValidateGitConnection validates a GitConnection (ADR-0033). It is the third
// entry point beside [ValidateEnvironment] and [ValidateSet], and it takes one
// document because a connection has no cross-document half: what it references
// is a Secret, and what references it is a Project's source.connection, and
// neither is resolvable without a cluster.
func ValidateGitConnection(g *GitConnection) Errors {
	v := validator{resource: fmt.Sprintf("%s/%s", KindGitConnection, g.Metadata.Name), kind: KindGitConnection}
	validateGitConnection(g, &v)
	return v.errs
}

// ValidateGitSource validates a GitSource (ADR-0035 decision 2). It takes one
// document for the reason [ValidateGitConnection] does: what references a global
// source is a component's `source:` name, in a document this one has never heard
// of, and that binding is resolved where both are in hand rather than here.
func ValidateGitSource(g *GitSource) Errors {
	v := validator{resource: fmt.Sprintf("%s/%s", KindGitSource, g.Metadata.Name), kind: KindGitSource}
	validateGitSource(g, &v)
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
