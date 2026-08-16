import type { RouteObject } from "react-router-dom";

import { DeployPage } from "../pages/DeployPage";
import { EnvironmentOverview } from "../pages/EnvironmentOverview";
import { EnvironmentPage } from "../pages/EnvironmentPage";
import { FlowRedirect } from "../pages/FlowRedirect";
import { HistoryPage } from "../pages/HistoryPage";
import { LogsPage } from "../pages/LogsPage";
import { PromotePage } from "../pages/PromotePage";
import { RollbackPage } from "../pages/RollbackPage";

/**
 * The environment's routes, as `App.tsx` mounts them (#260).
 *
 * A tab is not mountable on its own any more — it reads the spec its layout
 * fetched — so a test of one mounts the pair, which is also the honest thing to
 * test: "the History tab renders the record" and "the tab strip says History is
 * the one showing" are the same claim about the same screen.
 *
 * This mirrors the route table rather than importing it, because `App.tsx`'s is
 * wrapped in the auth boundary and the shell. `EnvironmentPage.test.tsx` asserts
 * the real table's paths directly, off the exported `routes`, so the mirror
 * cannot drift without a test saying so.
 */
export function environmentRoutes(): RouteObject[] {
  return [
    {
      path: "/projects/:project/:env",
      element: <EnvironmentPage />,
      children: [
        { index: true, element: <EnvironmentOverview /> },
        { path: "logs", element: <LogsPage /> },
        { path: "history", element: <HistoryPage /> },
      ],
    },
    {
      path: "/projects/:project/:env/actions/deploy",
      element: <DeployPage />,
    },
    {
      path: "/projects/:project/:env/actions/promote",
      element: <PromotePage />,
    },
    {
      path: "/projects/:project/:env/actions/rollback",
      element: <RollbackPage />,
    },
    {
      path: "/projects/:project/:env/deploy",
      element: <FlowRedirect flow="deploy" />,
    },
    {
      path: "/projects/:project/:env/diff",
      element: <FlowRedirect flow="diff" />,
    },
    {
      path: "/projects/:project/:env/promote",
      element: <FlowRedirect flow="promote" />,
    },
    {
      path: "/projects/:project/:env/rollback",
      element: <FlowRedirect flow="rollback" />,
    },
  ];
}
