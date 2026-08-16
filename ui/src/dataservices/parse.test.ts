import { describe, expect, it } from "vitest";

import {
  clusterResourceName,
  clusterVerdictFor,
  isDataServiceVerdict,
  parseDataServices,
} from "./parse";

/**
 * The fixture is the shape of examples/checkout-multi/project.yaml, comments
 * and blank lines included: that file is what a person writing a database into
 * a spec copies, so it is what the reader has to survive.
 */
const PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout

spec:
  env:
    LOG_LEVEL: info

  components:
    - name: db
      kind: postgres          # topology is the operator's job (ADR-0005)
      preset: ha-small        # 3 instances, synchronous

    - name: cache
      kind: valkey

    - name: web
      port: 8080
      env:
        name: not-the-component-name
      replicas: { min: 2, max: 10 }

  defaults:
    secrets:
      backend: cluster
`;

const ENVIRONMENT = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production

spec:
  project: checkout
  namespace: checkout-prod

  components:
    - name: db
      preset: ha-medium
    - name: web
      replicas: { min: 3, max: 20 }
`;

describe("parseDataServices", () => {
  it("reads the data components and leaves the workloads alone", () => {
    const services = parseDataServices(PROJECT);

    expect(services.map((s) => s.name)).toEqual(["db", "cache"]);
    expect(services[0]).toEqual({
      name: "db",
      kind: "postgres",
      preset: "ha-small",
      presetSource: "component",
    });
  });

  it("applies the environment's preset override (rule P5) and says it did", () => {
    const services = parseDataServices(PROJECT, ENVIRONMENT);

    expect(services[0]?.preset).toBe("ha-medium");
    expect(services[0]?.presetSource).toBe("environment");
    // The override names one component; the other keeps what the project said.
    expect(services[1]?.preset).toBe("shared");
  });

  it("defaults an unset preset to shared, exactly as model.Resolve does", () => {
    const services = parseDataServices(PROJECT);

    expect(services[1]).toEqual({
      name: "cache",
      kind: "valkey",
      preset: "shared",
      presetSource: "default",
    });
  });

  it("does not mistake a nested key for a component field", () => {
    // `env: {name: …}` under the web component must not become a component
    // called "not-the-component-name", and must not turn web into a data one.
    expect(
      parseDataServices(PROJECT).some((s) => s.name === "not-the-component-name"),
    ).toBe(false);
  });

  it("reads a flow-mapping component, which is what the model's own tests write", () => {
    const doc = `spec:
  components:
    - {name: db, kind: postgres, preset: small}
    - {name: web, port: 8080, replicas: {min: 1, max: 2}}
`;
    expect(parseDataServices(doc)).toEqual([
      { name: "db", kind: "postgres", preset: "small", presetSource: "component" },
    ]);
  });

  it("finds nothing in a document it cannot read, rather than guessing", () => {
    expect(parseDataServices("")).toEqual([]);
    expect(parseDataServices("spec:\n  components: []\n")).toEqual([]);
  });
});

describe("cluster resources", () => {
  it("names the Cluster the way the renderer does", () => {
    expect(clusterResourceName("checkout", "production", "db")).toBe(
      "checkout-production-db",
    );
  });

  it("matches only a Cluster verdict for that exact name", () => {
    const verdicts = [
      { resource: "Deployment/checkout-prod/web" },
      { resource: "Cluster/checkout-prod/checkout-production-db" },
    ];

    expect(clusterVerdictFor(verdicts, "checkout-production-db")?.resource).toBe(
      "Cluster/checkout-prod/checkout-production-db",
    );
    expect(clusterVerdictFor(verdicts, "checkout-staging-db")).toBeUndefined();
    expect(isDataServiceVerdict("Deployment/checkout-prod/web")).toBe(false);
    expect(isDataServiceVerdict("Cluster/checkout-prod/checkout-production-db")).toBe(
      true,
    );
  });
});
