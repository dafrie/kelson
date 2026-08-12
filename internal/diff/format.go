package diff

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Terminal and JSON renderers over the same Diff (issue #44). The data never
// changes per client — only the rendering does. The terminal renderer colours
// by Risk so a human sees blast radius at a glance; the JSON renderer is what
// agents and the UI consume.

// --- colour -----------------------------------------------------------------

// ANSI codes. Kept local so NO_COLOR / non-TTY degradation is explicit.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	ansiDim    = "\x1b[2m"
)

// riskANSICode maps Risk to its highlight colour.
func riskANSICode(r Risk) string {
	switch r {
	case RiskDisruptive:
		return ansiRed
	case RiskRestart:
		return ansiYellow
	case RiskAdditive:
		return ansiCyan
	default:
		return ansiGreen
	}
}

// DefaultColor reports whether colour output is appropriate for w: it must be
// a terminal (char device) and NO_COLOR must be unset (https://no-color.org).
// Passing a non-file writer (buffer, pipe) always yields plain text.
func DefaultColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// --- terminal renderer ------------------------------------------------------

// Write renders the Diff as a human-readable, colour-optional report. Output
// is deterministic for a given color flag, so it is safe to golden-test.
func Write(w io.Writer, d *Diff, color bool) error {
	_, err := io.WriteString(w, format(d, color))
	return err
}

func format(d *Diff, color bool) string {
	var b strings.Builder
	level := string(d.Level)
	// L2 results carry the same shape; this renderer is shared.
	if d.Degraded {
		level += " (degraded: " + d.DegradedReason + ")"
	}
	if color {
		b.WriteString(ansiDim)
	}
	fmt.Fprintf(&b, "%s diff %s/%s\n", level, d.Project, d.Environment)
	if color {
		b.WriteString(ansiReset)
	}
	b.WriteString("\n")

	for _, r := range d.Resources {
		writeResource(&b, r, color)
	}
	if len(d.Violations) > 0 {
		b.WriteString("\n")
		writeViolations(&b, d.Violations, color)
	}
	if len(d.Unvalidated) > 0 {
		b.WriteString("\n")
		writeUnvalidated(&b, d.Unvalidated, color)
	}
	b.WriteString("\n")
	writeSummary(&b, d.Summary, color)
	return b.String()
}

// writeViolations prints admission-policy findings. A developer reading this
// should see which policy rejects the change and why, without applying it
// (#45). Enforce and audit are labelled distinctly so a warning is never
// mistaken for a blocker.
func writeViolations(b *strings.Builder, vs []PolicyViolation, color bool) {
	for _, v := range vs {
		label := "BLOCKED"
		col := ansiRed
		if v.Enforcement == EnforcementAudit {
			label = "warning"
			col = ansiYellow
		}
		head := fmt.Sprintf("%s %s/%s", label, v.Engine, v.Policy)
		if v.Rule != "" {
			head += " rule " + v.Rule
		}
		if color {
			head = col + head + ansiReset
		}
		fmt.Fprintf(b, "%s\n", head)
		fmt.Fprintf(b, "    %s\n", v.Resource)
		if path := v.SpecPath; path != "" {
			fmt.Fprintf(b, "    at %s\n", path)
		} else if v.Path != "" {
			fmt.Fprintf(b, "    at %s\n", v.Path)
		}
		if v.Message != "" {
			fmt.Fprintf(b, "    %s\n", v.Message)
		}
	}
}

// writeUnvalidated prints resources the preview could not evaluate, so an
// incomplete preview never reads as a clean one (#43).
func writeUnvalidated(b *strings.Builder, us []Unvalidated, color bool) {
	for _, u := range us {
		head := "not validated: " + u.Resource
		if u.Requires != "" {
			head += " (requires " + u.Requires + ")"
		}
		if !u.InBatch {
			head += " — prerequisite is missing, apply would fail"
		}
		if color {
			c := ansiYellow
			if !u.InBatch {
				c = ansiRed
			}
			head = c + head + ansiReset
		}
		fmt.Fprintf(b, "%s\n", head)
	}
}

func writeResource(b *strings.Builder, r ResourceDiff, color bool) {
	sym := "~"
	switch r.Op {
	case OpAdded:
		sym = "+"
	case OpRemoved:
		sym = "-"
	}
	col := riskANSICode(r.Risk)
	if color {
		fmt.Fprintf(b, "%s%s%s %s/%s ", col, sym, ansiReset, r.Kind, r.Name)
		fmt.Fprintf(b, "%s%s%s\n", col, r.Risk, ansiReset)
	} else {
		fmt.Fprintf(b, "%s %s/%s %s\n", sym, r.Kind, r.Name, r.Risk)
	}

	for _, f := range r.Fields {
		origin := string(f.Origin)
		if origin == "" {
			origin = string(OriginSpec)
		}
		var line string
		switch {
		case f.Before == nil && f.After == nil:
			continue
		case f.Before == nil:
			line = fmt.Sprintf("    + %s = %s  (%s)\n", f.Path, valueString(f.After), origin)
		case f.After == nil:
			line = fmt.Sprintf("    - %s = %s  (%s, removed)\n", f.Path, valueString(f.Before), origin)
		default:
			line = fmt.Sprintf("    ~ %s = %s -> %s  (%s)\n", f.Path, valueString(f.Before), valueString(f.After), origin)
		}
		if color {
			line = riskANSICode(f.Risk) + line + ansiReset
		}
		b.WriteString(line)
	}
}

func writeSummary(b *strings.Builder, s Summary, color bool) {
	line := fmt.Sprintf("summary: %d added, %d modified, %d removed",
		s.Added, s.Modified, s.Removed)
	if len(s.Restarting) > 0 {
		line += fmt.Sprintf(" · restart: [%s]", strings.Join(s.Restarting, ", "))
	}
	if len(s.Disruptive) > 0 {
		line += fmt.Sprintf(" · disruptive: [%s]", strings.Join(s.Disruptive, ", "))
	}
	line += fmt.Sprintf(" · max risk: %s", s.MaxRisk)
	if color {
		b.WriteString(riskANSICode(s.MaxRisk))
		b.WriteString(line)
		b.WriteString(ansiReset)
		return
	}
	b.WriteString(line)
}

// valueString renders a Before/After value compactly for the terminal.
func valueString(v any) string {
	if v == nil {
		return "null"
	}
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("%q", t)
	default:
		// Maps/slices from an added/removed subtree: compact, sorted.
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

// --- JSON renderer ----------------------------------------------------------

// EncodeJSON encodes the Diff as indented JSON, the machine-readable form
// agents and the UI consume. Struct field order and sorted map keys make the
// bytes deterministic.
func EncodeJSON(d *Diff) ([]byte, error) {
	return json.MarshalIndent(d, "", "  ")
}
