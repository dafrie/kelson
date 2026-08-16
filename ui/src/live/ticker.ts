import { useCallback, useEffect, useRef, useState } from "react";

import { formatAge } from "../components/format";
import { statusForPhase, type Status } from "../components/status";
import type { StatusKind } from "../components/StatusPill";
import type { WatchResponse_Event } from "../gen/kelson/v1alpha1/events_pb";

/**
 * The reconciliation ticker: what the cluster is doing, as it does it (#260).
 *
 * Every other live surface in this UI answers "what is the state now" — the
 * pill, the phase rail, the drift mark, the verdict rows. They are all present
 * tense, and each new answer erases the last one. Nothing keeps the *sequence*,
 * so an environment that went `Committed → Reconciling → Rejected` while a
 * reader was looking at another tab shows one red pill and no story.
 *
 * This is that story, bounded to what a person can actually read: the last
 * {@link TICKER_LIMIT} status transitions the open stream delivered, newest
 * first. It is the ambient half of what the deploy screen already shows for one
 * deployment it started itself, in the same vocabulary.
 *
 * # It reads the stream, it does not re-interpret it
 *
 * A row's word is `statusForPhase(transition.phase)`, which is exactly what a
 * pill on the same transition lands on: `pages/matrix.ts`'s `deliveryFacts`
 * voids the fetched `answer` when a transition arrives, so `statusForDelivery`
 * falls through to the phase derivation. That agreement is the point. The
 * ticker and the pill are two renderings of one fact, and a row that called the
 * same transition something else would be a second opinion on screen at the
 * same time as the first.
 *
 * # What the stream cannot tell it
 *
 * `WatchResponse.StatusTransition` carries four fields — phase, previous phase,
 * revision, cause — and the event around it carries the pair it belongs to and
 * `at_unix_ms`. So a row can say when, where, which phases and which revision,
 * and it cannot say:
 *
 *   - **who or what caused it.** There is no actor on the wire, and a deploy
 *     started from this browser, from `kelson deploy` and by a controller
 *     re-reconciling are indistinguishable here.
 *   - **which component moved.** A transition is an environment's, not a
 *     component's; only `HealthChange` is per workload.
 *   - **the engine's answer.** `StatusTransition` has no `answer` field, which
 *     is why the word is derived — see above.
 *   - **how far behind the spec the new revision is.** `stale` is not on this
 *     message at all.
 *
 * # Two clocks, and which one a row is printed against
 *
 * `at_unix_ms` is the *server's* clock (internal/api's broker stamps it) and
 * `Date.now()` is the browser's, so "12s ago" is an elapsed time computed
 * across two machines. It is still the right field to read: a client that
 * reconnects inside the retained window is replayed events that are genuinely
 * minutes old, and printing those against receipt time would date every one of
 * them to now. The skew is handled by {@link sinceLabel} clamping at zero — a
 * server whose clock is ahead prints `now`, never a time in the future — and an
 * event carrying no timestamp at all falls back to when this browser received
 * it.
 *
 * # What is deliberately not in it
 *
 * `HEALTH_CHANGE` events. They are already applied in place on both screens
 * that watch — the verdict row moves, the attention band opens — so a ticker
 * row would be the same news twice, and `HealthChange` carries no remediation
 * and no `stuck`, so the row would be the poorer of the two tellings. A ticker
 * of two row shapes is also two things, and this one is about delivery.
 */

/**
 * How many rows are kept. Memory only: nothing is persisted, the ring starts
 * empty on every load, and the server retains no history a page could backfill
 * from — `WatchRequest.cursor` resumes a window, it does not fetch a past.
 */
export const TICKER_LIMIT = 20;

/** One transition, as a row. */
export interface TickerEntry {
  /** The stream's own cursor, which is unique and is therefore the React key. */
  key: string;
  /** Milliseconds since the epoch: the server's stamp, or receipt time. */
  at: number;
  project: string;
  environment: string;
  /** The phase this transition left. Empty when the stream sent none. */
  from: string;
  phase: string;
  /** The revision it moved to. Empty when none was reported. */
  revision: string;
  /** The shared vocabulary's word for `phase`, and its tone. */
  status: Status;
}

/** Which (project, environment) a ticker is about, or every one of them. */
export interface TickerScope {
  project: string;
  environment: string;
}

/**
 * A wire event as a row, or nothing when it is not a transition.
 *
 * `receivedAt` is passed in rather than read from a clock so this stays a
 * function of its arguments; the hook below supplies `Date.now()`.
 */
export function entryFor(
  event: WatchResponse_Event,
  receivedAt: number,
): TickerEntry | undefined {
  const payload = event.payload;
  if (payload.case !== "statusTransition") return undefined;
  const { phase, previousPhase, revision } = payload.value;
  const stamped = Number(event.atUnixMs);
  return {
    key: event.cursor,
    at: Number.isFinite(stamped) && stamped > 0 ? stamped : receivedAt,
    project: event.project,
    environment: event.environment,
    from: previousPhase,
    phase,
    revision,
    status: statusForPhase(phase),
  };
}

