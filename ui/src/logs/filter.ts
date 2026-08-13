import type { LogLine } from "../gen/kelson/v1alpha1/logs_pb";

/**
 * Finding things in a tail that is still moving, and telling replicas apart.
 *
 * This is deliberately *client-side*. `LogService` takes a `LogMatch` and the
 * bounded Query mode uses it — that is the right place for a filter over a
 * window the server is about to read. A live Follow is the opposite case: a
 * match sent upstream restarts the stream and throws away every line that did
 * not match, permanently, so clearing the box later shows a gap rather than the
 * lines that were always there. Filtering the retained buffer instead means the
 * query is instant, applies to what is already on screen, and is reversible.
 *
 * The consequence is that this is a find-in-page, not grep(1): both modes are
 * case-insensitive, because a reader typing `timeout` into a log view is not
 * making a claim about case. The case-sensitive contract is the server's
 * `LogMatch`, and that is still what Query sends.
 */

/** Half-open [start, end) offsets into a line's message. */
export type Range = readonly [number, number];

export interface LineFilter {
  /** Where the query hit. Empty means this line does not match. */
  ranges(text: string): Range[];
}

export type Filtering =
  | { kind: "all" }
  | { kind: "filter"; filter: LineFilter }
  | { kind: "invalid"; message: string };

/**
 * An empty query is not a filter that matches everything — it is the absence of
 * one, and the difference is visible: nothing is highlighted and nothing is
 * hidden. An unparseable regex is reported rather than silently treated as a
 * substring, because a filter that quietly means something else is how a reader
 * concludes their logs are empty.
 */
export function compileFilter(query: string, isRegex: boolean): Filtering {
  if (query === "") return { kind: "all" };
  if (!isRegex) return { kind: "filter", filter: substringFilter(query) };
  try {
    // `g` to walk every hit for highlighting, `i` for the case rule above.
    const re = new RegExp(query, "gi");
    return { kind: "filter", filter: regexFilter(re) };
  } catch (err) {
    return {
      kind: "invalid",
      message: err instanceof Error ? err.message : String(err),
    };
  }
}

function substringFilter(query: string): LineFilter {
  const needle = query.toLowerCase();
  return {
    ranges(text) {
      const hay = text.toLowerCase();
      const out: Range[] = [];
      let at = hay.indexOf(needle);
      while (at >= 0) {
        out.push([at, at + needle.length]);
        at = hay.indexOf(needle, at + needle.length);
      }
      return out;
    },
  };
}

function regexFilter(re: RegExp): LineFilter {
  return {
    ranges(text) {
      const out: Range[] = [];
      re.lastIndex = 0;
      let match = re.exec(text);
      while (match !== null) {
        const start = match.index;
        const end = start + match[0].length;
        // A pattern that can match nothing (`x*`) would otherwise spin here
        // forever, and a zero-width hit is not something to paint.
        if (end === start) {
          re.lastIndex = start + 1;
          if (re.lastIndex > text.length) break;
        } else {
          out.push([start, end]);
        }
        match = re.exec(text);
      }
      return out;
    },
  };
}

export interface Segment {
  text: string;
  hit: boolean;
}

/** Splits a message into plain and matched runs, in order, with no gaps. */
export function segments(text: string, ranges: readonly Range[]): Segment[] {
  if (ranges.length === 0) return [{ text, hit: false }];
  const out: Segment[] = [];
  let at = 0;
  for (const [start, end] of ranges) {
    // Overlapping or out-of-order ranges would otherwise duplicate text.
    if (end <= at) continue;
    const from = Math.max(at, start);
    if (from > at) out.push({ text: text.slice(at, from), hit: false });
    out.push({ text: text.slice(from, end), hit: true });
    at = end;
  }
  if (at < text.length) out.push({ text: text.slice(at), hit: false });
  return out;
}

/**
 * How many pod tones the palette defines (`--kelson-pod-1` … `-8` in
 * src/styles/tokens.css, in both themes).
 */
export const POD_TONES = 8;

/**
 * A stable colour for a pod name.
 *
 * Multi-replica merging is only readable if the same replica is the same colour
 * from one glance to the next, and pods come and go, so the colour is derived
 * from the name rather than handed out in arrival order. FNV-1a: the point is
 * an even spread over eight buckets, not cryptography. Collisions are expected
 * and harmless — the name is printed beside the colour, which is what
 * identifies the pod.
 */
export function podTone(pod: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < pod.length; i += 1) {
    hash ^= pod.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return (hash % POD_TONES) + 1;
}

/** The pods present in a buffer, in first-seen order — the filter's options. */
export function podsOf(lines: readonly LogLine[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const line of lines) {
    if (line.pod === "" || seen.has(line.pod)) continue;
    seen.add(line.pod);
    out.push(line.pod);
  }
  return out;
}

export interface VisibleLine {
  line: LogLine;
  /** Where the filter hit this line's message; empty when there is no filter. */
  ranges: Range[];
  /**
   * Position in the buffer this line came from. With the buffer's eviction
   * count it makes a key that survives both scrolling and filtering, so React
   * reuses rows instead of rebuilding the list on every frame.
   */
  index: number;
}

/**
 * What the reader is actually looking at: the retained buffer, narrowed.
 *
 * An empty pod set means every pod, not none — the control starts with nothing
 * ticked, and a filter that hid everything until you chose would be a screen
 * that opens blank. The query is matched against the message only: the pod and
 * container have their own control, and highlighting a substring of a pod name
 * would suggest the two filters are one.
 */
export function visibleLines(
  lines: readonly LogLine[],
  filtering: Filtering,
  pods: ReadonlySet<string>,
): VisibleLine[] {
  const out: VisibleLine[] = [];
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    if (line === undefined) continue;
    if (pods.size > 0 && !pods.has(line.pod)) continue;
    if (filtering.kind === "filter") {
      const ranges = filtering.filter.ranges(line.message);
      if (ranges.length === 0) continue;
      out.push({ line, ranges, index });
    } else {
      // An invalid regex filters nothing: the message says what is wrong, and
      // blanking the view while someone types `[` mid-pattern helps no one.
      out.push({ line, ranges: [], index });
    }
  }
  return out;
}
