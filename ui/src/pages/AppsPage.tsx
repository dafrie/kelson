import { useEffect, useState } from "react";
import { ConnectError } from "@connectrpc/connect";

import { clients } from "../api/clients";
import type { Spec } from "../gen/kelson/v1alpha1/spec_pb";
import { StatusPill, STATUS_KINDS } from "../components/StatusPill";
import { EmptyState, LoadingState, ServerUnreachableState } from "../components/States";

/**
 * Stage-1 placeholder for the apps screen.
 *
 * It lists what the spec store actually holds and nothing else. The dashboard
 * mockup's cards carry per-app status, a commit sha, a deploy age and a request
 * sparkline; none of that is in ListSpecsResponse, which returns
 * project/version/environments only. Inventing it would make a screen that
 * looks finished and reports fiction, so the card renders the three fields the
 * API has and stage 2 adds the rest when it wires DeployService.Status.
 */
export function AppsPage() {
  const [state, setState] = useState<
    | { kind: "loading" }
    | { kind: "ready"; specs: Spec[] }
    | { kind: "error"; message: string }
  >({ kind: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    clients.spec
      .listSpecs({}, { signal: controller.signal })
      .then((res) => setState({ kind: "ready", specs: res.specs }))
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({ kind: "error", message: describe(err) });
      });
    return () => controller.abort();
  }, []);

  return (
    <>
      <div className="k-page-head">
        <h1>Apps</h1>
      </div>
      <div className="k-page-sub">
        <span>
          {state.kind === "ready"
            ? `${state.specs.length} ${state.specs.length === 1 ? "project" : "projects"}`
            : "—"}
        </span>
      </div>

      {state.kind === "loading" ? <LoadingState what="projects" /> : null}

      {state.kind === "error" ? (
        <ServerUnreachableState detail={state.message} />
      ) : null}

      {state.kind === "ready" && state.specs.length === 0 ? (
        <EmptyState title="No projects stored yet — kelson-server's spec store is empty">
          Put one with `kelson` or SpecService.PutSpec, and it appears here.
        </EmptyState>
      ) : null}

      {state.kind === "ready" && state.specs.length > 0 ? (
        <div className="k-grid">
          {state.specs.map((spec) => (
            <SpecCard key={spec.project} spec={spec} />
          ))}
        </div>
      ) : null}

      <StatusPillLegend />
    </>
  );
}

function SpecCard({ spec }: { spec: Spec }) {
  return (
    <div className="k-panel k-panel--interactive k-card">
      <div className="k-card__head">
        <span className="k-card__name">{spec.project}</span>
        {/* The spec store knows nothing about rollout state; claiming a status
            here would be the fiction this screen refuses to print. */}
        <StatusPill status="unknown" label="no status yet" />
      </div>
      <div className="k-mono k-card__meta">
        <span>
          {spec.environments.length > 0
            ? spec.environments.join(" · ")
            : "no environments declared"}
        </span>
        <span>version {spec.version || "—"}</span>
      </div>
    </div>
  );
}

/**
 * A demo of the status pill in every state, so the primitive is visible and
 * reviewable before stage 2 has real statuses to put in it. It goes when the
 * first screen renders real ones.
 */
function StatusPillLegend() {
  return (
    <section className="k-legend">
      <div className="k-eyebrow">Status vocabulary</div>
      <div className="k-legend__row">
        {STATUS_KINDS.map((kind) => (
          <StatusPill key={kind} status={kind} />
        ))}
      </div>
    </section>
  );
}

function describe(err: unknown): string {
  if (err instanceof ConnectError) {
    return `${err.code}: ${err.rawMessage}`;
  }
  return err instanceof Error ? err.message : String(err);
}
