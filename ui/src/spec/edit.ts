import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import {
  bindingEnv,
  envValueText,
  errorTarget,
  namedEnv,
  plainEnv,
  secretEnv,
  yamlScalar,
  type EnvValue,
  type EnvVar,
} from "./documents";

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
 * order, a component kind this form does not model, a flow sequence, an anchor —
 * is absent from the rebuild and shows up as a byte difference. There is no way
 * for this module to silently drop something and still claim the document is
 * editable.
 *
 * That is why ADR-0014's wider kind set needs nothing here. The parser reads
 * exactly the fields the builder writes, which are the workload fields; a
 * `kind: postgres` component (or an agent, or a `preset:`) is a key the parser
 * captures nowhere, so the rebuild omits it, the bytes differ, and the document
 * goes to the YAML tab whole. Teaching the parser to *read* a data component
 * without teaching the form to edit one would break that symmetry and turn the
 * byte guard from a proof into a hope.
 *
 * The parser is deliberately small and deliberately strict. It reads the
 * restricted grammar the builders emit (2-space indentation, block mappings,
 * block sequences, flow mappings for `replicas` and for the two env reference
 * forms, plain and double-quoted scalars) and gives up on everything else. It
 * is not a YAML implementation and must never grow into one: a document it
 * cannot read costs the reader the form tab, which is the correct outcome, not
 * a broken edit.
 *
 * # Env values, and exactly what round-trips (ADR-0018)
 *
 * An env value is one of three things — a scalar, `{secret, key}` or
 * `{from: {service, key}}` — and the form displays all three as themselves
 * (src/spec/documents.ts: EnvValue). What it can *write back* byte-faithfully
 * is narrower than what it can read, and the difference is the byte guard's to
 * enforce rather than anyone's to remember:
 *
 *   - **Round-trips byte-identically:** the single-line flow styling this
 *     module writes, with its spacing and its key order —
 *     `KEY: { secret: db, key: url }`,
 *     `KEY: { from: { service: db, key: uri } }` — and plain (unquoted) inner
 *     scalars. That is the styling ADR-0018 and docs/model.md show, and the
 *     styling `kelson secret set` prints.
 *   - **Read and displayed, then read-only:** the same references written as
 *     block mappings (`KEY:` on its own line, `secret:`/`key:` under it), a
 *     block `from:`, and a flow mapping whose keys are in the other order
 *     (`{ key: url, secret: db }`). These parse into the identical edit state,
 *     so the form shows the reference correctly; the rebuild emits the
 *     canonical flow styling, the bytes differ, and the document goes to the
 *     YAML tab whole. Nothing is lost and nothing is rewritten silently.
 *   - **Refused outright, no form at all:** a flow mapping with different
 *     spacing (`{secret: db,key: url}`), a quoted inner scalar, a mapping whose
 *     key set is neither reference form, and any plain env value whose text
 *     begins with `{`. The last is the one deliberate refusal rather than a
 *     consequence: a mapping this reader could not decode must never reach the
 *     form as a *string* that happens to look like one, because that would show
 *     a reader a credential reference as ordinary configuration.
 */

/** The two documents of a spec, as text. The store's own shape. */
export interface SpecTextSet {
  project: string;
  environments: Record<string, string>;
}

/** One component's editable fields. Every value is the raw text of an input. */
export interface ComponentEdit {
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
  components: ComponentEdit[];
}

/**
 * The delivery stanza, as fields.
 *
 * `mode` empty means the document carries no `delivery:` block at all, which is
 * the model's default (direct). It is a field rather than a checkbox because
 * the block's whole content is the mode plus, for the modes that need one, a
 * git target — there is nothing else to switch on.
 */
export interface DeliveryEdit {
  mode: string;
  gitRepo: string;
  gitBranch: string;
  gitPath: string;
}

/**
 * The previews stanza (ADR-0017), as fields.
 *
 * `enabled` is the block's presence: previews are declared or they are not, and
 * there is no `enabled:` key in the schema to confuse it with. The two label
 * lists are held as the comma-separated text a reader types, not as arrays,
 * because the canonical styling is a flow sequence — `[deploy/preview]` — and
 * the text a reader edits and the bytes the builder writes then differ only by
 * the brackets. `limit` is text for the same reason `port` is: it round-trips
 * as written rather than through a number the form would have to re-render.
 */
