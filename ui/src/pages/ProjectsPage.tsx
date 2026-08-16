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
import { StatusPill, type StatusKind } from "../components/StatusPill";
import { phaseToStatus } from "../components/phase";
import { EmptyState, LoadingState } from "../components/States";

/**
 * The project list: one card per (project, environment).
 *
 * A project is not a deployable thing — an environment is (docs/model.md: the
 * Environment carries the namespace and the cluster). So the
 * grid is keyed by the pair, which is also what the mockup's cards are shaped
 * for: a name, a status, and mono metadata underneath.
 *
 * ListSpecs gives the pairs; each card then asks DeployService.Status for its
 * own. The calls are per-card and independent on purpose. Status renders the
 * spec and talks to a cluster, so it is the slow, failure-prone call of the
 * two, and one environment whose adapter is unreachable must not hold up — or
 * blank out — the other five. A card whose Status failed says exactly that and
 * carries the server's reason; it never shows green it did not earn.
 *
 * # One stream for the whole grid
 *
 * Once every card has an answer, the page opens a single EventService.Watch
 * covering all of them (#76) and applies what changes in place. One stream, not
 * one per card: the scopes are a request field precisely so a grid costs one
 * subscription. A Resync means the server could not honour the resume point, so
 * the grid relists — every card refetches and the live overlay is dropped,
 * because a stale overlay would outlive the answer it was a delta of.
 */

/** What the stream has said about one card since its Status was read. */
interface CardLive {
  transition?: { phase: string; revision: string; cause: string };
  /** An unhealthy workload, as one line. Cleared when it recovers. */
  health?: string;
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

  // Each card reports its own resolved pill up, so the mono line above the grid
  // counts what is actually on screen rather than a second, guessed tally.
  const [kinds, setKinds] = useState<Record<string, StatusKind>>({});
  const report = useCallback((key: string, kind: StatusKind) => {
    setKinds((prev) => (prev[key] === kind ? prev : { ...prev, [key]: kind }));
  }, []);

  const [live, setLive] = useState<Record<string, CardLive>>({});
  const [generation, setGeneration] = useState(0);

