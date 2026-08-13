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
  className,
}: {
  value: string;
  label?: string;
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
      title={state === "failed" ? "the browser refused clipboard access" : `copy ${value}`}
      aria-label={`copy ${value}`}
      data-state={state}
    >
      <span className="k-copy__value">{label ?? value}</span>
      <span className="k-copy__flag" aria-hidden="true">
        {state === "copied" ? "copied" : state === "failed" ? "blocked" : ""}
      </span>
    </button>
  );
}
