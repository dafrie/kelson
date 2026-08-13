import type { WatchState } from "../api/watch";

import "./LiveIndicator.css";

/**
 * Whether what you are looking at is updating itself.
 *
 * It is deliberately the smallest thing on the page: the interesting state is
 * the app's, not the transport's. Three states, and the third is silence — a
 * server that cannot watch renders nothing at all, because "this build has no
 * event stream" is not news to anyone reading a screen that simply behaves the
 * way it always did.
 */
export function LiveIndicator({ state }: { state: WatchState }) {
  if (state === "off") return null;
  return (
    <span className="k-live" data-state={state}>
      <span className="k-live__dot" />
      {state === "live" ? "live" : "reconnecting…"}
    </span>
  );
}
