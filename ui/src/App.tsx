import { Navigate, Route, Routes } from "react-router-dom";

import { AppShell } from "./components/AppShell";
import { AppsPage } from "./pages/AppsPage";
import { ClusterPage } from "./pages/ClusterPage";
import { EmptyState } from "./components/States";
import "./pages/pages.css";

export function App() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<Navigate to="/apps" replace />} />
        <Route path="apps" element={<AppsPage />} />
        <Route path="cluster" element={<ClusterPage />} />
        <Route
          path="*"
          element={
            <EmptyState title="No such page">
              The UI has two screens today: /apps and /cluster.
            </EmptyState>
          }
        />
      </Route>
    </Routes>
  );
}
