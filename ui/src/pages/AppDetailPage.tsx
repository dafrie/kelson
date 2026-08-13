import { useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { toFailure } from "../api/errors";
import type { SpecDocuments } from "../gen/kelson/v1alpha1/common_pb";
import type { WorkloadVerdict } from "../gen/kelson/v1alpha1/deploy_pb";
import { Copyable } from "../components/Copyable";
import { Disclosure, YamlBlock } from "../components/Disclosure";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { phaseToStatus } from "../components/phase";
import { EmptyState, LoadingState } from "../components/States";

/**
 * One project: its environments' delivery state, its documents, its actions.
 *
 * The two halves answer different questions and neither substitutes for the
 * other (issue #53): the phase says whether the change arrived, the verdicts
 * say whether it works. Both are shown, always, and a phase of Healthy with a
 * crash-looping verdict underneath is a real and important thing to see.
 *
 * The spec documents are printed byte-faithfully. The server stores what was
 * authored (ADR-0013: the spec is the user's document) and re-serialising YAML
 * in the browser would reorder keys the author chose, so the bytes go to the
 * screen untouched.
 */
export function AppDetailPage() {
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
        <Link to="/apps">← all apps</Link>
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
            <EnvironmentPanel project={project} environment={selected} />
          ) : null}
        </>
      ) : null}

      {spec.data?.spec?.documents ? (
        <Documents documents={spec.data.spec.documents} />
      ) : null}
    </>
  );
}

function EnvironmentPanel({
  project,
  environment,
}: {
  project: string;
  environment: string;
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

  const failure =
    status.error === undefined ? undefined : toFailure(status.error);
  // Rollback is the one action with a precondition the server has already
  // answered: a delivery plane this build was started without is Unimplemented,
  // and there is no adapter to restore anything with. Everything else stays
  // enabled — a deploy preview is a pure render that needs no cluster, and a
  // failing diff or log query has its own honest error to show.
  const rollbackBlocked = failure?.code === "unimplemented";
  const base = `/apps/${encodeURIComponent(project)}/${encodeURIComponent(environment)}`;

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
                status={phaseToStatus(status.data?.phase ?? "")}
                label={status.data?.phase.toLowerCase() || "unknown"}
              />
            )}
          </div>
          <div className="k-actions">
            <Link className="k-button k-button--primary" to={`${base}/deploy`}>
              Deploy
            </Link>
            <Link className="k-button" to={`${base}/diff`}>
              Diff
            </Link>
            <Link className="k-button" to={`${base}/logs`}>
              Logs
            </Link>
            {rollbackBlocked ? (
              <button
                type="button"
                className="k-button"
                disabled
                title={failure ? `${failure.code}: ${failure.message}` : ""}
              >
                Rollback
              </button>
            ) : (
              <Link className="k-button" to={`${base}/rollback`}>
                Rollback
              </Link>
            )}
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
            <div className="k-kv">
              <span className="k-kv__key">revision</span>
              <span>
                {status.data.revision ? (
                  <Copyable value={status.data.revision} />
                ) : (
                  "none recorded"
                )}
              </span>
              {status.data.cause ? (
                <>
                  <span className="k-kv__key">cause</span>
                  <span>{status.data.cause}</span>
                </>
              ) : null}
              {Object.entries(status.data.detail)
                .sort(([a], [b]) => a.localeCompare(b))
                .map(([k, v]) => (
                  <Fragmented key={k} name={k} value={v} />
                ))}
            </div>

            <div className="k-env__verdicts">
              <div className="k-eyebrow">
                Workloads ({status.data.verdicts.length})
              </div>
              {status.data.verdicts.length === 0 ? (
                <p className="k-mono k-env__note">
                  no verdicts — this build has no health probe wired, or the set
                  declares no Deployments. That is not the same as “nothing is
                  failing”.
                </p>
              ) : (
                <ul className="k-verdicts">
                  {status.data.verdicts.map((v) => (
                    <Verdict key={v.resource} verdict={v} />
                  ))}
                </ul>
              )}
            </div>
          </>
        ) : null}
      </div>
    </section>
  );
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
 * A verdict row, shaped like the CLI's (#151): the health code as a mono chip,
 * the message, and the remediation as a "fix:" line.
 */
function Verdict({ verdict }: { verdict: WorkloadVerdict }) {
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

function Documents({ documents }: { documents: SpecDocuments }) {
  const envs = Object.entries(documents.environments).sort(([a], [b]) =>
    a.localeCompare(b),
  );
  return (
    <section className="k-section">
      <div className="k-eyebrow">Spec documents ({1 + envs.length})</div>
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
