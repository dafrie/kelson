package model

import (
	"fmt"
	"slices"
	"strings"
)

// effective.go — what a component actually runs with in one environment, and
// which block of which document decided each value (#260).
//
// [Resolve] already computes every value here; what it deliberately does not
// keep is *where each one came from*. A [ResolvedComponent] carries one image
// and one env map, merged, with no memory of the scope that won — which is
// correct for the renderer (a manifest does not vary by provenance) and useless
// to a reader trying to work out why `LOG_LEVEL` is `trace` in staging when the
// Project document says `info`.
//
// So this is a second walk over the same documents, recording the answer *and*
// the block that gave it. It is deliberately not a second precedence
// implementation with its own opinions: every chain below is the one in
// resolve.go, written in the same order, and effective_test.go asserts the two
// agree value-for-value on the fixture that exercises P1–P6 at once. If the two
// ever disagree, the test names the setting.
//
// Since #268 it records what *lost* as well ([EffectiveSetting.Shadowed]).
// Resolution cannot keep that — a merge that overwrites is the whole point of a
// merge — so the walk is the only place a losing value survives, and "the
// project says info, so why is this trace" is the question the table exists to
// answer. It is held to the same standard as the winner: only a block that
// actually wrote something at that field appears, the entries are the documents
// as authored, and the walk still changes no resolution anywhere.
//
// # Why this lives beside the resolver rather than in a browser
//
// Because a copy of a precedence merge in another language drifts, silently, in
// exactly the direction that makes a provenance claim wrong. The merge is
// kelson's answer, so kelson computes it and the wire carries the result
// (internal/api's GetEffectiveConfig).
//
// # What it does not cover
//
// Overlays (P6). An overlay is a patch applied to rendered resources, so what
// it changes is a manifest field and not a spec setting — there is no name and
// no scope chain to report, and inventing one would claim a merge that does not
// happen. Sources and bindings are likewise out: they are already answered per
// component by [Resolved.SourceFor], and nothing about a source reaches a
// workload's configuration.

// SetAtLevel names the block that set a value: which document, and which part
// of it.
//
// The vocabulary is the documents' own — a reader who knows the two files knows
// all five — rather than the rule numbers, which are a property of this
// codebase and not of the spec somebody wrote. A consumer renders a sentence
// from the level plus the names in [SetAt]; nothing here is a code to branch on
// twice.
type SetAtLevel string

const (
	// SetAtBuiltIn is kelson's own default: no document says it. `replicas:
	// {min: 1}`, `policy.agents: allow`, `secrets.backend: cluster`.
	SetAtBuiltIn SetAtLevel = "built-in"
	// SetAtProject is the Project document outside any component: `spec.image`,
	// `spec.env`, `spec.defaults`.
	SetAtProject SetAtLevel = "project"
	// SetAtComponent is this component's own entry in the Project document's
	// `spec.components` list.
	SetAtComponent SetAtLevel = "component"
	// SetAtEnvironment is the Environment document outside any override:
	// `spec.policy`, `spec.secrets`, `spec.routing`, `spec.autoDeploy`.
	SetAtEnvironment SetAtLevel = "environment"
	// SetAtEnvironmentComponent is this environment's override of this
	// component, in the Environment document's `spec.components` list — the
	// innermost scope of P1, P2, P3 and P5.
	SetAtEnvironmentComponent SetAtLevel = "environment-component"
)

// SetAt is one provenance answer: the level, the names that make it concrete,
// and the exact path within the document.
//
// Field is a JSONPath in the same spelling [Error.Field] uses, because it is
// the same claim about the same file: a consumer that can point at
// `$.spec.components[2].env.LOG_LEVEL` for a validation error can point at it
// here without a second convention.
type SetAt struct {
	Level SetAtLevel `json:"level"`
	// Document is [KindProject] or [KindEnvironment]; empty at
	// [SetAtBuiltIn], which is in no document.
	Document string `json:"document,omitempty"`
	// Environment is the environment's name, on the two environment levels.
	Environment string `json:"environment,omitempty"`
	// Component is the component whose block set the value, on the two
	// component levels.
	Component string `json:"component,omitempty"`
	// Field is where in that document the value is written. Empty at
	// [SetAtBuiltIn].
	Field string `json:"field,omitempty"`
}

