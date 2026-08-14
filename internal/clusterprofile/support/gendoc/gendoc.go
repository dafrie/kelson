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
// The page also carries the storage clone-capability table (issue #91) from
// internal/clusterprofile/storage, for the reason given below the imports.
//
// Output is deterministic: the matrix slice is authored in stable order, and
// the driver table — which is a map — is emitted through storage.Drivers(),
// which sorts.
package main

import (
	"bytes"
	"fmt"

	"github.com/dafrie/kelson/internal/clusterprofile/storage"
	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// The storage clone-capability table belongs on this page rather than in a
// page of its own (issue #91). It is the same kind of promise as a version
// floor — "here is what kelson supports, and what it will tell you when your
// cluster is outside it" — and it is maintained as data in
// internal/clusterprofile/storage for exactly the reason the support matrix is:
// so the documented answer cannot drift from the answer the code gives. A
// reader asking "will branching be fast on my cluster?" is asking a support
// question, and this is the page they are already on.

// Render returns the bytes of docs/reference/support-matrix.md.
func Render() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/clusterprofile/support/gendoc`, which reads `internal/clusterprofile/support/matrix.go` and rewrites this page. -->\n\n")
	fmt.Fprintf(&b, "# Support matrix\n\n")
	fmt.Fprintf(&b, "This page is **generated** from the declared support matrix in "+
		"[`internal/clusterprofile/support/matrix.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/support/matrix.go) "+
		"and the storage clone-capability table in "+
		"[`internal/clusterprofile/storage/drivers.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/storage/drivers.go), "+
		"the same data the checks enforce. Edit those and regenerate; never edit this page by hand "+
		"(issues #57, #91).\n\n")
	fmt.Fprintf(&b, "kelson adopts components a cluster already has rather than installing its own (ADR-0003). "+
		"Each component below has a **minimum supported version**: a cluster reporting a version at or above it is "+
		"Supported, one below it is Unsupported, and a version a probe could not read is Unknown — reported for the "+
		"caller to decide, never silently treated as fine.\n\n")
	fmt.Fprintf(&b, "The **What degrades** column is the part to read first. A version number on its own sends you "+
		"to the source; knowing that a too-old operator means every `kind: postgres` component refuses to render is a "+
		"decision you can make. `kelson profile` prints these same statements for the cluster in front of you, and so "+
		"does every command that resolves a live profile — see "+
		"[detection](../detection.md#version-skew-and-explicit-degradation).\n\n")

	fmt.Fprintf(&b, "## Supported components\n\n")
	fmt.Fprintf(&b, "| Component | Minimum supported | Tested up to | Below the floor | What degrades |\n")
	fmt.Fprintf(&b, "|-----------|-------------------|--------------|-----------------|---------------|\n")
	for _, c := range support.Components {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s |\n",
			c.Name, c.Minimum, testedText(c.Tested), degradeText(c.Degrade), inline(c.Affects))
	}

	fmt.Fprintf(&b, "\n## Below the floor\n\n")
	fmt.Fprintf(&b, "- **`refuse`** — a version below the minimum makes kelson refuse to render and say why, "+
		"naming the component, the version found, the version required and what degrades. The per-capability "+
		"refusals for data and chart components are enforced in the renderer; the rest is reported so a caller "+
		"can act before apply time (issue #57).\n")
	fmt.Fprintf(&b, "- **`render-older`** — a version below the minimum is still usable but kelson must render the "+
		"older API. No component is on this path yet; the decision is recorded here so a future one is explicit.\n")

	fmt.Fprintf(&b, "\n## Newer than tested\n\n")
	fmt.Fprintf(&b, "A version **above** the tested column is never a refusal. kelson knows only that it has not "+
		"exercised that release — not that anything is wrong with it — and refusing on that basis would break "+
		"working clusters to prevent a hypothetical. It is reported as a `[note]`, so that if something does behave "+
		"oddly there, the fact that you are outside the tested range is already on screen instead of being a "+
		"mystery. A blank tested column claims no upper bound at all.\n")

	renderStorage(&b)
	renderWhy(&b)
	return b.Bytes()
}

