import { ServerUnreachableState } from "kelson-ui";

/** The single most common state: kelson-server is not running. */
export function Unreachable() {
  return <ServerUnreachableState detail="fetch failed: connection refused" />;
}
