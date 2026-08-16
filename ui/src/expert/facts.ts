/**
 * The Kubernetes-shaped facts already on the wire, read out of the strings that
 * carry them (#260).
 *
 * Expert mode's whole rule is that it *surfaces* rather than *computes*: every
 * value it prints is something a response already said, and where a response
 * cannot answer, nothing is drawn. So this module is two parsers and no
 * inference. Both are strict — a shape they do not recognise yields `undefined`
 * and the caller renders nothing — because a format that has changed is far
 * more likely than a value that is safe to take apart anyway, and a wrong
 * generation number on a dense screen is worse than an absent one.
 *
 * What is deliberately NOT here, and why:
 *
 *   - **The resource kinds a component would render.** A `service` becomes a
 *     Deployment and a `postgres` a CloudNativePG `Cluster`, but that mapping
 *     lives in `internal/renderer` and the browser would be quoting it from
 *     memory. Where the cluster actually reported an object, its verdict names
 *     it (`resourceParts` below) and that name is a fact; where it did not,
 *     expert mode says nothing rather than predicting one.
 *   - **How far behind a stale revision is.** `StatusResponse` carries `stale`
 *     as a boolean and the *spec's* current generation is not on the message at
 *     all, so "45 vs 47" is not a subtraction anything here can do. The
 *     revision's own generation is printable; the gap is not.
 */

/**
 * A revision tag taken apart: `<generation>-<hash8>`.
 *
 * That spelling is the controller's (ADR-0028 decision 2) and it is why
 * `StatusResponse.stale` needs no second read — the generation the artifact was
 * published for is *in* the tag. The generation is the Environment object's
 * `.metadata.generation` at publish time, which is the number an operator
 * correlates with `kubectl get environment`, and it is the one Kubernetes fact
 * on this screen that is currently visible only as a prefix nobody explains.
 */
export interface RevisionParts {
  /** `.metadata.generation` the artifact was published for, as digits. */
  generation: string;
  /** The short spec hash the rest of the tag is. */
  specHash: string;
}

/**
 * Strict: digits, a hyphen, and lower-case hex. A revision that does not match
 * — an older spine's git sha, a tag from a future scheme — is returned as
 * nothing, and callers keep printing the tag itself, which is always honest.
 */
const REVISION = /^(\d+)-([0-9a-f]+)$/;

export function revisionParts(revision: string): RevisionParts | undefined {
  const match = REVISION.exec(revision);
  if (match === null) return undefined;
  const [, generation = "", specHash = ""] = match;
  return { generation, specHash };
}

/**
 * A verdict's `resource` taken apart: `Kind/namespace/name`.
 *
 * Observation names what it probed and the UI already prints that string whole
 * on a verdict row. Expert mode splits it because the three parts answer three
 * different questions — which kind of object was probed, which namespace it is
 * in, what it is called — and only the middle one is otherwise on screen.
 */
export interface ResourceParts {
  kind: string;
  namespace: string;
  name: string;
}

export function resourceParts(resource: string): ResourceParts | undefined {
  const parts = resource.split("/");
  if (parts.length !== 3) return undefined;
  const [kind = "", namespace = "", name = ""] = parts;
  if (kind === "" || namespace === "" || name === "") return undefined;
  return { kind, namespace, name };
}
