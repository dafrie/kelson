import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import { errorTarget, yamlScalar, type EnvVar } from "./documents";

/**
 * Editing a spec that is already stored (#65).
 *
 * The store is byte-faithful — the spec is the user's document, comments and
 * key order included (ADR-0013 §1) — and that is the whole difficulty here.
 * Creating a document (#63) is a one-way street: the builder writes bytes and
 * nobody else has an opinion about them. Editing one means reading bytes back
 * into fields and writing them out again, and a form that regenerates the
 * document from parsed state destroys every comment and every deliberate key
 * order it did not know about.
 *
 * The strategy shipped is neither "always rebuild" nor line-level splicing:
 *
 *   1. Parse the stored document into edit state.
 *   2. Rebuild a document from that state.
 *   3. If the rebuild is byte-identical to what was stored, the UI wrote this
 *      document and nobody has touched it since — so the form may edit it, by
 *      rebuilding, and nothing can be lost.
 *   4. Otherwise the document has something in it this module cannot express.
 *      The form goes read-only and says so; the YAML tab is where that document
 *      is edited, with its formatting intact.
 *
 * Step 3 is a *total* guard, not a heuristic, and that is what makes the simple
 * strategy honest: anything the parser fails to capture — a comment, a key
 * order, a `services:` block, a flow sequence, an anchor — is absent from the
 * rebuild and shows up as a byte difference. There is no way for this module to
 * silently drop something and still claim the document is editable.
 *
 * The parser is deliberately small and deliberately strict. It reads the
 * restricted grammar the builders emit (2-space indentation, block mappings,
 * block sequences, one flow mapping for `replicas`, plain and double-quoted
 * scalars) and gives up on everything else. It is not a YAML implementation and
 * must never grow into one: a document it cannot read costs the reader the form
 * tab, which is the correct outcome, not a broken edit.
 */

/** The two documents of a spec, as text. The store's own shape. */
export interface SpecTextSet {
  project: string;
  environments: Record<string, string>;
}

/** One application's editable fields. Every value is the raw text of an input. */
export interface AppEdit {
  name: string;
  image: string;
  port: string;
  health: string;
  schedule: string;
  domains: string[];
  replicasMin: string;
  replicasMax: string;
  env: EnvVar[];
}

export interface ProjectEdit {
  name: string;
  image: string;
  env: EnvVar[];
  applications: AppEdit[];
}

export interface EnvironmentEdit {
  name: string;
  project: string;
  namespace: string;
}

export interface SpecEdit {
  project: ProjectEdit;
  environments: EnvironmentEdit[];
}

/** The workload rules of docs/model.md, mirrored from documents.ts. */
export function appWorkload(app: AppEdit): "service" | "worker" | "cron" {
  if (app.schedule.trim() !== "") return "cron";
  if (app.port.trim() !== "") return "service";
  return "worker";
}

/* -------------------------------------------------------------- the builder */

/**
 * The Project document, in examples/hello-single's shape.
 *
 * This is a superset of what documents.ts writes for a new app and produces
 * byte-identical output for that subset — which is what makes the round-trip
 * guard usable at all: a document the create form stored yesterday is editable
 * today. Applications are separated by a blank line, as examples/checkout-multi
 * writes them; a single application is therefore unchanged from #63's output.
 */
export function buildProjectDocument(p: ProjectEdit): string {
  const env = namedEnv(p.env);
  const lines = [
    "apiVersion: kelson.dev/v1alpha1",
    "kind: Project",
    "metadata:",
    `  name: ${yamlScalar(p.name)}`,
    "",
    "spec:",
  ];
  if (set(p.image)) lines.push(`  image: ${yamlScalar(p.image)}`);
  if (env.length > 0) {
    lines.push("", "  env:");
    for (const { key, value } of env) {
      lines.push(`    ${yamlScalar(key)}: ${yamlScalar(value)}`);
    }
  }

  lines.push("", "  applications:");
  p.applications.forEach((app, i) => {
    if (i > 0) lines.push("");
    lines.push(...applicationLines(app));
  });

  return lines.join("\n") + "\n";
}