// SettingGroup says what kind of setting an entry is.
//
// It exists because the names in one component's table come from two
// namespaces: an environment variable is named by its author and a workload
// setting by this model, so a project with an environment variable called
// `image` would otherwise produce two indistinguishable rows. A consumer
// separates them on the group and never on the name.
type SettingGroup string

const (
	// GroupEnv is an environment variable (rule P1).
	GroupEnv SettingGroup = "env"
	// GroupWorkload is a workload's own configuration: image, command,
	// replicas, resources, domains, tracking (rules P2 and P3).
	GroupWorkload SettingGroup = "workload"
	// GroupData is a data component's configuration: its preset (rule P5).
	GroupData SettingGroup = "data"
	// GroupEnvironment is an environment-scoped concern — policy and the secret
	// backend (rule P4). These belong to the environment rather than to any one
	// component.
	GroupEnvironment SettingGroup = "environment"
)

// Shadowed is one value the winner replaced: what a losing block held, and
// which block that was.
//
// It carries the same [EnvValue] the winner does, for the same reason: a
// shadowed `{secret, key}` is still a reference and flattening it on the way
// out would put a value where kelson has none (ADR-0009, ADR-0018).
type Shadowed struct {
	Value EnvValue `json:"value"`
	SetAt SetAt    `json:"setAt"`
}

// EffectiveSetting is one setting, its winning value, the block that set it,
// and the values that block replaced.
//
// The value is an [EnvValue] rather than a string so that a reference stays a
// reference all the way out: `{secret, key}` and `{from: {service, key}}` are
// what the spec carries and there is no value anywhere in kelson to put in
// their place (ADR-0009, ADR-0018). Settings that are not environment variables
// only ever use the literal arm.
type EffectiveSetting struct {
	Name  string       `json:"name"`
	Group SettingGroup `json:"group"`
	Value EnvValue     `json:"value"`
	SetAt SetAt        `json:"setAt"`
	// Shadowed are the values this setting's winner replaced, **outermost
	// first** — the order the merge applied them, so the list plus the winner
	// reads as the chain in one direction (#268).
	//
	// Only a block that actually held a value at that field appears. A scope
	// that said nothing about the setting is not a loser, it is a silence, and
	// a row for it would claim a merge that did not happen — which is the same
	// standard [SetAt] is held to.
	//
	// The winner is never in its own list.
	Shadowed []Shadowed `json:"shadowed,omitempty"`
}

// EffectiveComponent is one component's effective configuration.
type EffectiveComponent struct {
	Name string        `json:"name"`
	Kind ComponentKind `json:"kind"`
	// Settings are in reading order: environment variables sorted by key, then
	// the component's own configuration.
	//
	// A `kind: helm` component has none, and that is the honest answer rather
	// than an omission: no precedence rule reaches a chart, and an environment
	// takes no override on one (ADR-0016).
	Settings []EffectiveSetting `json:"settings,omitempty"`
}

// Effective is the effective configuration of one (Project, Environment) pair.
type Effective struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	// Settings are the environment-scoped concerns: the P4 chain over `policy:`
	// and `secrets:`. They are not per component because nothing about them is.
	Settings   []EffectiveSetting   `json:"settings,omitempty"`
	Components []EffectiveComponent `json:"components,omitempty"`
}

// ComponentNamed returns one component's effective configuration, or nil.
func (e *Effective) ComponentNamed(name string) *EffectiveComponent {
	for i := range e.Components {
		if e.Components[i].Name == name {
			return &e.Components[i]
		}
	}
	return nil
}

// Setting returns one of a component's settings by group and name, or nil. The
// group is part of the lookup for the reason [SettingGroup] exists.
func (c *EffectiveComponent) Setting(group SettingGroup, name string) *EffectiveSetting {
	for i := range c.Settings {
		if c.Settings[i].Group == group && c.Settings[i].Name == name {
			return &c.Settings[i]
		}
	}
	return nil
}

// The names this model gives its own settings. They are the spec's field names
// rather than prose, because a reader looking for `replicas` in a document
// should find the row that says `replicas`.
const (
	SettingImage           = "image"
	SettingCommand         = "command"
	SettingReplicas        = "replicas"
	SettingDomains         = "domains"
	SettingAutoDeploy      = "autoDeploy"
	SettingPreset          = "preset"
	SettingPolicyAgents    = "policy.agents"
	SettingPolicyRequire   = "policy.require"
	SettingSecretsBackend  = "secrets.backend"
	SettingSecretsStore    = "secrets.store"
	SettingSecretsRefresh  = "secrets.refreshInterval"
	SettingSecretsAgeKey   = "secrets.ageKeySecret"
	settingRequestsCPU     = "resources.requests.cpu"
	settingRequestsMemory  = "resources.requests.memory"
	settingLimitsCPU       = "resources.limits.cpu"
	settingLimitsMemory    = "resources.limits.memory"
	settingResourcesSuffix = "resources"
)