export interface PreviewsEdit {
  enabled: boolean;
  provider: string;
  repo: string;
  secretRef: string;
  interval: string;
  filterLabels: string;
  includeBranch: string;
  excludeBranch: string;
  limit: string;
  skipLabels: string;
  artifactsRepository: string;
  artifactsSecretRef: string;
}

export interface EnvironmentEdit {
  name: string;
  project: string;
  namespace: string;
  delivery: DeliveryEdit;
  previews: PreviewsEdit;
}

export function emptyDelivery(): DeliveryEdit {
  return { mode: "", gitRepo: "", gitBranch: "", gitPath: "" };
}

/**
 * A previews block with nothing filled in but the two defaults kelson itself
 * applies, so a reader who turns previews on sees the interval and the ceiling
 * the cluster would enforce rather than having to know them (ADR-0017).
 */
export function emptyPreviews(): PreviewsEdit {
  return {
    enabled: false,
    provider: "github",
    repo: "",
    secretRef: "",
    interval: "",
    filterLabels: "",
    includeBranch: "",
    excludeBranch: "",
    limit: "",
    skipLabels: "",
    artifactsRepository: "",
    artifactsSecretRef: "",
  };
}

export interface SpecEdit {
  project: ProjectEdit;
  environments: EnvironmentEdit[];
}

/** The kind-derivation rules of docs/model.md, mirrored from documents.ts. */
export function componentWorkload(c: ComponentEdit): "service" | "worker" | "cron" {
  if (c.schedule.trim() !== "") return "cron";
  if (c.port.trim() !== "") return "service";
  return "worker";
}

/* -------------------------------------------------------------- the builder */

/**
 * The Project document, in examples/hello-single's shape.
 *
 * This is a superset of what documents.ts writes for a new app and produces
 * byte-identical output for that subset — which is what makes the round-trip
 * guard usable at all: a document the create form stored yesterday is editable
 * today. Components are separated by a blank line, as examples/checkout-multi
 * writes them; a single component is therefore unchanged from #63's output.
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
      lines.push(`    ${yamlScalar(key)}: ${envValueText(value)}`);
    }
  }

  lines.push("", "  components:");
  p.components.forEach((component, i) => {
    if (i > 0) lines.push("");
    lines.push(...componentLines(component));
  });

  return lines.join("\n") + "\n";
}

function componentLines(app: ComponentEdit): string[] {
  const kind = componentWorkload(app);
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
      lines.push(`        ${yamlScalar(key)}: ${envValueText(value)}`);
    }
  }
  return lines;
}

/** Whether a field was filled in at all — the builder's one presence rule. */
function set(value: string): boolean {
  return value.trim() !== "";
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
  // Key order is the model's own (internal/model/environment.go): delivery
  // before previews, because that is the order a reader of the Go type and of
  // docs/model.md meets them in, and because the block that decides whether
  // previews may exist at all belongs above the block that declares them.
  lines.push(...deliveryLines(e.delivery));
  lines.push(...previewsLines(e.previews));
  return lines.join("\n") + "\n";
}

function deliveryLines(d: DeliveryEdit): string[] {
  if (!set(d.mode)) return [];
  const lines = ["  delivery:", `    mode: ${yamlScalar(d.mode)}`];
  // A git target is written when there is one to write. Flux and argocd need
  // one (semantic/git-target-missing) and the server says so about the missing
  // field; writing an empty stanza here would put the refusal on `repo` instead
  // of on the block, which is a worse place for it.
  if (set(d.gitRepo) || set(d.gitBranch) || set(d.gitPath)) {
    lines.push("    git:", `      repo: ${yamlScalar(d.gitRepo)}`);
    if (set(d.gitBranch)) lines.push(`      branch: ${yamlScalar(d.gitBranch)}`);
    if (set(d.gitPath)) lines.push(`      path: ${yamlScalar(d.gitPath)}`);
  }
  return lines;
}

