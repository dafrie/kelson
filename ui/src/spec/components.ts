import { sequenceItems } from "../dataservices/miniyaml";

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
    });
  }
  return out;
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
