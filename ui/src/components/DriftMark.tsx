import "./DriftMark.css";

import type { Drift } from "./status";

/**
 * Drift, rendered beside the word and never instead of it (#260).
 *
 * `status.ts`'s `driftFor` decides *whether* an environment has drifted and
 * what the sentence is; this decides nothing and only draws it. Two rules hold
 * it to its place:
 *
 * - **It never carries a status word.** The environment keeps whatever word the
 *   engine answered — usually `live`, which is the whole surprise the field
 *   exists to make visible — and this sits next to it as a second fact.
 * - **It never repeats the revision.** Every site that renders this already has
 *   the revision on screen as a mono value, and the mark is the sans sentence
 *   about it; printing `44-1a2b3c4d` twice, once qualified and once not, is
 *   exactly the doubled machine value the Console's mono rule is there to keep
 *   rare. `Drift.revision` is kept on the title attribute so the pairing is
 *   still explicit for a reader who is not sure which revision is meant.
 *
 * Nothing is rendered when the environment is not stale — currency is the norm
 * and recedes; drift is the exception and advances.
 */
export function DriftMark({ drift }: { drift: Drift | undefined }) {
  if (drift === undefined) return null;
  return (
    <span
      className="k-drift"
      data-pinned={drift.pinned ? "true" : "false"}
      title={drift.revision === "" ? undefined : `revision ${drift.revision}`}
    >
      <span className="k-drift__dot" />
      {drift.note}
    </span>
  );
}
