import { sequenceItems } from "./miniyaml";
import { DEFAULT_PRESET, type DataKind, type Preset } from "./presets";

/**
 * Reading a spec's data components out of the stored documents.
 *
 * The store is byte-faithful (ADR-0013 §1) and the API hands the UI the
 * authored bytes, so anything a screen wants to say about a component is
 * something it has to read out of YAML. This module reads exactly three fields
 * of exactly one shape — `spec.components[]` with a `kind:` — plus rule P5's
 * per-environment `preset:` override, and gives up on everything else.
 *
 * WHY THIS IS NOT src/spec/edit.ts's PARSER. That parser feeds a form whose
 * honesty rests on a byte-identical rebuild: a key it cannot express must be a
 * key it does not read, or the round-trip guard stops proving anything (see the
 * module comment there, which says so about data components by name). This one
 * never writes anything back. It reads a document to *describe* it, so it is a
 * separate module with a separate contract, and neither can weaken the other.
 *
 * A document the reader cannot follow yields no data components, and the
 * section then says nothing rather than something wrong.
 */

/** One data component, after rule P5's environment override. */
export interface DataService {
  name: string;
  kind: DataKind;
  /**
   * The resolved preset. Not narrowed to [Preset]: a stored document may name
   * one this build does not know, and reporting that is better than pretending
   * it is a preset we can size.
   */
  preset: string;
  /** Where the preset came from, which is what makes an override visible. */
  presetSource: "environment" | "component" | "default";
}

const DATA_KINDS = new Set<string>(["postgres", "valkey"]);

/**
 * The data components of a project document, with the environment document's
 * P5 overrides applied (internal/model/resolve.go, resolveDataService).
 *
 * `environmentDoc` may be empty: an environment that overrides nothing is the
 * common case, and the project's own presets stand.
 */
export function parseDataServices(
  projectDoc: string,
  environmentDoc = "",
): DataService[] {
  const overrides = new Map<string, string>();
  for (const item of sequenceItems(environmentDoc, "components")) {
    const name = item.get("name");
    const preset = item.get("preset");
    if (name !== undefined && preset !== undefined && preset !== "") {
      overrides.set(name, preset);
    }
  }

  const out: DataService[] = [];
  for (const item of sequenceItems(projectDoc, "components")) {
    const name = item.get("name");
    const kind = item.get("kind");
    if (name === undefined || kind === undefined || !DATA_KINDS.has(kind)) {
      continue;
    }
    const own = item.get("preset") ?? "";
    const override = overrides.get(name);
    const preset = override ?? (own !== "" ? own : DEFAULT_PRESET);
    out.push({
      name,
      kind: kind as DataKind,
      preset,
      presetSource:
        override !== undefined
          ? "environment"
          : own !== ""
            ? "component"
            : "default",
    });
  }
  return out;
}

/** True for a preset string this build knows how to talk about. */
export function isKnownPreset(preset: string): preset is Preset {
  return (
    preset === "shared" ||
    preset === "small" ||
    preset === "ha-small" ||
    preset === "ha-medium" ||
    preset === "branch"
  );
}

/**
 * The name every resource kelson renders for a data component carries:
 * `<project>-<environment>-<component>`, mirroring `serviceResourceName` in
 * internal/renderer/dataservice.go. It is what the CloudNativePG Cluster is
 * called, which is how a status verdict about that Cluster is recognised.
 */
export function clusterResourceName(
  project: string,
  environment: string,
  component: string,
): string {
  return `${project}-${environment}-${component}`;
}

/**
 * The verdict about this component's CloudNativePG Cluster, if the status data
 * carries one.
 *
 * Health is not invented here: `resource` is observation's own
 * `Kind/namespace/name` string (internal/observation/health.go), and this only
 * asks whether one of them is this component's Cluster. Today the probe watches
 * Deployments only, so the answer is usually "no verdict" — which the panel
 * says, rather than showing a green pill nobody earned.
 */
export function clusterVerdictFor<T extends { resource: string }>(
  verdicts: readonly T[],
  clusterName: string,
): T | undefined {
  return verdicts.find((v) => {
    const parts = v.resource.split("/");
    return (
      parts.length >= 2 &&
      parts[0] === "Cluster" &&
      parts[parts.length - 1] === clusterName
    );
  });
}

/** True for a verdict about a data service's own resource rather than a workload. */
export function isDataServiceVerdict(resource: string): boolean {
  return resource.split("/")[0] === "Cluster";
}