function previewsLines(p: PreviewsEdit): string[] {
  if (!p.enabled) return [];
  // provider, repo, secretRef and artifacts.repository are the schema's
  // required fields, so they are always written — an incomplete block reaches
  // the server and comes back with the server's own finding on the field it is
  // about, which is the whole point of checking before saving.
  const lines = [
    "  previews:",
    `    provider: ${yamlScalar(p.provider)}`,
    `    repo: ${yamlScalar(p.repo)}`,
    `    secretRef: ${yamlScalar(p.secretRef)}`,
  ];
  if (set(p.interval)) lines.push(`    interval: ${yamlScalar(p.interval)}`);

  const labels = labelList(p.filterLabels);
  const filter: string[] = [];
  if (labels.length > 0) filter.push(`      labels: ${flowSequence(labels)}`);
  if (set(p.includeBranch)) {
    filter.push(`      includeBranch: ${yamlScalar(p.includeBranch)}`);
  }
  if (set(p.excludeBranch)) {
    filter.push(`      excludeBranch: ${yamlScalar(p.excludeBranch)}`);
  }
  if (set(p.limit)) filter.push(`      limit: ${p.limit.trim()}`);
  if (filter.length > 0) lines.push("    filter:", ...filter);

  const skip = labelList(p.skipLabels);
  if (skip.length > 0) {
    lines.push("    skip:", `      labels: ${flowSequence(skip)}`);
  }

  lines.push("    artifacts:", `      repository: ${yamlScalar(p.artifactsRepository)}`);
  if (set(p.artifactsSecretRef)) {
    lines.push(`      secretRef: ${yamlScalar(p.artifactsSecretRef)}`);
  }
  return lines;
}

/** The comma-separated text a reader types, as the list the document holds. */
export function labelList(text: string): string[] {
  return text
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
}

/** The list as the text a reader edits. The inverse of [labelList]. */
export function labelText(values: readonly string[]): string {
  return values.join(", ");
}

/**
 * A one-line flow sequence, the styling ADR-0017 and docs/model.md show:
 * `[deploy/preview-pause, "!ci/passed"]`. Block sequences would be equally
 * valid YAML and are what the component builder writes for `domains:`; the
 * difference is that a label list is short and reads as one value, and matching
 * the documented styling is what lets a block pasted from the docs stay
 * editable in the form.
 */
function flowSequence(values: readonly string[]): string {
  return `[${values.map(yamlScalar).join(", ")}]`;
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

  const nodes = spec.get("components");
  if (!Array.isArray(nodes)) return undefined;
  const components: ComponentEdit[] = [];
  for (const node of nodes) {
    const component = readComponent(node);
    if (component === undefined) return undefined;
    components.push(component);
  }

  return { name, image, env, components };
}

/**
 * One component, in the workload vocabulary the form edits.
 *
 * `kind:`, `preset:` and `tools:` are deliberately absent: a component carrying
 * any of them is a component this form cannot edit, and the round-trip guard
 * turns that into a read-only document rather than a lossy one.
 */
function readComponent(node: YNode): ComponentEdit | undefined {
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
  for (const [key, node2] of node) {
    const value = readEnvValue(node2);
    if (value === undefined) return undefined;
    out.push({ key, value });
  }
  return out;
}

/**
 * One env value as the union it is (ADR-0018): a scalar is a value, a mapping
 * is a reference, and which reference is decided by its own key.
 *
 * Both stylings of both mapping forms arrive here identically — the YAML-lite
 * reader has already turned a flow mapping and a block mapping into the same
 * Map — so this decides only the shape, and the byte guard decides whether the
 * document can be written back. A mapping whose keys are neither reference form
 * is refused rather than guessed at: `schema/invalid-format` is the server's
 * answer to it and inventing a third arm here would put the browser ahead of
 * the model.
 */
function readEnvValue(node: YNode): EnvValue | undefined {
  if (typeof node === "string") {
    // A value that begins with `{` is a flow mapping this reader could not
    // decode, never a string somebody meant. Letting it through would draw a
    // credential reference in the form as ordinary configuration.
    return node.startsWith("{") ? undefined : plainEnv(node);
  }
  if (!isMap(node)) return undefined;

  if (node.size === 2 && node.has("secret") && node.has("key")) {
    const secret = node.get("secret");
    const key = node.get("key");
    if (typeof secret !== "string" || typeof key !== "string") return undefined;
    return secretEnv(secret, key);
  }

  if (node.size === 1 && node.has("from")) {
    const from = node.get("from");
    if (!isMap(from) || from.size !== 2) return undefined;
    const service = from.get("service");
    const key = from.get("key");
    if (typeof service !== "string" || typeof key !== "string") return undefined;
    return bindingEnv(service, key);
  }

  return undefined;
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

  const delivery = readDelivery(spec.get("delivery"));
  if (delivery === undefined) return undefined;
  const previews = readPreviews(spec.get("previews"));
  if (previews === undefined) return undefined;

  return { name, project, namespace, delivery, previews };
}

