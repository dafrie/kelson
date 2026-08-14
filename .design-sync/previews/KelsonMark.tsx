import { KelsonMark } from "kelson-ui";

/** The mark at header size, in brand green on the page surface. */
export function HeaderSize() {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
      <KelsonMark />
      <span style={{ fontWeight: 600, letterSpacing: "-0.02em" }}>kelson</span>
    </div>
  );
}

/** Sizes; stroke weight is compensated at call sites. */
export function Sizes() {
  return (
    <div style={{ display: "flex", alignItems: "flex-end", gap: 20 }}>
      <KelsonMark size={21} strokeWidth={10} />
      <KelsonMark size={40} strokeWidth={8} />
      <KelsonMark size={72} strokeWidth={7} />
    </div>
  );
}

/** The stroke follows context colour when asked to. */
export function ContextColour() {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 20 }}>
      <KelsonMark size={40} strokeWidth={8} />
      <KelsonMark size={40} strokeWidth={8} color="var(--kelson-text)" />
      <KelsonMark size={40} strokeWidth={8} color="var(--kelson-muted)" />
    </div>
  );
}
