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
 * What is deliberately NOT here, because the wire does not carry it:
 *
 *   - **The image.** `HistoryEntry` has no image field, so the commit a built
 *     image embeds in its tag (internal/build's DestinationTag, which is the
 *     short revision) cannot be recovered here. The revision id itself is the
 *     only provenance a recorded entry carries.
 *   - **An outcome.** No phase, no health, no exit status is recorded per
 *     revision. `DeployService.Status` answers that for the one revision the
 *     cluster reports as live, and for no other, which is why the screen puts a
 *     phase pill on exactly one row.
 *   - **The rendered manifests.** They are not on `HistoryEntry`, so a
 *     client-side revision-A-vs-revision-B diff cannot be assembled here. The
 *     server diffs the *current* spec against a recorded revision
 *     (`DiffRequest.from_revision`) and offers no A-vs-B call, so that is the
 *     comparison the screen links to, unfaked.
 *   - **Human-vs-agent attribution.** The Git modes do write `Kelson-Actor` and
 *     `Kelson-Agent-Id` commit trailers (internal/delivery/git/identity.go), but
 *     `History()` reads only the spec-hash/project/environment trailers and
 *     projects the commit *signature* as `author`. The distinction exists in the
 *     repository and not on this wire.
 */

/** The length of a full git object id in hex — internal/build's commitLength. */
const COMMIT_LENGTH = 40;

/** How much of a 40-hex revision is shown, matching internal/build's tag rule. */
const SHORT_LENGTH = 12;

/**
 * Whether a revision id is a git commit.
 *
 * The Git and Flux modes record the manifests-repository commit sha as the
 * revision (internal/delivery/git's History), so a revision that is a full
 * commit hash *is* the source commit of the manifests — surfacing it as one is
 * a fact, not a parse. Direct mode records a zero-padded counter ("rev-00000007")
 * and this returns false for it.
 *
 * The predicate is internal/build's `IsCommit`, verbatim: exactly forty hex
 * digits. An abbreviated sha is not accepted — a 12-character hex string is
 * indistinguishable from any other short token, and claiming it is a commit is
 * exactly the guess this screen must not make.
 */
export function isCommitRevision(revision: string): boolean {
  return (
    revision.length === COMMIT_LENGTH && /^[0-9a-f]+$/i.test(revision)
  );
}

/**
 * A revision id at reading length: a commit sha abbreviated, anything else
 * returned untouched. Direct mode's counter is already short and every
 * character of it is meaningful, so shortening it would only lose the padding
 * that makes it sort.
 */
export function shortRevision(revision: string): string {
  return isCommitRevision(revision)
    ? revision.slice(0, SHORT_LENGTH)
    : revision;
}

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
 * What kind of act a recorded entry was: a deploy, a rollback, or unknown.
 *
 * This is not an outcome — nothing on the wire says whether a revision ended up
 * healthy — it is what the adapter wrote down about the act itself, and it is
 * the one distinction every mode records, which is what lets the screen read
 * the same in direct and Git modes:
 *
 *   direct     `deploy <spec-hash>`                     `rollback to <revision>`
 *   git/flux   `kelson: update <project>/<environment>` `kelson: rollback <project>/<environment> to <short>`
 *
 * The four prefixes above are matched and nothing else is. The subject is a
 * human sentence — internal/delivery/git's Message says outright that "nothing
 * downstream parses the subject" — so a message from an older build, a
 * hand-written commit on the manifests branch, or a subject that merely starts
 * with a similar word ("deployment tuning") is `unknown`, and the screen shows
 * no chip rather than a wrong one.
 */
export type RecordKind = "deploy" | "rollback" | "unknown";

export function classifyRecord(message: string): RecordKind {
  const subject = message.trim();
  if (/^kelson:\s+rollback\b/i.test(subject)) return "rollback";
  if (/^kelson:\s+update\b/i.test(subject)) return "deploy";
  if (/^rollback\b/i.test(subject)) return "rollback";
  if (/^deploy\b/i.test(subject)) return "deploy";
  return "unknown";
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
