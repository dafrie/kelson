import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { useRun } from "../api/stream";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import type {
  HistoryEntry,
  RollbackResponse_Committed,
  RollbackResponse_Finding,
  RollbackResponse_Preview,
  RollbackResponse_Settled,
} from "../gen/kelson/v1alpha1/deploy_pb";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { LoadingState } from "../components/States";
import { DiffView } from "../diff/DiffView";
import { decodeDiff, summaryLine, type Diff } from "../diff/parse";

/**
 * Rollback, preview first and always.
 *
 * The API refuses to do it any other way (internal/api's Rollback: "a rollback
 * is what people reach for when they are already in trouble, and the one thing
 * that must not happen is discovering afterwards that it could not restore what
 * they thought"), and neither does this screen: dry_run=RENDER streams the
 * Preview event, the findings are read, and only then is the apply offered.
 *
 * A finding marked unrecoverable is what a rollback *cannot revert* — it names
 * a change that will survive the restore. It is called out in the failed colour
 * and never folded in with the rest, because a reader who skims past one is
 * exactly the person the preview exists for.
 *
 * The revision list comes from the History RPC. It is the picker for this
 * action — which revisions can be restored, and which one is already live —
 * while the history screen (#67) is the screen about the past; it links here
 * with `?to=<revision>` and this screen preselects it and previews it. What it
 * cannot do is arrive with the rollback already applied: the parameter selects
 * a target and runs the dry run, and the apply stays behind the button, because
 * a link that deploys is a link someone can be handed.
 */

interface PreviewState {
  preview: RollbackResponse_Preview;
  diff: Diff | undefined;
  decodeError: string | undefined;
}

interface Applied {
  committed: RollbackResponse_Committed | undefined;
  settled: RollbackResponse_Settled | undefined;
}

