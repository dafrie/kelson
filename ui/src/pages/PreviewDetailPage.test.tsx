import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";

import {
  PreviewService,
  type ListPreviewsResponseSchema,
} from "../gen/kelson/v1alpha1/preview_pb";
import { renderAt } from "../test/render";
import { PreviewDetailPage } from "./PreviewDetailPage";

/**
 * The page a commit status and a PR comment link to (ADR-0017 stage 3, #248).
 * There is no `GetPreview` RPC, so every case here drives the same
 * `ListPreviews` the project page's Previews panel uses and asserts the page
 * picks out the one row the route's `:pr` names — including the honest cases
 * where nothing does.
 */

type Response = MessageInitShape<typeof ListPreviewsResponseSchema>;

const CONFIGURED: Response = {
  project: "checkout",
  environment: "staging",
  namespace: "checkout-staging",
  mode: "flux",
  settings: {
    provider: "github",
    repo: "https://github.com/acme/checkout",
    secretRef: "github-auth",
    interval: "10m",
    filterLabels: ["deploy/preview"],
    includeBranch: "",
    excludeBranch: "",
    limit: 10,
    skipLabels: ["!ci/passed"],
    artifactsRepository: "oci://ghcr.io/acme/checkout-previews",
    artifactsSecretRef: "",
  },
  lifecycle: {
    name: "checkout-staging-previews",
    served: true,
    present: true,
    providerReady: "True",
    providerReason: "",
    providerMessage: "",
    setReady: "True",
    setReason: "",
    setMessage: "",
  },
};

const PREVIEWS = [
  {
    id: "412",
    namespace: "checkout-staging-pr412",
    sha: "0123456789abcdef0123456789abcdef01234567",
    phase: "ready",
    reason: "",
    message: "",
    artifactReady: "True",
    appliedReady: "True",
    revision: "sha256:beef",
    suspended: false,
    hosts: ["web-pr412.staging.acme.run"],
    createdAt: "2026-08-14T09:00:00Z",
    ageSeconds: 7200n,
  },
  {
    id: "9",
    namespace: "checkout-staging-pr9",
    sha: "fedcba9876543210fedcba9876543210fedcba98",
    phase: "awaiting-artifact",
    reason: "OCIArtifactPullFailed",
    message: "failed to pull artifact: MANIFEST_UNKNOWN",
    artifactReady: "False",
    appliedReady: "False",
    revision: "",
    suspended: false,
    hosts: [],
    createdAt: "",
    ageSeconds: 0n,
  },
];

function transportFor(response: Response): Transport {
  return createRouterTransport((router) => {
    router.service(PreviewService, { listPreviews: () => response });
  });
}

function renderDetail(pr: string, response: Response, path?: string) {
  return renderAt(
    transportFor(response),
    path ?? `/projects/checkout/staging/previews/${pr}`,
    "/projects/:project/:env/previews/:pr",
    <PreviewDetailPage />,
  );
}

