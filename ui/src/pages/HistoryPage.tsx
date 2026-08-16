import { Link, useParams, useSearchParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { toFailure } from "../api/errors";
import type { HistoryEntry } from "../gen/kelson/v1alpha1/deploy_pb";
import { Copyable } from "../components/Copyable";
import { DriftMark } from "../components/DriftMark";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import {
  driftFor,
  statusForDelivery,
  type Drift,
} from "../components/status";
import { EmptyState, LoadingState } from "../components/States";
import { Detail, KubeFact } from "../expert/Detail";
import { revisionParts } from "../expert/facts";
import { Evidence, Why } from "../expert/Why";
import { ComparePanel } from "../diff/ComparePanel";
import { formatWhen, shortHash } from "./history";

/**
 * The release history: what was deployed to this environment, newest first.
 *
 * # What this screen can say, and what it refuses to
 *
 * `DeployService.History` returns nine fields per revision — revision, spec
 * hash, committed-at, message, author, digest, images, outcome and
 * beyond-window — and the
 * screen shows what it can of those and derives nothing beyond them (see
 * ./history.ts, which holds the derivations and the reasons each one is
 * conservative). The outcome, the digest and the images used to travel as prose
 * inside `message` because `HistoryEntry` had no field for any of them; they
 * have fields now, and this screen reads the fields. `message` is not rendered
 * at all — the server keeps filling it for one release for clients built
 * against the older schema, and showing prose beside the same facts in fields
 * would be saying everything twice.
 *
 * Two limits still shape the layout and are stated on the screen rather than
 * papered over:
 *
 *  1. **A recorded outcome is a snapshot, not a live answer.** `outcome` is what
 *     the controller recorded when that revision stopped being the current one,
 *     and nothing refreshes it afterwards. So it is shown on every row as a
 *     record — labelled "recorded", in the muted meta line — while the phase
 *     pill, the same vocabulary as everywhere else, appears on exactly one row:
 *     the revision `DeployService.Status` reports as live, which is the only
 *     revision anything can currently answer *for right now*. A live-looking
 *     pill down the whole column would be an invention.
 *  2. **No author yet.** The spine records who deployed nothing
 *     (`internal/api`'s `History`), human or agent, so every entry reads
 *     "unattributed" and the note says why rather than guessing. That
 *     attribution is #74's work.
 *  3. **Some rows are only a revision, and say so.** The list now runs past the
 *     bounded history the cluster keeps into the registry's tag list, which is
 *     the record (ADR-0028 decision 4, #241). A `beyondWindow` entry carries
 *     one fact — this revision exists and can still be restored — so its whole
 *     meta line is replaced by a sentence saying nothing else was recorded.
 *     Rendering it like any other row would print an absent outcome and an
 *     absent timestamp beside "unattributed" and let a reader take the blanks
 *     for a deployment that had none.
 *
 * # A tab, a panel and one link
 *
 * This is the History tab of the environment (#260) rather than a screen of its
 * own, so the environment's name, its other views and its actions are the
 * layout's above — what is left here is the record.
 *
 * The comparison a row offers is a **panel**, not a link to a diff screen: it
 * opens under `?from=<revision>` on this same page, because comparing a revision
 * to what the spec says now is a question about a row a reader is already
 * looking at. `?compare=1` opens the same panel on its other mode, the live
 * cluster's dry-run verdict, which is what the retired `/diff` route showed when
 * it was given no revision.
 *
 * Rollback stays a link, and stays whole: its irreversibility preview must
 * never be skipped, so there is one rollback flow in the UI and not two. The row
 * sends its revision to it as `?to=`.
 *
 * Note what the comparison means here, because the honest version is narrower
 * than it sounds: `RenderService.Diff` compares the *current* spec against the
 * manifests a recorded revision actually rendered (`from_revision`). Revision A
 * against revision B is not a call the server offers, and `HistoryEntry` carries
 * no rendered manifests to do it client-side either, so the action is named for
 * what it does — compare against what is deployed now.
 */
export function HistoryPage() {
  const { project = "", env = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const from = params.get("from") ?? "";
  const comparing = from !== "" || params.get("compare") === "1";
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
  const liveAnswer = status.data?.answer ?? "";
  // The drift is the environment's and it belongs on the one row that is about
  // right now. A pinned rollback is exactly the case a reader opens this screen
  // for: the live marker is on an older row and this says it was chosen.
  const drift =
    status.data === undefined ? undefined : driftFor(status.data);
  const statusFailure =
    status.error === undefined ? undefined : toFailure(status.error);
  const statusLoading = status.loading && status.data === undefined;
  const base = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(env)}`;

  const compare = (next: URLSearchParams) => setParams(next, { replace: true });

  return (
    <>
      {/* The comparison against the live cluster is not about any one row, so it
          is offered once, above them. It is the mode the retired diff route
          opened on, and the reason that route no longer needs to exist. */}
      <div className="k-actions k-history__actions">
        {comparing ? null : (
          <button
            type="button"
            className="k-button"
            onClick={() => {
              const next = new URLSearchParams(params);
              next.set("compare", "1");
              compare(next);
            }}
          >
            Diff against the cluster
          </button>
        )}
        <span className="k-deploy__note">newest first</span>
      </div>

      {comparing ? (
        <ComparePanel
          // Keyed by the revision so that choosing a different row's comparison
          // starts the panel on it rather than leaving the first one selected.
          key={from}
          project={project}
          env={env}
          from={from}
          onClose={() => {
            const next = new URLSearchParams(params);
            next.delete("from");
            next.delete("compare");
            compare(next);
          }}
        />
      ) : null}

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
          A rollback adds no entry either — it repoints Flux at a revision that
          is already here.
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
                  live={liveRevision !== "" && entry.revision === liveRevision}
                  livePhase={livePhase}
                  liveAnswer={liveAnswer}
                  drift={drift}
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
            <p className="k-timeline__note">
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
                    : "the phase pill is live; a row’s recorded outcome is what was true when it stopped being current"}
            </p>
            <p className="k-timeline__note">
              kelson does not record who deployed yet (
              <a href="https://github.com/dafrie/kelson/issues/74">#74</a>), and
              there is no commit or pull-request link — a revision is an
              artifact, not a commit.
            </p>
          </div>
        </section>
      ) : null}
    </>
  );
}

/**
 * The one health claim on this screen, in the shared vocabulary: it appears on
 * exactly one row — the revision Status reports as live — because that is the
 * only one anything can answer for right now.
 */
function LivePill({ phase, answer }: { phase: string; answer: string }) {
  const state = statusForDelivery(answer, phase);
  return <StatusPill status={state.tone} label={state.word} />;
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
  liveAnswer,
  drift,
  newest,
}: {
  entry: HistoryEntry;
  base: string;
  live: boolean;
  livePhase: string;
  liveAnswer: string;
  drift: Drift | undefined;
  newest: boolean;
}) {
  const when = formatWhen(entry.committedAt);

  return (
    <li
      className={
        live ? "k-timeline__item k-timeline__item--live" : "k-timeline__item"
      }
    >
      <div className="k-timeline__head">
        {/* The revision id is already short — <generation>-<hash8>
            (ADR-0028 decision 2) — so it needs no further abbreviation. */}
        <Copyable value={entry.revision} className="k-timeline__rev" />
        {entry.beyondWindow ? (
          <span className="k-chip">registry only</span>
        ) : null}
        {live ? (
          <>
            <span className="k-chip k-timeline__live">deployed now</span>
            {/* "Deployed now" is a claim about one row out of a list of
                records, and it rests entirely on a second call: the caret says
                which one and what it answered. */}
            <Why statement="deployed now">
              <Evidence
                rows={[
                  { name: "this revision", value: entry.revision },
                  {
                    name: "generation",
                    value: revisionParts(entry.revision)?.generation ?? "",
                  },
                  { name: "answer", value: liveAnswer },
                  { name: "phase", value: livePhase },
                ]}
                note="The status call reports this revision as the one serving. Every other row carries a recorded outcome instead, frozen when it stopped being current."
              />
            </Why>
            <LivePill phase={livePhase} answer={liveAnswer} />
            {/* The row's own revision id is the mono value this qualifies, one
                element to the left of it, so the mark says only why it matters.
                It is the answer to "why is the marker not on the top row". */}
            <DriftMark drift={drift} />
          </>
        ) : null}
      </div>

      {/* A revision the cluster's bounded history has forgotten (#241). The
          registry's tag list confirms it exists and holds nothing else about
          it, so the whole meta line below — which would otherwise print
          "unattributed" beside four absent facts — is replaced by the sentence
          that says the record is thin rather than the deployment featureless.
          It is still restorable: the artifact is immutable. */}
      {entry.beyondWindow ? (
        <div className="k-timeline__meta">
          <span title="older than the history the cluster keeps; confirmed against the registry's tag list">
            only the registry remembers this revision — nothing was recorded
            about when it was published, what it ran, or how it ended
          </span>
        </div>
      ) : (
        <div className="k-timeline__meta">
          {when !== "" ? <span className="k-mono">{when}</span> : null}
          {/* The generation half of the tag above, named. A reader comparing
              this list against the cluster is comparing generations. */}
          <Detail>
            <KubeFact
              name="generation"
              value={revisionParts(entry.revision)?.generation ?? ""}
            />
          </Detail>
          {/* The recorded outcome is prefixed rather than shown as a pill, so it
            cannot be mistaken for the live one above it: it says how that
            deployment ended, not how it is. */}
          {entry.outcome !== "" ? (
            <span title="recorded when this revision stopped being the current one, and frozen since">
              recorded {entry.outcome.toLowerCase()}
            </span>
          ) : null}
          {entry.specHash !== "" ? (
            <span title={entry.specHash}>
              spec <span className="k-mono">{shortHash(entry.specHash)}</span>
            </span>
          ) : null}
          {entry.digest !== "" ? (
            <span title={entry.digest}>
              artifact <span className="k-mono">{shortHash(entry.digest)}</span>
            </span>
          ) : null}
          <span
            className={
              entry.author === "" ? "k-timeline__unattributed" : undefined
            }
            title={
              entry.author === ""
                ? "kelson does not record who deployed yet (#74)"
                : entry.author
            }
          >
            {entry.author === "" ? "unattributed" : entry.author}
          </span>
        </div>
      )}

      {/* Each image under the component that resolved it, which is what the
          controller recorded (ADR-0028 decision 4). A component name is absent
          only on an entry recorded before it did that, and the image then
          stands alone rather than under a guessed label. */}
      {entry.images.length > 0 ? (
        <div className="k-timeline__images k-mono">
          {entry.images.map((image) => (
            <div
              className="k-timeline__image"
              key={`${image.component} ${image.image}`}
            >
              {image.component !== "" ? (
                <span className="k-timeline__component">{image.component}</span>
              ) : null}
              <span>{image.image}</span>
            </div>
          ))}
        </div>
      ) : null}

      {/* Both actions stay links even though one of them opens a panel on this
          same page: the comparison a reader is looking at is worth a URL, and
          `?from=` is the URL the retired diff route redirects into. */}
      <div className="k-timeline__actions">
        <Link
          className="k-button"
          to={`${base}/history?from=${encodeURIComponent(entry.revision)}`}
        >
          Diff against current
        </Link>
        {newest ? null : (
          <Link
            className="k-button"
            to={`${base}/actions/rollback?to=${encodeURIComponent(entry.revision)}`}
          >
            Roll back to this
          </Link>
        )}
      </div>
    </li>
  );
}
