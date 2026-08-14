package explain

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/redact"
)

// The acceptance case of issue #77: a CrashLoopBackOff caused by a missing
// environment variable, explained with the specific variable and the revision
// that introduced it.
//
// # Two independent signals, and the confidence says which fired
//
//  1. The CHANGE. The recorded rendered manifests of the live revision and its
//     predecessor are diffed at the level of one container's environment
//     (change.go). A variable removed, renamed away, or repointed at a Secret
//     key is a candidate, and the history entry for the live revision is the
//     revision that introduced it.
//  2. The SYMPTOM. The crash-looping container's own bounded output is scanned:
//     for each candidate name literally, and — independently of any candidate —
//     for the phrasings a runtime uses when a variable is absent.
//
// Both agree: high, with the field reference, the log line and the revision as
// evidence. One only: medium, and the message states plainly which half is a
// correlation rather than a fact. Neither: nothing is emitted. A guess dressed
// as a cause is the failure mode this whole package exists to avoid.
//
// # What it cannot see
//
// A container that dies without saying why, in a revision that changed nothing,
// is not explained here — and should not be. A missing Secret *key* behind an
// existing reference produces CreateContainerConfigError, which the observation
// plane does not classify today (ADR-0023 records that as a known gap); the
// change signal catches the common shape of it, where the reference itself is
// what the revision added.

// missingEnvPatterns are the phrasings that mean "this process wanted an
// environment variable and did not get one". Each must capture the variable
// name in group 1.
//
// The list is deliberately short and specific. A pattern that fired on ordinary
// output would put a fabricated variable name into an answer a reader trusts,
// which is worse than missing a runtime whose phrasing is not here — a missed
// phrase costs confidence, never correctness, because the revision correlation
// still fires on its own at medium.
var missingEnvPatterns = []*regexp.Regexp{
	// "missing required environment variable DATABASE_URL", "missing env var: X"
	regexp.MustCompile(`(?i)missing (?:required )?(?:environment variable|env(?:ironment)? var(?:iable)?)[:\s]+["']?([A-Z][A-Z0-9_]{2,})`),
	// "environment variable DATABASE_URL is not set / is required / is empty"
	regexp.MustCompile(`(?i)(?:environment variable|env var)[:\s]+["']?([A-Z][A-Z0-9_]{2,})["']?\s+(?:is |was )?(?:not set|unset|required|missing|empty|not defined)`),
	// Python: KeyError: 'DATABASE_URL'
	regexp.MustCompile(`KeyError:\s*["']([A-Z][A-Z0-9_]{2,})["']`),
	// Go/Node/shell: "DATABASE_URL is not set", "DATABASE_URL must be set"
	regexp.MustCompile(`\b([A-Z][A-Z0-9_]{2,})\b\s+(?:is |was )?(?:not set|unset|must be set|is required|is undefined|not defined)`),
	// "unset variable DATABASE_URL", "no value for DATABASE_URL"
	regexp.MustCompile(`(?i)(?:unset variable|no value for|cannot read)\s+["']?([A-Z][A-Z0-9_]{2,})`),
}

// envVarCauses builds the missing-environment-variable causes, one per
// crash-looping workload at most.
func (b *builder) envVarCauses(ctx context.Context, live []workload, change *Change) []Cause {
	var out []Cause
	for _, v := range b.in.Verdicts {
		if v.Healthy || v.Code != observation.CodeCrashLoopBackOff {
			continue
		}
		if c, ok := b.envVarCause(ctx, v, live, change); ok {
			out = append(out, c)
		}
	}
	return out
}

func (b *builder) envVarCause(ctx context.Context, v observation.Verdict, live []workload, change *Change) (Cause, bool) {
	name := resourceName(v.Resource)
	lines := b.crashLogLines(ctx, v)
	candidates := envCandidates(change, name)

	named, evidence := matchCandidates(candidates, lines)
	if named == "" {
		// No candidate is named in the output. The output may still say a
		// variable is missing on its own — a variable that was never declared
		// at all changes nothing between revisions, so this is the half of the
		// case the diff cannot see.
		if fromLog, line := scanForMissingEnv(lines); fromLog != "" {
			return b.logOnlyCause(v, fromLog, line, live), true
		}
		if len(candidates) == 0 {
			return Cause{}, false
		}
		return b.changeOnlyCause(v, candidates, live, change), true
	}
	return b.agreedCause(v, named, candidates, evidence, live, change), true
}

