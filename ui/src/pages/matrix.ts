import {
  statusForPhase,
  statusForVerdict,
  UNKNOWN_STATUS,
  type Status,
  type StatusWord,
} from "../components/status";
import {
  clusterResourceName,
  clusterVerdictFor,
} from "../dataservices/parse";
import { isDataComponentKind } from "../spec/components";

/**
 * The component × environment matrix, as logic (#260).
 *
 * The screens that draw it — the project page's grid and the home page's rows —
 * ask the same three questions of the same two answers, so the questions live
 * here and are tested as functions rather than through a DOM.
 *
 * # What one Status call can and cannot say about one component
 *
 * `DeployService.Status` answers for an *environment*: one phase, one revision,
 * one cause. The only per-component thing it carries is `verdicts` — one entry
 * per rendered Deployment, named `Deployment/<namespace>/<component>` because
 * that is what the renderer names it (internal/renderer/workload.go: a
 * component's Deployment takes the component's own name).
 *
 * So a cell has three possible bases, and it says which one it is standing on:
 *
 * - **component** — this component has its own verdict, and the cell is about
 *   this component.
 * - **environment** — it has none, and the cell is showing the environment's
 *   word. Every kind but `service`, `worker` and `agent` lands here: a cron
 *   renders a CronJob, a database a `Cluster`, a chart a `HelmRelease`, and
 *   observation probes none of them today (internal/api's observeWorkloads).
 * - **unread** — the call failed or has not answered, which is `unknown` and
 *   never green.
 *
 * A cell that cannot tell a component's own state from its environment's would
 * be the quiet lie this whole vocabulary exists to prevent, which is why the
 * basis travels with the word instead of being flattened into it.
 */

/** A verdict row as rendered: the fetched one, or the stream's delta over it. */
export interface VerdictRow {
  resource: string;
  code: string;
  healthy: boolean;
  degraded: boolean;
  message: string;
  remediation: string;
}

/** What a HEALTH_CHANGE event carries about one resource. */
export interface LiveVerdict {
  code: string;
  healthy: boolean;
  message: string;
}

/**
 * The fetched verdicts with the stream's deltas applied.
 *
 * A HEALTH_CHANGE carries a code, a verdict and a message — not the
 * remediation, which is the fix for the code it replaced, so it is dropped
 * rather than left pointing at the wrong problem. `degraded` is likewise
 * recomputed as the softer claim the event supports: the failure-code set is
 * observation's (IsFailure) and lives in Go, so the browser must not guess
 * which side of it a live code falls on.
 *
 * A workload the event names but Status never returned is appended: a
 * Deployment that appeared since the last read is real, and hiding it until the
 * next refetch would be the staleness the stream exists to remove.
 */
export function mergeVerdicts(
  fetched: readonly VerdictRow[] | undefined,
  live: Record<string, LiveVerdict>,
): VerdictRow[] {
  const pending = { ...live };
  const rows: VerdictRow[] = (fetched ?? []).map((v) => {
    const update = pending[v.resource];
    if (update === undefined) {
      return {
        resource: v.resource,
        code: v.code,
        healthy: v.healthy,
        degraded: v.degraded,
        message: v.message,
        remediation: v.remediation,
      };
    }
    delete pending[v.resource];
    return {
      resource: v.resource,
      code: update.code,
      healthy: update.healthy,
      degraded: !update.healthy,
      message: update.message,
      remediation: "",
    };
  });
  for (const [resource, update] of Object.entries(pending)) {
    rows.push({
      resource,
      code: update.code,
      healthy: update.healthy,
      degraded: !update.healthy,
      message: update.message,
      remediation: "",
    });
  }
  return rows;
}

/** One environment's answer, as the matrix needs it. */
export interface EnvironmentRead {
  environment: string;
  /** The delivery phase, empty when nothing reported one. */
  phase: string;
  revision: string;
  cause: string;
  /** The resolved namespace, which is how a verdict is matched to a component. */
  namespace: string;
  verdicts: readonly VerdictRow[];
  /** False while the call is in flight or after it failed: nothing was read. */
  read: boolean;
}

export const NO_READ: EnvironmentRead = {
  environment: "",
  phase: "",
  revision: "",
  cause: "",
  namespace: "",
  verdicts: [],
  read: false,
};

export type CellBasis = "component" | "environment" | "unread";

export interface Cell {
  status: Status;
  basis: CellBasis;
  /** Observation's own code, verbatim, when the basis is the component's. */
  code: string;
  /** The one line worth showing under the word, when there is one. */
  detail: string;
}

