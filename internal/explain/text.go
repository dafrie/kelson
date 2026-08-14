package explain

import (
	"fmt"
	"strings"
)

// Text renders the explanation for a human, in the shape `kelson status` and
// the MCP reports already read in: sections in capitals, two-space indent,
// nothing invented and nothing hidden.
//
// It renders the same values a caller would read off the struct — no field is
// summarised away — so a reader and an agent are looking at one answer. The
// bounds have already been applied by the time this runs; Text neither trims
// nor pads.
func (e Explanation) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", e.Summary)
	fmt.Fprintf(&b, "\nSUBJECT\n  project      %s\n  environment  %s\n", e.Subject.Project, e.Subject.Environment)
	if e.Subject.Namespace != "" {
		fmt.Fprintf(&b, "  namespace    %s\n", e.Subject.Namespace)
	}
	if e.Subject.Revision != "" {
		fmt.Fprintf(&b, "  revision     %s\n", e.Subject.Revision)
	}
	if e.Phase != "" {
		fmt.Fprintf(&b, "  phase        %s\n", e.Phase)
	}

	b.WriteString("\nCAUSES\n")
	if len(e.Causes) == 0 {
		b.WriteString("  none: nothing in the delivery status, the workload verdicts or the recent change explains a failure\n")
	}
	for i, c := range e.Causes {
		writeCause(&b, i+1, c)
	}

	writeChange(&b, e.RecentChange)
	writeList(&b, "NOTES (what could not be read)", e.Notes)
	writeList(&b, "TRUNCATED", e.Truncated)
	return b.String()
}

func writeCause(b *strings.Builder, n int, c Cause) {
	fmt.Fprintf(b, "  %d. [%s] %s\n", n, c.Confidence, c.Code)
	if c.Resource != "" {
		fmt.Fprintf(b, "     resource: %s\n", c.Resource)
	}
	fmt.Fprintf(b, "     %s\n", c.Message)
	if c.IntroducedBy != nil {
		fmt.Fprintf(b, "     introduced by: %s\n", revisionLine(*c.IntroducedBy))
	}
	for _, ev := range c.Evidence {
		writeEvidence(b, ev)
	}
	if c.Remediation != "" {
		fmt.Fprintf(b, "     fix: %s\n", c.Remediation)
	}
}

// writeEvidence indents a multi-line excerpt under its own header, so a log
// tail reads as a block rather than smearing into the cause's prose.
func writeEvidence(b *strings.Builder, ev Evidence) {
	header := fmt.Sprintf("     %s", ev.Kind)
	if ev.Source != "" {
		header += " (" + ev.Source + ")"
	}
	lines := strings.Split(ev.Detail, "\n")
	if len(lines) == 1 {
		fmt.Fprintf(b, "%s: %s\n", header, lines[0])
		return
	}
	fmt.Fprintf(b, "%s:\n", header)
	for _, l := range lines {
		fmt.Fprintf(b, "       | %s\n", l)
	}
}

func writeChange(b *strings.Builder, change *Change) {
	if change == nil {
		return
	}
	b.WriteString("\nRECENT CHANGE\n")
	fmt.Fprintf(b, "  revision  %s\n", revisionLine(change.Revision))
	if change.Previous != nil {
		fmt.Fprintf(b, "  previous  %s\n", revisionLine(*change.Previous))
	}
	fmt.Fprintf(b, "  %s\n", change.Summary)
	for _, ic := range change.Images {
		fmt.Fprintf(b, "  image  %s/%s  %s → %s\n", ic.Workload, ic.Container, orDash(ic.Before), orDash(ic.After))
	}
	for _, ec := range change.Env {
		fmt.Fprintf(b, "  env    %s/%s  %s %s %s\n", ec.Workload, ec.Container, ec.Name, ec.Kind, envDelta(ec))
	}
}

func envDelta(ec EnvChange) string {
	switch {
	case ec.Before != "" && ec.After != "":
		return ec.Before + " → " + ec.After
	case ec.Before != "":
		return "(was " + ec.Before + ")"
	case ec.After != "":
		return "(now " + ec.After + ")"
	default:
		return ""
	}
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", title)
	for _, item := range items {
		fmt.Fprintf(b, "  - %s\n", item)
	}
}

func revisionLine(r RevisionRef) string {
	parts := []string{r.Revision}
	if r.CommittedAt != "" {
		parts = append(parts, r.CommittedAt)
	}
	if r.Message != "" {
		parts = append(parts, r.Message)
	}
	if r.Author != "" {
		parts = append(parts, "by "+r.Author)
	}
	return strings.Join(parts, "  ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
