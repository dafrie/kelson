import { useCallback, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { useRun } from "../api/stream";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import type { Manifest } from "../gen/kelson/v1alpha1/common_pb";
import type {
  DeployResponse_Committed,
  DeployResponse_Proposed,
  DeployResponse_Settled,
  DeployResponse_Transition,
} from "../gen/kelson/v1alpha1/deploy_pb";
import type { DiffResponse } from "../gen/kelson/v1alpha1/render_pb";
import { Copyable } from "../components/Copyable";
import { Disclosure, YamlBlock } from "../components/Disclosure";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { answerToStatus, formatInstant, phaseToStatus } from "../components/phase";
import { LoadingState } from "../components/States";
import { DiffView } from "../diff/DiffView";
import { decodeDiff, type Diff } from "../diff/parse";

/**
 * Deploy, in two steps, because the API is in two steps.
 *
 * Step one is DeployService.Deploy with dry_run=RENDER: the offline rung, where
 * the manifests ARE the answer and ride the Proposed event (internal/api's
 * Deploy is explicit that no adapter is ever built). Nothing is touched, so the
 * preview works with no cluster at all.
 *
 * Step two is the same RPC with dry_run=NONE, and its event order is the
 * deployment's own story: Proposed, Committed, one Transition per phase change,
 * exactly one Settled last. The stream is consumed with `for await` and each
 * event is rendered as it lands — the point of a stream is that you watch it.
 *
 * A deployment that settles unhealthy completes the stream *cleanly*, with the
 * error on the Settled event (statemachine.Run's contract: "not healthy" is an
 * answer, not a transport failure). So a settled error is rendered as the
 * result of the deploy, in the same panel as a successful one, and not as a
 * broken connection.
 */

interface Live {
  proposed: DeployResponse_Proposed | undefined;
  committed: DeployResponse_Committed | undefined;
  transitions: DeployResponse_Transition[];
  settled: DeployResponse_Settled | undefined;
}

const EMPTY: Live = {
  proposed: undefined,
  committed: undefined,
  transitions: [],
  settled: undefined,
};

export function DeployPage() {
  const { project = "", env = "" } = useParams();
  const clients = useClients();
  const preview = useAsync(async (signal) => {
    let proposed: DeployResponse_Proposed | undefined;
    for await (const res of clients.deploy.deploy(
      {
        spec: { spec: { case: "project", value: project } },
        environment: env,
        dryRun: DryRun.RENDER,
      },
      { signal },
    )) {
      if (res.event.case === "proposed") proposed = res.event.value;
    }
    return proposed;
  }, [clients, project, env]);

  const [live, setLive] = useState<Live>(EMPTY);
  const apply = useRun();

  const deploy = useCallback(() => {
    setLive(EMPTY);
    apply.start(async (signal) => {
      for await (const res of clients.deploy.deploy(
        {
          spec: { spec: { case: "project", value: project } },
          environment: env,
          dryRun: DryRun.NONE,
        },
        { signal },
      )) {
        const event = res.event;
        setLive((prev) => {
          switch (event.case) {
            case "proposed":
              return { ...prev, proposed: event.value };
            case "committed":
              return { ...prev, committed: event.value };
            case "transition":
              return { ...prev, transitions: [...prev.transitions, event.value] };
            case "settled":
              return { ...prev, settled: event.value };
            default:
              return prev;
          }
        });
      }
    });
  }, [apply, clients, project, env]);

  const proposed = preview.data;
  const started = apply.running || live.proposed !== undefined || live.settled !== undefined;

  return (
    <>
      <div className="k-page-head">
        <h1>Deploy</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/apps/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
      </div>

      <section className="k-section">
        <div className="k-eyebrow">Step 1 · Preview (dry run: render)</div>
        <div className="k-section__body">
          {preview.loading && preview.data === undefined ? (
            <LoadingState what="the rendered manifests" />
          ) : null}
          {preview.error !== undefined ? (
            <ErrorPanel title="Rendering this environment failed" error={preview.error} />
          ) : null}
          {proposed !== undefined ? (
            <Preview proposed={proposed} project={project} environment={env} />
          ) : null}
        </div>
      </section>

      {proposed !== undefined ? (
        <ServerDiff project={project} environment={env} />
      ) : null}

      {proposed !== undefined ? (
        <section className="k-section">
          <div className="k-eyebrow">Step 2 · Deploy</div>
          <div className="k-section__body k-deploy__confirm">
            <button
              type="button"
              className="k-button k-button--primary k-button--wide"
              onClick={deploy}
              disabled={apply.running}
            >
              {apply.running
                ? "Deploying…"
                : `Apply ${proposed.resources} ${proposed.resources === 1 ? "resource" : "resources"} to ${project}/${env}`}
            </button>
            {apply.running ? (
              <button type="button" className="k-button" onClick={apply.stop}>
                Stop watching
              </button>
            ) : null}
            <span className="k-mono k-deploy__note">
              mode {proposed.mode || "—"} · nothing is written until this is
              pressed
            </span>
          </div>
        </section>
      ) : null}

      {started ? <Stream live={live} error={apply.error} /> : null}
    </>
  );
}

function Preview({
  proposed,
  project,
  environment,
}: {
  proposed: DeployResponse_Proposed;
  project: string;
  environment: string;
}) {
  return (
    <div className="k-panel">
      <div className="k-kv">
        <span className="k-kv__key">project</span>
        <span>{proposed.project || project}</span>
        <span className="k-kv__key">environment</span>
        <span>{proposed.environment || environment}</span>
        <span className="k-kv__key">resources</span>
        <span>{proposed.resources}</span>
        <span className="k-kv__key">mode</span>
        <span>{proposed.mode || "—"}</span>
      </div>

      <div className="k-manifests">
        {proposed.manifests.map((m) => (
          <ManifestRow key={`${m.kind}/${m.namespace}/${m.name}`} manifest={m} />
        ))}
      </div>
    </div>
  );
}

function ManifestRow({ manifest }: { manifest: Manifest }) {
  return (
    <Disclosure
      summary={
        <span className="k-mono k-manifest__name">
          {manifest.kind}/{manifest.name}
        </span>
      }
      meta={[manifest.namespace, manifest.apiVersion]
        .filter((s) => s !== "")
        .join(" · ")}
    >
      <YamlBlock bytes={manifest.yaml} />
    </Disclosure>
  );
}

/**
 * The diff offer, and the honest explanation of why there is only one kind.
 *
 * RenderService.Diff at dry_run=RENDER compares against the request's `from`
 * documents; with no `from` there is no prior state and every resource is an
 * addition, which is a true answer but a useless one here. A stored spec has no
 * "from" — the previous documents are not the store's to produce — so the only
 * meaningful diff from this screen is dry_run=SERVER, against the live cluster.
 * It is not run automatically: it contacts the cluster, and step one deliberately
 * does not.
 */
function ServerDiff({
  project,
  environment,
}: {
  project: string;
  environment: string;
}) {
  const clients = useClients();
  const run = useRun();
  const [result, setResult] = useState<
    { response: DiffResponse; diff: Diff | undefined; decodeError: string | undefined } | undefined
  >(undefined);

  const load = useCallback(() => {
    setResult(undefined);
    run.start(async (signal) => {
      const response = await clients.render.diff(
        {
          spec: { spec: { case: "project", value: project } },
          environment,
          dryRun: DryRun.SERVER,
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
      setResult({ response, diff, decodeError });
    });
  }, [run, clients, project, environment]);

  return (
    <section className="k-section">
      <div className="k-eyebrow">Diff against the live cluster (optional)</div>
      <div className="k-section__body">
        <div className="k-deploy__confirm">
          <button
            type="button"
            className="k-button"
            onClick={load}
            disabled={run.running}
          >
            {run.running ? "Asking the cluster…" : "Run server dry-run diff"}
          </button>
          <span className="k-mono k-deploy__note">
            a rendered-vs-rendered diff needs a `from` spec, which a stored spec
            does not have — that is the CLI's `kelson diff --from` flow
          </span>
        </div>

        {run.error !== undefined ? (
          <ErrorPanel title="Server dry-run diff failed" error={run.error} />
        ) : null}
        {result?.response.errors.length ? (
          <ErrorPanel
            title="The spec was rejected"
            errors={result.response.errors}
          />
        ) : null}
        {result?.decodeError !== undefined ? (
          <ErrorPanel
            title="The diff payload could not be decoded"
            error={new Error(result.decodeError)}
          />
        ) : null}
        {result?.diff !== undefined ? (
          <DiffView
            diff={result.diff}
            exitSemantics={result.response.exitSemantics}
          />
        ) : null}
      </div>
    </section>
  );
}

function Stream({ live, error }: { live: Live; error: unknown }) {
  return (
    <section className="k-section">
      <div className="k-eyebrow">Deployment</div>
      <div className="k-section__body k-stream">
        {live.proposed ? (
          <div className="k-stream__row">
            <span className="k-mono k-stream__label">proposed</span>
            <span className="k-mono">
              {live.proposed.resources} resources · mode{" "}
              {live.proposed.mode || "—"}
            </span>
          </div>
        ) : null}

        {live.committed ? (
          <div className="k-stream__row">
            <span className="k-mono k-stream__label">committed</span>
            <span className="k-mono">
              revision <Copyable value={live.committed.revision} /> · adapter{" "}
              {live.committed.adapter}
            </span>
          </div>
        ) : null}

        {live.transitions.map((t, i) => (
          <TransitionRow key={`${t.phase}:${i}`} transition={t} />
        ))}

        {live.settled ? <Settled settled={live.settled} /> : null}

        {error !== undefined ? (
          <ErrorPanel title="The deploy stream failed" error={error} />
        ) : null}
      </div>
    </section>
  );
}

function TransitionRow({ transition }: { transition: DeployResponse_Transition }) {
  const at = formatInstant(transition.sinceUnixMs);
  return (
    <div className="k-stream__row">
      <StatusPill
        status={answerToStatus(transition.answer)}
        label={transition.phase.toLowerCase()}
      />
      <span className="k-mono k-stream__answer">
        {transition.answer}
        {transition.stuck ? " · stuck" : ""}
      </span>
      {transition.cause ? (
        <span className="k-mono k-stream__cause">
          {[transition.cause.component, transition.cause.reason]
            .filter(Boolean)
            .join("/")}
          {transition.cause.message ? `: ${transition.cause.message}` : ""}
        </span>
      ) : null}
      {at ? <span className="k-mono k-stream__at">{at}</span> : null}
    </div>
  );
}

function Settled({ settled }: { settled: DeployResponse_Settled }) {
  if (settled.error) {
    return (
      <ErrorPanel
        title={`Settled ${settled.final?.phase.toLowerCase() ?? "unhealthy"}`}
        errors={[settled.error]}
      />
    );
  }
  return (
    <div className="k-settled" role="status">
      <div className="k-settled__head">
        <StatusPill
          status={phaseToStatus(settled.final?.phase ?? "")}
          label={settled.final?.phase.toLowerCase() ?? "settled"}
        />
        <span className="k-settled__title">Deployment settled</span>
      </div>
      {settled.final?.observedRevision ? (
        <span className="k-mono">
          observed revision <Copyable value={settled.final.observedRevision} />
        </span>
      ) : null}
    </div>
  );
}
