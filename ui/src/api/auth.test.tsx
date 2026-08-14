import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import {
  createMemoryRouter,
  Outlet,
  Route,
  RouterProvider,
  createRoutesFromElements,
} from "react-router-dom";

import {
  AuthBoundary,
  RequireSession,
  notifyUnauthenticated,
  safeReturnPath,
} from "./auth";
import { AppShell } from "../components/AppShell";
import { LoginPage } from "../pages/LoginPage";

/**
 * The login flow, driven through the real router against a stubbed
 * `/auth/*` origin.
 *
 * The three states of `GET /auth/session` are the contract everything here
 * turns on (docs/server.md): 204 disabled, 200 logged in, 401 log in. The
 * screen tests elsewhere never see any of it — they render their screens
 * outside an AuthBoundary, where the context defaults to "disabled" — and that
 * is deliberate: adding authentication must not make every other test learn
 * about it.
 */

type Answer = { status: number; body?: unknown };

/** A tiny /auth origin: one queue of answers per endpoint, plus a call log. */
function stubAuth(answers: {
  session: Answer[];
  login?: Answer[];
  logout?: Answer[];
}) {
  const calls: { path: string; body?: unknown }[] = [];
  const next = (queue: Answer[] | undefined, fallback: Answer): Answer =>
    queue && queue.length > 1 ? (queue.shift() as Answer) : (queue?.[0] ?? fallback);

  const fetchMock = vi.fn(
    async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const path = String(input);
      calls.push({
        path,
        body: init?.body ? JSON.parse(String(init.body)) : undefined,
      });
      const answer =
        path === "/auth/session"
          ? next(answers.session, { status: 401 })
          : path === "/auth/login"
            ? next(answers.login, { status: 401, body: { error: "wrong password" } })
            : next(answers.logout, { status: 204 });
      return new Response(
        answer.body === undefined ? null : JSON.stringify(answer.body),
        {
          status: answer.status,
          headers: { "Content-Type": "application/json" },
        },
      );
    },
  );
  vi.stubGlobal("fetch", fetchMock);
  return calls;
}

/** The app's own route shape: boundary, /login outside the gate, gate, shell. */
function renderApp(at: string) {
  const routes = createRoutesFromElements(
    <Route element={<AuthBoundary />}>
      <Route path="login" element={<LoginPage />} />
      <Route element={<RequireSession />}>
        <Route element={<AppShell />}>
          <Route path="projects" element={<span>projects screen</span>} />
          <Route path="cluster" element={<Outlet />} />
        </Route>
      </Route>
    </Route>,
  );
  const router = createMemoryRouter(routes, { initialEntries: [at] });
  return { router, ...render(<RouterProvider router={router} />) };
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the session tri-state", () => {
  it("skips login entirely when the server has no password", async () => {
    stubAuth({ session: [{ status: 204 }] });
    renderApp("/projects");

    expect(await screen.findByText("projects screen")).toBeTruthy();
    expect(screen.queryByLabelText("Password")).toBeNull();
    // No password means no user chip: there is nobody to name.
    expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
  });

  it("sends an anonymous visitor to the login screen, keeping the return path", async () => {
    stubAuth({ session: [{ status: 401 }] });
    const { router } = renderApp("/projects");

    expect(await screen.findByLabelText("Password")).toBeTruthy();
    await waitFor(() =>
      expect(router.state.location.pathname + router.state.location.search).toBe(
        "/login?next=%2Fprojects",
      ),
    );
    // A cold boot is not an expiry, and must not claim to be one.
    expect(screen.queryByText(/session expired/i)).toBeNull();
  });

  it("shows the username and a way out once a session exists", async () => {
    stubAuth({ session: [{ status: 200, body: { username: "ada" } }] });
    renderApp("/projects");

    expect(await screen.findByText("projects screen")).toBeTruthy();
    expect(screen.getByText("ada")).toBeTruthy();
    // The mockups' avatar circle, holding the initial rather than a picture.
    expect(document.querySelector(".k-user__avatar")?.textContent).toBe("a");

    const out = screen.getByRole("button", { name: "Sign out" });
    await act(async () => {
      out.click();
    });
    expect(await screen.findByLabelText("Password")).toBeTruthy();
  });
});

describe("logging in", () => {
  it("returns to where the visitor was going", async () => {
    const calls = stubAuth({
      session: [{ status: 401 }],
      login: [{ status: 200, body: { username: "ada" } }],
    });
    const { router } = renderApp("/projects");

    const username = await screen.findByLabelText("Username");
    const password = screen.getByLabelText("Password");
    await act(async () => {
      fill(username, "ada");
      fill(password, "hunter2");
    });
    await act(async () => {
      screen.getByRole("button", { name: "Sign in" }).click();
    });

    await waitFor(() =>
      expect(router.state.location.pathname).toBe("/projects"),
    );
    expect(await screen.findByText("projects screen")).toBeTruthy();
    expect(calls.find((c) => c.path === "/auth/login")?.body).toEqual({
      username: "ada",
      password: "hunter2",
    });
  });

  it("shows the server's own words when the password is wrong", async () => {
    stubAuth({
      session: [{ status: 401 }],
      login: [{ status: 401, body: { error: "wrong password" } }],
    });
    renderApp("/projects");

    const username = await screen.findByLabelText("Username");
    await act(async () => {
      fill(username, "ada");
      fill(screen.getByLabelText("Password"), "nope");
    });
    await act(async () => {
      screen.getByRole("button", { name: "Sign in" }).click();
    });

    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      "wrong password",
    );
    // Still on the login screen, with the form usable again.
    expect(screen.getByRole("button", { name: "Sign in" })).toBeTruthy();
  });
});

describe("a session that expires mid-use", () => {
  it("says so, and keeps the return path", async () => {
    stubAuth({ session: [{ status: 200, body: { username: "ada" } }] });
    const { router } = renderApp("/projects");
    expect(await screen.findByText("projects screen")).toBeTruthy();

    // What an RPC interceptor does when the server answers `unauthenticated`
    // — the server restarted, and its per-process signing key with it.
    await act(async () => {
      notifyUnauthenticated();
    });

    expect(await screen.findByText(/session expired/i)).toBeTruthy();
    await waitFor(() =>
      expect(router.state.location.search).toContain("expired=1"),
    );
    expect(router.state.location.search).toContain("next=%2Fprojects");
  });
});

describe("safeReturnPath", () => {
  it("keeps a path on this origin and drops anything else", () => {
    expect(safeReturnPath("/projects/hello/edit")).toBe("/projects/hello/edit");
    expect(safeReturnPath(null)).toBe("/projects");
    // A `next` a user can type is a redirect a user can be handed.
    expect(safeReturnPath("//evil.example")).toBe("/projects");
    expect(safeReturnPath("https://evil.example")).toBe("/projects");
    expect(safeReturnPath("/\\evil.example")).toBe("/projects");
  });
});

/** Set a controlled input's value the way React's onChange will see it. */
function fill(element: HTMLElement, value: string) {
  const input = element as HTMLInputElement;
  const setter = Object.getOwnPropertyDescriptor(
    HTMLInputElement.prototype,
    "value",
  )?.set;
  setter?.call(input, value);
  input.dispatchEvent(new Event("input", { bubbles: true }));
}