export function RollbackPage() {
  const { project = "", env = "" } = useParams();
  const [params] = useSearchParams();
  const requested = params.get("to") ?? "";
  const clients = useClients();
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
  const [target, setTarget] = useState<string>("");
  const [preview, setPreview] = useState<PreviewState | undefined>(undefined);
  const [applied, setApplied] = useState<Applied | undefined>(undefined);
  const previewRun = useRun();
  const applyRun = useRun();

  const runPreview = useCallback(
    (revision: string) => {
      setPreview(undefined);
      setApplied(undefined);
      previewRun.start(async (signal) => {
        for await (const res of clients.deploy.rollback(
          {
            spec: { spec: { case: "project", value: project } },
            environment: env,
            toRevision: revision,
            dryRun: DryRun.RENDER,
          },
          { signal },
        )) {
          if (res.event.case !== "preview") continue;
          const value = res.event.value;
          let diff: Diff | undefined;
          let decodeError: string | undefined;
          if (value.diffJson.length > 0) {
            try {
              diff = decodeDiff(value.diffJson);
            } catch (err) {
              decodeError = err instanceof Error ? err.message : String(err);
            }
          }
          setPreview({ preview: value, diff, decodeError });
        }
      });
    },
    [previewRun, clients, project, env],
  );

  const runApply = useCallback(
    (revision: string) => {
      setApplied({ committed: undefined, settled: undefined });
      applyRun.start(async (signal) => {
        for await (const res of clients.deploy.rollback(
          {
            spec: { spec: { case: "project", value: project } },
            environment: env,
            toRevision: revision,
            dryRun: DryRun.NONE,
          },
          { signal },
        )) {
          const event = res.event;
          if (event.case === "committed") {
            setApplied((prev) => ({
              committed: event.value,
              settled: prev?.settled,
            }));
          } else if (event.case === "settled") {
            setApplied((prev) => ({
              committed: prev?.committed,
              settled: event.value,
            }));
          }
        }
      });
    },
    [applyRun, clients, project, env],
  );

  const select = useCallback(
    (revision: string) => {
      setTarget(revision);
      runPreview(revision);
    },
    [runPreview],
  );

  // A `?to=` that the history screen sent, applied once the revisions are
  // known. It is honoured only for a revision this environment actually
  // recorded and that the picker would let a click reach — the newest one is
  // disabled here, since restoring what is already live is not a rollback — so
  // a stale or hand-edited link cannot preview something the list does not
  // offer. The ref makes it a one-shot: after that the picker owns the target,
  // and re-running the preview under the reader would be the screen arguing
  // with them.
  const honoured = useRef(false);
  const requestable =
    requested !== "" && entries.slice(1).some((e) => e.revision === requested);
  useEffect(() => {
    if (honoured.current || !requestable) return;
    honoured.current = true;
    select(requested);
  }, [requestable, requested, select]);

  const unknownRequest =
    requested !== "" &&
    !requestable &&
    history.data !== undefined &&
    entries.length > 0;

  return (
    <>
      <div className="k-page-head">
        <h1>Rollback</h1>
      </div>
      <div className="k-page-sub">
        {/* Back to the environment, not to the project: an action is entered
            from the environment it acts on and finishes by returning to it
            (#260). The project stays one hop further out, through the
            environment's own breadcrumb. */}
        <Link
          to={`/projects/${encodeURIComponent(project)}/${encodeURIComponent(env)}`}
        >
          ← {env}
        </Link>
        <span>·</span>
        <span>{project}</span>
      </div>

      <section className="k-section">
        <div className="k-eyebrow">Restore which revision</div>
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
            <div className="k-panel k-panel--dim">
              no deploys yet, so there is nothing to roll back to
            </div>
          ) : null}
          {unknownRequest ? (
            <p className="k-rollback__requested">
              {entries[0]?.revision === requested
                ? `${requested} is what is deployed now — restoring it is not a rollback, so nothing is preselected`
                : `${requested} is not among the recorded revisions for this environment — pick a target below`}
            </p>
          ) : null}
          {entries.length > 0 ? (
            <ul className="k-revisions">
              {entries.map((entry, i) => (
                <Revision
                  key={entry.revision}
                  entry={entry}
                  current={i === 0}
                  checked={entry.revision === target}
                  onSelect={select}
                />
              ))}
            </ul>
          ) : null}
        </div>
      </section>

      {previewRun.running ? <LoadingState what="the rollback preview" /> : null}
      {previewRun.error !== undefined ? (
        <ErrorPanel
          title="The rollback preview failed"
          error={previewRun.error}
        />
      ) : null}

      {preview !== undefined ? (
        <>
          <Findings preview={preview} />
          <section className="k-section">
            <div className="k-eyebrow">Apply</div>
            <div className="k-section__body k-deploy__confirm">
              <button
                type="button"
                className="k-button k-button--primary k-button--wide"
                onClick={() => runApply(target)}
                disabled={applyRun.running}
              >
                {applyRun.running
                  ? "Rolling back…"
                  : `Restore ${preview.preview.toRevision} to ${project}/${env}`}
              </button>
              <span className="k-deploy__note">
                nothing has been written yet — the preview above is a dry run
              </span>
            </div>
          </section>
        </>
      ) : null}

      {applied !== undefined ? (
        <Outcome applied={applied} error={applyRun.error} />
      ) : null}
    </>
  );
}

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
          name="revision"
          value={entry.revision}
          checked={checked}
          disabled={current}
          onChange={() => onSelect(entry.revision)}
        />
        <span className="k-mono k-revision__rev">{entry.revision}</span>
        {current ? <span className="k-chip k-mono">current</span> : null}
        {/* A revision the cluster's bounded history has forgotten and the
            registry still holds (ADR-0028 decision 4, #241). It is a target
            like any other — the artifact is immutable — and the meta line
            beside it is empty because there is nothing recorded to put there,
            which is what the chip and the message below say out loud. */}
        {entry.beyondWindow ? (
          <span
            className="k-chip k-mono"
            title="older than the history kept in the cluster; confirmed against the registry's tag list"
          >
            registry only
          </span>
        ) : null}
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

/**
 * `rollback/preview-unavailable` is the finding the server sends when it could
 * not compute the comparison (internal/api's rollbackPreview, #247): normally
 * it pulls both revisions' artifacts and the preview carries a real diff, but a
 * server with no registry credential, an artifact the registry no longer
 * serves, or bytes that do not match their digest leaves nothing to compare —
 * which is not nothing to worry about. It is a statement about what kelson
 * looked at, never a claim that this specific rollback is safe, so it is pulled
 * out of the risk list and shown as a note instead: counting it among "what
 * this rollback cannot revert" would misname it as a change this rollback will
 * fail to undo, which is not what it says.
 */
const PREVIEW_UNAVAILABLE_CAUSE = "rollback/preview-unavailable";

