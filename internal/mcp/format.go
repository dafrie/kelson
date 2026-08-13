package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// Bounds. Every list a tool renders passes through [limit] with one of these,
// and the overflow is stated rather than dropped silently: an agent that reads
// a truncated list as the whole list will draw a wrong conclusion from a
// correct answer, which is worse than being told the answer is partial
// (ADR-0008 §2).
const (
	maxProjects     = 25
	maxEnvironments = 10
	maxVerdicts     = 12
	maxApplications = 20
	maxManifests    = 30
	maxHistory      = 5
	maxFindings     = 15
	maxSpecErrors   = 25
	maxEvents       = 20

	// diagnoseLogLines is the log window diagnose_application embeds. It shares
	// the answer with status, verdicts, history and a spec summary, so it is a
	// fraction of what logs_window alone may return.
	diagnoseLogLines = 80
	// maxLogLines is the hard cap of logs_window, whatever tail the caller asks
	// for.
	maxLogLines = 200
)

// truncation is the marker every capped list ends with.
const truncation = "… %d more %s (truncated)"

// report builds one tool's answer: plain text, mono-friendly, sections in
// capitals. Text rather than structured content is deliberate — a second,
// typed projection of the API's own messages would be a shape to keep in sync
// with no reader for it, and the models these tools are written for read the
// prose (ADR-0008 §3).
type report struct {
	b strings.Builder
}

func (r *report) addf(format string, a ...any) {
	fmt.Fprintf(&r.b, format+"\n", a...) //nolint:errcheck // a strings.Builder write cannot fail
}

// section starts a labelled block.
func (r *report) section(title string) {
	r.b.WriteString("\n")
	r.b.WriteString(title)
	r.b.WriteString("\n")
}

// truncated writes the overflow marker when limit dropped anything.
func (r *report) truncated(dropped int, noun string) {
	if dropped > 0 {
		r.addf("  "+truncation, dropped, noun)
	}
}

func (r *report) String() string { return r.b.String() }

// limit caps a list, returning what to render and how much was left out.
func limit[T any](items []T, n int) ([]T, int) {
	if len(items) <= n {
		return items, 0
	}
	return items[:n], len(items) - n
}

// joinCapped renders a capped string list inline, stating the overflow.
func joinCapped(shown []string, dropped int) string {
	joined := strings.Join(shown, ", ")
	if dropped > 0 {
		joined += fmt.Sprintf(" (+%d more, truncated)", dropped)
	}
	return joined
}

// sortedKeys orders a map's keys so a report is stable across calls. Go's map
// iteration is randomised, and an answer whose lines move between two identical
// calls reads to a model as a change.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// wireError renders one structured API error verbatim. The fields are the
// taxonomy an agent branches on (ADR-0013 §2, issue #72), so they are printed
// as-is rather than summarised into prose: a tool that paraphrased
// "store/version-conflict" would take away the only stable thing in the
// message.
func (r *report) wireError(indent string, e *kelsonv1alpha1.Error) {
	if e == nil {
		return
	}
	r.addf("%scode: %s", indent, e.GetCode())
	for _, field := range []struct{ label, value string }{
		{"resource", e.GetResource()},
		{"field", e.GetField()},
		{"application", e.GetApplication()},
		{"overlay", e.GetOverlay()},
		{"target", e.GetTarget()},
		{"message", e.GetMessage()},
		{"cause", e.GetCause()},
		{"remediation", e.GetRemediation()},
		{"docs_url", e.GetDocsUrl()},
	} {
		if field.value != "" {
			r.addf("%s  %s: %s", indent, field.label, field.value)
		}
	}
	if line := e.GetLine(); line > 0 {
		if col := e.GetColumn(); col > 0 {
			r.addf("%s  position: line %d, column %d", indent, line, col)
			return
		}
		r.addf("%s  position: line %d", indent, line)
	}
}

// stamp renders a wire instant. Zero means the producer had none, and the epoch
// is never the answer (the schema's convention, logs.go).
func stamp(unixMs int64) string {
	if unixMs == 0 {
		return "-"
	}
	return time.UnixMilli(unixMs).UTC().Format(time.RFC3339)
}

// logStamp is the time column of a log line: seconds resolution, no date,
// because a bounded window never spans one.
func logStamp(unixMs int64) string {
	if unixMs == 0 {
		return "--:--:--"
	}
	return time.UnixMilli(unixMs).UTC().Format("15:04:05")
}

// pad left-aligns a column so a report reads as a table in a monospace font.
func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// resourceName is the workload name inside an observation verdict's
// "Deployment/namespace/name" resource string. kelson renders one workload per
// application under the application's own name, which is what makes it the
// selector LogService wants.
func resourceName(resource string) string {
	if i := strings.LastIndex(resource, "/"); i >= 0 {
		return resource[i+1:]
	}
	return resource
}

// bytesize renders a manifest size. Sizes are reported instead of bodies:
// deploy's preview says what would be applied and how big it is, and an agent
// that wants the YAML itself has RenderService for that (ADR-0008 §2).
func bytesize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f kB", float64(n)/1024)
}
