/**
 * The smallest YAML reader that answers two questions, and no more.
 *
 * Two documents reach this screen as bytes rather than as fields: the stored
 * spec (ADR-0013 §1 — the store keeps what was authored) and the ClusterProfile
 * (proto/kelson/v1alpha1/profile.proto — "the YAML tags on
 * internal/clusterprofile are the schema of record", so the API deliberately
 * does not duplicate them as proto messages). Both are produced by kelson: the
 * profile by `yaml.Marshal` in internal/clusterprofile/yaml.go, the spec by a
 * person or by src/spec/documents.ts.
 *
 * So this reads the grammar those producers emit — block mappings, block
 * sequences, one-line flow mappings, plain and quoted scalars, trailing
 * comments — and gives up on everything else. It is not a YAML implementation
 * and must not become one; ui/README.md records that adding a YAML dependency
 * is a decision rather than a convenience, and every caller here treats "could
 * not read it" as "say nothing", never as "report absence".
 */

/**
 * The items of the first block sequence under `key`, each as its own top-level
 * scalar fields.
 *
 * Only fields at the item's own indentation are collected, so a nested block
 * (`env:`, `resources:`, `crds:`) is skipped whole and cannot contribute a key
 * that shadows one of the item's own.
 */
export function sequenceItems(doc: string, key: string): Map<string, string>[] {
  const found = findBlock(doc, key);
  if (found === undefined) return [];

  const items: Map<string, string>[] = [];
  let current: Map<string, string> | undefined;
  let fieldIndent = -1;

  for (const line of found.body) {
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("#")) continue;
    const indent = line.length - line.trimStart().length;

    if (trimmed.startsWith("-") && indent >= found.indent) {
      const rest = trimmed.slice(1).trimStart();
      // Where the item's own fields begin: the column of the first thing after
      // the dash, which is where a block item's later keys line up.
      fieldIndent = indent + (trimmed.length - rest.length);
      current = new Map();
      items.push(current);
      if (rest.startsWith("{")) {
        for (const [k, v] of flowPairs(rest)) current.set(k, v);
        // A flow item is complete on its own line; nothing may follow it.
        fieldIndent = -1;
      } else {
        const pair = keyValue(rest);
        if (pair !== undefined) current.set(pair[0], pair[1]);
      }
      continue;
    }
    if (indent <= found.indent) break; // the sequence ended: a sibling key
    if (current === undefined || indent !== fieldIndent) continue; // nested block
    const pair = keyValue(trimmed);
    if (pair !== undefined) current.set(pair[0], pair[1]);
  }
  return items;
}

/**
 * The scalar fields of the block mapping under `key`, or undefined when the
 * document has no such key at all.
 *
 * The distinction matters: an absent key and a key whose fields are all nested
 * blocks are different facts, and every caller of this treats a missing
 * mapping as "not detected" only because the profile's own encoding omits
 * absent components entirely (a nil pointer marshals to nothing).
 */
export function mappingFields(
  doc: string,
  key: string,
): Map<string, string> | undefined {
  const found = findBlock(doc, key);
  if (found === undefined) return undefined;

  const fields = new Map<string, string>();
  let fieldIndent = -1;
  for (const line of found.body) {
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("#")) continue;
    const indent = line.length - line.trimStart().length;
    if (indent <= found.indent) break;
    if (fieldIndent < 0) fieldIndent = indent;
    if (indent !== fieldIndent) continue; // a nested block's contents
    const pair = keyValue(trimmed);
    if (pair !== undefined) fields.set(pair[0], pair[1]);
  }
  return fields;
}

interface Block {
  /** Indentation of the key line, which is what ends the block. */
  indent: number;
  /** Everything after the key line. */
  body: string[];
}

function findBlock(doc: string, key: string): Block | undefined {
  const lines = doc.split("\n");
  const pattern = new RegExp(`^(\\s*)${key}:\\s*(#.*)?$`);
  for (let i = 0; i < lines.length; i++) {
    const match = pattern.exec(lines[i] ?? "");
    if (match) {
      return { indent: (match[1] ?? "").length, body: lines.slice(i + 1) };
    }
  }
  return undefined;
}

/** `key: value` as a pair, or undefined when the text is not one. */
function keyValue(text: string): [string, string] | undefined {
  const at = text.indexOf(":");
  if (at <= 0) return undefined;
  const key = text.slice(0, at).trim();
  if (key === "" || /[\s{}[\]]/.test(key)) return undefined;
  return [key, scalar(text.slice(at + 1))];
}

/**
 * A scalar value: quotes removed, trailing comment removed.
 *
 * A `#` inside a quoted scalar is part of the value, which is why the quoted
 * case is handled before the comment is cut.
 */
function scalar(text: string): string {
  const value = text.trim();
  if (value.startsWith('"')) {
    const end = value.indexOf('"', 1);
    return end < 0 ? value.slice(1) : value.slice(1, end);
  }
  if (value.startsWith("'")) {
    const end = value.indexOf("'", 1);
    return end < 0 ? value.slice(1) : value.slice(1, end);
  }
  const hash = value.indexOf("#");
  return (hash < 0 ? value : value.slice(0, hash)).trim();
}

/**
 * The top-level pairs of a single-line flow mapping, `{name: db, kind: postgres}`.
 * A nested flow mapping (`replicas: {min: 1}`) is kept out of the split by depth
 * and its contents are not read — nothing any caller needs lives inside one.
 */
function flowPairs(text: string): [string, string][] {
  const close = text.lastIndexOf("}");
  const body = close < 0 ? text.slice(1) : text.slice(1, close);
  const parts: string[] = [];
  let depth = 0;
  let from = 0;
  let at = 0;
  for (const char of body) {
    if (char === "{" || char === "[") depth++;
    else if (char === "}" || char === "]") depth--;
    else if (char === "," && depth === 0) {
      parts.push(body.slice(from, at));
      from = at + 1;
    }
    at++;
  }
  parts.push(body.slice(from));

  const out: [string, string][] = [];
  for (const part of parts) {
    if (part.trim() === "" || part.includes("{") || part.includes("[")) continue;
    const pair = keyValue(part.trim());
    if (pair !== undefined) out.push(pair);
  }
  return out;
}
