package diff

import "github.com/dafrie/kelson/internal/redact"

// Redact replaces every Kubernetes Secret value carried by a Diff with
// redact.Sentinel, keeping the keys (issue #117, ADR-0009).
//
// # Why the Diff is redacted at construction rather than at print time
//
// A Diff is a display artifact by definition. Nothing applies one: the delivery
// adapters consume a delivery.ManifestSet, and the recorded bytes a rollback
// replays are read straight from the history store, never from here. Every
// consumer of this type — `kelson diff`, the API's DiffResponse, the UI, the
// MCP tools, the rollback preview — is a reader. So redacting once, where the
// Diff is built, covers all of them, and a new renderer added later inherits
// the property instead of having to remember it. Redacting in EncodeJSON and
// Write instead would leave the in-memory Diff loaded and would be exactly the
// convention-not-a-property arrangement issue #117 exists to end.
//
// # What it covers, and what it deliberately does not
//
// It covers the field-level values, which is where a Secret's content actually
// travels: a `data` mapping added wholesale, one key under it changed, and the
// last-applied-configuration annotation an L2 readback drags in from the live
// cluster (redact.Value handles all three).
//
// It does not touch Unvalidated.Message, PolicyViolation.Message or
// DegradedReason. Those are the API server's own prose, and rewriting prose
// structurally would mean guessing which of its bytes are secret — the
// content-sniffing this project refuses (see the internal/redact package doc).
// Literal values kelson has itself resolved are handled by the known-value
// scrubber instead, at the surface that prints them.
func Redact(d *Diff) *Diff {
	if d == nil {
		return nil
	}
	for i := range d.Resources {
		r := &d.Resources[i]
		for j := range r.Fields {
			f := &r.Fields[j]
			f.Before = redact.Value(r.Kind, f.Path, f.Before)
			f.After = redact.Value(r.Kind, f.Path, f.After)
		}
	}
	return d
}
