import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import {
  InstallService,
  ListComponentsResponseSchema,
  PlanInstallResponseSchema,
  InstallResponseSchema,
  type InstallRequest,
  type PlanInstallRequest,
} from "../gen/kelson/v1alpha1/install_pb";
import { renderAt } from "../test/render";
import { OnboardingPage } from "./OnboardingPage";

/**
 * The onboarding checklist against a stubbed InstallService: the tri-state
 * renders as three states, the install flow is preview-confirm-apply, and the
 * list refetches after an apply so the row flips to present.
 */

const COMPONENTS = [
  {
    name: "cert-manager",
    title: "cert-manager",
    installable: true,
    presence: "yes",
    presenceDetail: "cert-manager v1.21.1 is present",
    provides: "TLS on routed services",
  },
  {
    name: "envoy-gateway",
    title: "Envoy Gateway",
    version: "v1.8.3",
    installable: true,
    presence: "no",
    provides: "all HTTP routing",
    manifestUrl:
      "https://github.com/envoyproxy/gateway/releases/download/v1.8.3/install.yaml",
    sha256: "37a62afe9bb07d87e86c5c2cff32f046f17397cb4fca9f2a741165826212d781",
  },
  {
    name: "external-secrets",
    title: "external-secrets",
    installable: false,
    presence: "no",
    followUp: "upstream publishes a Helm chart and no plain install manifest",
    provides: "the externalSecrets secret backend",
  },
  {
    name: "cnpg",
    title: "CloudNativePG",
    installable: true,
    presence: "unknown",
    presenceDetail: "detection could not read it: forbidden",
    provides: "every postgres component",
  },
];

function transport(opts?: {
  onPlan?: (req: PlanInstallRequest) => void;
  onInstall?: (req: InstallRequest) => void;
  listCalls?: { count: number };
  installedAfterApply?: boolean;
}) {
  let applied = false;
  return createRouterTransport(({ service }) => {
    service(InstallService, {
      listComponents() {
        if (opts?.listCalls) opts.listCalls.count += 1;
        const components = COMPONENTS.map((c) =>
          applied && opts?.installedAfterApply && c.name === "envoy-gateway"
            ? { ...c, presence: "yes", presenceDetail: "the Gateway API v1 is present" }
            : c,
        );
        return create(ListComponentsResponseSchema, { components });
      },
      planInstall(req) {
        opts?.onPlan?.(req);
        return create(PlanInstallResponseSchema, {
          items: [
            {
              component: "envoy-gateway",
              title: "Envoy Gateway",
              version: "v1.8.3",
              namespace: "envoy-gateway-system",
              manifestUrl:
                "https://github.com/envoyproxy/gateway/releases/download/v1.8.3/install.yaml",
              digest:
                "37a62afe9bb07d87e86c5c2cff32f046f17397cb4fca9f2a741165826212d781",
              objects: [
                { apiVersion: "v1", kind: "Namespace", name: "envoy-gateway-system" },
                {
                  apiVersion: "apps/v1",
                  kind: "Deployment",
                  name: "envoy-gateway",
                  namespace: "envoy-gateway-system",
                },
              ],
            },
          ],
        });
      },
      install(req) {
        opts?.onInstall?.(req);
        applied = true;
        return create(InstallResponseSchema, {
          components: [
            { component: "envoy-gateway", installed: true, created: 42, adopted: 1 },
          ],
        });
      },
    });
  });
}

function renderSetup(t = transport()) {
  return renderAt(t, "/setup", "/setup", <OnboardingPage />, [
    { path: "/projects/new", element: <div /> },
    { path: "/cluster", element: <div /> },
  ]);
}

describe("OnboardingPage", () => {
  it("renders the tri-state and offers Install only where it is honest", async () => {
    renderSetup();
    await screen.findByText("Envoy Gateway");

    // Present, missing and could-not-tell are three states, not two.
    expect(screen.getAllByText("present").length).toBeGreaterThan(0);
    expect(screen.getAllByText("missing").length).toBeGreaterThan(0);
    expect(screen.getByText("could not tell")).toBeTruthy();

    // One install button: envoy-gateway. The deferred row explains itself
    // instead, and the unknown row gets neither.
    const buttons = screen.getAllByRole("button", { name: /Install v/ });
    expect(buttons).toHaveLength(1);
    expect(buttons[0]?.textContent).toContain("v1.8.3");
    expect(screen.getByText(/kelson does not install this one/).textContent).toContain(
      "Helm chart",
    );

    // The progress line counts what detection found.
    expect(screen.getByText(/1 of 4 components present/)).toBeTruthy();
  });

  it("installs through preview, confirm, apply — and refetches", async () => {
    const listCalls = { count: 0 };
    let planned: PlanInstallRequest | undefined;
    let installed: InstallRequest | undefined;
    renderSetup(
      transport({
        onPlan: (req) => (planned = req),
        onInstall: (req) => (installed = req),
        listCalls,
        installedAfterApply: true,
      }),
    );

    fireEvent.click(await screen.findByRole("button", { name: "Install v1.8.3" }));

    // The preview: the digest is visible before anything can be applied, and
    // nothing has been installed yet.
    await screen.findByText(/sha256/);
    expect(planned?.components).toEqual(["envoy-gateway"]);
    expect(installed).toBeUndefined();

    fireEvent.click(screen.getByRole("button", { name: "Apply these 2 resources" }));

    // The report, the boundary, and the refetched list.
    await screen.findByText(/42 created, 1 adopted/);
    expect(installed?.components).toEqual(["envoy-gateway"]);
    expect(screen.getByText(/GatewayClass/).textContent).toContain(
      "gateway.envoyproxy.io/gatewayclass-controller",
    );
    await waitFor(() => expect(listCalls.count).toBeGreaterThan(1));
  });
});
