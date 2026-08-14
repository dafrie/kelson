import { LoadingState } from "kelson-ui";

/** The polite in-flight state every list shows first. */
export function Loading() {
  return <LoadingState what="projects" />;
}
