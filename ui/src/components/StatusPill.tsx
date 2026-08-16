import "./StatusPill.css";

/**
 * The six colour ramps the design system names (docs/design/assets/README.md),
 * plus `unknown` — because the API's three-state truth (present / absent /
 * could-not-tell) means "we could not read it" is a real answer and must not
 * be rendered as one of the ones that claim to know.
 *
 * These are **tones, not words**. `synced` and `reconciling` are the names of
 * two colours, pinned for contrast in both themes by `styles/tokens.test.ts`;
 * they are not what a reader sees. The words are `status.ts`'s, which is why
 * `label` is required: a pill that could fall back to its tone name would be a
 * pill that occasionally says "synced" to a person.
 */
export const STATUS_KINDS = [
  "synced",
  "reconciling",
  "degraded",
  "failed",
  "suspended",
  "unknown",
] as const;

export type StatusKind = (typeof STATUS_KINDS)[number];

export function StatusPill({
  status,
  label,
}: {
  status: StatusKind;
  label: string;
}) {
  return (
    <span className={`k-pill k-pill--${status}`} data-status={status}>
      <span className="k-pill__dot" />
      {label}
    </span>
  );
}
