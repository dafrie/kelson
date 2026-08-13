import { Code, ConnectError } from "@connectrpc/connect";

import type { LogLine } from "../gen/kelson/v1alpha1/logs_pb";

/**
 * What a live log tail has to survive: volume, and the connection going away.
 *
 * `LogService.Follow` is the deliberate unbounded stream (ADR-0008). Unbounded
 * is a property of the *stream*, not of the tab reading it: a workload logging
 * a thousand lines a second fills a browser's heap in minutes and freezes the
 * page long before that. Everything here exists so the screen holds a bounded,
 * honest window on an endless thing (#64).
 *
 * Three pieces, all of them pure so they can be tested without a DOM:
 *
 *   - `LogBuffer` — a capped tail. Constant memory by construction: past the
 *     cap the oldest lines are evicted, and the count of what was evicted is
 *     kept, because a view that silently forgets is a view that lies.
 *   - `GapGate` — the thing that makes a reconnect idempotent. See its doc.
 *   - `lastSeenUnixMs` / `isTerminal` — the two facts the reconnect loop needs:
 *     where to resume from, and whether resuming is worth trying at all.
 */

/**
 * A tail of at most `cap` lines.
 *
 * Mutable on purpose. The alternative — a new array per batch — copies the
 * whole window on every frame, which is precisely the cost this cap exists to
 * avoid; the screen re-renders off a version counter instead of off identity.
 */
export class LogBuffer {
  readonly cap: number;
  private buf: LogLine[] = [];
  /** Index of the oldest live line: everything before it has been evicted. */
  private head = 0;
  /** Lines this buffer dropped off the front to stay inside the cap. */
  private lost = 0;

  constructor(cap: number) {
    if (cap < 1) throw new Error("a log buffer holds at least one line");
    this.cap = cap;
  }

  /**
   * The live window, contiguous.
   *
   * Compacting the evicted prefix happens here rather than in `push`, which is
   * what keeps a line's arrival amortised O(1): the screen reads this once per
   * frame, and once per frame is exactly when the memmove is affordable.
   */
  get lines(): readonly LogLine[] {
    this.compact();
    return this.buf;
  }

  get length(): number {
    return this.buf.length - this.head;
  }

  /** How many lines fell out of the window. Not loss on the wire — see below. */
  get evicted(): number {
    return this.lost;
  }

  /** One line, the streaming hot path. Returns 1 if it evicted the oldest. */
  push(line: LogLine): number {
    this.buf.push(line);
    if (this.buf.length - this.head <= this.cap) return 0;
    this.head += 1;
    this.lost += 1;
    // Half the array is dead weight at worst: compact when the evicted prefix
    // has grown to the cap, so the cost is one memmove per cap lines.
    if (this.head >= this.cap) this.compact();
    return 1;
  }

  /** Appends a batch, evicting from the front. Returns how many were evicted. */
  append(incoming: readonly LogLine[]): number {
    if (incoming.length === 0) return 0;
    // A batch larger than the cap can only ever contribute its own tail;
    // pushing all of it first would spike memory to exactly the size this class
    // exists to bound.
    const start = Math.max(0, incoming.length - this.cap);
    this.lost += start;
    let dropped = start;
    for (let i = start; i < incoming.length; i += 1) {
      const line = incoming[i];
      if (line !== undefined) dropped += this.push(line);
    }
    return dropped;
  }

  /** Empties the buffer and forgets what it evicted: a new tail, not a resumed one. */
  clear(): void {
    this.buf = [];
    this.head = 0;
    this.lost = 0;
  }

  /** Removes and returns everything held, leaving the buffer empty. */
  drain(): LogLine[] {
    const out = this.buf.slice(this.head);
    this.clear();
    return out;
  }

  private compact(): void {
    if (this.head === 0) return;
    this.buf.splice(0, this.head);
    this.head = 0;
  }
}

/**
 * The identity of a line, for the one question a reconnect asks: have I already
 * shown this?
 *
 * A `LogLine` carries no ID — timestamp, pod, container and message are the
 * whole of it — so those four *are* the identity. The separator is NUL because
 * it is the one byte a container's stdout cannot put in the middle of a line.
 */
export function dedupKey(line: LogLine): string {
  return `${line.timestampUnixMs}\u0000${line.pod}\u0000${line.container}\u0000${line.message}`;
}

/**
 * The newest instant the reader has actually seen, or 0 if there is none.
 *
 * A zero timestamp means the engine could not parse one off the line, not 1970,
 * so such a line cannot advance the resume point — it is skipped here and
 * handled by `GapGate` instead.
 */
