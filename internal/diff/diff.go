package diff

// Level says how faithful the diff is to what the cluster would actually do.
// L1 diffs rendered manifests without contacting the cluster; L2 diffs the
// API server's own dry-run verdict (issue #43).
type Level string

const (
	LevelRendered Level = "rendered" // L1, no cluster contacted (#42)
	LevelServer   Level = "server"   // L2, the API server's own verdict (#43)
)

// Op is the resource-level change shape.
type Op string

const (
	OpAdded    Op = "added"
	OpModified Op = "modified"
	OpRemoved  Op = "removed"
)

// Risk is the typed answer to "is this disruptive?" — agents branch on this
// field and must never parse prose (#44 acceptance criterion).
type Risk string

const (
	RiskCosmetic   Risk = "cosmetic"         // nothing observable changes
	RiskAdditive   Risk = "additive"         // creates something new, running workloads untouched
	RiskRestart    Risk = "restart-required" // pods roll, workload stays available
	RiskDisruptive Risk = "disruptive"       // downtime, data loss, or a write the API server will reject
)

// Origin says who authored a field change, so an injected sidecar never looks
// like a user edit (#29, #43).
type Origin string

const (
	OriginSpec       Origin = "spec"
	OriginOverlay    Origin = "overlay"
	OriginAdmission  Origin = "admission"  // L2 only
	OriginDefaulting Origin = "defaulting" // L2 only
)

// Enforcement keeps audit-mode findings from being reported as blockers (#45).
type Enforcement string

const (
	EnforcementEnforce Enforcement = "enforce"
	EnforcementAudit   Enforcement = "audit"
)

// Diff is the uniform, machine-readable result of one render-vs-render or
// render-vs-live comparison. A single representation consumed by the CLI, the
// UI and agents (issue #44) — rendering differs per client; the data must not.
type Diff struct {
	Level       Level             `json:"level"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Resources   []ResourceDiff    `json:"resources"`
	Violations  []PolicyViolation `json:"violations,omitempty"`
	// Unvalidated lists resources the preview could not evaluate. It is
	// separate from Violations because "we checked this and it fails" and "we
	// could not check this" are different answers, and a preview that blurs
	// them is not trustworthy (#43).
	Unvalidated []Unvalidated `json:"unvalidated,omitempty"`
	Summary     Summary       `json:"summary"`
	// Degraded is set when L2 was requested but the live cluster was
	// unavailable, falling back to L1. Agents and the UI can then show a
	// best-effort preview instead of a hard error (#43).
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degradedReason,omitempty"`
}

// Unvalidated is a resource the API server could not evaluate, because a
// prerequisite it depends on does not exist: a custom resource before its CRD,
// anything before its Namespace (#43).
//
// This is deliberately not a PolicyViolation. Nothing rejected the resource —
// the question was never asked. Reporting it as a policy finding would make an
// agent branching on Violations see a policy that does not exist, and reporting
// nothing at all would let a resource slip through the preview unchecked.
//
// It is the preview's instance of the tri-state discipline documented once in
// internal/clusterprofile: checked-and-passing, checked-and-failing, and
// could-not-check are three answers, never two. It keeps its own shape rather
// than the shared clusterprofile.Outcome because it is not a three-valued
// field — it is a per-resource record naming the missing prerequisite, on a
// serialized contract the CLI, the UI (ui/src/diff/parse.ts) and agents parse.
// Folding it into the enum would move a wire format to make two internal types
// look alike (issue #144).
type Unvalidated struct {
	// Resource is apiVersion/Kind/namespace/name.
	Resource string `json:"resource"`
	// Requires names the missing prerequisite, e.g. "Namespace/checkout".
	Requires string `json:"requires,omitempty"`
	// InBatch distinguishes the benign case from the real one. True means the
	// prerequisite is created by this same batch, so applying in the renderer's
	// order resolves it and the resource is expected to validate. False means
	// the prerequisite is genuinely absent and the apply would fail too — the
	// resource carries a disruptive ResourceDiff so a CI gate still trips.
	InBatch bool `json:"inBatch"`
	// Message is the API server's own explanation.
	Message string `json:"message,omitempty"`
}

// ResourceDiff is one Kubernetes resource's change, plus the field-level
// detail an agent or human can act on.
type ResourceDiff struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace,omitempty"`
	Op         Op          `json:"op"`
	Risk       Risk        `json:"risk"`
	Fields     []FieldDiff `json:"fields,omitempty"`
}

// FieldDiff is one changed field within a resource. Added/removed/modified is
// encoded by which of Before/After is present: both on a modification, only
// Before on a removal, only After on an addition.
type FieldDiff struct {
	// Path is the field path in the rendered resource, e.g.
	// spec.template.spec.containers[0].env[DATABASE_URL].
	Path   string `json:"path"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
	Origin Origin `json:"origin"`
	Risk   Risk   `json:"risk"`
	// SpecPath links back to the authoring-plane field the user wrote, where
	// one exists, so output points at kelson.yaml and not only at the
	// rendered resource.
	SpecPath string `json:"specPath,omitempty"`
}

// PolicyViolation is a machine-readable admission-policy finding, surfaced at
// preview time so a human sees the policy name and offending field before
// apply, not an opaque apply-time failure (issue #45, epic exit criteria).
type PolicyViolation struct {
	Engine      string      `json:"engine"` // kyverno, gatekeeper, validating-admission-policy
	Policy      string      `json:"policy"`
	Rule        string      `json:"rule,omitempty"`
	Resource    string      `json:"resource"`
	Path        string      `json:"path,omitempty"`
	SpecPath    string      `json:"specPath,omitempty"`
	Message     string      `json:"message"`
	Enforcement Enforcement `json:"enforcement"`
}

// Summary is the blast-radius roll-up a CI gate or a quick human scan reads
// without parsing every resource.
type Summary struct {
	Added      int      `json:"added"`
	Modified   int      `json:"modified"`
	Removed    int      `json:"removed"`
	Restarting []string `json:"restarting,omitempty"` // workloads that will roll
	Disruptive []string `json:"disruptive,omitempty"`
	// MaxRisk is the highest risk across all resources — the single field a
	// CI gate branches on.
	MaxRisk Risk `json:"maxRisk"`
}