/**
 * `rollback/beyond-window` is the second finding of that kind, and it arrives
 * only for a target older than the history the cluster keeps (#241): kelson
 * confirmed the revision against the registry's tag list, so the restore is
 * exact, and nothing recorded when it was published, what it ran or how it
 * ended. Like the gap above it, that is a statement about what kelson knows —
 * counting it among "what this rollback cannot revert" would name it as a
 * change that will survive the restore, which is not what it says.
 */
const BEYOND_WINDOW_CAUSE = "rollback/beyond-window";

/** The findings that describe kelson's own knowledge rather than a risk. */
const NOTE_CAUSES = [PREVIEW_UNAVAILABLE_CAUSE, BEYOND_WINDOW_CAUSE];

function Findings({ preview }: { preview: PreviewState }) {
  const all = preview.preview.findings;
  const notes = all.filter((f) => NOTE_CAUSES.includes(f.cause));
  const findings = all.filter((f) => !NOTE_CAUSES.includes(f.cause));
  const unrecoverable = findings.filter((f) => f.unrecoverable);
  return (
    <section className="k-section">
      <div className="k-eyebrow">
        What this rollback cannot revert ({unrecoverable.length} of{" "}
        {findings.length})
      </div>
      <div className="k-section__body k-rollback__preview">
        {notes.map((note) => (
          <div className="k-panel k-panel--dim" key={note.cause}>
            {note.message}
          </div>
        ))}

        {findings.length === 0 ? (
          <div className="k-panel k-panel--dim">
            no findings — nothing kelson checked would survive this rollback
          </div>
        ) : (
          <ul className="k-diff__findings">
            {findings.map((f, i) => (
              <Finding key={`${f.resource}:${f.path}:${i}`} finding={f} />
            ))}
          </ul>
        )}

        {preview.decodeError !== undefined ? (
          <ErrorPanel
            title="The preview's diff payload could not be decoded"
            error={new Error(preview.decodeError)}
          />
        ) : null}

        {preview.diff !== undefined ? (
          <>
            <div className="k-mono">{summaryLine(preview.diff)}</div>
            <DiffView diff={preview.diff} />
          </>
        ) : null}
      </div>
    </section>
  );
}

function Finding({ finding }: { finding: RollbackResponse_Finding }) {
  return (
    <li
      className={
        finding.unrecoverable
          ? "k-diff__finding k-diff__finding--blocking"
          : "k-diff__finding"
      }
    >
      <div className="k-diff__finding-head">
        <code className="k-mono">{finding.resource}</code>
        {finding.cause ? (
          <span className="k-diff__finding-note">{finding.cause}</span>
        ) : null}
        {finding.unrecoverable ? (
          <span className="k-pill k-pill--failed">
            <span className="k-pill__dot" />
            cannot revert
          </span>
        ) : null}
      </div>
      <p className="k-diff__finding-message">{finding.message}</p>
      {finding.path ? (
        <div className="k-mono k-diff__finding-where">{finding.path}</div>
      ) : null}
    </li>
  );
}

function Outcome({ applied, error }: { applied: Applied; error: unknown }) {
  return (
    <section className="k-section">
      <div className="k-eyebrow">Outcome</div>
      <div className="k-section__body k-stream">
        {applied.committed ? (
          <div className="k-stream__row">
            <span className="k-mono k-stream__label">committed</span>
            <span className="k-mono">
              restored <Copyable value={applied.committed.restoredRevision} />
              {applied.committed.asRevision !== "" ? (
                <>
                  {" "}
                  as <Copyable value={applied.committed.asRevision} />
                </>
              ) : (
                // A rollback publishes nothing and prepends no history entry
                // (ADR-0028 decision 5), so there is no second revision it was
                // "recorded as" — as_revision arrives empty, always, and a
                // blank chip here would look like a value that was dropped
                // rather than one that never existed.
                <span className="k-rollback__note">
                  {" "}
                  — no new revision recorded; a rollback publishes nothing
                </span>
              )}
            </span>
          </div>
        ) : null}

        {applied.settled?.error ? (
          <ErrorPanel
            title="The rollback failed"
            errors={[applied.settled.error]}
          />
        ) : null}

        {applied.settled !== undefined && !applied.settled.error ? (
          <div className="k-settled" role="status">
            <div className="k-settled__head">
              <span className="k-settled__title">Rollback settled</span>
            </div>
          </div>
        ) : null}

        {error !== undefined ? (
          <ErrorPanel title="The rollback stream failed" error={error} />
        ) : null}
      </div>
    </section>
  );
}
