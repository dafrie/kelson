import { describe, expect, it } from "vitest";
import { createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import { createClients } from "./clients";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { RenderService } from "../gen/kelson/v1alpha1/render_pb";
import { ProfileService } from "../gen/kelson/v1alpha1/profile_pb";
import {
  DeployResponseSchema,
  DeployService,
} from "../gen/kelson/v1alpha1/deploy_pb";
import { LogService } from "../gen/kelson/v1alpha1/logs_pb";
import {
  EventService,
  WatchResponseSchema,
} from "../gen/kelson/v1alpha1/events_pb";

/**
 * Proves the generated schemas and the client wiring actually fit together, for
 * all six services, without a server.
 *
 * This is deliberately a wiring test, not a behaviour test: the value is that
 * it fails to compile — or fails to route — the moment protoc-gen-es output,
 * @connectrpc/connect and src/api/clients.ts stop agreeing. `buf generate` is
 * not drift-tested in CI (see buf.gen.yaml), so this is the check that catches
 * a stale ui/src/gen after a proto change.
 */

const transport = createRouterTransport((router) => {
  router.service(SpecService, {
    listSpecs: () => ({
      specs: [
        { project: "checkout", version: "42", environments: ["production"] },
      ],
    }),
    getSpec: (req) => ({ spec: { project: req.project, version: "42" } }),
  });

  router.service(RenderService, {
    render: () => ({
      manifests: [{ apiVersion: "apps/v1", kind: "Deployment", name: "web" }],
    }),
  });

  router.service(ProfileService, {
    getProfile: () => ({
      yaml: new TextEncoder().encode("gatewayAPI: true\n"),
      gaps: [{ field: "ingressClass", reason: "RBAC denied list" }],
    }),
  });

  router.service(DeployService, {
    status: () => ({ phase: "settled", revision: "7" }),
    deploy: async function* () {
      yield create(DeployResponseSchema, {
        event: {
          case: "settled",
          value: { final: { phase: "settled" } },
        },
      });
    },
  });

  router.service(LogService, {
    queryLogs: () => ({
      lines: [{ pod: "web-0", container: "web", message: "listening" }],
    }),
  });

  router.service(EventService, {
    watch: async function* () {
      yield create(WatchResponseSchema, {
        body: {
          case: "event",
          value: {
            cursor: "nonce.1",
            project: "checkout",
            environment: "production",
            payload: {
              case: "statusTransition",
              value: { phase: "Healthy", previousPhase: "Reconciling" },
            },
          },
        },
      });
      yield create(WatchResponseSchema, {
        body: { case: "resync", value: { reason: "the window moved" } },
      });
    },
  });
});

const clients = createClients(transport);

describe("generated clients", () => {
  it("round-trips SpecService.ListSpecs", async () => {
    const res = await clients.spec.listSpecs({});
    expect(res.specs.map((s) => s.project)).toEqual(["checkout"]);
    expect(res.specs[0]?.environments).toEqual(["production"]);
  });

  it("passes request fields through on SpecService.GetSpec", async () => {
    const res = await clients.spec.getSpec({ project: "checkout" });
    expect(res.spec?.project).toBe("checkout");
  });

  it("round-trips RenderService.Render", async () => {
    const res = await clients.render.render({ environment: "production" });
    expect(res.manifests[0]?.kind).toBe("Deployment");
  });

  it("round-trips ProfileService.GetProfile, gaps included", async () => {
    const res = await clients.profile.getProfile({});
    expect(new TextDecoder().decode(res.yaml)).toContain("gatewayAPI");
    expect(res.gaps[0]?.field).toBe("ingressClass");
  });

  it("round-trips DeployService.Status", async () => {
    const res = await clients.deploy.status({});
    expect(res.phase).toBe("settled");
  });

  it("consumes DeployService.Deploy as a server stream", async () => {
    const events = [];
    for await (const res of clients.deploy.deploy({})) {
      events.push(res.event.case);
    }
    expect(events).toEqual(["settled"]);
  });

  it("round-trips LogService.QueryLogs", async () => {
    const res = await clients.log.queryLogs({});
    expect(res.lines[0]?.message).toBe("listening");
  });

  it("consumes EventService.Watch as a server stream, both oneof arms", async () => {
    const bodies = [];
    for await (const res of clients.event.watch({})) {
      bodies.push(res.body.case);
      if (res.body.case === "event") {
        expect(res.body.value.payload.case).toBe("statusTransition");
      }
    }
    expect(bodies).toEqual(["event", "resync"]);
  });
});
