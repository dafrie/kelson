import { useState } from "react";
import { ConnectError } from "@connectrpc/connect";

import { useAsync, useClients } from "../api/data";
import type {
  ComponentStatus,
  InstallResponse,
  PlanInstallResponse,
} from "../gen/kelson/v1alpha1/install_pb";
import { Disclosure } from "./Disclosure";
import { ErrorState, LoadingState } from "./States";
import { StatusPill, type StatusKind } from "./StatusPill";

/**
 * The platform-component checklist: the pins table crossed with live
 * detection, with an install flow per missing row.
 *
 * The flow is the CLI's preview-confirm-apply with the confirmation owned by
 * this screen: Install first fetches the plan (every object, the pinned
 * version, the verified digest), renders it, and applies only after a second,
 * explicit click. The server re-plans before applying either way — the click
 * is a decision, never an authorization token.
 *
 * Presence is the tri-state (present / missing / could-not-tell) and "could
 * not tell" renders as its own state: collapsing it into "missing" here would
 * put an Install button on a component detection was not allowed to see,
 * which is exactly the guess the tri-state exists to prevent.
 */

function presencePill(c: ComponentStatus) {
  const map: Record<string, { status: StatusKind; label: string }> = {
    yes: { status: "synced", label: "present" },
    no: { status: "degraded", label: "missing" },
    unknown: { status: "unknown", label: "could not tell" },
  };
  const p = map[c.presence] ?? { status: "unknown" as StatusKind, label: c.presence };
  return <StatusPill status={p.status} label={p.label} />;
}

type Flow =
  | { kind: "idle" }
  | { kind: "planning"; component: string }
  | { kind: "planned"; component: string; plan: PlanInstallResponse }
  | { kind: "installing"; component: string; plan: PlanInstallResponse }
  | { kind: "installed"; component: string; report: InstallResponse }
  | { kind: "failed"; component: string; message: string };

