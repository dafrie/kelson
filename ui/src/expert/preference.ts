import { useSyncExternalStore } from "react";

/**
 * "Kubernetes detail": one persisted display preference, default off (#260).
 *
 * The owner's ask was two-sided — "introduce an expert mode showing more dense
 * and Kubernetes related information; but for the normal user, simplify the
 * interfaces as much you can" — and the second half is the harder constraint.
 * So this is *not* a mode of operation. It is a display preference in exactly
 * the sense the theme is one, and three rules keep it that way:
 *
 * - **It gates information, never actions.** Every button, link and flow is on
 *   screen in both states. A preference that hid a control would make two
 *   products out of one and would make a screenshot from one reader useless to
 *   another; `src/expert/expert.test.tsx` asserts the action bar is identical
 *   with it on and off.
 * - **It never hides a could-not-check state or a structured code.** An error
 *   panel, an "unread" cell and a "no reading for this component" note are the
 *   states a reader most needs, and the honest-absence rule that produced them
 *   does not have a loudness setting. Detail may *add* to them.
 * - **Off is exactly what shipped.** Turning it on adds facts and carets;
 *   nothing moves, nothing is re-worded, and normal mode gains no density.
 *
 * # Why a store and not a context
 *
 * Every screen in this app is tested by rendering it against an in-memory
 * transport (`src/test/render.tsx`) with no providers but the clients'. A
 * context would mean either wrapping fifty tests or shipping a default that
 * silently disagreed with what the header shows. `useSyncExternalStore` over a
 * module-level value has neither problem: a component reads the same answer
 * wherever it is mounted, including outside the shell.
 *
 * # The absent key is the default
 *
 * Off is the absence of `kelson-detail` in localStorage, the same shape
 * `src/theme.ts` gives its "system" state: a fresh browser and one whose
 * storage was cleared behave identically, and there is no second spelling of
 * off to keep in step. The `storage` event is listened to, so two tabs of the
 * same instance agree without a reload.
 */

export const DETAIL_STORAGE_KEY = "kelson-detail";

/** The one value that means on. Anything else — absent, stale, corrupt — is off. */
const ON = "on";

/**
 * The last known value, or `undefined` before anything has read one. Reading
 * localStorage on every `getSnapshot` would return a fresh answer to React on
 * every render pass; caching it here is what makes the snapshot stable.
 */
let current: boolean | undefined;

const listeners = new Set<() => void>();

/**
 * Storage can throw outright — Safari private browsing, a locked-down profile —
 * and the honest answer to "does this reader want more detail" when we cannot
 * tell is no. Off is the default and the safe failure.
 */
export function readStoredDetail(): boolean {
  try {
    return window.localStorage.getItem(DETAIL_STORAGE_KEY) === ON;
  } catch {
    return false;
  }
}

function snapshot(): boolean {
  if (current === undefined) current = readStoredDetail();
  return current;
}

function notify(): void {
  for (const listener of listeners) listener();
}

/** Another tab moved the preference. A cleared storage arrives with a null key. */
function onStorage(event: StorageEvent): void {
  if (event.key !== null && event.key !== DETAIL_STORAGE_KEY) return;
  const next = readStoredDetail();
  if (next === current) return;
  current = next;
  notify();
}

function subscribe(listener: () => void): () => void {
  if (listeners.size === 0) window.addEventListener("storage", onStorage);
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) window.removeEventListener("storage", onStorage);
  };
}

/**
 * Set the preference and persist it.
 *
 * A failed write still applies to this page; it just will not outlive it, which
 * is the same trade `src/theme.ts` makes for the same reason.
 */
export function setDetail(on: boolean): void {
  if (snapshot() === on) return;
  current = on;
  try {
    if (on) window.localStorage.setItem(DETAIL_STORAGE_KEY, ON);
    else window.localStorage.removeItem(DETAIL_STORAGE_KEY);
  } catch {
    // Applied for this page only.
  }
  notify();
}

/** Is Kubernetes detail on? The one question every surface asks. */
export function useDetail(): boolean {
  return useSyncExternalStore(subscribe, snapshot, offForever);
}

/** No browser, no stored preference: the default. */
function offForever(): boolean {
  return false;
}
