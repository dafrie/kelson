import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import {
  PreviewLifecycleSchema,
  PreviewSchema,
  PreviewSettingsSchema,
} from "../gen/kelson/v1alpha1/preview_pb";
import {
  changeRequestUrl,
  lifecycleLine,
  previewHeadline,
  previewStatus,
  settingsLine,
  shortSha,
} from "./phase";

const settings = (fields: Record<string, unknown> = {}) =>
  create(PreviewSettingsSchema, {
    provider: "github",
    repo: "https://github.com/acme/checkout",
    secretRef: "github-auth",
    interval: "10m",
    limit: 10,
    artifactsRepository: "oci://ghcr.io/acme/checkout-previews",
    ...fields,
  });

const preview = (fields: Record<string, unknown> = {}) =>
  create(PreviewSchema, {
    id: "412",
    namespace: "checkout-staging-pr412",
    sha: "0123456789abcdef0123456789abcdef01234567",
    phase: "ready",
    ...fields,
  });

describe("previewStatus", () => {
  const word = (phase: string, suspended = false) =>
    previewStatus(phase, suspended).word;

  it("maps each phase onto the one status vocabulary", () => {
    expect(word("ready")).toBe("live");
    expect(word("applying")).toBe("deploying");
    expect(word("failed")).toBe("failed");
  });

  it("draws a missing artifact as waiting, not failed", () => {
    // The commonest cause is a CI step nobody added (ADR-0017 decision 8), and
    // "failed" would send a reader to the manifests instead of the workflow.
    // The row's own sentence names the step.
    expect(word("awaiting-artifact")).toBe("waiting");
  });

  it("has somewhere honest to put a phase it does not know", () => {
    expect(word("unknown")).toBe("unknown");
    expect(word("teleported")).toBe("unknown");
    expect(word("")).toBe("unknown");
  });

  it("lets suspension win over the phase", () => {
    // A suspended Kustomization keeps its last conditions, so its phase
    // describes a moment nobody is maintaining any more — which is not the
    // same claim as "waiting", where something is expected to act.
    expect(word("ready", true)).toBe("suspended");
    expect(word("failed", true)).toBe("suspended");
    expect(previewStatus("ready", true).tone).toBe("suspended");
  });
});

describe("previewHeadline", () => {
  it("names the CI step for a preview with no artifact", () => {
    const text = previewHeadline(preview({ phase: "awaiting-artifact" }));
    expect(text).toContain("kelson preview publish");
    expect(text).toContain("CI step");
  });

  it("says what a suspended preview is running", () => {
    expect(previewHeadline(preview({ suspended: true }))).toContain(
      "last thing that reconciled",
    );
  });

  it("says nothing has reported for an unknown phase", () => {
    expect(previewHeadline(preview({ phase: "unknown" }))).toContain(
      "has reported yet",
    );
  });
});

describe("lifecycleLine", () => {
  it("tells a missing operator from a missing pair from a failing poller", () => {
    const absent = lifecycleLine(
      create(PreviewLifecycleSchema, { name: "checkout-staging-previews" }),
    );
    expect(absent.text).toContain("flux-operator is not installed");

    const undelivered = lifecycleLine(
      create(PreviewLifecycleSchema, {
        name: "checkout-staging-previews",
        served: true,
      }),
    );
    expect(undelivered.text).toContain("does not exist in the cluster yet");
    expect(undelivered.text).toContain("checkout-staging-previews");

    const failing = lifecycleLine(
      create(PreviewLifecycleSchema, {
        name: "checkout-staging-previews",
        served: true,
        present: true,
        providerReady: "False",
        providerReason: "AuthenticationFailed",
        providerMessage: "401 from api.github.com",
      }),
    );
    expect(failing.status).toBe("failed");
    expect(failing.text).toContain("AuthenticationFailed: 401 from api.github.com");
  });

  it("reports a healthy pair as synced", () => {
    const ready = lifecycleLine(
      create(PreviewLifecycleSchema, {
        name: "checkout-staging-previews",
        served: true,
        present: true,
        providerReady: "True",
        setReady: "True",
      }),
    );
    expect(ready.status).toBe("synced");
  });

  it("claims nothing when the cluster was not read", () => {
    expect(lifecycleLine(undefined).status).toBe("unknown");
  });
});

describe("changeRequestUrl", () => {
  it("builds the forge's own URL from the provider and the source repo", () => {
    expect(changeRequestUrl(settings(), "412")).toBe(
      "https://github.com/acme/checkout/pull/412",
    );
    expect(
      changeRequestUrl(
        settings({ provider: "gitlab", repo: "https://gitlab.com/acme/checkout/" }),
        "9",
      ),
    ).toBe("https://gitlab.com/acme/checkout/-/merge_requests/9");
  });

  it("declines to guess", () => {
    // A wrong link to a pull request is worse than none.
    expect(changeRequestUrl(settings({ repo: "git@github.com:acme/x.git" }), "1")).toBe("");
    expect(changeRequestUrl(settings({ provider: "gitea" }), "1")).toBe("");
    expect(changeRequestUrl(settings(), "")).toBe("");
    expect(changeRequestUrl(undefined, "1")).toBe("");
  });
});

describe("shortSha", () => {
  it("abbreviates for display only", () => {
    expect(shortSha("0123456789abcdef0123456789abcdef01234567")).toBe("0123456789ab");
    expect(shortSha("abc")).toBe("abc");
    expect(shortSha("")).toBe("");
  });
});

describe("settingsLine", () => {
  it("states the filter and the ceiling the cluster will enforce", () => {
    expect(settingsLine(settings({ filterLabels: ["deploy/preview"] }))).toBe(
      "Polling https://github.com/acme/checkout every 10m for change requests " +
        "labelled deploy/preview, at most 10 at once.",
    );
  });

  it("says so when no label narrows the poll", () => {
    expect(settingsLine(settings())).toContain("every open change request");
  });
});
