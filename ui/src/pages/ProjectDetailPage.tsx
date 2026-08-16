import { useCallback, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { useWatch, type WatchState } from "../api/watch";
import { toFailure, type Failure } from "../api/errors";
import type { SpecDocuments } from "../gen/kelson/v1alpha1/common_pb";
import type { StatusResponse } from "../gen/kelson/v1alpha1/deploy_pb";
import type { WatchResponse_Event } from "../gen/kelson/v1alpha1/events_pb";
import { Copyable } from "../components/Copyable";
import { Disclosure, YamlBlock } from "../components/Disclosure";
import { ErrorPanel } from "../components/ErrorPanel";
import { LiveIndicator } from "../components/LiveIndicator";
import { StatusPill } from "../components/StatusPill";
import { statusForPhase, verdictTone } from "../components/status";
import { EmptyState, LoadingState } from "../components/States";
import { DataServices } from "../dataservices/DataServices";
import { isDataServiceVerdict } from "../dataservices/parse";
import { PhaseRail } from "../deploy/PhaseRail";
import { parseCause, type RailInput } from "../deploy/rail";
import { Previews } from "../previews/Previews";
import { SecretsPanel } from "../secrets/SecretsPanel";
import {
  effectiveImage,
  isDataComponentKind,
  isKnownKind,
  parseComponents,
  parseOverrides,
  projectImage,
  type ComponentSummary,
} from "../spec/components";
import {
  mergeVerdicts,
  NO_READ,
  readCell,
  shortImage,
  verdictFor,
  type EnvironmentRead,
  type LiveVerdict,
  type VerdictRow,
} from "./matrix";

/**
 * One project: what it is made of, where it runs, and how each of those is
 * doing — as a matrix (#260).
 *
 * The component is the lifecycle-bearing unit (docs/model.md §6: components
 * deploy independently, each carries its own spec-hash) and the environment is
 * what gives it a namespace, an image pin and a phase. Neither alone has a
 * status, so the thing with one is the *pair* — and a page whose subject is the
 * project draws every pair at once: components down, environments across. It is
 * Heroku's pipeline view transposed, because kelson projects have more
 * components than environments.
 *
 * What the grid replaced was a list of components with no state at all, above
 * one environment's panel: a reader could see what the project contained, or
 * how one environment was doing, and had to click between tabs to put the two
 * together.
 *
 * # One Status per environment, and one stream for the page
 *
 * The columns are `DeployService.Status` calls, one per environment — the
 * slow, cluster-touching call — issued together and reported per column, so an
 * unreachable environment darkens its own column and blanks nothing else. The
 * panel below reuses its column's answer rather than issuing a second call for
 * the environment already on screen.
 *
 * The live half is one `EventService.Watch` over every environment (#76), the
 * same one-stream rule the project list follows: transitions land on the column
 * they name and health changes on the cell.
 *
 * # Two answers per cell, and the cell says which one it has
 *
 * `Status` reports per-component health only for what observation probes, which
 * is Deployments. A `cron`, a chart and a database therefore have no reading of
 * their own, and their cells show the *environment's* word marked as such
 * (`matrix.ts`) rather than borrowing a claim nobody made.
 *
 * Below the matrix the environment in view keeps its full panel: the phase
 * rail, the workload verdicts, data services, previews, Secrets, and the six
 * flows — all of which are addressed by (project, environment) and stay that
 * way in this slice.
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

  const environments = useMemo(
    () => spec.data?.spec?.environments ?? [],
    [spec.data],
  );
  // The list is the read's key: a Status per environment is re-issued when the
  // set of environments changes, not when the response object is replaced.
  const key = environments.join("\u0000");
  const [active, setActive] = useState<string | undefined>(undefined);
  const selected = active !== undefined && environments.includes(active)
    ? active
    : environments[0];

  const statuses = useAsync(
    (signal) =>
      Promise.all(
        environments.map(async (environment): Promise<EnvironmentAnswer> => {
          try {
            const data = await clients.deploy.status(
              { spec: { spec: { case: "project", value: project } }, environment },
              { signal },
            );
            return { environment, data };
          } catch (error) {
            // Per column: one environment whose cluster is unreachable must
            // not take the other columns' answers down with it.
            return { environment, error };
          }
        }),
      ),
    // `key` stands in for the array, which is a new object every render.
    [clients, project, key],
  );

  const [live, setLive] = useState<Record<string, EnvironmentLive>>({});
  const onEvent = useCallback((event: WatchResponse_Event) => {
    const environment = event.environment;
    const payload = event.payload;
    setLive((prev) => {
      const current = prev[environment] ?? NO_EVENTS;
      if (payload.case === "statusTransition") {
        const { phase, previousPhase, revision, cause } = payload.value;
        return {
          ...prev,
          [environment]: {
            ...current,
            transition: { phase, previousPhase, revision, cause },
          },
        };
      }
      if (payload.case === "healthChange") {
        const v = payload.value;
        return {
          ...prev,
          [environment]: {
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

  const reload = statuses.reload;
  const onResync = useCallback(() => {
    setLive({});
    reload();
  }, [reload]);
  // The stream opens once every column has an answer: a delta applied before
  // its Status landed would be overwritten by the older answer. The check is
  // per environment rather than "the read finished", because a refetch keeps
  // the previous key's answers on screen while the new ones are in flight.
  const settled =
    environments.length > 0 &&
    environments.every((environment) =>
      statuses.data?.some((a) => a.environment === environment),
    );
  const scopes = useMemo(
    () => (settled ? environments.map((environment) => ({ project, environment })) : []),
    [settled, environments, project],
  );
  const watch = useWatch({ scopes, onEvent, onResync });

  const columns = useMemo(
    () => environments.map((environment) => column(environment, statuses, live)),
    [environments, statuses, live],
  );
  const documents = spec.data?.spec?.documents;
  const projectDoc = useMemo(
    () => decodeDocument(documents?.project),
    [documents],
  );
  const components = useMemo(() => parseComponents(projectDoc), [projectDoc]);
  const selectedColumn =
    columns.find((c) => c.environment === selected) ?? undefined;

  return (
    <>
      <div className="k-page-head">
        <h1>{project}</h1>
        {/* Adding one is an edit of the stored spec, so it goes to the editor
            rather than growing a second write path — the query parameter opens
            it on the panel that does it. */}
        <Link
          className="k-button"
          to={`/projects/${encodeURIComponent(project)}/edit?add=component`}
        >
          Add component
        </Link>
      </div>
      <div className="k-page-sub">
        <Link to="/projects">← all projects</Link>
        <span>·</span>
        <span>version {spec.data?.spec?.version || "—"}</span>
        <span>·</span>
        <span>
          {components.length}{" "}
          {components.length === 1 ? "component" : "components"}
        </span>
        <span>·</span>
        <span>
          {environments.length}{" "}
          {environments.length === 1 ? "environment" : "environments"}
        </span>
        <LiveIndicator state={watch} />
      </div>

      {spec.loading && spec.data === undefined ? (
        <LoadingState what="the spec" />
      ) : null}

      {spec.error !== undefined ? (
        <ErrorPanel title={`Cannot read the spec for ${project}`} error={spec.error} />
      ) : null}

      {spec.data !== undefined && environments.length === 0 ? (
        <EmptyState title="No environments yet">
          Add one on the edit screen — until then there is nothing to deploy,
          diff or roll back.
        </EmptyState>
      ) : null}

      {spec.data !== undefined && environments.length > 0 ? (
        <Matrix
          project={project}
          components={components}
          columns={columns}
          selected={selected}
          onSelect={setActive}
          documents={documents}
          projectDoc={projectDoc}
        />
      ) : null}

      {environments.length > 0 && selected !== undefined ? (
        <>
          <nav className="k-tabs" aria-label="Environments">
            {environments.map((env) => (
              <button
                key={env}
                type="button"
                className={env === selected ? "k-tab k-tab--active" : "k-tab"}
                aria-current={env === selected ? "true" : undefined}
                onClick={() => setActive(env)}
              >
                {env}
              </button>
            ))}
          </nav>
          <EnvironmentPanel
            // Keyed by the pair: switching tabs must not carry one
            // environment's panel state onto another's.
            key={`${project}/${selected}`}
            project={project}
            environment={selected}
            others={environments.filter((name) => name !== selected)}
            documents={documents}
            column={selectedColumn}
            watch={watch}
          />
        </>
      ) : null}

      {documents ? <Documents project={project} documents={documents} /> : null}
    </>
  );
}

