import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { Navigate, Outlet, useLocation } from "react-router-dom";

/**
 * The session the UI holds, and the tri-state it boots on.
 *
 * kelson-server's authentication is one shared password (#84's interim cut,
 * ADR-0013 §3 as amended 2026-08-13, docs/server.md). `GET /auth/session`
 * answers three things and they are three different facts:
 *
 *   204 No Content   authentication is disabled — there is nothing to log in to
 *   200 {username}   this browser has a session
 *   401              log in
 *
 * The first two would collapse into "not logged in" if the contract had only
 * two states, and this UI would then show a login form no password could
 * satisfy. So "disabled" is a state here too, and in it the UI behaves exactly
 * as it did before authentication existed: no login screen, no user chip,
 * nothing.
 *
 * A username is a display name and not an identity. The server accepts any
 * username and checks only the password; the name exists so the login feels
 * like a login and the header has something to show. Nothing authorizes on it.
 */

export type AuthState =
  | { status: "loading" }
  | { status: "disabled" }
  /** `expired` distinguishes "you were logged in and are not now" from a cold boot. */
  | { status: "anonymous"; expired: boolean }
  | { status: "authenticated"; username: string };

export interface Auth {
  state: AuthState;
  signIn: (username: string, password: string) => Promise<void>;
  signOut: () => Promise<void>;
}

/**
 * The default is "disabled", not "loading".
 *
 * A component rendered outside an [AuthBoundary] — every screen test does this,
 * and so does any future embedding — must behave as it did before this module
 * existed rather than hang on a session request nobody is making.
 */
const AuthContext = createContext<Auth>({
  state: { status: "disabled" },
  signIn: async () => {},
  signOut: async () => {},
});

export function useAuth(): Auth {
  return useContext(AuthContext);
}

/**
 * A 401 from anywhere means the session is gone — the server restarted, or the
 * cookie expired mid-edit.
 *
 * The RPC clients are built once at module load (./clients) and cannot reach
 * React state, so they publish here and the provider subscribes. It is a
 * one-line event bus rather than a store because there is exactly one event and
 * one listener.
 */
type Listener = () => void;
const listeners = new Set<Listener>();

export function notifyUnauthenticated(): void {
  for (const listener of listeners) listener();
}

export function onUnauthenticated(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * `same-origin` is the default, and it is stated anyway: the session cookie is
 * the whole mechanism, and a future change to the transport that silently
 * dropped credentials would look like a server bug for a long time.
 */
async function authFetch(
  path: string,
  init: RequestInit = {},
): Promise<Response> {
  return fetch(path, { credentials: "same-origin", ...init });
}

export type SessionAnswer =
  | { kind: "disabled" }
  | { kind: "anonymous" }
  | { kind: "authenticated"; username: string };

export async function fetchSession(
  signal?: AbortSignal,
): Promise<SessionAnswer> {
  const res = await authFetch("/auth/session", signal ? { signal } : {});
  if (res.status === 204) return { kind: "disabled" };
  if (res.status === 401) return { kind: "anonymous" };
  if (!res.ok) {
    throw new Error(`/auth/session returned ${res.status} ${res.statusText}`);
  }
  const body = (await res.json()) as { username?: string };
  return { kind: "authenticated", username: body.username ?? "" };
}

/** The server's own words for a refused login, or a fallback if it sent none. */
async function refusal(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // A non-JSON body from a proxy in the middle is not the server's message.
  }
  return res.status === 401
    ? "wrong password"
    : `the server refused the login (${res.status})`;
}

export async function postLogin(
  username: string,
  password: string,
): Promise<SessionAnswer> {
  const res = await authFetch("/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
  // The server answers a login against a password-less server with the same
  // 204 it answers /auth/session with, rather than pretending the credentials
  // were wrong.
  if (res.status === 204) return { kind: "disabled" };
  if (!res.ok) throw new Error(await refusal(res));
  const body = (await res.json()) as { username?: string };
  return { kind: "authenticated", username: body.username ?? username };
}

export async function postLogout(): Promise<void> {
  await authFetch("/auth/logout", { method: "POST" });
}

function stateFor(answer: SessionAnswer): AuthState {
  switch (answer.kind) {
    case "authenticated":
      return { status: "authenticated", username: answer.username };
    case "disabled":
      return { status: "disabled" };
    default:
      return { status: "anonymous", expired: false };
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ status: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    fetchSession(controller.signal)
      .then((answer) => {
        if (controller.signal.aborted) return;
        setState(stateFor(answer));
      })
      .catch(() => {
        if (controller.signal.aborted) return;
        // The session endpoint is unreachable, which is not the same as being
        // logged out — the server is down, or this build is served by something
        // that is not kelson-server. Falling to "disabled" lets the screens
        // report the real failure through their own error panels instead of
        // covering it with a login form that could not succeed either. A server
        // that is up and requires a login still gets one: its 401s reach the
        // subscription below.
        setState({ status: "disabled" });
      });
    return () => controller.abort();
  }, []);

  useEffect(
    () =>
      onUnauthenticated(() => {
        // A 401 is proof authentication is on, whatever the boot decided.
        setState((current) => ({
          status: "anonymous",
          expired: current.status === "authenticated",
        }));
      }),
    [],
  );

  const signIn = useCallback(async (username: string, password: string) => {
    setState(stateFor(await postLogin(username, password)));
  }, []);

  const signOut = useCallback(async () => {
    await postLogout();
    setState({ status: "anonymous", expired: false });
  }, []);

  const value = useMemo<Auth>(
    () => ({ state, signIn, signOut }),
    [state, signIn, signOut],
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

/** The route-layout form of [AuthProvider], for App.tsx's route table. */
export function AuthBoundary() {
  return (
    <AuthProvider>
      <Outlet />
    </AuthProvider>
  );
}

/** Where a login should return to, encoded into the login URL. */
export function loginPath(returnTo: string, expired: boolean): string {
  const params = new URLSearchParams();
  if (returnTo && returnTo !== "/") params.set("next", returnTo);
  if (expired) params.set("expired", "1");
  const query = params.toString();
  return query ? `/login?${query}` : "/login";
}

/**
 * Gate the app on a session.
 *
 * The return path travels in the query string rather than in router state so it
 * survives the reload a real session expiry often comes with, and it is read
 * back through [safeReturnPath] because a `next` a user can type is a redirect
 * a user can be handed.
 */
export function RequireSession() {
  const { state } = useAuth();
  const location = useLocation();

  if (state.status === "loading") {
    // Deliberately blank. The session request is one round trip to the same
    // origin; a spinner here would flash on every load and say nothing.
    return null;
  }
  if (state.status === "anonymous") {
    return (
      <Navigate
        to={loginPath(location.pathname + location.search, state.expired)}
        replace
      />
    );
  }
  return <Outlet />;
}

/**
 * A return path must be a path on this origin. Anything else — a scheme, a
 * protocol-relative `//host`, a backslash Windows browsers normalise into one —
 * is dropped for the default landing page.
 */
export function safeReturnPath(next: string | null): string {
  if (!next || !next.startsWith("/") || next.startsWith("//") || next.startsWith("/\\")) {
    return "/projects";
  }
  return next;
}
