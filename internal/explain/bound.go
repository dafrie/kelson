package explain

import (
	"fmt"
	"strings"
)

// The bounds, and the one place they are enforced (issue #77, ADR-0023).
//
// An explanation exists to be pasted into a context window whole. That makes
// its size a contract rather than a side effect, so the numbers are constants
// with names, they are stated in the proto comments that carry this over the
// wire, and every trim is recorded in Explanation.Truncated instead of being
// performed silently. A truncated answer read as a complete one produces a
// wrong conclusion from a correct report, which is the one failure mode a
// diagnosis tool must not have.
//
// Enforcement runs last, over the assembled answer, rather than at each
// producer. A cause that knew its own budget would have to know what every
// other cause spent.
const (
	// MaxCauses is how many causes one explanation carries. Six is chosen
	// against the shape of a real failure: one root cause, its symptom on two
	// or three workloads, and room for a reconciler or policy cause above them.
	// Beyond that the answer stops being an explanation and becomes a list.
	MaxCauses = 6
	// MaxEvidence is how many evidence items one cause carries.
	MaxEvidence = 4
	// MaxLogLines is how many lines a single log excerpt carries.
	MaxLogLines = 8
	// MaxLineBytes truncates one line of evidence detail. A stack frame or a
	// JSON log line runs long, and the first 200 bytes are what identify it.
	MaxLineBytes = 200
	// MaxBytes is the whole explanation's budget, counted over every string it
	// carries. 12 KiB is a few hundred tokens of a context window, which is
	// what "usable directly in a context window" has to mean for a tool an
	// agent calls in a loop.
	MaxBytes = 12 * 1024
)

// ellipsis marks a value this package cut. It is a distinct marker rather than
// three dots so a reader can tell kelson's truncation from the workload's
// own output.
const ellipsis = " […truncated]"

// bound applies every cap, in a fixed order, and records what it did.
//
// The order is chosen so the cheapest cut happens first and the most
// destructive last: over-long lines, then over-long excerpts, then surplus
// evidence, then surplus causes, and only then — if the total is still over
// budget — evidence and whole causes from the least answering end, which is
// what Code.rank ordered them for.
func (e *Explanation) bound() {
	e.boundText()
	e.boundEvidence()
	e.boundCauses()
	e.boundChange()
	e.boundBudget()
}

// boundText truncates every individual string to something a line can hold.
func (e *Explanation) boundText() {
	e.Summary = clampLine(e.Summary, MaxBytes/8)
	for i := range e.Causes {
		c := &e.Causes[i]
		c.Message = clampLine(c.Message, MaxBytes/8)
		c.Remediation = clampLine(c.Remediation, MaxBytes/8)
		for j := range c.Evidence {
			ev := &c.Evidence[j]
			if ev.Kind == EvidenceLog {
				ev.Detail = clampExcerpt(ev.Detail)
				continue
			}
			ev.Detail = clampLine(ev.Detail, MaxLineBytes*2)
		}
	}
	for i := range e.Notes {
		e.Notes[i] = clampLine(e.Notes[i], MaxLineBytes*2)
	}
}

// boundEvidence caps the evidence per cause. Evidence is ordered by the
// producer with the most direct signal first, so the tail is what goes.
func (e *Explanation) boundEvidence() {
	for i := range e.Causes {
		c := &e.Causes[i]
		if len(c.Evidence) <= MaxEvidence {
			continue
		}
		e.truncate("%s: %d of %d evidence items dropped (the cap is %d per cause)",
			c.Code, len(c.Evidence)-MaxEvidence, len(c.Evidence), MaxEvidence)
		c.Evidence = c.Evidence[:MaxEvidence]
	}
}

func (e *Explanation) boundCauses() {
	if len(e.Causes) <= MaxCauses {
		return
	}
	e.truncate("%d of %d causes dropped (the cap is %d); the ones kept are the most answering, not the first found",
		len(e.Causes)-MaxCauses, len(e.Causes), MaxCauses)
	e.Causes = e.Causes[:MaxCauses]
}

// boundChange caps the change lists. They are evidence for a diagnosis, not a
// changelog: a revision that changed forty variables is one whose diff the
// reader should go and read in full.
func (e *Explanation) boundChange() {
	if e.RecentChange == nil {
		return
	}
	if n := len(e.RecentChange.Env); n > MaxChanges {
		e.truncate("%d of %d environment-variable changes dropped from the recent change (the cap is %d)",
			n-MaxChanges, n, MaxChanges)
		e.RecentChange.Env = e.RecentChange.Env[:MaxChanges]
	}
	if n := len(e.RecentChange.Images); n > MaxChanges {
		e.truncate("%d of %d image changes dropped from the recent change (the cap is %d)", n-MaxChanges, n, MaxChanges)
		e.RecentChange.Images = e.RecentChange.Images[:MaxChanges]
	}
	e.RecentChange.Summary = clampLine(e.RecentChange.Summary, MaxLineBytes*2)
}

