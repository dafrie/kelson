import { describe, expect, it } from "vitest";

import { resourceParts, revisionParts } from "./facts";

/**
 * The two parsers expert mode stands on. Both are tested for what they refuse
 * as much as for what they read: the contract is that a value they do not
 * recognise yields nothing and the screen prints nothing, because a wrong
 * generation number on a dense screen is worse than an absent one.
 */
describe("revisionParts", () => {
  it("reads the generation and the spec hash out of a revision tag", () => {
    expect(revisionParts("45-9e8d7c6b")).toEqual({
      generation: "45",
      specHash: "9e8d7c6b",
    });
    expect(revisionParts("1-0000000a")).toEqual({
      generation: "1",
      specHash: "0000000a",
    });
  });

  it("refuses anything that is not <generation>-<hex>", () => {
    // A git sha from the retired spine: no generation in it to print.
    expect(revisionParts("8f2c1ad")).toBeUndefined();
    expect(revisionParts("")).toBeUndefined();
    expect(revisionParts("v45-9e8d7c6b")).toBeUndefined();
    expect(revisionParts("45-9E8D7C6B")).toBeUndefined();
    expect(revisionParts("45-")).toBeUndefined();
    expect(revisionParts("-9e8d7c6b")).toBeUndefined();
    // A third segment is a scheme this build does not know.
    expect(revisionParts("45-9e8d7c6b-2")).toBeUndefined();
  });
});

describe("resourceParts", () => {
  it("splits a verdict's resource into kind, namespace and name", () => {
    expect(resourceParts("Deployment/checkout-production/web")).toEqual({
      kind: "Deployment",
      namespace: "checkout-production",
      name: "web",
    });
    // A data component's own object, which is not a Deployment.
    expect(resourceParts("Cluster/checkout-production/checkout-production-db"))
      .toEqual({
        kind: "Cluster",
        namespace: "checkout-production",
        name: "checkout-production-db",
      });
  });

  it("refuses a shape that is not Kind/namespace/name", () => {
    expect(resourceParts("web")).toBeUndefined();
    expect(resourceParts("Deployment/web")).toBeUndefined();
    expect(resourceParts("Deployment//web")).toBeUndefined();
    expect(resourceParts("Deployment/ns/name/extra")).toBeUndefined();
    expect(resourceParts("")).toBeUndefined();
  });
});
