import { useCallback, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { useWatch } from "../api/watch";
import { toFailure } from "../api/errors";
import type { SpecDocuments } from "../gen/kelson/v1alpha1/common_pb";
import type { WorkloadVerdict } from "../gen/kelson/v1alpha1/deploy_pb";
import type { WatchResponse_Event } from "../gen/kelson/v1alpha1/events_pb";
import { Copyable } from "../components/Copyable";
import { Disclosure, YamlBlock } from "../components/Disclosure";
import { ErrorPanel } from "../components/ErrorPanel";
import { LiveIndicator } from "../components/LiveIndicator";
import { StatusPill } from "../components/StatusPill";
import { phaseToStatus } from "../components/phase";
import { EmptyState, LoadingState } from "../components/States";
import { DataServices } from "../dataservices/DataServices";
import { isDataServiceVerdict } from "../dataservices/parse";
import { PhaseRail } from "../deploy/PhaseRail";
import { parseCause, type RailInput } from "../deploy/rail";
import { Previews } from "../previews/Previews";
import { SecretsPanel } from "../secrets/SecretsPanel";
import {
  isDataComponentKind,
  isKnownKind,
  parseComponents,
  type ComponentSummary,
} from "../spec/components";

/**
 * One project: its environments' delivery state, its documents, its actions.
 *
 * The two halves answer different questions and neither substitutes for the
 * other (issue #53): the phase says whether the change arrived, the verdicts
 * say whether it works. Both are shown, always, and a phase of Healthy with a
 * crash-looping verdict underneath is a real and important thing to see.
 *
 * Data components are a third thing and get their own section (issue #107): a
 * database has no image to roll and no replicas to scale, its topology is a
 * preset an operator implements, and listing it among the workloads would
 * invite every wrong instinct at once.
 *
 * Above all of it is what the Project *is*: its components (issue #214). A
 * Project is a container of them (ADR-0014) and this page used to show only
 * where they run, which made a project read as a single app with environments.
 * The list is the spec's own — read from the stored document, kind derived the
 * way docs/model.md derives it — and it sits before the environment tabs
 * because it is a fact about the project rather than about one environment. It
 * does not replace the data-services section: this says what the project
 * contains, that one says what a database is doing.
 *
 * The spec documents are printed byte-faithfully. The server stores what was
 * authored (ADR-0013: the spec is the user's document) and re-serialising YAML
 * in the browser would reorder keys the author chose, so the bytes go to the
 * screen untouched.
 */
export function ProjectDetailPage() {
  const { project = "" } = useParams();
  const clients = useClients();
  const spec = useAsync(
    (signal) => clients.spec.getSpec({ project }, { signal }),
    [clients, project],
  );

  const environments = spec.data?.spec?.environments ?? [];
  const [active, setActive] = useState<string | undefined>(undefined);
  const selected = active ?? environments[0];

  return (
    <>
      <div className="k-page-head">
        <h1>{project}</h1>
      </div>
      <div className="k-page-sub">
        <Link to="/projects">← all projects</Link>
        <span>·</span>
        <span>version {spec.data?.spec?.version || "—"}</span>
      </div>

      {spec.loading && spec.data === undefined ? (
        <LoadingState what="the spec" />
      ) : null}

      {spec.error !== undefined ? (
        <ErrorPanel title={`Cannot read the spec for ${project}`} error={spec.error} />
      ) : null}

      {spec.data !== undefined && environments.length === 0 ? (
        <EmptyState title="This project declares no environments">
          An Environment document is what names a namespace and a delivery mode.
          Without one there is nothing to deploy, diff or roll back.
        </EmptyState>
      ) : null}

      {spec.data !== undefined ? (
        <Components
          project={project}
          environment={selected}
          projectDoc={decodeDocument(spec.data.spec?.documents?.project)}
        />
      ) : null}

      {environments.length > 0 ? (
        <>
          <nav className="k-tabs" aria-label="Environments">
            {environments.map((env) => (
              <button
                key={env}
                type="button"
                className={
                  env === selected ? "k-tab k-tab--active" : "k-tab"
                }
                aria-current={env === selected ? "true" : undefined}
                onClick={() => setActive(env)}
              >
                {env}
              </button>
            ))}
          </nav>
          {selected ? (
            // Keyed by the pair: switching tabs must not carry one
            // environment's live deltas onto another's status.
            <EnvironmentPanel
              key={`${project}/${selected}`}
              project={project}
              environment={selected}
              others={environments.filter((name) => name !== selected)}
              documents={spec.data?.spec?.documents}
            />
          ) : null}
        </>
      ) : null}

      {spec.data?.spec?.documents ? (
        <Documents project={project} documents={spec.data.spec.documents} />
      ) : null}
    </>
  );
}

/**
 * What this project is made of (#214).
 *
 * The kinds are the spec's own: derived from the shape for a workload — a
 * `port:` is a service, a `schedule:` is a cron, neither is a worker — and read
 * from `kind:` for the ones that state it (docs/model.md, ADR-0014). Nothing
 * here asks a cluster: a Project document says what its components are whether
 * or not anything is deployed, and that is exactly the claim this section
 * makes. Health belongs to the environment panel below, per environment,
 * because a component is not healthy or unhealthy in the abstract.
 *
 * Each row links to the one screen that is *about* a single component: the log
 * tail, which takes a component name and prefills it from the link. Deploy,
 * diff and rollback are environment-wide acts and stay where they are.
 */
function Components({
  project,
  environment,
  projectDoc,
}: {
  project: string;
  /** The environment tab in view, which is where a per-component link points. */
  environment: string | undefined;
  /** The stored Project document, as authored. */
  projectDoc: string;
}) {
  const components = useMemo(() => parseComponents(projectDoc), [projectDoc]);
  const base = `/projects/${encodeURIComponent(project)}`;

  return (
    <section className="k-section">
      <div className="k-env__head">
        <div className="k-eyebrow">Components ({components.length})</div>
        {/* Adding one is an edit of the stored spec, so it goes to the editor
            rather than growing a second write path — the query parameter opens
            it on the panel that does it. */}
        <Link className="k-button" to={`${base}/edit?add=component`}>
          Add component
        </Link>
      </div>
      <div className="k-section__body">
        {components.length === 0 ? (
          <p className="k-note">
            No components were read from the stored Project document. A Project
            is a container of components — a service, its worker, a nightly job
            and the database they share are one Project (ADR-0014) — and the
            documents below are what this list is read from.
          </p>
        ) : (
          <ul className="k-components">
            {components.map((component) => (
              <ComponentRow
                key={component.name}
                component={component}
                project={project}
                environment={environment}
              />
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

function ComponentRow({
  component,
  project,
  environment,
}: {
  component: ComponentSummary;
  project: string;
  environment: string | undefined;
}) {
  const data = isDataComponentKind(component.kind);
  return (
    <li className="k-component">
      <div className="k-component__ident">
        <span className="k-mono k-component__name">{component.name}</span>
        <span className="k-chip k-mono">{component.kind}</span>
        <span className="k-mono k-component__fact">{componentFact(component)}</span>
        {isKnownKind(component.kind) ? null : (
          <span className="k-mono k-component__fact">
            a kind this build does not know — the server is the authority on
            whether it renders
          </span>
        )}
      </div>
      {data ? (
        // A database has no pods, so no log stream to offer: what it is doing
        // is the data services section's answer, per environment (#107).
        <span className="k-mono k-component__fact">
          a managed data service — its preset and health are in Data services
        </span>
      ) : environment !== undefined ? (
        <Link
          className="k-button"
          to={`/projects/${encodeURIComponent(project)}/${encodeURIComponent(
            environment,
          )}/logs?component=${encodeURIComponent(component.name)}`}
        >
          Logs
        </Link>
      ) : null}
    </li>
  );
}

/** The one thing worth saying about a component beside its kind. */
function componentFact(component: ComponentSummary): string {
  if (isDataComponentKind(component.kind)) {
    return component.preset === "" ? "the model's default preset" : `preset: ${component.preset}`;
  }
  const shape =
    component.schedule !== ""
      ? component.schedule
      : component.port !== ""
        ? `port ${component.port}`
        : "no port, no schedule";
  // Rule P3, which is what makes one repository ship a web process and a
  // worker: a component with no image of its own runs the project's.
  return component.image === ""
    ? `${shape} · the project's image`
    : `${shape} · ${component.image}`;
}

/** A verdict row as rendered: the fetched one, or the stream's delta over it. */
interface VerdictRow {
  resource: string;
  code: string;
  healthy: boolean;
  degraded: boolean;
  message: string;
  remediation: string;
}

/** What the stream has said about this environment since Status was read. */
interface PanelLive {
  transition?: {
    phase: string;
    /** The phase this transition left — how the rail places a rejection. */
    previousPhase: string;
    revision: string;
    cause: string;
  };
  verdicts: Record<string, { code: string; healthy: boolean; message: string }>;
}

const NO_LIVE: PanelLive = { verdicts: {} };

function EnvironmentPanel({
  project,
  environment,
  others,
  documents,
}: {
  project: string;
  environment: string;
  /**
   * The project's other environments — the promotion's possible sources. A
   * project with only this one has nothing to promote from, and the action says
   * so rather than opening a screen with an empty picker.
   */
  others: string[];
  /** The stored documents, which is where a data component's preset lives. */
  documents: SpecDocuments | undefined;
}) {
  const clients = useClients();
  const status = useAsync(
    (signal) =>
      clients.deploy.status(
        { spec: { spec: { case: "project", value: project } }, environment },
        { signal },
      ),
    [clients, project, environment],
  );

  // The same watch the project list opens (#76), narrowed to this one
  // environment: the status block follows transitions, the workload list
  // follows health changes.
  const [live, setLive] = useState<PanelLive>(NO_LIVE);
  const onEvent = useCallback((event: WatchResponse_Event) => {
    const payload = event.payload;
    setLive((prev) => {
      if (payload.case === "statusTransition") {
        const { phase, previousPhase, revision, cause } = payload.value;
        return { ...prev, transition: { phase, previousPhase, revision, cause } };
      }
      if (payload.case === "healthChange") {
        const v = payload.value;
        return {
          ...prev,
          verdicts: {
            ...prev.verdicts,
            [v.resource]: {
              code: v.code,
              healthy: v.healthy,
              message: v.message,
            },
          },
        };
      }
      return prev;
    });
  }, []);
  const reload = status.reload;
  const onResync = useCallback(() => {
    setLive(NO_LIVE);
    reload();
  }, [reload]);
  const scopes = useMemo(
    () => [{ project, environment }],
    [project, environment],
  );
  const watch = useWatch({ scopes, onEvent, onResync });

  const failure =
    status.error === undefined ? undefined : toFailure(status.error);
  const phase = live.transition?.phase ?? status.data?.phase;
  const revision = live.transition
    ? live.transition.revision
    : status.data?.revision;
  const cause = live.transition ? live.transition.cause : status.data?.cause;
  const verdicts = useMemo(
    () => mergeVerdicts(status.data?.verdicts, live.verdicts),
    [status.data, live.verdicts],
  );
  // A data component's own resource is not a workload, so it is taken out of
  // the workload list and handed to the section that knows what it is. The
  // verdicts themselves are untouched: one health source, two readers.
  const workloads = useMemo(
    () => verdicts.filter((v) => !isDataServiceVerdict(v.resource)),
    [verdicts],
  );
  // The same rail the deploy screen draws, in compact form, off Status plus the
  // stream's deltas — so an environment that goes stuck or degraded while this
  // page is open says so, and says what to do, without a reload.
  //
  // Two things the deploy stream has are missing here and are NOT invented:
  // StatusResponse carries no delivery mode or adapter, so the reconciler stage
  // reads "not reported" until a failure cause names a component; and it
  // carries no `stuck` flag, so a stuck verdict is recovered from the engine's
  // own cause reasons (rail.ts:isStuckReason).
  const railInput = useMemo<RailInput>(
    () => ({
      phase: phase ?? "",
      cause: parseCause(cause ?? ""),
      reachedPhase: live.transition?.previousPhase,
      unhealthyWorkloads: workloads.filter((v) => !v.healthy).length,
    }),
    [phase, cause, live.transition, workloads],
  );
  const documentText = useMemo(
    () => ({
      project: decodeDocument(documents?.project),
      environment: decodeDocument(documents?.environments[environment]),
    }),
    [documents, environment],
  );
  // Rollback used to share a precondition with this page's Status call: both
  // came from the same delivery plane, so Status failing Unimplemented meant
  // Rollback would too. The rebuilt server (ADR-0028, R2 #225) split them —
  // Status's Unimplemented now means only that this build has no workload
  // observation client, which says nothing about whether the Environment
  // store Rollback needs is configured. So Rollback is no longer preemptively
  // disabled here: like Deploy, Diff and Logs, it stays a plain link and
  // shows its own honest error if the call itself fails.
  const base = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(environment)}`;

  return (
    <section className="k-section">
      <div className="k-env">
        <div className="k-env__head">
          <div className="k-env__ident">
            <span className="k-chip k-mono">{environment}</span>
            {failure !== undefined ? (
              <StatusPill status="unknown" label="status unavailable" />
            ) : status.loading && status.data === undefined ? (
              <StatusPill status="unknown" label="reading…" />
            ) : (
              <StatusPill
                status={phaseToStatus(phase ?? "")}
                label={phase?.toLowerCase() || "unknown"}
              />
            )}
            <span className="k-mono">
              <LiveIndicator state={watch} />
            </span>
          </div>
          <div className="k-actions">
            <Link className="k-button k-button--primary" to={`${base}/deploy`}>
              Deploy
            </Link>
            <Link className="k-button" to={`${base}/diff`}>
              Diff
            </Link>
            {/* Named from this environment's side, because that is the side the
                reader is standing on: the environment in the tab is the one the
                pins are written to, and the source is picked on the screen.
                Promotion writes the spec and deploys nothing, so it sits with
                the other spec-shaped actions and not next to Deploy. */}
            {others.length === 0 ? (
              <button
                type="button"
                className="k-button"
                disabled
                title={`${project} declares no other environment to promote from — a promotion has a source and a target`}
              >
                Promote into this environment
              </button>
            ) : (
              <Link className="k-button" to={`${base}/promote`}>
                Promote into this environment
              </Link>
            )}
            <Link className="k-button" to={`${base}/logs`}>
              Logs
            </Link>
            {/* History reads the Environment's own record and needs no
                observation client, so it is offered even when Status could
                not be read — the past is exactly what a reader wants when the
                present is unavailable. */}
            <Link className="k-button" to={`${base}/history`}>
              History
            </Link>
            <Link className="k-button" to={`${base}/rollback`}>
              Rollback
            </Link>
          </div>
        </div>

        {failure !== undefined ? (
          <ErrorPanel
            title="Could not read this environment's status"
            error={status.error}
          />
        ) : null}

        {status.data !== undefined ? (
          <>
            <PhaseRail
              input={railInput}
              project={project}
              environment={environment}
              compact
              label={`Deployment phase for ${environment}`}
            />

            <div className="k-kv">
              <span className="k-kv__key">revision</span>
              <span>
                {revision ? <Copyable value={revision} /> : "none recorded"}
              </span>
              {cause ? (
                <>
                  <span className="k-kv__key">cause</span>
                  <span>{cause}</span>
                </>
              ) : null}
              {Object.entries(status.data.detail)
                .sort(([a], [b]) => a.localeCompare(b))
                .map(([k, v]) => (
                  <Fragmented key={k} name={k} value={v} />
                ))}
            </div>

            <div className="k-env__verdicts">
              <div className="k-eyebrow">Workloads ({workloads.length})</div>
              {workloads.length === 0 ? (
                <p className="k-mono k-env__note">
                  no verdicts — this build has no health probe wired, or the set
                  declares no Deployments. That is not the same as “nothing is
                  failing”.
                </p>
              ) : (
                <ul className="k-verdicts">
                  {workloads.map((v) => (
                    <Verdict key={v.resource} verdict={v} />
                  ))}
                </ul>
              )}
            </div>
          </>
        ) : null}

        {/* Outside the status block on purpose: what a spec declares is
            readable without a cluster, and an environment whose status cannot
            be read still has databases worth describing. */}
        <DataServices
          project={project}
          environment={environment}
          projectDoc={documentText.project}
          environmentDoc={documentText.environment}
          health={
            status.data !== undefined
              ? { state: "read", verdicts }
              : status.loading
                ? { state: "loading" }
                : { state: "unavailable" }
          }
        />

        {/* A preview is a *child* of this environment rather than a part of it
            (ADR-0017): kelson recorded no Environment document for it and its
            phase is flux-operator's, not the state machine's. So it sits below
            the environment's own state rather than among the workloads, for the
            same reason the data services do — reading it as one of this
            environment's resources is the mistake the placement prevents. */}
        <Previews project={project} environment={environment} />

        {/* Beside the data services, and outside the status block for the same
            reason: the Secrets an environment holds are readable whether or not
            its workloads are. A spec's `{secret: <name>, key: <key>}` points
            here, and #116 is what writes what it points at. */}
        <SecretsPanel project={project} environment={environment} />
      </div>
    </section>
  );
}

/** The stored bytes as text. Absent documents are an empty document. */
function decodeDocument(bytes: Uint8Array | undefined): string {
  return bytes === undefined ? "" : new TextDecoder().decode(bytes);
}

function Fragmented({ name, value }: { name: string; value: string }) {
  return (
    <>
      <span className="k-kv__key">{name}</span>
      <span>{value}</span>
    </>
  );
}

/**
 * The fetched verdicts with the stream's deltas applied.
 *
 * A HEALTH_CHANGE carries a code, a verdict and a message — not the
 * remediation, which is the fix for the code it replaced, so it is dropped
 * rather than left pointing at the wrong problem. `degraded` is likewise
 * recomputed as the softer claim the event supports: the failure-code set is
 * observation's (IsFailure) and lives in Go, so the browser must not guess
 * which side of it a live code falls on.
 *
 * A workload the event names but Status never returned is appended: a
 * Deployment that appeared since the last read is real, and hiding it until
 * the next refetch would be the staleness this stream exists to remove.
 */
function mergeVerdicts(
  fetched: readonly WorkloadVerdict[] | undefined,
  live: PanelLive["verdicts"],
): VerdictRow[] {
  const pending = { ...live };
  const rows: VerdictRow[] = (fetched ?? []).map((v) => {
    const update = pending[v.resource];
    if (update === undefined) {
      return {
        resource: v.resource,
        code: v.code,
        healthy: v.healthy,
        degraded: v.degraded,
        message: v.message,
        remediation: v.remediation,
      };
    }
    delete pending[v.resource];
    return {
      resource: v.resource,
      code: update.code,
      healthy: update.healthy,
      degraded: !update.healthy,
      message: update.message,
      remediation: "",
    };
  });
  for (const [resource, update] of Object.entries(pending)) {
    rows.push({
      resource,
      code: update.code,
      healthy: update.healthy,
      degraded: !update.healthy,
      message: update.message,
      remediation: "",
    });
  }
  return rows;
}

/**
 * A verdict row, shaped like the CLI's (#151): the health code as a mono chip,
 * the message, and the remediation as a "fix:" line.
 */
function Verdict({ verdict }: { verdict: VerdictRow }) {
  const kind = verdict.healthy
    ? "synced"
    : verdict.degraded
      ? "degraded"
      : "failed";
  return (
    <li className="k-verdict">
      <div className="k-verdict__head">
        <span className="k-mono k-verdict__resource">{verdict.resource}</span>
        <StatusPill status={kind} label={verdict.code || "unknown"} />
      </div>
      {verdict.message ? (
        <p className="k-verdict__message">{verdict.message}</p>
      ) : null}
      {verdict.remediation ? (
        <p className="k-verdict__fix">
          <span className="k-verdict__fix-label">fix:</span>{" "}
          {verdict.remediation}
        </p>
      ) : null}
    </li>
  );
}

function Documents({
  project,
  documents,
}: {
  project: string;
  documents: SpecDocuments;
}) {
  const envs = Object.entries(documents.environments).sort(([a], [b]) =>
    a.localeCompare(b),
  );
  return (
    <section className="k-section">
      <div className="k-env__head">
        <div className="k-eyebrow">Spec documents ({1 + envs.length})</div>
        {/* Editing is a write to the store and nothing else: it changes what
            would be deployed, never what is running. The Deploy button above is
            the separate act (#65). */}
        <Link
          className="k-button"
          to={`/projects/${encodeURIComponent(project)}/edit`}
        >
          Edit configuration
        </Link>
      </div>
      <div className="k-section__body k-docs">
        <Disclosure summary="Project" meta={`${documents.project.length} bytes`}>
          <YamlBlock bytes={documents.project} />
        </Disclosure>
        {envs.map(([name, bytes]) => (
          <Disclosure
            key={name}
            summary={`Environment · ${name}`}
            meta={`${bytes.length} bytes`}
          >
            <YamlBlock bytes={bytes} />
          </Disclosure>
        ))}
      </div>
    </section>
  );
}
