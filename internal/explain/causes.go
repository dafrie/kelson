package explain

import (
	"context"
	"fmt"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/redact"
)

// The cause derivations (issue #77, ADR-0023).
//
// Each one turns a signal another plane produced into a Cause with the evidence
// behind it. None of them re-classifies: a crash loop is a crash loop because
// the observation plane said so, a rejection is a rejection because the API
// server said so, and what is added here is the evidence, the correlation and
// the confidence rule that says how much of a claim this is.

// builder carries the explanation under construction plus the per-call caches.
// It exists so the log window of a workload is fetched once even though several
// causes read it, and so the Explanation type itself stays free of machinery a
// caller would see in its JSON.
type builder struct {
	Explanation
	in   Input
	logs map[string][]observation.Line
}

// verdictCauses turns the observation plane's failing verdicts into causes.
//
// Only failures. A wait state is not a cause — observation keeps "has not
// finished" apart from "is broken" (issue #53) and folding the two here would
// undo that distinction at the last surface before the reader.
//
// `missing` is the one code taken beyond observation.IsFailure, which
// deliberately excludes it so a Tracker never calls a workload that has not
// been created yet broken. An explanation is asked after the fact rather than
// during a rollout, and "kelson rendered this and nothing with that name is in
// the cluster" is a fact worth stating; the message names the benign reading
// too, so a deploy still in flight is not mistaken for a fault.
func (b *builder) verdictCauses(ctx context.Context, live []workload) []Cause {
	var out []Cause
	for _, v := range b.in.Verdicts {
		if v.Healthy || (!observation.IsFailure(v.Code) && v.Code != observation.CodeMissing) {
			continue
		}
		out = append(out, b.verdictCause(ctx, v, live))
	}
	return out
}

func (b *builder) verdictCause(ctx context.Context, v observation.Verdict, live []workload) Cause {
	c := Cause{
		Resource:    v.Resource,
		Confidence:  ConfidenceHigh,
		Remediation: v.Remediation,
	}
	// The verdict's own reason is the controller's words and leads the
	// evidence for every code: it is the sentence the kubelet, the scheduler or
	// the external-secrets controller wrote about this exact object.
	if v.Reason != "" {
		c.Evidence = append(c.Evidence, Evidence{
			Kind: EvidenceCondition, Source: v.Resource, Detail: redact.Scrub(v.Reason),
		})
	}

	switch v.Code {
	case observation.CodeCrashLoopBackOff:
		b.crashCause(ctx, &c, v)
	case observation.CodeImagePullBackOff:
		c.Code = CodeImagePull
		c.Message = fmt.Sprintf("%s cannot pull %s: the registry refused or the reference does not resolve",
			resourceName(v.Resource), imageOf(v, live))
	case observation.CodeFailingProbe:
		c.Code = CodeFailingProbe
		// Observation calls this one a heuristic in its own doc comment: the
		// container is running and not ready, which is *usually* a probe. That
		// is exactly a medium, and the message says which part is inferred.
		c.Confidence = ConfidenceMedium
		c.Message = fmt.Sprintf("%s is running but never becomes ready, which observation reads as a failing probe (inferred from readiness, not from a kubelet reason)%s",
			resourceName(v.Resource), probeSuffix(v, live))
		c.Evidence = append(c.Evidence, probeEvidence(v, live)...)
	case observation.CodeInsufficientResources:
		c.Code = CodeInsufficientResources
		c.Message = fmt.Sprintf("%s cannot be scheduled: the cluster does not have the CPU or memory its pods request",
			resourceName(v.Resource))
	case observation.CodeSchedulingFailed:
		c.Code = CodeUnschedulable
		c.Message = fmt.Sprintf("%s cannot be scheduled: no node satisfies its selectors, affinity rules or taints",
			resourceName(v.Resource))
	case observation.CodeSecretSyncFailed:
		c.Code = CodeSecretSyncFailed
		c.Message = fmt.Sprintf("%s did not sync: external-secrets could not read the value from the store or could not write the Secret, so every workload referencing it is waiting",
			v.Resource)
	case observation.CodeMissing:
		c.Code = CodeWorkloadMissing
		c.Message = fmt.Sprintf("%s is not in the cluster: kelson rendered it and nothing with that name is there — either the apply never reached it, or this deploy is still in flight",
			v.Resource)
	default:
		// A failure code this package does not know yet. Relaying it with the
		// verdict's own words is the honest answer; inventing a class for it
		// would put a code into the taxonomy that means nothing.
		c.Code = Code("explain/" + string(v.Code))
		c.Confidence = ConfidenceMedium
		c.Message = v.String()
	}
	return c
}

