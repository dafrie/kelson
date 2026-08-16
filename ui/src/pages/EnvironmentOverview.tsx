import { useCallback, useMemo, useState } from "react";

import { useAsync, useClients } from "../api/data";
import { useWatch } from "../api/watch";
import { toFailure } from "../api/errors";
import type { WatchResponse_Event } from "../gen/kelson/v1alpha1/events_pb";
import { Copyable } from "../components/Copyable";
import { DriftMark } from "../components/DriftMark";
import { ErrorPanel } from "../components/ErrorPanel";
import { LiveIndicator } from "../components/LiveIndicator";
import { StatusPill } from "../components/StatusPill";
import {
  driftFor,
  statusFor,
  statusForDelivery,
  verdictTone,
  type Drift,
} from "../components/status";
import { LoadingState } from "../components/States";
import { Detail, KubeFact } from "../expert/Detail";
import { revisionParts, resourceParts } from "../expert/facts";
import { Evidence, Why } from "../expert/Why";
import { DataServices } from "../dataservices/DataServices";
import { isDataServiceVerdict } from "../dataservices/parse";
import { PhaseRail } from "../deploy/PhaseRail";
import { parseCause, type RailInput } from "../deploy/rail";
import { Ticker } from "../live/Ticker";
import { useTicker } from "../live/ticker";
import { Previews } from "../previews/Previews";
import { SecretsPanel } from "../secrets/SecretsPanel";
import { useEnvironment } from "./EnvironmentPage";
import {
  deliveryFacts,
  mergeVerdicts,
  NO_READ,
  type EnvironmentRead,
  type LiveVerdict,
  type VerdictRow,
} from "./matrix";

/**
 * The environment in view, whole: its phase rail, its workload verdicts, its
 * data services, its previews and its Secrets.
 *
 * This is the panel that used to sit under the project page's matrix, selected
 * by a strip of environment buttons. It is the Overview tab of the environment's
 * own route now (#260), which changes two things and no more: the environment
 * that is in view is the one in the URL rather than the one last clicked, so it
 * is a link a person can send; and the `Status` call is this tab's own rather
 * than the project page's column, because the project page no longer has an
 * environment on screen twice.
 *
 * The live half is one `EventService.Watch` over this environment (#76):
 * transitions move the rail and the pill, health changes replace the verdict
 * they name.
 *
 * What the deploy stream has and this does not is still NOT invented:
 * `StatusResponse` carries no adapter name, so the reconciler stage reads "not
 * reported" until a failure cause names a component.
 *
 * `StatusResponse.answer` closed the other half of that gap — the engine's own
 * verdict now reaches the pill and the rail without being re-derived here. Its
 * one documented limit is honoured rather than papered over: `stuck` is a
 * timeout verdict, a poll holds no budget that can expire, and so a poll that
 * never says stuck has not ruled it out. The rail still recovers a stuck
 * *stage* from the engine's cause reasons (`deploy/rail.ts`'s `isStuckReason`)
 * for exactly that reason.
 */
