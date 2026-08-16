import type { ReactNode } from "react";

import "./expert.css";
import { useDetail } from "./preference";

/**
 * Content that exists only when Kubernetes detail is on (#260).
 *
 * The gate is a component rather than an `if` at every call site so that the
 * rule is greppable: `<Detail>` wraps information and nothing else. A reviewer
 * asking "does this preference hide a button" can answer it by reading what is
 * inside these, and the answer has to stay "no" — a control inside one would be
 * a control half the readers of this instance do not have.
 */
export function Detail({ children }: { children: ReactNode }) {
  return useDetail() ? <>{children}</> : null;
}

/**
 * One inline Kubernetes fact: a sans label and a mono value.
 *
 * Mono because the value is always something a machine produced — a
 * generation, a namespace, a kind, an object name — which is the rule the whole
 * type system here rests on (ui/README.md, "Mono means the machine produced
 * this exact string"). The label beside it is a word a person reads, so it is
 * not.
 *
 * An empty value renders nothing at all. Expert mode's contract is that what it
 * shows was on the wire; a label with a blank after it is a claim that the wire
 * sent an empty string.
 */
export function KubeFact({ name, value }: { name: string; value: string }) {
  if (value === "") return null;
  return (
    <span className="k-kfact">
      {name} <span className="k-mono">{value}</span>
    </span>
  );
}
