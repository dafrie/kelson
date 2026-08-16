import { useId, useState } from "react";
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
import { useEnvironment } from "./EnvironmentPage";
import {
  formatWhen,
  railPlacements,
  shortHash,
  type RailPlacement,
} from "./history";

/**
 * The release history as a **rail**: what ran here, newest at the top, with one
 * marker on the revision that is running now (#260).
 *
 * # Why the sequence is a rail and not a list
 *
 * A list of revisions answers "what happened". The question people actually
 * bring to this screen is "what is running, and is it the top one" — and that is
 * a question about a *position*. So the record is drawn as a vertical rail with
 * one stop per revision and a single filled marker on the live one, and the
 * distance between the top of the rail and that marker is the whole statement.
 * An environment pinned by a rollback shows its marker two stops down and the
 * reader has the answer before reading a word.
 *
 * That composes with the drift mark rather than repeating it. `DriftMark` is
 * still the sentence — "pinned to an older revision" or "older than the spec" —
 * and it renders on the marker's own stop exactly as it did before. The rail
 * adds no second sentence and, deliberately, **no count**: "three revisions
 * behind" is a claim about the spec's generation, and the wire carries no
 * generation to support it (`StatusResponse.stale` is a boolean). Position is
 * what the screen can honestly show, so position is all it shows.
 *
 * The rail's tail is the revisions only the registry remembers (#241). It fades
 * — dotted rail, dimmed ink — because the record thins out there rather than
 * ending: those revisions exist and can still be restored, and nothing else
 * about them was written down.
 *
 * # Click-first, and no drag
 *
 * The UX research's direction B makes dragging the marker the rollback gesture
 * and dragging it sideways the promotion. Its own recommendation is to ship the
 * click first and add the gestures only once the confirm flows are proven, and
 * that is what this is: **a stop opens**. No drag, no modal, no second rollback
 * flow — the offer inside an opened stop is two links into the flows that
 * already exist, `?from=` for the comparison panel on this same page and
 * `actions/rollback?to=` for the whole irreversibility-preview screen. A
 * rollback's preview must never be skippable, so the rail never applies
 * anything; it only carries a revision to the screen that does.
 *
 * Collapsing the offers is also what makes the rail readable. Two standing
 * buttons per row is twenty-four controls on a twelve-revision environment, and
 * the Console's bargain is that a screen is quiet until something asks for
 * attention.
 *
 * # The rail's head
 *
 * Above the newest revision is one more stop, and it is the only one that is not
 * a revision: it is where the next one arrives. Opening it offers the promotion,
 * which is the action that prepares what lands there — it writes this
 * environment's image pins from a sibling environment's and publishes nothing,
 * so the note says a deploy is what publishes. The head is drawn only when the
 * project declares another environment to promote from, the same condition the
 * environment's action bar uses, and it carries that bar's label unchanged
 * because it is the same action and a reader who has seen one should recognise
 * the other.
 *
 * Promotion is still not a *row's* action. A row's revision is this
 * environment's; a promotion reads the source environment's latest, which no row
 * here knows. The head is the only place on the rail where offering it promises
 * nothing the RPC does not do.
 *
 * # What the rows can say, and what they refuse to
 *
 * `DeployService.History` returns nine fields per revision — revision, spec
 * hash, committed-at, message, author, digest, images, outcome and
 * beyond-window — and the screen shows what it can of those and derives nothing
 * beyond them (see ./history.ts, which holds the derivations and the reasons
 * each one is conservative). `message` is not rendered at all — the server keeps
 * filling it for one release for clients built against the older schema, and
 * showing prose beside the same facts in fields would be saying everything
 * twice.
 *
 * Three limits shape the rows and are stated rather than papered over:
 *
 *  1. **A recorded outcome is a snapshot, not a live answer.** `outcome` is what
 *     the controller recorded when that revision stopped being the current one,
 *     and nothing refreshes it afterwards. So it is shown on every row as a
 *     record — labelled "recorded", in the muted meta line — while the phase
 *     pill, the same vocabulary as everywhere else, appears on exactly one stop:
 *     the marker. A live-looking pill down the whole rail would be an invention.
 *  2. **No author yet.** The spine records who deployed nothing
 *     (`internal/api`'s `History`), human or agent, so every entry reads
 *     "unattributed" and the note says why rather than guessing. That
 *     attribution is #74's work.
 *  3. **Some stops are only a revision, and say so.** The rail runs past the
 *     bounded history the cluster keeps into the registry's tag list, which is
 *     the record (ADR-0028 decision 4, #241). A `beyondWindow` entry carries one
 *     fact — this revision exists and can still be restored — so its whole meta
 *     line is replaced by a sentence saying nothing else was recorded.
 *
 * # A tab, a panel and one link
 *
 * This is the History tab of the environment (#260) rather than a screen of its
 * own, so the environment's name, its other views and its actions are the
 * layout's above — what is left here is the record.
 *
 * The comparison a stop offers is a **panel**, not a link to a diff screen: it
 * opens under `?from=<revision>` on this same page, because comparing a revision
 * to what the spec says now is a question about a row a reader is already
 * looking at. `?compare=1` opens the same panel on its other mode, the live
 * cluster's dry-run verdict, which is what the retired `/diff` route showed when
 * it was given no revision.
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

  // The promotion's possible sources, already read by the environment layout
  // this tab sits in (#260) rather than by a second GetSpec: the head offers the
  // action under exactly the condition the action bar above uses, and a project
  // with one environment has nothing to promote from.
  const { others } = useEnvironment();

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

  // One stop is open at a time, because a rail is a thing you run your eye down
  // and the offer is what you are considering at one point on it. The key is
  // never in the URL: the comparison a stop opens is worth an address and gets
  // one, and "which stop is expanded" is not.
  const [open, setOpen] = useState<string | null>(null);
  const toggle = (key: string) =>
    setOpen((current) => (current === key ? null : key));

  const entries = history.data?.entries ?? [];
  const liveRevision = status.data?.revision ?? "";
  const livePhase = status.data?.phase ?? "";
  const liveAnswer = status.data?.answer ?? "";
  // The drift is the environment's and it belongs on the one stop that is about
  // right now. A pinned rollback is exactly the case a reader opens this screen
  // for: the marker is on an older stop and this says it was chosen.
  const drift =
    status.data === undefined ? undefined : driftFor(status.data);
  const placements = railPlacements(entries, liveRevision);
  const statusFailure =
    status.error === undefined ? undefined : toFailure(status.error);
  const statusLoading = status.loading && status.data === undefined;
  const base = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(env)}`;

  const compare = (next: URLSearchParams) => setParams(next, { replace: true });

  return (
    <>
      {/* The comparison against the live cluster is not about any one stop, so
          it is offered once, above them. It is the mode the retired diff route
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
          // Keyed by the revision so that choosing a different stop's comparison
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
            {others.length > 0 ? (
              <Head
                base={base}
                open={open === HEAD}
                onToggle={() => toggle(HEAD)}
              />
            ) : null}

            <ol className="k-history__rail">
              {entries.map((entry, i) => (
                <Stop
                  key={entry.revision}
                  entry={entry}
                  base={base}
                  placement={placements[i] ?? "unmarked"}
                  livePhase={livePhase}
                  liveAnswer={liveAnswer}
                  drift={drift}
                  // The rollback screen disables its newest entry — restoring
                  // the revision at the top of the record is not a rollback — so
                  // the offer does not carry a target that would arrive
                  // disabled, and says why the offer is short.
                  newest={i === 0}
                  open={open === stopKey(entry.revision)}
                  onToggle={() => toggle(stopKey(entry.revision))}
                />
              ))}
            </ol>

            {/* Four states and four sentences, because "still reading" and
                "the server named nothing" are different answers and only one
                of them is about this environment. */}
            <p className="k-history__note">
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
                    : "the phase pill is live; a stop’s recorded outcome is what was true when it stopped being current"}
            </p>
            <p className="k-history__note">
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

