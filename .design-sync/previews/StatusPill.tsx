import { STATUS_KINDS, StatusPill } from "kelson-ui";

/** All six statuses the design system names, side by side. */
export function AllStatuses() {
  return (
    <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
      {STATUS_KINDS.map((kind) => (
        <StatusPill key={kind} status={kind} />
      ))}
    </div>
  );
}

/** A custom label over the status colour — counts, environment names. */
export function CustomLabels() {
  return (
    <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
      <StatusPill status="synced" label="12 synced" />
      <StatusPill status="degraded" label="2 degraded" />
      <StatusPill status="reconciling" label="rolling out" />
    </div>
  );
}