function readDelivery(node: YNode | undefined): DeliveryEdit | undefined {
  const out = emptyDelivery();
  if (node === undefined) return out;
  if (!isMap(node)) return undefined;

  const mode = node.get("mode") ?? "";
  if (typeof mode !== "string") return undefined;
  out.mode = mode;

  const git = node.get("git");
  if (git !== undefined) {
    if (!isMap(git)) return undefined;
    const fields = readStrings(git, ["repo", "branch", "path"]);
    if (fields === undefined) return undefined;
    out.gitRepo = fields.repo ?? "";
    out.gitBranch = fields.branch ?? "";
    out.gitPath = fields.path ?? "";
  }
  return out;
}

/**
 * The previews block, in the vocabulary the form edits (ADR-0017).
 *
 * Reading it is wider than writing it, as everywhere else in this module: a
 * block sequence of labels, a `limit` written as `10`, and keys in another
 * order all parse into the same edit state, and the byte guard is what decides
 * whether the form may write it back. Nothing is lost either way — the document
 * that cannot be rebuilt goes to the YAML tab whole.
 */
function readPreviews(node: YNode | undefined): PreviewsEdit | undefined {
  const out = emptyPreviews();
  if (node === undefined) return out;
  if (!isMap(node)) return undefined;
  out.enabled = true;

  const top = readStrings(node, ["provider", "repo", "secretRef", "interval"]);
  if (top === undefined) return undefined;
  out.provider = top.provider ?? "";
  out.repo = top.repo ?? "";
  out.secretRef = top.secretRef ?? "";
  out.interval = top.interval ?? "";

  const filter = node.get("filter");
  if (filter !== undefined) {
    if (!isMap(filter)) return undefined;
    const labels = readStringList(filter.get("labels"));
    if (labels === undefined) return undefined;
    out.filterLabels = labelText(labels);
    const fields = readStrings(filter, ["includeBranch", "excludeBranch", "limit"]);
    if (fields === undefined) return undefined;
    out.includeBranch = fields.includeBranch ?? "";
    out.excludeBranch = fields.excludeBranch ?? "";
    out.limit = fields.limit ?? "";
  }

  const skip = node.get("skip");
  if (skip !== undefined) {
    if (!isMap(skip)) return undefined;
    const labels = readStringList(skip.get("labels"));
    if (labels === undefined) return undefined;
    out.skipLabels = labelText(labels);
  }

  const artifacts = node.get("artifacts");
  if (artifacts !== undefined) {
    if (!isMap(artifacts)) return undefined;
    const fields = readStrings(artifacts, ["repository", "secretRef"]);
    if (fields === undefined) return undefined;
    out.artifactsRepository = fields.repository ?? "";
    out.artifactsSecretRef = fields.secretRef ?? "";
  }
  return out;
}

/** The named keys as strings, or undefined if any of them is not one. */
function readStrings(
  node: Map<string, YNode>,
  keys: readonly string[],
): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const key of keys) {
    const value = node.get(key);
    if (value === undefined) continue;
    if (typeof value !== "string") return undefined;
    out[key] = value;
  }
  return out;
}

