import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { EventService } from "../gen/kelson/v1alpha1/events_pb";
import { PreviewService } from "../gen/kelson/v1alpha1/preview_pb";
import { SecretService } from "../gen/kelson/v1alpha1/secret_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt, renderRoutes } from "../test/render";
import { environmentRoutes } from "../test/routes";
import { transitionEvent, healthEvent, watchStub } from "../test/watch";
import { ProjectsPage } from "./ProjectsPage";

/**
 * The reconciliation ticker, on the two screens that carry one (#260).
 *
 * `live/Ticker.test.tsx` pins what a strip looks like. These pin where it is
 * fed from and what it is a claim about, which are the two things a component
 * test cannot see:
 *
 *  1. **The environment's ticker is the environment's.** It reads the one
 *     stream the Overview already opened and shows that pair's transitions and
 *     no others — the claim is about the environment in the URL, not about
 *     whatever the transport happened to deliver.
 *  2. **Home's is the instance's.** One stream over every watched pair, one
 *     strip, and every row says which pair it is about.
 *
 * Both are absent until something happens, which is the rule that makes the
 * strip worth having: on a quiet instance there is nothing there to learn to
 * ignore.
 */

const PROJECT_YAML = `kind: Project
metadata:
  name: checkout

spec:
  components:
    - name: web
      port: 8080
`;

const ENV_YAML = "kind: Environment\nmetadata:\n  name: production\n";

/** A stamp the strip will print as `now` rather than as a count of days. */
function justNow(): number {
  return Date.now();
}

function environmentServer(events: ReturnType<typeof watchStub>) {
  return createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => ({
        spec: {
          project: "checkout",
          version: "7",
          environments: ["production", "staging"],
          documents: {
            project: new TextEncoder().encode(PROJECT_YAML),
            environments: { production: new TextEncoder().encode(ENV_YAML) },
          },
        },
      }),
    });
    router.service(SecretService, { listSecrets: () => ({ secrets: [] }) });
    router.service(PreviewService, {
      listPreviews: () => ({ project: "checkout", environment: "production" }),
    });
    router.service(DeployService, {
      status: () => ({
        phase: "Healthy",
        answer: "live",
        revision: "44-1a2b3c4d",
        observedRevision: "44-1a2b3c4d",
        namespace: "checkout-production",
        verdicts: [],
      }),
    });
    events.install(router);
  });
}

function homeServer(events: ReturnType<typeof watchStub>) {
  return createRouterTransport((router) => {
    router.service(SpecService, {
      listSpecs: () => ({
        specs: [
          {
            project: "checkout",
            version: "7",
            environments: ["production", "staging"],
          },
        ],
      }),
    });
    router.service(DeployService, {
      status: () => ({
        phase: "Healthy",
        answer: "live",
        revision: "44-1a2b3c4d",
        namespace: "checkout-production",
      }),
    });
    events.install(router);
  });
}

/** The strip's rows, as the strings a reader sees on each one. */
function ticks(strip: HTMLElement): string[] {
  return within(strip)
    .getAllByRole("listitem")
    .map((li) =>
      [...li.children]
        .map((el) => el.textContent ?? "")
        .filter((s) => s !== "")
        .join(" · "),
    );
}

