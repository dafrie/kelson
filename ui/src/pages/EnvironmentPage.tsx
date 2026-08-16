import { useMemo } from "react";
import { Link, NavLink, Outlet, useOutletContext, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import type { SpecDocuments } from "../gen/kelson/v1alpha1/common_pb";
import { ErrorPanel } from "../components/ErrorPanel";
import { EmptyState } from "../components/States";
import { environmentPath } from "./flows";

/**
 * One environment, with its views as tabs and its actions as actions (#260).
 *
 * Six routes used to hang off `(project, environment)` as siblings — deploy,
 * diff, history, logs, rollback, promote — and a reader had to know which of the
 * six answered the question they had. They were not siblings. `logs` and
 * `history` are two ways of looking at *this environment*, so they are tabs of
 * it and keep their paths. `deploy`, `rollback` and `promote` are things a
 * person does to it, so they are actions: entered from the bar below, and
 * finished by coming back here. `diff` was never a place — it is a panel of the
 * screens that already had a comparison to show.
 *
 * This layout is the frame that makes that true, and it is deliberately thin: it
 * reads the spec, because the spec is what says this environment exists and what
 * its siblings are (the promotion's possible sources), and it hands the stored
 * documents to the Overview tab so the tab does not fetch them a second time.
 * Every cluster-touching call still belongs to the tab that needs it — the logs
 * tab opens no `Status`, and the environment's own status is read once, by
 * Overview, exactly as the project page's column used to read it.
 */
export function EnvironmentPage() {
  const { project = "", env = "" } = useParams();
  const clients = useClients();
  const spec = useAsync(
    (signal) => clients.spec.getSpec({ project }, { signal }),
    [clients, project],
  );

  const environments = useMemo(
    () => spec.data?.spec?.environments ?? [],
    [spec.data],
  );
  const declared = spec.data === undefined || environments.includes(env);
  const others = useMemo(
    () => environments.filter((name) => name !== env),
    [environments, env],
  );
  const context = useMemo<EnvironmentContext>(
    () => ({
      project,
      environment: env,
      others,
      documents: spec.data?.spec?.documents,
      specLoading: spec.loading && spec.data === undefined,
      specError: spec.error,
    }),
    [project, env, others, spec.data, spec.loading, spec.error],
  );

  const base = environmentPath(project, env);
  // Promotion writes this environment's pins and reads another environment's
  // revision, so a project with one environment has nothing to promote from and
  // the action says so rather than opening a screen with an empty picker. Only
  // once the spec has actually been read: "we have not looked yet" is not "there
  // is nowhere".
  const nowhereToPromoteFrom = spec.data !== undefined && others.length === 0;

  return (
    <>
      <div className="k-page-head">
        <h1>{env}</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span>environment</span>
      </div>

      {/* Tabs on the left are where this environment can be looked at; actions
          on the right are what can be done to it. They share one bar because
          they are the same layer of navigation, and keeping the actions on it
          means a reader tailing the logs can deploy without going back first. */}
      <div className="k-bar">
        <nav className="k-tabs" aria-label={`${env} views`}>
          <Tab to={base} end>
            Overview
          </Tab>
          <Tab to={`${base}/logs`}>Logs</Tab>
          <Tab to={`${base}/history`}>History</Tab>
        </nav>
        <div className="k-actions">
          <Link className="k-button k-button--primary" to={`${base}/actions/deploy`}>
            Deploy
          </Link>
          {/* Named from this environment's side, because that is the side the
              reader is standing on: the environment in the path is the one the
              pins are written to, and the source is picked on the screen. */}
          {nowhereToPromoteFrom ? (
            <button
              type="button"
              className="k-button"
              disabled
              title={`${project} declares no other environment to promote from`}
            >
              Promote into this environment
            </button>
          ) : (
            <Link className="k-button" to={`${base}/actions/promote`}>
              Promote into this environment
            </Link>
          )}
          <Link className="k-button" to={`${base}/actions/rollback`}>
            Rollback
          </Link>
        </div>
      </div>

      {spec.error !== undefined ? (
        <ErrorPanel
          title={`Cannot read the spec for ${project}`}
          error={spec.error}
        />
      ) : null}

      {declared ? (
        <Outlet context={context} />
      ) : (
        <EmptyState title={`${project} declares no environment called ${env}`}>
          The stored spec is what this page reads. Check the name, or add the
          environment on the edit screen.
        </EmptyState>
      )}
    </>
  );
}

/**
 * A tab: a link that looks like the tab strip and knows when it is the one.
 *
 * A link and not a button, because a tab here is a *route* — the whole point of
 * the consolidation is that the logs and the history keep addresses a person can
 * paste. `NavLink` marks the active one `aria-current="page"` itself.
 */
function Tab({
  to,
  end = false,
  children,
}: {
  to: string;
  /** Overview is the index route, so only an exact match makes it the one. */
  end?: boolean;
  children: string;
}) {
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) => (isActive ? "k-tab k-tab--active" : "k-tab")}
    >
      {children}
    </NavLink>
  );
}

/**
 * What the layout has already read, handed to the tab below it.
 *
 * The stored documents are the expensive part of `GetSpec` and the Overview tab
 * needs all of them; passing them down is what keeps switching tabs from
 * re-reading the project once per tab.
 */
export interface EnvironmentContext {
  project: string;
  environment: string;
  /** The project's other environments — the promotion's possible sources. */
  others: string[];
  documents: SpecDocuments | undefined;
  specLoading: boolean;
  specError: unknown;
}

export function useEnvironment(): EnvironmentContext {
  return useOutletContext<EnvironmentContext>();
}
