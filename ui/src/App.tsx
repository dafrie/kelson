import { createRoutesFromElements, Navigate, Route } from "react-router-dom";

import { AuthBoundary, RequireSession } from "./api/auth";
import { AppShell } from "./components/AppShell";
import { LoginPage } from "./pages/LoginPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { ProjectDetailPage } from "./pages/ProjectDetailPage";
import { ComponentPage } from "./pages/ComponentPage";
import { ClusterPage } from "./pages/ClusterPage";
import { ConnectionsPage } from "./pages/ConnectionsPage";
import { DeployPage } from "./pages/DeployPage";
import { EditSpecPage } from "./pages/EditSpecPage";
import { EnvironmentPage } from "./pages/EnvironmentPage";
import { EnvironmentOverview } from "./pages/EnvironmentOverview";
import { FlowRedirect } from "./pages/FlowRedirect";
import { HistoryPage } from "./pages/HistoryPage";
import { LogsPage } from "./pages/LogsPage";
import { NewProjectPage } from "./pages/NewProjectPage";
import { OnboardingPage } from "./pages/OnboardingPage";
import { PreviewDetailPage } from "./pages/PreviewDetailPage";
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
 * # Two tabs, three actions, and no diff route (#260)
 *
 * Six flows used to hang off `(project, environment)` as sibling routes and a
 * reader had to know which of the six answered their question. They were not
 * siblings, so they are not routed like siblings any more:
 *
 *  - **Logs** and **History** are *views* of the environment — tabs of
 *    `EnvironmentPage`, at the paths they already had, so every link that ever
 *    pointed at a log tail or a release history still lands on it.
 *  - **Deploy**, **Rollback** and **Promote** are *actions* — things done to
 *    the environment, entered from its bar and finished by returning to it.
 *    They sit under `actions/` so that the URL says which kind of thing they
 *    are.
 *  - **Diff** is neither. It is a panel of the screens that have a comparison
 *    to show: the deploy action's own preview, the editor's pre-save guard, and
 *    the history tab, where a row opens it against that revision.
 *
 * The four paths that moved keep working: `FlowRedirect` maps each old one onto
 * its new home and carries the query string with it (`pages/flows.ts`), because
 * those URLs are in commit statuses, pull-request comments and the docs.
 *
 * The history screen (#67) is the one read-only flow: it shows the recorded
 * revisions and hands them to the comparison and the rollback as a query
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
        {/* The environment: one layout, its two views as tabs beneath it. The
            index is the panel that used to sit under the project page's matrix
            (#260) — the environment in view is now the one in the URL. */}
        <Route path="projects/:project/:env" element={<EnvironmentPage />}>
          <Route index element={<EnvironmentOverview />} />
          <Route path="logs" element={<LogsPage />} />
          <Route path="history" element={<HistoryPage />} />
        </Route>
        {/* The component in an environment, which is the unit that deploys
            (docs/model.md §6) and until #260 had no address of its own. The
            path is the pair every other flow takes plus the component's name,
            so it composes with them rather than replacing any of them: the page
            links out to the same tabs and actions. It is not a tab of the
            environment layout because its subject is narrower than the tabs' —
            a tab strip whose first two entries change subject would be lying
            about what it switches between. */}
        <Route
          path="projects/:project/:env/components/:component"
          element={<ComponentPage />}
        />
        {/* The three actions. Each is a focused task surface with its own
            confirm step, entered from the environment's bar and returning to
            it. The environment in a promotion's path is its *target* — the one
            whose pins are written — and the source is picked on the screen. */}
        <Route
          path="projects/:project/:env/actions/deploy"
          element={<DeployPage />}
        />
        <Route
          path="projects/:project/:env/actions/promote"
          element={<PromotePage />}
        />
        <Route
          path="projects/:project/:env/actions/rollback"
          element={<RollbackPage />}
        />
        {/* The four paths that moved. Kept because they are in commit statuses,
            pull-request comments and the docs, and `ui/README.md` promises that
            a link into a deploy is a link that keeps working. */}
        <Route
          path="projects/:project/:env/deploy"
          element={<FlowRedirect flow="deploy" />}
        />
        <Route
          path="projects/:project/:env/diff"
          element={<FlowRedirect flow="diff" />}
        />
        <Route
          path="projects/:project/:env/promote"
          element={<FlowRedirect flow="promote" />}
        />
        <Route
          path="projects/:project/:env/rollback"
          element={<FlowRedirect flow="rollback" />}
        />
        {/* The identifier a commit status and a PR comment link to (ADR-0017
            stage 3, #248): pr<N> is what a human types when they go looking,
            so it is the route's own leaf rather than the compound
            <project>-<environment>-pr<N> the namespace uses internally. */}
        <Route
          path="projects/:project/:env/previews/:pr"
          element={<PreviewDetailPage />}
        />
        <Route path="cluster" element={<ClusterPage />} />
        {/* Instance-wide, not project-scoped: a connection is what *any*
            project's source resolves through (ADR-0033 decision 4), so it has
            no project or environment in its path and hangs off the root beside
            the other two instance screens. */}
        <Route path="connections" element={<ConnectionsPage />} />
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
