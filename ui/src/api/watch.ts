import { useEffect, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";

import { useClients } from "./data";
import { isAbort } from "./errors";
import type {
  EventType,
  WatchResponse_Event,
} from "../gen/kelson/v1alpha1/events_pb";

/**
 * One EventService.Watch stream, kept alive for as long as a screen needs it.
 *
 * Screens used to be as fresh as the last time someone reloaded them. This is
 * the other half of that: a card that says "reconciling" becomes a card that
 * says "healthy" because the server said so, not because a timer fired
 * (issue #76).
 *
 * Three rules, and they are all about not lying:
 *
 *   - A Resync is a relist, not a reconnect. The server sends one when it
 *     cannot honour a cursor (it restarted, or the event window moved past
 *     it). The stream stays up; what is stale is the screen, so `onResync`
 *     refetches and the cursor is dropped.
 *   - Reconnecting resumes from the last cursor. Within the server's retained
 *     window that replays exactly what was missed; beyond it the server
 *     answers with a Resync, which is the case above.
 *   - Unavailable is not an error to show. A server built without a delivery
 *     plane answers Unimplemented, and a UI that cannot watch is a UI that
 *     works exactly as it did before this feature: the indicator disappears
 *     and nothing else changes. Every other failure is transient and retried
 *     with capped exponential backoff.
 */
export type WatchState = "live" | "reconnecting" | "off";

export interface WatchScope {
  project: string;
  environment: string;
}

export interface WatchOptions {
  /** Empty disables the watch — a stream over no scopes is not worth opening. */
  scopes: WatchScope[];
  onEvent: (event: WatchResponse_Event) => void;
  /** The window could not be honoured: refetch, then keep reading. */
  onResync: () => void;
  types?: EventType[];
}

const RETRY_BASE_MS = 1_000;
/** Cap the backoff: a tab left open overnight should still recover promptly. */
const RETRY_CAP_MS = 30_000;

export function useWatch({
  scopes,
  onEvent,
  onResync,
  types,
}: WatchOptions): WatchState {
  const clients = useClients();
  const [state, setState] = useState<WatchState>("off");

  // The effect restarts on the scope set alone. Everything else it needs is
  // read through a ref at call time, so a parent re-rendering (which it does
  // on every event) cannot tear down and reopen the stream.
  const key = scopes
    .map((s) => `${s.project}/${s.environment}`)
    .sort()
    .join(",");
  const latest = useRef({ scopes, onEvent, onResync, types });
  latest.current = { scopes, onEvent, onResync, types };
  const cursor = useRef("");

  useEffect(() => {
    if (key === "") {
      setState("off");
      return;
    }
    const controller = new AbortController();
    let stopped = false;
    let failed = false;
    let attempt = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const read = async () => {
      // The first attempt reports live optimistically: a stream sends nothing
      // until something changes, so "connected" is not observable and a
      // "connecting…" that never resolves would be worse than an optimism a
      // failed request corrects a round trip later. After a failure the
      // indicator stays honest — only a message proves the stream is back.
      if (!failed) setState("live");
      for await (const res of clients.event.watch(
        {
          scopes: latest.current.scopes,
          // No types is every type, which the schema spells as an empty list.
          types: latest.current.types ?? [],
          cursor: cursor.current,
        },
        { signal: controller.signal },
      )) {
        failed = false;
        attempt = 0;
        setState("live");
        const body = res.body;
        if (body.case === "event") {
          cursor.current = body.value.cursor;
          latest.current.onEvent(body.value);
        } else if (body.case === "resync") {
          // The cursor is the thing that could not be honoured; keeping it
          // would ask the same impossible question on every reconnect.
          cursor.current = "";
          latest.current.onResync();
        }
      }
    };

    const retry = () => {
      if (stopped) return;
      failed = true;
      setState("reconnecting");
      const delay = Math.min(RETRY_CAP_MS, RETRY_BASE_MS * 2 ** attempt);
      attempt += 1;
      timer = setTimeout(run, delay);
    };

    const run = () => {
      read()
        .then(retry) // A stream that ended cleanly is a server that went away.
        .catch((err: unknown) => {
          if (stopped || controller.signal.aborted || isAbort(err)) return;
          if (ConnectError.from(err).code === Code.Unimplemented) {
            // This build cannot watch. Fall back silently to the static
            // behaviour rather than blinking "reconnecting…" forever.
            stopped = true;
            setState("off");
            return;
          }
          retry();
        });
    };
    run();

    return () => {
      stopped = true;
      if (timer !== undefined) clearTimeout(timer);
      controller.abort();
    };
  }, [clients, key]);

  return state;
}