export function EnvironmentOverview() {
  const { project, environment, documents, specLoading } = useEnvironment();
  const clients = useClients();

  const status = useAsync(
    (signal) =>
      clients.deploy.status(
        {
          spec: { spec: { case: "project", value: project } },
          environment,
        },
        { signal },
      ),
    [clients, project, environment],
  );

  const [live, setLive] = useState<Live>(NO_EVENTS);
  // The ticker is a second reader of this same stream, not a second stream:
  // `record` keeps the sequence the overlay below overwrites.
  const ticker = useTicker(
    useMemo(() => ({ project, environment }), [project, environment]),
  );
  const recordTick = ticker.record;
  const onEvent = useCallback((event: WatchResponse_Event) => {
    recordTick(event);
    const payload = event.payload;
    setLive((prev) => {
      if (payload.case === "statusTransition") {
        const { phase, previousPhase, revision, cause } = payload.value;
        return { ...prev, transition: { phase, previousPhase, revision, cause } };
      }
      if (payload.case === "healthChange") {
        const v = payload.value;
        return {
          ...prev,
          verdicts: {
            ...prev.verdicts,
            [v.resource]: { code: v.code, healthy: v.healthy, message: v.message },
          },
        };
      }
      return prev;
    });
  }, [recordTick]);

  const reload = status.reload;
  const onResync = useCallback(() => {
    setLive(NO_EVENTS);
    reload();
  }, [reload]);
  // The stream opens once Status has landed: a delta applied before the read it
  // amends would be overwritten by the older answer.
  const settled = status.data !== undefined || status.error !== undefined;
  const scopes = useMemo(
    () => (settled ? [{ project, environment }] : []),
    [settled, project, environment],
  );
  const watch = useWatch({ scopes, onEvent, onResync });

  const failure =
    status.error === undefined ? undefined : toFailure(status.error);
  const loading = status.loading && status.data === undefined;

  // A transition replaces the fields it carries whole, and voids the two it
  // does not — `matrix.ts`'s deliveryFacts is where that rule is written down.
  const read = useMemo<EnvironmentRead>(() => {
    if (status.data === undefined) return { ...NO_READ, environment };
    return {
      ...deliveryFacts(status.data, live.transition),
      environment,
      namespace: status.data.namespace,
      verdicts: mergeVerdicts(status.data.verdicts, live.verdicts),
      read: true,
    };
  }, [status.data, live, environment]);

  const verdicts = read.verdicts;
  // A data component's own resource is not a workload, so it is taken out of
  // the workload list and handed to the section that knows what it is. The
  // verdicts themselves are untouched: one health source, two readers.
  const workloads = useMemo(
    () => verdicts.filter((v) => !isDataServiceVerdict(v.resource)),
    [verdicts],
  );
  const railInput = useMemo<RailInput>(
    () => ({
      phase: read.phase,
      // The engine's own verdict, so the rail's headline and the pill above it
      // cannot disagree. Empty after a live transition, and the rail derives
      // one from the phase exactly as it did before the field existed.
      answer: read.answer,
      cause: parseCause(read.cause),
      reachedPhase: live.transition?.previousPhase,
      unhealthyWorkloads: workloads.filter((v) => !v.healthy).length,
    }),
    [
      read.phase,
      read.answer,
      read.cause,
      live.transition?.previousPhase,
      workloads,
    ],
  );
  const documentText = useMemo(
    () => ({
      project: decodeDocument(documents?.project),
      environment: decodeDocument(documents?.environments[environment]),
    }),
    [documents, environment],
  );

  return (
    <section className="k-section">
      <div className="k-env">
        <div className="k-env__head">
          <div className="k-env__ident">
            {/* The environment's name is the page's heading now, so this line
                is only its state: the word, the wire's phase beside it, and
                whether the stream feeding them is connected. */}
            <div className="k-eyebrow">Status</div>
            {failure !== undefined ? (
              <StatusPill status="unknown" label="status unavailable" />
            ) : loading ? (
              <StatusPill status="unknown" label="reading…" />
            ) : (
              <EnvironmentStatus read={read} drift={driftFor(read)} />
            )}
            <LiveIndicator state={watch} />
          </div>
        </div>

        {specLoading ? <LoadingState what="the spec" /> : null}

        {failure !== undefined ? (
          <ErrorPanel
            title="Could not read this environment's status"
            error={status.error}
          />
        ) : null}

        {read.read ? (
          <>
            <PhaseRail
              input={railInput}
              project={project}
              environment={environment}
              compact
              label={`Deployment phase for ${environment}`}
            />

            <div className="k-kv">
              <span className="k-kv__key">revision</span>
              <span>
                {read.revision ? (
                  <Copyable value={read.revision} />
                ) : (
                  "none recorded"
                )}{" "}
                {/* The tag's two halves named. The generation is the
                    environment object's own, which is what an operator
                    correlates with the cluster, and it is otherwise on this
                    screen only as an unexplained prefix. */}
                <Detail>
                  <span className="k-kfacts">
                    <KubeFact
                      name="generation"
                      value={revisionParts(read.revision)?.generation ?? ""}
                    />
                    <KubeFact
                      name="spec hash"
                      value={revisionParts(read.revision)?.specHash ?? ""}
                    />
                  </span>
                </Detail>
              </span>
              {read.cause ? (
                <>
                  <span className="k-kv__key">cause</span>
                  <span className="k-kv__prose">{read.cause}</span>
                </>
              ) : null}
              {read.namespace ? (
                <>
                  <span className="k-kv__key">namespace</span>
                  <span>{read.namespace}</span>
                </>
              ) : null}
            </div>

            <div className="k-env__verdicts">
              <div className="k-eyebrow">Workloads ({workloads.length})</div>
              {workloads.length === 0 ? (
                <p className="k-env__note">
                  no verdicts — nothing here is being watched, which is not the
                  same as nothing failing
                </p>
              ) : (
                <ul className="k-verdicts">
                  {workloads.map((v) => (
                    <Verdict key={v.resource} verdict={v} />
                  ))}
                </ul>
              )}
            </div>
          </>
        ) : null}

        {/* The sequence behind the rail above: what this environment has been
            doing since the page opened, newest first. It sits outside the
            `read.read` block on purpose — a transition about an environment
            whose Status call failed is still a real phase, and the stream is
            then the only thing on the page that can say anything at all. No
            subject on the rows: the heading is already this environment. */}
        <Ticker
          entries={ticker.entries}
          state={watch}
          subject={false}
          name={`Activity in ${environment}`}
        />

        {/* Outside the status block on purpose: what a spec declares is
            readable without a cluster, and an environment whose status cannot
            be read still has databases worth describing. */}
        <DataServices
          project={project}
          environment={environment}
          projectDoc={documentText.project}
          environmentDoc={documentText.environment}
          health={
            read.read
              ? { state: "read", verdicts }
              : loading
                ? { state: "loading" }
                : { state: "unavailable" }
          }
        />

        {/* A preview is a *child* of this environment rather than a part of it
            (ADR-0017): kelson recorded no Environment document for it and its
            phase is flux-operator's, not the state machine's. So it sits below
            the environment's own state rather than among the workloads, for the
            same reason the data services do — reading it as one of this
            environment's resources is the mistake the placement prevents. */}
        <Previews project={project} environment={environment} />

        {/* Beside the data services, and outside the status block for the same
            reason: the Secrets an environment holds are readable whether or not
            its workloads are. A spec's `{secret: <name>, key: <key>}` points
            here, and #116 is what writes what it points at. */}
        <SecretsPanel project={project} environment={environment} />
      </div>
    </section>
  );
}

