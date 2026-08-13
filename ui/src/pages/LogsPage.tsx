import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { useRun } from "../api/stream";
import type { LogLine } from "../gen/kelson/v1alpha1/logs_pb";
import { ErrorPanel } from "../components/ErrorPanel";
import "./LogsPage.css";

/**
 * Logs for one environment, in the engine's two shapes: a bounded Query and an
 * unbounded Follow (proto/kelson/v1alpha1/logs.proto).
 *
 * LogSelector wants a namespace and an application, and the UI has neither
 * resolved for it: resolving a spec is the server's job and no RPC exposes the
 * resolved namespace. So the namespace is an input, prefilled with the model's
 * documented default `<project>-<environment>` (docs/model.md — "Namespace is
 * the target namespace; default <project>-<environment>"), and the application
 * list is read out of the stored Project document. A spec that sets an explicit
 * `spec.namespace` needs that field corrected, which is why it is an editable
 * input and not a caption.
 *
 * The bounds rules are the engine's and are not duplicated here: a query must
 * carry tail, since or around, and around and tail are mutually exclusive. The
 * server rejects what it rejects and the reason is rendered verbatim.
 */
export function LogsPage() {
  const { project = "", env = "" } = useParams();
  const clients = useClients();

  const spec = useAsync(
    (signal) => clients.spec.getSpec({ project }, { signal }),
    [clients, project],
  );

  const applications = useMemo(() => {
    const doc = spec.data?.spec?.documents?.project;
    return doc ? applicationNames(new TextDecoder().decode(doc)) : [];
  }, [spec.data]);

  const [namespace, setNamespace] = useState(`${project}-${env}`);
  const [application, setApplication] = useState("");
  const [mode, setMode] = useState<"query" | "follow">("query");

  // The picker fills itself in once, from the parsed spec. It stays a free-text
  // input either way: a parse that found nothing must not lock the screen.
  useEffect(() => {
    const first = applications[0];
    if (application === "" && first !== undefined) setApplication(first);
  }, [applications, application]);

  return (
    <>
      <div className="k-page-head">
        <h1>Logs</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/apps/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
      </div>

      <div className="k-panel k-logs__selector">
        <label className="k-field">
          <span className="k-eyebrow">Namespace</span>
          <input
            className="k-input k-mono"
            value={namespace}
            onChange={(e) => setNamespace(e.target.value)}
            placeholder={`${project}-${env}`}
          />
          <span className="k-field__note k-mono">
            the model's default for this pair; override it if the Environment
            sets spec.namespace
          </span>
        </label>

        <label className="k-field">
          <span className="k-eyebrow">Application</span>
          <input
            className="k-input k-mono"
            value={application}
            onChange={(e) => setApplication(e.target.value)}
            list="k-applications"
            placeholder="web"
          />
          <datalist id="k-applications">
            {applications.map((name) => (
              <option key={name} value={name} />
            ))}
          </datalist>
          <span className="k-field__note k-mono">
            {applications.length > 0
              ? `from the stored Project document: ${applications.join(", ")}`
              : "no applications parsed from the stored spec — type the name"}
          </span>
        </label>
      </div>

      {spec.error !== undefined ? (
        <ErrorPanel title="Cannot read the spec" error={spec.error} />
      ) : null}

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
        <QueryLogs namespace={namespace} application={application} />
      ) : (
        <FollowLogs namespace={namespace} application={application} />
      )}
    </>
  );
}

/**
 * Application names out of the stored Project document.
 *
 * A YAML-lite scan, deliberately: `applications:` is a list of mappings whose
 * first key is `name` (docs/model.md), so the names are found by indentation
 * without a parser. It is a convenience for the picker and nothing more — the
 * field stays free text, so a spec this misses costs a reader one word of
 * typing rather than a broken screen.
 */
