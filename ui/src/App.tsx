import { createRoutesFromElements, Navigate, Route } from "react-router-dom";

import { AppShell } from "./components/AppShell";
import { AppsPage } from "./pages/AppsPage";
import { AppDetailPage } from "./pages/AppDetailPage";
import { ClusterPage } from "./pages/ClusterPage";
import { DeployPage } from "./pages/DeployPage";
import { DiffPage } from "./pages/DiffPage";
import { EditSpecPage } from "./pages/EditSpecPage";
import { LogsPage } from "./pages/LogsPage";
import { NewAppPage } from "./pages/NewAppPage";
import { RollbackPage } from "./pages/RollbackPage";
import { EmptyState } from "./components/States";
import "./pages/pages.css";

/**
 * The M6 screen set.
 *
 * Every flow is addressed by (project, environment) because that pair is what
 * the API's mutating RPCs take — a SpecRef plus an environment name — so a URL
 * is enough to reach any of them and a link into a deploy or a log tail is a
 * link that keeps working.
 *
 * There is deliberately no history screen: #67 defers it, and the Rollback flow
 * calls the History RPC only to offer target revisions.
 *
 * The routes are exported as objects rather than rendered as <Routes>, because
 * the spec editor (#65) has unsaved changes to protect and `useBlocker` — the
 * only thing in react-router-dom v7 that can see a client-side navigation
 * before it happens — is available exclusively under a data router. Nothing
 * else in the app uses a loader or an action; this is the whole reason the
 * router is a data one, and `createRoutesFromElements` keeps the declaration
 * readable as the route table it is.
 */
export const routes = createRoutesFromElements(
  <Route element={<AppShell />}>
    <Route index element={<Navigate to="/apps" replace />} />
    <Route path="apps" element={<AppsPage />} />
    {/* Static before dynamic: /apps/new is the create flow, not a project
        called "new". React Router ranks it first either way; the order here
        says so to the reader too. */}
    <Route path="apps/new" element={<NewAppPage />} />
    <Route path="apps/:project" element={<AppDetailPage />} />
    <Route path="apps/:project/edit" element={<EditSpecPage />} />
    <Route path="apps/:project/:env/deploy" element={<DeployPage />} />
    <Route path="apps/:project/:env/diff" element={<DiffPage />} />
    <Route path="apps/:project/:env/logs" element={<LogsPage />} />
    <Route path="apps/:project/:env/rollback" element={<RollbackPage />} />
    <Route path="cluster" element={<ClusterPage />} />
    <Route
      path="*"
      element={
        <EmptyState title="No such page">
          The UI starts at /apps; every flow hangs off a project and an
          environment.
        </EmptyState>
      }
    />
  </Route>,
);