// crashCause splits a crash-loop verdict into the three things it can be. The
// kubelet's terminated reason is what separates them, and it is quoted rather
// than paraphrased.
func (b *builder) crashCause(ctx context.Context, c *Cause, v observation.Verdict) {
	name := resourceName(v.Resource)
	switch {
	case containsFold(v.Reason, "OOMKilled"):
		c.Code = CodeOOMKilled
		c.Message = fmt.Sprintf("%s is killed for exceeding its memory limit (OOMKilled) and restarted, repeatedly", name)
		c.Remediation = "raise the component's memory limit, or find what allocates: the container is not crashing on its own logic, the kernel is stopping it"
	case containsFold(v.Reason, "Evicted"):
		c.Code = CodeEvicted
		c.Message = fmt.Sprintf("%s was evicted: the node reclaimed resources and this pod was the one it took", name)
		c.Remediation = "check node pressure and this component's requests: an evicted pod is a scheduling decision made under duress, not an application fault"
	default:
		c.Code = CodeCrashLoop
		c.Message = fmt.Sprintf("%s starts and dies repeatedly", name)
		if !containsFold(v.Reason, "CrashLoopBackOff") {
			// Observation also infers a loop from a terminated container with
			// restarts, where the kubelet named no waiting reason. That is one
			// signal short of the kubelet saying it, so it is a medium and the
			// message says which.
			c.Confidence = ConfidenceMedium
			c.Message += " (inferred from a terminated container with restarts; the kubelet reported no CrashLoopBackOff waiting reason)"
		}
	}
	c.Evidence = append(c.Evidence, containerEvidence(v)...)
	c.Evidence = append(c.Evidence, b.logEvidence(ctx, v)...)
}

// containerEvidence quotes each failing container's own last state. It is the
// per-container half of the verdict (observation.Container) and the thing that
// says WHICH container of a multi-container pod died.
func containerEvidence(v observation.Verdict) []Evidence {
	var out []Evidence
	for _, c := range v.Containers {
		if c.Reason == "" {
			continue
		}
		out = append(out, Evidence{
			Kind:   EvidenceCondition,
			Source: fmt.Sprintf("pod/%s container %s", c.Pod, c.Name),
			Detail: redact.Scrub(fmt.Sprintf("%s: %s", c.Code, c.Reason)),
		})
	}
	return out
}

// logEvidence attaches the bounded window of the workload's own output.
//
// The verdict's containers already carry the lines the probe fetched before the
// container terminated, which is the crash-loop diagnosis; those are preferred
// because they are the right window and cost nothing. A caller with a log seam
// and a verdict that carries none falls back to it. Either way the excerpt is
// the tail — the newest lines are what explain a failure — and it is bounded
// twice, here and again in bound().
func (b *builder) logEvidence(ctx context.Context, v observation.Verdict) []Evidence {
	for _, c := range v.Containers {
		if c.LogError != "" {
			b.note("the logs of pod %s container %s could not be read: %s", c.Pod, c.Name, c.LogError)
		}
		if strings.TrimSpace(c.Logs) == "" {
			continue
		}
		return []Evidence{{
			Kind:   EvidenceLog,
			Source: fmt.Sprintf("pod/%s container %s", c.Pod, c.Name),
			Detail: strings.Join(tailLines(splitLines(c.Logs), MaxLogLines), "\n"),
		}}
	}
	lines := b.logWindow(ctx, resourceName(v.Resource))
	if len(lines) == 0 {
		return nil
	}
	texts := make([]string, 0, len(lines))
	for _, l := range lines {
		texts = append(texts, l.Message)
	}
	return []Evidence{{
		Kind:   EvidenceLog,
		Source: b.in.Namespace + "/" + resourceName(v.Resource),
		Detail: strings.Join(tailLines(texts, MaxLogLines), "\n"),
	}}
}