function applicationLines(app: AppEdit): string[] {
  const kind = appWorkload(app);
  const domains = app.domains.filter(set);
  const env = namedEnv(app.env);
  const lines = [`    - name: ${yamlScalar(app.name)}`];
  if (set(app.image)) lines.push(`      image: ${yamlScalar(app.image)}`);
  if (kind === "service") {
    lines.push(`      port: ${app.port}`);
    if (set(app.health)) lines.push(`      health: ${yamlScalar(app.health)}`);
  }
  if (kind === "cron") lines.push(`      schedule: ${yamlScalar(app.schedule)}`);
  if (kind === "service" && domains.length > 0) {
    lines.push("      domains:");
    for (const domain of domains) lines.push(`        - ${yamlScalar(domain)}`);
  }
  if (set(app.replicasMin)) {
    lines.push(
      set(app.replicasMax)
        ? `      replicas: { min: ${app.replicasMin}, max: ${app.replicasMax} }`
        : `      replicas: { min: ${app.replicasMin} }`,
    );
  }
  if (env.length > 0) {
    lines.push("      env:");
    for (const { key, value } of env) {
      lines.push(`        ${yamlScalar(key)}: ${yamlScalar(value)}`);
    }
  }
  return lines;
}

/** Whether a field was filled in at all — the builder's one presence rule. */
function set(value: string): boolean {
  return value.trim() !== "";
}

/**
 * The variables that have a name.
 *
 * A row with no name is a row the user has started and not finished, and the
 * form keeps showing it; the document does not, because `"": value` is not a
 * variable. It is the same rule documents.ts applies on create.
 */
function namedEnv(env: EnvVar[]): EnvVar[] {
  return env.map(({ key, value }) => ({ key: key.trim(), value })).filter((e) => e.key !== "");
}

export function buildEnvironmentDocument(e: EnvironmentEdit): string {
  const lines = [
    "apiVersion: kelson.dev/v1alpha1",
    "kind: Environment",
    "metadata:",
    `  name: ${yamlScalar(e.name)}`,
    "",
    "spec:",
    `  project: ${yamlScalar(e.project)}`,
  ];
  if (set(e.namespace)) lines.push(`  namespace: ${yamlScalar(e.namespace)}`);
  return lines.join("\n") + "\n";
}

export function writeSpec(edit: SpecEdit): SpecTextSet {
  const environments: Record<string, string> = {};
  for (const env of edit.environments) {
    environments[env.name] = buildEnvironmentDocument(env);
  }
  return { project: buildProjectDocument(edit.project), environments };
}

/* --------------------------------------------------------------- the reader */

export function parseProjectDocument(text: string): ProjectEdit | undefined {
  const doc = readDocument(text);
  if (doc === undefined || !isMap(doc)) return undefined;
  if (doc.get("apiVersion") !== "kelson.dev/v1alpha1") return undefined;
  if (doc.get("kind") !== "Project") return undefined;

  const metadata = doc.get("metadata");
  const spec = doc.get("spec");
  if (!isMap(metadata) || !isMap(spec)) return undefined;
  const name = metadata.get("name");
  if (typeof name !== "string") return undefined;

  const image = spec.get("image") ?? "";
  if (typeof image !== "string") return undefined;

  const env = readEnv(spec.get("env"));
  if (env === undefined) return undefined;

  const apps = spec.get("applications");
  if (!Array.isArray(apps)) return undefined;
  const applications: AppEdit[] = [];
  for (const node of apps) {
    const app = readApplication(node);
    if (app === undefined) return undefined;
    applications.push(app);
  }

  return { name, image, env, applications };
}

function readApplication(node: YNode): AppEdit | undefined {
  if (!isMap(node)) return undefined;
  const name = node.get("name");
  if (typeof name !== "string") return undefined;

  const scalars: Record<string, string> = {};
  for (const key of ["image", "port", "health", "schedule"]) {
    const value = node.get(key) ?? "";
    if (typeof value !== "string") return undefined;
    scalars[key] = value;
  }

  const domainsNode = node.get("domains");
  let domains: string[] = [];
  if (domainsNode !== undefined) {
    if (!Array.isArray(domainsNode)) return undefined;
    if (!domainsNode.every((d): d is string => typeof d === "string")) return undefined;
    domains = domainsNode;
  }

  const replicas = node.get("replicas");
  let replicasMin = "";
  let replicasMax = "";
  if (replicas !== undefined) {
    if (!isMap(replicas)) return undefined;
    const min = replicas.get("min");
    const max = replicas.get("max") ?? "";
    if (typeof min !== "string" || typeof max !== "string") return undefined;
    replicasMin = min;
    replicasMax = max;
  }

  const env = readEnv(node.get("env"));
  if (env === undefined) return undefined;

  return {
    name,
    image: scalars.image ?? "",
    port: scalars.port ?? "",
    health: scalars.health ?? "",
    schedule: scalars.schedule ?? "",
    domains,
    replicasMin,
    replicasMax,
    env,
  };
}

