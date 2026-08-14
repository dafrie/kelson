import { ErrorState } from "kelson-ui";

/** An error with its mono detail line. */
export function WithDetail() {
  return (
    <ErrorState
      title="Could not load the deployment history"
      detail="rpc unavailable: connection refused"
    />
  );
}
