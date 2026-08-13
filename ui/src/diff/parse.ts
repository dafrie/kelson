/**
 * Decoder for the bytes DiffResponse.diff_json and RollbackResponse.Preview
 * carry: internal/diff's Diff, encoded by diff.EncodeJSON (json.MarshalIndent).
 *
 * The Go types in internal/diff/diff.go are the authority for every field name
 * and every vocabulary value; this file mirrors them and adds nothing. Fields
 * tagged `omitempty` on the Go side are genuinely absent from the JSON, so
 * everything optional is decoded defensively — a diff with no violations has no
 * `violations` key at all, and reading that as an error would make the clean
 * case the broken one.
 *
 * Decoding is total: an unparseable or wrong-shaped payload returns an error
 * rather than a half-populated Diff, because a diff the UI half-understood is
 * exactly the thing a person would act on wrongly.
 */

export type DiffLevel = "rendered" | "server";
export type DiffOp = "added" | "modified" | "removed";
export type DiffRisk =
  | "cosmetic"
  | "additive"
  | "restart-required"
  | "disruptive";
export type DiffOrigin = "spec" | "overlay" | "admission" | "defaulting";
export type Enforcement = "enforce" | "audit";

export interface FieldDiff {
  path: string;
  before: unknown;
  after: unknown;
  origin: DiffOrigin | string;
  risk: DiffRisk | string;
  specPath: string;
}

export interface ResourceDiff {
  apiVersion: string;
  kind: string;
  name: string;
  namespace: string;
  op: DiffOp | string;
  risk: DiffRisk | string;
  fields: FieldDiff[];
}

export interface PolicyViolation {
  engine: string;
  policy: string;
  rule: string;
  resource: string;
  path: string;
  specPath: string;
  message: string;
  enforcement: Enforcement | string;
}

export interface Unvalidated {
  resource: string;
  requires: string;
  inBatch: boolean;
  message: string;
}

export interface DiffSummary {
  added: number;
  modified: number;
  removed: number;
  restarting: string[];
  disruptive: string[];
  maxRisk: DiffRisk | string;
}

export interface Diff {
  level: DiffLevel | string;
  project: string;
  environment: string;
  resources: ResourceDiff[];
  violations: PolicyViolation[];
  unvalidated: Unvalidated[];
  summary: DiffSummary;
  degraded: boolean;
  degradedReason: string;
}

export function decodeDiff(bytes: Uint8Array | string): Diff {
  const text =
    typeof bytes === "string" ? bytes : new TextDecoder().decode(bytes);
  if (text.trim() === "") {
    throw new Error("diff payload was empty");
  }
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (err) {
    throw new Error(
      `diff payload is not JSON: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
  if (!isRecord(raw)) {
    throw new Error("diff payload is not a JSON object");
  }
  return {
    level: str(raw.level),
    project: str(raw.project),
    environment: str(raw.environment),
    resources: list(raw.resources).map(resource),
    violations: list(raw.violations).map(violation),
    unvalidated: list(raw.unvalidated).map(unvalidated),
    summary: summary(raw.summary),
    degraded: bool(raw.degraded),
    degradedReason: str(raw.degradedReason),
  };
}

function resource(raw: unknown): ResourceDiff {
  const r = record(raw);
  return {
    apiVersion: str(r.apiVersion),
    kind: str(r.kind),
    name: str(r.name),
    namespace: str(r.namespace),
    op: str(r.op),
    risk: str(r.risk),
    fields: list(r.fields).map(field),
  };
}

function field(raw: unknown): FieldDiff {
  const f = record(raw);
  return {
    path: str(f.path),
    // before/after are `any` in Go: a scalar, a list or a whole object. They
    // are carried through untouched and rendered as JSON at the use site.
    before: f.before,
    after: f.after,
    origin: str(f.origin),
    risk: str(f.risk),
    specPath: str(f.specPath),
  };
}

function violation(raw: unknown): PolicyViolation {
  const v = record(raw);
  return {
    engine: str(v.engine),
    policy: str(v.policy),
    rule: str(v.rule),
    resource: str(v.resource),
    path: str(v.path),
    specPath: str(v.specPath),
    message: str(v.message),
    enforcement: str(v.enforcement),
  };
}

function unvalidated(raw: unknown): Unvalidated {
  const u = record(raw);
  return {
    resource: str(u.resource),
    requires: str(u.requires),
    inBatch: bool(u.inBatch),
    message: str(u.message),
  };
}

function summary(raw: unknown): DiffSummary {
  const s = isRecord(raw) ? raw : {};
  return {
    added: num(s.added),
    modified: num(s.modified),
    removed: num(s.removed),
    restarting: list(s.restarting).map(str),
    disruptive: list(s.disruptive).map(str),
    maxRisk: str(s.maxRisk),
  };
}

/**
 * The one-line roll-up, in the CLI's words (internal/diff/format.go's summary
 * line), so the same preview reads the same in both surfaces.
 */
export function summaryLine(d: Diff): string {
  const parts = [
    `${d.summary.added} added`,
    `${d.summary.modified} modified`,
    `${d.summary.removed} removed`,
  ];
  if (d.summary.restarting.length > 0) {
    parts.push(`restart: [${d.summary.restarting.join(" ")}]`);
  }
  if (d.summary.disruptive.length > 0) {
    parts.push(`disruptive: [${d.summary.disruptive.join(" ")}]`);
  }
  if (d.summary.maxRisk) parts.push(`max risk: ${d.summary.maxRisk}`);
  return parts.join(" · ");
}

/** Renders a FieldDiff before/after value the way the JSON carried it. */
export function formatValue(value: unknown): string {
  if (value === undefined) return "—";
  if (typeof value === "string") return JSON.stringify(value);
  return JSON.stringify(value) ?? String(value);
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function record(v: unknown): Record<string, unknown> {
  if (!isRecord(v)) throw new Error("diff payload: expected an object");
  return v;
}

function list(v: unknown): unknown[] {
  if (v === undefined || v === null) return [];
  if (!Array.isArray(v)) throw new Error("diff payload: expected a list");
  return v;
}

function str(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v !== "string") throw new Error("diff payload: expected a string");
  return v;
}

function num(v: unknown): number {
  if (v === undefined || v === null) return 0;
  if (typeof v !== "number") throw new Error("diff payload: expected a number");
  return v;
}

function bool(v: unknown): boolean {
  return v === true;
}