/** A sequence of plain scalars, in either styling. Absent is empty. */
function readStringList(node: YNode | undefined): string[] | undefined {
  if (node === undefined) return [];
  if (!Array.isArray(node)) return undefined;
  if (!node.every((v): v is string => typeof v === "string")) return undefined;
  return node;
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
/**
 * One flow mapping wrapping one more, which is exactly `{ from: { service: …,
 * key: … } }` and nothing wider. The nesting is one level because the model has
 * one nested form; a general flow parser would be a YAML implementation, which
 * this module's comment forbids.
 */
const FLOW_NESTED = /^\{ ([A-Za-z0-9_][A-Za-z0-9_.-]*): (\{ [^{}]* \}) \}$/;

/**
 * A one-line flow sequence of scalars: `[]`, `[a]`, `[a, b]`.
 *
 * It is here because it is the styling ADR-0017 and docs/model.md write label
 * lists in, so a previews block pasted from the documentation reaches the form
 * rather than the YAML tab. The grammar stays one level deep and scalar-only:
 * an element containing a comma or a bracket is refused, which is what keeps
 * `[a,b]` — a spacing this reader cannot reproduce — off the form entirely
 * instead of silently reading one label as two or as "a,b".
 */
const FLOW_SEQ = /^\[(.*)\]$/;

function decodeSequence(inner: string): YNode[] | undefined {
  if (inner.trim() === "") return [];
  const out: YNode[] = [];
  for (const part of inner.split(", ")) {
    if (/[,[\]{}]/.test(part)) return undefined;
    const value = decodeScalar(part);
    if (value === undefined) return undefined;
    out.push(value);
  }
  return out;
}

function decodeValue(text: string): YNode | undefined {
  const seq = FLOW_SEQ.exec(text);
  if (seq?.[1] !== undefined) return decodeSequence(seq[1]);

  const nested = FLOW_NESTED.exec(text);
  if (nested?.[1] !== undefined && nested[2] !== undefined) {
    const inner = decodeValue(nested[2]);
    if (inner === undefined) return undefined;
    return new Map<string, YNode>([[nested[1], inner]]);
  }

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
 * The edit form reaches every component the document declares, not only the
 * first, so an error at `$.spec.components[1].port` has an input to land on
 * here where the create form had none. Keys are dotted paths rather than the
 * create form's flat union for the same reason: they carry the index and the
 * environment name.
 */
export type EditFieldKey = string;

export function editFieldForError(error: WireError): EditFieldKey | undefined {
  const target = errorTarget(error);
  if (target === undefined) return undefined;
  if (target.doc === "environment") {
    switch (target.on) {
      case "namespace":
        return `environment.${target.environment}.namespace`;
      case "delivery": {
        const field = deliveryField(target.field);
        return field === undefined
          ? undefined
          : `environment.${target.environment}.delivery.${field}`;
      }
      case "previews": {
        const field = previewsField(target.field);
        return field === undefined
          ? undefined
          : `environment.${target.environment}.previews.${field}`;
      }
      default:
        return undefined;
    }
  }
  switch (target.on) {
    case "name":
      return "project.name";
    case "image":
      return "project.image";
    case "env":
      return `project.env.${target.name}`;
    case "component-env":
      return `component.${target.index}.env.${target.name}`;
    case "component":
      // A component-level finding with no subfield is about the whole
      // component; the image is the field the form can act on.
      if (target.field === "whole") {
        return error.code === "semantic/no-image-source"
          ? `component.${target.index}.image`
          : undefined;
      }
      return `component.${target.index}.${target.field}`;
  }
}

/**
 * A `$.spec.delivery...` path onto the input that holds it. A whole-stanza
 * finding — `semantic/git-target-missing` points at `$.spec.delivery.git`, the
 * block rather than a key — lands on the repository, which is the field that
 * makes it go away.
 */
function deliveryField(field: string): string | undefined {
  if (field === "" || field === "mode") return "mode";
  if (field === "git" || field === "git.repo") return "gitRepo";
  if (field === "git.branch") return "gitBranch";
  if (field === "git.path") return "gitPath";
  return undefined;
}

/**
 * A `$.spec.previews...` path onto the input that holds it.
 *
 * The indexed forms matter: validation reports a bad label as
 * `$.spec.previews.filter.labels[1]`, and the form has one input for the whole
 * list, so the index is dropped rather than turned into a field nothing owns.
 */
function previewsField(field: string): string | undefined {
  const base = field.replace(/\[\d+\]$/, "");
  switch (base) {
    case "provider":
      return "provider";
    case "repo":
      return "repo";
    case "secretRef":
      return "secretRef";
    case "interval":
      return "interval";
    case "filter.labels":
      return "filterLabels";
    case "filter.includeBranch":
      return "includeBranch";
    case "filter.excludeBranch":
      return "excludeBranch";
    case "filter.limit":
      return "limit";
    case "skip.labels":
      return "skipLabels";
    case "artifacts.repository":
      return "artifactsRepository";
    case "artifacts.secretRef":
      return "artifactsSecretRef";
    default:
      return undefined;
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