export function applicationNames(yaml: string): string[] {
  const names: string[] = [];
  const lines = yaml.split("\n");
  let indent = -1;
  for (const line of lines) {
    if (/^\s*#/.test(line)) continue;
    const opens = /^(\s*)applications:\s*$/.exec(line);
    if (opens?.[1] !== undefined) {
      indent = opens[1].length;
      continue;
    }
    if (indent < 0) continue;
    const item = /^(\s*)-\s+name:\s*("?)([A-Za-z0-9][A-Za-z0-9-]*)\2\s*$/.exec(line);
    if (item?.[1] !== undefined && item[1].length > indent && item[3]) {
      names.push(item[3]);
      continue;
    }
    // Dedenting to or past `applications:` ends the block; blank lines and
    // deeper keys inside an item do not.
    const leading = /^(\s*)\S/.exec(line);
    if (leading?.[1] !== undefined && leading[1].length <= indent) indent = -1;
  }
  return [...new Set(names)];
}

function QueryLogs({
  namespace,
  application,
}: {
  namespace: string;
  application: string;
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
          selector: { namespace, application },
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
  }, [run, clients, namespace, application, tail, sinceMinutes, match, isRegex]);

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
          <LogLines lines={lines} />
        )
      ) : null}
    </>
  );
}

function FollowLogs({
  namespace,
  application,
}: {
  namespace: string;
  application: string;
}) {
  const clients = useClients();
  const run = useRun();
  const [lines, setLines] = useState<LogLine[]>([]);
  const [dropped, setDropped] = useState(0);
  const [match, setMatch] = useState("");
  const [isRegex, setIsRegex] = useState(false);
  const [pinned, setPinned] = useState(true);
  const box = useRef<HTMLDivElement | null>(null);

  // Auto-scroll, but only while the reader is at the bottom. Scrolling up is
  // how a person reads a line that just went past, and yanking them back down
  // on the next line makes a live tail unreadable.
  useEffect(() => {
    const el = box.current;
    if (el && pinned) el.scrollTop = el.scrollHeight;
  }, [lines, pinned]);

  const onScroll = useCallback(() => {
    const el = box.current;
    if (!el) return;
    setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 24);
  }, []);

  const follow = useCallback(() => {
    setLines([]);
    setDropped(0);
    setPinned(true);
    run.start(async (signal) => {
      for await (const res of clients.log.followLogs(
        {
          selector: { namespace, application },
          match: isRegex ? { regex: match } : { substring: match },
        },
        { signal },
      )) {
        const event = res.event;
        if (event.case === "line") {
          const line = event.value;
          setLines((prev) => [...prev, line]);
        } else if (event.case === "dropped") {
          setDropped(Number(event.value));
        }
      }
    });
  }, [run, clients, namespace, application, match, isRegex]);

  return (
    <>
      <div className="k-panel k-logs__controls">
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
        {run.running ? (
          <button type="button" className="k-button" onClick={run.stop}>
            Stop following
          </button>
        ) : (
          <button
            type="button"
            className="k-button k-button--primary"
            onClick={follow}
          >
            Start following
          </button>
        )}
        <span className="k-mono k-field__note">
          {run.running
            ? pinned
              ? "streaming · following the tail"
              : "streaming · scrolled up, auto-scroll paused"
            : "unbounded stream — it runs until you stop it or leave"}
        </span>
      </div>

      {run.error !== undefined ? (
        <ErrorPanel title="The log stream failed" error={run.error} />
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

      {lines.length > 0 || run.running ? (
        <LogLines lines={lines} boxRef={box} onScroll={onScroll} />
      ) : null}
    </>
  );
}

function LogLines({
  lines,
  boxRef,
  onScroll,
}: {
  lines: readonly LogLine[];
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
      {lines.map((line, i) => (
        <div className="k-logline" key={`${line.pod}:${i}`}>
          <span className="k-logline__time">{stamp(line.timestampUnixMs)}</span>
          <span className="k-logline__pod">{line.pod}</span>
          <span className="k-logline__container">{line.container}</span>
          <span className="k-logline__message">{line.message}</span>
        </div>
      ))}
    </div>
  );
}

/** 0 means the line carried no timestamp the engine could parse, not 1970. */
function stamp(unixMs: bigint): string {
  const ms = Number(unixMs);
  if (!ms) return "--:--:--";
  return new Date(ms).toISOString().slice(11, 19);
}
