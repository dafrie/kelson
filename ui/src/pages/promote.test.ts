import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import {
  PromotedComponentSchema,
  PromotionStatus,
} from "../gen/kelson/v1alpha1/deploy_pb";
import {
  countStatuses,
  promotionSummary,
  shortImage,
  statusName,
} from "./promote";

const DIGEST =
  "ghcr.io/acme/checkout@sha256:9f6ad2c1e4b5a70d3c8f1e2b4a6d8c0e2f4a6b8d0c2e4f6a8b0d2c4e6f8a0b2c4";

function component(status: PromotionStatus) {
  return create(PromotedComponentSchema, { component: "web", status });
}

describe("statusName", () => {
  it("uses internal/promote's own words", () => {
    expect(statusName(PromotionStatus.PINNED)).toBe("pinned");
    expect(statusName(PromotionStatus.UNCHANGED)).toBe("unchanged");
    expect(statusName(PromotionStatus.SKIPPED)).toBe("skipped");
  });

  it("does not render an outcome it does not know as one it does", () => {
    // A newer server naming a fourth status must not arrive on screen disguised
    // as "pinned" — the reader would believe an image was written.
    expect(statusName(PromotionStatus.UNSPECIFIED)).toBe("unknown");
    expect(statusName(9 as PromotionStatus)).toBe("unknown");
  });
});

describe("promotionSummary", () => {
  it("counts every status the plan carried, in the CLI's words", () => {
    const counts = countStatuses([
      component(PromotionStatus.PINNED),
      component(PromotionStatus.PINNED),
      component(PromotionStatus.UNCHANGED),
      component(PromotionStatus.SKIPPED),
    ]);
    expect(counts).toEqual({ pinned: 2, unchanged: 1, skipped: 1, unknown: 0 });
    expect(promotionSummary(counts)).toBe("2 pinned, 1 unchanged, 1 skipped");
  });

  it("says so rather than absorbing a status it cannot name", () => {
    const counts = countStatuses([
      component(PromotionStatus.PINNED),
      component(PromotionStatus.UNSPECIFIED),
    ]);
    expect(promotionSummary(counts)).toBe(
      "1 pinned, 0 unchanged, 0 skipped, 1 not understood by this build",
    );
  });
});

describe("shortImage", () => {
  it("leaves a reference that already reads at a glance alone", () => {
    expect(shortImage("ghcr.io/acme/checkout:v3")).toBe(
      "ghcr.io/acme/checkout:v3",
    );
  });

  it("elides the middle of a digest, keeping the repository and the tail", () => {
    const short = shortImage(DIGEST);
    expect(short.length).toBeLessThan(DIGEST.length);
    // The repository and the leading hex — what a reader compares — survive.
    expect(short.startsWith("ghcr.io/acme/checkout@sha256:9f6ad2c1")).toBe(true);
    // So does the tail, which is what tells two truncations apart.
    expect(short.endsWith(DIGEST.slice(-8))).toBe(true);
    expect(short).toContain("…");
  });

  it("truncates a reference of any shape without parsing it", () => {
    // No tag, no digest, no registry: still just a string, still truncated.
    const odd = "a".repeat(120);
    expect(shortImage(odd)).toBe(`${"a".repeat(39)}…${"a".repeat(8)}`);
  });
});
