import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { toFailure, type Failure } from "../api/errors";
import type { StatusResponse } from "../gen/kelson/v1alpha1/deploy_pb";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill, type StatusKind } from "../components/StatusPill";
import { phaseToStatus } from "../components/phase";
import { EmptyState, LoadingState } from "../components/States";

/**
 * The app list: one card per (project, environment).
 *
 * A project is not a deployable thing — an environment is (docs/model.md: the
 * Environment carries the namespace, the delivery mode and the cluster). So the
 * grid is keyed by the pair, which is also what the mockup's cards are shaped
 * for: a name, a status, and mono metadata underneath.
 *
 * ListSpecs gives the pairs; each card then asks DeployService.Status for its
 * own. The calls are per-card and independent on purpose. Status renders the
 * spec and talks to a cluster, so it is the slow, failure-prone call of the
 * two, and one environment whose adapter is unreachable must not hold up — or
 * blank out — the other five. A card whose Status failed says exactly that and
 * carries the server's reason; it never shows green it did not earn.
 */
export function AppsPage() {
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

  const projects = specs.data?.specs.length ?? 0;

  return (
    <>
      <div className="k-page-head">
        <h1>Apps</h1>
        {/* The mockup's one accent-outlined mono action, in the place it puts
            it. This is the only way into the create flow, so it stays visible
            whether the grid is full or empty. */}
        <Link className="k-button k-button--primary" to="/apps/new">
          New app
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
      </div>

      {specs.loading && specs.data === undefined ? (
        <LoadingState what="projects" />
      ) : null}

      {specs.error !== undefined ? (
        <ErrorPanel title="Cannot list projects" error={specs.error} />
      ) : null}

      {specs.data !== undefined && projects === 0 ? (
        <EmptyState title="No projects stored yet — kelson-server's spec store is empty">
          Create one with “New app” above, or put one with `kelson` or
          SpecService.PutSpec, and it appears here.
        </EmptyState>
      ) : null}

      {projects > 0 && pairs.length === 0 ? (
        <EmptyState title="No environments declared">
          Every stored project has a Project document but no Environment
          document. An Environment is what names a namespace and a delivery
          mode, so there is nothing to deploy yet.
        </EmptyState>
      ) : null}

      {pairs.length > 0 ? (
        <div className="k-grid">
          {pairs.map((pair) => (
            <AppCard
              key={`${pair.project}/${pair.environment}`}
              project={pair.project}
              environment={pair.environment}
              onStatus={report}
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

function AppCard({
  project,
  environment,
  onStatus,
}: {
  project: string;
  environment: string;
  onStatus: (key: string, kind: StatusKind) => void;
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
    [clients, project, environment],
  );

  const failure: Failure | undefined =
    status.error === undefined ? undefined : toFailure(status.error);
  const kind: StatusKind =
    failure !== undefined
      ? "unknown"
      : status.data
        ? phaseToStatus(status.data.phase)
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
            to={`/apps/${encodeURIComponent(project)}`}
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
          <StatusPill
            status={kind}
            label={status.data?.phase.toLowerCase() || "unknown"}
          />
        )}
      </div>

      <div className="k-mono k-card__meta">
        <CardMeta status={status.data} failure={failure} />
      </div>
    </div>
  );
}

function CardMeta({
  status,
  failure,
}: {
  status: StatusResponse | undefined;
  failure: Failure | undefined;
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
  // a mode that reports none simply has no counts line.
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
        {status.revision ? (
          <Copyable value={status.revision} className="k-card__rev" />
        ) : (
          <span className="k-card__rev">no revision recorded</span>
        )}
      </span>
      {counts.length > 0 ? <span>{counts.join(" · ")}</span> : null}
      {status.cause ? (
        <span className="k-card__reason">{status.cause}</span>
      ) : null}
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
