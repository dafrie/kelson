import { Link } from "react-router-dom";

import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import { ErrorPanel } from "../components/ErrorPanel";
import { buildRail, type Diagnosis, type RailInput, type RailStage } from "./rail";
import "./PhaseRail.css";

/**
 * The five phases as a rail, with the responsible component named under each
 * one and — when something is wrong — exactly one diagnosis and one next step.
 *
 * The naming is half the point (issue #68). "Reconciling" without an actor is
 * an invitation to stare at the wrong screen: whether the thing that has to act
 * next is kelson, Flux or the cluster decides where you look. So every stage
 * says who owns it, and the reconciler stage says "not reported" rather than a
 * plausible name whenever the server did not send the mode — see
 * rail.ts:reconcilerActor for what field would fix that.
 *
 * `compact` is the same rail on one line, for a screen where the deployment is
 * one of several things being shown (ProjectDetailPage). It drops nothing that
 * carries a verdict: the diagnosis and its next step render in both forms,
 * because a state that needs an action needs it at every size.
 */
export function PhaseRail({
  input,
  project,
  environment,
  compact = false,
  errors,
  label = "Deployment phase",
}: {
  input: RailInput;
  project: string;
  environment: string;
  compact?: boolean | undefined;
  /** The structured error a Settled event carried, rendered by ErrorPanel. */
  errors?: readonly WireError[] | undefined;
  label?: string | undefined;
}) {
  const rail = buildRail(input);
  const { diagnosis } = rail;

  return (
    <div className={compact ? "k-rail k-rail--compact" : "k-rail"}>
      <ol className="k-rail__stages" aria-label={label}>
        {rail.stages.map((stage) => (
          <Stage key={stage.phase} stage={stage} compact={compact} />
        ))}
      </ol>

      {diagnosis === undefined ? (
        <p className="k-rail__headline">{rail.headline}</p>
      ) : (
        <DiagnosisPanel
          diagnosis={diagnosis}
          project={project}
          environment={environment}
        />
      )}

      {/* The rejection's structured error, in the one panel this UI renders
          remote failures with — code, message, remediation, docs link. It sits
          under the diagnosis rather than replacing it: the panel says what the
          server said, the diagnosis says which of the three failures this is. */}
      {errors !== undefined && errors.length > 0 ? (
        <ErrorPanel title="What the server reported" errors={errors} />
      ) : null}
    </div>
  );
}

function Stage({ stage, compact }: { stage: RailStage; compact: boolean }) {
  return (
    <li
      className={`k-rail__stage k-rail__stage--${stage.state}`}
      data-state={stage.state}
      data-phase={stage.phase}
      aria-current={stage.state === "current" ? "step" : undefined}
    >
      <span className="k-rail__marker" aria-hidden="true" />
      <span className="k-rail__phase">{stage.phase}</span>
      {/* The actor is the reason this is a rail and not a progress bar. It is
          kept in the compact form too, as the smaller line under the name. */}
      <span
        className={
          stage.actorKnown
            ? "k-rail__actor k-mono"
            : "k-rail__actor k-rail__actor--unknown k-mono"
        }
        title={
          stage.actorKnown
            ? undefined
            : "the server did not report which delivery mode ran"
        }
      >
        {stage.actorKnown ? stage.actor : `${stage.actor} · not reported`}
      </span>
      <span className="k-rail__state k-mono">{compact ? "" : stage.state}</span>
    </li>
  );
}

/**
 * One diagnosis, three possible shapes, never a bare red badge.
 *
 * The kind is on the element as a data attribute as well as in the copy: the
 * three failures are what an operator (and a test) branches on, and they must
 * be distinguishable without reading prose.
 */
function DiagnosisPanel({
  diagnosis,
  project,
  environment,
}: {
  diagnosis: Diagnosis;
  project: string;
  environment: string;
}) {
  return (
    <div
      className={`k-rail__diagnosis k-rail__diagnosis--${diagnosis.kind}`}
      data-diagnosis={diagnosis.kind}
      role="status"
    >
      <div className="k-rail__diagnosis-head">
        <span className="k-rail__diagnosis-title">{diagnosis.title}</span>
        <span className="k-rail__diagnosis-where k-mono">
          at {diagnosis.stage}
          {diagnosis.component ? ` · ${diagnosis.component}` : ""}
        </span>
      </div>
      {diagnosis.detail ? (
        <p className="k-rail__diagnosis-detail">{diagnosis.detail}</p>
      ) : null}
      <p className="k-rail__next">
        <span className="k-rail__next-label">next:</span> {diagnosis.nextStep}
      </p>
      <ActionLink
        diagnosis={diagnosis}
        project={project}
        environment={environment}
      />
    </div>
  );
}

function ActionLink({
  diagnosis,
  project,
  environment,
}: {
  diagnosis: Diagnosis;
  project: string;
  environment: string;
}) {
  const app = `/projects/${encodeURIComponent(project)}`;
  const to =
    diagnosis.action.kind === "logs"
      ? `${app}/${encodeURIComponent(environment)}/logs`
      : `${app}/edit`;
  return (
    <Link className="k-button k-rail__action" to={to}>
      {diagnosis.action.label}
    </Link>
  );
}
