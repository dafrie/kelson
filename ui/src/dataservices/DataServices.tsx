import { useMemo } from "react";

import { useAsync, useClients } from "../api/data";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { verdictTone } from "../components/status";
import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import { CapabilityPanel } from "./CapabilityPanel";
import {
  clusterResourceName,
  clusterVerdictFor,
  isKnownPreset,
  parseDataServices,
  type DataService,
} from "./parse";
import {
  COMING_SOON,
  deferralFor,
  sizingLinesFor,
  type Deferral,
} from "./presets";
import "./dataservices.css";

/**
 * Data services, as their own section of the project detail page.
 *
 * A database is not a workload and reading it as one is the mistake this
 * section exists to prevent: it has no replicas to scale, no image to roll, and
 * its topology is a preset the operator implements (ADR-0005, ADR-0007). So it
 * gets its own list under the workloads, with the numbers the preset actually
 * means — because "ha-small" tells a reader nothing until it says three
 * instances, 500m, 1Gi, 5Gi and synchronous replication.
 *
 * Three things this section refuses to do:
 *
 *   - **Invent health.** The only health kelson has is observation's verdicts,
 *     which today are computed for Deployments (internal/observation/probe.go).
 *     If the status data carries a verdict for the component's CloudNativePG
 *     Cluster, it is shown; if it does not, the section says there is none. A
 *     database with no probe is not a healthy database.
 *   - **Invent a refusal.** A preset kelson does not render is shown as the
 *     server's own structured error, fetched from RenderService, code and
 *     remediation intact — `render/service-not-implemented` naming #93 reads as
 *     a deferred feature, and a generic failure would not.
 *   - **Pretend the coming-soon features are here.** Backups and branching are
 *     labelled items with muted badges and links to the issues, never controls.
 */

/** The shape of a status verdict this section reads. Structurally the page's. */
export interface ResourceVerdict {
  resource: string;
  code: string;
  healthy: boolean;
  degraded: boolean;
  /** The probe gave up waiting. Not a failure, and not the same as degraded. */
  stuck: boolean;
  message: string;
  remediation: string;
}

/**
 * Where this section's live health comes from — and the two ways it can be
 * absent, which are different facts and get different words. A status still in
 * flight is not a status that failed, and neither of those is "the probe
 * reported no verdict for this resource".
 */
export type HealthSource =
  | { state: "loading" }
  | { state: "unavailable" }
  | { state: "read"; verdicts: readonly ResourceVerdict[] };

/** Whether the server has said what it renders for this spec (see below). */
type ServerAnswer = "pending" | "answered" | "unavailable";

/** The render error codes that are a statement about a data component. */
const DATA_ERROR_CODES = new Set([
  "render/service-not-implemented",
  "render/postgres-unsupported",
  "render/service-name-too-long",
]);

export function DataServices({
  project,
  environment,
  projectDoc,
  environmentDoc,
  health,
}: {
  project: string;
  environment: string;
  /** The stored Project document, as authored. */
  projectDoc: string;
  /** The stored Environment document, for rule P5's preset override. */
  environmentDoc: string;
  /** The environment's status readback, in whatever state it is in. */
  health: HealthSource;
}) {
  const services = useMemo(
    () => parseDataServices(projectDoc, environmentDoc),
    [projectDoc, environmentDoc],
  );
  const deferred = services.some(
    (svc) => deferralFor(svc.kind, svc.preset) !== undefined,
  );

  // The render is asked for only when the spec names something kelson may
  // refuse. It is the offline rung — no profile, the same pure function
  // `kelson render` runs — so it needs no cluster, and its answer is the
  // server's own structured error rather than a sentence written here. For a
  // spec of ordinary presets there is nothing to ask: the capability verdict
  // against a zero profile is Unknown, and Unknown renders (issue #144).
  const clients = useClients();
  const check = useAsync(
    (signal) =>
      deferred
        ? clients.render.render(
            {
              spec: { spec: { case: "project", value: project } },
              environment,
            },
            { signal },
          )
        : Promise.resolve(undefined),
    [clients, project, environment, deferred],
  );
  const refusals: WireError[] = (check.data?.errors ?? []).filter((e) =>
    DATA_ERROR_CODES.has(e.code),
  );
  const answer: ServerAnswer =
    check.data !== undefined
      ? "answered"
      : check.loading
        ? "pending"
        : "unavailable";

  if (services.length === 0) return null;

  return (
    <div className="k-data">
      <div className="k-eyebrow">Data services ({services.length})</div>

      {refusals.length > 0 ? (
        <ErrorPanel
          title="kelson will not render part of this section yet"
          errors={refusals}
        />
      ) : null}

      <ul className="k-data__list">
        {services.map((svc) => (
          <DataServiceRow
            key={svc.name}
            service={svc}
            project={project}
            environment={environment}
            health={health}
            answer={answer}
          />
        ))}
      </ul>

      <ul className="k-data__list k-data__soon">
        {COMING_SOON.map((item) => (
          <li className="k-data__item k-data__item--soon" key={item.title}>
            <div className="k-data__head">
              <span className="k-data__name">{item.title}</span>
              <span className="k-soon">Coming soon</span>
            </div>
            <p className="k-data__summary">{item.summary}</p>
          </li>
        ))}
      </ul>

      <CapabilityPanel />
    </div>
  );
}

