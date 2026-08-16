import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { useWatch } from "../api/watch";
import { toFailure, type Failure } from "../api/errors";
import type { StatusResponse } from "../gen/kelson/v1alpha1/deploy_pb";
import type { WatchResponse_Event } from "../gen/kelson/v1alpha1/events_pb";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { LiveIndicator } from "../components/LiveIndicator";
import { StatusPill } from "../components/StatusPill";
import {
  statusFor,
  statusForPhase,
  UNKNOWN_STATUS,
  type Status,
  type StatusWord,
} from "../components/status";
import { EmptyState, LoadingState } from "../components/States";
import {
  componentsFromVerdicts,
  mergeVerdicts,
  needsAttention,
  readCell,
  type EnvironmentRead,
  type LiveVerdict,
} from "./matrix";

/**
 * Home: every component in every environment, grouped by project (#260).
 *
 * The unit here used to be the (project, environment) pair — one card per pair,
 * summarising four components into two numbers. But the thing that deploys, and
 * therefore the thing that breaks, is a *component in an environment*
 * (docs/model.md §6), and a home screen whose rows are not that unit makes a
 * reader open a project to find out which of its components is the unhappy one.
 *
 * Two rules shape what is on screen:
 *
 * - **Quiet when healthy.** The attention band at the top is *absent* when
 *   nothing needs attention — not empty with a reassuring sentence. What is in
 *   it is `matrix.ts`'s list, which deliberately excludes work in flight: a
 *   band that fills up during every deploy is a band people stop reading.
 * - **No more calls than before.** It is still one `ListSpecs` and one
 *   `DeployService.Status` per (project, environment) — the rows come out of
 *   the verdicts that response already carries, one per rendered Deployment.
 *   `ListSpecs` omits the stored documents, so the alternative was a `GetSpec`
 *   per project for a list this page can read for free.
 *
 * The floor that buys is honest but real: observation probes Deployments, so a
 * `cron`, a chart and a database contribute no row, and a server started
 * without an observation client contributes none at all. An environment in that
 * position says it has no per-component readings and keeps its own status —
 * which is the answer the previous version of this page gave for everything.
 *
 * # One stream for the whole page
 *
 * Once every environment has an answer the page opens a single
 * `EventService.Watch` covering all of them (#76) and applies what changes in
 * place: a transition moves the environment's word, a health change moves one
 * row. A Resync means the server could not honour the resume point, so the page
 * relists — every environment refetches and the live overlay is dropped,
 * because a stale overlay would outlive the answer it was a delta of.
 */

/** What the stream has said about one environment since its Status was read. */
interface EnvironmentLive {
  transition?: { phase: string; revision: string; cause: string };
  verdicts: Record<string, LiveVerdict>;
}

const NO_EVENTS: EnvironmentLive = { verdicts: {} };

/** One line the attention band can raise: a component, or an environment. */
interface AttentionRow {
  project: string;
  environment: string;
  /** Empty for a row that is about the environment itself. */
  component: string;
  status: Status;
  detail: string;
  href: string;
}

/** What one environment reported up, for the band and the tally. */
interface Report {
  word: StatusWord;
  attention: AttentionRow[];
}

