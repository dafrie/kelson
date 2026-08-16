/**
 * Reading a `HistoryEntry` for display, and refusing to read more than it says.
 *
 * `DeployService.History` returns eight fields per revision — `revision`,
 * `spec_hash`, `committed_at`, `message`, `author`, `digest`, `images` and
 * `outcome` — and nothing else (proto/kelson/v1alpha1/deploy.proto,
 * `HistoryEntry`). Every function here derives from those, conservatively: each
 * one has a "cannot tell" answer and returns it rather than guessing, because a
 * history screen that guesses is worse than one that admits a gap.
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
 * # The outcome, the digest and the images are fields now
 *
 * They used to travel inside `message` as prose — a snippet like
 * `Healthy · serving · sha256:deadbeef · ghcr.io/acme/hello:1.4.2` — and this
 * module deliberately did not parse it apart, because there is no reliable seam
 * in free text. They are `outcome`, `digest` and `images` on the wire now, so
 * the screen reads them directly and `message` is not read at all: the server
 * keeps filling it for one release for clients built against the older schema,
 * and parsing it back would be re-introducing exactly what the fields removed.
 *
 * `images` carries the component that resolved each image, because the
 * controller records the pair at the moment it renders the revision
 * (`api/kelson/v1alpha1`'s `HistoryEntry.componentImages`). A component name
 * arrives empty only for an entry recorded before the controller did that, and
 * such an image is shown unlabelled rather than under a guessed name.
 *
 * What is deliberately NOT here, because the wire still does not carry it:
 *
 *   - **A live answer for every row.** `outcome` is what the controller
 *     recorded when that revision stopped being the current one, and only
 *     `DeployService.Status` answers for what is true *now* — and only for the
 *     one revision the cluster reports as live, which is why the live phase pill
 *     appears on exactly one row and every other row's outcome is labelled as a
 *     record.
 *   - **The rendered manifests.** They are not on `HistoryEntry`, so a
 *     client-side revision-A-vs-revision-B diff cannot be assembled here. The
 *     server diffs the *current* spec against a recorded revision
 *     (`DiffRequest.from_revision`) and offers no A-vs-B call, so that is the
 *     comparison the screen links to, unfaked.
 *   - **Human-vs-agent attribution.** The spine records who deployed nothing
 *     yet (`author` arrives empty on every entry) — that is #74's work, not a
 *     property of which entry this is.
 */

/**
 * Where one revision sits on the rail, relative to the marker.
 *
 * The whole vocabulary is positional on purpose. `above` and `below` say only
 * which side of the live marker a stop is on, which is a fact about the list the
 * server returned; they carry no claim about how far behind the environment is,
 * because nothing on the wire supports one — `stale` is a boolean and
 * `StatusResponse` has no generation to compare against.
 */
export type RailPlacement =
  /** The revision `Status` reports as live: the marker itself. */
  | "live"
  /** Recorded after the live revision — above the marker on the rail. */
  | "above"
  /** Recorded before it. */
  | "below"
  /** Only the registry remembers it (#241): the rail's faded tail. */
  | "tail"
  /** There is no marker on this rail at all, so no stop has a side. */
  | "unmarked";

/** The one field placement reads beyond the revision id. */
interface RailEntry {
  revision: string;
  beyondWindow?: boolean;
}

/**
 * Every stop's placement, in the order the entries arrived.
 *
 * Two orderings decide the whole function, and both are the honest way round:
 *
 *  - **The marker wins over the tail.** A rollback can pin an environment to a
 *    revision the cluster's bounded history has forgotten, and that is exactly
 *    the case a reader opens this screen for. Fading the one stop that is live
 *    would hide the marker on the rail it is the point of.
 *  - **A live revision the list does not contain marks nothing.** `Status` can
 *    name a revision that is not among the recorded entries — an unreadable
 *    delivery plane answers nothing at all, and a truncated window can answer
 *    something this list has no row for — so the rail is `unmarked` rather than
 *    guessing which end of it the marker belongs on.
 */
export function railPlacements(
  entries: readonly RailEntry[],
  liveRevision: string,
): RailPlacement[] {
  const liveIndex =
    liveRevision === ""
      ? -1
      : entries.findIndex((entry) => entry.revision === liveRevision);
  return entries.map((entry, i) => {
    if (i === liveIndex) return "live";
    if (entry.beyondWindow === true) return "tail";
    if (liveIndex === -1) return "unmarked";
    return i < liveIndex ? "above" : "below";
  });
}

/** How much of a long hash is shown, e.g. a spec hash's or digest's hex half. */
const SHORT_LENGTH = 12;

/**
 * A spec hash or an artifact digest at reading length:
 * `sha256:0123456789ab`.
 *
 * Only an `algorithm:hex` spelling is shortened, and only when there is more
 * hex than the short form would show. Anything else is returned as it arrived —
 * the hash format is the server's business, and a value this does not recognise
 * is far more likely to be a format change than something safe to truncate.
 */
export function shortHash(hash: string): string {
  const match = /^([0-9a-z]+):([0-9a-f]+)$/i.exec(hash);
  if (match === null) return hash;
  const [, algorithm = "", hex = ""] = match;
  if (hex.length <= SHORT_LENGTH) return hash;
  return `${algorithm}:${hex.slice(0, SHORT_LENGTH)}`;
}

/**
 * `committed_at` as wall clock, in the shape the deploy stream's timestamps
 * already take on screen (components/format.ts's formatInstant): UTC, seconds,
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
