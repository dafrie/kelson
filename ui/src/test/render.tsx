import type { ReactNode } from "react";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { Transport } from "@connectrpc/connect";

import { createClients } from "../api/clients";
import { ClientsProvider } from "../api/data";

/**
 * Render one screen against an in-memory transport.
 *
 * The screens read their clients from context, so a test substitutes a
 * createRouterTransport one and nothing else changes: the same generated
 * clients, the same serialisation, the same streaming semantics — the stub is a
 * server, not a mock of the client. Routing is real too, because every screen
 * takes its project and environment from the path.
 */
export function renderAt(
  transport: Transport,
  path: string,
  routePath: string,
  element: ReactNode,
) {
  return render(
    <ClientsProvider clients={createClients(transport)}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path={routePath} element={element} />
        </Routes>
      </MemoryRouter>
    </ClientsProvider>,
  );
}