export function ProjectsPage() {
  const clients = useClients();
  const specs = useAsync(
    (signal) => clients.spec.listSpecs({}, { signal }),
    [clients],
  );

  const pairs = useMemo(() => {
    const out: { project: string; environment: string }[] = [];
    for (const spec of specs.data?.specs ?? []) {
      for (const environment of spec.environments) {
        out.push({ project: spec.project, environment });
      }
    }
    return out;
  }, [specs.data]);

  // Each environment reports its own resolved state up, so the band and the
  // mono line above the grid describe what is actually on screen rather than a
  // second, guessed tally.
  const [reports, setReports] = useState<
    Record<string, { signature: string; report: Report }>
  >({});
  // The signature is what makes the report idempotent: the child recomputes its
  // rows on every render and an unconditional set would loop.
  const report = useCallback((key: string, signature: string, value: Report) => {
    setReports((prev) =>
      prev[key]?.signature === signature
        ? prev
        : { ...prev, [key]: { signature, report: value } },
    );
  }, []);

  const [live, setLive] = useState<Record<string, EnvironmentLive>>({});
  const [generation, setGeneration] = useState(0);

  const onEvent = useCallback((event: WatchResponse_Event) => {
    const key = `${event.project}/${event.environment}`;
    const payload = event.payload;
    setLive((prev) => {
      const current = prev[key] ?? NO_EVENTS;
      if (payload.case === "statusTransition") {
        const { phase, revision, cause } = payload.value;
        return { ...prev, [key]: { ...current, transition: { phase, revision, cause } } };
      }
      if (payload.case === "healthChange") {
        const v = payload.value;
        return {
          ...prev,
          [key]: {
            ...current,
            verdicts: {
              ...current.verdicts,
              [v.resource]: {
                code: v.code,
                healthy: v.healthy,
                message: v.message,
              },
            },
          },
        };
      }
      return prev;
    });
  }, []);

  const reloadSpecs = specs.reload;
  const onResync = useCallback(() => {
    setLive({});
    setGeneration((n) => n + 1);
    reloadSpecs();
  }, [reloadSpecs]);

  // The watch opens only once every environment has settled: until then the
  // deltas have nothing to be deltas of, and a transition applied before its
  // Status landed would be overwritten by the older answer.
  const settled =
    pairs.length > 0 &&
    pairs.every((p) => reports[`${p.project}/${p.environment}`] !== undefined);
  const watch = useWatch({ scopes: settled ? pairs : [], onEvent, onResync });

  const projects = specs.data?.specs ?? [];
  const attention = useMemo(
    () =>
      pairs.flatMap(
        (p) => reports[`${p.project}/${p.environment}`]?.report.attention ?? [],
      ),
    [pairs, reports],
  );
  const words = useMemo(() => {
    const out: Record<string, StatusWord> = {};
    for (const [key, value] of Object.entries(reports)) {
      out[key] = value.report.word;
    }
    return out;
  }, [reports]);

  return (
    <>
      <div className="k-page-head">
        <h1>Projects</h1>
        {/* The mockup's one accent-outlined mono action, in the place it puts
            it. This is the only way into the create flow, so it stays visible
            whether the grid is full or empty. */}
        <Link className="k-button k-button--primary" to="/projects/new">
          New project
        </Link>
      </div>

      <div className="k-page-sub">
        <span>
          <span className="k-mono">{projects.length}</span>{" "}
          {projects.length === 1 ? "project" : "projects"}
        </span>
        <span>·</span>
        <span>
          <span className="k-mono">{pairs.length}</span>{" "}
          {pairs.length === 1 ? "environment" : "environments"}
        </span>
        <Counts words={words} />
        <LiveIndicator state={watch} />
      </div>

      {specs.loading && specs.data === undefined ? (
        <LoadingState what="projects" />
      ) : null}

      {specs.error !== undefined ? (
        <ErrorPanel title="Cannot list projects" error={specs.error} />
      ) : null}

      {specs.data !== undefined && projects.length === 0 ? (
        <EmptyState title="No projects yet">
          Create one with “New project” above, or with `kelson spec put`.
        </EmptyState>
      ) : null}

      {projects.length > 0 && pairs.length === 0 ? (
        <EmptyState title="No environments yet">
          No stored project declares one, so there is nothing to deploy. Add one
          on a project's edit screen.
        </EmptyState>
      ) : null}

      <Attention rows={attention} />

      {projects.map((spec) =>
        spec.environments.length === 0 ? null : (
          <section className="k-section" key={spec.project}>
            {/* The project's name is a heading, not an eyebrow: it used to be a
                link inside `.k-eyebrow`, which set it uppercase and letterspaced
                a name somebody chose. Names are words (#260). */}
            <div className="k-env__head">
              <h2 className="k-card__name">
                <Link to={`/projects/${encodeURIComponent(spec.project)}`}>
                  {spec.project}
                </Link>
              </h2>
            </div>
            <div className="k-section__body k-envs">
              {spec.environments.map((environment) => (
                <EnvironmentBlock
                  key={`${spec.project}/${environment}`}
                  project={spec.project}
                  environment={environment}
                  onRead={report}
                  live={live[`${spec.project}/${environment}`]}
                  generation={generation}
                />
              ))}
            </div>
          </section>
        ),
      )}
    </>
  );
}

