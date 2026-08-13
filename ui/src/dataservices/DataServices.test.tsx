import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";

import { ProfileService } from "../gen/kelson/v1alpha1/profile_pb";
import { RenderService } from "../gen/kelson/v1alpha1/render_pb";
import { renderAt } from "../test/render";
import { DataServices, type ResourceVerdict } from "./DataServices";

const PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout

spec:
  image: ghcr.io/acme/checkout:1

  components:
    - name: db
      kind: postgres
      preset: ha-small

    - name: web
      port: 8080
`;

const SHARED_PROJECT = `spec:
  image: ghcr.io/acme/checkout:1

  components:
    - name: db
      kind: postgres
      preset: shared
`;

const PROFILE = `storageClasses:
    - name: local-path
      provisioner: rancher.io/local-path
      default: true
      cloneCapability: none
      cloneConfidence: observed
cnpg:
    version: 1.30.0
    namespace: cnpg-system
`;

/** The structured refusal internal/renderer returns for a deferred preset. */
const SHARED_REFUSAL = {
  code: "render/service-not-implemented",
  message:
    'service "db" requests preset "shared": the shared preset is deferred — dedicated presets (small, ha-small, ha-medium) work today',
  remediation:
    "the shared cluster was ADR-0007's cost optimization, never an ask (owner decision, issue #93). Use preset: small",
};

function transportFor(errors: (typeof SHARED_REFUSAL)[] = []) {
  return createRouterTransport((router) => {
    router.service(ProfileService, {
      getProfile: () => ({ yaml: new TextEncoder().encode(PROFILE) }),
    });
    router.service(RenderService, {
      render: () => ({ errors }),
    });
  });
}

function renderSection(
  projectDoc: string,
  verdicts: readonly ResourceVerdict[],
  errors: (typeof SHARED_REFUSAL)[] = [],
) {
  return renderAt(
    transportFor(errors),
    "/",
    "/",
    <DataServices
      project="checkout"
      environment="production"
      projectDoc={projectDoc}
      environmentDoc=""
      health={{ state: "read", verdicts }}
    />,
  );
}

describe("DataServices", () => {
  it("spells out what the preset means, in the renderer's own numbers", async () => {
    renderSection(PROJECT, []);

    expect(await screen.findByText("Data services (1)")).toBeTruthy();
    expect(screen.getByText("db")).toBeTruthy();
    expect(screen.getByText("preset: ha-small")).toBeTruthy();
    expect(screen.getByText("3 instances")).toBeTruthy();
    expect(screen.getByText("500m CPU requested per instance, no limit")).toBeTruthy();
    expect(screen.getByText("1Gi memory, request and limit")).toBeTruthy();
    expect(screen.getByText("5Gi storage per instance")).toBeTruthy();
    expect(
      screen.getByText(
        "synchronous replication — 1 standby must confirm every commit",
      ),
    ).toBeTruthy();
    // The name the renderer gives the Cluster, so a reader can find it in the
    // cluster without deriving it themselves.
    expect(
      screen.getByText("CloudNativePG Cluster · checkout-production-db"),
    ).toBeTruthy();
    // The workload in the same list is not a data service.
    expect(screen.queryByText("web")).toBeNull();
  });

  it("shows the Cluster's verdict when the status data carries one", async () => {
    const verdicts: ResourceVerdict[] = [
      {
        resource: "Cluster/checkout-prod/checkout-production-db",
        code: "healthy",
        healthy: true,
        degraded: false,
        message: "3/3 instances ready",
        remediation: "",
      },
    ];
    renderSection(PROJECT, verdicts);

    expect(await screen.findByText("healthy", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.getByText("3/3 instances ready")).toBeTruthy();
  });

  it("says there is no verdict rather than showing a database as healthy", async () => {
    renderSection(PROJECT, []);

    expect(
      await screen.findByText(/Live health: no verdict/),
    ).toBeTruthy();
    expect(screen.queryByText("healthy", { selector: ".k-pill" })).toBeNull();
  });

  it("distinguishes a status that could not be read from an absent verdict", async () => {
    renderAt(
      transportFor(),
      "/",
      "/",
      <DataServices
        project="checkout"
        environment="production"
        projectDoc={PROJECT}
        environmentDoc=""
        health={{ state: "unavailable" }}
      />,
    );

    expect(await screen.findByText(/status could not be read/)).toBeTruthy();
    expect(screen.queryByText(/no verdict/)).toBeNull();
  });

  it("labels backups and branching as coming soon, with their issues", async () => {
    renderSection(PROJECT, []);

    expect(await screen.findByText("Backups")).toBeTruthy();
    expect(screen.getByText("Branching")).toBeTruthy();
    expect(screen.getAllByText("Coming soon")).toHaveLength(2);
    expect(
      screen.getByText("volume snapshots", { exact: false }).textContent,
    ).toContain("per environment");
    const issues = screen.getAllByRole("link", { name: /^#(94|99)$/ });
    expect(issues.map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/dafrie/kelson/issues/94",
      "https://github.com/dafrie/kelson/issues/99",
    ]);
  });

  it("surfaces the server's own not-implemented error for a deferred preset", async () => {
    renderSection(SHARED_PROJECT, [], [SHARED_REFUSAL]);

    // The API's code and remediation, not a sentence invented in the browser.
    expect(await screen.findByText("render/service-not-implemented")).toBeTruthy();
    expect(screen.getByText(SHARED_REFUSAL.message)).toBeTruthy();
    expect(screen.getByText(/Use preset: small/)).toBeTruthy();
    // And the row itself reads as deliberate rather than broken.
    expect(screen.getByText("Deferred")).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "#93" }).getAttribute("href"),
    ).toBe("https://github.com/dafrie/kelson/issues/93");
    // A deferred component renders nothing, so it is never given a topology.
    expect(screen.queryByText("1 instance")).toBeNull();
  });

  it("renders nothing at all for a spec with no data components", () => {
    const { container } = renderSection("spec:\n  components:\n    - name: web\n", []);

    expect(container.querySelector(".k-data")).toBeNull();
  });

  it("states the storage capability in the words of the point of use", async () => {
    renderSection(PROJECT, []);

    expect(
      await screen.findByText(
        "Fast branching unavailable — your storage class (local-path) has no snapshot driver.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("why:")).toBeTruthy();
    expect(screen.getByText(/no snapshot class serves rancher.io\/local-path/)).toBeTruthy();
    expect(screen.getByText("snapshot driver: none detected")).toBeTruthy();
    expect(screen.getByText(/CloudNativePG: detected \(1.30.0, in cnpg-system\)/)).toBeTruthy();
  });
});
