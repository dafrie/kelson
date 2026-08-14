import { useEffect, useState } from "react";
import { ConnectError } from "@connectrpc/connect";

import { clients, fetchHealth, type Health } from "../api/clients";
import type { ProfileGap } from "../gen/kelson/v1alpha1/profile_pb";
import { ComponentsChecklist } from "../components/ComponentsChecklist";
import { NodesSection } from "../components/NodesSection";
import {
  EmptyState,
  ErrorState,
  LoadingState,
  ServerUnreachableState,
} from "../components/States";

/**
 * Stage-1 placeholder for the cluster screen: the server's build from
 * /healthz, and the live ClusterProfile from ProfileService.
 *
 * The profile travels as its canonical YAML document, not as proto fields
 * (proto/kelson/v1alpha1/profile.proto is explicit that the YAML tags on
 * internal/clusterprofile are the schema of record). So it is printed as the
 * document it is. Gaps are pulled out and shown separately because
 * "could not tell" is a third state the UI must never collapse into "absent" —
 * stage 2 gives them a proper treatment.
 */

interface Loaded {
  health: Health;
  profileYaml: string;
  gaps: ProfileGap[];
}

export function ClusterPage() {
  const [state, setState] = useState<
    | { kind: "loading" }
    | { kind: "ready"; data: Loaded }
    | { kind: "unreachable"; message: string }
    | { kind: "error"; message: string }
  >({ kind: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    void (async () => {
      let health: Health;
      try {
        health = await fetchHealth(controller.signal);
      } catch (err: unknown) {
        if (controller.signal.aborted) return;
        setState({ kind: "unreachable", message: describe(err) });
        return;
      }
      try {
        const res = await clients.profile.getProfile(
          {},
          { signal: controller.signal },
        );
        if (controller.signal.aborted) return;
        setState({
          kind: "ready",
          data: {
            health,
            profileYaml: new TextDecoder().decode(res.yaml),
            gaps: res.gaps,
          },
        });
      } catch (err: unknown) {
        if (controller.signal.aborted) return;
        // The server is up — this is a detection failure, not a connectivity
        // one, and the two deserve different words.
        setState({ kind: "error", message: describe(err) });
      }
    })();
    return () => controller.abort();
  }, []);

  return (
    <>
      <div className="k-page-head">
        <h1>Cluster</h1>
      </div>
      <div className="k-page-sub">
        <span>server build, nodes, platform components and detected capabilities</span>
      </div>

      {state.kind === "loading" ? <LoadingState what="cluster profile" /> : null}

      {state.kind === "unreachable" ? (
        <ServerUnreachableState detail={state.message} />
      ) : null}

      {state.kind === "error" ? (
        <ErrorState
          title="Cluster profile detection failed"
          detail={state.message}
        />
      ) : null}

      {state.kind === "ready" ? <Profile data={state.data} /> : null}

      {/* Nodes and components read through their own clients: a failed
          profile capture must not hide the machine inventory, and vice
          versa. Each section degrades alone. */}
      {state.kind === "ready" ? (
        <>
          <section className="k-section">
            <div className="k-eyebrow">Nodes</div>
            <div className="k-section__body">
              <NodesSection />
            </div>
          </section>
          <section className="k-section">
            <div className="k-eyebrow">Platform components</div>
            <div className="k-section__body">
              <ComponentsChecklist />
            </div>
          </section>
        </>
      ) : null}
    </>
  );
}

function Profile({ data }: { data: Loaded }) {
  return (
    <>
      <section className="k-section">
        <div className="k-eyebrow">Server</div>
        <div className="k-section__body k-panel">
          <div className="k-kv">
            <span className="k-kv__key">status</span>
            <span>{data.health.status}</span>
            <span className="k-kv__key">version</span>
            <span>{data.health.version}</span>
            <span className="k-kv__key">commit</span>
            <span>{data.health.commit}</span>
          </div>
        </div>
      </section>

      <section className="k-section">
        <div className="k-eyebrow">Gaps ({data.gaps.length})</div>
        <div className="k-section__body">
          {data.gaps.length === 0 ? (
            <EmptyState title="No gaps — detection read everything it looked for" />
          ) : (
            <div className="k-panel">
              <ul className="k-gaps">
                {data.gaps.map((gap) => (
                  <li key={`${gap.field}:${gap.reason}`}>
                    <div className="k-gaps__field">{gap.field}</div>
                    <div className="k-gaps__reason">{gap.reason}</div>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      </section>

      <section className="k-section">
        <div className="k-eyebrow">ClusterProfile</div>
        <div className="k-section__body k-panel k-panel--dim">
          <pre className="k-pre">
            <code>{data.profileYaml.trimEnd()}</code>
          </pre>
        </div>
      </section>
    </>
  );
}

function describe(err: unknown): string {
  if (err instanceof ConnectError) {
    return `${err.code}: ${err.rawMessage}`;
  }
  return err instanceof Error ? err.message : String(err);
}
