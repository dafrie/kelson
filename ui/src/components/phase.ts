import type { StatusKind } from "./StatusPill";

/**
 * delivery.Phase → the design system's status vocabulary.
 *
 * The phases are internal/delivery/delivery.go's, verbatim off the wire; the
 * pill names are docs/design/assets/README.md's. Everything between Proposed
 * and Healthy is one visual state — "work is in flight" — because that is the
 * one thing a reader scanning a grid needs to tell apart from "arrived" and
 * "broken". A phase this map does not know is `unknown`, never a guess: the
 * whole point of the sixth pill is that "we could not tell" has somewhere
 * honest to go.
 */
const PHASES: Record<string, StatusKind> = {
  Healthy: "synced",
  Reconciling: "reconciling",
  Committed: "reconciling",
  Proposed: "reconciling",
  // Applied is the adapter's "the write landed, health not yet judged" rung —
  // still in flight, so it reads as reconciling like the three above.
  Applied: "reconciling",
  Degraded: "degraded",
  Rejected: "failed",
};

export function phaseToStatus(phase: string): StatusKind {
  return PHASES[phase] ?? "unknown";
}

/**
 * statemachine.Answer → pill, for the deploy stream's Transition rows, where
 * the answer is the more specific signal ("stuck" is not a phase).
 */
const ANSWERS: Record<string, StatusKind> = {
  live: "synced",
  waiting: "reconciling",
  progressing: "reconciling",
  stuck: "degraded",
  degraded: "degraded",
  rejected: "failed",
};

export function answerToStatus(answer: string): StatusKind {
  return ANSWERS[answer] ?? "unknown";
}

/** Wall-clock formatting for the stream's since_unix_ms; 0 means unset. */
export function formatInstant(unixMs: bigint | number): string {
  const ms = typeof unixMs === "bigint" ? Number(unixMs) : unixMs;
  if (!ms) return "";
  return new Date(ms).toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}
