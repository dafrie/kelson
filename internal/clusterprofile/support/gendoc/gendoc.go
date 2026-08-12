// Package main contains the gendoc generator: it renders the declared support
// matrix (internal/clusterprofile/support.Components) into the user-facing
// docs/reference/support-matrix.md.
//
// The reference is generated, not hand-maintained (issue #57): it is sourced
// from the same slice the version-skew checks (support.Check) enforce, so the
// documented support matrix cannot drift from what the checks allow — the exact
// failure the issue exists to prevent. The generator is invoked both by this
// command (`go generate ./internal/clusterprofile/support/gendoc`) and by a
// golden test that fails if the committed Markdown ever differs from the output.
//
// Output is deterministic: the matrix slice is authored in stable order, so
// no sorting or map iteration is needed.
package main

import (
	"bytes"
	"fmt"

	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// Render returns the bytes of docs/reference/support-matrix.md.
func Render() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/clusterprofile/support/gendoc`, which reads `internal/clusterprofile/support/matrix.go` and rewrites this page. -->\n\n")
	fmt.Fprintf(&b, "# Support matrix\n\n")
	fmt.Fprintf(&b, "This page is **generated** from the declared support matrix in "+
		"[`internal/clusterprofile/support/matrix.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/support/matrix.go), "+
		"the same data the version-skew checks enforce. Edit the matrix and regenerate; never edit this page by hand "+
		"(issue #57).\n\n")
	fmt.Fprintf(&b, "kelson adopts components a cluster already has rather than installing its own (ADR-0003). "+
		"Each component below has a **minimum supported version**: a cluster reporting a version at or above it is "+
		"Supported, one below it is Unsupported, and a version a probe could not read is Unknown — reported for the "+
		"caller to decide, never silently treated as fine.\n\n")

	fmt.Fprintf(&b, "## Supported components\n\n")
	fmt.Fprintf(&b, "| Component | Minimum supported | Too-old behaviour | Notes |\n")
	fmt.Fprintf(&b, "|-----------|-------------------|-------------------|-------|\n")
	for _, c := range support.Components {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", c.Name, c.Minimum, degradeText(c.Degrade), inline(c.Note))
	}

	fmt.Fprintf(&b, "\n## Too-old behaviour\n\n")
	fmt.Fprintf(&b, "- **`refuse`** — a version below the minimum makes kelson refuse to render and say why, "+
		"naming the component, the version found and the version required. Enforcement is the renderer's job and is "+
		"not yet wired; the checks report the finding so a caller can act (issue #57).\n")
	fmt.Fprintf(&b, "- **`render-older`** — a version below the minimum is still usable but kelson must render the "+
		"older API. No component is on this path yet; the decision is recorded here so a future one is explicit.\n")

	return b.Bytes()
}

// degradeText maps a Degrade decision to the human phrase shown in the table.
func degradeText(d support.Degrade) string {
	switch d {
	case support.DegradeRenderOlder:
		return "render older API"
	default:
		return "refuse with a reason"
	}
}

// inline collapses newlines in a note so table rows stay well-formed.
func inline(s string) string {
	out := bytes.Buffer{}
	space := false
	for _, r := range s {
		if r == '\n' {
			if !space {
				out.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		out.WriteRune(r)
	}
	return out.String()
}
