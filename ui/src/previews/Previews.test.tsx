import { beforeEach, describe, expect, it } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";

import { setDetail } from "../expert/preference";
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

beforeEach(() => setDetail(false));

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

/**
 * Kubernetes detail on the previews section (#268 item 3, following #260).
 * `Preview.namespace` is documented as also naming the OCIRepository and the
 * Kustomization it is read from, and `PreviewLifecycle.name` is documented as
 * the one name the ResourceSetInputProvider and the ResourceSet share.
 */
const WITH_PREVIEWS: Response = {
  ...CONFIGURED,
  lifecycle: {
    name: "checkout-staging-previews",
    served: true,
    present: true,
    providerReady: "True",
    providerReason: "",
    providerMessage: "",
    setReady: "False",
    setReason: "ResourceSetProvisioning",
    setMessage: "waiting for the input provider's first poll",
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
  ],
};

describe("Kubernetes detail on the previews section", () => {
  it("adds no facts and no carets when the preference is off", async () => {
    renderPanel(WITH_PREVIEWS);
    await screen.findByText("Previews (1)");

    expect(document.querySelector(".k-why")).toBeNull();
    expect(document.querySelector(".k-kfact")).toBeNull();
    expect(screen.queryByText("OCIRepository")).toBeNull();
    expect(screen.queryByText("ResourceSet")).toBeNull();
    // The namespace itself is on screen, as it always was.
    expect(screen.getByText("checkout-staging-pr412")).toBeTruthy();
  });

  it("names the OCIRepository and Kustomization a preview's namespace also is", async () => {
    setDetail(true);
    renderPanel(WITH_PREVIEWS);
    await screen.findByText("Previews (1)");

    expect(screen.getByText("OCIRepository")).toBeTruthy();
    expect(screen.getByText("Kustomization")).toBeTruthy();
    // Both facts carry the same wire value, because the proto documents one
    // namespace naming both objects — nothing here invents a second string.
    expect(
      screen.getAllByText("checkout-staging-pr412").length,
    ).toBeGreaterThan(1);
  });

  it("names the ResourceSetInputProvider and ResourceSet, and attaches evidence to the lifecycle sentence", async () => {
    setDetail(true);
    renderPanel(WITH_PREVIEWS);
    await screen.findByText("Previews (1)");

    expect(screen.getByText("ResourceSetInputProvider")).toBeTruthy();
    expect(screen.getByText("ResourceSet")).toBeTruthy();
    expect(
      screen.getAllByText("checkout-staging-previews").length,
    ).toBeGreaterThan(1);

    // The lifecycle sentence names the ResourceSet's own Ready condition
    // (setReady is False here); the caret shows both conditions' reasons,
    // verbatim.
    const lifecycleText = "The preview environments are not ready: ResourceSetProvisioning: waiting for the input provider's first poll";
    expect(screen.getByLabelText(`Evidence for “${lifecycleText}”`)).toBeTruthy();
    expect(screen.getByText("set reason")).toBeTruthy();
    expect(screen.getByText("ResourceSetProvisioning")).toBeTruthy();
    expect(screen.getByText("provider ready")).toBeTruthy();
  });

  it("attaches evidence to a preview's status word showing both Ready conditions verbatim", async () => {
    setDetail(true);
    renderPanel(WITH_PREVIEWS);
    await screen.findByText("Previews (1)");

    expect(screen.getByLabelText("Evidence for “live”")).toBeTruthy();
    expect(screen.getByText("artifact ready")).toBeTruthy();
    expect(screen.getByText("applied ready")).toBeTruthy();
    expect(screen.getAllByText("True").length).toBeGreaterThanOrEqual(2);
  });

  it("keeps the could-not-check lifecycle state visible in both modes", async () => {
    const notServed: Response = {
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
    };

    renderPanel(notServed);
    expect(await screen.findByText(/flux-operator is not installed/)).toBeTruthy();
    cleanup();

    setDetail(true);
    renderPanel(notServed);
    expect(await screen.findByText(/flux-operator is not installed/)).toBeTruthy();
  });

  it("renders the same action links whether the preference is on or off", async () => {
    renderPanel(WITH_PREVIEWS);
    await screen.findByText("Previews (1)");
    const normal = screen
      .getAllByRole("link")
      .map((a) => `${a.textContent} → ${a.getAttribute("href")}`)
      .sort();
    cleanup();

    setDetail(true);
    renderPanel(WITH_PREVIEWS);
    await screen.findByText("Previews (1)");
    const expert = screen
      .getAllByRole("link")
      .map((a) => `${a.textContent} → ${a.getAttribute("href")}`)
      .sort();

    expect(expert).toEqual(normal);
    expect(normal).toContain(
      "pr412 → https://github.com/acme/checkout/pull/412",
    );
    expect(normal).toContain(
      "details → → /projects/checkout/staging/previews/412",
    );
  });
});
