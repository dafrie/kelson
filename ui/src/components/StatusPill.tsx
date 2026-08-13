import "./StatusPill.css";

/**
 * The five statuses the design system names (docs/design/assets/README.md),
 * plus `unknown` — because the API's three-state truth (present / absent /
 * could-not-tell) means "we could not read it" is a real answer and must not
 * be rendered as one of the four that claim to know.
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
  label?: string;
}) {
  return (
    <span className={`k-pill k-pill--${status}`} data-status={status}>
      <span className="k-pill__dot" />
      {label ?? status}
    </span>
  );
}
