// Package redact keeps secret values out of everything kelson prints
// (issue #117, [ADR-0009] "Logs are redacted").
//
// The property it exists to make testable is absolute: no secret value is ever
// written to a build log, a deploy log, an event, an error message or a diff.
// Coolify's documented failure mode is the reason it is a package with tests
// rather than a convention — deployment logs there have printed .env values in
// the clear, and encrypting at rest is worthless if the value appears in a log
// five minutes later.
//
// # Two mechanisms, because there are two kinds of knowledge
//
// [Document], [Documents], [Node] and [Value] are *structural*: they know that
// a Kubernetes Secret keeps its values under data/stringData, so they replace
// every such value with [Sentinel] while keeping the keys. Nothing is guessed
// from the content — the resource's own kind is what selects it.
//
// [Scrubber] and the process-wide [Register]/[Scrub] pair are *known-value*:
// they replace literal strings kelson has itself resolved and therefore knows
// are credentials. This is the mechanism for text kelson does not own the
// structure of, such as a build log.
//
// # The boundary: kelson does not content-sniff user output
//
// There is deliberately no third mechanism that guesses whether an arbitrary
// string "looks like" a secret. A workload's stdout is the workload's; kelson
// streams it through (internal/observation, internal/api/logs.go) and cannot
// know which of its bytes are secret. Pattern-matching over user log lines
// would corrupt legitimate output and would still miss the credential shaped
// like a word. The guarantee kelson makes is the one it can keep: **kelson
// never adds a secret to a log**. What the user's own process prints is the
// user's to control.
//
// # Display bytes are redacted; delivery bytes are not
//
// The single rule every caller is wired against:
//
//   - A *display* surface exists to be read — a diff, a preview, an error
//     message, a log stream, an agent's tool response, a manifest returned over
//     the API for inspection. These are redacted.
//   - A *delivery* surface exists to be applied — `kelson render`'s own stdout
//     and -o output, the bytes a delivery adapter applies (delivery.ManifestSet),
//     what a Git writer commits, what the rendered-history store records, and
//     what a rollback replays from it. These keep their real bytes, always.
//
// The rule is not a preference. Applying a redacted manifest would write the
// literal string "[redacted]" into the cluster as a Secret value, which is
// worse than the leak it prevents: it is a silent, successful corruption. So
// redaction is applied at the point bytes are *projected for a reader*, never
// at the point they are produced, and never in place on a manifest set.
//
// Two consequences of that split are worth stating because they look like
// omissions:
//
//   - The rendered-history store keeps real bytes ([internal/serverstate]).
//     Rollback replays exactly what was applied (#38), so a redacted history
//     would be a history that cannot restore. What is redacted is the
//     *readback for display* — the diffs computed from those bytes, which is
//     the only way recorded manifests reach a reader today.
//   - The renderer is untouched. It is a pure function whose output is the
//     artifact (ADR-0001); a redacting renderer would have no way to produce
//     the real thing.
//
// [ADR-0009]: https://github.com/dafrie/kelson/blob/main/docs/adr/0009-secrets.md
package redact
