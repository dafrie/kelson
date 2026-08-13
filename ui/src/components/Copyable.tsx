import { useCallback, useState } from "react";

/**
 * A mono value you can click to copy: revision shas, error codes, namespaces.
 *
 * These are the values a reader retypes into a terminal, and retyping a sha is
 * how the wrong revision gets rolled back. Plain navigator.clipboard, no
 * dependency; where the browser refuses (an insecure origin, a denied
 * permission) the label says so rather than pretending it worked.
 */
export function Copyable({
  value,
  label,
  title,
  className,
}: {
  value: string;
  label?: string;
  /**
   * The tooltip, and the marker that `label` is an *abbreviation* rather than a
   * substitute name.
   *
   * The two cases differ in what the label stands for. A label alone stands in
   * for something unprintable — a whole spec document — and is therefore the
   * accessible name too. A label with a title is a shortening of a value that
   * is perfectly printable, just long: an image digest elided in the middle. In
   * that case the whole value belongs on the tooltip *and* in the accessible
   * name, because a screen reader announcing "ghcr.io/acme/che…0b2c4" would be
   * reading out the ellipsis this is only supposed to draw.
   */
  title?: string;
  className?: string;
}) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");

  const copy = useCallback(() => {
    const clipboard = navigator.clipboard;
    if (!clipboard) {
      setState("failed");
      return;
    }
    clipboard.writeText(value).then(
      () => {
        setState("copied");
        setTimeout(() => setState("idle"), 1200);
      },
      () => setState("failed"),
    );
  }, [value]);

  return (
    <button
      type="button"
      className={className ? `k-copy ${className}` : "k-copy"}
      onClick={copy}
      title={
        state === "failed"
          ? "the browser refused clipboard access"
          : (title ?? label ?? `copy ${value}`)
      }
      // A label alone is given when the value is too long to be its own name —
      // a whole spec document, say — and then the label is the accessible name.
      // A label with a title is only an abbreviation of a printable value, so
      // the name stays the whole of it.
      aria-label={title === undefined ? (label ?? `copy ${value}`) : `copy ${value}`}
      data-state={state}
    >
      <span className="k-copy__value">{label ?? value}</span>
      <span className="k-copy__flag" aria-hidden="true">
        {state === "copied" ? "copied" : state === "failed" ? "blocked" : ""}
      </span>
    </button>
  );
}