/** What the stream has said about this environment since its Status was read. */
interface Live {
  transition?: {
    phase: string;
    /** The phase this transition left — how the rail places a rejection. */
    previousPhase: string;
    revision: string;
    cause: string;
  };
  verdicts: Record<string, LiveVerdict>;
}

const NO_EVENTS: Live = { verdicts: {} };

/** The stored bytes as text. Absent documents are an empty document. */
function decodeDocument(bytes: Uint8Array | undefined): string {
  return bytes === undefined ? "" : new TextDecoder().decode(bytes);
}

/**
 * The environment's own pill: the shared word, with the wire's phase kept as a
 * labelled fact beside it rather than as the pill's text. The phase is what an
 * operator correlates with Flux and it stays reachable; it is not the answer to
 * "is my change live".
 *
 * The drift mark sits on the same line for the opposite reason: it *is* part of
 * that answer, and the pair "live, and older than the spec" is the one sentence
 * neither half says alone. It goes after the word and never over it.
 */
function EnvironmentStatus({
  read,
  drift,
}: {
  read: EnvironmentRead;
  drift: Drift | undefined;
}) {
  const { phase, answer } = read;
  const state = statusForDelivery(answer, phase);
  return (
    <>
      <StatusPill status={state.tone} label={state.word} />
      {/* The word is the page's loudest plain-language claim, so it is the one
          that most owes a reader its working: `statusForDelivery` reads the
          engine's answer and derives from the phase only when there is none,
          and the caret shows which of those happened here. */}
      <Why statement={state.word}>
        <Evidence
          rows={[
            { name: "answer", value: answer },
            { name: "phase", value: phase },
            { name: "revision", value: read.revision },
            { name: "cause", value: read.cause, prose: true },
          ]}
          note={
            answer === ""
              ? "No answer was reported for this environment, so the word is derived from the phase — a derivation that cannot express stuck at all."
              : "The word is the engine's own answer, translated. The phase is what the controller last wrote and is kept beside it rather than re-read."
          }
        />
      </Why>
      <DriftMark drift={drift} />
      {/* Drift is a second claim and gets a second caret: a boolean the server
          computed by comparing the revision's generation against the
          environment's current one. */}
      {drift === undefined ? null : (
        <Why statement={drift.note}>
          <Evidence
            rows={[
              { name: "stale", value: "true" },
              { name: "revision", value: read.revision },
              { name: "observed revision", value: read.observedRevision },
              {
                name: "generation",
                value: revisionParts(drift.revision)?.generation ?? "",
              },
              { name: "cause", value: read.cause, prose: true },
            ]}
            note={
              drift.pinned
                ? "The cause names a rollback pin, so this environment is serving an older revision on purpose."
                : "The revision above was published for an older generation of this environment than the stored spec now has. How far behind is not on the wire."
            }
          />
        </Why>
      )}
      {phase !== "" ? (
        <span className="k-env__phase">
          phase <span className="k-mono">{phase}</span>
        </span>
      ) : null}
    </>
  );
}

