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

// UnvalidatedReason is the machine-branchable class of "could not check".
// Two families live here and they must not be confused: the API server was
// asked and could not answer for this resource (a missing prerequisite, a
// rejection in a shape preview cannot attribute), and the preview knows a check
// never ran at all (an admission webhook the dry-run request does not reach).
// Only the first family says anything about whether the apply would fail
// (#43, #45).
type UnvalidatedReason string

const (
	// ReasonMissingPrerequisite: the resource depends on something that does
	// not exist yet — a custom resource before its CRD, anything before its
	// Namespace. InBatch says whether this batch creates it.
	ReasonMissingPrerequisite UnvalidatedReason = "missing-prerequisite"
	// ReasonUnattributedRejection: the API server rejected the dry-run in a
	// shape no recogniser matched. The apply would be rejected too; preview
	// simply cannot name the culprit, and must not invent one (#45).
	ReasonUnattributedRejection UnvalidatedReason = "unattributed-rejection"
	// ReasonDryRunUnsupported: an admission webhook declares side effects it
	// cannot suppress, so the API server refuses to run it for a dry-run
	// request. Nothing rejected the change — it was never checked (#45).
	ReasonDryRunUnsupported UnvalidatedReason = "dry-run-unsupported"
	// ReasonWebhookExcludesDryRun: the cluster registers a validating webhook
	// that a dry-run request does not reach, and whose failure policy lets the
	// API server skip it silently. The preview is honest about the hole rather
	// than reporting a coverage it does not have (#45).
	ReasonWebhookExcludesDryRun UnvalidatedReason = "webhook-excludes-dry-run"
)

// Unvalidated is a resource the API server could not evaluate, because a
// prerequisite it depends on does not exist: a custom resource before its CRD,
// anything before its Namespace (#43). It is also how the preview admits a
// check that never ran — an admission webhook a dry-run request does not
// reach (#45); Reason says which.
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
	// Resource is apiVersion/Kind/namespace/name. For a coverage gap it names
	// the admission webhook that the preview cannot reach instead, because that
	// is the object to go and read.
	Resource string `json:"resource"`
	// Requires names what was missing, e.g. "Namespace/checkout" for a
	// prerequisite or "admission webhook x.y.z" for a check the dry-run could
	// not run.
	Requires string `json:"requires,omitempty"`
	// InBatch distinguishes the benign case from the real one. True means the
	// prerequisite is created by this same batch, so applying in the renderer's
	// order resolves it and the resource is expected to validate. False means
	// the prerequisite is genuinely absent and the apply would fail too — the
	// resource carries a disruptive ResourceDiff so a CI gate still trips.
	//
	// It answers a question only ReasonMissingPrerequisite asks. Use Blocking()
	// rather than reading this field to decide whether an entry is a blocker.
	InBatch bool `json:"inBatch"`
	// Reason is the machine-branchable class of the gap. Empty on entries
	// written before the reasons existed, which Blocking() treats as the
	// prerequisite case they were.
	Reason UnvalidatedReason `json:"reason,omitempty"`
	// Message is the API server's own explanation.
	Message string `json:"message,omitempty"`
}

// Blocked reports whether a preview is a blocker: an admission policy or
// webhook denied the change, the API server's own validation rejected it, or a
// resource could not be evaluated for a reason the apply would hit too.
//
// It is the single predicate behind `kelson diff`'s exit code 3 and the Diff
// RPC's exit_semantics 3. Both call it rather than restating it, because a gate
// running the CLI and a gate calling the RPC must assert the same thing about
// the same preview (#46).
func Blocked(d *Diff) bool {
	if d == nil {
		return false
	}
	for _, v := range d.Violations {
		if v.Enforcement == EnforcementEnforce {
			return true
		}
	}
	for _, u := range d.Unvalidated {
		if u.Blocking() {
			return true
		}
	}
	return false
}

// Blocking reports whether an Unvalidated entry is a blocker — the single
// question the CI exit contract (exit 3) branches on, kept here so the CLI and
// the API cannot drift apart on it.
//
// "We asked and the answer was no" blocks. "We could not ask" does not: a
// webhook the dry-run never reaches has not rejected anything, and failing CI
// on it would punish a coverage gap as if it were a verdict. Those entries are
// still reported, so an incomplete preview never reads as a clean one.
func (u Unvalidated) Blocking() bool {
	switch u.Reason {
	case ReasonDryRunUnsupported, ReasonWebhookExcludesDryRun:
		return false
	default:
		return !u.InBatch
	}
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

// ViolationCode is the stable class of a finding, the field an agent branches
// on instead of matching on Engine strings or parsing Message prose. It shares
// the shape of the delivery-plane error codes (`<domain>/<class>`) so one
// vocabulary reads across the surfaces (#28, #32, #45).
type ViolationCode string

const (
	// CodeWebhookDenied: a validating admission webhook denied the request —
	// Kyverno, Gatekeeper/OPA, or any other webhook registered on the cluster.
	// Policy carries the policy, constraint or webhook name the server named.
	CodeWebhookDenied ViolationCode = "policy/webhook-denied"
	// CodeAdmissionPolicyDenied: a built-in ValidatingAdmissionPolicy denied
	// the request. Policy is the policy, Rule the binding.
	CodeAdmissionPolicyDenied ViolationCode = "policy/admission-policy-denied"
	// CodeValidationFailed: the API server's own validation rejected the
	// object — an immutable field, a schema violation. No policy is involved.
	CodeValidationFailed ViolationCode = "policy/validation-failed"
	// CodeAuditFinding: read from a policy report, not from a rejection. The
	// request was not vetoed, so it is a warning and never a blocker.
	CodeAuditFinding ViolationCode = "policy/audit-finding"
)

// PolicyViolation is a machine-readable admission-policy finding, surfaced at
// preview time so a human sees the policy name and offending field before
// apply, not an opaque apply-time failure (issue #45, epic exit criteria).
//
// Message is the server's own words, verbatim. Preview never paraphrases a
// rejection: the policy author wrote that sentence for exactly this moment, and
// a rewritten one would be kelson guessing at intent it does not have.
type PolicyViolation struct {
	Code        ViolationCode `json:"code,omitempty"`
	Engine      string        `json:"engine"` // kyverno, gatekeeper, validating-admission-policy, validating-webhook, kubernetes
	Policy      string        `json:"policy"`
	Rule        string        `json:"rule,omitempty"`
	Resource    string        `json:"resource"`
	Path        string        `json:"path,omitempty"`
	SpecPath    string        `json:"specPath,omitempty"`
	Message     string        `json:"message"`
	Enforcement Enforcement   `json:"enforcement"`
	// Remediation names the cluster object to go and read — the policy,
	// constraint or webhook configuration that produced this finding — so the
	// answer to "which policy is this?" is in the finding, not in the reader's
	// head (#28's structured-error shape).
	Remediation string `json:"remediation,omitempty"`
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