function readEnv(node: YNode | undefined): EnvVar[] | undefined {
  if (node === undefined) return [];
  if (!isMap(node)) return undefined;
  const out: EnvVar[] = [];
  for (const [key, value] of node) {
    if (typeof value !== "string") return undefined;
    out.push({ key, value });
  }
  return out;
}

export function parseEnvironmentDocument(text: string): EnvironmentEdit | undefined {
  const doc = readDocument(text);
  if (doc === undefined || !isMap(doc)) return undefined;
  if (doc.get("apiVersion") !== "kelson.dev/v1alpha1") return undefined;
  if (doc.get("kind") !== "Environment") return undefined;

  const metadata = doc.get("metadata");
  const spec = doc.get("spec");
  if (!isMap(metadata) || !isMap(spec)) return undefined;

  const name = metadata.get("name");
  const project = spec.get("project");
  const namespace = spec.get("namespace") ?? "";
  if (typeof name !== "string" || typeof project !== "string") return undefined;
  if (typeof namespace !== "string") return undefined;

  return { name, project, namespace };
}

export function readSpec(text: SpecTextSet): SpecEdit | undefined {
  const project = parseProjectDocument(text.project);
  if (project === undefined) return undefined;
  const environments: EnvironmentEdit[] = [];
  for (const name of Object.keys(text.environments).sort()) {
    const doc = text.environments[name];
    const parsed = doc === undefined ? undefined : parseEnvironmentDocument(doc);
    if (parsed === undefined) return undefined;
    environments.push(parsed);
  }
  return { project, environments };
}

/**
 * Whether the form may edit these documents.
 *
 * True exactly when reading and rewriting them reproduces the stored bytes. A
 * false answer is never a claim that the document is wrong — only that editing
 * it through fields would rewrite it, and rewriting someone's file is not
 * something a UI gets to do quietly.
 */
export function isRebuildable(text: SpecTextSet): boolean {
  const edit = readSpec(text);
  return edit !== undefined && sameText(writeSpec(edit), text);
}

export function sameText(a: SpecTextSet, b: SpecTextSet): boolean {
  if (a.project !== b.project) return false;
  const keys = Object.keys(a.environments);
  if (keys.length !== Object.keys(b.environments).length) return false;
  return keys.every((k) => a.environments[k] === b.environments[k]);
}

/* ------------------------------------------------------------- YAML-lite */

type YNode = string | YNode[] | Map<string, YNode>;

function isMap(node: YNode | undefined): node is Map<string, YNode> {
  return node instanceof Map;
}

interface Line {
  indent: number;
  content: string;
}

/**
 * The document as a tree, or undefined when it is outside the grammar.
 *
 * Refusing is cheap and always safe: it puts the document on the YAML tab.
 * Comments are *skipped* rather than refused, so a hand-annotated document
 * still fills the form — read-only, because the rebuild will not carry the
 * comment and the byte guard says so. Showing a reader their own configuration
 * is worth more than refusing to look at it.
 */
