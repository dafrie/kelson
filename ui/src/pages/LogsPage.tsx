import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams, useSearchParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { isAbort } from "../api/errors";
import { useRun } from "../api/stream";
import type { WatchState } from "../api/watch";
import {
  backoffMs,
  GapGate,
  isTerminal,
  lastSeenUnixMs,
  LogBuffer,
} from "../logs/buffer";
import {
  compileFilter,
  podsOf,
  podTone,
  segments,
  visibleLines,
  type VisibleLine,
} from "../logs/filter";
import { logFileName, saveText, stamp, toText } from "../logs/text";
import type { LogLine } from "../gen/kelson/v1alpha1/logs_pb";
import { ErrorPanel } from "../components/ErrorPanel";
import { LiveIndicator } from "../components/LiveIndicator";
import { useEnvironment } from "./EnvironmentPage";
import "./LogsPage.css";

/**
 * Logs for one environment, in the engine's two shapes: a bounded Query and an
 * unbounded Follow (proto/kelson/v1alpha1/logs.proto).
 *
 * This is the Logs tab of the environment (#260), which is what it always was
 * in everything but placement: the path is unchanged, so every link that ever
 * pointed at a log tail still lands here, and the environment's name, its other
 * views and its actions are now the layout's above rather than a breadcrumb of
 * this screen's own.
 *
 * LogSelector wants a namespace and a component. The namespace is the server's
 * answer: StatusResponse carries the resolved one (#161), so the input prefills
 * from Status and a spec that sets `spec.namespace` is right without anyone
 * correcting it. Status needs a delivery plane, and a build started without one
 * answers Unimplemented — so the model's documented default
 * `<project>-<environment>` (docs/model.md) stays as the fallback for exactly
 * that case, and the field stays an editable input either way. The component
 * list is read out of the stored Project document, and `?component=<name>`
 * preselects one: that is the link the component page carries (#214), so
 * pressing Logs there arrives at that component's logs.
 *
 * The selector's wire field is still `application`: ADR-0032 finished the
 * label rename (pods now carry `kelson.dev/component`) but deliberately left
 * v1alpha1 field names alone (its decision D), so the request spells the wire
 * word while the screen says what the model says.
 *
 * The bounds rules are the engine's and are not duplicated here: a query must
 * carry tail, since or around, and around and tail are mutually exclusive. The
 * server rejects what it rejects and the reason is rendered verbatim.
 */
