import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { toFailure } from "../api/errors";
import type { HistoryEntry } from "../gen/kelson/v1alpha1/deploy_pb";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { phaseToStatus } from "../components/phase";
import { EmptyState, LoadingState } from "../components/States";
import {
  classifyRecord,
  formatWhen,
  isCommitRevision,
  shortRevision,
  shortSpecHash,
} from "./history";

/**
 * The release history: what was deployed to this environment, newest first.
 *
 * # What this screen can say, and what it refuses to
 *
 * `DeployService.History` returns five strings per revision — revision, spec
 * hash, committed-at, message, author — and the screen shows those five and
 * derives nothing beyond them (see ./history.ts, which holds the derivations and
 * the reasons each one is conservative). Three absences shape the whole layout
 * and are stated on the screen rather than papered over:
 *
 *  1. **No per-revision outcome is recorded.** Nothing on `HistoryEntry` says
 *     whether a revision became healthy, got stuck, or was rolled back out an
 *     hour later. So the phase pill — the same vocabulary as everywhere else —
 *     appears on exactly one row: the revision `DeployService.Status` reports as
 *     live, which is the only revision anything can currently answer for. Every
 *     other row gets the act it was (deploy or rollback, from the recorded
 *     message) and no health claim at all. A green pill down the whole column
 *     would be an invention.
 *  2. **No author, in half the deployment modes.** Direct mode never writes one
 *     (internal/delivery/direct records `deploy <spec-hash>` with an empty
 *     author); the Git modes write the commit signature. Neither records whether
 *     a human or an agent deployed — the Git modes put that in `Kelson-Actor`
 *     and `Kelson-Agent-Id` trailers, which `History()` does not project — so an
 *     entry without an author reads "unattributed" and the note says why. That
 *     attribution is #74's work, and until it lands, guessing it here would
 *     invent an audit trail.
 *  3. **No repository URL.** A revision that is a full commit sha *is* the
 *     manifests-repository commit (the Git modes record it as the revision), so
 *     it is labelled as one and offered for copying. It is not a hyperlink, and
 *     no pull request is named: History carries no remote URL and no PR number,
 *     so both would be a guess at someone else's forge.
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
        Every revision kelson recorded for this environment, as the delivery mode
        wrote it down. The record is the same five facts in direct and Git mode;
        what differs is that Git commits carry an author and direct-mode journal
        entries do not.
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
          recorded when a deploy or a rollback lands, so an empty history means
          the environment has never been written to — not that the record was
          lost.
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
              authorship is recorded by the delivery mode, not by kelson: Git
              modes carry the commit signature, direct mode records none, and
              neither distinguishes a human from an agent (
              <a href="https://github.com/dafrie/kelson/issues/74">#74</a>).
              Commit and pull-request links are absent for the same reason —
              History carries no repository URL to build one from.
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
  const kind = classifyRecord(entry.message);
  const when = formatWhen(entry.committedAt);
  const commit = isCommitRevision(entry.revision);
  const short = shortRevision(entry.revision);

  return (
    <li className={live ? "k-timeline__item k-timeline__item--live" : "k-timeline__item"}>
      <div className="k-timeline__head">
        {/* A shortened sha gets the short form as its label; the click still
            copies the whole id, and a revision that was never shortened keeps
            Copyable's own "copy <value>" accessible name. */}
        <Copyable
          value={entry.revision}
          {...(short === entry.revision ? {} : { label: short })}
          className="k-timeline__rev"
        />
        {kind !== "unknown" ? (
          <span className="k-chip k-mono">{kind}</span>
        ) : null}
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
        {commit ? (
          <span title={entry.revision}>manifests commit {short}</span>
        ) : null}
        <span
          className={
            entry.author === "" ? "k-timeline__unattributed" : undefined
          }
          title={
            entry.author === ""
              ? "This delivery mode records no author. Agent and human identities are not recorded yet (#74)."
              : "The commit signature the delivery mode wrote. It does not say whether a human or an agent deployed."
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
