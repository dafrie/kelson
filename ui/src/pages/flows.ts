/**
 * Where the six flows live now (#260).
 *
 * Until this change `deploy`, `diff`, `history`, `logs`, `rollback` and
 * `promote` were six sibling routes hanging off `(project, environment)`. Two of
 * them are views of the environment and are tabs; three are things a person
 * *does* to it and are actions; one — diff — was never a place at all and is now
 * a panel of the screens that need it.
 *
 * The old paths are what a commit status, a pull-request comment and the docs
 * link to, and `ui/README.md` promises that "a link into a deploy or a log tail
 * is a link that keeps working". So every old path still resolves, and the
 * mapping lives here as a pure function rather than inside six `<Navigate>`
 * elements: it is the part worth testing, and a redirect that silently drops
 * `?component=`, `?from=` or `?to=` is exactly the bug that makes a surviving
 * link useless.
 */

/** The flows that had a route of their own before the consolidation. */
export type Flow =
  | "deploy"
  | "diff"
  | "history"
  | "logs"
  | "promote"
  | "rollback";

/** The environment's own path — the tab strip's root, and Overview. */
export function environmentPath(project: string, env: string): string {
  return `/projects/${encodeURIComponent(project)}/${encodeURIComponent(env)}`;
}

/**
 * Where `/projects/:project/:env/<flow><search>` resolves to now.
 *
 * `logs` and `history` map to themselves: they became tabs of the environment
 * without moving, which is the best outcome a deep link can have. The other four
 * move, and every query parameter the old URL carried is carried with them —
 * `?component=` into the logs tab, `?to=` into the rollback action, `?from=`
 * into the comparison the history tab now opens, `?image=` into the deploy
 * action.
 */
export function flowTarget(
  project: string,
  env: string,
  flow: Flow,
  search: string,
): string {
  const base = environmentPath(project, env);
  const params = new URLSearchParams(search);

  switch (flow) {
    case "logs":
    case "history":
      return withSearch(`${base}/${flow}`, params);
    case "deploy":
    case "promote":
    case "rollback":
      return withSearch(`${base}/actions/${flow}`, params);
    case "diff":
      // The diff screen had two modes and each one had a home to go to. With a
      // revision named, it is the comparison a history row now opens, and the
      // revision rides along under the name it already had. With no revision it
      // was the cluster's own dry-run verdict, which the history tab opens too —
      // `?compare=1` is what says so, because a bare `/history` must not open a
      // panel nobody asked for.
      if (!params.has("compare") && params.get("from") === null) {
        params.set("compare", "1");
      }
      return withSearch(`${base}/history`, params);
  }
}

function withSearch(path: string, params: URLSearchParams): string {
  const query = params.toString();
  return query === "" ? path : `${path}?${query}`;
}
