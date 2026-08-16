/**
 * Formatting the browser must not invent.
 *
 * Both of these render a value the SERVER counted — an age in seconds, an
 * instant in milliseconds — so the screen never needs a clock that agrees with
 * the cluster's. What a status *means* is `status.ts`; this file is only how a
 * number is printed.
 */

/**
 * An age the way the CLI prints one (cmd/kelson/secret.go: humanAge), from the
 * seconds the server counted — so the browser renders the same age the CLI does
 * without needing a clock that agrees with the cluster's.
 *
 * Every RPC that reports an age reports it this way (ListSecrets, ListPreviews),
 * so the formatting lives here rather than beside the first panel that needed
 * it.
 */
export function formatAge(seconds: bigint): string {
  const s = Number(seconds);
  if (!Number.isFinite(s) || s <= 0) return "-";
  if (s < 60) return `${Math.floor(s)}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

/** Wall-clock formatting for the stream's since_unix_ms; 0 means unset. */
export function formatInstant(unixMs: bigint | number): string {
  const ms = typeof unixMs === "bigint" ? Number(unixMs) : unixMs;
  if (!ms) return "";
  return new Date(ms).toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}
