import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";

import { clients as defaultClients, type Clients } from "./clients";

/**
 * The data layer: which clients a screen talks to, and one hook for reading.
 *
 * The clients themselves are built once in ./clients over the shared transport.
 * This module only adds the two things every screen needs — a way to substitute
 * that transport (tests hand in a createRouterTransport one) and a load-state
 * shape that does not lie during a refetch.
 */

const ClientsContext = createContext<Clients>(defaultClients);

export function ClientsProvider({
  clients,
  children,
}: {
  clients: Clients;
  children: ReactNode;
}) {
  return (
    <ClientsContext.Provider value={clients}>{children}</ClientsContext.Provider>
  );
}

export function useClients(): Clients {
  return useContext(ClientsContext);
}

export interface Async<T> {
  loading: boolean;
  /** Kept across a refetch: the previous answer is stale, not absent. */
  data: T | undefined;
  error: unknown;
  reload: () => void;
}

/**
 * Run an aborting async read, keyed by `deps`.
 *
 * Stale-while-refetch: navigating to a new key leaves the old data in place
 * with `loading` true, so a screen re-renders its content rather than flashing
 * back to a spinner. `error` is cleared on each attempt — a stale error would
 * be a claim about a request that is no longer in flight.
 */
export function useAsync<T>(
  run: (signal: AbortSignal) => Promise<T>,
  deps: readonly unknown[],
): Async<T> {
  const [state, setState] = useState<{ data: T | undefined; error: unknown }>({
    data: undefined,
    error: undefined,
  });
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);
  const latest = useRef(run);
  latest.current = run;

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setState((prev) => ({ data: prev.data, error: undefined }));
    latest
      .current(controller.signal)
      .then((data) => {
        if (controller.signal.aborted) return;
        setState({ data, error: undefined });
        setLoading(false);
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted) return;
        setState((prev) => ({ data: prev.data, error }));
        setLoading(false);
      });
    return () => controller.abort();
    // The caller owns the key: `deps` is spread so a screen re-reads when its
    // project or environment changes, and `nonce` is what reload() bumps.
  }, [...deps, nonce]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  return { loading, data: state.data, error: state.error, reload };
}