export function ComponentsChecklist() {
  const clients = useClients();
  const list = useAsync(
    (signal) => clients.install.listComponents({}, { signal }),
    [clients],
  );
  const [flow, setFlow] = useState<Flow>({ kind: "idle" });

  async function plan(component: string) {
    setFlow({ kind: "planning", component });
    try {
      const res = await clients.install.planInstall({ components: [component] });
      setFlow({ kind: "planned", component, plan: res });
    } catch (err: unknown) {
      setFlow({ kind: "failed", component, message: describe(err) });
    }
  }

  async function apply(component: string, planned: PlanInstallResponse) {
    setFlow({ kind: "installing", component, plan: planned });
    try {
      const res = await clients.install.install({ components: [component] });
      setFlow({ kind: "installed", component, report: res });
      list.reload();
    } catch (err: unknown) {
      setFlow({ kind: "failed", component, message: describe(err) });
    }
  }

  if (list.loading && !list.data) {
    return <LoadingState what="platform components" />;
  }
  if (list.error && !list.data) {
    return (
      <ErrorState title="Component detection failed" detail={describe(list.error)} />
    );
  }
  const components = list.data?.components ?? [];
  const gaps = list.data?.profileGaps ?? [];

  return (
    <div className="k-components">
      {gaps.length > 0 ? (
        <p className="k-note">
          Detection could not read everything it looked for; components below
          may show &ldquo;could not tell&rdquo;. The Cluster page lists the
          missing permissions.
        </p>
      ) : null}
      <ul className="k-components__list">
        {components.map((c) => (
          <li key={c.name} className="k-panel k-components__row">
            <div className="k-components__head">
              <div>
                <span className="k-components__name">{c.title}</span>{" "}
                <code className="k-components__token">{c.name}</code>
              </div>
              <div className="k-components__state">
                {presencePill(c)}
                {c.presence === "no" && c.installable ? (
                  <button
                    className="k-button k-button--primary"
                    disabled={flow.kind === "planning" || flow.kind === "installing"}
                    onClick={() => void plan(c.name)}
                  >
                    Install {c.version}
                  </button>
                ) : null}
              </div>
            </div>
            <p className="k-components__provides">{c.provides}</p>
            {c.presenceDetail ? (
              <p className="k-components__detail">{c.presenceDetail}</p>
            ) : null}
            {c.presence === "no" && !c.installable ? (
              <p className="k-components__detail">
                kelson does not install this one: {c.followUp}. Install it from
                upstream and detection will adopt it.
              </p>
            ) : null}
            {flowFor(flow, c.name) ? (
              <InstallFlow
                flow={flow}
                component={c}
                onConfirm={(planned) => void apply(c.name, planned)}
                onDismiss={() => setFlow({ kind: "idle" })}
              />
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  );
}

function flowFor(flow: Flow, component: string): boolean {
  return flow.kind !== "idle" && flow.component === component;
}

function InstallFlow({
  flow,
  component,
  onConfirm,
  onDismiss,
}: {
  flow: Flow;
  component: ComponentStatus;
  onConfirm: (plan: PlanInstallResponse) => void;
  onDismiss: () => void;
}) {
  switch (flow.kind) {
    case "planning":
      return <LoadingState what="the install plan" />;
    case "failed":
      return (
        <div className="k-components__flow">
          <ErrorState title="Install failed" detail={flow.message} />
          <button className="k-button" onClick={onDismiss}>
            Dismiss
          </button>
        </div>
      );
    case "planned":
    case "installing": {
      const items = flow.plan.items;
      const refusals = flow.plan.refusals;
      return (
        <div className="k-components__flow">
          {refusals.map((r) => (
            <p key={r.component} className="k-components__detail">
              {r.component}: not installed — {r.reason}. {r.remediation}
            </p>
          ))}
          {items.map((item) => (
            <div key={item.component}>
              <p>
                {item.title} {item.version} → namespace{" "}
                <code>{item.namespace}</code>, {item.objects.length} resources.
              </p>
              <p className="k-components__detail">
                from <code>{item.manifestUrl}</code>
                <br />
                sha256 <code>{item.digest}</code> (verified)
              </p>
              <Disclosure summary={`The ${item.objects.length} resources`}>
                <ul className="k-components__objects">
                  {item.objects.map((o) => (
                    <li key={`${o.kind}/${o.namespace}/${o.name}`}>
                      <code>
                        {o.kind}/{o.namespace ? `${o.namespace}/` : ""}
                        {o.name}
                      </code>
                      {o.exists
                        ? " — already exists; kelson applies over it and never deletes it"
                        : ""}
                      {o.authored ? " — written by kelson, not the upstream manifest" : ""}
                    </li>
                  ))}
                </ul>
              </Disclosure>
              <div className="k-actions">
                <button
                  className="k-button k-button--primary"
                  disabled={flow.kind === "installing"}
                  onClick={() => onConfirm(flow.plan)}
                >
                  {flow.kind === "installing"
                    ? "Installing…"
                    : `Apply these ${item.objects.length} resources`}
                </button>
                <button
                  className="k-button"
                  disabled={flow.kind === "installing"}
                  onClick={onDismiss}
                >
                  Cancel
                </button>
              </div>
            </div>
          ))}
          {items.length === 0 && refusals.length === 0 ? (
            <p className="k-components__detail">Nothing to do.</p>
          ) : null}
          {items.length === 0 && refusals.length > 0 ? (
            <button className="k-button" onClick={onDismiss}>
              Close
            </button>
          ) : null}
        </div>
      );
    }
    case "installed": {
      const reports = flow.report.components;
      return (
        <div className="k-components__flow">
          {reports.map((r) => (
            <p key={r.component}>
              Installed: {r.created} created, {r.adopted} adopted
              {r.failed > 0 ? `, ${r.failed} failed` : ""}.
            </p>
          ))}
          <Boundary name={component.name} />
          <button className="k-button" onClick={onDismiss}>
            Done
          </button>
        </div>
      );
    }
    default:
      return null;
  }
}

/**
 * What installing did NOT configure — stated exactly where somebody will
 * assume it did, mirroring the CLI's post-install boundary.
 */
function Boundary({ name }: { name: string }) {
  const text: Record<string, string> = {
    "cert-manager":
      "cert-manager has no ClusterIssuer yet. Which ACME account or CA to trust is your decision; kelson renders a Certificate once detection reports an issuer.",
    flux: "Flux is running and reconciling nothing until a deploy in Flux mode points it at a repository.",
    cnpg: "CloudNativePG is running with no databases; postgres components render against it from the next deploy.",
    "envoy-gateway":
      "Envoy Gateway has no GatewayClass and carries no traffic. Create a GatewayClass naming controller gateway.envoyproxy.io/gatewayclass-controller and a Gateway with your listeners — routes attach once detection reports the class.",
  };
  const line = text[name];
  return line ? <p className="k-note">{line}</p> : null;
}

function describe(err: unknown): string {
  if (err instanceof ConnectError) {
    return `${err.code}: ${err.rawMessage}`;
  }
  return err instanceof Error ? err.message : String(err);
}
