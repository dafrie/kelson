import { describe, expect, it } from "vitest";

import { decodeDiff, summaryLine } from "./parse";
import fixture from "./testdata/server-diff.json?raw";

/**
 * The fixture is real output, not a hand-written approximation: it is
 * diff.EncodeJSON(serverDiff()) from internal/diff/format_test.go, captured
 * from a Go test run. That matters — the decoder's whole job is to agree with
 * what Go actually emits, including which `omitempty` fields are simply absent.
 */
describe("decodeDiff", () => {
  const diff = decodeDiff(fixture);

  it("decodes the headers and the level", () => {
    expect(diff.level).toBe("server");
    expect(diff.project).toBe("checkout");
    expect(diff.environment).toBe("production");
  });

  it("decodes resources with their op and risk", () => {
    expect(diff.resources).toHaveLength(2);
    const [deployment, netpol] = diff.resources;
    expect(deployment?.kind).toBe("Deployment");
    expect(deployment?.op).toBe("modified");
    expect(deployment?.risk).toBe("restart-required");
    expect(netpol?.op).toBe("added");
    expect(netpol?.risk).toBe("additive");
  });

  it("decodes field changes with before, after and origin", () => {
    const field = diff.resources[0]?.fields[0];
    expect(field?.path).toBe(
      "spec.template.spec.containers[0].env[LOG_LEVEL].value",
    );
    expect(field?.before).toBe("info");
    expect(field?.after).toBe("debug");
    expect(field?.origin).toBe("spec");
  });

  it("keeps enforce and audit violations distinct", () => {
    expect(diff.violations.map((v) => v.enforcement)).toEqual([
      "enforce",
      "audit",
    ]);
    expect(diff.violations[0]?.policy).toBe("require-run-as-nonroot");
  });

  it("keeps unvalidated resources out of violations", () => {
    expect(diff.unvalidated).toHaveLength(1);
    expect(diff.unvalidated[0]?.resource).toBe("Widget/thing");
    expect(diff.unvalidated[0]?.inBatch).toBe(true);
    expect(diff.violations.some((v) => v.resource.includes("Widget"))).toBe(
      false,
    );
  });

  it("decodes the summary and renders the CLI's roll-up", () => {
    expect(diff.summary.added).toBe(1);
    expect(diff.summary.maxRisk).toBe("restart-required");
    expect(summaryLine(diff)).toBe(
      "1 added · 1 modified · 0 removed · restart: [web] · max risk: restart-required",
    );
  });

  it("treats absent omitempty fields as empty, not as an error", () => {
    // The NetworkPolicy has no `fields` key at all, and neither resource is
    // degraded — a clean diff must not decode as a broken one.
    expect(diff.resources[1]?.fields).toEqual([]);
    expect(diff.degraded).toBe(false);
    expect(diff.degradedReason).toBe("");
  });

  it("refuses a payload it cannot understand rather than half-decoding", () => {
    expect(() => decodeDiff("")).toThrow(/empty/);
    expect(() => decodeDiff("not json")).toThrow(/not JSON/);
    expect(() => decodeDiff("[]")).toThrow(/not a JSON object/);
    expect(() => decodeDiff('{"resources":[{"kind":7}]}')).toThrow(
      /expected a string/,
    );
  });
});
