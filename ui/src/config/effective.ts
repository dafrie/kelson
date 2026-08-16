import type {
  EffectiveConfig,
  EffectiveSetting,
  EffectiveValue,
  SetAt,
} from "../gen/kelson/v1alpha1/effectiveconfig_pb";
import {
  SetAtLevel,
  SettingGroup,
} from "../gen/kelson/v1alpha1/effectiveconfig_pb";
import {
  bindingEnv,
  plainEnv,
  secretEnv,
  type EnvValue,
} from "../spec/documents";

/**
 * The effective-config table, as logic (#260).
 *
 * A component's configuration is the winner of a two- or three-level merge
 * across two documents, and until now the only way to read it was to open both
 * and merge them in your head. `SpecService.GetEffectiveConfig` answers it
 * instead — the value *and* the block that set it — and this module turns that
 * answer into rows and one plain sentence per row.
 *
 * **It computes no precedence.** Every value and every provenance answer comes
 * off the wire exactly as the server sent it. A copy of the merge here would
 * drift from internal/model, silently, in the direction that makes a
 * provenance claim wrong — which is the whole reason the RPC exists.
 */

/** One row: a name, the value it resolves to, and where that was set. */
export interface ConfigRow {
  name: string;
  value: EnvValue;
  /** The plain-language answer to "set at:". */
  setAt: string;
  /** The path into the document, for the reader who wants the exact line. */
  where: string;
  /** True when nothing in either document says it. */
  builtIn: boolean;
}

/** One labelled block of rows. */
export interface ConfigGroup {
  title: string;
  rows: ConfigRow[];
}

/**
 * The value union, carried across without flattening.
 *
 * A `{secret, key}` and a `{from: {service, key}}` are references and there is
 * no value behind either — kelson never reads the Secret, so there is nothing
 * on this wire to render even if a screen wanted to. They arrive as their own
 * arms of a oneof and they stay references here.
 */
export function valueOf(value: EffectiveValue | undefined): EnvValue {
  switch (value?.value.case) {
    case "secret":
      return secretEnv(value.value.value.secret, value.value.value.key);
    case "binding":
      return bindingEnv(value.value.value.service, value.value.value.key);
    case "literal":
      return plainEnv(value.value.value);
    default:
      return plainEnv("");
  }
}

/**
 * The sentence under "set at".
 *
 * Five answers, one per block a value can come from, said as facts about the
 * two files rather than as the rule numbers that govern them. The two
 * environment answers are parallel to the two project ones on purpose: the
 * document is the first half and "for this component" is the second, so a
 * reader learns the shape once.
 *
 * A level this build does not know reads "not stated" rather than falling back
 * to something plausible. A wrong provenance sentence is worse than none.
 */
export function setAtSentence(at: SetAt | undefined): string {
  const named = at?.environment ?? "";
  const environment = named === "" ? "this environment" : named;
  switch (at?.level) {
    case SetAtLevel.BUILT_IN:
      return "kelson's default";
    case SetAtLevel.PROJECT:
      return "set on the project";
    case SetAtLevel.COMPONENT:
      return "set on the component";
    case SetAtLevel.ENVIRONMENT:
      return `set on ${environment}`;
    case SetAtLevel.ENVIRONMENT_COMPONENT:
      return `set on ${environment}, for this component`;
    default:
      return "not stated";
  }
}

function rowOf(setting: EffectiveSetting): ConfigRow {
  return {
    name: setting.name,
    value: valueOf(setting.value),
    setAt: setAtSentence(setting.setAt),
    where: setting.setAt?.field ?? "",
    builtIn: setting.setAt?.level === SetAtLevel.BUILT_IN,
  };
}

function rowsIn(settings: readonly EffectiveSetting[], group: SettingGroup): ConfigRow[] {
  return settings.filter((s) => s.group === group).map(rowOf);
}

/**
 * The groups for one component, in reading order.
 *
 * Rows are separated on the wire's `group` and never on the name, because the
 * two namespaces collide: a project is free to declare an environment variable
 * called `image`, and it is a different row from the workload setting of the
 * same name.
 *
 * An empty group is omitted rather than drawn empty. A component the answer
 * does not mention yields no groups at all, which is the caller's cue to say
 * so — a table of nothing looks like a component with no configuration, and
 * those are different facts.
 */
export function configGroups(
  config: EffectiveConfig | undefined,
  component: string,
): ConfigGroup[] {
  if (config === undefined) return [];
  const found = config.components.find((c) => c.name === component);
  if (found === undefined) return [];

  const groups: ConfigGroup[] = [
    { title: "Environment variables", rows: rowsIn(found.settings, SettingGroup.ENV) },
    {
      title: "Runtime",
      rows: [
        ...rowsIn(found.settings, SettingGroup.WORKLOAD),
        ...rowsIn(found.settings, SettingGroup.DATA),
      ],
    },
    // The environment's own settings ride along because one of them changes
    // what a row above *means*: a `{secret, key}` reference is served by
    // whichever backend this environment names, and a reader looking at the
    // reference is entitled to see which.
    { title: "This environment", rows: rowsIn(config.settings, SettingGroup.ENVIRONMENT) },
  ];
  return groups.filter((g) => g.rows.length > 0);
}

/** Whether the answer mentions this component at all. */
export function answersFor(
  config: EffectiveConfig | undefined,
  component: string,
): boolean {
  return config?.components.some((c) => c.name === component) ?? false;
}
