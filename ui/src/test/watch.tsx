import { create } from "@bufbuild/protobuf";
import type { ConnectRouter, HandlerContext } from "@connectrpc/connect";

import {
  EventService,
  WatchResponseSchema,
  type WatchResponse,
} from "../gen/kelson/v1alpha1/events_pb";

/**
 * A stubbed EventService.Watch, for the screens that subscribe to one (#76).
 *
 * The stub is a server, not a mock: the messages go through the generated
 * schema and the real client, so a screen that mis-reads the oneof fails here
 * the way it would fail against kelson-server.
 *
 * It stays open after its scripted messages, because that is what a real watch
 * does — a stream that ended would put the screen into "reconnecting…" and make
 * every indicator assertion a race. `aborted` resolves when the client hangs
 * up, which is how the unmount tests observe that the stream was torn down
 * rather than left running.
 */
export interface WatchStub {
  /** Install on a createRouterTransport router. */
  install: (router: ConnectRouter) => void;
  /** Send another message to the connected client. */
  push: (message: WatchResponse) => void;
  /** Resolves once the client aborted the stream. */
  aborted: Promise<void>;
  /** How many times a client opened the stream. */
  opened: () => number;
}

export function watchStub(messages: WatchResponse[] = []): WatchStub {
  let opens = 0;
  let hungUp: () => void = () => {};
  const aborted = new Promise<void>((resolve) => {
    hungUp = resolve;
  });

  // Delivery is one message at a time on the test's schedule. Yielding a whole
  // script at once would collapse into a single React update and make the
  // intermediate states — the very thing a live screen shows — unobservable.
  const queue: WatchResponse[] = [...messages];
  let wake: (() => void) | undefined;

  return {
    opened: () => opens,
    aborted,
    push: (message) => {
      queue.push(message);
      const resume = wake;
      wake = undefined;
      resume?.();
    },
    install: (router) => {
      router.service(EventService, {
        watch: async function* (_req: unknown, ctx: HandlerContext) {
          opens += 1;
          for (;;) {
            while (queue.length > 0) {
              const next = queue.shift();
              if (next !== undefined) yield next;
            }
            if (ctx.signal.aborted) {
              hungUp();
              return;
            }
            await new Promise<void>((resolve) => {
              wake = resolve;
              ctx.signal.addEventListener(
                "abort",
                () => {
                  hungUp();
                  resolve();
                },
                { once: true },
              );
            });
          }
        },
      });
    },
  };
}

/** A STATUS_TRANSITION event on the wire. */
export function transitionEvent(fields: {
  project: string;
  environment: string;
  phase: string;
  previousPhase?: string;
  revision?: string;
  cause?: string;
  cursor?: string;
}): WatchResponse {
  return create(WatchResponseSchema, {
    body: {
      case: "event",
      value: {
        cursor: fields.cursor ?? "nonce.1",
        atUnixMs: BigInt(1_700_000_000_000),
        project: fields.project,
        environment: fields.environment,
        payload: {
          case: "statusTransition",
          value: {
            phase: fields.phase,
            previousPhase: fields.previousPhase ?? "",
            revision: fields.revision ?? "",
            cause: fields.cause ?? "",
          },
        },
      },
    },
  });
}

/** A HEALTH_CHANGE event on the wire. */
export function healthEvent(fields: {
  project: string;
  environment: string;
  resource: string;
  code: string;
  previousCode?: string;
  healthy?: boolean;
  message?: string;
  cursor?: string;
}): WatchResponse {
  return create(WatchResponseSchema, {
    body: {
      case: "event",
      value: {
        cursor: fields.cursor ?? "nonce.2",
        atUnixMs: BigInt(1_700_000_000_000),
        project: fields.project,
        environment: fields.environment,
        payload: {
          case: "healthChange",
          value: {
            resource: fields.resource,
            code: fields.code,
            previousCode: fields.previousCode ?? "",
            healthy: fields.healthy ?? false,
            message: fields.message ?? "",
          },
        },
      },
    },
  });
}

/** A Resync: the server could not honour the resume point. */
export function resyncEvent(reason: string): WatchResponse {
  return create(WatchResponseSchema, {
    body: { case: "resync", value: { reason } },
  });
}
