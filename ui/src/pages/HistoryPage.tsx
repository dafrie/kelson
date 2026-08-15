import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { toFailure } from "../api/errors";
import type { HistoryEntry } from "../gen/kelson/v1alpha1/deploy_pb";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { phaseToStatus } from "../components/phase";
import { EmptyState, LoadingState } from "../components/States";
import { formatWhen, shortSpecHash } from "./history";

/**
 * The release history: what was deployed to this environment, newest first.
 *
 * # What this screen can say, and what it refuses to
 *
 * `DeployService.History` returns five strings per revision — revision, spec
 * hash, committed-at, message, author — and the screen shows those five and
 * derives nothing beyond them (see ./history.ts, which holds the derivations and
 * the reasons each one is conservative). Under the rebuilt delivery spine
 * (ADR-0028), `message` is where the outcome, the digest and the images the
 * revision resolved to travel — `HistoryEntry` has no field of its own for any
 * of them yet — so it is rendered as the server's own prose, unparsed. Two
 * absences still shape the layout and are stated on the screen rather than
 * papered over:
 *
 *  1. **`message`'s outcome is a snapshot, not a live answer.** It is what was
 *     true when the entry was captured, and nothing refreshes it afterwards. So
 *     the phase pill — the same vocabulary as everywhere else — appears on
 *     exactly one row: the revision `DeployService.Status` reports as live,
 *     which is the only revision anything can currently answer *for right now*.
 *     A green pill down the whole column would be an invention.
 *  2. **No author yet.** The spine records who deployed nothing
 *     (`internal/api`'s `History`), human or agent, so every entry reads
 *     "unattributed" and the note says why rather than guessing. That
 *     attribution is #74's work.
 *
 * # The two actions are links, not copies
 *
 * Diff and rollback already have screens that do those jobs properly — one with
 * both comparison modes and its own error handling, the other with the
 * irreversibility preview that must never be skipped. This screen sends the
 * revision to them as a query parameter and stays out of the way, so there is
 * one rollback flow in the UI and not two.
 *
 * Promotion is linked the same way and deliberately *not* per revision: it pins
 * this environment to what another environment's latest revision runs, so the
 * revision it reads is never one picked from this list. A "promote this
 * revision" button would be a promise the RPC does not make.
 *
 * Note what "diff" means here, because the honest version is narrower than it
 * sounds: `RenderService.Diff` compares the *current* spec against the manifests
 * a recorded revision actually rendered (`from_revision`). Revision A against
 * revision B is not a call the server offers, and `HistoryEntry` carries no
 * rendered manifests to do it client-side either, so the action is named for
 * what it does — compare against what is deployed now.
 */
