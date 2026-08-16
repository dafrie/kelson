import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";

import {
  PreviewService,
  type ListPreviewsResponseSchema,
} from "../gen/kelson/v1alpha1/preview_pb";
import { renderAt } from "../test/render";
import { Previews } from "./Previews";

/**
 * The panel against a stub PreviewService — the real generated client, the real
 * serialisation, a server that is not there.
 *
 * The four cases below are the four this section exists to keep apart, and they
 * are the reason it does not simply print a list: an environment that declares
 * no previews, one the renderer refuses, one whose cluster has no
 * flux-operator, and one with previews that are actually running.
 */

type Response = MessageInitShape<typeof ListPreviewsResponseSchema>;

const CONFIGURED: Response = {
  project: "checkout",
  environment: "staging",
  namespace: "checkout-staging",
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
};

function renderPanel(response: Response) {
  const transport = createRouterTransport((router) => {
    router.service(PreviewService, { listPreviews: () => response });
  });
  renderAt(
    transport,
    "/",
    "/",
    <Previews project="checkout" environment="staging" />,
  );
}

describe("Previews", () => {
  it("explains how previews come to exist when the environment declares none", async () => {
    renderPanel({
      project: "checkout",
      environment: "staging",
      namespace: "checkout-staging",
    });

    // Both halves, because a spec block alone gets a reader the stage-1
    // experience: flux-operator finding change requests nothing published for.
    expect(await screen.findByText(/spawns no per-pull-request children/)).toBeTruthy();
    expect(screen.getByText(/Two halves have to be in place/)).toBeTruthy();
    expect(screen.getByText(/kelson preview publish/)).toBeTruthy();
    expect(
      screen.getByRole("link", { name: /Previews: a child environment/ }),
    ).toBeTruthy();
  });

  it("shows a structured refusal the server sent, code and fix intact", async () => {
    renderPanel({
      ...CONFIGURED,
      errors: [
        {
          code: "render/preview-name-too-long",
          resource: "",
          field: "",
          application: "",
          overlay: "",
          target: "",
          message:
            'previews name every child "checkout-staging-pr<change request>", and "checkout-staging" is already 47 characters',
          remediation:
            "shorten the project or environment name so that <project>-<environment> is at most 45 characters",
          docsUrl: "",
          line: 0,
          column: 0,
          cause: "",
        },
      ],
    });

    expect(await screen.findByText("render/preview-name-too-long")).toBeTruthy();
    expect(screen.getByText(/is already 47 characters/)).toBeTruthy();
    expect(screen.getByText(/shorten the project or environment name/)).toBeTruthy();
    // The configuration is still shown: a reader whose spec was refused needs
    // to see what they configured.
    expect(screen.getByText(/Polling https:\/\/github.com\/acme\/checkout/)).toBeTruthy();
  });

  it("says flux-operator is missing rather than reporting no previews", async () => {
    renderPanel({
      ...CONFIGURED,
      lifecycle: {
        name: "checkout-staging-previews",
        served: false,
        present: false,
        providerReady: "",
        providerReason: "",
        providerMessage: "",
        setReady: "",
        setReason: "",
        setMessage: "",
      },
    });

    expect(await screen.findByText(/flux-operator is not installed/)).toBeTruthy();
    // An empty list under a missing operator must not read as "nothing is
    // wrong", so the no-previews note is withheld.
    expect(screen.queryByText(/No change request has a preview/)).toBeNull();
  });

  it("lists each change request with its commit, namespace, hostnames and age", async () => {
    renderPanel({
      ...CONFIGURED,
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
      previews: [
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
      ],
    });

    expect(await screen.findByText("Previews (2)")).toBeTruthy();

    // The change request chip links to the forge, derived from the provider and
    // the source repository the spec already names.
    expect(
      screen.getByRole("link", { name: "pr412" }).getAttribute("href"),
    ).toBe("https://github.com/acme/checkout/pull/412");

    // Each row also links to its own detail page — the route a commit status
    // and a PR comment link to (ADR-0017 stage 3, #248) — distinct from the
    // forge link above, which leaves kelson entirely.
    const detailLinks = screen
      .getAllByRole("link", { name: "details →" })
      .map((a) => a.getAttribute("href"));
    expect(detailLinks).toContain("/projects/checkout/staging/previews/412");
    expect(detailLinks).toContain("/projects/checkout/staging/previews/9");

    // The commit, abbreviated for reading and whole for copying: the tag flux
    // pins is the full commit.
    expect(screen.getByText("0123456789ab")).toBeTruthy();
    expect(screen.getByText("checkout-staging-pr412")).toBeTruthy();
    expect(screen.getByText("2h")).toBeTruthy();

    // The hostname carries the change request in its first label — a preview
    // never serves the parent's hostname.
    const host = screen.getByRole("link", { name: "web-pr412.staging.acme.run" });
    expect(host.getAttribute("href")).toBe("https://web-pr412.staging.acme.run");

    // And the unpublished one names its cause rather than reading as a failure
    // of the manifests.
    expect(screen.getByText(/No manifests for this commit yet/)).toBeTruthy();
    expect(
      screen.getByText(/OCIArtifactPullFailed: failed to pull artifact/),
    ).toBeTruthy();
  });

  it("offers no control that would create or destroy a preview", async () => {
    renderPanel({
      ...CONFIGURED,
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
    });

    await screen.findByText(/Polling https:\/\/github.com\/acme\/checkout/);
    // Previews are published by CI and torn down by flux-operator, so the only
    // buttons on this panel are the copy affordances.
    for (const button of screen.queryAllByRole("button")) {
      expect(button.className).toContain("k-copy");
    }
  });
});
