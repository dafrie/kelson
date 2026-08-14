import { DiffView } from "kelson-ui";

const base = {
  level: "live",
  project: "checkout",
  environment: "production",
  unvalidated: [],
  violations: [],
  degraded: false,
  degradedReason: "",
};

/** A routine change: one image bump, one config edit. */
export function RoutineChange() {
  return (
    <DiffView
      diff={{
        ...base,
        resources: [
          {
            apiVersion: "apps/v1",
            kind: "Deployment",
            name: "checkout-web",
            namespace: "shop-production",
            op: "modified",
            risk: "restarting",
            fields: [
              {
                path: "spec.template.spec.containers[0].image",
                before: "ghcr.io/acme/checkout:1.41.2",
                after: "ghcr.io/acme/checkout:1.42.0",
                origin: "spec",
                risk: "restarting",
                specPath: "components.web.image",
              },
              {
                path: "spec.template.spec.containers[0].env[2].value",
                before: "info",
                after: "warn",
                origin: "overlay",
                risk: "safe",
                specPath: "environments.production.overlays.web.env.LOG_LEVEL",
              },
            ],
          },
          {
            apiVersion: "v1",
            kind: "ConfigMap",
            name: "checkout-web-config",
            namespace: "shop-production",
            op: "added",
            risk: "safe",
            fields: [],
          },
        ],
        summary: {
          added: 1,
          modified: 1,
          removed: 0,
          restarting: ["Deployment/checkout-web"],
          disruptive: [],
          maxRisk: "restarting",
        },
      }}
    />
  );
}

/** An enforcing policy rejected the preview — the blocked banner. */
export function Blocked() {
  return (
    <DiffView
      exitSemantics={3}
      diff={{
        ...base,
        resources: [
          {
            apiVersion: "apps/v1",
            kind: "Deployment",
            name: "checkout-web",
            namespace: "shop-production",
            op: "modified",
            risk: "disruptive",
            fields: [
              {
                path: "spec.replicas",
                before: 4,
                after: 0,
                origin: "spec",
                risk: "disruptive",
                specPath: "components.web.replicas",
              },
            ],
          },
        ],
        violations: [
          {
            engine: "kyverno",
            policy: "require-minimum-replicas",
            rule: "min-two-replicas",
            resource: "Deployment/checkout-web",
            path: "spec.replicas",
            specPath: "components.web.replicas",
            message: "production workloads must keep at least two replicas",
            enforcement: "enforce",
          },
        ],
        summary: {
          added: 0,
          modified: 1,
          removed: 0,
          restarting: [],
          disruptive: ["Deployment/checkout-web"],
          maxRisk: "disruptive",
        },
      }}
    />
  );
}

/** The cluster was unavailable: a best-effort rendered diff, plus an unvalidated resource. */
export function BestEffort() {
  return (
    <DiffView
      diff={{
        ...base,
        level: "rendered",
        degraded: true,
        degradedReason: "cluster connection timed out",
        resources: [],
        unvalidated: [
          {
            resource: "RedisFailover/checkout-cache",
            requires: "databases.spotahome.com/v1",
            inBatch: true,
            message:
              "the CRD this resource needs is created earlier in this same batch, so it could not be validated against the live cluster",
          },
        ],
        summary: {
          added: 0,
          modified: 0,
          removed: 0,
          restarting: [],
          disruptive: [],
          maxRisk: "safe",
        },
      }}
    />
  );
}