/** One environment's Status call, settled either way. */
interface EnvironmentAnswer {
  environment: string;
  data?: StatusResponse;
  error?: unknown;
}

/** What the stream has said about one environment since its Status was read. */
interface EnvironmentLive {
  transition?: {
    phase: string;
    /** The phase this transition left — how the rail places a rejection. */
    previousPhase: string;
    revision: string;
    cause: string;
  };
  verdicts: Record<string, LiveVerdict>;
}

const NO_EVENTS: EnvironmentLive = { verdicts: {} };

/** One column of the matrix: an environment, as read and as streamed. */
interface Column {
  environment: string;
  read: EnvironmentRead;
  /** Decoded for the header's tooltip; `error` is what the panel renders. */
  failure: Failure | undefined;
  error: unknown;
  loading: boolean;
  previousPhase: string | undefined;
}

function column(
  environment: string,
  statuses: { data: EnvironmentAnswer[] | undefined; loading: boolean },
  live: Record<string, EnvironmentLive>,
): Column {
  const answer = statuses.data?.find((a) => a.environment === environment);
  const events = live[environment] ?? NO_EVENTS;
  const failure =
    answer?.error === undefined ? undefined : toFailure(answer.error);
  const loading = answer === undefined && statuses.loading;
  // A transition replaces the three fields it carries whole: the fetched
  // revision and cause described the phase the column has just left.
  const phase = events.transition?.phase ?? answer?.data?.phase ?? "";
  const revision = events.transition
    ? events.transition.revision
    : (answer?.data?.revision ?? "");
  const cause = events.transition
    ? events.transition.cause
    : (answer?.data?.cause ?? "");
  const read: EnvironmentRead =
    answer?.data === undefined
      ? { ...NO_READ, environment }
      : {
          environment,
          phase,
          revision,
          cause,
          namespace: answer.data.namespace,
          verdicts: mergeVerdicts(answer.data.verdicts, events.verdicts),
          read: true,
        };
  return {
    environment,
    read,
    failure,
    error: answer?.error,
    loading,
    previousPhase: events.transition?.previousPhase,
  };
}

