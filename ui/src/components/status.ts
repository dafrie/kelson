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
 * That answer now reaches a *poll* as well as a stream (`StatusResponse.answer`),
 * so `statusForDelivery` is the entry point every environment surface uses and
 * `statusForPhase` is its fallback rather than its default. Deriving a word
 * from a phase is the older, narrower claim — it cannot say `stuck` at all —
 * and it is kept only for the sources that genuinely carry no answer.
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
 * (`EventService`'s `StatusTransition`, and any older server whose `Status`
 * predates the answer field).
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
 * An environment's word: the engine's own answer where there is one, the phase
 * derivation where there is not.
 *
 * `StatusResponse.answer` is `statemachine.State.Answer` for this environment —
 * the same vocabulary the deploy stream's transitions carry — and it is the
 * field to read rather than a thing to re-derive. The phase cannot express all
 * six answers: `stuck` is not a phase but a verdict *about* one, so a client
 * deriving from the phase alone has to call a wedged deployment `waiting` and
 * report that nothing is wrong. Re-deriving is also a second opinion, free to
 * disagree with what the CLI prints about the same environment.
 *
 * The fallback is deliberate and narrow, and the two absences are not the same:
 *
 * - **Empty** falls back to the phase. `EventService`'s `StatusTransition`
 *   carries a phase, a previous phase, a revision and a cause — no answer at
 *   all — so every live delta arrives this way, and deriving is what this UI
 *   did everywhere before the field existed. Empty from a *poll* means the
 *   delivery half was not reported at all and `cause` says why; it never means
 *   "fine", and the phase is empty in that case too, so the derivation lands on
 *   `unknown` rather than inventing something cheerier.
 * - **A token this build does not know** does NOT fall back. The engine said
 *   something and we cannot read it, which is exactly what `unknown` is for.
 *   Deriving a word from the phase instead would bury the disagreement.
 */