/**
 * The ring with one more row on the front of it.
 *
 * Newest first, because the newest row is the one being read and a ticker that
 * grows downward moves it off the bottom of the strip. Bounded, because this is
 * a strip on a page and not a log: twenty is what fits under a heading without
 * becoming the page.
 *
 * A key already in the ring is dropped rather than repeated. A reconnect
 * resumes *after* the last cursor, so this should never fire against
 * kelson-server; it costs one lookup and it means a stream that redelivers
 * cannot print the same transition twice.
 */
export function pushEntry(
  ring: readonly TickerEntry[],
  entry: TickerEntry,
  limit = TICKER_LIMIT,
): TickerEntry[] {
  if (ring.some((held) => held.key === entry.key)) return [...ring];
  return [entry, ...ring].slice(0, limit);
}

/** Whether a row belongs to the ticker's scope. No scope is every scope. */
export function inScope(
  entry: TickerEntry,
  scope: TickerScope | undefined,
): boolean {
  if (scope === undefined) return true;
  return (
    entry.project === scope.project && entry.environment === scope.environment
  );
}

/**
 * The phase movement, as the row prints it.
 *
 * A transition is emitted for a change of phase *or* of revision, so the two
 * phases are sometimes the same one — an environment republished at a new
 * revision sits in `Healthy` on both sides of it. `Healthy → Healthy` claims a
 * movement that did not happen, so the arrow appears only when there was one.
 */
export function phaseMove(entry: TickerEntry): string {
  const moved = entry.from !== "" && entry.from !== entry.phase;
  return moved ? `${entry.from} → ${entry.phase}` : entry.phase;
}

/**
 * The tones a row may paint its word in, which is two of the six.
 *
 * The Console's badge has three loudness tiers and a ticker is the surface that
 * makes the argument for them plainest: twenty rows of blue `deploying` is
 * twenty coloured objects reporting that everything is normal. So the quiet and
 * working tiers are ink here, and only the loud tier — the settled failures —
 * keeps its hue. Nothing else on a row is ever coloured, so one red word in a
 * grey strip is the only thing in it.
 *
 * The colour, when there is one, is still the tone `status.ts` assigned. This
 * decides *whether* a word is painted, never in what.
 */
const LOUD: ReadonlySet<StatusKind> = new Set<StatusKind>(["degraded", "failed"]);

export function rowTone(status: Status): StatusKind | undefined {
  return LOUD.has(status.tone) ? status.tone : undefined;
}

/**
 * How long ago, in the age vocabulary the CLI prints (`components/format.ts`).
 *
 * Under five seconds is `now`: a row that lands while it is being read should
 * not count `1s`, `2s`, `3s` at a reader, and the ticker's clock only advances
 * every few seconds anyway. Negative is `now` too — that is the server's clock
 * running ahead of the browser's, and the honest floor for "how long ago" is
 * zero rather than a time in the future.
 */
export function sinceLabel(elapsedMs: number): string {
  if (!Number.isFinite(elapsedMs) || elapsedMs < 5_000) return "now";
  return formatAge(BigInt(Math.floor(elapsedMs / 1_000)));
}

/**
 * The ring, fed from a screen's existing `useWatch` (#76).
 *
 * It deliberately opens no stream of its own. Both screens that show a ticker
 * already watch — one scope on the environment's Overview, every watched pair on
 * home — and the ticker is another reader of that same `onEvent`, not a second
 * subscription to the same events.
 *
 * `record` is stable, because the pages hand their `onEvent` to `useWatch` and a
 * callback that changed identity on every row would tear the stream down and
 * reopen it. The scope is read through a ref for the same reason.
 *
 * # Nothing clears it except a change of subject
 *
 * A Resync does not. It says the client's *view* may have gaps, not that what
 * the client already saw was false: every row held is a transition that really
 * happened at the instant it names, and dropping them would destroy true
 * information to represent an absence the strip never claimed to fill. The
 * ticker is the last twenty transitions it was told about and has never claimed
 * to be the last twenty that occurred.
 *
 * A change of scope does. `/projects/:project/:env` keeps one component
 * instance across a change of `:env`, so without this an environment's ticker
 * would open holding the previous environment's rows.
 */
export function useTicker(scope?: TickerScope): {
  entries: TickerEntry[];
  record: (event: WatchResponse_Event) => void;
} {
  const [entries, setEntries] = useState<TickerEntry[]>([]);
  const held = useRef(scope);
  held.current = scope;

  const record = useCallback((event: WatchResponse_Event) => {
    const entry = entryFor(event, Date.now());
    if (entry === undefined || !inScope(entry, held.current)) return;
    setEntries((ring) => pushEntry(ring, entry));
  }, []);

  const subject = scope === undefined ? "" : scopeKey(scope);
  useEffect(() => setEntries([]), [subject]);

  return { entries, record };
}

function scopeKey(scope: TickerScope): string {
  return `${scope.project}/${scope.environment}`;
}
