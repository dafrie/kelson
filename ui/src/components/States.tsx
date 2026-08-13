import type { ReactNode } from "react";

/**
 * Loading, empty and error rendered as first-class states.
 *
 * kelson-server binds loopback and is not running most of the time a developer
 * opens this UI, so "cannot reach the server" is the single most common thing
 * these screens display. It gets the same care as the happy path, and it names
 * the command that fixes it rather than printing a fetch error.
 */

export function LoadingState({ what }: { what: string }) {
  return (
    <div className="k-state k-state--loading" role="status" aria-live="polite">
      <span className="k-state__title">Loading {what}…</span>
    </div>
  );
}

export function EmptyState({
  title,
  children,
}: {
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="k-state">
      <span className="k-state__title">{title}</span>
      {children ? <div className="k-state__detail">{children}</div> : null}
    </div>
  );
}

export function ErrorState({
  title,
  detail,
  children,
}: {
  title: string;
  detail?: string;
  children?: ReactNode;
}) {
  return (
    <div className="k-state k-state--error" role="alert">
      <span className="k-state__title">{title}</span>
      {children}
      {detail ? <p className="k-state__detail">{detail}</p> : null}
    </div>
  );
}

/**
 * The error a browser reports for a server that is not listening is
 * indistinguishable from a dozen others, so the remedy is stated rather than
 * inferred: the address is kelson-server's own default (cmd/kelson-server
 * --listen 127.0.0.1:8420) and the dev proxy in vite.config.ts points at it.
 */
export function ServerUnreachableState({ detail }: { detail: string }) {
  return (
    <ErrorState title="Cannot reach kelson-server" detail={detail}>
      <p className="k-state__detail">
        Start it on the address the dev proxy expects:
        {"\n"}
        kelson-server --listen 127.0.0.1:8420
      </p>
    </ErrorState>
  );
}