export function statusForDelivery(answer: string, phase: string): Status {
  return answer === "" ? statusForPhase(phase) : statusForAnswer(answer);
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
 * The classification a `WorkloadVerdict` carries, as the three booleans the
 * wire sends. Exclusive, and in this order of precedence: healthy, stuck,
 * degraded, none-of-them.
 */
export interface VerdictFacts {
  healthy: boolean;
  degraded: boolean;
  stuck: boolean;
}

/**
 * A workload verdict → a word.
 *
 * Four cases now, because the wire can finally tell the fourth apart:
 *
 * - **Healthy** keeps the environment's own word (`statusForVerdict`). A green
 *   Deployment inside an environment that is still reconciling is not live yet
 *   — the delivery answer is the wider claim and it wins.
 * - **Stuck** is `stuck`. The probe gave up waiting: no progress before its
 *   budget expired, and it was not failing. `code` stays a wait code such as
 *   `workload/progressing`, which is precisely why the word has to say what the
 *   code cannot.
 * - **Degraded** is `unhealthy`. The wire's `degraded` is
 *   `!healthy && !stuck && IsFailure(code)` (internal/api's workloadVerdicts),
 *   so it is already the settled-failure half of observation's split.
 * - **Neither** is a rollout still in flight — pods not ready yet, no pods yet
 *   — and it is `deploying`, never a failure.
 *
 * Before `WorkloadVerdict.stuck` existed, "gave up waiting" and "still
 * starting" were one case and both had to be called `deploying`. They are two
 * now, and `stuck` is the loud one.
 */
export function wordForVerdict(verdict: VerdictFacts): StatusWord {
  if (verdict.healthy) return "live";
  if (verdict.stuck) return "stuck";
  return verdict.degraded ? "unhealthy" : "deploying";
}

/**
 * A workload verdict → a tone.
 *
 * The pill on a workload line carries the observation code (`workload/…`)
 * rather than a word: that code is the server's own and this UI renders it
 * verbatim, the way it renders every structured code. Only the colour comes
 * from here, and it comes through `wordForVerdict` so a red workload line and a
 * red environment pill mean the same thing — the two cannot drift, because
 * there is one classification and this reads its word.
 *
 * That unification changed one case. This function used to paint "neither
 * healthy nor degraded" in the `failed` tone while `statusForVerdict` called
 * the same verdict `deploying`, so one screen showed a starting pod in red and
 * another showed it in blue. Blue is the claim the fields support.
 */
export function verdictTone(verdict: VerdictFacts): StatusKind {
  return TONES[wordForVerdict(verdict)];
}

/**
 * A workload verdict → a status, for a surface that has room for one word and
 * not for a code.
 *
 * The matrix's cells are that surface: a cell says how one component is doing
 * in one environment, and the observation code goes underneath as the labelled
 * fact rather than as the pill. Healthy defers to the environment — see
 * `wordForVerdict` for the four cases and why healthy is the one that borrows.
 */
export function statusForVerdict(
  verdict: VerdictFacts,
  environment: Status,
): Status {
  return verdict.healthy ? environment : statusFor(wordForVerdict(verdict));
}

/**
 * Drift: an environment that is not serving the spec it holds (#260).
 *
 * `StatusResponse.stale` is the one field that answers "am I looking at what I
 * asked for?", and it is a statement about REVISIONS and not about health: a
 * stale environment is very often `live`, because revision 44 is up and well
 * and simply is not revision 45. So this returns a fact to render *beside* a
 * status and never a status of its own, and no caller may turn it into a word.
 *
 * # The tone decision: the quiet tier, with one mark
 *
 * Stale is not a failure and is not painted like one — no fill, no amber, no
 * red, and no place in the needs-attention band (`pages/matrix.ts`'s
 * ATTENTION). It takes the quiet tier's shape, the one `live` and `suspended`
 * already use: hairline ring, neutral ink, and the only colour a 5px dot. The
 * dot is the `suspended` hue, because that is the palette's single "nothing is
 * tracking this, and it may well be on purpose" colour and it is already pinned
 * as a graphical object rather than as text (3:1 in `styles/tokens.test.ts`).
 * Reusing it spends no new colour and puts drift in the one tier the Console
 * reserves for facts that are notable without being wrong. `DriftMark` is the
 * whole of the rendering.
 *
 * # Two notes, because the wire can tell them apart
 *
 * A rollback pin is stale by construction and correctly so: the environment is
 * deliberately serving an older revision, and `cause` names the pin. Saying
 * only "older than the spec" about a pin would read as neglect, so the pinned
 * note says it was chosen.
 *
 * # What the line does not say
 *
 * Not "the spec is at 45". `StatusResponse` carries `stale` as a boolean and no
 * generation of its own; the *revision's* generation is readable from its tag
 * (`<generation>-<hash8>`, ADR-0028 decision 2) but the spec's current
 * generation is not on this message at all, so the only honest claim about it
 * is "greater than the one serving". A number nobody sent is exactly the
 * invention this vocabulary exists to prevent.
 */
export interface Drift {
  /** The revision the cluster is observed to be serving. */
  revision: string;
  /** True when `cause` names a rollback pin: this drift was chosen. */
  pinned: boolean;
  /** The sentence rendered beside the word. Short, and never a status word. */
  note: string;
}

/**
 * The engine's own reason tokens for a rollback pin — a closed set
 * (api/kelson/v1alpha1's `Reason*` constants). `StatusResponse.cause` is
 * `statemachine.Cause.String()`, the non-empty parts of component / reason /
 * message joined with ": ", so a reason is one whole segment of it. Matching
 * segments and never substrings is what keeps this from firing on a message
 * that merely mentions a rollback.
 */
const PIN_REASONS = new Set(["RollbackPinned", "RolledBack"]);

export function driftFor(facts: {
  stale: boolean;
  revision: string;
  observedRevision: string;
  cause: string;
}): Drift | undefined {
  if (!facts.stale) return undefined;
  const pinned = facts.cause.split(": ").some((part) => PIN_REASONS.has(part));
  return {
    // `observed_revision` is the field that answers what is in the cluster
    // right now. On this spine it agrees with `revision`; reading it first
    // keeps the seam open for a source that can tell the two apart.
    revision: facts.observedRevision || facts.revision,
    pinned,
    note: pinned ? "pinned to an older revision" : "older than the spec",
  };
}
