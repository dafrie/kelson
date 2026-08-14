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
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
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
            <div className="k-panel k-panel--dim k-mono">
              no recorded history — nothing has been deployed for this
              environment, so there is nothing to roll back to
            </div>
          ) : null}
          {unknownRequest ? (
            <p className="k-mono k-rollback__requested">
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
        <ErrorPanel title="The rollback preview failed" error={previewRun.error} />
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
              <span className="k-mono k-deploy__note">
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

function Findings({ preview }: { preview: PreviewState }) {
  const findings = preview.preview.findings;
  const unrecoverable = findings.filter((f) => f.unrecoverable);
  return (
    <section className="k-section">
      <div className="k-eyebrow">
        What this rollback cannot revert ({unrecoverable.length} of{" "}
        {findings.length})
      </div>
      <div className="k-section__body k-rollback__preview">
        {findings.length === 0 ? (
          <div className="k-panel k-panel--dim k-mono">
            no findings. The preview event is always sent, even when the mode's
            recorded history cannot be read — an absent warning is not the same
            as nothing to warn about.
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
          <span className="k-mono k-diff__finding-where">{finding.cause}</span>
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
              restored <Copyable value={applied.committed.restoredRevision} /> as{" "}
              <Copyable value={applied.committed.asRevision} />
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