export function LogsPage() {
  const { project = "", env = "" } = useParams();
  const [params] = useSearchParams();
  const clients = useClients();

  // The stored documents come from the environment layout this tab sits in
  // (#260) rather than from a second `GetSpec`: the component list is the only
  // thing this screen wants out of them, and the layout has already read the
  // project to know that this environment exists.
  const { documents } = useEnvironment();

  // Status is read for one field: the resolved namespace. Its error is
  // deliberately not surfaced — a server with no delivery plane cannot answer
  // it, and that is not a failure of the log screen, only a reason to fall back
  // to the model's default.
  const status = useAsync(
    (signal) =>
      clients.deploy.status(
        { spec: { spec: { case: "project", value: project } }, environment: env },
        { signal },
      ),
    [clients, project, env],
  );

  const components = useMemo(() => {
    const doc = documents?.project;
    return doc ? componentNames(new TextDecoder().decode(doc)) : [];
  }, [documents]);

  const fallbackNamespace = `${project}-${env}`;
  const [namespace, setNamespace] = useState(fallbackNamespace);
  const [resolved, setResolved] = useState(false);
  // `?component=` is how the component page links here (#214):
  // the row a reader pressed is the component whose logs they want, and having
  // to pick it again on arrival would be the link failing to carry its own
  // subject. It seeds the field and nothing more — the field stays theirs.
  const [component, setComponent] = useState(() => params.get("component") ?? "");
  const [mode, setMode] = useState<"query" | "follow">("query");

  // The server's answer replaces the guess once, and only once: after that the
  // field belongs to whoever is typing in it.
  useEffect(() => {
    const ns = status.data?.namespace;
    if (!resolved && ns !== undefined && ns !== "") {
      setNamespace(ns);
      setResolved(true);
    }
  }, [status.data, resolved]);

  // The picker fills itself in once, from the parsed spec. It stays a free-text
  // input either way: a parse that found nothing must not lock the screen.
  useEffect(() => {
    const first = components[0];
    if (component === "" && first !== undefined) setComponent(first);
  }, [components, component]);

  return (
    <>
      <div className="k-panel k-logs__selector">
        <label className="k-field">
          <span className="k-eyebrow">Namespace</span>
          <input
            className="k-input k-mono"
            value={namespace}
            onChange={(e) => setNamespace(e.target.value)}
            placeholder={fallbackNamespace}
          />
          <span className="k-field__note k-mono">
            {resolved
              ? "resolved by the server from this environment's spec"
              : "the model's default for this pair — the server has not answered with the resolved one"}
          </span>
        </label>

        <label className="k-field">
          <span className="k-eyebrow">Component</span>
          <input
            className="k-input k-mono"
            value={component}
            onChange={(e) => setComponent(e.target.value)}
            list="k-components"
            placeholder="web"
          />
          <datalist id="k-components">
            {components.map((name) => (
              <option key={name} value={name} />
            ))}
          </datalist>
          <span className="k-field__note k-mono">
            {components.length > 0
              ? `from the stored Project document: ${components.join(", ")}`
              : "no components parsed from the stored spec — type the name"}
          </span>
        </label>
      </div>

      {/* The spec's failure is reported once, by the layout that asked for it
          (#260). Here it costs the picker its list and nothing else, which the
          note above already says — a second copy of the same panel would make
          one failure look like two. */}

      <nav className="k-tabs" aria-label="Log mode">
        <button
          type="button"
          className={mode === "query" ? "k-tab k-tab--active" : "k-tab"}
          aria-current={mode === "query" ? "true" : undefined}
          onClick={() => setMode("query")}
        >
          Query
        </button>
        <button
          type="button"
          className={mode === "follow" ? "k-tab k-tab--active" : "k-tab"}
          aria-current={mode === "follow" ? "true" : undefined}
          onClick={() => setMode("follow")}
        >
          Follow
        </button>
      </nav>

      {mode === "query" ? (
        <QueryLogs namespace={namespace} component={component} />
      ) : (
        <FollowLogs namespace={namespace} component={component} />
      )}
    </>
  );
}

/**
 * Workload names out of the stored Project document.
 *
 * A YAML-lite scan, deliberately: `components:` is a list of mappings whose
 * first key is `name` (docs/model.md), so the names are found by indentation
 * without a parser. It is a convenience for the picker and nothing more — the
 * field stays free text, so a spec this misses costs a reader one word of
 * typing rather than a broken screen.
 *
 * A data component has no pods and no logs, so an entry carrying `kind:
 * postgres` or `kind: valkey` is dropped: offering it in a log picker would
 * promise a stream that cannot exist (ADR-0014).
 */