/**
 * The verdict about one component, if the environment reported one.
 *
 * A data component's resources are named `<project>-<environment>-<component>`
 * rather than after the component alone (internal/renderer/dataservice.go), so
 * the two kinds of component are looked up two ways — the data one through the
 * same matcher the data-services section uses, so one rule serves both screens.
 *
 * The namespace is required to match when Status reported one, because a
 * component name is unique in a project and not in a cluster.
 */
export function verdictFor(
  read: EnvironmentRead,
  options: { project: string; component: string; kind: string },
): VerdictRow | undefined {
  const { project, component, kind } = options;
  if (isDataComponentKind(kind)) {
    return clusterVerdictFor(
      read.verdicts,
      clusterResourceName(project, read.environment, component),
    );
  }
  return read.verdicts.find((v) => {
    const parts = v.resource.split("/");
    if (parts.length !== 3 || parts[0] !== "Deployment") return false;
    if (read.namespace !== "" && parts[1] !== read.namespace) return false;
    return parts[2] === component;
  });
}

/** One cell of the matrix: the word, what it is a claim about, and one fact. */
export function readCell(
  read: EnvironmentRead,
  verdict: VerdictRow | undefined,
): Cell {
  if (!read.read) {
    return { status: UNKNOWN_STATUS, basis: "unread", code: "", detail: "" };
  }
  const environment = statusForPhase(read.phase);
  if (verdict === undefined) {
    return {
      status: environment,
      basis: "environment",
      code: "",
      detail: read.cause,
    };
  }
  return {
    status: statusForVerdict(verdict.healthy, verdict.degraded, environment),
    basis: "component",
    code: verdict.code,
    detail: verdict.healthy ? "" : verdict.message,
  };
}

/**
 * The components an environment reported a verdict about, in the order it
 * reported them.
 *
 * This is how the home screen gets a component list without a `GetSpec` per
 * project: `ListSpecs` omits the documents (proto/kelson/v1alpha1/spec.proto),
 * so the verdicts are the only per-component thing home already has. It is
 * therefore a *floor* and not the project's component list — a cron, a chart
 * and a database are absent from it, and so is everything when the server was
 * started without a workload observation client. A screen using it says which
 * it is showing rather than presenting the floor as the whole.
 */
export function componentsFromVerdicts(
  read: EnvironmentRead,
  project: string,
): { component: string; verdict: VerdictRow }[] {
  const out: { component: string; verdict: VerdictRow }[] = [];
  const dataPrefix = `${project}-${read.environment}-`;
  for (const verdict of read.verdicts) {
    const parts = verdict.resource.split("/");
    const kind = parts[0] ?? "";
    const name = parts[parts.length - 1] ?? "";
    if (name === "") continue;
    if (kind === "Deployment") {
      out.push({ component: name, verdict });
      continue;
    }
    // A data component's own resource, recognised by the name the renderer
    // gives it. Anything else — an ExternalSecret is the one that exists today
    // — is an environment's resource and not a component's.
    if (
      (kind === "Cluster" || kind === "ValkeyCluster") &&
      name.startsWith(dataPrefix)
    ) {
      out.push({ component: name.slice(dataPrefix.length), verdict });
    }
  }
  return out;
}

/**
 * The words that put a row in the attention band.
 *
 * `deploying` and `waiting` are deliberately out: work in flight resolves
 * itself, and a band that fills up during every deploy is a band people learn
 * to ignore. `suspended` is out for the opposite reason — nothing is trying, on
 * purpose. `unknown` is in, because "we could not tell" is exactly the state a
 * person has to go and look at, and this UI has never rendered it as fine.
 */
const ATTENTION: ReadonlySet<StatusWord> = new Set<StatusWord>([
  "failed",
  "stuck",
  "unhealthy",
  "unknown",
]);

export function needsAttention(word: StatusWord): boolean {
  return ATTENTION.has(word);
}

/**
 * An image reference with its middle removed, for a column that has room for
 * the ends of one.
 *
 * A digest is the value that must not be retyped from a screenshot, and its
 * head and tail are what a reader compares — the same trade the promotion table
 * makes (src/pages/promote.ts), kept short enough for a matrix cell. The whole
 * value stays on the title attribute wherever this is used.
 */
export function shortImage(image: string): string {
  if (image === "") return "";
  const at = image.lastIndexOf("@");
  if (at > 0) {
    const digest = image.slice(at + 1);
    const head = image.slice(0, at);
    const repo = head.slice(head.lastIndexOf("/") + 1);
    return `${repo}@${digest.slice(0, 14)}…`;
  }
  const slash = image.lastIndexOf("/");
  return slash < 0 ? image : image.slice(slash + 1);
}
