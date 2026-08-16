import { useEffect, useState } from "react";
import { Link } from "react-router-dom";

import "./Ticker.css";

import { formatInstant } from "../components/format";
import type { WatchState } from "../api/watch";
import { environmentPath } from "../pages/flows";
import {
  phaseMove,
  rowTone,
  sinceLabel,
  TICKER_LIMIT,
  type TickerEntry,
} from "./ring";

/**
 * The reconciliation ticker, drawn (#260).
 *
 * `ticker.ts` decides what a row is and what word it carries; this decides
 * nothing and only draws it. Four rules hold it where the Console put it:
 *
 * - **Absent when it is empty**, on every surface, and not an empty box with a
 *   reassuring sentence in it. The ring is memory only and starts empty on
 *   every load, so on a quiet instance the box would be the permanent state and
 *   the strip would teach a reader to stop looking at it. Nothing is the loudest
 *   way to say nothing has happened.
 * - **Rows are records, not statuses**, so a word is plain text and never a
 *   `StatusPill`. A pill says "this is the state of the thing now"; the third
 *   row down is four minutes old and is a statement about an instant that has
 *   passed. The environment's *current* word is the pill above the strip, and
 *   there must be exactly one object on screen making that claim.
 * - **One coloured word at most.** `rowTone` paints only the settled failures;
 *   everything else is ink. See `ticker.ts` for the argument.
 * - **No motion, and no second transport indicator.** Both screens that carry a
 *   ticker already have a `LiveIndicator` in their header, and that indicator
 *   is deliberately the smallest thing on the page — two of them saying the
 *   same word about the same stream would make the transport the loudest thing
 *   instead. What the strip adds is the one fact the indicator cannot carry:
 *   while the stream is down the rows are not merely stale, they are missing
 *   some. That is a sentence, it appears only when it is true, and it is the
 *   whole of the disconnect affordance here.
 *
 * # When a deploy is streaming
 *
 * The deploy screen is a route of its own (`/actions/deploy`) and renders its
 * own transitions as they land, so it and a ticker are never on one screen and
 * nothing is duplicated. The Overview's ticker is deliberately the *ambient*
 * telling of the same events: a deploy someone else started, or one started
 * from `kelson deploy`, moves these rows exactly as one started here does — and
 * that is the case the deploy screen cannot cover, because nobody has it open.
 * So there is no "a deploy is in progress" banner: the rows are the answer, and
 * inferring an in-flight deploy from a phase would be a second opinion beside
 * the phase rail that already draws it.
 */

/** How often the relative times are recomputed. */
const TICK_MS = 5_000;

export function Ticker({
  entries,
  state,
  limit = TICKER_LIMIT,
  subject = true,
  name = EYEBROW,
}: {
  entries: readonly TickerEntry[];
  /** The transport's state, which the strip reads only to say it has gaps. */
  state: WatchState;
  /** Rows drawn. Home takes fewer than the ring holds; see `ProjectsPage`. */
  limit?: number;
  /** False where the page already names the environment every row is about. */
  subject?: boolean;
  /**
   * The section's accessible name, which says *whose* activity this is. The
   * visible eyebrow stays the bare word: it is set uppercase and letterspaced,
   * and an environment's name is a name somebody chose rather than a label.
   */
  name?: string;
}) {
  // Before any hook, so an absent ticker also runs no clock. The rows are the
  // only thing this component has to say and there is no shell without them.
  if (entries.length === 0) return null;
  return (
    <TickerStrip
      entries={entries}
      state={state}
      limit={limit}
      subject={subject}
      name={name}
    />
  );
}

const EYEBROW = "Activity";

function TickerStrip({
  entries,
  state,
  limit,
  subject,
  name,
}: {
  entries: readonly TickerEntry[];
  state: WatchState;
  limit: number;
  subject: boolean;
  name: string;
}) {
  const now = useNow();
  return (
    <section className="k-ticker" aria-label={name}>
      <div className="k-eyebrow">{EYEBROW}</div>
      <ol className="k-ticker__rows">
        {entries.slice(0, limit).map((entry) => (
          <Row key={entry.key} entry={entry} now={now} subject={subject} />
        ))}
      </ol>
      {/* The one thing the header's indicator cannot say: while the stream is
          down the strip is not merely stale, it is incomplete. */}
      {state === "reconnecting" ? (
        <p className="k-ticker__gap">reconnecting — rows may be missing</p>
      ) : null}
    </section>
  );
}

function Row({
  entry,
  now,
  subject,
}: {
  entry: TickerEntry;
  now: number;
  subject: boolean;
}) {
  return (
    <li className="k-tick">
      <time
        className="k-mono k-tick__at"
        dateTime={new Date(entry.at).toISOString()}
        title={formatInstant(entry.at)}
      >
        {sinceLabel(now - entry.at)}
      </time>
      {/* Names people chose, so sans and not mono — and a link, because a row
          that reports a failure a reader has to go and look at should be the
          way there. */}
      {subject ? (
        <Link
          className="k-tick__subject"
          to={environmentPath(entry.project, entry.environment)}
        >
          {entry.project} · {entry.environment}
        </Link>
      ) : null}
      {/* Mono for the same reason the pill is: the word is the vocabulary's
          computed token, not a label somebody wrote. */}
      <span className="k-mono k-tick__word" data-tone={rowTone(entry.status)}>
        {entry.status.word}
      </span>
      <span className="k-mono k-tick__move">{phaseMove(entry)}</span>
      {entry.revision ? (
        <span className="k-mono k-tick__rev">{entry.revision}</span>
      ) : null}
    </li>
  );
}

/**
 * A clock that advances while the strip is mounted, so `12s` becomes `4m`
 * without the page being reloaded.
 *
 * It exists only inside `TickerStrip`, which exists only when there are rows:
 * an interval on an instance where nothing has happened would be a heartbeat
 * with nothing to update.
 */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(timer);
  }, []);
  return now;
}
