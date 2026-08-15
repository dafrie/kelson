import type { GitOpsOwnership } from "../gen/kelson/v1alpha1/spec_pb";

import type { SpecTextSet } from "./edit";

/**
 * Reading `Spec.gitops`, and everything the edit screen decides from it (#248).
 *
 * The server answers which stored documents somebody else's Flux is
 * reconciling, per document, by reading the labels kustomize-controller stamps
 * on what it applies. This module is the client half: which documents are
 * affected, how to name the thing that owns them, and the two questions the
 * screen has to answer that the server cannot.
 *
 * # What the server does not know, and this module does not pretend to
 *
 * Where in the repository a document lives. Identifying the Kustomization needs
 * a label; resolving its source and path needs RBAC on the Flux kinds that
 * kelson-server deliberately does not hold (ADR-0013 §3). So a path is asked of
 * the person proposing the change, `suggestedPath` is a *starting point* rather
 * than an answer, and `rememberedPaths` keeps what they typed in this browser
 * so they only type it once.
 *
 * Remembering it in localStorage rather than on the document is deliberate. The
 * only durable place kelson could write it is the Project or Environment CR —
 * which is the document the banner has just told this reader git owns, and
 * writing to it to record how to avoid writing to it would be the same mistake
 * with an extra step. The consequence is stated rather than hidden: a colleague
 * proposing from another browser types the path again.
 */

/** The document key for a project document, matching controlstore.DocumentProject. */
export const PROJECT_DOCUMENT = "project";

/** Ownership by document name, for the per-document questions the screen asks. */
export function ownersByDocument(
  gitops: readonly GitOpsOwnership[] | undefined,
): Map<string, GitOpsOwnership> {
  const out = new Map<string, GitOpsOwnership>();
  for (const owner of gitops ?? []) {
    if (owner.document !== "") out.set(owner.document, owner);
  }
  return out;
}

/**
 * Whether *any* document of this spec is reconciled from a repository.
 *
 * Any, not all, because Save writes the whole document set: one managed
 * document is enough for the write to be partly undone, and a screen that only
 * warned when everything was managed would be silent in the mixed case that is
 * the common one.
 */
export function isGitOpsManaged(
  gitops: readonly GitOpsOwnership[] | undefined,
): boolean {
  return ownersByDocument(gitops).size > 0;
}

/** `flux-system/apps`, or `apps` when only the name label is present. */
export function kustomizationName(owner: GitOpsOwnership): string {
  return owner.namespace === ""
    ? owner.kustomization
    : `${owner.namespace}/${owner.kustomization}`;
}

/**
 * The distinct Kustomizations owning any of these documents, in the order the
 * server reported them.
 *
 * Distinct, because the ordinary case is one Kustomization applying every
 * document of a project and a banner naming it three times reads like three
 * problems.
 */
export function owningKustomizations(
  gitops: readonly GitOpsOwnership[] | undefined,
): string[] {
  const seen = new Set<string>();
  for (const owner of gitops ?? []) {
    if (owner.kustomization !== "") seen.add(kustomizationName(owner));
  }
  return [...seen];
}