export function componentNames(yaml: string): string[] {
  const names: string[] = [];
  const lines = yaml.split("\n");
  let indent = -1;
  let last: string | undefined;
  for (const line of lines) {
    if (/^\s*#/.test(line)) continue;
    const opens = /^(\s*)components:\s*$/.exec(line);
    if (opens?.[1] !== undefined) {
      indent = opens[1].length;
      continue;
    }
    if (indent < 0) continue;
    const item = /^(\s*)-\s+name:\s*("?)([A-Za-z0-9][A-Za-z0-9-]*)\2\s*$/.exec(line);
    if (item?.[1] !== undefined && item[1].length > indent && item[3]) {
      names.push(item[3]);
      last = item[3];
      continue;
    }
    // A data component is not a log source; drop the entry the `kind:` belongs
    // to rather than listing a name with no pods behind it.
    const dataKind = /^\s*kind:\s*("?)(postgres|valkey)\1\s*$/.exec(line);
    if (dataKind !== null && last !== undefined) {
      names.pop();
      last = undefined;
      continue;
    }
    // Dedenting to or past `components:` ends the block; blank lines and
    // deeper keys inside an item do not.
    const leading = /^(\s*)\S/.exec(line);
    if (leading?.[1] !== undefined && leading[1].length <= indent) {
      indent = -1;
      last = undefined;
    }
  }
  return [...new Set(names)];
}

function QueryLogs({
  namespace,
  component,
}: {
  namespace: string;
  component: string;
}) {
  const clients = useClients();
  const run = useRun();
  const [tail, setTail] = useState("200");
  const [sinceMinutes, setSinceMinutes] = useState("");
  const [match, setMatch] = useState("");
  const [isRegex, setIsRegex] = useState(false);
  const [lines, setLines] = useState<LogLine[] | undefined>(undefined);

  const query = useCallback(() => {
    setLines(undefined);
    run.start(async (signal) => {
      const minutes = Number(sinceMinutes);
      const res = await clients.log.queryLogs(
        {
          selector: { namespace, application: component },
          tail: Number(tail) || 0,
          sinceUnixMs:
            sinceMinutes !== "" && Number.isFinite(minutes) && minutes > 0
              ? BigInt(Date.now() - minutes * 60_000)
              : 0n,
          match: isRegex ? { regex: match } : { substring: match },
        },
        { signal },
      );
      setLines(res.lines);
    });
  }, [run, clients, namespace, component, tail, sinceMinutes, match, isRegex]);

  return (
    <>
      <div className="k-panel k-logs__controls">
        <label className="k-field k-field--narrow">
          <span className="k-eyebrow">Tail</span>
          <input
            className="k-input k-mono"
            value={tail}
            onChange={(e) => setTail(e.target.value)}
            inputMode="numeric"
          />
        </label>
        <label className="k-field k-field--narrow">
          <span className="k-eyebrow">Since (minutes)</span>
          <input
            className="k-input k-mono"
            value={sinceMinutes}
            onChange={(e) => setSinceMinutes(e.target.value)}
            inputMode="numeric"
            placeholder="—"
          />
        </label>
        <label className="k-field">
          <span className="k-eyebrow">Match</span>
          <input
            className="k-input k-mono"
            value={match}
            onChange={(e) => setMatch(e.target.value)}
            placeholder={isRegex ? "^ERROR" : "timeout"}
          />
        </label>
        <label className="k-check k-mono">
          <input
            type="checkbox"
            checked={isRegex}
            onChange={(e) => setIsRegex(e.target.checked)}
          />
          regex
        </label>
        <button
          type="button"
          className="k-button k-button--primary"
          onClick={query}
          disabled={run.running}
        >
          {run.running ? "Querying…" : "Run query"}
        </button>
      </div>

      {run.error !== undefined ? (
        <ErrorPanel title="The log query was rejected" error={run.error} />
      ) : null}

      {lines !== undefined ? (
        lines.length === 0 ? (
          <div className="k-panel k-panel--dim k-mono">
            no lines matched — the bounds held, nothing inside them said anything
          </div>
        ) : (
          // A bounded result is already the window the reader asked for, and
          // the match went to the server, so there is nothing left to narrow.
          <LogLines
            rows={lines.map((line, index) => ({ line, ranges: [], index }))}
            keyBase={0}
          />
        )
      ) : null}
    </>
  );
}


/**
 * The retained tail. Ten thousand lines is minutes of a chatty workload and a
 * few megabytes; past it the oldest go, and the count of what went is on
 * screen. A live tail is a window on something unbounded, and the only
 * dishonest window is one that pretends otherwise.
 */
const RETAINED_LINES = 10_000;

/**
 * How much of the tail is in the DOM.
 *
 * The cap bounds memory; this bounds *layout*, which is the thing that actually
 * freezes a tab (#64's acceptance criterion). Ten thousand log rows is tens of
 * thousands of elements to style and reflow on every batch, and no amount of
 * batching makes that free. The buffer above is what copy, download and the
 * filter read; this is what the browser draws.
 */
const RENDERED_LINES = 2_000;

/** The bounded Query that fills a reconnect gap. */
const BACKFILL_TAIL = 1_000;

/**
 * A live tail that survives volume, a paused reader and a dropped connection.
 *
 * Four decisions worth knowing before reading the code:
 *
 *   - **Pause buffers; it does not drop, and it does not close the stream.**
 *     Closing would make "pause" mean "lose whatever happens while you read the
 *     line you paused for", which is the opposite of why anyone pauses; and
 *     dropping would make it mean the same thing while looking like it didn't.
 *     So the stream stays up, the arriving lines go into a second capped
 *     buffer, and the pill says how many are waiting. Resuming appends them and
 *     returns to the tail.
 *   - **Lines arrive faster than a screen can be painted, so they are batched.**
 *     Every event lands in a queue and one `requestAnimationFrame` drains it —
 *     one re-render per frame no matter how many lines that was. The buffers
 *     are refs, not state: making them state would copy the whole window on
 *     every batch, which is the cost the cap exists to avoid. A version counter
 *     is what tells React something changed.
 *   - **A dropped stream is reconnected, and the gap is filled.** The screen
 *     remembers the newest timestamp it has seen, backfills the gap with a
 *     bounded Query from that instant, then re-follows from it. Both ends
 *     replay the boundary instant, and `GapGate` (src/logs/buffer.ts) is what
 *     stops the replay from doubling on screen.
 *   - **The find box is client-side.** See the note in src/logs/filter.ts: a
 *     match sent upstream restarts the stream and discards non-matching lines
 *     permanently, so a live tail filters what it has retained instead. The
 *     server-side `LogMatch` is still what the bounded Query mode sends.
 */
function FollowLogs({
  namespace,
  component,
}: {
  namespace: string;
  component: string;
}) {
  const clients = useClients();

  // Three capped buffers: what is on screen, what arrived while paused, and
  // what has arrived since the last frame. All three are bounded, so a tab left
  // open on a screaming workload — or backgrounded, where the browser stops
  // firing frames altogether — holds a fixed amount of memory rather than
  // however much the workload felt like producing.
  const retained = useRef(new LogBuffer(RETAINED_LINES));
  const held = useRef(new LogBuffer(RETAINED_LINES));
  const pending = useRef(new LogBuffer(RETAINED_LINES));
  // Lines forgotten by the queue and the paused buffer. The retained tail keeps
  // its own count; these two reset when they drain, so they are summed here.
  const forgotten = useRef(0);
  const frame = useRef<number | undefined>(undefined);
  const [version, setVersion] = useState(0);
  const bump = useCallback(() => setVersion((v) => v + 1), []);

  // 0 is "not following". Incrementing it starts a stream; the effect below is
  // keyed on it, so a restart is one state change rather than a lifecycle.
  const [session, setSession] = useState(0);
  const [phase, setPhase] = useState<WatchState>("off");
  const [error, setError] = useState<unknown>(undefined);
  const [dropped, setDropped] = useState(0);
  const [paused, setPaused] = useState(false);
  const pausedNow = useRef(false);
  const [query, setQuery] = useState("");
  const [isRegex, setIsRegex] = useState(false);
  const [podFilter, setPodFilter] = useState<ReadonlySet<string>>(new Set());
  const [pinned, setPinned] = useState(true);
  const [copyState, setCopyState] = useState<"idle" | "copied" | "blocked">(
    "idle",
  );
  const box = useRef<HTMLDivElement | null>(null);

  // The stream reads these at connect time, not at render time: a reconnect
  // must not pick up a namespace someone is halfway through typing, and a
  // parent re-render must not tear the stream down.
  const target = useRef({ namespace, application: component });
  target.current = { namespace, application: component };

  const flush = useCallback(() => {
    frame.current = undefined;
    const queue = pending.current;
    if (queue.length === 0) return;
    forgotten.current += queue.evicted;
    const batch = queue.drain();
    if (pausedNow.current) forgotten.current += held.current.append(batch);
    else retained.current.append(batch);
    bump();
  }, [bump]);

  const schedule = useCallback(() => {
    if (frame.current !== undefined) return;
    frame.current = requestAnimationFrame(flush);
  }, [flush]);

  /** Straight into the buffer: history, not a live batch, and never per line. */
  const accept = useCallback(
    (lines: readonly LogLine[]) => {
      if (lines.length === 0) return;
      if (pausedNow.current) forgotten.current += held.current.append(lines);
      else retained.current.append(lines);
      bump();
    },
    [bump],
  );

  useEffect(
    () => () => {
      if (frame.current !== undefined) cancelAnimationFrame(frame.current);
    },
    [],
  );

  useEffect(() => {
    if (session === 0) return;
    const controller = new AbortController();
    let stopped = false;
    let attempt = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;

    // Everything the reader has, in arrival order — the paused buffer is part
    // of the position, or pausing across a reconnect would refetch what is
    // already waiting behind the pill.
    const history = () => [...retained.current.lines, ...held.current.lines];

    const read = async () => {
      // Anything still queued is part of the position too.
      if (frame.current !== undefined) cancelAnimationFrame(frame.current);
      flush();

      const since = lastSeenUnixMs(history());
      if (since > 0n) {
        // The gap has two ends, so it is a bounded Query and not a follow.
        const backfill = await clients.log.queryLogs(
          {
            selector: { ...target.current },
            tail: BACKFILL_TAIL,
            sinceUnixMs: since,
          },
          { signal: controller.signal },
        );
        accept(new GapGate(history(), since).admitAll(backfill.lines));
      }
      // `since` is inclusive server-side, so the new Follow replays the
      // boundary instant — including whatever the backfill just added, which is
      // why this gate is built after it rather than reused from it.
      const gate = since > 0n ? new GapGate(history(), since) : undefined;

      setPhase("live");
      for await (const res of clients.log.followLogs(
        { selector: { ...target.current }, sinceUnixMs: since },
        { signal: controller.signal },
      )) {
        attempt = 0;
        setPhase("live");
        const event = res.event;
        if (event.case === "line") {
          if (gate === undefined || gate.admit(event.value)) {
            pending.current.push(event.value);
            schedule();
          }
        } else if (event.case === "dropped") {
          setDropped(Number(event.value));
        }
      }
    };

    const retry = () => {
      if (stopped) return;
      setPhase("reconnecting");
      timer = setTimeout(run, backoffMs(attempt));
      attempt += 1;
    };

    const run = () => {
      read()
        // A stream that ended cleanly is a server that went away — the same
        // event as a failed one, and answered the same way.
        .then(retry)
        .catch((err: unknown) => {
          if (stopped || controller.signal.aborted || isAbort(err)) return;
          if (isTerminal(err)) {
            // A rejected selector, a build with no delivery plane, a session
            // that expired: none of these change on a second identical
            // request, and retrying would replace the reason with a spinner.
            stopped = true;
            setError(err);
            setPhase("off");
            setSession(0);
            return;
          }
          retry();
        });
    };
    run();

    return () => {
      stopped = true;
      if (timer !== undefined) clearTimeout(timer);
      controller.abort();
    };
  }, [clients, session, accept, flush, schedule]);

  const start = useCallback(() => {
    retained.current.clear();
    held.current.clear();
    pending.current.clear();
    forgotten.current = 0;
    pausedNow.current = false;
    setPaused(false);
    setPinned(true);
    setDropped(0);
    setError(undefined);
    setPodFilter(new Set());
    setSession((s) => s + 1);
    bump();
  }, [bump]);

  // Stopping keeps the buffer. The lines are still the answer to whatever
  // question they were opened for, and searching, copying and saving them are
  // all things a reader does *after* deciding they have seen enough.
  const stop = useCallback(() => {
    setSession(0);
    setPhase("off");
  }, []);

  const togglePause = useCallback(() => {
    if (pausedNow.current) {
      pausedNow.current = false;
      setPaused(false);
      retained.current.append(held.current.drain());
      setPinned(true);
    } else {
      pausedNow.current = true;
      setPaused(true);
    }
    bump();
  }, [bump]);

  const onScroll = useCallback(() => {
    const el = box.current;
    if (!el) return;
    // Auto-scroll, but only while the reader is at the bottom. Scrolling up is
    // how a person reads a line that just went past, and yanking them back down
    // on the next line makes a live tail unreadable.
    setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 24);
  }, []);

  const filtering = useMemo(() => compileFilter(query, isRegex), [query, isRegex]);
  const lines = retained.current.lines;
  const pods = useMemo(() => podsOf(lines), [lines, version]);
  const visible = useMemo(
    () => visibleLines(lines, filtering, podFilter),
    [lines, version, filtering, podFilter],
  );
  const rows =
    visible.length > RENDERED_LINES ? visible.slice(-RENDERED_LINES) : visible;
  const waiting = held.current.length;
  const missing = forgotten.current + retained.current.evicted;

  useEffect(() => {
    const el = box.current;
    if (el && pinned && !paused) el.scrollTop = el.scrollHeight;
  }, [version, pinned, paused]);

  const togglePod = useCallback((pod: string) => {
    setPodFilter((prev) => {
      const next = new Set(prev);
      if (!next.delete(pod)) next.add(pod);
      return next;
    });
  }, []);

  const copyVisible = useCallback(() => {
    const clipboard = navigator.clipboard;
    if (!clipboard) {
      setCopyState("blocked");
      return;
    }
    clipboard.writeText(toText(visible.map((row) => row.line))).then(
      () => {
        setCopyState("copied");
        setTimeout(() => setCopyState("idle"), 1200);
      },
      () => setCopyState("blocked"),
    );
  }, [visible]);

  const download = useCallback(() => {
    const { namespace: ns, application: name } = target.current;
    saveText(logFileName(ns, name, new Date()), toText(retained.current.lines));
  }, []);

  return (
    <>
      <div className="k-panel k-logs__controls">
        {session === 0 ? (
          <button
            type="button"
            className="k-button k-button--primary"
            onClick={start}
          >
            Start following
          </button>
        ) : (
          <button type="button" className="k-button" onClick={stop}>
            Stop following
          </button>
        )}
        <button
          type="button"
          className="k-button"
          onClick={togglePause}
          disabled={session === 0}
          aria-pressed={paused}
        >
          {paused ? "Resume" : "Pause"}
        </button>
        <label className="k-field">
          <span className="k-eyebrow">Find</span>
          <input
            className="k-input k-mono"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={isRegex ? "^ERROR" : "timeout"}
            aria-label="Find in the retained lines"
          />
        </label>
        <label className="k-check k-mono">
          <input
            type="checkbox"
            checked={isRegex}
            onChange={(e) => setIsRegex(e.target.checked)}
          />
          regex
        </label>
        <LiveIndicator state={phase} />
      </div>

      {filtering.kind === "invalid" ? (
        <p className="k-logs__invalid k-mono">
          not a regular expression: {filtering.message}
        </p>
      ) : null}

      <div className="k-logs__toolbar">
        {pods.length > 1 ? (
          <div className="k-logs__pods" role="group" aria-label="Filter by pod">
            {pods.map((pod) => (
              <button
                key={pod}
                type="button"
                className="k-logs__pod k-mono"
                data-tone={podTone(pod)}
                aria-pressed={podFilter.has(pod)}
                onClick={() => togglePod(pod)}
              >
                {pod}
              </button>
            ))}
            {podFilter.size > 0 ? (
              <button
                type="button"
                className="k-logs__pod k-logs__pod--all k-mono"
                onClick={() => setPodFilter(new Set())}
              >
                every pod
              </button>
            ) : null}
          </div>
        ) : null}

        <div className="k-logs__actions">
          <button
            type="button"
            className="k-button"
            onClick={copyVisible}
            disabled={visible.length === 0}
          >
            {copyState === "copied"
              ? "Copied"
              : copyState === "blocked"
                ? "Clipboard blocked"
                : `Copy ${visible.length === lines.length ? "all" : "matching"}`}
          </button>
          <button
            type="button"
            className="k-button"
            onClick={download}
            disabled={lines.length === 0}
          >
            Download .txt
          </button>
        </div>
      </div>

      <p className="k-logs__note k-mono">
        {session === 0 && lines.length === 0 ? (
          `an unbounded stream — it runs until you stop it or leave, and the last ${RETAINED_LINES} lines are kept`
        ) : (
          <>
            {visible.length === lines.length
              ? `${lines.length} ${lines.length === 1 ? "line" : "lines"} retained`
              : `${visible.length} of ${lines.length} retained lines shown`}
            {rows.length < visible.length
              ? ` · the last ${RENDERED_LINES} are drawn`
              : ""}
            {missing > 0
              ? ` · ${missing} older ${missing === 1 ? "line has" : "lines have"} left the ${RETAINED_LINES}-line window`
              : ""}
          </>
        )}
      </p>

      {error !== undefined ? (
        <ErrorPanel title="The log stream failed" error={error} />
      ) : null}

      {dropped > 0 ? (
        <div className="k-logs__dropped" role="alert">
          <span className="k-logs__dropped-title">
            {dropped} {dropped === 1 ? "line" : "lines"} dropped
          </span>
          <span className="k-mono">
            the stream fell behind under backpressure — this gap is loss, not
            silence
          </span>
        </div>
      ) : null}

      {paused ? (
        <button type="button" className="k-logs__resume" onClick={togglePause}>
          {waiting > 0
            ? `${waiting} new ${waiting === 1 ? "line" : "lines"} · resume`
            : "paused · resume"}
        </button>
      ) : null}

      {lines.length > 0 || session > 0 ? (
        <LogLines
          rows={rows}
          keyBase={retained.current.evicted}
          boxRef={box}
          onScroll={onScroll}
        />
      ) : null}
    </>
  );
}

function LogLines({
  rows,
  keyBase,
  boxRef,
  onScroll,
}: {
  rows: readonly VisibleLine[];
  /**
   * How many lines the buffer has already evicted. Added to a row's index it
   * gives every line an identity that survives eviction, filtering and
   * scrolling, so React reuses rows instead of rebuilding the list per frame.
   */
  keyBase: number;
  boxRef?: React.RefObject<HTMLDivElement | null>;
  onScroll?: () => void;
}) {
  return (
    <div
      className="k-logs__box"
      ref={boxRef ?? null}
      onScroll={onScroll}
      role="log"
    >
      {rows.map(({ line, ranges, index }) => (
        <div className="k-logline" key={keyBase + index}>
          <span className="k-logline__time">{stamp(line.timestampUnixMs)}</span>
          <span className="k-logline__pod" data-tone={podTone(line.pod)}>
            {line.pod}
          </span>
          <span className="k-logline__container">{line.container}</span>
          <span className="k-logline__message">
            {ranges.length === 0
              ? line.message
              : segments(line.message, ranges).map((part, at) =>
                  part.hit ? (
                    <mark className="k-logs__hit" key={at}>
                      {part.text}
                    </mark>
                  ) : (
                    <span key={at}>{part.text}</span>
                  ),
                )}
          </span>
        </div>
      ))}
    </div>
  );
}
