import { useMemo } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { toFailure } from "../api/errors";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { verdictTone } from "../components/status";
import { EmptyState, LoadingState } from "../components/States";
import {
  bindingFor,
  effectiveImage,
  isDataComponentKind,
  parseComponents,
  parseOverrides,
  parseSources,
  projectImage,
  type ComponentSummary,
  type SourceBinding,
} from "../spec/components";
import {
  mergeVerdicts,
  readCell,
  verdictFor,
  type EnvironmentRead,
} from "./matrix";

/**
 * One component, in one environment — the page the matrix's cells link to
 * (#260).
 *
 * The component is the unit that deploys (docs/model.md §6) and until now it
 * had no address: its name was a query parameter on the log tail and a row on
 * the project page, and everything else about it was folded into an
 * environment. This is the pair as a page: what it is, what it is configured to
 * run, where that comes from, how it is doing, and the flows that act on it.
 *
 * # Two calls, no new fields
 *
 * `GetSpec` for what the documents say — kind, image, the source it builds from
 * (ADR-0035) — and `DeployService.Status` for what the cluster says. Nothing
 * here needs an RPC that does not exist, and nothing here invents one:
 *
 * - The **image is configuration**, not an observation. It is rule P3's winner
 *   across the three documents and it is labelled with the scope that set it,
 *   because "which file put this here" is the question a three-level merge
 *   otherwise hides. What is *running* is the revision's business, and the
 *   revision is the environment's.
 * - The **revision, phase and cause are the environment's**, said as such. A
 *   component does not have its own revision: one publish carries every
 *   component in the environment (ADR-0028).
 * - The **health is this component's**, when observation has one to give. It
 *   probes Deployments, so a `cron`, a chart and a database have none — and the
 *   page says there is none rather than showing the environment's green as if
 *   it were the component's.
 */
export function ComponentPage() {
  const { project = "", env = "", component = "" } = useParams();
  const clients = useClients();

  const spec = useAsync(
    (signal) => clients.spec.getSpec({ project }, { signal }),
    [clients, project],
  );
  const status = useAsync(
    (signal) =>
      clients.deploy.status(
        { spec: { spec: { case: "project", value: project } }, environment: env },
        { signal },
      ),
    [clients, project, env],
  );

  const documents = spec.data?.spec?.documents;
  const projectDoc = useMemo(
    () =>
      documents?.project === undefined
        ? ""
        : new TextDecoder().decode(documents.project),
    [documents],
  );
  const environmentDoc = useMemo(
    () =>
      documents?.environments[env] === undefined
        ? ""
        : new TextDecoder().decode(documents.environments[env]),
    [documents, env],
  );

  const summary = useMemo(
    () => parseComponents(projectDoc).find((c) => c.name === component),
    [projectDoc, component],
  );
  const sources = useMemo(() => parseSources(projectDoc), [projectDoc]);
  const image = useMemo(
    () =>
      summary === undefined
        ? undefined
        : effectiveImage(
            summary,
            parseOverrides(environmentDoc).get(component),
            projectImage(projectDoc),
          ),
    [summary, environmentDoc, component, projectDoc],
  );

  const failure =
    status.error === undefined ? undefined : toFailure(status.error);
  const read = useMemo<EnvironmentRead>(
    () =>
      status.data === undefined
        ? {
            environment: env,
            phase: "",
            revision: "",
            cause: "",
            namespace: "",
            verdicts: [],
            read: false,
          }
        : {
            environment: env,
            phase: status.data.phase,
            revision: status.data.revision,
            cause: status.data.cause,
            namespace: status.data.namespace,
            verdicts: mergeVerdicts(status.data.verdicts, {}),
            read: true,
          },
    [status.data, env],
  );
  const verdict =
    summary === undefined
      ? undefined
      : verdictFor(read, {
          project,
          component,
          kind: summary.kind,
        });
  const cell = readCell(read, verdict);

  const base = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(env)}`;
  const data = summary !== undefined && isDataComponentKind(summary.kind);

  return (
    <>
      <div className="k-page-head">
        <h1>{component}</h1>
        {summary === undefined ? null : (
          <StatusPill status={cell.status.tone} label={cell.status.word} />
        )}
      </div>
      <div className="k-page-sub">
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
        {summary !== undefined ? (
          <>
            <span>·</span>
            <span className="k-chip k-mono">{summary.kind}</span>
          </>
        ) : null}
      </div>

      {spec.loading && spec.data === undefined ? (
        <LoadingState what="the spec" />
      ) : null}

      {spec.error !== undefined ? (
        <ErrorPanel
          title={`Cannot read the spec for ${project}`}
          error={spec.error}
        />
      ) : null}

      {spec.data !== undefined && summary === undefined ? (
        <EmptyState title={`${project} declares no component called ${component}`}>
          The stored Project document is what this page reads. Check the name,
          or add the component on the edit screen.
        </EmptyState>
      ) : null}

      {summary !== undefined ? (
        <>
          <section className="k-section">
            <div className="k-env__head">
              <div className="k-eyebrow">What it runs</div>
              <div className="k-actions">
                {data ? null : (
                  <Link
                    className="k-button"
                    to={`${base}/logs?component=${encodeURIComponent(component)}`}
                  >
                    Logs
                  </Link>
                )}
                <Link className="k-button k-button--primary" to={`${base}/deploy`}>
                  Deploy
                </Link>
                <Link className="k-button" to={`${base}/history`}>
                  History
                </Link>
                <Link className="k-button" to={`${base}/diff`}>
                  Diff
                </Link>
                <Link className="k-button" to={`${base}/rollback`}>
                  Rollback
                </Link>
              </div>
            </div>
            <div className="k-section__body">
              <div className="k-kv">
                <Fact name="shape" value={shapeOf(summary)} />
                {data ? (
                  <Fact
                    name="preset"
                    value={summary.preset || "the model's default preset"}
                  />
                ) : (
                  <>
                    <span className="k-kv__key">image</span>
                    <span>
                      {image === undefined || image.image === "" ? (
                        "none configured — it builds from its source"
                      ) : (
                        <>
                          <Copyable value={image.image} />{" "}
                          <span className="k-mono k-component__fact">
                            set on the {image.scope}
                          </span>
                        </>
                      )}
                    </span>
                  </>
                )}
                <SourceFacts binding={bindingFor(summary, sources)} data={data} />
                <span className="k-kv__key">revision</span>
                <span>
                  {read.revision ? (
                    <>
                      <Copyable value={read.revision} />{" "}
                      <span className="k-mono k-component__fact">
                        the environment's, not this component's
                      </span>
                    </>
                  ) : failure !== undefined ? (
                    "not read"
                  ) : (
                    "none recorded"
                  )}
                </span>
                {read.namespace ? (
                  <Fact name="namespace" value={read.namespace} />
                ) : null}
                {read.phase ? <Fact name="phase" value={read.phase} /> : null}
                {read.cause ? <Fact name="cause" value={read.cause} /> : null}
              </div>
            </div>
          </section>

          <section className="k-section">
            <div className="k-env__head">
              <div className="k-eyebrow">Health</div>
            </div>
            <div className="k-section__body">
              {status.loading && status.data === undefined ? (
                <LoadingState what="the status" />
              ) : failure !== undefined ? (
                <ErrorPanel
                  title="Could not read this environment's status"
                  error={status.error}
                />
              ) : verdict !== undefined ? (
                <ul className="k-verdicts">
                  <li className="k-verdict">
                    <div className="k-verdict__head">
                      <span className="k-mono k-verdict__resource">
                        {verdict.resource}
                      </span>
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
                </ul>
              ) : (
                <p className="k-mono k-env__note">
                  {data
                    ? "no reading for this database — its section on the project page has what there is"
                    : `no reading for ${component} here — the word above is ${env}'s own`}
                </p>
              )}
            </div>
          </section>
        </>
      ) : null}
    </>
  );
}