function readDocument(text: string): YNode | undefined {
  const lines: Line[] = [];
  for (const raw of text.split("\n")) {
    if (raw.trim() === "") continue;
    if (/^\s*#/.test(raw)) continue;
    if (raw.includes("\t")) return undefined;
    if (/^(?:---|\.\.\.)/.test(raw)) return undefined;
    const trimmed = raw.trimEnd();
    const indent = trimmed.length - trimmed.trimStart().length;
    lines.push({ indent, content: trimmed.slice(indent) });
  }
  if (lines.length === 0) return undefined;
  if (lines[0]?.indent !== 0) return undefined;

  const read = parseNode(lines, 0, 0);
  if (read === undefined || read[1] !== lines.length) return undefined;
  return read[0];
}

function parseNode(lines: Line[], i: number, indent: number): [YNode, number] | undefined {
  const content = lines[i]?.content ?? "";
  return content === "-" || content.startsWith("- ")
    ? parseSequence(lines, i, indent)
    : parseMapping(lines, i, indent);
}

const KEY = /^([A-Za-z0-9_][A-Za-z0-9_.-]*):(?: (.*))?$/;

function parseMapping(
  lines: Line[],
  start: number,
  indent: number,
): [YNode, number] | undefined {
  const out = new Map<string, YNode>();
  let i = start;
  while (i < lines.length) {
    const line = lines[i];
    if (line === undefined || line.indent !== indent) break;
    const match = KEY.exec(line.content);
    if (match?.[1] === undefined) break;
    const key = match[1];
    if (out.has(key)) return undefined;
    const inline = match[2];
    if (inline === undefined) {
      const next = lines[i + 1];
      if (next !== undefined && next.indent > indent) {
        const child = parseNode(lines, i + 1, next.indent);
        if (child === undefined) return undefined;
        out.set(key, child[0]);
        i = child[1];
        continue;
      }
      out.set(key, "");
      i += 1;
      continue;
    }
    const value = decodeValue(inline);
    if (value === undefined) return undefined;
    out.set(key, value);
    i += 1;
  }
  if (i === start) return undefined;
  return [out, i];
}

function parseSequence(
  lines: Line[],
  start: number,
  indent: number,
): [YNode, number] | undefined {
  const out: YNode[] = [];
  let i = start;
  while (i < lines.length) {
    const line = lines[i];
    if (line === undefined || line.indent !== indent) break;
    if (line.content !== "-" && !line.content.startsWith("- ")) break;
    const head = line.content === "-" ? "" : line.content.slice(2);

    let end = i + 1;
    while (end < lines.length && (lines[end]?.indent ?? -1) > indent) end += 1;

    if (KEY.test(head)) {
      // A mapping item: the text after "- " is its first key, one level in.
      const item: Line[] = [
        { indent: indent + 2, content: head },
        ...lines.slice(i + 1, end),
      ];
      const parsed = parseMapping(item, 0, indent + 2);
      if (parsed === undefined || parsed[1] !== item.length) return undefined;
      out.push(parsed[0]);
    } else {
      if (end !== i + 1) return undefined;
      const value = decodeValue(head);
      if (value === undefined) return undefined;
      out.push(value);
    }
    i = end;
  }
  if (i === start) return undefined;
  return [out, i];
}

const FLOW = /^\{ (.*) \}$/;
const FLOW_ENTRY = /^([A-Za-z0-9_][A-Za-z0-9_.-]*): ([^,{}"]*)$/;

function decodeValue(text: string): YNode | undefined {
  const flow = FLOW.exec(text);
  if (flow?.[1] !== undefined) {
    const out = new Map<string, YNode>();
    for (const part of flow[1].split(", ")) {
      const entry = FLOW_ENTRY.exec(part);
      if (entry?.[1] === undefined || entry[2] === undefined) return undefined;
      if (out.has(entry[1])) return undefined;
      out.set(entry[1], entry[2]);
    }
    return out;
  }
  return decodeScalar(text);
}

/**
 * A scalar back to its string. `yamlScalar` writes double-quoted scalars with
 * `JSON.stringify`, so `JSON.parse` is its exact inverse; a plain scalar is its
 * own text, which is what keeps `port: 8080` an editable "8080" rather than a
 * number the form would have to re-render.
 */
function decodeScalar(text: string): string | undefined {
  if (!text.startsWith('"')) {
    return text.includes("#") || text.includes("'") ? undefined : text;
  }
  try {
    const value: unknown = JSON.parse(text);
    return typeof value === "string" ? value : undefined;
  } catch {
    return undefined;
  }
}

/* ------------------------------------------------------- server error paths */

/**
 * Which edit-form input a structured error belongs to.
 *
 * The edit form reaches every application the document declares, not only the
 * first, so an error at `$.spec.applications[1].port` has an input to land on
 * here where the create form had none. Keys are dotted paths rather than the
 * create form's flat union for the same reason: they carry the index and the
 * environment name.
 */
export type EditFieldKey = string;

export function editFieldForError(error: WireError): EditFieldKey | undefined {
  const target = errorTarget(error);
  if (target === undefined) return undefined;
  if (target.doc === "environment") {
    return target.on === "namespace"
      ? `environment.${target.environment}.namespace`
      : undefined;
  }
  switch (target.on) {
    case "name":
      return "project.name";
    case "image":
      return "project.image";
    case "env":
      return `project.env.${target.name}`;
    case "app-env":
      return `app.${target.index}.env.${target.name}`;
    case "app":
      // An application-level finding with no subfield is about the whole
      // application; the image is the field the form can act on.
      if (target.field === "whole") {
        return error.code === "semantic/no-image-source"
          ? `app.${target.index}.image`
          : undefined;
      }
      return `app.${target.index}.${target.field}`;
  }
}

export interface MappedEditErrors {
  byField: Map<EditFieldKey, WireError[]>;
  general: WireError[];
}

export function mapEditErrors(errors: readonly WireError[]): MappedEditErrors {
  const byField = new Map<EditFieldKey, WireError[]>();
  const general: WireError[] = [];
  for (const error of errors) {
    const field = editFieldForError(error);
    if (field === undefined) {
      general.push(error);
      continue;
    }
    const existing = byField.get(field);
    if (existing === undefined) byField.set(field, [error]);
    else existing.push(error);
  }
  return { byField, general };
}