describe("PreviewDetailPage", () => {
  it("shows what ListPreviews reports for the pr the route names, and nothing it doesn't", async () => {
    renderDetail("412", { ...CONFIGURED, previews: PREVIEWS });

    expect(await screen.findByText("Preview pr412")).toBeTruthy();
    // The breadcrumb names the project and environment the route carries.
    expect(screen.getByRole("link", { name: "← checkout" }).getAttribute("href")).toBe(
      "/projects/checkout",
    );
    expect(screen.getByText("staging")).toBeTruthy();

    // Phase, commit and age, the same vocabulary the list panel uses.
    expect(screen.getByText("ready")).toBeTruthy();
    expect(screen.getByText("0123456789ab")).toBeTruthy();
    expect(screen.getByText("2h")).toBeTruthy();

    // The two Ready conditions, shown as raw values — this page says more
    // than the list row does, because a reader who typed a PR number in
    // wants the detail.
    expect(screen.getByText("artifact ready")).toBeTruthy();
    expect(screen.getByText("applied ready")).toBeTruthy();
    const trueValues = screen.getAllByText("True");
    expect(trueValues.length).toBeGreaterThanOrEqual(2);

    // Namespace and applied revision, both copyable.
    expect(screen.getByText("checkout-staging-pr412")).toBeTruthy();
    expect(screen.getByText("sha256:beef")).toBeTruthy();

    // The hostname, linked and carrying the change request in its label.
    const host = screen.getByRole("link", { name: "web-pr412.staging.acme.run" });
    expect(host.getAttribute("href")).toBe("https://web-pr412.staging.acme.run");

    // The forge link, derived the same way the list row derives it.
    expect(
      screen.getByRole("link", { name: "View pull request" }).getAttribute("href"),
    ).toBe("https://github.com/acme/checkout/pull/412");

    // Created, formatted the way history's committed_at is.
    expect(screen.getByText("2026-08-14 09:00:00Z")).toBeTruthy();

    // No branch name anywhere: Preview carries none, so none is invented.
    expect(screen.queryByText(/branch/i)).toBeNull();
  });

  it("names the CI-step cause for a preview with no artifact, and shows no revision or hosts it wasn't given", async () => {
    renderDetail("9", { ...CONFIGURED, previews: PREVIEWS });

    expect(await screen.findByText("Preview pr9")).toBeTruthy();
    expect(screen.getByText(/OCIArtifactPullFailed: failed to pull artifact/)).toBeTruthy();
    expect(screen.getByText(/No artifact for this commit/)).toBeTruthy();
    expect(screen.getByText(/no hostnames/)).toBeTruthy();
    // revision is "" on this preview, so the row is withheld rather than shown empty.
    expect(screen.queryByText("applied revision")).toBeNull();
    // No forge URL was configured to derive from here — it was, so check the
    // negative on a preview whose id would build a real one but is untested:
    // the important thing is that a *missing* revision produces no bare "".
  });

  it("says a preview is not currently reported, distinctly from a preview that never existed", async () => {
    renderDetail("999", { ...CONFIGURED, previews: PREVIEWS });

    expect(
      await screen.findByText(/No preview pr999 is currently reported for staging/),
    ).toBeTruthy();
    // The lifecycle line still renders, so a reader can tell the poller apart
    // from a change request that simply isn't labelled.
    expect(
      screen.getByText(/polling the forge and instantiating one OCIRepository/),
    ).toBeTruthy();
    // Nothing invents a detail section for a preview that was not found — no
    // forge link, no phase pill, no namespace.
    expect(screen.queryByRole("link", { name: "View pull request" })).toBeNull();
    expect(screen.queryByText("checkout-staging-pr412")).toBeNull();
  });

  it("says the environment declares no previews at all, rather than pretending one might exist", async () => {
    renderDetail("412", {
      project: "checkout",
      environment: "staging",
      namespace: "checkout-staging",
      mode: "direct",
    });

    expect(
      await screen.findByText("This environment declares no previews"),
    ).toBeTruthy();
    expect(screen.getByText(/pull request 412 was never a candidate/)).toBeTruthy();
  });

  it("shows the server's own render/previews-require-flux, code and fix intact", async () => {
    renderDetail("412", {
      ...CONFIGURED,
      mode: "direct",
      errors: [
        {
          code: "render/previews-require-flux",
          resource: "",
          field: "",
          application: "",
          overlay: "",
          target: "",
          message:
            'environment "staging" declares previews, which render a flux-operator ResourceSet and ResourceSetInputProvider, but its delivery mode is "direct"',
          remediation:
            "set delivery.mode: flux on this environment, or remove the previews block",
          docsUrl: "",
          line: 0,
          column: 0,
          cause: "",
        },
      ],
    });

    expect(await screen.findByText("render/previews-require-flux")).toBeTruthy();
    expect(screen.getByText(/set delivery.mode: flux/)).toBeTruthy();
  });

  it("reports a transport failure as one, rather than an empty preview", async () => {
    const transport = createRouterTransport((router) => {
      router.service(PreviewService, {
        listPreviews: () => {
          throw new ConnectError("no usable cluster credentials", Code.Unavailable);
        },
      });
    });
    renderAt(
      transport,
      "/projects/checkout/staging/previews/412",
      "/projects/:project/:env/previews/:pr",
      <PreviewDetailPage />,
    );

    expect(
      await screen.findByText("Could not read this environment's previews"),
    ).toBeTruthy();
    expect(screen.getByText(/no usable cluster credentials/)).toBeTruthy();
  });

  it("reads the pr, project and environment from the route rather than from props", async () => {
    // Two different environments answer differently; the route alone decides
    // which ListPreviews call is made and which preview is picked out of it.
    const transport = createRouterTransport((router) => {
      router.service(PreviewService, {
        listPreviews: (req) => {
          if (req.environment === "production") {
            return { ...CONFIGURED, environment: "production", previews: [] };
          }
          return { ...CONFIGURED, previews: PREVIEWS };
        },
      });
    });

    renderAt(
      transport,
      "/projects/checkout/production/previews/412",
      "/projects/:project/:env/previews/:pr",
      <PreviewDetailPage />,
    );

    // production has no previews in this stub, so 412 is not found there —
    // proof the environment segment actually drove the call.
    expect(
      await screen.findByText(/No preview pr412 is currently reported for production/),
    ).toBeTruthy();
  });
});