function DataServiceRow({
  service,
  project,
  environment,
  health,
  answer,
}: {
  service: DataService;
  project: string;
  environment: string;
  health: HealthSource;
  /** What came back from the render check, for the rows that need one. */
  answer: ServerAnswer;
}) {
  const deferral = deferralFor(service.kind, service.preset);
  const lines = sizingLinesFor(service.kind, service.preset);
  const clusterKind =
    service.kind === "postgres" ? "CloudNativePG Cluster" : "ValkeyCluster";
  const cluster = clusterResourceName(project, environment, service.name);
  const verdict =
    health.state === "read" ? clusterVerdictFor(health.verdicts, cluster) : undefined;

  return (
    <li className="k-data__item">
      <div className="k-data__head">
        <span className="k-data__name">{service.name}</span>
        <span className="k-chip k-mono">{service.kind}</span>
        <span className="k-chip">
          preset: <span className="k-mono">{service.preset}</span>
        </span>
        {service.presetSource === "environment" ? (
          <span className="k-data__source">
            set by this environment
          </span>
        ) : service.presetSource === "default" ? (
          <span className="k-data__source">
            no preset in the spec — the model's default
          </span>
        ) : null}
        {deferral !== undefined ? (
          <span className="k-soon">Deferred</span>
        ) : verdict !== undefined ? (
          <StatusPill
            status={verdictTone(verdict)}
            label={verdict.code || "unknown"}
          />
        ) : null}
      </div>

      {deferral !== undefined ? (
        <Deferred deferral={deferral} answer={answer} />
      ) : lines !== undefined ? (
        <>
          <ul className="k-data__facts">
            {lines.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
          <div className="k-mono k-data__cluster">
            {clusterKind} · {cluster}
          </div>
          <Health verdict={verdict} health={health} />
        </>
      ) : (
        <p className="k-data__summary">
          {isKnownPreset(service.preset)
            ? `This build has no sizing recorded for preset ${service.preset}.`
            : `This build does not know the preset ${service.preset}; the server is the authority on whether it renders.`}
        </p>
      )}
    </li>
  );
}

function Deferred({
  deferral,
  answer,
}: {
  deferral: Deferral;
  answer: ServerAnswer;
}) {
  return (
    <>
      <p className="k-data__summary">
        {deferral.label}. {deferral.summary}
      </p>
      {/* The server's own error is what the section prefers to show; it is
          above, once it arrives. Until then, and if it never does, this stays a
          statement about the spec rather than a claim about what the server
          did. */}
      {answer === "pending" ? (
        <p className="k-data__note">Asking the server what it renders for this…</p>
      ) : answer === "unavailable" ? (
        <p className="k-data__note">
          Nothing is rendered for this component. The server did not answer with
          its own reason.
        </p>
      ) : null}
    </>
  );
}

/**
 * Live health, or the honest absence of it.
 *
 * The status surfaces report per-resource verdicts and this reuses them
 * verbatim — there is no second health source, and a database the probe does
 * not watch must read as unwatched, not as fine.
 */
function Health({
  verdict,
  health,
}: {
  verdict: ResourceVerdict | undefined;
  health: HealthSource;
}) {
  if (health.state === "loading") {
    return <p className="k-data__note">Live health: reading the status…</p>;
  }
  if (health.state === "unavailable") {
    return (
      <p className="k-data__note">
        Live health: the environment’s status could not be read.
      </p>
    );
  }
  if (verdict === undefined) {
    return (
      <p className="k-data__note">
        Live health: no verdict — kelson does not watch this service, so its
        health comes from its operator rather than from here.
      </p>
    );
  }
  return (
    <>
      <p className="k-data__health">{verdict.message}</p>
      {verdict.remediation ? (
        <p className="k-verdict__fix">
          <span className="k-verdict__fix-label">fix:</span> {verdict.remediation}
        </p>
      ) : null}
    </>
  );
}
