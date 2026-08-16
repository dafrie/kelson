import type { StatusKind } from "./StatusPill";

/**
 * One status vocabulary for the whole UI (#260).
 *
 * Every word a reader sees on a status pill comes from here. Before this
 * module the same fact was said three ways on three screens — `synced` on a
 * card, `Reconciling` on a tab, `awaiting-artifact` on a preview row — which
 * are Flux's word, Kubernetes' word and a poller's word, and none of them
 * answer the question a developer is asking: is my change live, is it moving,
 * or is something wrong.
 *
 * The engine already computes that answer. `statemachine.State.Answer` keeps
 * six of them apart by construction and every wire message that reports a
 * state can be projected onto them, so this module does no judging: it
 * translates, and a value it does not know becomes `unknown` rather than a
 * guess.
 *
 * # Word and tone are different things
 *
 * `StatusWord` is the word. `StatusKind` — `synced`, `reconciling`, `degraded`,
 * `failed`, `suspended`, `unknown` — is the *palette*: the six colour ramps in
 * `src/styles/tokens.css`, named in `docs/design/assets/README.md` and pinned
 * for contrast in both themes. Those names stay because they name colours, and
 * a colour does not change because the word painted in it did. Nothing renders
 * a `StatusKind` as text; `StatusPill` takes a `label`, and on this module's
 * surfaces the label is always the word.
 *
 * # Why `suspended` survives as a seventh
 *
 * A suspended thing is not waiting. Waiting means something is expected to act;
 * suspended means nothing is trying, on purpose, and what is running is the
 * last thing that reconciled rather than the last thing that was asked for.
 * Collapsing the two would make a paused preview look like one that is about to
 * come up. It is reported by the wire (`Preview.suspended`) and never derived,
 * so it is only ever shown when something actually said so.
 */

export const STATUS_WORDS = [
  "live",
  "deploying",
  "waiting",
  "stuck",
  "unhealthy",
  "failed",
  "suspended",
  "unknown",
] as const;

export type StatusWord = (typeof STATUS_WORDS)[number];

/** A word and the palette tone it is painted in. */
export interface Status {
  word: StatusWord;
  tone: StatusKind;
}

/**
 * The word → tone map, and the whole of the colour policy.
 *
 * `waiting` and `deploying` share the busy tone — the pulsing one — because
 * both mean work is in flight and a reader scanning a grid needs one signal for
 * that, not two. `stuck` and `unhealthy` share the amber tone: both are "this
 * needs you" without being settled failures. `failed` is the only red, and it
 * is reserved for a state nothing will move off on its own.
 */
const TONES: Record<StatusWord, StatusKind> = {
  live: "synced",
  deploying: "reconciling",
  waiting: "reconciling",
  stuck: "degraded",
  unhealthy: "degraded",
  failed: "failed",
  suspended: "suspended",
  unknown: "unknown",
};

export function statusFor(word: StatusWord): Status {
  return { word, tone: TONES[word] };
}

export const UNKNOWN_STATUS: Status = statusFor("unknown");

/**
 * `statemachine.Answer` → a word.
 *
 * The engine's six tokens are its own vocabulary and they are not all the words
 * a developer would choose: `progressing` is what a state machine calls it and
 * `deploying` is what a person does, `rejected` names who refused rather than
 * what happened, and `degraded` is a Kubernetes condition. The tokens stay on
 * the wire; the translation is here so it happens once.
 */
const ANSWERS: Record<string, StatusWord> = {
  live: "live",
  progressing: "deploying",
  waiting: "waiting",
  stuck: "stuck",
  degraded: "unhealthy",
  rejected: "failed",
};

export function statusForAnswer(answer: string): Status {
  const word = ANSWERS[answer];
  return word === undefined ? UNKNOWN_STATUS : statusFor(word);
}

/**
 * `delivery.Phase` → a word, for the sources that report a phase and no answer
 * (`DeployService.Status`, `History`, the Settled event's `final`).
 *
 * It is `deriveAnswer` in `src/deploy/rail.ts` composed with the map above, and
 * it agrees with it by construction: everything between Proposed and Applied is
 * work in flight, Healthy is live, and the two off-rail phases are the two
 * failures. A phase this map does not know is `unknown` — the point of the
 * eighth word is that "we could not tell" has somewhere honest to go.
 */
const PHASES: Record<string, StatusWord> = {
  Proposed: "waiting",
  Committed: "waiting",
  Reconciling: "deploying",
  Applied: "deploying",
  Healthy: "live",
  Degraded: "unhealthy",
  Rejected: "failed",
};

export function statusForPhase(phase: string): Status {
  const word = PHASES[phase];
  return word === undefined ? UNKNOWN_STATUS : statusFor(word);
}

/**
 * A preview's phase → the same words.
 *
 * Previews are flux-operator's lifecycle rather than the state machine's
 * (ADR-0017), so their phases are a fourth vocabulary off the wire
 * (`internal/delivery/flux/previews.go`). They answer the same question and get
 * the same words: `awaiting-artifact` is a preview waiting for a CI step that
 * has not run, which is `waiting` — the row's own sentence underneath says
 * which CI step, so nothing is lost by not printing the token.
 *
 * Suspension wins over the phase, because a suspended preview keeps its last
 * conditions and its phase therefore describes a moment nothing is maintaining.
 */
const PREVIEW_PHASES: Record<string, StatusWord> = {
  ready: "live",
  applying: "deploying",
  "awaiting-artifact": "waiting",
  failed: "failed",
};

export function statusForPreview(phase: string, suspended: boolean): Status {
  if (suspended) return statusFor("suspended");
  const word = PREVIEW_PHASES[phase];
  return word === undefined ? UNKNOWN_STATUS : statusFor(word);
}

/**
 * A workload verdict → a tone.
 *
 * The pill on a workload line carries the observation code (`workload/…`)
 * rather than a word: that code is the server's own and this UI renders it
 * verbatim, the way it renders every structured code. Only the colour comes
 * from here, and it comes through the same words so a red workload line and a
 * red environment pill mean the same thing.
 */
export function verdictTone(healthy: boolean, degraded: boolean): StatusKind {
  if (healthy) return TONES.live;
  return degraded ? TONES.unhealthy : TONES.failed;
}
