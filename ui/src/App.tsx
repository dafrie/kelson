import { createRoutesFromElements, Navigate, Route } from "react-router-dom";

import { AuthBoundary, RequireSession } from "./api/auth";
import { AppShell } from "./components/AppShell";
import { LoginPage } from "./pages/LoginPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { ProjectDetailPage } from "./pages/ProjectDetailPage";
import { ClusterPage } from "./pages/ClusterPage";
import { DeployPage } from "./pages/DeployPage";
import { DiffPage } from "./pages/DiffPage";
import { EditSpecPage } from "./pages/EditSpecPage";
import { HistoryPage } from "./pages/HistoryPage";
import { LogsPage } from "./pages/LogsPage";
import { NewProjectPage } from "./pages/NewProjectPage";
import { OnboardingPage } from "./pages/OnboardingPage";
import { PromotePage } from "./pages/PromotePage";
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
 * The history screen (#67) is the one read-only flow: it shows the recorded
 * revisions and hands them to the diff and rollback screens as a query
 * parameter, rather than growing its own copy of either action.
 *
 * The routes are exported as objects rather than rendered as <Routes>, because
 * the spec editor (#65) has unsaved changes to protect and `useBlocker` — the
 * only thing in react-router-dom v7 that can see a client-side navigation
 * before it happens — is available exclusively under a data router. Nothing
 * else in the app uses a loader or an action; this is the whole reason the
 * router is a data one, and `createRoutesFromElements` keeps the declaration
 * readable as the route table it is.
 *
 * # Three layers, in this order
 *
 * `AuthBoundary` asks the server once whether there is anything to log in to
 * (#84's interim cut, ../docs/server.md). `/login` sits inside it and outside
 * the gate — a login screen behind a login gate is a redirect loop — and
 * outside the AppShell, because a page with no session has no nav to offer.
 * `RequireSession` gates everything else, and does nothing at all on a server
 * with no password, which is the default.
 */
export const routes = createRoutesFromElements(
  <Route element={<AuthBoundary />}>
    <Route path="login" element={<LoginPage />} />
    <Route element={<RequireSession />}>
      <Route element={<AppShell />}>
        <Route index element={<Navigate to="/projects" replace />} />
        <Route path="projects" element={<ProjectsPage />} />
        {/* Static before dynamic: /projects/new is the create flow, not a
            project called "new". React Router ranks it first either way; the
            order here says so to the reader too. */}
        <Route path="projects/new" element={<NewProjectPage />} />
        <Route path="projects/:project" element={<ProjectDetailPage />} />
        <Route path="projects/:project/edit" element={<EditSpecPage />} />
        <Route path="projects/:project/:env/deploy" element={<DeployPage />} />
        <Route path="projects/:project/:env/diff" element={<DiffPage />} />
        <Route path="projects/:project/:env/history" element={<HistoryPage />} />
        <Route path="projects/:project/:env/logs" element={<LogsPage />} />
        {/* The environment in the path is the promotion's *target* — the one
            whose pins are written — and the source is picked on the screen.
            Naming the target is what makes this route reachable from the
            environment a reader is already looking at. */}
        <Route path="projects/:project/:env/promote" element={<PromotePage />} />
        <Route path="projects/:project/:env/rollback" element={<RollbackPage />} />
        <Route path="cluster" element={<ClusterPage />} />
        <Route path="setup" element={<OnboardingPage />} />
        <Route
          path="*"
          element={
            <EmptyState title="No such page">
              The UI starts at /projects; every flow hangs off a project and an
              environment.
            </EmptyState>
          }
        />
      </Route>
    </Route>
  </Route>,
);
