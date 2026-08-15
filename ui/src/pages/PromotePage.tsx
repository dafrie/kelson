import { useCallback, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { isVersionConflict } from "../api/errors";
import { useRun } from "../api/stream";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import type {
  PromotedComponent,
  PromoteResponse,
} from "../gen/kelson/v1alpha1/deploy_pb";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { EmptyState, LoadingState } from "../components/States";
import { DiffView } from "../diff/DiffView";
import { decodeDiff, type Diff } from "../diff/parse";
import {
  countStatuses,
  promotionSummary,
  shortImage,
  statusName,
  type PromotionCounts,
} from "./promote";

/**
 * Promoting *into* this environment (issue #11, ADR-0016 decision 2).
 *
 * The URL's environment is the **target** — the one whose image pins are
 * written — and the picker chooses the source. That direction is the one the
 * reader arrives with: they are looking at production and want what staging
 * runs, so the screen is named from production's side and says "promote into
 * this environment" everywhere rather than making the reader work out which end
 * of a pair of names they are standing on.
 *
 * # What this screen does not do
 *
 * **It never deploys.** Promotion is a spec edit and nothing else (docs/model.md
 * §Promotion: "promoting in kelson v0 is editing one field"), so a successful
 * promotion leaves the target environment running exactly what it was running a
 * moment before, with a new pin waiting in the store. The success state says so
 * and hands over to the deploy screen; it does not deploy on the reader's
 * behalf, because a screen that wrote a pin and shipped it would have merged
 * the two acts ADR-0016 keeps apart.
 *
 * **It never plans and writes in one click.** The source picker runs
 * `dry_run=RENDER`, which computes the pins and the diff and stores nothing, and
 * the confirm re-sends the same promotion at `dry_run=NONE`. The plan is what
 * the confirm is about; there is no path from picking a source to a written
 * pin that does not pass through the table below it.
 *
 * # The version, and what a conflict means
 *
 * The plan reads the spec version in the same run that plans against it, and
 * the confirm sends that version back — the optimistic concurrency `PutSpec`
 * carries. When someone else stores the spec in between, the server answers
 * `store/version-conflict` and this screen offers a **re-plan**, not a retry.
 * That is deliberate: the pins were computed from documents that no longer
 * exist, so re-sending them would write a plan the reader never saw. There is
 * no force here either — the edit screen offers one because a person's typing
 * cannot be recomputed, and a promotion can be, exactly.
 */

/** A dry run and everything decoded from it, plus what a confirm would send. */
interface Plan {
  /** The source environment this plan read its images from. */
  from: string;
  response: PromoteResponse;
  diff: Diff | undefined;
  decodeError: string | undefined;
  /** Minted once per plan, so a retried confirm is a replay, not a second write. */
  idempotencyKey: string;
  /** The spec version this plan was computed against, sent by the confirm. */
  version: string;
}

export function PromotePage() {
  const { project = "", env = "" } = useParams();
  const clients = useClients();

  const spec = useAsync(
    (signal) => clients.spec.getSpec({ project }, { signal }),
    [clients, project],
  );
  // The stored spec's own environment list, minus this one: promoting an
  // environment to itself would pin it to what it already runs, and the server
  // refuses it outright (internal/api's checkPromoteRequest).
  const sources = (spec.data?.spec?.environments ?? []).filter(
    (name) => name !== env,
  );

  const [source, setSource] = useState("");
  const [plan, setPlan] = useState<Plan | undefined>(undefined);
  const [applied, setApplied] = useState<PromoteResponse | undefined>(undefined);
  const [conflict, setConflict] = useState(false);
  const planRun = useRun();
  const applyRun = useRun();

  /**
   * The dry run, and the spec version it is planning against, in one run.
   *
   * The version is read here rather than taken from the page's own GetSpec so
   * that a re-plan after a conflict is one action: it re-reads the documents
   * *and* re-computes the pins from them, which is the only pair that is safe
   * to confirm together.
   */
  const runPlan = useCallback(
    (from: string) => {
      setPlan(undefined);
      setApplied(undefined);
      setConflict(false);
      planRun.start(async (signal) => {
        const stored = await clients.spec.getSpec({ project }, { signal });
        const response = await clients.deploy.promote(
          {
            project,
            fromEnvironment: from,
            toEnvironment: env,
            dryRun: DryRun.RENDER,
          },
          { signal },
        );
        let diff: Diff | undefined;
        let decodeError: string | undefined;
        if (response.diffJson.length > 0) {
          try {
            diff = decodeDiff(response.diffJson);
          } catch (err) {
            decodeError = err instanceof Error ? err.message : String(err);
          }
        }
        setPlan({
          from,
          response,
          diff,
          decodeError,
          idempotencyKey: crypto.randomUUID(),
          version: stored.spec?.version ?? "",
        });
      });
    },
    [planRun, clients, project, env],
  );

  const select = useCallback(
    (from: string) => {
      setSource(from);
      runPlan(from);
    },
    [runPlan],
  );

  const confirm = useCallback(() => {
    if (plan === undefined) return;
    setConflict(false);
    applyRun.start(async (signal) => {
      try {
        const response = await clients.deploy.promote(
          {
            project,
            fromEnvironment: plan.from,
            toEnvironment: env,
            dryRun: DryRun.NONE,
            version: plan.version,
            idempotencyKey: plan.idempotencyKey,
          },
          { signal },
        );
        setApplied(response);
      } catch (err) {
        if (isVersionConflict(err)) {
          setConflict(true);
          return;
        }
        throw err;
      }
    });
  }, [plan, applyRun, clients, project, env]);

  const base = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(env)}`;
  const counts = countStatuses(plan?.response.components ?? []);
  const refused = (plan?.response.errors.length ?? 0) > 0;

  if (applied !== undefined && applied.errors.length === 0) {
    return <Pinned response={applied} project={project} env={env} base={base} />;
  }

  return (
    <>
      <div className="k-page-head">
        <h1>Promote into {env}</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
        <span>·</span>
        <span>the target: where the pins are written</span>
      </div>

      <p className="k-note">
        Pins {env} to the images the source environment is actually running.
        Nothing is rebuilt and <strong>nothing is deployed</strong> — the deploy
        screen ships them.
      </p>

      <section className="k-section">
        <div className="k-eyebrow">Promote from which environment</div>
        <div className="k-section__body">
          {spec.loading && spec.data === undefined ? (
            <LoadingState what="this project's environments" />
          ) : null}
          {spec.error !== undefined ? (
            <ErrorPanel
              title={`Cannot read the spec for ${project}`}
              error={spec.error}
            />
          ) : null}
          {spec.data !== undefined && sources.length === 0 ? (
            <EmptyState title="There is no environment to promote from">
              This project declares no environment other than {env}. Add one on
              the edit screen.
            </EmptyState>
          ) : null}
          {sources.length > 0 ? (
            <ul className="k-revisions">
              {sources.map((name) => (
                <Source
                  key={name}
                  name={name}
                  checked={name === source}
                  onSelect={select}
                />
              ))}
            </ul>
          ) : null}
        </div>
      </section>

      {planRun.running ? <LoadingState what="the promotion plan" /> : null}
      {planRun.error !== undefined ? (
        <ErrorPanel title="The promotion plan failed" error={planRun.error} />
      ) : null}

      {plan !== undefined ? (
        <>
          {refused ? (
            <ErrorPanel
              title="This promotion was refused — nothing would be written"
              errors={plan.response.errors}
            />
          ) : null}

          <PlanTable plan={plan} env={env} counts={counts} />
          <PlanDiff plan={plan} />

          {conflict ? (
            <ConflictState
              project={project}
              running={planRun.running}
              onReplan={() => runPlan(plan.from)}
            />
          ) : null}

          {applied !== undefined && applied.errors.length > 0 ? (
            <ErrorPanel
              title="The promotion was refused — nothing was written"
              errors={applied.errors}
            />
          ) : null}

          {applyRun.error !== undefined ? (
            <ErrorPanel title="The promotion failed" error={applyRun.error} />
          ) : null}

          {refused ? null : counts.pinned === 0 ? (
            <div className="k-panel k-panel--dim k-mono k-promote__nothing">
              nothing to pin: no component would change. {env} is already
              pinned to what {plan.from} deployed, or every component was
              skipped for the reason above — either way there is nothing to
              write, so no confirmation is offered.
            </div>
          ) : (
            <section className="k-section">
              <div className="k-eyebrow">Write the pins</div>
              <div className="k-section__body k-deploy__confirm">
                <button
                  type="button"
                  className="k-button k-button--primary k-button--wide"
                  onClick={confirm}
                  disabled={applyRun.running || conflict}
                >
                  {applyRun.running ? "Pinning…" : "Pin these images"}
                </button>
                <span className="k-mono k-deploy__note">
                  nothing has been written yet · writes against version{" "}
                  {plan.version || "—"} · deploys nothing
                </span>
              </div>
            </section>
          )}
        </>
      ) : null}
    </>
  );
}

/**
 * One candidate source. Unlike the rollback picker there is no disabled entry:
 * every environment other than the target is a legitimate source, and which one
 * has something deployed is the server's answer (`promote/nothing-deployed`),
 * not a guess this list is in a position to make.
 */
function Source({
  name,
  checked,
  onSelect,
}: {
  name: string;
  checked: boolean;
  onSelect: (name: string) => void;
}) {
  return (
    <li className="k-revision">
      <label className="k-revision__label">
        <input
          type="radio"
          name="promote-source"
          value={name}
          checked={checked}
          onChange={() => onSelect(name)}
        />
        <span className="k-mono k-revision__rev">{name}</span>
        <span className="k-mono k-revision__meta">
          promotes the images this environment's latest revision runs
        </span>
      </label>
    </li>
  );
}

/**
 * The plan: every component the promotion considered, including the ones it
 * would do nothing to.
 *
 * A component missing from this table would be indistinguishable from a
 * component kelson forgot, which is why the server returns all of them
 * (internal/api's wirePromoted) and why none are filtered out here.
 */
function PlanTable({
  plan,
  env,
  counts,
}: {
  plan: Plan;
  env: string;
  counts: PromotionCounts;
}) {
  const components = plan.response.components;
  return (
    <section className="k-section">
      <div className="k-eyebrow">What would be pinned ({components.length})</div>
      <div className="k-section__body">
        <div className="k-mono k-promote__summary">
          <span>{promotionSummary(counts)}</span>
          <span>·</span>
          <span>
            read from {plan.from} revision{" "}
            {plan.response.fromRevision ? (
              <Copyable value={plan.response.fromRevision} />
            ) : (
              "—"
            )}
          </span>
        </div>

        {components.length === 0 ? (
          <div className="k-panel k-panel--dim k-mono">
            the promotion considered no components — this project declares no
            workload component that could carry an image pin
          </div>
        ) : (
          <div className="k-promote__scroll">
            <table className="k-promote__table">
              <thead>
                <tr>
                  <th scope="col">Component</th>
                  <th scope="col">{env} runs</th>
                  <th scope="col">Would pin to</th>
                  <th scope="col">Status</th>
                </tr>
              </thead>
              <tbody>
                {components.map((c) => (
                  <Row key={c.component} component={c} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  );
}

function Row({ component }: { component: PromotedComponent }) {
  const status = statusName(component.status);
  return (
    <>
      <tr className="k-promote__row" data-status={status}>
        <th scope="row" className="k-mono k-promote__component">
          {component.component}
        </th>
        <td>
          <ImageCell
            image={component.fromImage}
            absent="unpinned"
            absentWhy="no pin today: this environment follows the component's or the project's image"
          />
        </td>
        <td>
          <ImageCell
            image={component.toImage}
            absent="—"
            absentWhy="the promotion read no image for this component, so there is nothing to pin"
          />
        </td>
        <td>
          <span className={`k-chip k-mono k-promote__status--${status}`}>
            {status}
          </span>
        </td>
      </tr>
      {component.reason !== "" || component.code !== "" ? (
        <tr className="k-promote__why">
          <td colSpan={4}>
            {component.code !== "" ? (
              <code className="k-promote__code">{component.code}</code>
            ) : null}
            <span>{component.reason}</span>
          </td>
        </tr>
      ) : null}
    </>
  );
}

/**
 * One image reference, middle-truncated.
 *
 * The whole value is on the title and on the clipboard — a digest is what a
 * person retypes into a terminal, and retyping a digest is how the wrong image
 * gets pinned — so the ellipsis hides nothing a hover or a click cannot
 * recover.
 */
function ImageCell({
  image,
  absent,
  absentWhy,
}: {
  image: string;
  absent: string;
  absentWhy: string;
}) {
  if (image === "") {
    return (
      <span className="k-mono k-promote__absent" title={absentWhy}>
        {absent}
      </span>
    );
  }
  return (
    <Copyable
      value={image}
      label={shortImage(image)}
      title={image}
      className="k-promote__image"
    />
  );
}

/**
 * The rendered half of the plan, through the same decode and the same view
 * every other preview in this UI uses. A payload that will not decode is
 * reported rather than dropped: a blank space where a diff should be reads as
 * "nothing changed", which is the one thing it must never be mistaken for.
 */
function PlanDiff({ plan }: { plan: Plan }) {
  if (plan.decodeError === undefined && plan.diff === undefined) return null;
  return (
    <section className="k-section">
      <div className="k-eyebrow">What the pins change</div>
      <div className="k-section__body">
        <p className="k-note">
          The target environment rendered before and after the pins.
        </p>
        {plan.decodeError !== undefined ? (
          <ErrorPanel
            title="The promotion's diff payload could not be decoded"
            error={new Error(plan.decodeError)}
          />
        ) : null}
        {plan.diff !== undefined ? (
          <DiffView
            diff={plan.diff}
            exitSemantics={plan.response.exitSemantics}
          />
        ) : null}
      </div>
    </section>
  );
}

/**
 * Someone else stored this spec between the plan and the confirm.
 *
 * The offer is to plan again, and only that. The pins in the table were read
 * out of documents that no longer exist, so re-sending them would write a
 * promotion the reader never saw — and unlike a half-typed edit, a promotion
 * can be recomputed exactly, which is why no force is offered here at all.
 */
function ConflictState({
  project,
  running,
  onReplan,
}: {
  project: string;
  running: boolean;
  onReplan: () => void;
}) {
  return (
    // The same box the editor's conflict uses: it is the same event, and the
    // reader who has seen one should recognise the other.
    <div className="k-edit__conflict" role="alert">
      <div className="k-settled__head">
        <StatusPill status="degraded" label="store/version-conflict" />
        <span className="k-settled__title">
          The spec changed while this plan was on screen
        </span>
      </div>
      <p className="k-edit__conflict-body">
        Something else stored a new version of {project} after this plan was
        computed, so the write was refused. Nothing has been written.
      </p>
      <div className="k-actions k-edit__conflict-actions">
        <button
          type="button"
          className="k-button k-button--primary"
          onClick={onReplan}
          disabled={running}
        >
          {running ? "Planning…" : "Plan again from the stored spec"}
        </button>
      </div>
      <span className="k-mono k-deploy__note">
        the plan above was computed from documents that are no longer stored, so
        it is not offered for a retry
      </span>
    </div>
  );
}

/**
 * What was written, and the one thing that has not happened.
 *
 * The pins are in the store and the cluster has not been touched, so the
 * primary action is the deploy screen and the sentence above it says why there
 * is one.
 */
function Pinned({
  response,
  project,
  env,
  base,
}: {
  response: PromoteResponse;
  project: string;
  env: string;
  base: string;
}) {
  const written = response.components.filter(
    (c) => statusName(c.status) === "pinned",
  );
  return (
    <>
      <div className="k-page-head">
        <h1>Promote into {env}</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
      </div>

      <div className="k-settled" role="status">
        <div className="k-settled__head">
          <StatusPill status="synced" label="pinned" />
          <span className="k-settled__title">
            {written.length} image pin{written.length === 1 ? "" : "s"} written
            to {project}
          </span>
        </div>
        <span className="k-mono">
          spec version {response.version || "—"} · read from revision{" "}
          {response.fromRevision ? (
            <Copyable value={response.fromRevision} />
          ) : (
            "—"
          )}
        </span>
        <span className="k-mono">
          nothing has been deployed — {env} is still running what it was
          running before
        </span>

        <ul className="k-promote__written">
          {written.map((c) => (
            <li key={c.component}>
              <span className="k-mono k-promote__component">
                {c.component}
              </span>
              <Copyable
                value={c.toImage}
                label={shortImage(c.toImage)}
                title={c.toImage}
                className="k-promote__image"
              />
            </li>
          ))}
        </ul>

        <div className="k-actions">
          <Link className="k-button k-button--primary" to={`${base}/deploy`}>
            Next: deploy {env}
          </Link>
          <Link className="k-button" to={`${base}/diff`}>
            Diff first
          </Link>
          <Link className="k-button" to={`/projects/${encodeURIComponent(project)}`}>
            Back to {project}
          </Link>
        </div>
      </div>
    </>
  );
}
