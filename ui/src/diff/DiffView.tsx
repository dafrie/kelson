import {
  formatValue,
  summaryLine,
  type Diff,
  type FieldDiff,
  type ResourceDiff,
} from "./parse";
import "./DiffView.css";

/**
 * One preview, rendered.
 *
 * Colour carries the op (added green, removed red, modified amber) and risk is
 * a pill, because risk is the field a CI gate branches on (internal/diff:
 * "agents branch on this field and must never parse prose") and a human
 * scanning should read the same signal the gate does.
 *
 * Three things are kept visually distinct on purpose:
 *   - a policy violation with enforcement `enforce` blocks; `audit` warns
 *     (#45 — a warning must never be read as a blocker),
 *   - an unvalidated resource is not a violation: nothing rejected it, the
 *     question was never asked (#43),
 *   - a degraded L2 fell back to L1, so the preview is best-effort.
 */

const BLOCKED = 3;

export function DiffView({
  diff,
  exitSemantics,
}: {
  diff: Diff;
  exitSemantics?: number | undefined;
}) {
  const blocked = exitSemantics === BLOCKED;
  return (
    <div className="k-diff">
      {blocked ? (
        <div className="k-diff__blocked" role="alert">
          <span className="k-diff__blocked-title">Blocked</span>
          <span className="k-mono">
            this preview would not apply — an enforcing policy rejected it, or a
            resource could not be validated and its prerequisite is absent
          </span>
        </div>
      ) : null}

      {diff.degraded ? (
        <div className="k-diff__degraded">
          <span className="k-eyebrow">Best effort</span>
          <span className="k-mono">
            the live cluster was unavailable, so this fell back to a rendered
            diff{diff.degradedReason ? `: ${diff.degradedReason}` : ""}
          </span>
        </div>
      ) : null}

      <div className="k-mono k-diff__summary">
        <span>{diff.level} diff</span>
        <span>·</span>
        <span>
          {diff.project}/{diff.environment}
        </span>
        <span>·</span>
        <span>{summaryLine(diff)}</span>
      </div>

      {diff.resources.length === 0 ? (
        <div className="k-panel k-panel--dim k-mono">
          no differences — the cluster already matches this spec
        </div>
      ) : (
        <ul className="k-diff__resources">
          {diff.resources.map((r) => (
            <Resource key={`${r.kind}/${r.namespace}/${r.name}`} resource={r} />
          ))}
        </ul>
      )}

      {diff.violations.length > 0 ? (
        <section className="k-section">
          <div className="k-eyebrow">Policy ({diff.violations.length})</div>
          <ul className="k-diff__findings">
            {diff.violations.map((v, i) => (
              <li
                key={`${v.engine}/${v.policy}/${i}`}
                className={
                  v.enforcement === "enforce"
                    ? "k-diff__finding k-diff__finding--blocking"
                    : "k-diff__finding"
                }
              >
                <div className="k-diff__finding-head">
                  <code className="k-mono">
                    {v.engine}/{v.policy}
                    {v.rule ? `/${v.rule}` : ""}
                  </code>
                  <span
                    className={`k-pill k-pill--${v.enforcement === "enforce" ? "failed" : "degraded"}`}
                  >
                    <span className="k-pill__dot" />
                    {v.enforcement === "enforce" ? "blocking" : "warning"}
                  </span>
                </div>
                <p className="k-diff__finding-message">{v.message}</p>
                <div className="k-mono k-diff__finding-where">
                  {[v.resource, v.path, v.specPath].filter(Boolean).join(" · ")}
                </div>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {diff.unvalidated.length > 0 ? (
        <section className="k-section">
          <div className="k-eyebrow">Not validated ({diff.unvalidated.length})</div>
          <ul className="k-diff__findings">
            {diff.unvalidated.map((u, i) => (
              <li
                key={`${u.resource}/${i}`}
                className={
                  u.inBatch
                    ? "k-diff__finding"
                    : "k-diff__finding k-diff__finding--blocking"
                }
              >
                <div className="k-diff__finding-head">
                  <code className="k-mono">{u.resource}</code>
                  <span className="k-mono k-diff__finding-where">
                    {u.inBatch
                      ? "prerequisite is created by this same batch"
                      : "prerequisite is absent — the apply would fail too"}
                  </span>
                </div>
                {u.requires ? (
                  <p className="k-diff__finding-message">requires {u.requires}</p>
                ) : null}
                {u.message ? (
                  <p className="k-mono k-diff__finding-where">{u.message}</p>
                ) : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </div>
  );
}

function Resource({ resource }: { resource: ResourceDiff }) {
  const glyph =
    resource.op === "added" ? "+" : resource.op === "removed" ? "-" : "~";
  return (
    <li className={`k-diff__resource k-diff__resource--${opClass(resource.op)}`}>
      <div className="k-diff__resource-head">
        <span className="k-diff__glyph" aria-hidden="true">
          {glyph}
        </span>
        <span className="k-mono k-diff__resource-name">
          {resource.kind}/{resource.name}
        </span>
        {resource.namespace ? (
          <span className="k-mono k-diff__resource-ns">
            {resource.namespace}
          </span>
        ) : null}
        <span className="k-mono k-diff__op">{resource.op}</span>
        <RiskPill risk={resource.risk} />
      </div>
      {resource.fields.length > 0 ? (
        <ul className="k-diff__fields">
          {resource.fields.map((f) => (
            <Field key={f.path} field={f} />
          ))}
        </ul>
      ) : null}
    </li>
  );
}

function Field({ field }: { field: FieldDiff }) {
  const added = field.before === undefined && field.after !== undefined;
  const removed = field.after === undefined && field.before !== undefined;
  return (
    <li className="k-diff__field k-mono">
      <span className="k-diff__field-path">{field.path}</span>
      <span className="k-diff__field-values">
        {removed || !added ? (
          <span className="k-diff__before">{formatValue(field.before)}</span>
        ) : null}
        {!added && !removed ? <span className="k-diff__arrow">→</span> : null}
        {added || !removed ? (
          <span className="k-diff__after">{formatValue(field.after)}</span>
        ) : null}
      </span>
      <span className="k-diff__field-origin">
        ({field.origin}
        {field.specPath ? ` · ${field.specPath}` : ""})
      </span>
    </li>
  );
}

function RiskPill({ risk }: { risk: string }) {
  const kind =
    risk === "disruptive"
      ? "failed"
      : risk === "restart-required"
        ? "degraded"
        : risk === "additive"
          ? "synced"
          : "unknown";
  return (
    <span className={`k-pill k-pill--${kind}`}>
      <span className="k-pill__dot" />
      {risk}
    </span>
  );
}

function opClass(op: string): string {
  return op === "added" || op === "removed" ? op : "modified";
}