  const onEvent = useCallback((event: WatchResponse_Event) => {
    const key = `${event.project}/${event.environment}`;
    const payload = event.payload;
    setLive((prev) => {
      if (payload.case === "statusTransition") {
        const { phase, revision, cause } = payload.value;
        return { ...prev, [key]: { ...prev[key], transition: { phase, revision, cause } } };
      }
      if (payload.case === "healthChange") {
        const v = payload.value;
        const next: CardLive = { ...prev[key] };
        // A workload that recovered leaves no note behind: the line said what
        // was wrong, and nothing is now.
        if (v.healthy) delete next.health;
        else next.health = `${v.resource}: ${v.message || v.code}`;
        return { ...prev, [key]: next };
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

  // The watch opens only once every card has settled: until then the deltas
  // have nothing to be deltas of, and a transition applied before its Status
  // landed would be overwritten by the older answer.
  const settled = pairs.length > 0 && pairs.every((p) => kinds[`${p.project}/${p.environment}`] !== undefined);
  const watch = useWatch({
    scopes: settled ? pairs : [],
    onEvent,
    onResync,
  });

  const projects = specs.data?.specs.length ?? 0;

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
          {projects} {projects === 1 ? "project" : "projects"}
        </span>
        <span>·</span>
        <span>
          {pairs.length} {pairs.length === 1 ? "environment" : "environments"}
        </span>
        <Counts kinds={kinds} />
        <LiveIndicator state={watch} />
      </div>

      {specs.loading && specs.data === undefined ? (
        <LoadingState what="projects" />
      ) : null}

      {specs.error !== undefined ? (
        <ErrorPanel title="Cannot list projects" error={specs.error} />
      ) : null}

      {specs.data !== undefined && projects === 0 ? (
        <EmptyState title="No projects yet">
          Create one with “New project” above, or with `kelson spec put`.
        </EmptyState>
      ) : null}

      {projects > 0 && pairs.length === 0 ? (
        <EmptyState title="No environments yet">
          No stored project declares one, so there is nothing to deploy. Add one
          on a project's edit screen.
        </EmptyState>
      ) : null}

      {pairs.length > 0 ? (
        <div className="k-grid">
          {pairs.map((pair) => (
            <ProjectCard
              key={`${pair.project}/${pair.environment}`}
              project={pair.project}
              environment={pair.environment}
              onStatus={report}
              live={live[`${pair.project}/${pair.environment}`]}
              generation={generation}
            />
          ))}
        </div>
      ) : null}
    </>
  );
}

const COUNTED: { kind: StatusKind; label: string }[] = [
  { kind: "synced", label: "synced" },
  { kind: "reconciling", label: "reconciling" },
  { kind: "degraded", label: "degraded" },
  { kind: "failed", label: "failed" },
  { kind: "unknown", label: "unknown" },
];

function Counts({ kinds }: { kinds: Record<string, StatusKind> }) {
  const tally = Object.values(kinds);
  return (
    <>
      {COUNTED.map(({ kind, label }) => {
        const n = tally.filter((k) => k === kind).length;
        if (n === 0) return null;
        return (
          <span key={kind} className="k-count-group">
            <span className={`k-count k-count--${kind}`}>{n}</span> {label}
          </span>
        );
      })}
    </>
  );
}

function ProjectCard({
  project,
  environment,
  onStatus,
  live,
  generation,
}: {
  project: string;
  environment: string;
  onStatus: (key: string, kind: StatusKind) => void;
  live: CardLive | undefined;
  /** Bumped on a resync: the card refetches rather than trusting a delta. */
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
  // fetched revision and cause described the phase the card has just left.
  const phase = live?.transition?.phase ?? status.data?.phase;
  const revision = live?.transition
    ? live.transition.revision
    : status.data?.revision;
  const cause = live?.transition ? live.transition.cause : status.data?.cause;

  const kind: StatusKind =
    failure !== undefined
      ? "unknown"
      : phase !== undefined
        ? phaseToStatus(phase)
        : "unknown";

  const settled = !status.loading;
  useEffect(() => {
    if (settled) onStatus(`${project}/${environment}`, kind);
  }, [settled, kind, project, environment, onStatus]);

  return (
    <div className="k-panel k-panel--interactive k-card">
      <div className="k-card__head">
        <div className="k-card__ident">
          <Link
            to={`/projects/${encodeURIComponent(project)}`}
            className="k-card__name"
          >
            {project}
          </Link>
          <span className="k-chip k-mono">{environment}</span>
        </div>
        {status.loading && status.data === undefined && failure === undefined ? (
          <StatusPill status="unknown" label="reading…" />
        ) : failure !== undefined ? (
          // The reason is the server's own, kept verbatim on the tooltip: an
          // unreachable cluster and a rejected spec are different problems and
          // the card must not blur them into one grey pill with no story.
          <span title={statusReason(failure)}>
            <StatusPill status="unknown" label="status unavailable" />
          </span>
        ) : (
          <StatusPill status={kind} label={phase?.toLowerCase() || "unknown"} />
        )}
      </div>

      <div className="k-mono k-card__meta">
        <CardMeta
          status={status.data}
          failure={failure}
          revision={revision}
          cause={cause}
          health={live?.health}
        />
      </div>
    </div>
  );
}

function CardMeta({
  status,
  failure,
  revision,
  cause,
  health,
}: {
  status: StatusResponse | undefined;
  failure: Failure | undefined;
  revision: string | undefined;
  cause: string | undefined;
  health: string | undefined;
}) {
  if (failure !== undefined) {
    return (
      <>
        <span className="k-card__reason">{statusReason(failure)}</span>
        <span>no revision, no counts — nothing was read</span>
      </>
    );
  }
  if (status === undefined) return <span>—</span>;

  // The adapters put resources/live/degraded here (internal/delivery/direct);
  // a mode that reports none simply has no counts line. The counts are the
  // fetched ones: no event type carries them, and inventing them from a
  // transition would be a number nobody measured.
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
      <span>
        {revision ? (
          <Copyable value={revision} className="k-card__rev" />
        ) : (
          <span className="k-card__rev">no revision recorded</span>
        )}
      </span>
      {counts.length > 0 ? <span>{counts.join(" · ")}</span> : null}
      {cause ? <span className="k-card__reason">{cause}</span> : null}
      {health ? <span className="k-card__reason">{health}</span> : null}
    </>
  );
}

function statusReason(failure: Failure): string {
  const first = failure.wire[0];
  if (first) {
    return `${first.code}: ${first.message}`;
  }
  return failure.code ? `${failure.code}: ${failure.message}` : failure.message;
}