// crashLogLines is the container output this detection reads: the lines the
// probe captured before the container terminated, falling back to the caller's
// log seam. Bounded on both paths.
func (b *builder) crashLogLines(ctx context.Context, v observation.Verdict) []string {
	for _, c := range v.Containers {
		if strings.TrimSpace(c.Logs) != "" {
			return tailLines(splitLines(c.Logs), MaxLogLines*2)
		}
	}
	var out []string
	for _, l := range b.logWindow(ctx, resourceName(v.Resource)) {
		out = append(out, l.Message)
	}
	return tailLines(out, MaxLogLines*2)
}

// envCandidate is one environment-variable change that could explain a crash.
type envCandidate struct {
	change EnvChange
	// why is the one clause that says what about this change could break a
	// container, appended to the cause message.
	why string
}

// envCandidates selects the changes of this workload that could starve a
// container of a variable.
//
// Removals are the classic. A modification that repoints a literal at a Secret
// key is the same failure wearing a different hat — the value the container
// reads now depends on a Secret that may not hold that key. An addition of a
// secretKeyRef is included for the same reason. A plain literal changing value
// is NOT a candidate: the variable is still there, and blaming a value the
// container never complained about would be the guess this package refuses.
func envCandidates(change *Change, workload string) []envCandidate {
	if change == nil {
		return nil
	}
	var out []envCandidate
	for _, ec := range change.Env {
		if resourceName(ec.Workload) != workload {
			continue
		}
		switch {
		case ec.Kind == ChangeRemoved:
			out = append(out, envCandidate{ec, "the live revision removed it"})
		case ec.Kind == ChangeAdded && strings.HasPrefix(ec.After, "secret "):
			out = append(out, envCandidate{ec, "the live revision added it as a reference to " + ec.After +
				", which resolves only if that Secret holds that key"})
		case ec.Kind == ChangeModified && strings.HasPrefix(ec.After, "secret ") && !strings.HasPrefix(ec.Before, "secret "):
			out = append(out, envCandidate{ec, "the live revision repointed it at " + ec.After +
				", which resolves only if that Secret holds that key"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].change.Name < out[j].change.Name })
	return out
}

// matchCandidates looks for each candidate's name in the container's output.
// The first match wins, in candidate order, so the answer is deterministic.
func matchCandidates(candidates []envCandidate, lines []string) (name, line string) {
	for _, c := range candidates {
		for _, l := range lines {
			if strings.Contains(l, c.change.Name) {
				return c.change.Name, l
			}
		}
	}
	return "", ""
}

// scanForMissingEnv reads a variable name out of the output alone.
func scanForMissingEnv(lines []string) (name, line string) {
	for _, l := range lines {
		for _, re := range missingEnvPatterns {
			if m := re.FindStringSubmatch(l); len(m) > 1 {
				return m[1], l
			}
		}
	}
	return "", ""
}

// agreedCause is the acceptance case at full strength: the container names the
// variable AND a revision changed it. Two independent signals, so high.
func (b *builder) agreedCause(v observation.Verdict, name string, candidates []envCandidate, line string, live []workload, change *Change) Cause {
	c := candidateNamed(candidates, name)
	cause := Cause{
		Code:       CodeMissingEnvVar,
		Confidence: ConfidenceHigh,
		Resource:   v.Resource,
		Message: fmt.Sprintf("%s crash-loops because the environment variable %s is missing: %s, and the container's own output names it",
			resourceName(v.Resource), name, c.why),
		Remediation: remediationFor(c.change),
		Evidence: []Evidence{
			{Kind: EvidenceLog, Source: v.Resource, Detail: redact.Scrub(strings.TrimSpace(line))},
			envChangeEvidence(c.change),
		},
	}
	if ref := fieldRef(live, resourceName(v.Resource), c.change.Container, name); ref != "" {
		cause.Evidence = append(cause.Evidence, Evidence{Kind: EvidenceField, Source: v.Resource, Detail: ref})
	}
	cause.IntroducedBy = introducedBy(change)
	return cause
}

// changeOnlyCause is the correlation without the confession: a revision took a
// variable away and the container is crash-looping, but its output does not
// name it. Medium, and the message says exactly that — an agent must not read
// this as the container having complained.
func (b *builder) changeOnlyCause(v observation.Verdict, candidates []envCandidate, live []workload, change *Change) Cause {
	c := candidates[0]
	names := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		names = append(names, cand.change.Name)
	}
	cause := Cause{
		Code:       CodeMissingEnvVar,
		Confidence: ConfidenceMedium,
		Resource:   v.Resource,
		Message: fmt.Sprintf("%s crash-loops and the live revision changed the environment it runs with (%s): %s. This is a correlation — the container's output does not name any of them",
			resourceName(v.Resource), strings.Join(names, ", "), c.why),
		Remediation: remediationFor(c.change),
		Evidence:    []Evidence{envChangeEvidence(c.change)},
	}
	if ref := fieldRef(live, resourceName(v.Resource), c.change.Container, c.change.Name); ref != "" {
		cause.Evidence = append(cause.Evidence, Evidence{Kind: EvidenceField, Source: v.Resource, Detail: ref})
	}
	cause.IntroducedBy = introducedBy(change)
	return cause
}