// logWindow fetches (once per component) the bounded tail the log seam
// offers. A failure is a Note: an explanation whose log engine is down is worth
// less, never worthless.
func (b *builder) logWindow(ctx context.Context, component string) []observation.Line {
	if b.in.Logs == nil || component == "" {
		return nil
	}
	if lines, ok := b.logs[component]; ok {
		return lines
	}
	lines, err := b.in.Logs(ctx, b.in.Namespace, component, MaxLogLines)
	if err != nil {
		b.note("the log window for %s could not be read: %s", component, err)
		lines = nil
	}
	b.logs[component] = lines
	return lines
}

// imageOf names the image a pull failure is about. The kubelet's own message
// usually quotes it, and that is preferred — it is the reference the runtime
// actually tried. Otherwise the live revision's recorded manifest is asked,
// which is the reference kelson applied.
func imageOf(v observation.Verdict, live []workload) string {
	if quoted := firstQuoted(v.Reason); quoted != "" {
		return quoted
	}
	if w, ok := findWorkload(live, resourceName(v.Resource)); ok && len(w.Containers) > 0 {
		return w.Containers[0].Image
	}
	return "its image"
}

// firstQuoted extracts the first double-quoted span of a controller message,
// which is where the kubelet puts the image reference.
func firstQuoted(s string) string {
	start := strings.IndexByte(s, '"')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(s[start+1:], '"')
	if end <= 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

// probeEvidence names the probes of the failing workload — which probe, which
// path, which port — read from the recorded manifest kelson applied.
func probeEvidence(v observation.Verdict, live []workload) []Evidence {
	w, ok := findWorkload(live, resourceName(v.Resource))
	if !ok {
		return nil
	}
	var out []Evidence
	for _, c := range w.Containers {
		for _, p := range c.Probes {
			out = append(out, Evidence{
				Kind:   EvidenceField,
				Source: fmt.Sprintf("%s.%sProbe.httpGet", c.path, p.Kind),
				Detail: fmt.Sprintf("container %s: %s", c.Name, p),
			})
		}
	}
	return out
}

// probeSuffix names the probes inline in the cause message, so the sentence
// answers "which probe" without the reader walking the evidence.
func probeSuffix(v observation.Verdict, live []workload) string {
	w, ok := findWorkload(live, resourceName(v.Resource))
	if !ok {
		return ""
	}
	var probes []string
	for _, c := range w.Containers {
		for _, p := range c.Probes {
			probes = append(probes, p.String())
		}
	}
	if len(probes) == 0 {
		return ""
	}
	return ". The declared probes are: " + strings.Join(probes, "; ")
}

// statusCauses reads the delivery adapter's own status.
//
// Two things live here that no verdict can see. The release command hook's Job
// is a barrier before the workloads (ADR-0019, issue #104): while it runs there
// are no new pods to have a verdict about, and when it fails the workloads were
// never applied at all. And a reconciler that refused the change is the reason
// nothing downstream reflects it — including the SOPS decryption failure of
// ADR-0022, which kustomize-controller reports as an opaque build error.
func statusCauses(in Input) []Cause {
	var out []Cause
	if c, ok := releaseCause(in.Status); ok {
		out = append(out, c)
	}
	if c, ok := reconcilerCause(in.Status); ok {
		out = append(out, c)
	}
	return out
}

func releaseCause(status delivery.Status) (Cause, bool) {
	state := status.Detail["releaseState"]
	job := status.Detail["releaseJob"]
	resource := "Job/" + job
	evidence := []Evidence{{Kind: EvidenceCondition, Source: "delivery", Detail: redact.Scrub(status.Cause)}}
	switch state {
	case "failed", "timeout":
		return Cause{
			Code:       CodeReleaseJobFailed,
			Confidence: ConfidenceHigh,
			Resource:   resource,
			Message: fmt.Sprintf("the release command hook %s %s, so the workloads of this revision were never applied and the previous revision is still serving",
				job, releaseVerb(state)),
			Evidence:    evidence,
			Remediation: "read the Job's logs, fix the release command or what it depends on, and deploy again: kelson deletes and re-creates a failed release Job on the next attempt (ADR-0019)",
		}, true
	case "running":
		return Cause{
			Code:       CodeReleaseJobPending,
			Confidence: ConfidenceHigh,
			Resource:   resource,
			Message: fmt.Sprintf("the release command hook %s is still running: the workloads wait behind it by design, so this is a deployment in progress rather than a failure",
				job),
			Evidence:    evidence,
			Remediation: "wait, or read the Job's logs if it is taking longer than the migration should",
		}, true
	default:
		return Cause{}, false
	}
}

func releaseVerb(state string) string {
	if state == "timeout" {
		return "ran out of time"
	}
	return "failed"
}

// reconcilerCauses fire only when the adapter's cause names a reconciler
// object. Direct mode applies manifests itself and its degraded status is the
// same fact the workload verdicts already carry in more detail; repeating it as
// a second cause would spend the answer's budget saying one thing twice.
func reconcilerCause(status delivery.Status) (Cause, bool) {
	cause := status.Cause
	if cause == "" || !namesReconciler(cause) {
		return Cause{}, false
	}
	if status.Phase != delivery.PhaseRejected && status.Phase != delivery.PhaseDegraded {
		return Cause{}, false
	}
	c := Cause{
		Code:       CodeReconcilerNotReady,
		Confidence: ConfidenceHigh,
		Resource:   status.Detail["kustomization"],
		Message: fmt.Sprintf("the reconciler is not ready for this revision and the delivery phase is %s; its own report is the evidence below",
			status.Phase),
		Evidence:    []Evidence{{Kind: EvidenceCondition, Source: "reconciler", Detail: redact.Scrub(cause)}},
		Remediation: "fix what the reconciler refused and deploy again; the change is not live, so the previous revision is what is serving",
	}
	if namesDecryptionFailure(cause) {
		c.Code = CodeDecryptionFailed
		c.Message = "the reconciler could not decrypt this revision's SOPS-encrypted manifests, so none of the change is live"
		c.Remediation = "check, in this order: the Kustomization declares spec.decryption with a secretRef; that Secret exists in the reconciler's namespace and holds an age identity; the identity is one of the recipients the files are encrypted to (`kelson secret rotate` lists them without needing a key)"
	}
	return c, true
}

// namesReconciler recognises a cause produced by a GitOps reconciler. It
// matches the object kinds rather than the adapter's name so a cause relayed
// through another surface still reads as one.
func namesReconciler(cause string) bool {
	for _, marker := range []string{"flux:", "Kustomization", "HelmRelease"} {
		if strings.Contains(cause, marker) {
			return true
		}
	}
	return false
}

// namesDecryptionFailure recognises the decryption failure the flux adapter
// already names (internal/delivery/flux/status.go, ADR-0022). It matches the
// phrase that adapter appends rather than re-running its marker list: the
// classification belongs to the adapter, and duplicating the markers here would
// be a second place for them to drift.
func namesDecryptionFailure(cause string) bool {
	return containsFold(cause, "could not decrypt")
}

// violationCauses relay the admission findings of an L2 dry-run (issue #45).
//
// The server's own words are the message, verbatim, for the reason the preview
// gives: the policy author wrote that sentence for exactly this moment, and a
// rewritten one would be kelson guessing at an intent it does not have. What is
// added is the confidence rule — an enforced rejection is what stopped the
// change, an audit finding never stopped anything and is a low.
func violationCauses(in Input) []Cause {
	out := make([]Cause, 0, len(in.Violations))
	for _, v := range in.Violations {
		c := Cause{
			Code:        CodePolicyRejected,
			Confidence:  ConfidenceHigh,
			Resource:    v.Resource,
			Message:     fmt.Sprintf("%s rejected %s: %s", policyName(v), v.Resource, redact.Scrub(v.Message)),
			Remediation: v.Remediation,
			Evidence: []Evidence{{
				Kind:   EvidenceCondition,
				Source: v.Engine,
				Detail: redact.Scrub(fmt.Sprintf("%s (%s): %s", policyName(v), v.Code, v.Message)),
			}},
		}
		if v.Enforcement == diff.EnforcementAudit {
			c.Confidence = ConfidenceLow
			c.Message = fmt.Sprintf("%s reports %s in audit mode: %s. Nothing was vetoed by it",
				policyName(v), v.Resource, redact.Scrub(v.Message))
		}
		if v.Path != "" {
			c.Evidence = append(c.Evidence, Evidence{Kind: EvidenceField, Source: v.Resource, Detail: v.Path})
		}
		out = append(out, c)
	}
	return out
}

func policyName(v diff.PolicyViolation) string {
	switch {
	case v.Policy != "" && v.Rule != "":
		return v.Policy + "/" + v.Rule
	case v.Policy != "":
		return v.Policy
	default:
		return v.Engine
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// tailLines keeps the newest n lines. An over-long window loses its head, not
// its tail: the last thing a container said before it died is the diagnosis.
func tailLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
