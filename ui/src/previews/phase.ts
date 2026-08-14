import type { StatusKind } from "../components/StatusPill";
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
 * here is the *rendering* of it — which pill, and the one sentence that says
 * what a reader should do about it — because that sentence is different in a
 * browser than it is in a terminal and neither belongs in the other.
 *
 * A phase this module does not know is `unknown`, never a guess.
 */

/** The phases internal/delivery/flux publishes. */
export const PREVIEW_PHASES = [
  "ready",
  "applying",
  "awaiting-artifact",
  "failed",
  "unknown",
] as const;

const PHASE_PILLS: Record<string, StatusKind> = {
  ready: "synced",
  applying: "reconciling",
  // Not "failed": a missing artifact is almost always a CI step nobody added,
  // and drawing it as a failure sends a reader to the manifests instead of to
  // the workflow (ADR-0017 decision 8).
  "awaiting-artifact": "degraded",
  failed: "failed",
};

export function previewStatus(phase: string, suspended: boolean): StatusKind {
  // Suspension wins over the phase for the same reason Flux reports it
  // separately: a suspended Kustomization keeps its last conditions, so its
  // phase describes a moment that is no longer being maintained.
  if (suspended) return "suspended";
  return PHASE_PILLS[phase] ?? "unknown";
}

/** The one line under a preview row: what this state is, and what fixes it. */
export function previewHeadline(preview: Preview): string {
  if (preview.suspended) {
    return "Suspended: this preview's Kustomization is paused, so what is running is the last thing that reconciled, not the change request's head.";
  }
  switch (preview.phase) {
    case "ready":
      return "Running: the artifact for this commit was fetched and applied.";
    case "applying":
      return "The artifact was fetched; the apply has not settled yet.";
    case "awaiting-artifact":
      return (
        "No artifact for this commit. flux-operator found the change request and is waiting for " +
        "`kelson preview publish` to push the manifests for this head commit — a CI step, not a deploy."
      );
    case "failed":
      return "The artifact was fetched and the apply failed. The manifests are the ones `kelson preview render` prints for this commit.";
    default:
      return "Neither the artifact nor the apply has reported yet.";
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
  if (!lifecycle.served) {
    return {
      status: "unknown",
      text:
        "flux-operator is not installed on this cluster: fluxcd.controlplane.io is not served, so nothing " +
        "polls the forge and no preview can exist. Previews are flux-operator's lifecycle (ADR-0017).",
    };
  }
  if (!lifecycle.present) {
    return {
      status: "unknown",
      text:
        `${lifecycle.name} does not exist in the cluster yet. kelson renders the pair from this spec — ` +
        "deploy this environment and flux-operator starts polling.",
    };
  }
  if (lifecycle.providerReady !== "True") {
    return {
      status: lifecycle.providerReady === "False" ? "failed" : "reconciling",
      text: `The forge poller is not ready${reasonSuffix(lifecycle.providerReason, lifecycle.providerMessage)}`,
    };
  }
  if (lifecycle.setReady !== "True") {
    return {
      status: lifecycle.setReady === "False" ? "failed" : "reconciling",
      text: `The ResourceSet is not ready${reasonSuffix(lifecycle.setReason, lifecycle.setMessage)}`,
    };
  }
  return {
    status: "synced",
    text: `${lifecycle.name} is polling the forge and instantiating one OCIRepository and Kustomization per change request.`,
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