// logOnlyCause is the confession without the change: the container says a
// variable is missing and no recent revision touched it. Medium, with no
// introducedBy — a variable that was never declared has no revision to blame,
// and inventing one would be the worst kind of wrong answer.
func (b *builder) logOnlyCause(v observation.Verdict, name, line string, live []workload) Cause {
	return Cause{
		Code:       CodeMissingEnvVar,
		Confidence: ConfidenceMedium,
		Resource:   v.Resource,
		Message: fmt.Sprintf("%s crash-loops and its own output says the environment variable %s is missing. No recorded revision changed it, so nothing correlates this with a recent change",
			resourceName(v.Resource), name),
		Remediation: fmt.Sprintf("declare %s in the component's env in the spec — as a literal, or as {secret: <name>, key: <key>} for a credential (ADR-0018) — and deploy again", name),
		Evidence: []Evidence{
			{Kind: EvidenceLog, Source: v.Resource, Detail: redact.Scrub(strings.TrimSpace(line))},
			{Kind: EvidenceRevision, Source: "history", Detail: "no environment-variable change for this container between the live revision and its predecessor"},
		},
	}
}

func candidateNamed(candidates []envCandidate, name string) envCandidate {
	for _, c := range candidates {
		if c.change.Name == name {
			return c
		}
	}
	return envCandidate{}
}

func envChangeEvidence(ec EnvChange) Evidence {
	detail := fmt.Sprintf("%s %s on %s container %s", ec.Name, ec.Kind, ec.Workload, ec.Container)
	switch {
	case ec.Before != "" && ec.After != "":
		detail += fmt.Sprintf(": %s → %s", ec.Before, ec.After)
	case ec.Before != "":
		detail += ": was " + ec.Before
	case ec.After != "":
		detail += ": now " + ec.After
	}
	return Evidence{Kind: EvidenceRevision, Source: "recorded manifests", Detail: redact.Scrub(detail)}
}

// fieldRef is the path of the variable in the rendered manifest, in
// internal/diff's convention, so a reader can go and look at the exact field.
// Empty when the variable is gone from the live revision, which is precisely
// the removal case — there is no field to point at, and the revision evidence
// is what says so.
func fieldRef(live []workload, workloadName, containerName, envName string) string {
	w, ok := findWorkload(live, workloadName)
	if !ok {
		return ""
	}
	c, ok := w.container(containerName)
	if !ok {
		return ""
	}
	for _, v := range c.Env {
		if v.Name == envName {
			return c.envPath(envName)
		}
	}
	return ""
}

func remediationFor(ec EnvChange) string {
	if ec.Kind == ChangeRemoved {
		return fmt.Sprintf("restore %s in the component's env, or roll back to the previous revision if the removal was not intended; `kelson rollback` previews what it cannot revert first",
			ec.Name)
	}
	return fmt.Sprintf("check that %s resolves: the Secret and key named in %s must exist in this namespace (diagnose_application lists the Secrets kelson manages, and `kelson secret set` writes a missing one)",
		ec.Name, ec.After)
}

// introducedBy is the revision the change belongs to: the live one, because a
// change is between it and its predecessor and it is the one that shipped it.
func introducedBy(change *Change) *RevisionRef {
	if change == nil || change.Revision.Revision == "" {
		return nil
	}
	ref := change.Revision
	return &ref
}