/**
 * A verdict row, shaped like the CLI's (#151): the health code as a mono chip,
 * the message, and the remediation as a "fix:" line.
 */
function Verdict({ verdict }: { verdict: VerdictRow }) {
  const parts = resourceParts(verdict.resource);
  return (
    <li className="k-verdict">
      <div className="k-verdict__head">
        <span className="k-mono k-verdict__resource">{verdict.resource}</span>
        {/* The resource string is already whole above; this names its three
            parts, which is the difference between a reader recognising a
            Deployment and a reader parsing one. Nothing is drawn for a
            resource that is not `Kind/namespace/name`. */}
        {parts === undefined ? null : (
          <Detail>
            <span className="k-kfacts">
              <KubeFact name="kind" value={parts.kind} />
              <KubeFact name="namespace" value={parts.namespace} />
              <KubeFact name="name" value={parts.name} />
            </span>
          </Detail>
        )}
        {/* The label is observation's own code, rendered verbatim like every
            other structured code in this UI; only the colour is the shared
            vocabulary's, so a red line here means what a red pill means. */}
        <StatusPill
          status={verdictTone(verdict)}
          label={verdict.code || "unknown"}
        />
        {/* A stuck verdict keeps a *wait* code — `workload/progressing` — so
            the code above cannot say it and the word has to. It is the only
            case where a second badge appears on this row, and the reason is
            that the two badges are saying different kinds of thing: what was
            observed, and what it means. */}
        {verdict.stuck ? (
          <>
            <StatusPill status={statusFor("stuck").tone} label="stuck" />
            {/* The claim a reader is most likely to dispute, because the code
                beside it is a *wait* code: the three booleans are what says
                the probe gave up rather than that the rollout is slow. */}
            <Why statement="stuck">
              <Evidence
                rows={[
                  { name: "healthy", value: String(verdict.healthy) },
                  { name: "degraded", value: String(verdict.degraded) },
                  { name: "stuck", value: "true" },
                  { name: "code", value: verdict.code },
                ]}
                note="The probe made no progress on this workload before its budget expired, and it was not failing — which is why the code stays a wait code and the word has to say the rest."
              />
            </Why>
          </>
        ) : null}
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