export function lastSeenUnixMs(lines: readonly LogLine[]): bigint {
  let newest = 0n;
  for (let i = 0; i < lines.length; i += 1) {
    const ts = lines[i]?.timestampUnixMs ?? 0n;
    if (ts > newest) newest = ts;
  }
  return newest;
}

/**
 * Makes reconnecting idempotent.
 *
 * When Follow drops, the screen resumes from the last timestamp it saw — first
 * a bounded `Query` to backfill the gap, then a new `Follow` from the same
 * instant. Both are inclusive of that instant, deliberately: excluding it would
 * lose every line sharing the last millisecond. So both replay a little, and
 * this gate is what stops the replay from doubling on screen.
 *
 * It is a *multiset* of the keys already retained at or after the resume point,
 * consumed as matches arrive: two identical lines held means two admitted-as-
 * seen, and a third genuine repeat still gets through. The gate closes when the
 * multiset empties, so it can never drop more lines than it was holding keys
 * for.
 *
 * Two honest limits, since exactness is not available:
 *
 *   - A line the engine could not timestamp (`timestamp_unix_ms == 0`) cannot
 *     be located in time. Such lines *are* keyed if they were retained in the
 *     window, so a replayed one is caught; one that arrived before the window
 *     and is replayed after it will show twice.
 *   - A workload that emits a byte-identical message twice from the same
 *     container inside the same millisecond, inside the resume window, has its
 *     second copy suppressed. That is a duplicate-looking line lost rather than
 *     a line invented, which is the direction to err.
 */
export class GapGate {
  private readonly seen = new Map<string, number>();
  private remaining = 0;

  /**
   * @param retained lines already on screen, in arrival order
   * @param since the resume point both the backfill query and the new follow use
   */
  constructor(retained: readonly LogLine[], since: bigint) {
    // The window is a suffix of the retained lines: everything from the first
    // line at or after the resume point onwards. Taking it by position rather
    // than by timestamp keeps the untimestamped lines interleaved in it.
    let start = retained.length;
    for (let i = 0; i < retained.length; i += 1) {
      const ts = retained[i]?.timestampUnixMs ?? 0n;
      if (ts > 0n && ts >= since) {
        start = i;
        break;
      }
    }
    for (let i = start; i < retained.length; i += 1) {
      const line = retained[i];
      if (line === undefined) continue;
      const key = dedupKey(line);
      this.seen.set(key, (this.seen.get(key) ?? 0) + 1);
      this.remaining += 1;
    }
  }

  /** False once the gate has nothing left to suppress. */
  get open(): boolean {
    return this.remaining > 0;
  }

  /** True if this line is new and should be appended. */
  admit(line: LogLine): boolean {
    if (this.remaining === 0) return true;
    const key = dedupKey(line);
    const count = this.seen.get(key);
    if (count === undefined || count === 0) return true;
    if (count === 1) this.seen.delete(key);
    else this.seen.set(key, count - 1);
    this.remaining -= 1;
    return false;
  }

  /** The lines of `incoming` that are new, in order. */
  admitAll(incoming: readonly LogLine[]): LogLine[] {
    const out: LogLine[] = [];
    for (const line of incoming) if (this.admit(line)) out.push(line);
    return out;
  }
}

/**
 * Whether a failed stream is worth reopening.
 *
 * The default is yes: a dropped connection, a restarted server and a stream
 * that simply ended are all the same transient thing, and the point of the
 * retry loop is that none of them need a human. The exceptions are the answers
 * that will not change on a second identical request — a build with no delivery
 * plane (`Unimplemented`), a selector the engine rejects (`InvalidArgument`), a
 * namespace that is not there, a session that is gone. Retrying those forever
 * would hammer the server and leave "reconnecting…" on screen in place of the
 * reason.
 */
export function isTerminal(err: unknown): boolean {
  const code = ConnectError.from(err).code;
  return (
    code === Code.Unimplemented ||
    code === Code.InvalidArgument ||
    code === Code.NotFound ||
    code === Code.PermissionDenied ||
    code === Code.Unauthenticated ||
    code === Code.FailedPrecondition
  );
}

/** Capped exponential backoff, shared with the event watch's shape (src/api/watch.ts). */
export function backoffMs(attempt: number, base = 1_000, cap = 30_000): number {
  return Math.min(cap, base * 2 ** Math.max(0, attempt));
}
