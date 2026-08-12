package diff

type Level string
const (
	LevelRendered Level = "rendered" // L1, no cluster contacted (#42)
	LevelServer   Level = "server"   // L2, the API server's own verdict (#43)
)

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

type Diff struct {
	Level          Level             `json:"level"`
	Project        string            `json:"project"`
	Environment    string            `json:"environment"`
	Resources      []ResourceDiff    `json:"resources"`
	Violations     []PolicyViolation `json:"violations,omitempty"`
	Summary        Summary           `json:"summary"`
	Degraded       bool              `json:"degraded,omitempty"`       // L2 requested but unavailable, fell back to L1 (#43)
	DegradedReason string            `json:"degradedReason,omitempty"`
}

type ResourceDiff struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace,omitempty"`
	Op         Op          `json:"op"`
	Risk       Risk        `json:"risk"`
	Fields     []FieldDiff `json:"fields,omitempty"`
}

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