describe("the environment's ticker", () => {
  it("is absent until the stream has said something", async () => {
    const events = watchStub([]);
    renderRoutes(
      environmentServer(events),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    expect(await screen.findByText("live", { selector: ".k-pill" })).toBeTruthy();
    await screen.findByText("streaming");
    // Quiet when healthy: the strip is nothing at all, not an empty box.
    expect(screen.queryByLabelText(/^Activity/)).toBeNull();
  });

  it("draws the transitions it is sent, newest first, without the subject", async () => {
    const events = watchStub([
      transitionEvent({
        cursor: "nonce.1",
        atUnixMs: justNow(),
        project: "checkout",
        environment: "production",
        phase: "Committed",
        previousPhase: "Proposed",
        revision: "45-9e8d7c6b",
      }),
    ]);
    renderRoutes(
      environmentServer(events),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    const strip = await screen.findByLabelText("Activity in production");
    await waitFor(() =>
      expect(ticks(strip)).toEqual([
        "now · waiting · Proposed → Committed · 45-9e8d7c6b",
      ]),
    );

    events.push(
      transitionEvent({
        cursor: "nonce.2",
        atUnixMs: justNow(),
        project: "checkout",
        environment: "production",
        phase: "Rejected",
        previousPhase: "Committed",
        revision: "45-9e8d7c6b",
      }),
    );

    await waitFor(() =>
      expect(ticks(strip)).toEqual([
        "now · failed · Committed → Rejected · 45-9e8d7c6b",
        "now · waiting · Proposed → Committed · 45-9e8d7c6b",
      ]),
    );
    // The failure is the only coloured word in the strip.
    const tones = [...strip.querySelectorAll(".k-tick__word")].map(
      (el) => (el as HTMLElement).dataset.tone,
    );
    expect(tones).toEqual(["failed", undefined]);
  });

  it("shows this environment's transitions and no other environment's", async () => {
    const events = watchStub([
      transitionEvent({
        cursor: "nonce.1",
        atUnixMs: justNow(),
        project: "checkout",
        environment: "staging",
        phase: "Rejected",
        previousPhase: "Committed",
        revision: "99-deadbeef",
      }),
    ]);
    renderRoutes(
      environmentServer(events),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    await screen.findByText("streaming");
    events.push(
      transitionEvent({
        cursor: "nonce.2",
        atUnixMs: justNow(),
        project: "checkout",
        environment: "production",
        phase: "Healthy",
        previousPhase: "Applied",
        revision: "45-9e8d7c6b",
      }),
    );

    const strip = await screen.findByLabelText("Activity in production");
    await waitFor(() =>
      expect(ticks(strip)).toEqual([
        "now · live · Applied → Healthy · 45-9e8d7c6b",
      ]),
    );
    // The strip is a claim about the environment in the URL, so staging's
    // rejection is not on it however the transport chose to deliver it.
    expect(screen.queryByText(/99-deadbeef/)).toBeNull();
  });

  it("keeps a health change out of the strip", async () => {
    const events = watchStub([
      healthEvent({
        cursor: "nonce.1",
        project: "checkout",
        environment: "production",
        resource: "Deployment/checkout-production/web",
        code: "crash-loop-back-off",
        message: "web is restarting repeatedly (7 restarts)",
      }),
    ]);
    renderRoutes(
      environmentServer(events),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    // It moved the verdict list, which is where a health change belongs.
    expect(
      await screen.findByText("crash-loop-back-off", { selector: ".k-pill" }),
    ).toBeTruthy();
    expect(screen.queryByLabelText(/^Activity/)).toBeNull();
  });

  it("says the strip may be incomplete once the stream drops", async () => {
    let opens = 0;
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production"],
            documents: {
              project: new TextEncoder().encode(PROJECT_YAML),
              environments: { production: new TextEncoder().encode(ENV_YAML) },
            },
          },
        }),
      });
      router.service(SecretService, { listSecrets: () => ({ secrets: [] }) });
      router.service(PreviewService, {
        listPreviews: () => ({ project: "checkout", environment: "production" }),
      });
      router.service(DeployService, {
        status: () => ({ phase: "Healthy", answer: "live", revision: "44-1a2b3c4d" }),
      });
      // One transition, then a transport failure that every retry repeats: the
      // row survives and the strip says it can no longer vouch for being whole.
      router.service(EventService, {
        watch: async function* () {
          opens += 1;
          if (opens === 1) {
            const message = transitionEvent({
              cursor: "nonce.1",
              atUnixMs: justNow(),
              project: "checkout",
              environment: "production",
              phase: "Healthy",
              previousPhase: "Applied",
            });
            if (message.body.case === "event") yield message;
          }
          throw new ConnectError("the connection dropped", Code.Unavailable);
        },
      });
    });
    renderRoutes(transport, "/projects/checkout/production", environmentRoutes());

    const strip = await screen.findByLabelText("Activity in production");
    await waitFor(() =>
      expect(ticks(strip)).toEqual(["now · live · Applied → Healthy"]),
    );
    // The transport's own affordance, plus the one thing it cannot say.
    expect(await screen.findByText("reconnecting…")).toBeTruthy();
    expect(
      within(strip).getByText("reconnecting — rows may be missing"),
    ).toBeTruthy();
    // A row is a transition that really happened; a dropped stream does not
    // make it false, only incomplete.
    expect(ticks(strip)).toEqual(["now · live · Applied → Healthy"]);
  });
});

describe("home's ticker", () => {
  it("is absent until the stream has said something", async () => {
    const events = watchStub([]);
    renderAt(homeServer(events), "/projects", "/projects", <ProjectsPage />);

    await screen.findByText("streaming");
    expect(screen.queryByLabelText("Activity")).toBeNull();
  });

  it("is instance-wide, and every row says which pair it is about", async () => {
    const events = watchStub([
      transitionEvent({
        cursor: "nonce.1",
        atUnixMs: justNow(),
        project: "checkout",
        environment: "production",
        phase: "Reconciling",
        previousPhase: "Committed",
        revision: "45-9e8d7c6b",
      }),
    ]);
    renderAt(homeServer(events), "/projects", "/projects", <ProjectsPage />);

    const strip = await screen.findByLabelText("Activity");
    events.push(
      transitionEvent({
        cursor: "nonce.2",
        atUnixMs: justNow(),
        project: "checkout",
        environment: "staging",
        phase: "Degraded",
        previousPhase: "Applied",
        revision: "12-0badcafe",
      }),
    );

    await waitFor(() =>
      expect(ticks(strip)).toEqual([
        "now · checkout · staging · unhealthy · Applied → Degraded · 12-0badcafe",
        "now · checkout · production · deploying · Committed → Reconciling · 45-9e8d7c6b",
      ]),
    );
    // The subject is the way to the environment it names.
    expect(
      within(strip)
        .getByRole("link", { name: "checkout · staging" })
        .getAttribute("href"),
    ).toBe("/projects/checkout/staging");
  });

  it("stays short, however much the ring is holding", async () => {
    const events = watchStub([]);
    renderAt(homeServer(events), "/projects", "/projects", <ProjectsPage />);
    await screen.findByText("streaming");

    for (let n = 0; n < 9; n += 1) {
      events.push(
        transitionEvent({
          cursor: `nonce.${n}`,
          atUnixMs: justNow(),
          project: "checkout",
          environment: "production",
          phase: "Healthy",
          previousPhase: "Applied",
          revision: `${n}-0badcafe`,
        }),
      );
    }

    const strip = await screen.findByLabelText("Activity");
    await waitFor(() => expect(ticks(strip)).toHaveLength(5));
    // The newest five of the nine: home is the ambient view, not the record.
    expect(ticks(strip)[0]).toContain("8-0badcafe");
    expect(ticks(strip)[4]).toContain("4-0badcafe");
  });
});
