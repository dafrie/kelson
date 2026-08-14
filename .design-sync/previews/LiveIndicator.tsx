import { LiveIndicator } from "kelson-ui";

/** The stream is up: a quiet green dot. */
export function Live() {
  return <LiveIndicator state="live" />;
}

/** The stream dropped and is being retried. */
export function Reconnecting() {
  return <LiveIndicator state="reconnecting" />;
}