/** The one field that says what a workload is: a port, a schedule, or neither. */
function shapeOf(component: ComponentSummary): string {
  if (isDataComponentKind(component.kind)) return component.kind;
  if (component.schedule !== "") return `runs at ${component.schedule}`;
  if (component.port !== "") return `serves on port ${component.port}`;
  return "no port, no schedule";
}

/**
 * The repository a component builds from, and how the binding was decided.
 *
 * The undeclared case is the careful one: a name this Project does not declare
 * may still be an instance-wide `GitSource` (ADR-0035's second tier), which no
 * RPC on this page can see. So it is reported as what is knowable — this
 * document does not declare it — and not as an error.
 */
function SourceFacts({
  binding,
  data,
}: {
  binding: SourceBinding;
  data: boolean;
}) {
  if (data) return null;
  if (binding.basis === "none") {
    return <Fact name="source" value="none declared — image only" />;
  }
  if (binding.basis === "undeclared") {
    return (
      <Fact
        name="source"
        value={`${binding.requested} — not declared by this project`}
      />
    );
  }
  if (binding.basis === "ambiguous") {
    return (
      <Fact
        name="source"
        value="several declared and none named default — this component names none"
      />
    );
  }
  const source = binding.source;
  if (source === undefined) return null;
  return (
    <>
      <span className="k-kv__key">source</span>
      <span>
        {source.name}
        {binding.basis === "named" ? "" : " (the project's default)"}
      </span>
      <Fact name="repository" value={source.git} />
      {source.ref ? <Fact name="ref" value={source.ref} /> : null}
      {source.connection ? (
        <Fact name="connection" value={source.connection} />
      ) : null}
    </>
  );
}

function Fact({ name, value }: { name: string; value: string }) {
  return (
    <>
      <span className="k-kv__key">{name}</span>
      <span>{value}</span>
    </>
  );
}