// EffectiveConfig validates the pair and returns its effective configuration
// with a provenance answer per setting. A pair that does not validate has no
// effective configuration, exactly as it has no resolution.
func EffectiveConfig(p *Project, e *Environment) (*Effective, Errors) {
	if errs := ValidateSet(p, e); len(errs) > 0 {
		return nil, errs
	}
	return effectiveConfig(p, e), nil
}

// effectiveConfig is the walk without the gate, for the same reason [resolve]
// is: the precedence over a field issue #141 has not un-gated yet is real
// behaviour, and it stays under test.
func effectiveConfig(p *Project, e *Environment) *Effective {
	out := &Effective{Project: p.Metadata.Name, Environment: e.Metadata.Name}
	out.Settings = environmentSettings(p, e)

	overrides := make(map[string]int, len(e.Spec.Components))
	for i, ov := range e.Spec.Components {
		overrides[ov.Name] = i
	}
	for i, c := range p.Spec.Components {
		w := walker{project: p, environment: e, component: c, componentIndex: i, overrideIndex: -1}
		if j, ok := overrides[c.Name]; ok {
			w.override, w.overrideIndex = e.Spec.Components[j], j
		}
		ec := EffectiveComponent{Name: c.Name, Kind: c.EffectiveKind()}
		switch kind := ec.Kind; {
		case kind.IsData():
			ec.Settings = w.dataSettings()
		case kind.IsChart():
			// Nothing: no rule reaches a chart, and a row claiming a default
			// would be a merge kelson does not perform.
		default:
			ec.Settings = w.workloadSettings()
		}
		out.Components = append(out.Components, ec)
	}
	return out
}

// walker holds the four scopes one component's settings are merged from, so the
// chains below read as one line per scope in precedence order.
type walker struct {
	project        *Project
	environment    *Environment
	component      Component
	componentIndex int
	override       ComponentOverride
	overrideIndex  int
}

// The four scopes as SetAt values. Each is built once per component rather than
// per setting, and each one's Field is completed by the caller.

func (w walker) atProject(field string) SetAt {
	return SetAt{Level: SetAtProject, Document: KindProject, Field: "$.spec." + field}
}

func (w walker) atComponent(field string) SetAt {
	return SetAt{
		Level:     SetAtComponent,
		Document:  KindProject,
		Component: w.component.Name,
		Field:     fmt.Sprintf("$.spec.components[%d].%s", w.componentIndex, field),
	}
}

func (w walker) atEnvironment(field string) SetAt {
	return SetAt{
		Level:       SetAtEnvironment,
		Document:    KindEnvironment,
		Environment: w.environment.Metadata.Name,
		Field:       "$.spec." + field,
	}
}

func (w walker) atOverride(field string) SetAt {
	return SetAt{
		Level:       SetAtEnvironmentComponent,
		Document:    KindEnvironment,
		Environment: w.environment.Metadata.Name,
		Component:   w.component.Name,
		Field:       fmt.Sprintf("$.spec.components[%d].%s", w.overrideIndex, field),
	}
}

func builtIn() SetAt { return SetAt{Level: SetAtBuiltIn} }

func literal(group SettingGroup, name, value string, at SetAt) EffectiveSetting {
	return EffectiveSetting{Name: name, Group: group, Value: EnvValue{Literal: value}, SetAt: at}
}

// held is one block's contribution to a chain: a value, and the block that
// writes it. A scope that says nothing about the setting contributes no held —
// which is what keeps a silence out of the shadow list.
type held struct {
	value EnvValue
	at    SetAt
}

func heldLiteral(value string, at SetAt) held { return held{value: EnvValue{Literal: value}, at: at} }

// chain turns the blocks that held a value, outermost first, into one setting:
// the innermost wins and the rest are its shadows, in the order they were
// applied. An empty chain has no setting at all — the caller decides whether
// that is a built-in or an absence, because those are different answers.
//
// This is the same order resolve.go applies the scopes in, written once, so a
// chain here cannot pick a different winner than the resolver does by picking a
// different *direction*.
func chain(group SettingGroup, name string, blocks []held) *EffectiveSetting {
	if len(blocks) == 0 {
		return nil
	}
	win := blocks[len(blocks)-1]
	out := EffectiveSetting{Name: name, Group: group, Value: win.value, SetAt: win.at}
	out.Shadowed = shadows(blocks[:len(blocks)-1])
	return &out
}

