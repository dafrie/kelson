import { mappingFields, sequenceItems } from "../dataservices/miniyaml";

/**
 * The components a stored Project document declares, for screens that describe
 * one (#214).
 *
 * A Project is a container of components (ADR-0014, docs/model.md) and the API
 * hands the UI the authored bytes rather than a parsed list (ADR-0013 §1), so a
 * screen that wants to say what a project is made of has to read YAML. This
 * reads two keys per entry — the name, and whatever decides the kind — and
 * gives up on everything else.
 *
 * WHY THIS IS NOT src/spec/edit.ts's PARSER, again. That parser feeds a form
 * whose honesty rests on rebuilding the document byte-identically, so it must
 * read exactly what its builder writes and refuse everything else; a document
 * outside that grammar has no form at all. This one never writes anything back,
 * so it can be lenient, and it must be: a hand-written Project document is
 * still a project whose components a reader is entitled to see listed. It is
 * the same split, and the same reasoning, as src/dataservices/parse.ts — which
 * is why it borrows that module's YAML-lite reader rather than growing a third.
 *
 * A document this reader cannot follow yields no components, and the section
 * then says so rather than claiming the project has none.
 */

/**
 * The closed kind set of ADR-0014, plus the empty string for a `kind:` this
 * build has never heard of — which is shown as written rather than mapped onto
 * a kind we would be guessing at.
 */
export type ComponentKind =
  | "service"
  | "worker"
  | "cron"
  | "agent"
  | "postgres"
  | "valkey"
  | "helm";

const WRITTEN_KINDS = new Set<string>([
  "service",
  "worker",
  "cron",
  "agent",
  "postgres",
  "valkey",
  "helm",
]);

/** The kinds whose configuration is an operator's, not a workload's. */
const DATA_KINDS = new Set<string>(["postgres", "valkey"]);

export interface ComponentSummary {
  name: string;
  /** The kind: written when the document states one, derived otherwise. */
  kind: ComponentKind | string;
  /** True when the document stated the kind rather than the shape implying it. */
  written: boolean;
  /** The one field that names what this component is, if it has one. */
  port: string;
  schedule: string;
  /** Empty means the component inherits the Project's image or source (rule P3). */
  image: string;
  /** A data component's preset, empty when the document leaves it to the model. */
  preset: string;
  /** The source this component names, empty when it names none. */
  source: string;
}

/**
 * The kind-derivation table of docs/model.md: a written `kind:` when there is
 * one, otherwise `schedule:` → cron, `port:` → service, neither → worker.
 * Mirrors model.Component.DerivedKind and src/spec/edit.ts's componentWorkload
 * — schedule first, so the two cannot disagree about a document the model
 * refuses as mutually exclusive anyway.
 */
export function parseComponents(projectDoc: string): ComponentSummary[] {
  const out: ComponentSummary[] = [];
  for (const item of sequenceItems(projectDoc, "components")) {
    const name = item.get("name");
    if (name === undefined || name === "") continue;

    const written = item.get("kind") ?? "";
    const port = item.get("port") ?? "";
    const schedule = item.get("schedule") ?? "";
    out.push({
      name,
      kind:
        written !== ""
          ? written
          : schedule !== ""
            ? "cron"
            : port !== ""
              ? "service"
              : "worker",
      written: written !== "",
      port,
      schedule,
      image: item.get("image") ?? "",
      preset: item.get("preset") ?? "",
      source: item.get("source") ?? "",
    });
  }
  return out;
}

/**
 * What an Environment document says about one component (rules P2 and P3).
 *
 * Two fields, because two are what a screen can state without pretending to be
 * the resolver: the image pin, which is the one override a reader looks for,
 * and the replica override, which is read as written (`replicas: {min: 2}`
 * yields the flow mapping's text, not a resolved count).
 */
export interface ComponentOverride {
  image: string;
  replicas: string;
}

export function parseOverrides(
  environmentDoc: string,
): Map<string, ComponentOverride> {
  const out = new Map<string, ComponentOverride>();
  for (const item of sequenceItems(environmentDoc, "components")) {
    const name = item.get("name");
    if (name === undefined || name === "") continue;
    out.set(name, {
      image: item.get("image") ?? "",
      replicas: item.get("replicas") ?? "",
    });
  }
  return out;
}

/** The Project's own `spec.image`, which every component falls back to (P3). */
export function projectImage(projectDoc: string): string {
  return mappingFields(projectDoc, "spec")?.get("image") ?? "";
}

