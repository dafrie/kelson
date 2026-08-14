// Package explain answers "why is this application degraded?" with structured,
// causal data rather than a log dump for a model to guess at (issue #77,
// ADR-0023).
//
// # What it is
//
// One function — [Explain] — turns what the other planes already decided into
// an [Explanation]: a phase, a one-line summary, an ordered list of [Cause]
// values each carrying a stable code, a [Confidence], the [Evidence] behind it,
// a remediation where one is determinable, and the revision that introduced the
// change it blames. Plus the recent change itself, so "what is broken" and
// "what changed" arrive in the same answer.
//
// # What it deliberately is not
//
// It is not a second observation plane. Every health judgement here comes from
// an [observation.Verdict] that was computed elsewhere; explain adds evidence,
// correlation and confidence on top and never re-derives a code. It holds no
// Kubernetes client and cannot: outside the cluster-facing planes .golangci.yml
// allows this package stdlib, kelson, jsonschema, cobra and yaml.v3 only, which
// is the mechanical enforcement of that rule.
//
// Everything cluster-shaped therefore arrives as an input or a function seam:
// the adapter's [delivery.Status], the verdicts, the delivery history, the
// optional L2 policy violations, a bounded log window ([LogFn]) and the
// recorded rendered manifests of a revision ([ManifestFn]). Both seams are
// optional; a caller that has neither still gets every cause the verdicts and
// the status support, and the answer says in [Explanation.Notes] what could not
// be read.
//
// # Confidence is a rule
//
// [ConfidenceHigh] means a controller named the reason (an exact kubelet
// waiting reason, a scheduler message, an external-secrets condition, an API
// server rejection) or two independent signals agreed. [ConfidenceMedium] means
// one signal, or a heuristic the producing plane itself calls one — and the
// message says so in words. [ConfidenceLow] is an audit-mode finding: recorded,
// and not what vetoed anything. A cause with no direct evidence is never high,
// and a candidate with no signal at all is not emitted.
//
// # Bounded, always
//
// The answer is written to be pasted into a context window whole:
// [MaxCauses] causes, [MaxEvidence] evidence items each, [MaxLogLines] lines per
// excerpt at [MaxLineBytes] bytes, and [MaxBytes] for the whole thing. Every
// trim is recorded in [Explanation.Truncated] rather than performed silently,
// because a truncated answer read as a complete one produces a wrong conclusion
// from a correct report.
package explain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
)

// Confidence is how much the evidence supports a cause. It is a typed value a
// caller gates on — an agent may act on high, must verify medium, and reads low
// as context — rather than an adjective in a sentence.
type Confidence string

const (
	// ConfidenceHigh: a controller named the reason, or two independent
	// signals agree.
	ConfidenceHigh Confidence = "high"
	// ConfidenceMedium: one signal, or a heuristic the producing plane already
	// calls one. The cause's message states the correlation as a correlation.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceLow: a finding that is real but did not veto anything, such as
	// an audit-mode policy report.
	ConfidenceLow Confidence = "low"
)

// Code is the stable class of a cause, in the `<domain>/<class>` shape the
// delivery error and policy violation taxonomies already use (#28, #45). Codes
// are a compatibility promise: an existing code never changes meaning.
type Code string

