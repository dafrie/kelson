import { useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import type { HistoryEntry } from "../gen/kelson/v1alpha1/deploy_pb";
import type { DiffResponse } from "../gen/kelson/v1alpha1/render_pb";
import { ErrorPanel } from "../components/ErrorPanel";
import { EmptyState, LoadingState } from "../components/States";
import { DiffView } from "../diff/DiffView";
import { decodeDiff, type Diff } from "../diff/parse";
import type { Async } from "../api/data";

/**
 * What would change, against either of the two things there are to change from.
 *
 * RenderService.Diff offers both and they answer different questions. SERVER is
 * the live cluster's own dry-run verdict — the same preview `kelson diff`
 * produces at L2 — and it is what a deploy would actually meet. `from_revision`
 * (#162, #247) is the rendered-level comparison: today's render against the
 * manifests a recorded revision actually rendered, pulled back out of the
 * registry as the immutable artifact that revision published. That is the
 * stored-spec answer to the CLI's `--from`, which this screen could not offer
 * before: the store keeps the current documents, not the previous ones, so the
 * only prior state the server can name is what it recorded when it deployed.
 *
 * Neither mode falls back to the other. internal/api/render.go is explicit that
 * a silently downgraded preview gives a CI gate a clean answer it did not earn,
 * so an unreachable cluster and an unreadable history each show up as the error
 * they are.
 */
export function DiffPage() {
  const { project = "", env = "" } = useParams();
  // `?from=<revision>` is how the history screen (#67) arrives: it names a
  // recorded revision, which is only meaningful in the rendered mode, so the
  // parameter selects that tab as well as the revision. Without it the screen
  // opens where it always did, on the live cluster's verdict.
  const [params] = useSearchParams();
  const from = params.get("from") ?? "";
  const [mode, setMode] = useState<"server" | "revision">(
    from === "" ? "server" : "revision",
  );

  return (
    <>
      <div className="k-page-head">
        <h1>Diff</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
        <span>·</span>
        <span>
          {mode === "server"
            ? "server dry-run, against the live cluster"
            : "rendered, against a deployed revision"}
        </span>
      </div>

      <nav className="k-tabs" aria-label="Compare against">
        <button
          type="button"
          className={mode === "server" ? "k-tab k-tab--active" : "k-tab"}
          aria-current={mode === "server" ? "true" : undefined}
          onClick={() => setMode("server")}
        >
          Against live cluster
        </button>
        <button
          type="button"
          className={mode === "revision" ? "k-tab k-tab--active" : "k-tab"}
          aria-current={mode === "revision" ? "true" : undefined}
          onClick={() => setMode("revision")}
        >
          Against deployed revision
        </button>
      </nav>

      <p className="k-note">
        {mode === "server"
          ? "What a deploy would change, asked of the cluster itself. Needs a reachable cluster."
          : "Today's render against what that revision actually deployed — the recorded manifests, not a re-render of the old spec."}
      </p>

      {mode === "server" ? (
        <ServerDiff project={project} env={env} />
      ) : (
        <RevisionDiff project={project} env={env} initial={from} />
      )}
    </>
  );
}

function ServerDiff({ project, env }: { project: string; env: string }) {
  const clients = useClients();
  const result = useAsync(
    (signal) =>
      clients.render.diff(
        {
          spec: { spec: { case: "project", value: project } },
          environment: env,
          dryRun: DryRun.SERVER,
        },
        { signal },
      ),
    [clients, project, env],
  );

  return <DiffResult result={result} what="the server dry-run preview" />;
}

function RevisionDiff({
  project,
  env,
  initial,
}: {
  project: string;
  env: string;
  /** A revision named in the URL, preselected. Empty means none. */
  initial: string;
}) {
  const clients = useClients();
  const [revision, setRevision] = useState(initial);
  const history = useAsync(
    (signal) =>
      clients.deploy.history(
        {
          spec: { spec: { case: "project", value: project } },
          environment: env,
        },
        { signal },
      ),
    [clients, project, env],
  );

  const entries = history.data?.entries ?? [];

  // No revision, no request: a diff against nothing would be every resource
  // reported as an addition, which is the answer this mode exists to avoid.
  const result = useAsync(
    async (signal) =>
      revision === ""
        ? undefined
        : clients.render.diff(
            {
              spec: { spec: { case: "project", value: project } },
              environment: env,
              fromRevision: revision,
            },
            { signal },
          ),
    [clients, project, env, revision],
  );

  return (
    <>
      <section className="k-section">
        <div className="k-eyebrow">Compare against which revision</div>
        <div className="k-section__body">
          {history.loading && history.data === undefined ? (
            <LoadingState what="the recorded revisions" />
          ) : null}
          {history.error !== undefined ? (
            <ErrorPanel
              title="Cannot read the recorded history"
              error={history.error}
            />
          ) : null}
          {history.data !== undefined && entries.length === 0 ? (
            <EmptyState title="No deploys yet">
              There is nothing to compare against. The other tab asks the live
              cluster instead.
            </EmptyState>
          ) : null}
          {entries.length > 0 ? (
            <ul className="k-revisions">
              {entries.map((entry, i) => (
                <Revision
                  key={entry.revision}
                  entry={entry}
                  current={i === 0}
                  checked={entry.revision === revision}
                  onSelect={setRevision}
                />
              ))}
            </ul>
          ) : null}
        </div>
      </section>

      {revision !== "" ? (
        <DiffResult result={result} what={`the diff against ${revision}`} />
      ) : null}
    </>
  );
}

/**
 * The rendered half of both modes: one decode, one view, one set of errors.
 * A payload that will not decode is reported rather than dropped — a blank
 * screen where a diff should be reads as "nothing changed".
 */
function DiffResult({
  result,
  what,
}: {
  result: Async<DiffResponse | undefined>;
  what: string;
}) {
  const decoded = useMemo((): { diff?: Diff; error?: string } => {
    const bytes = result.data?.diffJson;
    if (bytes === undefined || bytes.length === 0) return {};
    try {
      return { diff: decodeDiff(bytes) };
    } catch (err) {
      return { error: err instanceof Error ? err.message : String(err) };
    }
  }, [result.data]);

  return (
    <>
      {result.loading && result.data === undefined ? (
        <LoadingState what={what} />
      ) : null}

      {result.error !== undefined ? (
        <ErrorPanel title="The preview failed" error={result.error} />
      ) : null}

      {result.data?.errors.length ? (
        <ErrorPanel title="The spec was rejected" errors={result.data.errors} />
      ) : null}

      {decoded.error !== undefined ? (
        <ErrorPanel
          title="The diff payload could not be decoded"
          error={new Error(decoded.error)}
        />
      ) : null}

      {decoded.diff !== undefined ? (
        <DiffView
          diff={decoded.diff}
          exitSemantics={result.data?.exitSemantics}
        />
      ) : null}
    </>
  );
}

/**
 * One revision to compare against. Unlike the rollback picker, the newest
 * revision is selectable: comparing today's render against what is deployed
 * right now is the drift question, and it is the most useful one here.
 */
function Revision({
  entry,
  current,
  checked,
  onSelect,
}: {
  entry: HistoryEntry;
  current: boolean;
  checked: boolean;
  onSelect: (revision: string) => void;
}) {
  return (
    <li className={current ? "k-revision k-revision--current" : "k-revision"}>
      <label className="k-revision__label">
        <input
          type="radio"
          name="diff-revision"
          value={entry.revision}
          checked={checked}
          onChange={() => onSelect(entry.revision)}
        />
        <span className="k-mono k-revision__rev">{entry.revision}</span>
        {current ? <span className="k-chip k-mono">current</span> : null}
        <span className="k-mono k-revision__meta">
          {[entry.committedAt, entry.author, entry.specHash]
            .filter((s) => s !== "")
            .join(" · ")}
        </span>
      </label>
      {entry.message ? (
        <p className="k-revision__message">{entry.message}</p>
      ) : null}
    </li>
  );
}
