import { createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { RenderService } from "../gen/kelson/v1alpha1/render_pb";
import { ProfileService } from "../gen/kelson/v1alpha1/profile_pb";
import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { LogService } from "../gen/kelson/v1alpha1/logs_pb";
import { EventService } from "../gen/kelson/v1alpha1/events_pb";
import { BuildService } from "../gen/kelson/v1alpha1/build_pb";

/**
 * Clients for the seven kelson services.
 *
 * Connect-ES v2 needs no per-service generated client: `createClient` takes the
 * service descriptor protoc-gen-es emits and produces the typed client from it.
 * That is why buf.gen.yaml lists protoc-gen-es alone on the TypeScript side —
 * there is no protoc-gen-connect-es counterpart to protoc-gen-connect-go.
 */

/**
 * The default transport talks to the origin the UI is served from.
 *
 * In production that is kelson-server itself. In development it is the Vite dev
 * server, whose proxy (see vite.config.ts) forwards `/kelson.v1alpha1.*` and
 * `/healthz` to 127.0.0.1:8420. Either way the request is same-origin, which is
 * what lets kelson-server ship without CORS handling.
 */
export const defaultTransport: Transport = createConnectTransport({
  baseUrl: "/",
});

export function createClients(transport: Transport) {
  return {
    spec: createClient(SpecService, transport),
    render: createClient(RenderService, transport),
    profile: createClient(ProfileService, transport),
    deploy: createClient(DeployService, transport),
    log: createClient(LogService, transport),
    event: createClient(EventService, transport),
    build: createClient(BuildService, transport),
  };
}

export type Clients = ReturnType<typeof createClients>;

export const clients: Clients = createClients(defaultTransport);

/**
 * `/healthz` is plain JSON on the same mux as the RPC handlers
 * (cmd/kelson-server/main.go), not an RPC, so it is fetched directly. It is the
 * only way to learn the server's build without a cluster round-trip.
 */
export interface Health {
  status: string;
  version: string;
  commit: string;
}

export async function fetchHealth(signal?: AbortSignal): Promise<Health> {
  const res = await fetch("/healthz", signal ? { signal } : {});
  if (!res.ok) {
    throw new Error(`/healthz returned ${res.status} ${res.statusText}`);
  }
  return (await res.json()) as Health;
}
