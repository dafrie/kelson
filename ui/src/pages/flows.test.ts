import { describe, expect, it } from "vitest";

import { environmentPath, flowTarget, type Flow } from "./flows";

/**
 * Where the six flows went (#260).
 *
 * These are the assertions that keep a deep link honest. Every one of the old
 * paths is in a commit status, a pull-request comment or the docs, and the
 * query parameters are the half that is easy to lose: a redirect that arrives
 * at the rollback screen without `?to=` has technically not broken, and has
 * lost the revision the reader clicked.
 */
describe("flowTarget", () => {
  const target = (flow: Flow, search = "") =>
    flowTarget("checkout", "production", flow, search);

  it("keeps the two that became tabs exactly where they were", () => {
    // The best thing that can happen to a link is nothing. Logs and History are
    // views of the environment, so they became tabs of it without moving.
    expect(target("logs")).toBe("/projects/checkout/production/logs");
    expect(target("history")).toBe("/projects/checkout/production/history");
  });

  it("moves the three actions under actions/, carrying their parameters", () => {
    expect(target("deploy")).toBe(
      "/projects/checkout/production/actions/deploy",
    );
    expect(target("promote")).toBe(
      "/projects/checkout/production/actions/promote",
    );
    // `?to=` is the revision a history row picked; losing it would land the
    // reader on an empty picker with no idea which row they pressed.
    expect(target("rollback", "?to=rev-00000041")).toBe(
      "/projects/checkout/production/actions/rollback?to=rev-00000041",
    );
    // `?image=` is what the build hands the deploy (#136).
    expect(target("deploy", "?image=ghcr.io%2Facme%2Fcheckout%3Av2")).toBe(
      "/projects/checkout/production/actions/deploy?image=ghcr.io%2Facme%2Fcheckout%3Av2",
    );
  });

  it("sends a diff of a revision to the history row that now opens it", () => {
    expect(target("diff", "?from=rev-00000041")).toBe(
      "/projects/checkout/production/history?from=rev-00000041",
    );
  });

  it("sends a diff with no revision to the same panel on its cluster mode", () => {
    // The old screen opened on the live cluster's dry-run verdict when nothing
    // named a revision. `?compare=1` is what asks the history tab for that, and
    // it is only added when nothing else already says which comparison to open.
    expect(target("diff")).toBe(
      "/projects/checkout/production/history?compare=1",
    );
    expect(target("diff", "?component=web")).toBe(
      "/projects/checkout/production/history?component=web&compare=1",
    );
  });

  it("carries ?component= through, whichever flow was linked", () => {
    expect(target("logs", "?component=web")).toBe(
      "/projects/checkout/production/logs?component=web",
    );
    expect(target("deploy", "?component=web")).toBe(
      "/projects/checkout/production/actions/deploy?component=web",
    );
  });

  it("encodes a project and an environment that need it", () => {
    expect(environmentPath("acme/checkout", "pr 12")).toBe(
      "/projects/acme%2Fcheckout/pr%2012",
    );
    expect(flowTarget("acme/checkout", "pr 12", "logs", "")).toBe(
      "/projects/acme%2Fcheckout/pr%2012/logs",
    );
  });
});