func shadows(blocks []held) []Shadowed {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]Shadowed, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, Shadowed{Value: b.value, SetAt: b.at})
	}
	return out
}

// workloadSettings is P1, P2 and P3 for one workload, plus the domain default
// and the two-level autoDeploy flag (ADR-0036 decision 1).
func (w walker) workloadSettings() []EffectiveSetting {
	out := w.envSettings()
	out = append(out, w.imageSetting()...)
	out = append(out, w.commandSetting()...)
	out = append(out, w.replicasSetting())
	out = append(out, w.resourceSettings()...)
	out = append(out, w.domainSettings()...)
	out = append(out, w.autoDeploySetting())
	return out
}

// envSettings is rule P1: project < component < environment override, key by
// key, with the innermost scope that names a key replacing the outer value
// wholesale.
//
// A key set at an outer scope and again at an inner one appears **once**,
// carrying the inner value and the inner block — the row still says what runs,
// and says it in one place. What the row now also carries is the chain it beat
// (#268): the outer values, in the order they were applied, each with the block
// that wrote it. That is a statement about the documents rather than about the
// container, so it cannot compete with the winner for the question "what is
// running" while it answers the one a reader actually arrives with — *why is it
// not what I wrote*.
func (w walker) envSettings() []EffectiveSetting {
	blocks := make(map[string][]held, len(w.project.Spec.Env))
	add := func(scope map[string]EnvValue, at func(string) SetAt) {
		for k, v := range scope {
			blocks[k] = append(blocks[k], held{value: v, at: at("env." + k)})
		}
	}
	add(w.project.Spec.Env, w.atProject)
	add(w.component.Env, w.atComponent)
	add(w.override.Env, w.atOverride)

	keys := make([]string, 0, len(blocks))
	for k := range blocks {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := make([]EffectiveSetting, 0, len(keys))
	for _, k := range keys {
		out = append(out, *chain(GroupEnv, k, blocks[k]))
	}
	return out
}

// imageSetting is rule P3: the environment's pin, else the component's image,
// else the Project's.
//
// A component that names none anywhere is reported with **no image setting at
// all**, and that is deliberate. [Resolve] gives such a component
// [ImageUnresolved] when it builds from source — a sentinel that must never
// reach a manifest (issue #136) and must not reach a table of what runs either,
// because "@" is not a value anybody set. The absence is the honest answer:
// nothing in these two documents names an image, and what will run comes from a
// build.
func (w walker) imageSetting() []EffectiveSetting {
	var blocks []held
	if w.project.Spec.Image != "" {
		blocks = append(blocks, heldLiteral(w.project.Spec.Image, w.atProject(SettingImage)))
	}
	if w.component.Image != "" {
		blocks = append(blocks, heldLiteral(w.component.Image, w.atComponent(SettingImage)))
	}
	if w.override.Image != "" {
		blocks = append(blocks, heldLiteral(w.override.Image, w.atOverride(SettingImage)))
	}
	if s := chain(GroupWorkload, SettingImage, blocks); s != nil {
		return []EffectiveSetting{*s}
	}
	return nil
}

// commandSetting is the other half of P3, and it is a one-scope chain: a
// component's command always wins because it is the only scope that has one —
// the Project has no command and an environment override does not set one.
func (w walker) commandSetting() []EffectiveSetting {
	if len(w.component.Command) == 0 {
		return nil
	}
	return []EffectiveSetting{literal(GroupWorkload, SettingCommand,
		strings.Join(w.component.Command, " "), w.atComponent(SettingCommand))}
}

// replicasSetting is half of rule P2: the environment override replaces the
// component's whole, and absent at both is the built-in `{min: 1}`.
//
// The block is replaced whole and reported as one row, so it shadows as one
// row: `2–4` shadowed by `5` says exactly what the merge did, and there is no
// per-field claim to get wrong.
func (w walker) replicasSetting() EffectiveSetting {
	var blocks []held
	if w.component.Replicas != nil {
		blocks = append(blocks, heldLiteral(replicaText(*w.component.Replicas), w.atComponent(SettingReplicas)))
	}
	if w.override.Replicas != nil {
		blocks = append(blocks, heldLiteral(replicaText(*w.override.Replicas), w.atOverride(SettingReplicas)))
	}
	if s := chain(GroupWorkload, SettingReplicas, blocks); s != nil {
		return *s
	}
	return literal(GroupWorkload, SettingReplicas, replicaText(Replicas{Min: 1}), builtIn())
}

// replicaText says a replica count the way the model means it: a fixed count,
// or a range when Max exceeds Min (autoscaling between the bounds).
func replicaText(r Replicas) string {
	if r.Max > r.Min {
		return fmt.Sprintf("%d–%d", r.Min, r.Max)
	}
	return fmt.Sprintf("%d", r.Min)
}

// resourceSettings is the other half of rule P2. The block is replaced whole —
// there is no deep merge of requests against limits — so every field reported
// here carries the same block as its provenance, which is the fact a reader
// needs: overriding `resources:` for an environment drops whatever the
// component set and did not restate.
//
// Nothing is reported when neither scope sets the block. "No requests and no
// limits" is the built-in and it is an absence rather than a value; four rows
// of nothing would be four rows a reader has to read.
// The shadow follows the same attribution. The **block** is what was replaced,
// so the block the override displaced is what shadows — reported on the rows
// the winning block produced, at the same leaf inside the losing block, and
// only where that block actually wrote that leaf. Nothing is invented in either
// direction: a leaf the winning block does not set has no row to hang a shadow
// on, so a quantity the component set and the environment did not restate is
// absent here exactly as it is absent from the container.
func (w walker) resourceSettings() []EffectiveSetting {
	res, at := w.component.Resources, w.atComponent(settingResourcesSuffix)
	var lost *Resources
	var lostAt SetAt
	if w.override.Resources != nil {
		lost, lostAt = w.component.Resources, at
		res, at = w.override.Resources, w.atOverride(settingResourcesSuffix)
	}
	if res == nil {
		return nil
	}
	var out []EffectiveSetting
	add := func(name, field string) {
		value := resourceQuantity(res, field)
		if value == "" {
			return
		}
		set := at
		set.Field += field
		s := literal(GroupWorkload, name, value, set)
		if before := resourceQuantity(lost, field); before != "" {
			shadowAt := lostAt
			shadowAt.Field += field
			s.Shadowed = []Shadowed{{Value: EnvValue{Literal: before}, SetAt: shadowAt}}
		}
		out = append(out, s)
	}
	add(settingRequestsCPU, ".requests.cpu")
	add(settingRequestsMemory, ".requests.memory")
	add(settingLimitsCPU, ".limits.cpu")
	add(settingLimitsMemory, ".limits.memory")
	return out
}

// resourceQuantity reads one leaf of a resources block by the same suffix the
// row's Field is built from, so the row and its shadow cannot point at
// different quantities.
func resourceQuantity(res *Resources, field string) string {
	if res == nil {
		return ""
	}
	switch field {
	case ".requests.cpu":
		if res.Requests != nil {
			return res.Requests.CPU
		}
	case ".requests.memory":
		if res.Requests != nil {
			return res.Requests.Memory
		}
	case ".limits.cpu":
		if res.Limits != nil {
			return res.Limits.CPU
		}
	case ".limits.memory":
		if res.Limits != nil {
			return res.Limits.Memory
		}
	}
	return ""
}

// domainSettings is the domain defaulting docs/model.md resolves beside the
// precedence rules: explicit component domains win, and a component with a port
// and no domains takes `<component>.<domainSuffix>` from the environment's
// routing block.
//
// The derived host's provenance is the *environment's routing block*, because
// that is the thing a reader edits to change it — the component says nothing
// about its hostname at all.
func (w walker) domainSettings() []EffectiveSetting {
	if len(w.component.Domains) > 0 {
		return []EffectiveSetting{literal(GroupWorkload, SettingDomains,
			strings.Join(w.component.Domains, ", "), w.atComponent(SettingDomains))}
	}
	suffix := ""
	if r := w.environment.Spec.Routing; r != nil {
		suffix = r.DomainSuffix
	}
	if w.component.Port == 0 || suffix == "" {
		return nil
	}
	host := fmt.Sprintf("%s.%s", w.component.Name, strings.TrimPrefix(suffix, "."))
	return []EffectiveSetting{literal(GroupWorkload, SettingDomains, host, w.atEnvironment("routing.domainSuffix"))}
}

// autoDeploySetting is ADR-0036 decision 1: the component's override if it
// declared one, else the environment's, else false.
//
// It is always reported, including when it is the built-in false, because "this
// component does not follow its source" is the answer to a question a reader
// has — and an absent row would read as "kelson did not say" on the one setting
// whose whole point is that both levels are legible at once.
func (w walker) autoDeploySetting() EffectiveSetting {
	var blocks []held
	if w.environment.Spec.AutoDeploy != nil {
		blocks = append(blocks, heldLiteral(boolText(*w.environment.Spec.AutoDeploy), w.atEnvironment(SettingAutoDeploy)))
	}
	if w.override.AutoDeploy != nil {
		blocks = append(blocks, heldLiteral(boolText(*w.override.AutoDeploy), w.atOverride(SettingAutoDeploy)))
	}
	if s := chain(GroupWorkload, SettingAutoDeploy, blocks); s != nil {
		return *s
	}
	return literal(GroupWorkload, SettingAutoDeploy, boolText(false), builtIn())
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// dataSettings is rule P5: the environment's preset override, else the
// component's own, else the built-in `shared`.
func (w walker) dataSettings() []EffectiveSetting {
	var blocks []held
	if w.component.Preset != "" {
		blocks = append(blocks, heldLiteral(string(w.component.Preset), w.atComponent(SettingPreset)))
	}
	if w.override.Preset != "" {
		blocks = append(blocks, heldLiteral(string(w.override.Preset), w.atOverride(SettingPreset)))
	}
	if s := chain(GroupData, SettingPreset, blocks); s != nil {
		return []EffectiveSetting{*s}
	}
	return []EffectiveSetting{literal(GroupData, SettingPreset, string(PresetShared), builtIn())}
}

// environmentSettings is rule P4: an explicit Environment block wins whole over
// the Project's `defaults` block, which wins whole over the built-in.
//
// "Whole" is the part that surprises people and the part this reports honestly.
// An environment that writes `policy: {require: [dry-run]}` and says nothing
// about `agents:` does not inherit the project's `agents:` — the project's
// block is gone, and the effective `agents` is the *built-in* `allow`. So the
// two fields of one block can carry two different levels, and each row says
// which.
// This is also where a shadow is at its most useful and at its most easily
// wrong, so it is stated as a block and never as a field: what the Environment
// displaced is the Project's whole `defaults` block, and each row reports what
// that block held **at that row's own field**, or nothing when it held nothing
// there. The `agents` case is the one to read twice — the winner is the
// *built-in* and the shadow is the project's `propose-only`, which is exactly
// the surprise the rule produces and the only place a table can say it.
//
// A field the winning block leaves unset and the displaced one wrote produces
// no row at all, here as before: an entry whose winner does not exist would be
// a setting kelson invented.
func environmentSettings(p *Project, e *Environment) []EffectiveSetting {
	var out []EffectiveSetting

	// policy: whichever block won, then the built-in for the fields it left
	// unset. The blocks are collected outermost first so the last is the winner
	// and the rest are what it displaced.
	type policyBlock struct {
		policy Policy
		at     SetAt
	}
	var policyBlocks []policyBlock
	if d := p.Spec.Defaults; d != nil && d.Policy != nil {
		policyBlocks = append(policyBlocks, policyBlock{*d.Policy,
			SetAt{Level: SetAtProject, Document: KindProject, Field: "$.spec.defaults.policy"}})
	}
	if e.Spec.Policy != nil {
		policyBlocks = append(policyBlocks, policyBlock{*e.Spec.Policy, SetAt{
			Level: SetAtEnvironment, Document: KindEnvironment,
			Environment: e.Metadata.Name, Field: "$.spec.policy",
		}})
	}
	policy, policyAt := Policy{}, builtIn()
	var displacedPolicy []policyBlock
	if n := len(policyBlocks); n > 0 {
		policy, policyAt = policyBlocks[n-1].policy, policyBlocks[n-1].at
		displacedPolicy = policyBlocks[:n-1]
	}
	policyShadows := func(field string, read func(Policy) string) []Shadowed {
		var shadowed []Shadowed
		for _, b := range displacedPolicy {
			value := read(b.policy)
			if value == "" {
				continue
			}
			at := b.at
			at.Field += field
			shadowed = append(shadowed, Shadowed{Value: EnvValue{Literal: value}, SetAt: at})
		}
		return shadowed
	}

	agents, agentsAt := policy.Agents, policyAt
	if agents == "" {
		agents, agentsAt = AgentsAllow, builtIn()
	} else {
		agentsAt.Field += ".agents"
	}
	agentsRow := literal(GroupEnvironment, SettingPolicyAgents, string(agents), agentsAt)
	agentsRow.Shadowed = policyShadows(".agents", func(pl Policy) string { return string(pl.Agents) })
	out = append(out, agentsRow)
	if len(policy.Require) > 0 {
		at := policyAt
		at.Field += ".require"
		requireRow := literal(GroupEnvironment, SettingPolicyRequire, strings.Join(policy.Require, ", "), at)
		requireRow.Shadowed = policyShadows(".require", func(pl Policy) string { return strings.Join(pl.Require, ", ") })
		out = append(out, requireRow)
	}

	// secrets: the same chain, then the two defaults resolution fills in.
	type secretsBlock struct {
		secrets SecretBackend
		at      SetAt
	}
	var secretsBlocks []secretsBlock
	if d := p.Spec.Defaults; d != nil && d.Secrets != nil {
		secretsBlocks = append(secretsBlocks, secretsBlock{*d.Secrets,
			SetAt{Level: SetAtProject, Document: KindProject, Field: "$.spec.defaults.secrets"}})
	}
	if e.Spec.Secrets != nil {
		secretsBlocks = append(secretsBlocks, secretsBlock{*e.Spec.Secrets, SetAt{
			Level: SetAtEnvironment, Document: KindEnvironment,
			Environment: e.Metadata.Name, Field: "$.spec.secrets",
		}})
	}
	secrets, secretsAt := SecretBackend{Backend: SecretsCluster}, builtIn()
	var displacedSecrets []secretsBlock
	if n := len(secretsBlocks); n > 0 {
		secrets, secretsAt = secretsBlocks[n-1].secrets, secretsBlocks[n-1].at
		displacedSecrets = secretsBlocks[:n-1]
	}
	secretsShadows := func(field string, read func(SecretBackend) string) []Shadowed {
		var shadowed []Shadowed
		for _, b := range displacedSecrets {
			value := read(b.secrets)
			if value == "" {
				continue
			}
			at := b.at
			at.Field += field
			shadowed = append(shadowed, Shadowed{Value: EnvValue{Literal: value}, SetAt: at})
		}
		return shadowed
	}

	backendAt := secretsAt
	if backendAt.Level != SetAtBuiltIn {
		backendAt.Field += ".backend"
	}
	backendRow := literal(GroupEnvironment, SettingSecretsBackend, string(secrets.Backend), backendAt)
	backendRow.Shadowed = secretsShadows(".backend", func(s SecretBackend) string { return string(s.Backend) })
	out = append(out, backendRow)
	if secrets.Store != "" {
		at := secretsAt
		at.Field += ".store"
		storeRow := literal(GroupEnvironment, SettingSecretsStore, secrets.Store, at)
		storeRow.Shadowed = secretsShadows(".store", func(s SecretBackend) string { return s.Store })
		out = append(out, storeRow)
	}
	// The two fields resolution defaults are reported with the level that
	// actually decided them, which is the built-in whenever the block was
	// silent — the same distinction the manifest makes by writing the value out
	// rather than leaving it implicit.
	if secrets.Backend == SecretsExternalSecrets {
		interval, at := secrets.RefreshInterval, secretsAt
		if interval == "" {
			interval, at = DefaultSecretRefreshInterval, builtIn()
		} else {
			at.Field += ".refreshInterval"
		}
		refreshRow := literal(GroupEnvironment, SettingSecretsRefresh, interval, at)
		refreshRow.Shadowed = secretsShadows(".refreshInterval", func(s SecretBackend) string { return s.RefreshInterval })
		out = append(out, refreshRow)
	}
	if secrets.Backend == SecretsSOPS {
		name, at := secrets.AgeKeySecret, secretsAt
		if name == "" {
			name, at = DefaultAgeKeySecret, builtIn()
		} else {
			at.Field += ".ageKeySecret"
		}
		ageKeyRow := literal(GroupEnvironment, SettingSecretsAgeKey, name, at)
		ageKeyRow.Shadowed = secretsShadows(".ageKeySecret", func(s SecretBackend) string { return s.AgeKeySecret })
		out = append(out, ageKeyRow)
	}
	return out
}