/** A human-readable list: "a", "a and b", "a, b and c". */
export function joinNames(names: readonly string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

/**
 * The documents declaring `autoDeploy`, which is ADR-0036 decision 5's warning.
 *
 * A trigger's whole effect is a *spec write* — it splices a pinned image into
 * `Environment.spec.components[].image` — so in an install where the
 * Environment is reconciled from a repository, the trigger and the git
 * reconciler overwrite each other on a loop. The ADR says so and names this
 * screen as where it should be said.
 *
 * The detection is a line match rather than a parse, and the boundary is worth
 * being explicit about: the UI carries no YAML parser (src/spec/edit.ts edits
 * documents as text on purpose, to keep comments and key order), so this looks
 * for a `autoDeploy:` key at the start of a line. It over-reports a document
 * that mentions the word inside a block scalar and under-reports one written in
 * flow style on a single line. Both directions are acceptable for a warning
 * that names a real incompatibility and tells the reader to look — neither is
 * acceptable for anything that refuses.
 */
export function autoDeployDocuments(text: SpecTextSet): string[] {
  const out: string[] = [];
  for (const name of Object.keys(text.environments).sort()) {
    if (declaresAutoDeploy(text.environments[name] ?? "")) out.push(name);
  }
  return out;
}

const AUTO_DEPLOY = /^\s*autoDeploy\s*:/m;

export function declaresAutoDeploy(document: string): boolean {
  return AUTO_DEPLOY.test(document);
}

/**
 * A starting point for the repository path of one document.
 *
 * It is what a repository laid out the way this project's own docs suggest
 * would call the file, and it is offered as a prefill in an editable field —
 * never sent unless the person leaves it alone, which is them saying it is
 * right.
 */
export function suggestedPath(project: string, document: string): string {
  return document === PROJECT_DOCUMENT
    ? `${project}.yaml`
    : `${project}-${document}.yaml`;
}

/** The localStorage key one project's remembered paths live under. */
function pathsKey(project: string): string {
  return `kelson.gitops.paths.${project}`;
}

/** The localStorage key one project's remembered repository/branch lives under. */
function targetKey(project: string): string {
  return `kelson.gitops.target.${project}`;
}

/** Where a project's documents were last proposed to. */
export interface ProposalTarget {
  connection: string;
  repository: string;
  baseBranch: string;
}

export const EMPTY_TARGET: ProposalTarget = {
  connection: "",
  repository: "",
  baseBranch: "",
};

/**
 * Read remembered values, tolerating every way storage can be unavailable.
 *
 * Private browsing throws on access, a quota-full origin throws on write, and a
 * value written by an older build may not parse. None of those is worth an
 * error on a screen whose actual job is elsewhere: the fallback is an empty
 * field, which is where this feature started.
 */
function read<T>(key: string, fallback: T): T {
  try {
    const raw = window.localStorage.getItem(key);
    if (raw === null) return fallback;
    const parsed: unknown = JSON.parse(raw);
    if (parsed === null || typeof parsed !== "object") return fallback;
    return parsed as T;
  } catch {
    return fallback;
  }
}

function write(key: string, value: unknown): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Nothing to do and nothing worth saying: the person types the path again
    // next time, which is exactly what happens with no memory at all.
  }
}

export function rememberedPaths(project: string): Record<string, string> {
  return read<Record<string, string>>(pathsKey(project), {});
}

export function rememberPaths(
  project: string,
  paths: Record<string, string>,
): void {
  write(pathsKey(project), paths);
}

export function rememberedTarget(project: string): ProposalTarget {
  const stored = read<Partial<ProposalTarget>>(targetKey(project), {});
  return {
    connection: stored.connection ?? "",
    repository: stored.repository ?? "",
    baseBranch: stored.baseBranch ?? "",
  };
}

export function rememberTarget(project: string, target: ProposalTarget): void {
  write(targetKey(project), target);
}

/** One document as the export and the proposal both see it. */
export interface ExportedDocument {
  /** [PROJECT_DOCUMENT] or an environment's name. */
  name: string;
  /** The heading a reader sees: "project" or "environment production". */
  label: string;
  text: string;
  /** The Kustomization reconciling it, when there is one. */
  owner: GitOpsOwnership | undefined;
}

/**
 * Every document of a spec, project first, with its owner attached.
 *
 * This is the one list the export panel and the proposal form both walk, so a
 * document can never appear in one and not the other — which is the failure
 * that would let somebody export three files and propose two.
 */
export function exportedDocuments(
  text: SpecTextSet,
  gitops: readonly GitOpsOwnership[] | undefined,
): ExportedDocument[] {
  const owners = ownersByDocument(gitops);
  const out: ExportedDocument[] = [
    {
      name: PROJECT_DOCUMENT,
      label: "project",
      text: text.project,
      owner: owners.get(PROJECT_DOCUMENT),
    },
  ];
  for (const name of Object.keys(text.environments).sort()) {
    out.push({
      name,
      label: `environment ${name}`,
      text: text.environments[name] ?? "",
      owner: owners.get(name),
    });
  }
  return out;
}

/** Every document as one multi-document YAML file, for "download all". */
export function bundle(documents: readonly ExportedDocument[]): string {
  return documents
    .map((d) => (d.text.startsWith("---") ? d.text : `---\n${d.text}`))
    .map((text) => (text.endsWith("\n") ? text : `${text}\n`))
    .join("");
}
