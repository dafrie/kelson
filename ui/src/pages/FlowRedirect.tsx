import { Navigate, useLocation, useParams } from "react-router-dom";

import { flowTarget, type Flow } from "./flows";

/**
 * One of the six old flow routes, kept alive as a redirect (#260).
 *
 * `replace` because the old path is not somewhere a reader should be able to go
 * back to: pressing Back from the new home would land on the redirect and bounce
 * forward again. The whole query string travels with them — `flows.ts` decides
 * what it becomes.
 */
export function FlowRedirect({ flow }: { flow: Flow }) {
  const { project = "", env = "" } = useParams();
  const { search } = useLocation();
  return <Navigate to={flowTarget(project, env, flow, search)} replace />;
}
