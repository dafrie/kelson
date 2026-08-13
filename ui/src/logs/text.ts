import type { LogLine } from "../gen/kelson/v1alpha1/logs_pb";

/**
 * Getting the lines out of the browser — into a clipboard, or onto a disk.
 *
 * A log view whose contents can only be read inside itself is a dead end: the
 * next step after finding the line is pasting it into an issue or attaching the
 * window to one. So the same rendering serves both, and it is not the screen's
 * rendering: the timestamps are full ISO-8601 rather than the wall-clock
 * `HH:MM:SS` the tail shows, because a file outlives the day it was saved on
 * and a bare time in it would be unresolvable.
 */

/** The screen's timestamp: time only, because the tail is about *now*. */
export function stamp(unixMs: bigint): string {
  const ms = Number(unixMs);
  // 0 means the line carried no timestamp the engine could parse, not 1970.
  if (!ms) return "--:--:--";
  return new Date(ms).toISOString().slice(11, 19);
}

/** The saved timestamp: the whole instant, UTC, or a dash for lines without one. */
export function isoStamp(unixMs: bigint): string {
  const ms = Number(unixMs);
  if (!ms) return "-";
  return new Date(ms).toISOString();
}

/**
 * One line per line, `timestamp pod container message`.
 *
 * Whatever the caller passes is what gets written: the visible (filtered) lines
 * for a copy of what is on screen, the whole retained buffer for a download.
 * Neither is annotated as complete, because neither is — the buffer is a capped
 * window on an unbounded stream, and the file records what the tab still held.
 */
export function toText(lines: readonly LogLine[]): string {
  if (lines.length === 0) return "";
  const out: string[] = [];
  for (const line of lines) {
    out.push(
      `${isoStamp(line.timestampUnixMs)} ${line.pod} ${line.container} ${line.message}`,
    );
  }
  return `${out.join("\n")}\n`;
}

/** Filesystem-safe, and sortable: the instant goes last so names group by workload. */
export function logFileName(
  namespace: string,
  application: string,
  at: Date,
): string {
  const when = at.toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
  const parts = [namespace, application]
    .map((part) => part.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-|-$/g, ""))
    .filter((part) => part !== "");
  return ["kelson-logs", ...parts, when].join("-") + ".txt";
}

/**
 * Hands the text to the browser as a file.
 *
 * An object URL rather than a `data:` one: a retained buffer is megabytes and
 * some browsers cap `data:` href length. Revoked on the next tick — revoking
 * before the click has been dispatched cancels the download.
 */
export function saveText(fileName: string, text: string): void {
  const url = URL.createObjectURL(
    new Blob([text], { type: "text/plain;charset=utf-8" }),
  );
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = fileName;
  anchor.style.display = "none";
  document.body.appendChild(anchor);
  anchor.click();
  document.body.removeChild(anchor);
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
