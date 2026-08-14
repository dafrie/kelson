import {
  Code,
  ConnectError,
  createClient,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import { notifyUnauthenticated } from "./auth";

import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { RenderService } from "../gen/kelson/v1alpha1/render_pb";
import { ProfileService } from "../gen/kelson/v1alpha1/profile_pb";
import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { LogService } from "../gen/kelson/v1alpha1/logs_pb";
import { EventService } from "../gen/kelson/v1alpha1/events_pb";
import { BuildService } from "../gen/kelson/v1alpha1/build_pb";
import { SecretService } from "../gen/kelson/v1alpha1/secret_pb";

/**
 * Clients for the eight kelson services.
 *
 * Connect-ES v2 needs no per-service generated client: `createClient` takes the
 * service descriptor protoc-gen-es emits and produces the typed client from it.
 * That is why buf.gen.yaml lists protoc-gen-es alone on the TypeScript side —
 * there is no protoc-gen-connect-es counterpart to protoc-gen-connect-go.
 */

/**
 * Report a 401 to the auth layer, then let it fall through unchanged.
 *
 * A server started with a password (#84's interim cut) refuses an RPC without a
 * session as ConnectRPC `unauthenticated`, and it can start doing so at any
 * moment — a restart mints a new signing key, so every open tab's cookie dies
 * with the old process (docs/server.md). The screens keep receiving the error
 * they always did; this only tells the session state that it is stale, which is
 * what turns a mid-edit expiry into a login screen instead of an error panel.
 *
 * The catch is around the call rather than around the stream body because
 * connect-web validates the response status before it hands back a stream, so a
 * 401 on a deploy or a log follow arrives here too.
 */
const reportUnauthenticated: Interceptor = (next) => async (req) => {
  try {
    return await next(req);
  } catch (error) {
    if (error instanceof ConnectError && error.code === Code.Unauthenticated) {
      notifyUnauthenticated();
    }
    throw error;
  }
};

/**
 * The default transport talks to the origin the UI is served from.
 *
 * In production that is kelson-server itself. In development it is the Vite dev
 * server, whose proxy (see vite.config.ts) forwards `/kelson.v1alpha1.*`,
 * `/auth/` and `/healthz` to 127.0.0.1:8420. Either way the request is
 * same-origin, which is what lets kelson-server ship without CORS handling —
 * and what lets the session cookie ride along at all, since `SameSite=Lax`
 * would not survive a cross-site call.
 */
export const defaultTransport: Transport = createConnectTransport({
  baseUrl: "/",
  interceptors: [reportUnauthenticated],
  // Same-origin is fetch's default. It is stated because the session cookie is
  // the whole mechanism and a silent change here would look like a server bug.
  fetch: (input, init) =>
    globalThis.fetch(input, { credentials: "same-origin", ...init }),
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
    secret: createClient(SecretService, transport),
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
