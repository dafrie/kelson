import { useMemo } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { toFailure } from "../api/errors";
import { Copyable } from "../components/Copyable";
import { DriftMark } from "../components/DriftMark";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { driftFor, statusFor, verdictTone } from "../components/status";
import { EmptyState, LoadingState } from "../components/States";
import { EffectiveConfigTable } from "../config/EffectiveConfigTable";
import { Detail, KubeFact } from "../expert/Detail";
import { resourceParts, revisionParts } from "../expert/facts";
import { Evidence, Why } from "../expert/Why";
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
  deliveryFacts,
  mergeVerdicts,
  NO_READ,
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
        ? { ...NO_READ, environment: env }
        : {
            ...deliveryFacts(status.data, undefined),
            environment: env,
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
          <>
            <StatusPill status={cell.status.tone} label={cell.status.word} />
            {/* The one claim on this page a reader is most likely to
                misattribute: a component with no probe of its own shows its
                environment's word, and the caret is where that is said in
                fields rather than in a note at the bottom of the page. */}
            <Why statement={cell.status.word}>
              <Evidence
                rows={
                  verdict === undefined
                    ? [
                        { name: "basis", value: cell.basis },
                        { name: "answer", value: read.answer },
                        { name: "phase", value: read.phase },
                        { name: "cause", value: read.cause, prose: true },
                      ]
                    : [
                        { name: "basis", value: cell.basis },
                        { name: "resource", value: verdict.resource },
                        { name: "code", value: verdict.code },
                        { name: "healthy", value: String(verdict.healthy) },
                        { name: "degraded", value: String(verdict.degraded) },
                        { name: "stuck", value: String(verdict.stuck) },
                      ]
                }
                note={
                  cell.basis === "unread"
                    ? "The status call did not answer, so nothing is claimed about this component."
                    : verdict === undefined
                      ? `Nothing reports on ${component} separately, so this is ${env}'s own word.`
                      : verdict.healthy
                        ? `This component's own probe is healthy, so the word is ${env}'s — the delivery answer is the wider claim and it wins.`
                        : "This component's own probe decided the word, overruling the environment's."
                }
              />
            </Why>
          </>
        )}
      </div>
      <div className="k-page-sub">
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        {/* The environment is a link, not a label: it is where this component's
            logs, history and actions live (#260), so the page it names is one
            press away rather than a path a reader reassembles. */}
        <Link className="k-chip" to={base}>
          {env}
        </Link>
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
              {/* Two tabs of this component's environment and two of its
                  actions (#260). Diff is not among them any more: it is not a
                  place, it is what the deploy shows before it writes and what a
                  history row opens against a revision. */}
              <div className="k-actions">
                {data ? null : (
                  <Link
                    className="k-button"
                    to={`${base}/logs?component=${encodeURIComponent(component)}`}
                  >
                    Logs
                  </Link>
                )}
                <Link
                  className="k-button k-button--primary"
                  to={`${base}/actions/deploy`}
                >
                  Deploy
                </Link>
                <Link className="k-button" to={`${base}/history`}>
                  History
                </Link>
                <Link className="k-button" to={`${base}/actions/rollback`}>
                  Rollback
                </Link>
              </div>
            </div>
            <div className="k-section__body">
              <div className="k-kv">
                <Fact name="shape" value={shapeOf(summary)} prose />
                {data ? (
                  <Fact
                    name="preset"
                    value={summary.preset || "the model's default preset"}
                    prose={summary.preset === ""}
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
                          <span className="k-component__fact">
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
                      <span className="k-component__fact">
                        the environment's, not this component's
                      </span>{" "}
                      {/* Drift is the environment's fact too, so it goes here
                          rather than beside the page's own pill — that pill is
                          this component's word and must not be qualified by
                          something that is not about it. */}
                      <DriftMark drift={driftFor(read)} />{" "}
                      <Detail>
                        <span className="k-kfacts">
                          <KubeFact
                            name="generation"
                            value={
                              revisionParts(read.revision)?.generation ?? ""
                            }
                          />
                          <KubeFact
                            name="spec hash"
                            value={revisionParts(read.revision)?.specHash ?? ""}
                          />
                        </span>
                      </Detail>
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
                {read.cause ? (
                  <Fact name="cause" value={read.cause} prose />
                ) : null}
              </div>
            </div>
          </section>

          {/* The three-level merge, drawn. The facts above are what this page
              could read out of the documents itself; this is the answer only
              the resolver has — every effective setting and the block that set
              it — and it is a third call rather than a derivation, because a
              second copy of the precedence rules in a browser would drift from
              the one the renderer uses. */}
          <EffectiveConfigTable
            project={project}
            environment={env}
            component={component}
          />

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
                      <ResourceParts resource={verdict.resource} />
                      <StatusPill
                        status={verdictTone(verdict)}
                        label={verdict.code || "unknown"}
                      />
                      {/* A stuck verdict keeps a wait code, so the code chip
                          cannot say it and the word has to. */}
                      {verdict.stuck ? (
                        <>
                          <StatusPill
                            status={statusFor("stuck").tone}
                            label="stuck"
                          />
                          <Why statement="stuck">
                            <Evidence
                              rows={[
                                {
                                  name: "healthy",
                                  value: String(verdict.healthy),
                                },
                                {
                                  name: "degraded",
                                  value: String(verdict.degraded),
                                },
                                { name: "stuck", value: "true" },
                                { name: "code", value: verdict.code },
                              ]}
                              note="The probe made no progress on this workload before its budget expired, and it was not failing — which is why the code stays a wait code and the word has to say the rest."
                            />
                          </Why>
                        </>
                      ) : null}
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
                <p className="k-env__note">
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

/**
 * The three parts of the resource observation named, in expert mode.
 *
 * The whole string is beside this already; what is added is which part is the
 * kind and which the namespace, because that is the difference between reading
 * `Deployment/checkout-production/web` and recognising it.
 */
function ResourceParts({ resource }: { resource: string }) {
  const parts = resourceParts(resource);
  if (parts === undefined) return null;
  return (
    <Detail>
      <span className="k-kfacts">
        <KubeFact name="kind" value={parts.kind} />
        <KubeFact name="namespace" value={parts.namespace} />
        <KubeFact name="name" value={parts.name} />
      </span>
    </Detail>
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
    return <Fact name="source" value="none declared — image only" prose />;
  }
  if (binding.basis === "undeclared") {
    return (
      <Fact
        name="source"
        value={`${binding.requested} — not declared by this project`}
        prose
      />
    );
  }
  if (binding.basis === "ambiguous") {
    return (
      <Fact
        name="source"
        value="several declared and none named default — this component names none"
        prose
      />
    );
  }
  const source = binding.source;
  if (source === undefined) return null;
  return (
    <>
      <span className="k-kv__key">source</span>
      <span className="k-kv__prose">
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

/**
 * One row of the fact grid: a label and a value.
 *
 * The grid renders values in mono because a value here is what the machine
 * holds — an image reference, a namespace, a phase. `prose` is the opt-out for
 * the rows whose "value" is a sentence somebody wrote (a shape read out loud, a
 * cause the engine phrased, a source that has to explain itself), because mono
 * on a sentence is the claim this UI spends its type system making, said about
 * something that is not true of it (#260).
 */
function Fact({
  name,
  value,
  prose,
}: {
  name: string;
  value: string;
  prose?: boolean;
}) {
  return (
    <>
      <span className="k-kv__key">{name}</span>
      <span className={prose ? "k-kv__prose" : undefined}>{value}</span>
    </>
  );
}
