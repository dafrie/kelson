import { Code, ConnectError } from "@connectrpc/connect";
import { codeToString } from "@connectrpc/connect/protocol-connect";

import {
  ErrorSchema,
  type Error as WireError,
} from "../gen/kelson/v1alpha1/common_pb";

/**
 * Decoding the API's one structured error shape.
 *
 * kelson.v1alpha1.Error is the field union of model.Error, renderer.Error,
 * delivery.Error and serverstate.Error (proto/kelson/v1alpha1/common.proto);
 * the Go side attaches every one an error carries as a ConnectRPC error detail
 * (internal/api/errors.go, `fail`). Connect-ES v2 exposes them through
 * `ConnectError.findDetails(desc)`, which decodes the google.protobuf.Any
 * wrappers and drops details of other types — so passing ErrorSchema yields
 * exactly the kelson errors and nothing else.
 *
 * Codes are never re-mapped here. "delivery/unsupported" reaches the screen as
 * the string the owning Go package defines, because the wire must not grow a
 * second taxonomy and neither must the UI.
 */
export interface Failure {
  /** Human-readable summary, always populated. */
  message: string;
  /** ConnectRPC code name ("unavailable", "unimplemented", …), when known. */
  code: string | undefined;
  /** Structured errors from the wire; empty when the error carried none. */
  wire: WireError[];
  /** True when the shape says nothing is listening, not that a call failed. */
  unreachable: boolean;
}

/** Substrings browsers and Node use for "nothing is listening on that port". */
const REFUSED = [
  "econnrefused",
  "failed to fetch",
  "fetch failed",
  "networkerror",
  "load failed",
];

export function toFailure(err: unknown): Failure {
  if (err instanceof ConnectError) {
    return {
      message: err.rawMessage,
      code: codeToString(err.code),
      wire: err.findDetails(ErrorSchema),
      unreachable:
        err.code === Code.Unavailable &&
        looksRefused(`${err.rawMessage} ${describeCause(err.cause)}`),
    };
  }
  const message = err instanceof Error ? err.message : String(err);
  return {
    message,
    code: undefined,
    wire: [],
    unreachable: looksRefused(message),
  };
}

/**
 * True when the store refused a write because the spec is not what the writer
 * thought it was.
 *
 * One shape, two meanings, and the caller supplies which: on a create it can
 * only mean the name is taken, because a create carries no version and
 * internal/serverstate refuses an empty expected version against an existing
 * object; on an update it means someone else wrote in between. The connect code
 * is checked as well as the detail, because a store error that arrives without
 * its details is still a failed precondition.
 */
export function isVersionConflict(err: unknown): boolean {
  const failure = toFailure(err);
  return (
    failure.wire.some((e) => e.code === "store/version-conflict") ||
    failure.code === "failed_precondition"
  );
}

/**
 * The codes a connection uses to say it cannot be browsed (internal/api's
 * `connection/…` family, ADR-0033 decision 3).
 *
 * Both mean the same thing to a picker and neither means the connection is
 * broken: `capability-unsupported` is a forge whose adapter implements no
 * repository browser, and `provider-unknown` is one this build has no adapter
 * for at all. A connection in either state still mints credentials and clones,
 * which is the capability a deploy needs — so the answer to both is the pasted
 * URL, not a repair.
 */
const BROWSE_REFUSALS = [
  "connection/capability-unsupported",
  "connection/provider-unknown",
];

/**
 * The server's own words for why a connection cannot be browsed, or undefined
 * when the failure was something else.
 *
 * The message and the remediation are returned rather than a boolean, because
 * the refusal is written to be *read*: it names what the connection can do and
 * says that pasting a URL works, which is the entire next step for whoever hit
 * it. A picker that reduced it to "not supported" would throw away the half
 * that tells them they are not stuck.
 */
export function browseRefusal(err: unknown): string | undefined {
  const wire = toFailure(err).wire.find((e) =>
    BROWSE_REFUSALS.includes(e.code),
  );
  if (wire === undefined) return undefined;
  return [wire.message, wire.remediation]
    .map((part) => part.trim())
    .filter((part) => part !== "")
    .join(" — ");
}

/** True when we aborted the request ourselves, which is not a failure. */
export function isAbort(err: unknown): boolean {
  if (err instanceof ConnectError) return err.code === Code.Canceled;
  return err instanceof DOMException && err.name === "AbortError";
}

function describeCause(cause: unknown): string {
  if (cause instanceof Error) {
    return `${cause.message} ${describeCause(cause.cause)}`;
  }
  return cause === undefined || cause === null ? "" : String(cause);
}

function looksRefused(text: string): boolean {
  const haystack = text.toLowerCase();
  return REFUSED.some((needle) => haystack.includes(needle));
}
