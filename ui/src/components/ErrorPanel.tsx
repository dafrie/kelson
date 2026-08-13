import { toFailure } from "../api/errors";
import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import "./ErrorPanel.css";

/**
 * The one way this UI renders a remote failure.
 *
 * When the error carries kelson.v1alpha1.Error details, they are what is shown:
 * the code as a mono chip (agents and humans both branch on it), the message,
 * the remediation as a "fix:" line — the CLI's own shape — and docs_url as a
 * link. When it carries none, the message stands alone, plus the one hint worth
 * printing: a connection refused means kelson-server is not running.
 *
 * Errors returned *inline* (RenderResponse.errors, DiffResponse.errors, the
 * Settled events) are already kelson.v1alpha1.Error values, so they are passed
 * as `errors` and rendered by the same component. The schema is explicit that
 * an invalid spec is an answer rather than a transport failure, and the two
 * must not look different to the reader.
 */

const SERVER_HINT =
  "kelson-server --listen 127.0.0.1:8420\n" +
  "Nothing answered on the API origin. Start the server, or check the dev proxy in vite.config.ts.";

export function ErrorPanel({
  title,
  error,
  errors,
}: {
  title: string;
  /** A thrown error — a ConnectError or anything else. */
  error?: unknown;
  /** Structured errors returned inline by a successful RPC. */
  errors?: readonly WireError[];
}) {
  const failure = error === undefined ? undefined : toFailure(error);
  const wire = [...(failure?.wire ?? []), ...(errors ?? [])];

  return (
    <div className="k-error" role="alert">
      <div className="k-error__head">
        <span className="k-error__title">{title}</span>
        {failure?.code ? (
          <span className="k-error__rpc k-mono">rpc {failure.code}</span>
        ) : null}
      </div>

      {wire.length > 0 ? (
        <ul className="k-error__list">
          {wire.map((e, i) => (
            <WireErrorRow key={`${e.code}:${e.field}:${i}`} error={e} />
          ))}
        </ul>
      ) : null}

      {/* The RPC message is redundant once the structured details are shown —
          the Go side builds it from the same error — so it prints only when
          there are none. */}
      {wire.length === 0 && failure ? (
        <p className="k-error__message">{failure.message}</p>
      ) : null}

      {failure?.unreachable ? (
        <pre className="k-error__hint">{SERVER_HINT}</pre>
      ) : null}
    </div>
  );
}

function WireErrorRow({ error }: { error: WireError }) {
  const where = [
    error.resource,
    error.application,
    error.target,
    error.overlay,
    error.field,
  ].filter((s) => s !== "");
  const position = error.line > 0 ? `line ${error.line}:${error.column}` : "";

  return (
    <li className="k-error__item">
      <div className="k-error__row">
        {error.code ? <code className="k-error__code">{error.code}</code> : null}
        {where.length > 0 ? (
          <span className="k-error__where k-mono">{where.join(" · ")}</span>
        ) : null}
        {position ? (
          <span className="k-error__where k-mono">{position}</span>
        ) : null}
      </div>
      <p className="k-error__message">{error.message}</p>
      {error.cause ? (
        <p className="k-error__cause k-mono">cause: {error.cause}</p>
      ) : null}
      {error.remediation ? (
        <p className="k-error__fix">
          <span className="k-error__fix-label">fix:</span> {error.remediation}
        </p>
      ) : null}
      {error.docsUrl ? (
        <a
          className="k-error__docs"
          href={error.docsUrl}
          target="_blank"
          rel="noreferrer"
        >
          {error.docsUrl}
        </a>
      ) : null}
    </li>
  );
}
