/**
 * Reading a `HistoryEntry` for display, and refusing to read more than it says.
 *
 * `DeployService.History` returns five strings per revision — `revision`,
 * `spec_hash`, `committed_at`, `message`, `author` — and nothing else
 * (proto/kelson/v1alpha1/deploy.proto, `HistoryEntry`). Every function here
 * derives from those five, conservatively: each one has a "cannot tell" answer
 * and returns it rather than guessing, because a history screen that guesses is
 * worse than one that admits a gap.
 *
 * # What changed when the delivery spine was rebuilt (ADR-0028, R2 #225)
 *
 * `revision` is now `<generation>-<hash8>`, the OCI artifact tag the controller
 * published (ADR-0028 decision 2) — never a git commit sha, because there is no
 * git writer left to commit one. The history entry it names is a publish: a
 * rollback repoints Flux at bytes that already exist and prepends no entry of
 * its own (ADR-0028 decision 5, `internal/controller/history.go`), so every row
 * this screen shows is something that was deployed, not restored.
 *
 * `message` is where the outcome, the digest and the images travel, because
 * `HistoryEntry` has no field of its own for any of them yet
 * (`internal/api`'s `revisionSummary`): a snippet like
 * `Healthy · serving · sha256:deadbeef · ghcr.io/acme/hello:1.4.2` is a
 * captured snapshot, not a live health check, and it is rendered as the
 * server's own prose rather than parsed apart — there is no reliable seam in
 * free text to parse one out of.
 *
 * What is deliberately NOT here, because the wire does not carry it:
 *
 *   - **An outcome you can rely on for every row.** `message`'s outcome is what
 *     was true when the entry was captured, and only `DeployService.Status`
 *     answers for what is true *now* — and only for the one revision the
 *     cluster reports as live, which is why the phase pill appears on exactly
 *     one row.
 *   - **The rendered manifests.** They are not on `HistoryEntry`, so a
 *     client-side revision-A-vs-revision-B diff cannot be assembled here. The
 *     server diffs the *current* spec against a recorded revision
 *     (`DiffRequest.from_revision`) and offers no A-vs-B call, so that is the
 *     comparison the screen links to, unfaked.
 *   - **Human-vs-agent attribution.** The spine records who deployed nothing
 *     yet (`author` arrives empty on every entry) — that is #74's work, not a
 *     property of which entry this is.
 */

/** How much of a long hash is shown, e.g. a spec hash's digest half. */
const SHORT_LENGTH = 12;

/**
 * A spec hash at reading length: `sha256:0123456789ab`.
 *
 * Only an `algorithm:hex` spelling is shortened, and only when there is more
 * hex than the short form would show. Anything else is returned as it arrived —
 * the store's hash format is the server's business, and a value this does not
 * recognise is far more likely to be a format change than something safe to
 * truncate.
 */
export function shortSpecHash(specHash: string): string {
  const match = /^([0-9a-z]+):([0-9a-f]+)$/i.exec(specHash);
  if (match === null) return specHash;
  const [, algorithm = "", hex = ""] = match;
  if (hex.length <= SHORT_LENGTH) return specHash;
  return `${algorithm}:${hex.slice(0, SHORT_LENGTH)}`;
}

/**
 * `committed_at` as wall clock, in the shape the deploy stream's timestamps
 * already take on screen (components/phase.ts's formatInstant): UTC, seconds,
 * no fractional part.
 *
 * The field is documented as optional and is a free-form string on the wire, so
 * a value that does not parse yields "" and the caller omits the column. An
 * unparseable timestamp rendered as "Invalid Date" would be a claim about when
 * something happened.
 */
export function formatWhen(committedAt: string): string {
  if (committedAt === "") return "";
  const when = new Date(committedAt);
  if (Number.isNaN(when.getTime())) return "";
  return when.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}
