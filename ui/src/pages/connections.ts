import type { StatusKind } from "../components/StatusPill";
import {
  GitAuthKind,
  GitOwnerKind,
  type GitConnection,
  type GitOwner,
} from "../gen/kelson/v1alpha1/gitconnection_pb";
import { dnsLabelProblem } from "../spec/documents";

/**
 * Reading a `GitConnection` for display, and refusing to read more than it
 * says.
 *
 * `GitConnectionService` answers with references and provider-reported fact —
 * which forge, which host, which Secret, and then what the provider said when
 * the server last asked it (proto/kelson/v1alpha1/gitconnection.proto). Three
 * of those readings are easy to get wrong in a way that would put a claim on
 * the screen nobody measured, so each is a function here with the rule written
 * beside it:
 *
 *   - **Health is three-state, not two.** `ready` and `reachable` are both
 *     false before the first probe *and* after a failed one, and `message` is
 *     the only thing that separates them. A connection nothing has looked at is
 *     therefore `unknown`, not `failed` — the same present / absent /
 *     could-not-tell discipline `ClusterProfile` gaps already get.
 *   - **A count is only a count once somebody counted.** `repositories` is zero
 *     for an installation with nothing selected and zero for a connection never
 *     probed, so it is reported only when the probe that produced it succeeded.
 *   - **Owner is a label, not a boundary.** ADR-0033 decision 6 fixes the
 *     ownership semantics now and enforces none of them until principals exist
 *     (#231). Rendering "owned by alice" beside a connection every operator can
 *     delete is authorization theatre, which is the exact failure ADR-0031
 *     refuses, so the screen shows the recorded kind and says in words that
 *     everything here is visible to everyone on this instance.
 *
 * The vocabulary is the wire's own: providers are the strings internal/forge's
 * registry uses, and an enum value this build does not know renders as unknown
 * rather than as one of the values it does — a newer server naming a third auth
 * kind must not have it silently drawn as a token.
 */

/** The forge base URL a `provider: github` connection means when it names none. */
export const DEFAULT_GITHUB_HOST = "https://github.com";

/**
 * What a client is told when both conditions are false and the connection says
 * nothing about why. It is the one case the wire genuinely cannot resolve, and
 * the screen says so rather than picking the more alarming of the two.
 */
export const NOT_OBSERVED =
  "nothing has probed this connection yet, or a probe failed without saying why";

const NO_MESSAGE = "the connection reports no reason";

export interface ConnectionHealth {
  status: StatusKind;
  /** The word on the pill. */
  label: string;
  /** The connection's own `message`, or what its absence means. */
  detail: string;
}

/**
 * The two conditions and the message, read as one health answer.
 *
 * `ready` is the CR's own well-formedness — the connection parses and its
 * Secret exists with the keys its auth kind needs — and `reachable` is whether
 * the last probe authenticated against the forge. They fail independently: a
 * Secret that was deleted breaks the first, a revoked installation breaks the
 * second, and the two want different words.
 */
export function connectionHealth(connection: GitConnection): ConnectionHealth {
  const message = connection.message.trim();
  if (connection.ready && connection.reachable) {
    return {
      status: "synced",
      label: "ready",
      detail: message || "the last probe authenticated",
    };
  }
  if (connection.ready) {
    return {
      status: "degraded",
      label: "unreachable",
      detail: message || NO_MESSAGE,
    };
  }
  if (connection.reachable) {
    return {
      status: "degraded",
      label: "not ready",
      detail: message || NO_MESSAGE,
    };
  }
  return message === ""
    ? { status: "unknown", label: "not observed", detail: NOT_OBSERVED }
    : { status: "failed", label: "not ready", detail: message };
}

/**
 * Which of ADR-0033 decision 2's two credential kinds this connection carries.
 *
 * An unrecognised value is `unknown auth` rather than a guess: the enum grows
 * with the adapters, and a build that has not been regenerated must not draw a
 * kind it has never heard of as one it has.
 */
export function authLabel(kind: GitAuthKind): string {
  switch (kind) {
    case GitAuthKind.GITHUB_APP:
      return "GitHub App";
    case GitAuthKind.TOKEN:
      return "token";
    default:
      return "unknown auth";
  }
}

/**
 * The app's public identifiers, for a connection that has them.
 *
 * A zero installation ID is not missing data: the app exists from the manifest
 * callback and is installed a step later, so the gap between them is a real
 * state a connection sits in — and the one that explains a `Ready=False` with
 * nothing wrong.
 */
export function appIdentity(connection: GitConnection): string | undefined {
  if (connection.authKind !== GitAuthKind.GITHUB_APP) return undefined;
  const installed =
    connection.installationId === 0n
      ? "not installed yet"
      : `installation ${connection.installationId}`;
  return `app ${connection.appId} · ${installed}`;
}