/** The open stop's key. The head is not a revision, so it has one of its own. */
const HEAD = "head";
const stopKey = (revision: string) => `rev:${revision}`;

/**
 * The one health claim on this screen, in the shared vocabulary: it appears on
 * exactly one stop — the revision Status reports as live — because that is the
 * only one anything can answer for right now.
 */
function LivePill({ phase, answer }: { phase: string; answer: string }) {
  const state = statusForDelivery(answer, phase);
  return <StatusPill status={state.tone} label={state.word} />;
}

/**
 * The top of the rail: where the next revision arrives, and the action that
 * prepares it.
 *
 * It is a stop like the others — it opens on a click and offers links, it never
 * acts — and unlike the others it has no nested value to copy, so the whole line
 * is the button and its visible text is its accessible name.
 */
function Head({
  base,
  open,
  onToggle,
}: {
  base: string;
  open: boolean;
  onToggle: () => void;
}) {
  const offerId = useId();
  return (
    <div
      className="k-history__stop k-history__stop--head"
      data-rail="head"
      data-open={open ? "true" : "false"}
    >
      <button
        type="button"
        className="k-history__pick k-history__pick--head"
        aria-expanded={open}
        aria-controls={open ? offerId : undefined}
        onClick={onToggle}
      >
        <span className="k-history__next">what comes next</span>
        <span className="k-history__caret" aria-hidden="true" />
      </button>
      {open ? (
        <div className="k-history__offer" id={offerId}>
          {/* The action bar above this tab carries the same words for the same
              destination, on purpose: it is one action, and naming it twice
              would make a reader wonder what the difference is. */}
          <Link className="k-button" to={`${base}/actions/promote`}>
            Promote into this environment
          </Link>
          <span className="k-history__offer-note">
            Pins images from another environment. A deploy publishes them.
          </span>
        </div>
      ) : null}
    </div>
  );
}