// boundBudget is the last resort: the per-item caps can still add up over
// budget, so evidence and then whole causes are dropped from the least
// answering end until the total fits.
func (e *Explanation) boundBudget() {
	for e.size() > MaxBytes {
		if e.dropLastEvidence() {
			continue
		}
		if len(e.Causes) > 1 {
			dropped := e.Causes[len(e.Causes)-1]
			e.Causes = e.Causes[:len(e.Causes)-1]
			e.truncate("cause %s dropped to stay inside the %d-byte budget", dropped.Code, MaxBytes)
			continue
		}
		// One cause, no evidence left on it, and still over: the remaining
		// prose is the answer itself, so it is clamped rather than dropped.
		e.Summary = clampLine(e.Summary, MaxLineBytes)
		if len(e.Causes) == 1 {
			e.Causes[0].Message = clampLine(e.Causes[0].Message, MaxLineBytes)
			e.Causes[0].Remediation = clampLine(e.Causes[0].Remediation, MaxLineBytes)
		}
		e.Notes = clampList(e.Notes)
		e.RecentChange = nil
		if e.size() > MaxBytes {
			// Nothing left that can be cut without lying about the shape.
			e.Truncated = clampList(e.Truncated)
			return
		}
	}
}

// dropLastEvidence removes one evidence item from the least answering cause
// that still has any, reporting whether it found one.
func (e *Explanation) dropLastEvidence() bool {
	for i := len(e.Causes) - 1; i >= 0; i-- {
		c := &e.Causes[i]
		if len(c.Evidence) == 0 {
			continue
		}
		c.Evidence = c.Evidence[:len(c.Evidence)-1]
		e.truncate("evidence dropped from %s to stay inside the %d-byte budget", c.Code, MaxBytes)
		return true
	}
	return false
}

// size is the byte count the budget is measured against: every string the
// explanation carries. It is deliberately the sum of the payload rather than a
// serialisation, so the same bound holds for the JSON, the wire message and the
// rendered text.
func (e *Explanation) size() int {
	n := len(e.Summary) + len(e.Phase) + len(e.Subject.Project) + len(e.Subject.Environment) +
		len(e.Subject.Namespace) + len(e.Subject.Revision)
	for _, c := range e.Causes {
		n += len(c.Code) + len(c.Message) + len(c.Confidence) + len(c.Resource) + len(c.Remediation)
		for _, ev := range c.Evidence {
			n += len(ev.Kind) + len(ev.Source) + len(ev.Detail)
		}
		if c.IntroducedBy != nil {
			n += len(c.IntroducedBy.Revision) + len(c.IntroducedBy.CommittedAt) +
				len(c.IntroducedBy.Message) + len(c.IntroducedBy.Author)
		}
	}
	if ch := e.RecentChange; ch != nil {
		n += len(ch.Summary) + len(ch.Revision.Revision) + len(ch.Revision.CommittedAt) + len(ch.Revision.Message)
		for _, c := range ch.Env {
			n += len(c.Workload) + len(c.Container) + len(c.Name) + len(c.Kind) + len(c.Before) + len(c.After)
		}
		for _, c := range ch.Images {
			n += len(c.Workload) + len(c.Container) + len(c.Before) + len(c.After)
		}
	}
	for _, s := range e.Notes {
		n += len(s)
	}
	for _, s := range e.Truncated {
		n += len(s)
	}
	return n
}

// truncate records one trim. Like note, a repeat is dropped.
func (e *Explanation) truncate(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	for _, existing := range e.Truncated {
		if existing == msg {
			return
		}
	}
	e.Truncated = append(e.Truncated, msg)
}

// clampExcerpt bounds a log excerpt on both axes: the number of lines and the
// length of each. The newest lines are kept, because the last thing a container
// said before it died is the diagnosis.
func clampExcerpt(s string) string {
	lines := tailLines(splitLines(s), MaxLogLines)
	for i := range lines {
		lines[i] = clampLine(lines[i], MaxLineBytes)
	}
	return strings.Join(lines, "\n")
}

// clampLine truncates one string to n bytes on a rune boundary.
func clampLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isBoundary(s, cut) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + ellipsis
}

// isBoundary reports whether i splits s between runes, so truncation never
// leaves half a multi-byte character behind.
func isBoundary(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}

// clampList bounds a list of notes when the budget is already exhausted.
func clampList(list []string) []string {
	if len(list) <= 2 {
		return list
	}
	return list[:2]
}