// renderWhy writes the per-component reasoning. It is a section rather than a
// sixth table column because a floor is a judgement call a maintainer has to be
// able to re-derive when bumping it, and that argument does not fit in a cell.
func renderWhy(b *bytes.Buffer) {
	fmt.Fprintf(b, "\n## Why these floors\n\n")
	fmt.Fprintf(b, "Each floor is a decision, not a default. Move one by editing its row in "+
		"`internal/clusterprofile/support/matrix.go` and regenerating this page.\n\n")
	for _, c := range support.Components {
		fmt.Fprintf(b, "- **`%s` %s** — %s\n", c.Name, c.Minimum, inline(c.Note))
	}
}

// testedText renders the ceiling cell. An empty ceiling is stated as such: a
// blank cell would read as an oversight rather than as "no upper bound is
// claimed for this component".
func testedText(tested string) string {
	if tested == "" {
		return "—"
	}
	return "`" + tested + "`"
}

// renderStorage writes the storage clone-capability section from the maintained
// driver table (issue #91), so what `kelson profile` reports about a cluster's
// storage and what this page promises are the same data.
func renderStorage(b *bytes.Buffer) {
	fmt.Fprintf(b, "\n## Storage clone capability\n\n")
	fmt.Fprintf(b, "Database branching snapshots a Postgres cluster's volume, and what that costs depends entirely on "+
		"the CSI driver behind the storage class (ADR-0007). `kelson profile` reports it per storage class as "+
		"`cloneCapability`, with a `cloneConfidence` saying how the answer was reached.\n\n")
	fmt.Fprintf(b, "| Capability | What branching does | Cost |\n")
	fmt.Fprintf(b, "|------------|---------------------|------|\n")
	fmt.Fprintf(b, "| `thin` | copy-on-write clone of the volume | seconds, almost no extra space |\n")
	fmt.Fprintf(b, "| `full-copy` | snapshot restored into a full-size volume | time and space proportional to the database |\n")
	fmt.Fprintf(b, "| `none` | no CSI snapshot exists; restore from a backup instead | not available yet — the destination is issue #94 and the restore is #100 |\n")
	fmt.Fprintf(b, "| `unknown` | snapshots exist but the driver is unrecognised, or storage could not be read | plan for a full copy until confirmed |\n")

	fmt.Fprintf(b, "\n### Drivers kelson recognises\n\n")
	fmt.Fprintf(b, "This table is maintained by driver name rather than probed: measuring a driver would mean "+
		"provisioning and snapshotting a volume during what is a read-only capability probe. A driver that is not "+
		"listed is reported as `unknown` with `cloneConfidence: unknown-driver` — never guessed in either direction.\n\n")
	fmt.Fprintf(b, "| Driver | Clone capability |\n")
	fmt.Fprintf(b, "|--------|------------------|\n")
	for _, d := range storage.Drivers() {
		capability, _ := storage.Capability(d)
		fmt.Fprintf(b, "| `%s` | `%s` |\n", d, capability)
	}

	fmt.Fprintf(b, "\n### Confidence\n\n")
	fmt.Fprintf(b, "- **`observed`** — no snapshot class serves the provisioner, so nothing can clone it. Read from "+
		"cluster state, not from the table.\n")
	fmt.Fprintf(b, "- **`known-driver`** — the driver is in the table above.\n")
	fmt.Fprintf(b, "- **`unknown-driver`** — snapshots exist, the driver is not in the table, so the cost is unknown.\n")
	fmt.Fprintf(b, "- **`unreadable`** — the probe could not list snapshot classes; the profile's `incomplete` entry "+
		"names the permission that would settle it.\n")
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