/**
 * The band, which is absent when there is nothing in it.
 *
 * An empty band with "everything is fine" written in it is a thing a reader
 * has to check every time; nothing at all is a thing they can see from across
 * the room.
 */
function Attention({ rows }: { rows: AttentionRow[] }) {
  if (rows.length === 0) return null;
  return (
    <section className="k-attention" aria-label="Needs attention">
      <div className="k-eyebrow">Needs attention ({rows.length})</div>
      <ul className="k-attention__list">
        {rows.map((row) => (
          <li
            key={`${row.project}/${row.environment}/${row.component}`}
            className="k-attention__row"
          >
            <Link className="k-attention__what" to={row.href}>
              {row.component === ""
                ? `${row.project} · ${row.environment}`
                : `${row.project} · ${row.environment} · ${row.component}`}
            </Link>
            <StatusPill status={row.status.tone} label={row.status.word} />
            {row.detail ? (
              <span className="k-attention__why">{row.detail}</span>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * Worst first, so the tally reads as a to-do list: a page with one broken
 * environment says "1 failed" before it says "12 live". The words and their
 * colours are components/status.ts's; this only decides the order.
 *
 * It counts environments and not rows, because an environment is the one unit
 * that always exists: a build with no observation client has no rows to count
 * and the tally must not silently become a different measurement.
 */
const COUNTED: StatusWord[] = [
  "failed",
  "stuck",
  "unhealthy",
  "deploying",
  "waiting",
  "suspended",
  "live",
  "unknown",
];

function Counts({ words }: { words: Record<string, StatusWord> }) {
  const tally = Object.values(words);
  return (
    <>
      {COUNTED.map((word) => {
        const n = tally.filter((w) => w === word).length;
        if (n === 0) return null;
        const { tone } = statusFor(word);
        return (
          <span key={word} className="k-count-group">
            <span className={`k-count k-count--${tone}`}>{n}</span> {word}
          </span>
        );
      })}
    </>
  );
}

/**
 * One environment of one project: its own Status call, its own row list.
 *
 * The call is per environment and independent on purpose. Status renders a spec
 * and talks to a cluster, so it is the slow, failure-prone call on this page,
 * and one unreachable cluster must not hold up — or blank out — the others. An
 * environment whose Status failed says exactly that and carries the server's
 * reason; it never shows green it did not earn.
 */
function EnvironmentBlock({
  project,
  environment,
  onRead,
  live,
  generation,
}: {
  project: string;
  environment: string;
  onRead: (key: string, signature: string, report: Report) => void;
  live: EnvironmentLive | undefined;
  /** Bumped on a resync: the environment refetches rather than trusting a delta. */
  generation: number;
}) {
  const clients = useClients();
  const status = useAsync(
    (signal) =>
      clients.deploy.status(
        {
          spec: { spec: { case: "project", value: project } },
          environment,
        },
        { signal },
      ),
    [clients, project, environment, generation],
  );

  const failure: Failure | undefined =
    status.error === undefined ? undefined : toFailure(status.error);
  // A transition is a whole replacement for the three fields it carries: the
  // fetched revision and cause described the phase this environment has just
  // left.
  const phase = live?.transition?.phase ?? status.data?.phase ?? "";
  const revision = live?.transition
    ? live.transition.revision
    : (status.data?.revision ?? "");
  const cause = live?.transition
    ? live.transition.cause
    : (status.data?.cause ?? "");

  const verdicts = useMemo(
    () => mergeVerdicts(status.data?.verdicts, live?.verdicts ?? {}),
    [status.data, live?.verdicts],
  );
  const read = useMemo<EnvironmentRead>(
    () => ({
      environment,
      phase,
      revision,
      cause,
      namespace: status.data?.namespace ?? "",
      verdicts,
      read: failure === undefined && status.data !== undefined,
    }),
    [environment, phase, revision, cause, status.data, verdicts, failure],
  );

  const state: Status =
    failure !== undefined || !read.read ? UNKNOWN_STATUS : statusForPhase(phase);
  const rows = useMemo(
    () =>
      componentsFromVerdicts(read, project).map((found) => ({
        component: found.component,
        cell: readCell(read, found.verdict),
      })),
    [read, project],
  );

  const base = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(environment)}`;
  const settled = !status.loading;
  useEffect(() => {
    if (!settled) return;
    const attention: AttentionRow[] = [];
    for (const row of rows) {
      if (!needsAttention(row.cell.status.word)) continue;
      attention.push({
        project,
        environment,
        component: row.component,
        status: row.cell.status,
        detail: row.cell.detail,
        href: `${base}/components/${encodeURIComponent(row.component)}`,
      });
    }
    // An environment nobody could read, or one whose own word is unhappy while
    // no component owned up to it, is raised as itself: the band must never be
    // quiet because the bad news had nowhere to sit.
    if (
      attention.length === 0 &&
      needsAttention(failure !== undefined ? "unknown" : state.word)
    ) {
      attention.push({
        project,
        environment,
        component: "",
        status: failure !== undefined ? UNKNOWN_STATUS : state,
        detail: failure !== undefined ? reasonOf(failure) : cause,
        href: `/projects/${encodeURIComponent(project)}`,
      });
    }
    const value: Report = { word: state.word, attention };
    const signature = JSON.stringify([
      state.word,
      attention.map((a) => [a.component, a.status.word, a.detail]),
    ]);
    onRead(`${project}/${environment}`, signature, value);
  }, [
    settled,
    rows,
    state,
    failure,
    cause,
    project,
    environment,
    base,
    onRead,
  ]);

  return (
    <div className="k-envblock">
      <div className="k-envblock__head">
        <span className="k-chip">{environment}</span>
        {status.loading && status.data === undefined && failure === undefined ? (
          <StatusPill status="unknown" label="reading…" />
        ) : failure !== undefined ? (
          // The reason is the server's own, kept verbatim on the tooltip: an
          // unreachable cluster and a rejected spec are different problems and
          // the row must not blur them into one grey pill with no story.
          <span title={reasonOf(failure)}>
            <StatusPill status="unknown" label="status unavailable" />
          </span>
        ) : (
          <StatusPill status={state.tone} label={state.word} />
        )}
        <span className="k-envblock__meta">
          <EnvironmentMeta
            status={status.data}
            failure={failure}
            revision={revision}
            cause={cause}
          />
        </span>
      </div>

      {failure !== undefined ? null : rows.length === 0 ? (
        <p className="k-env__note">
          {status.loading && status.data === undefined
            ? "reading…"
            : "no per-component readings here"}
        </p>
      ) : (
        <ul className="k-rows">
          {rows.map((row) => (
            <li className="k-row" key={row.component}>
              <Link
                className="k-row__name"
                to={`${base}/components/${encodeURIComponent(row.component)}`}
              >
                {row.component}
              </Link>
              <StatusPill
                status={row.cell.status.tone}
                label={row.cell.status.word}
              />
              <span className="k-row__fact">
                <span className="k-mono">{row.cell.code}</span>
                {row.cell.detail ? ` · ${row.cell.detail}` : ""}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function EnvironmentMeta({
  status,
  failure,
  revision,
  cause,
}: {
  status: StatusResponse | undefined;
  failure: Failure | undefined;
  revision: string;
  cause: string;
}) {
  if (failure !== undefined) {
    return <span className="k-card__reason">{reasonOf(failure)}</span>;
  }
  if (status === undefined) return null;

  // The adapters put resources/live/degraded here; a mode that reports none
  // simply has no counts line. The counts are the fetched ones: no event type
  // carries them, and inventing them from a transition would be a number nobody
  // measured.
  const live = status.detail["live"];
  const degraded = status.detail["degraded"];
  const resources = status.detail["resources"];
  const counts = [
    live !== undefined && resources !== undefined
      ? `${live}/${resources} live`
      : undefined,
    degraded !== undefined && degraded !== "0"
      ? `${degraded} degraded`
      : undefined,
  ].filter((s): s is string => s !== undefined);

  return (
    <>
      {revision ? (
        <Copyable value={revision} className="k-card__rev" />
      ) : (
        <span className="k-card__rev">no revision recorded</span>
      )}
      {counts.length > 0 ? (
        <span className="k-mono">{counts.join(" · ")}</span>
      ) : null}
      {cause ? <span className="k-card__reason">{cause}</span> : null}
    </>
  );
}

function reasonOf(failure: Failure): string {
  const first = failure.wire[0];
  if (first) {
    return `${first.code}: ${first.message}`;
  }
  return failure.code ? `${failure.code}: ${failure.message}` : failure.message;
}