/**
 * The matrix: one row per component, one column per environment.
 *
 * A row header states what the component *is* — the kind the document says or
 * the shape derives (docs/model.md, ADR-0014) — because that is a fact about
 * the project and does not change per environment. A cell states how it is
 * doing where it runs, and links to the page about exactly that pair.
 *
 * The image on a cell is the one the *documents* resolve to (rule P3: the
 * environment's pin, else the component's, else the project's), not one a
 * cluster reported. It is labelled as configuration on the component's own page
 * and shown here as the value that differs from cell to cell — the revision
 * does not, so it belongs to the column header where it is stated once.
 */
function Matrix({
  project,
  components,
  columns,
  selected,
  onSelect,
  documents,
  projectDoc,
}: {
  project: string;
  components: ComponentSummary[];
  columns: Column[];
  selected: string | undefined;
  onSelect: (environment: string) => void;
  documents: SpecDocuments | undefined;
  projectDoc: string;
}) {
  const fromProject = useMemo(() => projectImage(projectDoc), [projectDoc]);
  const overrides = useMemo(() => {
    const out = new Map<string, ReturnType<typeof parseOverrides>>();
    for (const c of columns) {
      out.set(
        c.environment,
        parseOverrides(decodeDocument(documents?.environments[c.environment])),
      );
    }
    return out;
  }, [columns, documents]);

  const cells = useMemo(
    () =>
      components.map((component) =>
        columns.map((c) => {
          const verdict = verdictFor(c.read, {
            project,
            component: component.name,
            kind: component.kind,
          });
          return readCell(c.read, verdict);
        }),
      ),
    [components, columns, project],
  );
  const borrowed = cells.some((row) =>
    row.some((cell) => cell.basis === "environment"),
  );

  return (
    <section className="k-section">
      <div className="k-env__head">
        <div className="k-eyebrow">Components × environments</div>
      </div>
      <div className="k-section__body">
        {components.length === 0 ? (
          <p className="k-note">
            No components were read from the stored Project document — the
            documents below are what this list comes from.
          </p>
        ) : (
          <>
            <div className="k-matrix__scroll">
              <table className="k-matrix">
                <thead>
                  <tr>
                    <th scope="col" className="k-matrix__corner">
                      Component
                    </th>
                    {columns.map((c) => (
                      <th
                        key={c.environment}
                        scope="col"
                        className="k-matrix__col"
                        aria-current={
                          c.environment === selected ? "true" : undefined
                        }
                      >
                        <ColumnHead
                          column={c}
                          selected={c.environment === selected}
                          onSelect={onSelect}
                        />
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {components.map((component, row) => (
                    <tr key={component.name}>
                      <th scope="row" className="k-matrix__row">
                        <span className="k-mono k-matrix__name">
                          {component.name}
                        </span>
                        <span className="k-chip k-mono">{component.kind}</span>
                        {isKnownKind(component.kind) ? null : (
                          <span className="k-mono k-component__fact">
                            a kind this build does not know
                          </span>
                        )}
                      </th>
                      {columns.map((c, index) => (
                        <td key={c.environment} className="k-matrix__cell">
                          <MatrixCell
                            project={project}
                            component={component}
                            environment={c.environment}
                            cell={cells[row]?.[index]}
                            image={
                              effectiveImage(
                                component,
                                overrides.get(c.environment)?.get(component.name),
                                fromProject,
                              ).image
                            }
                          />
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {borrowed ? (
              <p className="k-mono k-matrix__legend">
                env — the environment's own status: nothing reports on this
                component separately
              </p>
            ) : null}
          </>
        )}
      </div>
    </section>
  );
}

/**
 * A column header: the environment, its word, its revision — and the control
 * that brings its panel, and with it the six flows, into view below.
 */
function ColumnHead({
  column,
  selected,
  onSelect,
}: {
  column: Column;
  selected: boolean;
  onSelect: (environment: string) => void;
}) {
  const state = statusForPhase(column.read.phase);
  return (
    <>
      <button
        type="button"
        className={
          selected ? "k-matrix__env k-matrix__env--on" : "k-matrix__env"
        }
        aria-pressed={selected}
        onClick={() => onSelect(column.environment)}
      >
        <span className="k-mono">{column.environment}</span>
        {column.failure !== undefined ? (
          <span title={reasonOf(column.failure)}>
            <StatusPill status="unknown" label="status unavailable" />
          </span>
        ) : column.loading ? (
          <StatusPill status="unknown" label="reading…" />
        ) : (
          <StatusPill status={state.tone} label={state.word} />
        )}
      </button>
      <span className="k-mono k-matrix__rev">
        {column.read.revision || "no revision"}
      </span>
    </>
  );
}

function MatrixCell({
  project,
  component,
  environment,
  cell,
  image,
}: {
  project: string;
  component: ComponentSummary;
  environment: string;
  cell: ReturnType<typeof readCell> | undefined;
  image: string;
}) {
  if (cell === undefined) return null;
  const data = isDataComponentKind(component.kind);
  // The code when something is wrong, the configured image when nothing is:
  // a reader scanning for trouble wants the code, and a reader scanning a row
  // wants to know what each environment is set to run.
  const fact =
    cell.basis === "unread"
      ? "not read"
      : cell.code !== "" && cell.detail !== ""
        ? cell.code
        : data
          ? component.preset || "the model's default preset"
          : shortImage(image) || "no image configured";
  return (
    <Link
      className="k-cell"
      data-basis={cell.basis}
      data-word={cell.status.word}
      to={`/projects/${encodeURIComponent(project)}/${encodeURIComponent(
        environment,
      )}/components/${encodeURIComponent(component.name)}`}
      title={cell.detail || (image !== "" && !data ? image : undefined)}
    >
      <span className="k-cell__word">
        <StatusPill status={cell.status.tone} label={cell.status.word} />
        {cell.basis === "environment" ? (
          <span className="k-mono k-cell__basis">env</span>
        ) : null}
      </span>
      <span className="k-mono k-cell__fact">{fact}</span>
    </Link>
  );
}

function reasonOf(failure: Failure): string {
  const first = failure.wire[0];
  if (first) return `${first.code}: ${first.message}`;
  return failure.code ? `${failure.code}: ${failure.message}` : failure.message;
}

/**
 * The environment in view, whole: its phase rail, its workload verdicts, its
 * data services, its previews, its Secrets, and the flows that take a
 * (project, environment) pair.
 *
 * It reads the column the page already fetched rather than calling Status a
 * second time for an environment that is on screen twice.
 */
function EnvironmentPanel({
  project,
  environment,
  others,
  documents,
  column,
  watch,
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
  column: Column | undefined;
  watch: WatchState;
}) {
  const read = column?.read ?? { ...NO_READ, environment };
  const failure = column?.failure;
  const verdicts = read.verdicts;
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
  // StatusResponse carries no adapter name, so the reconciler stage
  // reads "not reported" until a failure cause names a component; and it
  // carries no `stuck` flag, so a stuck verdict is recovered from the engine's
  // own cause reasons (rail.ts:isStuckReason).
  const railInput = useMemo<RailInput>(
    () => ({
      phase: read.phase,
      cause: parseCause(read.cause),
      reachedPhase: column?.previousPhase,
      unhealthyWorkloads: workloads.filter((v) => !v.healthy).length,
    }),
    [read.phase, read.cause, column?.previousPhase, workloads],
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
            ) : column?.loading ? (
              <StatusPill status="unknown" label="reading…" />
            ) : (
              <EnvironmentStatus phase={read.phase} />
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
                title={`${project} declares no other environment to promote from`}
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
            error={column?.error}
          />
        ) : null}

        {read.read ? (
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
                {read.revision ? (
                  <Copyable value={read.revision} />
                ) : (
                  "none recorded"
                )}
              </span>
              {read.cause ? (
                <>
                  <span className="k-kv__key">cause</span>
                  <span>{read.cause}</span>
                </>
              ) : null}
              {read.namespace ? (
                <>
                  <span className="k-kv__key">namespace</span>
                  <span>{read.namespace}</span>
                </>
              ) : null}
            </div>

            <div className="k-env__verdicts">
              <div className="k-eyebrow">Workloads ({workloads.length})</div>
              {workloads.length === 0 ? (
                <p className="k-mono k-env__note">
                  no verdicts — nothing here is being watched, which is not the
                  same as nothing failing
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
            read.read
              ? { state: "read", verdicts }
              : column?.loading
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

/**
 * The environment's own pill: the shared word, with the wire's phase kept as a
 * labelled fact beside it rather than as the pill's text. The phase is what an
 * operator correlates with Flux and it stays reachable; it is not the answer to
 * "is my change live".
 */
function EnvironmentStatus({ phase }: { phase: string }) {
  const state = statusForPhase(phase);
  return (
    <>
      <StatusPill status={state.tone} label={state.word} />
      {phase !== "" ? (
        <span className="k-mono k-env__phase">phase {phase}</span>
      ) : null}
    </>
  );
}

/**
 * A verdict row, shaped like the CLI's (#151): the health code as a mono chip,
 * the message, and the remediation as a "fix:" line.
 */
function Verdict({ verdict }: { verdict: VerdictRow }) {
  return (
    <li className="k-verdict">
      <div className="k-verdict__head">
        <span className="k-mono k-verdict__resource">{verdict.resource}</span>
        {/* The label is observation's own code, rendered verbatim like every
            other structured code in this UI; only the colour is the shared
            vocabulary's, so a red line here means what a red pill means. */}
        <StatusPill
          status={verdictTone(verdict.healthy, verdict.degraded)}
          label={verdict.code || "unknown"}
        />
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