/**
 * One stop on the rail. The head line is identity — what it is, and whether the
 * marker is on it — everything under it is what the record actually said, and
 * the offer below appears only when the stop is opened.
 */
function Stop({
  entry,
  base,
  placement,
  livePhase,
  liveAnswer,
  drift,
  newest,
  open,
  onToggle,
}: {
  entry: HistoryEntry;
  base: string;
  placement: RailPlacement;
  livePhase: string;
  liveAnswer: string;
  drift: Drift | undefined;
  newest: boolean;
  open: boolean;
  onToggle: () => void;
}) {
  const offerId = useId();
  const when = formatWhen(entry.committedAt);
  const live = placement === "live";

  return (
    <li
      className="k-history__stop"
      data-rail={placement}
      data-open={open ? "true" : "false"}
    >
      {/* The whole summary is the click target — the row is the affordance, not
          a control tucked at one end of it — so the button is stretched over it
          and everything in it that is itself interactive (the revision, which is
          a copy button) is lifted back above. The caret is the visible half and
          is drawn in the head line, where the eye already is. */}
      <div className="k-history__summary">
        <button
          type="button"
          className="k-history__pick"
          aria-expanded={open}
          aria-controls={open ? offerId : undefined}
          aria-label={`Actions for revision ${entry.revision}`}
          onClick={onToggle}
        />

        <div className="k-history__head">
          {/* The revision id is already short — <generation>-<hash8>
              (ADR-0028 decision 2) — so it needs no further abbreviation. */}
          <Copyable value={entry.revision} className="k-history__rev" />
          {entry.beyondWindow ? (
            <span className="k-chip">registry only</span>
          ) : null}
          {live ? (
            <>
              <span className="k-chip k-history__marker">deployed now</span>
              {/* "Deployed now" is a claim about one stop out of a list of
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
              {/* The stop's own revision id is the mono value this qualifies,
                  one element to the left of it, so the mark says only why it
                  matters. It is the sentence for what the marker's position on
                  the rail has already shown. */}
              <DriftMark drift={drift} />
            </>
          ) : null}
          <span className="k-history__caret" aria-hidden="true" />
        </div>

        {/* A revision the cluster's bounded history has forgotten (#241). The
            registry's tag list confirms it exists and holds nothing else about
            it, so the whole meta line below — which would otherwise print
            "unattributed" beside four absent facts — is replaced by the sentence
            that says the record is thin rather than the deployment featureless.
            It is still restorable: the artifact is immutable. */}
        {entry.beyondWindow ? (
          <div className="k-history__meta">
            <span title="older than the history the cluster keeps; confirmed against the registry's tag list">
              only the registry remembers this revision — nothing was recorded
              about when it was published, what it ran, or how it ended
            </span>
          </div>
        ) : (
          <div className="k-history__meta">
            {when !== "" ? <span className="k-mono">{when}</span> : null}
            {/* The generation half of the tag above, named. A reader comparing
                this list against the cluster is comparing generations. */}
            <Detail>
              <KubeFact
                name="generation"
                value={revisionParts(entry.revision)?.generation ?? ""}
              />
            </Detail>
            {/* The recorded outcome is prefixed rather than shown as a pill, so
              it cannot be mistaken for the live one above it: it says how that
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
                artifact{" "}
                <span className="k-mono">{shortHash(entry.digest)}</span>
              </span>
            ) : null}
            <span
              className={
                entry.author === "" ? "k-history__unattributed" : undefined
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
          <div className="k-history__images k-mono">
            {entry.images.map((image) => (
              <div
                className="k-history__image"
                key={`${image.component} ${image.image}`}
              >
                {image.component !== "" ? (
                  <span className="k-history__component">
                    {image.component}
                  </span>
                ) : null}
                <span>{image.image}</span>
              </div>
            ))}
          </div>
        ) : null}
      </div>

      {/* What can be done from this stop. Both are links even though one of them
          opens a panel on this same page: the comparison a reader is looking at
          is worth a URL, and `?from=` is the URL the retired diff route
          redirects into. Neither applies anything — the rollback's
          irreversibility preview is a screen and stays one. */}
      {open ? (
        <div className="k-history__offer" id={offerId}>
          <Link
            className="k-button"
            to={`${base}/history?from=${encodeURIComponent(entry.revision)}`}
          >
            Diff against current
          </Link>
          {newest ? (
            <span className="k-history__offer-note">
              The newest revision is not a rollback target.
            </span>
          ) : (
            <Link
              className="k-button"
              to={`${base}/actions/rollback?to=${encodeURIComponent(entry.revision)}`}
            >
              Roll back to this
            </Link>
          )}
        </div>
      ) : null}
    </li>
  );
}