const (
	// CodeMissingEnvVar: a container is failing and an environment variable it
	// needs is absent — removed, renamed, or repointed at a Secret key — as of
	// the live revision. This is issue #77's acceptance case.
	CodeMissingEnvVar Code = "explain/missing-env-var"
	// CodeCrashLoop: the container starts and dies repeatedly.
	CodeCrashLoop Code = "explain/crash-loop"
	// CodeOOMKilled: the kernel killed the container for exceeding its memory
	// limit.
	CodeOOMKilled Code = "explain/oom-killed"
	// CodeEvicted: the kubelet evicted the pod under node pressure.
	CodeEvicted Code = "explain/evicted"
	// CodeImagePull: the image cannot be pulled — a wrong name or tag, or a
	// registry that refused.
	CodeImagePull Code = "explain/image-pull"
	// CodeFailingProbe: a readiness or liveness probe is not passing.
	CodeFailingProbe Code = "explain/failing-probe"
	// CodeInsufficientResources: unschedulable because the cluster lacks the
	// requested CPU or memory.
	CodeInsufficientResources Code = "explain/insufficient-resources"
	// CodeUnschedulable: unschedulable for a non-resource reason — a node
	// selector, an affinity rule, a taint.
	CodeUnschedulable Code = "explain/unschedulable"
	// CodeSecretSyncFailed: external-secrets could not sync a Secret the
	// workloads reference (issue #80, ADR-0020).
	CodeSecretSyncFailed Code = "explain/secret-sync-failed"
	// CodeWorkloadMissing: the workload kelson rendered is not in the cluster.
	CodeWorkloadMissing Code = "explain/workload-missing"
	// CodePolicyRejected: an admission policy or webhook refused the change
	// (issue #45).
	CodePolicyRejected Code = "explain/policy-rejected"
	// CodeDecryptionFailed: the reconciler could not decrypt the SOPS-encrypted
	// manifests (ADR-0022).
	CodeDecryptionFailed Code = "explain/decryption-failed"
	// CodeReconcilerNotReady: the reconciler processed the change and is not
	// ready — it refused it, or applied it and reports it unhealthy.
	CodeReconcilerNotReady Code = "explain/reconciler-not-ready"
	// CodeReleaseJobFailed: the release command hook's Job failed or ran out of
	// time, so the workloads of this revision were never applied (ADR-0019).
	CodeReleaseJobFailed Code = "explain/release-job-failed"
	// CodeReleaseJobPending: the release command hook's Job is still running
	// and the workloads wait behind it (issue #104).
	CodeReleaseJobPending Code = "explain/release-job-pending"
)

// rank orders causes most-answering first, so a bounded answer keeps the cause
// and drops the symptom rather than the other way round. It is also what makes
// the output deterministic: two identical inputs produce byte-identical causes.
func (c Code) rank() int {
	switch c {
	case CodeDecryptionFailed:
		return 1
	case CodePolicyRejected:
		return 2
	case CodeReconcilerNotReady:
		return 3
	case CodeReleaseJobFailed:
		return 4
	case CodeSecretSyncFailed:
		return 5
	case CodeMissingEnvVar:
		return 6
	case CodeImagePull:
		return 7
	case CodeOOMKilled:
		return 8
	case CodeEvicted:
		return 9
	case CodeInsufficientResources:
		return 10
	case CodeUnschedulable:
		return 11
	case CodeCrashLoop:
		return 12
	case CodeFailingProbe:
		return 13
	case CodeWorkloadMissing:
		return 14
	case CodeReleaseJobPending:
		return 15
	default:
		return 99
	}
}

// EvidenceKind says what a piece of evidence *is*, so a reader (or a UI) can
// tell a quoted controller condition from a field reference from the
// application's own output without parsing the text.
type EvidenceKind string

const (
	// EvidenceCondition is a controller's own words: a kubelet reason, a
	// scheduler message, a Flux condition, an external-secrets reason. Relayed
	// verbatim — the controller author wrote that sentence for this moment.
	EvidenceCondition EvidenceKind = "condition"
	// EvidenceLog is a bounded excerpt of the workload's own output.
	EvidenceLog EvidenceKind = "log"
	// EvidenceField is a reference into the rendered manifest, e.g.
	// spec.template.spec.containers[0].env[DATABASE_URL].
	EvidenceField EvidenceKind = "field"
	// EvidenceRevision is a change between two recorded revisions.
	EvidenceRevision EvidenceKind = "revision"
)

// Evidence is one checkable fact behind a cause. Source names where it came
// from and Detail is the bounded text itself.
type Evidence struct {
	Kind   EvidenceKind `json:"kind"`
	Source string       `json:"source,omitempty"`
	Detail string       `json:"detail"`
}