/** Where an effective value came from — rule P3's scopes, innermost first. */
export type ImageScope = "environment" | "component" | "project" | "none";

export interface EffectiveImage {
  image: string;
  scope: ImageScope;
}

/**
 * Rule P3 as a screen can state it: the innermost scope that names an image
 * wins, and the scope is reported beside the value because "which of my three
 * documents put this here" is the question the merge otherwise hides.
 *
 * A component with no image anywhere is not an error — it builds from its
 * source (ADR-0010) — so the absence is a scope of its own rather than a blank.
 */
export function effectiveImage(
  component: ComponentSummary,
  override: ComponentOverride | undefined,
  fromProject: string,
): EffectiveImage {
  if (override?.image) return { image: override.image, scope: "environment" };
  if (component.image !== "") {
    return { image: component.image, scope: "component" };
  }
  if (fromProject !== "") return { image: fromProject, scope: "project" };
  return { image: "", scope: "none" };
}

/** One entry of a Project's `sources:`, or the singular `source:` shorthand. */
export interface SpecSource {
  name: string;
  git: string;
  ref: string;
  /** The connection this source resolves through, empty when it names none. */
  connection: string;
  /** True for the one-entry list the singular `source:` shorthand declares. */
  shorthand: boolean;
}

/**
 * The repositories a Project declares (ADR-0035).
 *
 * Two spellings, one list: `sources:` is the list, and `source:` as a mapping
 * is the shorthand for the single entry named `default`. A document that uses
 * both is refused by the model, so reading the list first and the shorthand
 * only in its absence agrees with the server without duplicating its check.
 *
 * A component's `source:` is a *name* and never a mapping, so it cannot be
 * mistaken for the shorthand: the shorthand's key has no value on its line.
 */
export function parseSources(projectDoc: string): SpecSource[] {
  const listed = sequenceItems(projectDoc, "sources")
    .map((item) => ({
      name: item.get("name") ?? "",
      git: item.get("git") ?? "",
      ref: item.get("ref") ?? "",
      connection: item.get("connection") ?? "",
      shorthand: false,
    }))
    .filter((source) => source.name !== "" || source.git !== "");
  if (listed.length > 0) return listed;

  const single = mappingFields(projectDoc, "source");
  const git = single?.get("git") ?? "";
  if (git === "") return [];
  return [
    {
      name: "default",
      git,
      ref: single?.get("ref") ?? "",
      connection: single?.get("connection") ?? "",
      shorthand: true,
    },
  ];
}

/**
 * Which source a component builds from, and how that was decided.
 *
 * The rules are docs/model.md's: a named `source:` wins, otherwise the sole
 * entry (a list of one is not a decision), otherwise the entry called
 * `default`. What is *not* here is the second tier — the instance's `GitSource`
 * objects — because a Project document cannot see them. So a name this document
 * does not declare is reported as undeclared *here* rather than as unknown: it
 * may be a perfectly good global source, and saying otherwise would be a claim
 * this reader cannot make.
 */
export type BindingBasis =
  | "named"
  | "sole"
  | "default"
  | "undeclared"
  | "ambiguous"
  | "none";

export interface SourceBinding {
  basis: BindingBasis;
  /** The bound source, present only when the basis found one. */
  source: SpecSource | undefined;
  /** The name the component asked for, when it asked for one. */
  requested: string;
}

export function bindingFor(
  component: ComponentSummary,
  sources: readonly SpecSource[],
): SourceBinding {
  if (component.source !== "") {
    const named = sources.find((s) => s.name === component.source);
    return named === undefined
      ? { basis: "undeclared", source: undefined, requested: component.source }
      : { basis: "named", source: named, requested: component.source };
  }
  if (sources.length === 0) {
    return { basis: "none", source: undefined, requested: "" };
  }
  if (sources.length === 1) {
    return { basis: "sole", source: sources[0], requested: "" };
  }
  const fallback = sources.find((s) => s.name === "default");
  return fallback === undefined
    ? { basis: "ambiguous", source: undefined, requested: "" }
    : { basis: "default", source: fallback, requested: "" };
}

/** True for a kind this build knows how to talk about (ADR-0014's enum). */
export function isKnownKind(kind: string): kind is ComponentKind {
  return WRITTEN_KINDS.has(kind);
}

/**
 * True for a component whose own screen is the data-services panel: it has no
 * pods, so no logs, and no image, so nothing to roll (#107).
 */
export function isDataComponentKind(kind: string): boolean {
  return DATA_KINDS.has(kind);
}
