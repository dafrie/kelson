import { Navigate, Route, Routes } from "react-router-dom";

import { AppShell } from "./components/AppShell";
import { AppsPage } from "./pages/AppsPage";
import { AppDetailPage } from "./pages/AppDetailPage";
import { ClusterPage } from "./pages/ClusterPage";
import { DeployPage } from "./pages/DeployPage";
import { DiffPage } from "./pages/DiffPage";
import { LogsPage } from "./pages/LogsPage";
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
 */
export function App() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<Navigate to="/apps" replace />} />
        <Route path="apps" element={<AppsPage />} />
        <Route path="apps/:project" element={<AppDetailPage />} />
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
      </Route>
    </Routes>
  );
}