export function HistoryPage() {
  const { project = "", env = "" } = useParams();
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

  // Status is what makes "deployed now" a fact instead of a position in a list.
  // Both modes answer it from the same place the history comes from — direct
  // mode reports its latest journal record, the Git modes the observed revision
  // — so matching on the id marks the right row even when the newest recorded
  // revision is not the live one, which is precisely the case a reader is on
  // this screen to notice. Its failure is not the history's failure: a server
  // built without a delivery plane still has a readable past.
  const status = useAsync(
    (signal) =>
      clients.deploy.status(
        {
          spec: { spec: { case: "project", value: project } },
          environment: env,
        },
        { signal },
      ),
    [clients, project, env],
  );

  const entries = history.data?.entries ?? [];
  const liveRevision = status.data?.revision ?? "";
  const livePhase = status.data?.phase ?? "";
  const statusFailure =
    status.error === undefined ? undefined : toFailure(status.error);
  const statusLoading = status.loading && status.data === undefined;
  const base = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(env)}`;

  return (
    <>
      <div className="k-page-head">
        <h1>History</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
        <span>·</span>
        <span>newest first</span>
      </div>

      <p className="k-note">
        Every revision kelson published to this environment, newest first. Each
        one is an immutable artifact tagged with the generation that produced it
        and a short hash of the spec that rendered it; the message underneath is
        what the controller recorded about it — the outcome at the time, the
        artifact digest, the images it resolved to.
      </p>

      {/* Promotion is the one action here that is not about a revision in this
          list, so it is offered once, above it, rather than on every row: it
          reads whatever the *source* environment's latest revision runs, and a
          per-row button would suggest a reader could promote the revision they
          clicked. */}
      <div className="k-actions k-history__actions">
        <Link className="k-button" to={`${base}/promote`}>
          Promote into this environment
        </Link>
        <span className="k-mono k-deploy__note">
          pins {env} to the images another environment's latest revision runs —
          it writes the spec and deploys nothing, and it never promotes a
          revision picked from the list below
        </span>
      </div>

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
        <EmptyState title="No recorded history">
          Nothing has been deployed for this environment yet. A revision is
          recorded when a deploy publishes one — a rollback repoints Flux at a
          revision that is already here and adds no entry of its own — so an
          empty history means the environment has never been written to, not
          that the record was lost.
        </EmptyState>
      ) : null}

      {entries.length > 0 ? (
        <section className="k-section">
          <div className="k-eyebrow">Revisions ({entries.length})</div>
          <div className="k-section__body">
            <ol className="k-timeline">
              {entries.map((entry, i) => (
                <Revision
                  key={entry.revision}
                  entry={entry}
                  base={base}
                  live={
                    liveRevision !== "" && entry.revision === liveRevision
                  }
                  livePhase={livePhase}
                  // The rollback screen disables its newest entry — restoring
                  // the revision you are already on is not a rollback — so the
                  // action is not offered for a target that would arrive
                  // disabled.
                  newest={i === 0}
                />
              ))}
            </ol>

            {/* Four states and four sentences, because "still reading" and
                "the server named nothing" are different answers and only one
                of them is about this environment. */}
            <p className="k-mono k-timeline__note">
              {statusFailure !== undefined
                ? `no “deployed now” marker: the live revision could not be read${
                    statusFailure.code === undefined
                      ? ""
                      : ` (${statusFailure.code})`
                  }`
                : statusLoading
                  ? "reading which revision is live…"
                  : liveRevision === ""
                    ? "no “deployed now” marker: the server reports no live revision for this environment"
                    : "the phase pill is the live revision’s current state — recorded history holds no outcome for the revisions above it"}
            </p>
            <p className="k-mono k-timeline__note">
              kelson does not record who deployed yet, human or agent (
              <a href="https://github.com/dafrie/kelson/issues/74">#74</a>).
              There is no commit or pull-request link either: a revision is an
              OCI artifact in a registry, not a commit in a repository, so
              there is no forge to point at.
            </p>
          </div>
        </section>
      ) : null}
    </>
  );
}

/**
 * One revision. The head line is identity — what it is and whether it is live —
 * and everything under it is what the record actually said.
 */
function Revision({
  entry,
  base,
  live,
  livePhase,
  newest,
}: {
  entry: HistoryEntry;
  base: string;
  live: boolean;
  livePhase: string;
  newest: boolean;
}) {
  const when = formatWhen(entry.committedAt);

  return (
    <li className={live ? "k-timeline__item k-timeline__item--live" : "k-timeline__item"}>
      <div className="k-timeline__head">
        {/* The revision id is already short — <generation>-<hash8>
            (ADR-0028 decision 2) — so it needs no further abbreviation. */}
        <Copyable value={entry.revision} className="k-timeline__rev" />
        {live ? (
          <>
            <span className="k-chip k-mono k-timeline__live">deployed now</span>
            <StatusPill
              status={phaseToStatus(livePhase)}
              label={livePhase.toLowerCase() || "unknown"}
            />
          </>
        ) : null}
      </div>

      <div className="k-timeline__meta k-mono">
        {when !== "" ? <span>{when}</span> : null}
        {entry.specHash !== "" ? (
          <span title={entry.specHash}>spec {shortSpecHash(entry.specHash)}</span>
        ) : null}
        <span
          className={
            entry.author === "" ? "k-timeline__unattributed" : undefined
          }
          title={
            entry.author === ""
              ? "kelson does not record who deployed yet. Agent and human identities are not recorded yet (#74)."
              : entry.author
          }
        >
          {entry.author === "" ? "unattributed" : entry.author}
        </span>
      </div>

      {entry.message !== "" ? (
        <p className="k-timeline__message">{entry.message}</p>
      ) : null}

      <div className="k-timeline__actions">
        <Link
          className="k-button"
          to={`${base}/diff?from=${encodeURIComponent(entry.revision)}`}
        >
          Diff against current
        </Link>
        {newest ? null : (
          <Link
            className="k-button"
            to={`${base}/rollback?to=${encodeURIComponent(entry.revision)}`}
          >
            Roll back to this
          </Link>
        )}
      </div>
    </li>
  );
}
