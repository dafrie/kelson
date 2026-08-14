import { AppShell, EmptyState, Route, Routes } from "kelson-ui";

/**
 * The shell mounted as the layout route it is in the app: 56px bar with the
 * mark, primary nav (Projects active), docs link and theme toggle, content
 * centred beneath. Routes/Route come from the DS bundle so the preview shares
 * the provider's router.
 */
export function Shell() {
  return (
    <div style={{ margin: -20 }}>
      <Routes>
        <Route element={<AppShell />}>
          <Route
            path="/projects"
            element={
              <EmptyState title="No projects yet">
                kelson init writes a starter spec; kelson deploy puts it on the
                cluster.
              </EmptyState>
            }
          />
        </Route>
      </Routes>
    </div>
  );
}
