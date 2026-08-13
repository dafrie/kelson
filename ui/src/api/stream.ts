import { useCallback, useEffect, useRef, useState } from "react";

import { isAbort } from "./errors";

/**
 * Running one abortable operation — a server stream, or any call the user
 * starts by pressing a button.
 *
 * Three rules the deploy, rollback and log screens all need:
 *
 *   - No client-side timeout. A deployment that takes eight minutes to settle
 *     is a deployment, not a hang; the server owns the budget (DeployRequest
 *     .timeout_seconds, default 5m) and gives up by *sending* a settled event.
 *     A browser-side timer would abandon the stream and lose the answer.
 *   - Navigating away aborts. The AbortController is torn down on unmount, so a
 *     `for await` loop over a stream stops at the next chunk and the request is
 *     cancelled rather than left writing into an unmounted component.
 *   - An abort is not an error. Cancelling is something we did, and reporting
 *     it as a failure would put a red panel on a screen the user just left.
 */
export interface Run {
  running: boolean;
  error: unknown;
  /** Starts fn, aborting anything already in flight. */
  start: (fn: (signal: AbortSignal) => Promise<void>) => void;
  /** Aborts what is in flight, if anything. */
  stop: () => void;
}

export function useRun(): Run {
  const controller = useRef<AbortController | null>(null);
  const mounted = useRef(true);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      controller.current?.abort();
    };
  }, []);

  const start = useCallback((fn: (signal: AbortSignal) => Promise<void>) => {
    controller.current?.abort();
    const next = new AbortController();
    controller.current = next;
    setError(undefined);
    setRunning(true);
    fn(next.signal)
      .then(() => {
        if (!mounted.current || next.signal.aborted) return;
        setRunning(false);
      })
      .catch((err: unknown) => {
        if (!mounted.current || next.signal.aborted || isAbort(err)) return;
        setError(err);
        setRunning(false);
      });
  }, []);

  const stop = useCallback(() => {
    controller.current?.abort();
    if (mounted.current) setRunning(false);
  }, []);

  return { running, error, start, stop };
}
