import { Fragment, type ReactNode } from "react";

import "./expert.css";
import { useDetail } from "./preference";

/**
 * The per-statement why-caret (#260).
 *
 * This UI makes plain-language claims — a status word, "older than the spec",
 * "stuck", "deployed now" — and each of them is computed from something the
 * wire said. Normal mode keeps the claim and drops the derivation, which is the
 * right trade for the reader who wants to know whether their change is live.
 * The reader who wants to know *why we say so* used to have nowhere to go but
 * the source, so this attaches the evidence to the individual sentence rather
 * than dumping a debug panel at the bottom of the page.
 *
 * Three rules, and they are what keep it from becoming a second status display:
 *
 * - **It is absent in normal mode.** Not disabled, not a smaller caret —
 *   absent, so nothing about the simple screen changes shape when the
 *   preference is off.
 * - **It is collapsed in expert mode too.** Expert mode says a reader wants the
 *   evidence *available*, not that they want fifteen expanded panels on one
 *   page. The caret is 11px and neutral; opening one is a deliberate act.
 * - **It is evidence for ONE statement.** The `statement` prop is the exact
 *   claim on screen beside it, and it goes into the accessible name, so a
 *   screen reader user who lands on the third caret in a row is told which
 *   sentence it belongs to.
 *
 * # Why it is a popover and not a block
 *
 * Every site that carries one is a flex row of small inline things — a pill, a
 * drift mark, a phase, a stream indicator. A disclosure that expanded in flow
 * would reflow that row and push the page around on every open. The body is
 * therefore positioned, which is the one place in this UI something floats;
 * it still has no shadow, because the Console has no elevation at all
 * (ui/README.md), and it is a hairline panel like everything else.
 *
 * It is built on `<details>`/`<summary>` for the same reason `Disclosure` is:
 * keyboard operation and the browser's own semantics, with no script.
 */
export function Why({
  statement,
  children,
}: {
  statement: string;
  children: ReactNode;
}) {
  const detail = useDetail();
  if (!detail) return null;
  const label = `Evidence for “${statement}”`;
  return (
    <details className="k-why">
      <summary className="k-why__caret" aria-label={label} title={label} />
      <div className="k-why__body">{children}</div>
    </details>
  );
}

/** One row of evidence: what the wire called it, and what it said. */
export interface EvidenceRow {
  /** The field's name as the response spells it — a machine's label. */
  name: string;
  value: string;
  /** The value is a sentence somebody wrote, so it is set in sans. */
  prose?: boolean;
}

/**
 * The evidence itself: the fields a claim was computed from, verbatim.
 *
 * A row whose value is empty is dropped rather than rendered blank. An absent
 * field is the wire declining to answer, and a key with nothing beside it reads
 * as an empty string that was actually sent — which is the one confusion a
 * panel about *what we were told* must not create. When every row drops, the
 * note is all that is left and it is what explains the emptiness.
 */
export function Evidence({
  rows,
  note,
}: {
  rows: readonly EvidenceRow[];
  note?: string;
}) {
  const shown = rows.filter((row) => row.value !== "");
  return (
    <>
      {shown.length > 0 ? (
        <div className="k-kv k-why__facts">
          {shown.map((row) => (
            <Fragment key={row.name}>
              <span className="k-kv__key">{row.name}</span>
              <span className={row.prose ? "k-kv__prose" : undefined}>
                {row.value}
              </span>
            </Fragment>
          ))}
        </div>
      ) : null}
      {note ? <p className="k-why__note">{note}</p> : null}
    </>
  );
}
