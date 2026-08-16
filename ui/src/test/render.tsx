import type { ReactNode } from "react";
import { render } from "@testing-library/react";
import { createMemoryRouter, RouterProvider, type RouteObject } from "react-router-dom";
import type { Transport } from "@connectrpc/connect";

import { createClients } from "../api/clients";
import { ClientsProvider } from "../api/data";

/**
 * Render one screen against an in-memory transport.
 *
 * The screens read their clients from context, so a test substitutes a
 * createRouterTransport one and nothing else changes: the same generated
 * clients, the same serialisation, the same streaming semantics — the stub is a
 * server, not a mock of the client. Routing is real too, and it is a *data*
 * router, matching main.tsx: `useBlocker` only exists under one, so a screen
 * that guards unsaved changes has to be tested under the router the app ships.
 *
 * `also` adds sibling routes for a test that navigates — a destination has to
 * exist before a navigation to it can be blocked.
 *
 * The returned `router` is what a test reads to assert on the URL itself — a
 * clear-the-query-params effect, say — rather than only on what rendered.
 */
export function renderAt(
  transport: Transport,
  path: string,
  routePath: string,
  element: ReactNode,
  also: RouteObject[] = [],
) {
  return renderRoutes(transport, path, [{ path: routePath, element }, ...also]);
}

/**
 * The same, given a route table rather than one route.
 *
 * The environment's screens are a layout with tabs under it (#260), so a test
 * of one of them has to mount the layout too: the tab reads what the layout
 * fetched, and "the tab strip marks the right tab" is a claim about the pair.
 * A test that needs a redirect to *land* passes the destination in the same
 * table, which is also how a navigation assertion gets somewhere to arrive.
 */
export function renderRoutes(
  transport: Transport,
  path: string,
  routes: RouteObject[],
) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  return {
    router,
    ...render(
      <ClientsProvider clients={createClients(transport)}>
        <RouterProvider router={router} />
      </ClientsProvider>,
    ),
  };
}
