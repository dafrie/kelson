import type { ReactNode } from "react";

/**
 * A collapsible block, built on <details> so it is keyboard-operable and
 * findable by the browser's own find-in-page without any script.
 *
 * Used wherever a byte-faithful document is shown: the stored spec documents,
 * the rendered manifests. The bytes are printed as they arrived — the renderer
 * emits an ordered document (internal/renderer, ADR-0001) and re-serialising it
 * in the browser would silently reorder keys the author chose.
 */
export function Disclosure({
  summary,
  meta,
  children,
  open,
}: {
  summary: ReactNode;
  meta?: ReactNode;
  children: ReactNode;
  open?: boolean;
}) {
  return (
    <details className="k-disclosure" open={open ?? false}>
      <summary className="k-disclosure__summary">
        <span className="k-disclosure__label">{summary}</span>
        {meta ? <span className="k-disclosure__meta k-mono">{meta}</span> : null}
      </summary>
      <div className="k-disclosure__body">{children}</div>
    </details>
  );
}

export function YamlBlock({ bytes }: { bytes: Uint8Array | string }) {
  const text =
    typeof bytes === "string" ? bytes : new TextDecoder().decode(bytes);
  return (
    <pre className="k-pre k-panel k-panel--dim">
      <code>{text.replace(/\n+$/, "")}</code>
    </pre>
  );
}
