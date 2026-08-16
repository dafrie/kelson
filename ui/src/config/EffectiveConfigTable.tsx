import { useMemo } from "react";

import { useAsync, useClients } from "../api/data";
import { ErrorPanel } from "../components/ErrorPanel";
import { LoadingState } from "../components/States";
import { envValueText, type EnvValue } from "../spec/documents";
import { configGroups, type ConfigRow, type ShadowRow } from "./effective";
import "./config.css";

/**
 * What this component actually runs with here, and where each value was set
 * (#260).
 *
 * The precedence merge is the server's — `SpecService.GetEffectiveConfig`
 * answers with the winning value *and* the block that decided it — so this
 * screen reads a table and draws it. Four things it is deliberate about:
 *
 * - **A setting the answer did not mention is absent, not blank.** A component
 *   awaiting its first build has no image row at all, because nothing in either
 *   document names one; a row saying "—" would claim the question was asked and
 *   answered.
 * - **A reference stays a reference.** `{ secret: checkout-db, key: url }` is
 *   rendered as itself, in the one spelling `src/spec/documents.ts` writes, and
 *   there is no value behind it to show — kelson never reads the Secret.
 * - **A built-in default recedes.** It is the row nobody wrote, so it is dimmed
 *   and its "set at" says whose default it is. The rows a reader is looking for
 *   are the ones a file put there.
 * - **A replaced value is drawn under the one that replaced it, struck
 *   through.** Only where there is one: a row nothing overrode is the row it
 *   always was, so a table where nothing is overridden is exactly the height it
 *   was before (#268). The shadow line carries no name of its own and no rule
 *   of its own — it is collapsed onto the winner as part of the same block, so
 *   the eye reads one setting rather than two rows.
 * - **Nothing is loud until it breaks.** The table is hairlines and mono
 *   values; the only coloured object it can produce is `ErrorPanel`, when the
 *   call fails or the stored spec no longer resolves.
 */
export function EffectiveConfigTable({
  project,
  environment,
  component,
}: {
  project: string;
  environment: string;
  component: string;
}) {
  const clients = useClients();
  const effective = useAsync(
    (signal) =>
      clients.spec.getEffectiveConfig(
        { project, environment, component },
        { signal },
      ),
    [clients, project, environment, component],
  );

  const groups = useMemo(
    () => configGroups(effective.data?.config, component),
    [effective.data, component],
  );
  const findings = effective.data?.errors ?? [];

  return (
    <section className="k-section">
      <div className="k-env__head">
        <div className="k-eyebrow">Configuration</div>
      </div>
      <div className="k-section__body">
        {effective.loading && effective.data === undefined ? (
          <LoadingState what="the effective configuration" />
        ) : null}

        {effective.error !== undefined ? (
          <ErrorPanel
            title="Could not read the effective configuration"
            error={effective.error}
          />
        ) : null}

        {findings.length > 0 ? (
          <ErrorPanel
            title="These documents no longer resolve"
            errors={findings}
          />
        ) : null}

        {effective.error === undefined && findings.length === 0 ? (
          groups.length > 0 ? (
            <table className="k-config">
              <thead>
                <tr>
                  <th>setting</th>
                  <th>value</th>
                  <th>set at</th>
                </tr>
              </thead>
              {groups.map((group) => (
                <tbody key={group.title}>
                  <tr className="k-config__group">
                    <th colSpan={3} scope="colgroup">
                      {group.title}
                    </th>
                  </tr>
                  {group.rows.map((row) => (
                    <Row key={`${group.title}:${row.name}`} row={row} />
                  ))}
                </tbody>
              ))}
            </table>
          ) : effective.data !== undefined ? (
            <p className="k-env__note">
              Nothing is configured for {component} in {environment}.
            </p>
          ) : null
        ) : null}
      </div>
    </section>
  );
}

/**
 * One row: the setting's name, its value, and the block that set it — followed
 * by a line per value it replaced.
 *
 * The name is mono because it is written in a document rather than chosen for
 * this screen — an environment variable is the author's own string and
 * `resources.requests.cpu` is the model's. The "set at" sentence is sans,
 * because it is a sentence. The document path rides on the row's title for the
 * reader who wants the exact line, and never takes space in the table.
 */
function Row({ row }: { row: ConfigRow }) {
  const classes = [
    row.builtIn ? "k-config__row--default" : "",
    row.shadows.length > 0 ? "k-config__row--shadowed" : "",
  ]
    .filter((c) => c !== "")
    .join(" ");
  return (
    <>
      <tr className={classes === "" ? undefined : classes}>
        <td className="k-mono k-config__name">{row.name}</td>
        <td className="k-mono k-config__value">
          <Value value={row.value} />
        </td>
        <td className="k-config__setat" title={row.where || undefined}>
          {row.setAt}
        </td>
      </tr>
      {row.shadows.map((shadow, i) => (
        <ShadowLine
          key={shadow.where === "" ? `${row.name}:${i}` : shadow.where}
          shadow={shadow}
          last={i === row.shadows.length - 1}
        />
      ))}
    </>
  );
}

/**
 * A value this row's winner replaced: what it was, struck through, and the two
 * blocks the sentence names.
 *
 * It carries no name — it is the same setting as the row above it — and it says
 * "was" so the strikethrough is not the only thing telling a reader that this
 * is not what runs. The value is rendered by exactly the same rules as a
 * winner's: a reference is still a reference and an empty string is still named
 * rather than left blank, because a shadow is a value somebody wrote too.
 */
function ShadowLine({ shadow, last }: { shadow: ShadowRow; last: boolean }) {
  return (
    <tr className={last ? "k-config__shadow k-config__shadow--last" : "k-config__shadow"}>
      <td />
      <td className="k-mono k-config__value">
        <span className="k-config__was">was</span>{" "}
        <s className="k-config__shadow-value">
          <Value value={shadow.value} />
        </s>
      </td>
      <td className="k-config__setat" title={shadow.where || undefined}>
        {shadow.setAt}
      </td>
    </tr>
  );
}

function Value({ value }: { value: EnvValue }) {
  if (value.kind !== "plain") return <>{envValueText(value)}</>;
  // An empty string is a value somebody set — docs/model.md's way of unsetting
  // a project variable for one environment — so it is shown as one rather than
  // as an absence.
  if (value.value === "") return <span className="k-config__empty">empty</span>;
  return <>{value.value}</>;
}