// RevisionRef identifies one recorded revision, enough to find it in the
// history without a second call.
type RevisionRef struct {
	Revision    string `json:"revision"`
	CommittedAt string `json:"committedAt,omitempty"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
}

// Cause is one reason the subject is in the state it is in.
type Cause struct {
	Code       Code       `json:"code"`
	Message    string     `json:"message"`
	Confidence Confidence `json:"confidence"`
	// Resource names what the cause is about, e.g. "Deployment/prod/web".
	Resource string     `json:"resource,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
	// Remediation is the fix stated as an action. Empty when none is
	// determinable — an invented one is worse than none.
	Remediation string `json:"remediation,omitempty"`
	// IntroducedBy is the revision that made the change this cause blames. Nil
	// when the correlation could not be made, which is the ordinary case for a
	// failure nothing recent caused.
	IntroducedBy *RevisionRef `json:"introducedBy,omitempty"`
}

// ChangeKind is what happened to one environment variable or image between two
// revisions.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
)

// EnvChange is one environment variable's change between two revisions, at the
// container level — which is the level a diagnosis needs, because two
// containers of the same workload fail for different reasons.
//
// Before and After are the variable's *rendered form*: a literal value, or
// "secret <name>/<key>" for a secretKeyRef, which carries no value because a
// manifest never holds one (ADR-0009). Literals pass through redact.Scrub on
// the way in like every other display byte (#117).
type EnvChange struct {
	Workload  string     `json:"workload"`
	Container string     `json:"container"`
	Name      string     `json:"name"`
	Kind      ChangeKind `json:"kind"`
	Before    string     `json:"before,omitempty"`
	After     string     `json:"after,omitempty"`
}