/**
 * The repository count, when one was actually reported.
 *
 * Zero means "an installation with nothing selected" and zero also means
 * "nobody has asked", so the count is shown only alongside a probe that
 * answered. `undefined` is the screen's cue to say it was not reported rather
 * than to print a number the provider never gave.
 */
export function repositoriesLabel(
  connection: GitConnection,
): string | undefined {
  if (!connection.reachable) return undefined;
  const n = connection.repositories;
  return `${n} ${n === 1 ? "repository" : "repositories"}`;
}

/**
 * The owner as recorded — never as a permission.
 *
 * An absent owner is instance-owned, which is what internal/model says a nil
 * owner means and what CreateConnection defaults to. An unrecognised kind is
 * not flattened into `instance`: that would be inventing an answer for a value
 * this build cannot read.
 */
export function ownerLabel(owner: GitOwner | undefined): string {
  const kind = owner?.kind ?? GitOwnerKind.UNSPECIFIED;
  const name = owner?.name.trim() ?? "";
  switch (kind) {
    case GitOwnerKind.UNSPECIFIED:
    case GitOwnerKind.INSTANCE:
      return "instance";
    case GitOwnerKind.USER:
      return name === "" ? "user" : `user · ${name}`;
    case GitOwnerKind.TEAM:
      return name === "" ? "team" : `team · ${name}`;
    default:
      return "unknown owner kind";
  }
}

/**
 * True when the connection records no principal — the day-one shape, and the
 * only one whose label and whose behaviour agree today.
 */
export function ownerIsInstance(owner: GitOwner | undefined): boolean {
  const kind = owner?.kind ?? GitOwnerKind.UNSPECIFIED;
  return kind === GitOwnerKind.INSTANCE || kind === GitOwnerKind.UNSPECIFIED;
}

/** The two providers `CreateConnection` accepts (internal/model: GitProviders). */
export const PROVIDERS = ["github", "generic"] as const;

export type GitProviderName = (typeof PROVIDERS)[number];

export interface ConnectionForm {
  name: string;
  provider: GitProviderName;
  host: string;
  secretRef: string;
}

export const EMPTY_FORM: ConnectionForm = {
  name: "",
  provider: "github",
  host: DEFAULT_GITHUB_HOST,
  secretRef: "",
};

export type ConnectionField = "name" | "host" | "secretRef";

export interface ConnectionProblem {
  field: ConnectionField;
  message: string;
}

/**
 * Switching provider fills the host in, and never overwrites a typed one.
 *
 * `github` is the only provider with a default worth writing down, so it is
 * offered as one; every other forge has to say where it lives. The current
 * value is replaced only when it is still the other provider's default —
 * anything a person typed is theirs, including a `generic` connection that
 * really does point at github.com.
 */
export function hostForProvider(
  next: GitProviderName,
  current: string,
): string {
  if (next === "github") return current.trim() === "" ? DEFAULT_GITHUB_HOST : current;
  return current.trim() === DEFAULT_GITHUB_HOST ? "" : current;
}

/**
 * What the browser refuses to send, and nothing more.
 *
 * The server owns validation — it holds the taxonomy, the JSONPaths and the
 * remediations (internal/model's validateGitConnection), and a second copy of
 * those rules here would drift. What is checked is the shape the request must
 * have to be worth sending at all: the two names that become a `metadata.name`
 * and a Secret name, and that the host is a URL rather than a bare hostname —
 * which is the one field a reader gets wrong by typing what they would type
 * into a browser.
 *
 * `provider` is not checked because it cannot be wrong: it is a two-option
 * select, and the type says so.
 */
export function formProblems(form: ConnectionForm): ConnectionProblem[] {
  const out: ConnectionProblem[] = [];

  const name = dnsLabelProblem(form.name.trim(), "a connection name");
  if (name !== undefined) out.push({ field: "name", message: name });

  const host = form.host.trim();
  if (host === "") {
    out.push({
      field: "host",
      // No default: kelson will not silently claim github.com for a forge
      // nobody named.
      message: "a forge base URL is required",
    });
  } else if (!/^https?:\/\/\S+$/.test(host)) {
    // HTTP(S) only — an SSH remote is a different credential class.
    out.push({
      field: "host",
      message: `"${host}" is not a forge base URL — include the scheme, e.g. ${DEFAULT_GITHUB_HOST}`,
    });
  }

  const secret = dnsLabelProblem(form.secretRef.trim(), "a Secret name");
  if (secret !== undefined) out.push({ field: "secretRef", message: secret });

  return out;
}
