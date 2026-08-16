import type { StatusKind } from "../components/StatusPill";
import { statusForPreview, type Status } from "../components/status";
import type {
  Preview,
  PreviewLifecycle,
  PreviewSettings,
} from "../gen/kelson/v1alpha1/preview_pb";

/**
 * What a preview's state means, in words and in a pill.
 *
 * The phase itself is decided server-side (internal/delivery/flux/previews.go)
 * for the reason every other vocabulary in kelson is: one place decides what a
 * state means and the CLI, the UI and an agent render the same word. What lives
 * here is the *rendering* of it — the one sentence that says what a reader
 * should do about it — because that sentence is different in a browser than it
 * is in a terminal and neither belongs in the other.
 *
 * The pill itself is not this module's to name: a preview is an environment and
 * a reader asks the same question of it as of any other, so the word and the
 * colour come from `components/status.ts` like everything else's.
 */

/** The phases internal/delivery/flux publishes. */
export const PREVIEW_PHASES = [
  "ready",
  "applying",
  "awaiting-artifact",
  "failed",
  "unknown",
] as const;

export function previewStatus(phase: string, suspended: boolean): Status {
  return statusForPreview(phase, suspended);
}

/** The one line under a preview row: what this state is, and what fixes it. */
export function previewHeadline(preview: Preview): string {
  if (preview.suspended) {
    return "Suspended: what is running is the last thing that reconciled, not this change request's head.";
  }
  switch (preview.phase) {
    case "ready":
      return "Running: the manifests for this commit were fetched and applied.";
    case "applying":
      return "The manifests for this commit arrived; the rollout has not settled yet.";
    case "awaiting-artifact":
      return (
        "No manifests for this commit yet — a CI step running `kelson preview publish` " +
        "is what pushes them."
      );
    case "failed":
      return "The manifests arrived and the rollout failed. `kelson preview render` prints the ones for this commit.";
    default:
      return "Nothing has reported yet.";
  }
}

/**
 * The lifecycle pair as one statement.
 *
 * Three absences that look alike in an empty list and are not alike at all:
 * flux-operator missing, the manifests never delivered, and a poller that
 * cannot reach the forge. Each gets its own sentence.
 */
export interface LifecycleLine {
  status: StatusKind;
  text: string;
}

export function lifecycleLine(
  lifecycle: PreviewLifecycle | undefined,
): LifecycleLine {
  if (lifecycle === undefined) {
    return {
      status: "unknown",
      text: "The cluster was not read, so nothing is claimed about what is running.",
    };
  }
  // `served` is whether fluxcd.controlplane.io answers — previews are
  // flux-operator's lifecycle (ADR-0017), so without it there is no poller.
  if (!lifecycle.served) {
    return {
      status: "unknown",
      text: "Previews are not available on this cluster: flux-operator is not installed.",
    };
  }
  if (!lifecycle.present) {
    return {
      status: "unknown",
      text: `${lifecycle.name} does not exist in the cluster yet. Deploy this environment and previews start.`,
    };
  }
  if (lifecycle.providerReady !== "True") {
    return {
      status: lifecycle.providerReady === "False" ? "failed" : "reconciling",
      text: `The forge poller is not ready${reasonSuffix(lifecycle.providerReason, lifecycle.providerMessage)}`,
    };
  }
  // The ResourceSet's own Ready condition, said as what it produces.
  if (lifecycle.setReady !== "True") {
    return {
      status: lifecycle.setReady === "False" ? "failed" : "reconciling",
      text: `The preview environments are not ready${reasonSuffix(lifecycle.setReason, lifecycle.setMessage)}`,
    };
  }
  return {
    status: "synced",
    text: `${lifecycle.name} is polling the forge and standing up one environment per change request.`,
  };
}

function reasonSuffix(reason: string, message: string): string {
  const detail = [reason, message].filter((s) => s !== "").join(": ");
  return detail === "" ? "." : `: ${detail}`;
}

/**
 * The first twelve characters of the head commit — the abbreviation git itself
 * would print — with the whole value kept for copying. Nothing here abbreviates
 * for the *wire*: the tag flux pins is the full commit, and an abbreviation
 * would address an artifact nothing published (internal/preview/naming).
 */
export function shortSha(sha: string): string {
  return sha.length > 12 ? sha.slice(0, 12) : sha;
}

/**
 * The change request's page on its forge, or "" when it cannot be derived.
 *
 * It is built from the two things the spec already says — the provider and the
 * source repository — and from nothing else. A repo URL that is not a plain
 * https:// one yields no link rather than a guess, because a wrong link to a
 * pull request is worse than none.
 */
export function changeRequestUrl(
  settings: PreviewSettings | undefined,
  id: string,
): string {
  if (settings === undefined || id === "") return "";
  const repo = settings.repo.replace(/\/+$/, "");
  if (!repo.startsWith("https://") && !repo.startsWith("http://")) return "";
  switch (settings.provider) {
    case "github":
      return `${repo}/pull/${id}`;
    case "gitlab":
      return `${repo}/-/merge_requests/${id}`;
    default:
      return "";
  }
}

/** What the poller is configured to do, as one line of prose. */
export function settingsLine(settings: PreviewSettings): string {
  const labels =
    settings.filterLabels.length > 0
      ? `change requests labelled ${settings.filterLabels.join(", ")}`
      : "every open change request";
  return (
    `Polling ${settings.repo} every ${settings.interval} for ${labels}, ` +
    `at most ${settings.limit} at once.`
  );
}