// ImageChange is one container's image change between two revisions.
type ImageChange struct {
	Workload  string `json:"workload"`
	Container string `json:"container"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
}

// Change is the provenance half of an explanation: what the most recent
// revision did, so a reader correlates the current state with the last change
// without a second round trip.
type Change struct {
	Revision RevisionRef  `json:"revision"`
	Previous *RevisionRef `json:"previous,omitempty"`
	// Summary is the one-line rendering of the fields below.
	Summary string        `json:"summary"`
	Env     []EnvChange   `json:"env,omitempty"`
	Images  []ImageChange `json:"images,omitempty"`
}

// Subject is what the explanation is about.
type Subject struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Namespace   string `json:"namespace,omitempty"`
	Revision    string `json:"revision,omitempty"`
}

// Explanation is the whole answer: bounded, ordered and self-describing about
// what it could not read.
type Explanation struct {
	Subject Subject `json:"subject"`
	// Phase is the delivery state-machine phase, relayed from the adapter.
	Phase string `json:"phase,omitempty"`
	// Summary is the one-line answer the causes explain.
	Summary string  `json:"summary"`
	Causes  []Cause `json:"causes,omitempty"`
	// RecentChange is the last recorded revision and what it changed. Nil when
	// no history was readable; Notes then says why.
	RecentChange *Change `json:"recentChange,omitempty"`
	// Notes records what could not be read, so a partial answer is never
	// mistaken for a complete one.
	Notes []string `json:"notes,omitempty"`
	// Truncated records every bound that fired.
	Truncated []string `json:"truncated,omitempty"`
}

// LogFn returns a bounded window of one application's most recent output. It is
// a function rather than an interface because there is exactly one thing to
// ask, and a nil one is a caller with no log access — never an error.
type LogFn func(ctx context.Context, namespace, application string, lines int) ([]observation.Line, error)

// ManifestFn returns the rendered manifests recorded for one revision — the
// bytes that were applied, not a re-render (#38). It is how the change
// correlation is made honestly: an env-var diff between two recorded revisions,
// not a guess from the current spec. A nil one means no correlation is
// available, which is a Note, not an error.
type ManifestFn func(ctx context.Context, revision string) ([]delivery.Manifest, error)

// Input is everything an explanation is computed from. Only Project and
// Environment are required; every other field narrows the answer, and an absent
// one costs a cause rather than the whole report.
type Input struct {
	Project     string
	Environment string
	Namespace   string

	// Status is the delivery adapter's own report for the environment.
	Status delivery.Status
	// Verdicts are the observation plane's workload verdicts, in the order the
	// caller computed them (secret-sync verdicts first, by convention of
	// internal/api's workloadVerdicts).
	Verdicts []observation.Verdict
	// History is the recorded revisions, newest first.
	History []delivery.Entry
	// Violations are the L2 dry-run's admission findings, when the caller has
	// run one (issue #45). Empty is the ordinary case.
	Violations []diff.PolicyViolation

	// Notes are degradations the caller already knows about — a history store
	// that refused, a seam this build was not wired with. They are seeded into
	// the answer's own notes so the caller's knowledge and this package's land
	// in one list, inside one budget.
	Notes []string

	// Logs fetches a bounded log window. Nil disables log evidence.
	Logs LogFn
	// Manifests fetches a revision's recorded manifests. Nil disables the
	// change correlation and the manifest-derived evidence (probe paths, images).
	Manifests ManifestFn
}

// Explain composes the explanation. It never returns an error: a seam that
// cannot be read degrades to a [Explanation.Notes] entry, because an
// explanation that fails entirely because the log engine is down is worse than
// one that says the logs were unavailable.
func Explain(ctx context.Context, in Input) Explanation {
	b := &builder{in: in, logs: map[string][]observation.Line{}}
	b.Subject = Subject{
		Project:     in.Project,
		Environment: in.Environment,
		Namespace:   in.Namespace,
		Revision:    in.Status.Revision,
	}
	b.Phase = string(in.Status.Phase)
	for _, n := range in.Notes {
		b.note("%s", n)
	}

	// The recorded manifests come first: the probe paths, the images and the
	// env-var diff all read from them, and the change correlation is what turns
	// a symptom into a cause.
	live, _, change := b.recentChange(ctx, in)
	b.RecentChange = change

	b.Causes = append(b.Causes, statusCauses(in)...)
	b.Causes = append(b.Causes, violationCauses(in)...)
	b.Causes = append(b.Causes, b.verdictCauses(ctx, live)...)
	b.Causes = append(b.Causes, b.envVarCauses(ctx, live, change)...)

	sortCauses(b.Causes)
	b.Summary = summarize(in, b.Causes)
	b.bound()
	return b.Explanation
}

// sortCauses orders by rank then resource then code, which is total: two
// identical inputs produce byte-identical output, and a bounded answer keeps
// the most answering causes.
func sortCauses(causes []Cause) {
	sort.SliceStable(causes, func(i, j int) bool {
		a, b := causes[i], causes[j]
		if ra, rb := a.Code.rank(), b.Code.rank(); ra != rb {
			return ra < rb
		}
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return a.Code < b.Code
	})
}

// summarize writes the one-line answer the causes explain. It counts what it
// found rather than characterising it, and names the leading cause: an agent
// that reads only the first line still learns which cause to act on.
func summarize(in Input, causes []Cause) string {
	failing := 0
	for _, v := range in.Verdicts {
		if !v.Healthy && observation.IsFailure(v.Code) {
			failing++
		}
	}
	subject := in.Project + "/" + in.Environment
	phase := string(in.Status.Phase)
	if phase == "" {
		phase = "unknown phase"
	}
	switch {
	case len(causes) == 0 && len(in.Verdicts) == 0:
		return fmt.Sprintf("%s is %s; no workload verdicts were available, so nothing here is a statement about the pods",
			subject, phase)
	case len(causes) == 0:
		return fmt.Sprintf("%s is %s with %d workloads observed and no cause found", subject, phase, len(in.Verdicts))
	default:
		lead := causes[0]
		return fmt.Sprintf("%s is %s: %s (%s confidence)%s — %d of %d workloads failing",
			subject, phase, lead.Message, lead.Confidence, introducedSuffix(lead), failing, len(in.Verdicts))
	}
}

func introducedSuffix(c Cause) string {
	if c.IntroducedBy == nil {
		return ""
	}
	return ", introduced by " + c.IntroducedBy.Revision
}

// note records a degradation once. Repeats are dropped: three verdicts whose
// logs could not be read are one fact about the log engine, not three.
func (e *Explanation) note(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	for _, existing := range e.Notes {
		if existing == msg {
			return
		}
	}
	e.Notes = append(e.Notes, msg)
}

// resourceName is the workload name inside "Deployment/namespace/name". kelson
// renders one workload per component under the component's own name, which is
// what makes it the selector a log query wants.
func resourceName(resource string) string {
	if i := strings.LastIndex(resource, "/"); i >= 0 {
		return resource[i+1:]
	}
	return resource
}
