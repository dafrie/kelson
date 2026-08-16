import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import {
  EffectiveConfigSchema,
  SetAtLevel,
  SetAtSchema,
  SettingGroup,
} from "../gen/kelson/v1alpha1/effectiveconfig_pb";
import {
  configGroups,
  answersFor,
  setAtSentence,
  shadowSentence,
  valueOf,
} from "./effective";

/**
 * The table, as logic. Every fixture here is the shape
 * `SpecService.GetEffectiveConfig` sends — the value and the block that set it,
 * never the documents behind them — because the one thing this module must not
 * grow is an opinion about precedence.
 */

function setAt(level: SetAtLevel, extra: Record<string, string> = {}) {
  return create(SetAtSchema, { level, ...extra });
}

function literal(
  name: string,
  group: SettingGroup,
  text: string,
  at: ReturnType<typeof setAt>,
) {
  return {
    name,
    group,
    value: { value: { case: "literal" as const, value: text } },
    setAt: at,
  };
}

const CONFIG = create(EffectiveConfigSchema, {
  project: "checkout",
  environment: "production",
  settings: [
    literal(
      "secrets.backend",
      SettingGroup.ENVIRONMENT,
      "sops",
      setAt(SetAtLevel.ENVIRONMENT, {
        document: "Environment",
        environment: "production",
        field: "$.spec.secrets.backend",
      }),
    ),
  ],
  components: [
    {
      name: "web",
      kind: "service",
      settings: [
        {
          ...literal(
            "LOG_LEVEL",
            SettingGroup.ENV,
            "warn",
            setAt(SetAtLevel.ENVIRONMENT_COMPONENT, {
              document: "Environment",
              environment: "production",
              component: "web",
              field: "$.spec.components[0].env.LOG_LEVEL",
            }),
          ),
          // The chain the winner replaced, in the wire's own order: the
          // project's value, then the component's.
          shadowed: [
            {
              value: { value: { case: "literal" as const, value: "info" } },
              setAt: setAt(SetAtLevel.PROJECT, {
                document: "Project",
                field: "$.spec.env.LOG_LEVEL",
              }),
            },
            {
              value: { value: { case: "literal" as const, value: "debug" } },
              setAt: setAt(SetAtLevel.COMPONENT, {
                document: "Project",
                component: "web",
                field: "$.spec.components[1].env.LOG_LEVEL",
              }),
            },
          ],
        },
        literal(
          "REGION",
          SettingGroup.ENV,
          "eu-central",
          setAt(SetAtLevel.PROJECT, {
            document: "Project",
            field: "$.spec.env.REGION",
          }),
        ),
        {
          name: "STRIPE_KEY",
          group: SettingGroup.ENV,
          value: {
            value: {
              case: "secret" as const,
              value: { secret: "checkout-stripe", key: "secretKey" },
            },
          },
          setAt: setAt(SetAtLevel.COMPONENT, {
            document: "Project",
            component: "web",
            field: "$.spec.components[1].env.STRIPE_KEY",
          }),
          // A shadowed reference is still a reference: there is no value behind
          // either arm of the chain.
          shadowed: [
            {
              value: {
                value: {
                  case: "secret" as const,
                  value: { secret: "checkout-stripe-test", key: "secretKey" },
                },
              },
              setAt: setAt(SetAtLevel.PROJECT, {
                document: "Project",
                field: "$.spec.env.STRIPE_KEY",
              }),
            },
          ],
        },
        {
          name: "DATABASE_URL",
          group: SettingGroup.ENV,
          value: {
            value: {
              case: "binding" as const,
              value: { service: "db", key: "uri" },
            },
          },
          setAt: setAt(SetAtLevel.PROJECT, {
            document: "Project",
            field: "$.spec.env.DATABASE_URL",
          }),
        },
        {
          // The unset case: production overrides a project variable to "", so
          // the winner says nothing about itself and the shadow is the only
          // thing that can say what was unset.
          ...literal("EMPTY", SettingGroup.ENV, "", setAt(SetAtLevel.ENVIRONMENT_COMPONENT, {
            document: "Environment",
            environment: "production",
            component: "web",
            field: "$.spec.components[0].env.EMPTY",
          })),
          shadowed: [
            {
              value: { value: { case: "literal" as const, value: "on" } },
              setAt: setAt(SetAtLevel.PROJECT, {
                document: "Project",
                field: "$.spec.env.EMPTY",
              }),
            },
          ],
        },
        literal(
          "image",
          SettingGroup.WORKLOAD,
          "ghcr.io/acme/checkout:1.4.3",
          setAt(SetAtLevel.ENVIRONMENT_COMPONENT, {
            document: "Environment",
            environment: "production",
            component: "web",
            field: "$.spec.components[0].image",
          }),
        ),
        literal("replicas", SettingGroup.WORKLOAD, "1", setAt(SetAtLevel.BUILT_IN)),
      ],
    },
    { name: "ingress", kind: "helm", settings: [] },
    {
      name: "db",
      kind: "postgres",
      settings: [
        literal(
          "preset",
          SettingGroup.DATA,
          "ha-small",
          setAt(SetAtLevel.ENVIRONMENT_COMPONENT, {
            document: "Environment",
            environment: "production",
            component: "db",
            field: "$.spec.components[1].preset",
          }),
        ),
      ],
    },
  ],
});

describe("setAtSentence", () => {
  it("names each block as a fact about a file, not as a rule number", () => {
    expect(setAtSentence(setAt(SetAtLevel.BUILT_IN))).toBe("kelson's default");
    expect(setAtSentence(setAt(SetAtLevel.PROJECT))).toBe("set on the project");
    expect(setAtSentence(setAt(SetAtLevel.COMPONENT))).toBe(
      "set on the component",
    );
    expect(
      setAtSentence(setAt(SetAtLevel.ENVIRONMENT, { environment: "production" })),
    ).toBe("set on production");
    expect(
      setAtSentence(
        setAt(SetAtLevel.ENVIRONMENT_COMPONENT, { environment: "production" }),
      ),
    ).toBe("set on production, for this component");
  });

  it("says nothing rather than guessing when the level is one it cannot read", () => {
    expect(setAtSentence(setAt(SetAtLevel.UNSPECIFIED))).toBe("not stated");
    expect(setAtSentence(undefined)).toBe("not stated");
  });

  it("falls back to a nameless environment rather than printing an empty name", () => {
    expect(setAtSentence(setAt(SetAtLevel.ENVIRONMENT))).toBe(
      "set on this environment",
    );
  });
});

describe("shadowSentence", () => {
  it("names where a replaced value was written and where it was taken away", () => {
    expect(
      shadowSentence(
        setAt(SetAtLevel.COMPONENT),
        setAt(SetAtLevel.ENVIRONMENT_COMPONENT, { environment: "production" }),
      ),
    ).toBe("set on the component, overridden for production");
    expect(
      shadowSentence(setAt(SetAtLevel.PROJECT), setAt(SetAtLevel.COMPONENT)),
    ).toBe("set on the project, overridden on the component");
    expect(
      shadowSentence(
        setAt(SetAtLevel.PROJECT),
        setAt(SetAtLevel.ENVIRONMENT, { environment: "production" }),
      ),
    ).toBe("set on the project, overridden for production");
  });

  it("says only that it was overridden when the winner names no block", () => {
    // A block chain taken whole can leave kelson's own default in force, and a
    // default has no block to name — the row above already says whose it is.
    expect(
      shadowSentence(setAt(SetAtLevel.PROJECT), setAt(SetAtLevel.BUILT_IN)),
    ).toBe("set on the project, overridden");
    expect(shadowSentence(setAt(SetAtLevel.PROJECT), undefined)).toBe(
      "set on the project, overridden",
    );
  });
});

describe("valueOf", () => {
  it("keeps a secret reference a reference", () => {
    expect(
      valueOf({
        $typeName: "kelson.v1alpha1.EffectiveValue",
        value: {
          case: "secret",
          value: {
            $typeName: "kelson.v1alpha1.SecretKeyReference",
            secret: "checkout-db",
            key: "url",
          },
        },
      }),
    ).toEqual({ kind: "secret", secret: "checkout-db", key: "url" });
  });

  it("keeps a data-service binding a binding", () => {
    expect(
      valueOf({
        $typeName: "kelson.v1alpha1.EffectiveValue",
        value: {
          case: "binding",
          value: {
            $typeName: "kelson.v1alpha1.ServiceBindingReference",
            service: "db",
            key: "uri",
          },
        },
      }),
    ).toEqual({ kind: "binding", service: "db", key: "uri" });
  });

  it("carries an empty literal through as one: it is a value somebody set", () => {
    expect(
      valueOf({
        $typeName: "kelson.v1alpha1.EffectiveValue",
        value: { case: "literal", value: "" },
      }),
    ).toEqual({ kind: "plain", value: "" });
  });
});

describe("configGroups", () => {
  it("groups the rows in reading order and keeps the wire's provenance", () => {
    const groups = configGroups(CONFIG, "web");
    expect(groups.map((g) => g.title)).toEqual([
      "Environment variables",
      "Runtime",
      "This environment",
    ]);

    const env = groups[0]?.rows ?? [];
    expect(env.map((r) => r.name)).toEqual([
      "LOG_LEVEL",
      "REGION",
      "STRIPE_KEY",
      "DATABASE_URL",
      "EMPTY",
    ]);
    expect(env[0]?.setAt).toBe("set on production, for this component");
    expect(env[0]?.where).toBe("$.spec.components[0].env.LOG_LEVEL");
    expect(env[1]?.setAt).toBe("set on the project");
    expect(env[2]?.value).toEqual({
      kind: "secret",
      secret: "checkout-stripe",
      key: "secretKey",
    });
  });

  it("carries the chain each winner replaced, in the wire's order", () => {
    const env = configGroups(CONFIG, "web")[0]?.rows ?? [];
    const logLevel = env.find((r) => r.name === "LOG_LEVEL");
    expect(logLevel?.shadows.map((s) => s.value)).toEqual([
      { kind: "plain", value: "info" },
      { kind: "plain", value: "debug" },
    ]);
    expect(logLevel?.shadows.map((s) => s.setAt)).toEqual([
      "set on the project, overridden for production",
      "set on the component, overridden for production",
    ]);
    expect(logLevel?.shadows[0]?.where).toBe("$.spec.env.LOG_LEVEL");

    // A row nothing overrode carries no chain at all, which is what keeps the
    // table the height it was.
    expect(env.find((r) => r.name === "REGION")?.shadows).toEqual([]);
  });

  it("keeps a shadowed reference a reference", () => {
    const env = configGroups(CONFIG, "web")[0]?.rows ?? [];
    expect(env.find((r) => r.name === "STRIPE_KEY")?.shadows[0]?.value).toEqual({
      kind: "secret",
      secret: "checkout-stripe-test",
      key: "secretKey",
    });
  });

  it("says what an empty override unset", () => {
    const env = configGroups(CONFIG, "web")[0]?.rows ?? [];
    const empty = env.find((r) => r.name === "EMPTY");
    expect(empty?.value).toEqual({ kind: "plain", value: "" });
    expect(empty?.shadows).toEqual([
      {
        value: { kind: "plain", value: "on" },
        setAt: "set on the project, overridden for production",
        where: "$.spec.env.EMPTY",
      },
    ]);
  });

  it("marks the rows nobody wrote, so the authored ones are findable", () => {
    const runtime = configGroups(CONFIG, "web")[1]?.rows ?? [];
    const replicas = runtime.find((r) => r.name === "replicas");
    expect(replicas?.builtIn).toBe(true);
    expect(replicas?.setAt).toBe("kelson's default");
    expect(replicas?.where).toBe("");
    expect(runtime.find((r) => r.name === "image")?.builtIn).toBe(false);
  });

  it("separates an env variable from a workload setting of the same name", () => {
    const collision = create(EffectiveConfigSchema, {
      project: "checkout",
      environment: "production",
      components: [
        {
          name: "web",
          kind: "service",
          settings: [
            literal("image", SettingGroup.ENV, "not-a-reference", setAt(SetAtLevel.COMPONENT)),
            literal("image", SettingGroup.WORKLOAD, "ghcr.io/acme/x:1", setAt(SetAtLevel.PROJECT)),
          ],
        },
      ],
    });
    const groups = configGroups(collision, "web");
    expect(groups[0]?.rows[0]?.value).toEqual({
      kind: "plain",
      value: "not-a-reference",
    });
    expect(groups[1]?.rows[0]?.value).toEqual({
      kind: "plain",
      value: "ghcr.io/acme/x:1",
    });
  });

  it("puts a data component's preset in the runtime group", () => {
    const groups = configGroups(CONFIG, "db");
    expect(groups.map((g) => g.title)).toEqual(["Runtime", "This environment"]);
    expect(groups[0]?.rows[0]?.name).toBe("preset");
    expect(groups[0]?.rows[0]?.value).toEqual({ kind: "plain", value: "ha-small" });
  });

  it("gives a chart the environment's rows and nothing of its own", () => {
    // No precedence rule reaches a chart, so its own table is empty and the
    // project's env must not leak into it.
    const groups = configGroups(CONFIG, "ingress");
    expect(groups.map((g) => g.title)).toEqual(["This environment"]);
  });

  it("answers nothing for a component the config does not mention", () => {
    expect(configGroups(CONFIG, "ghost")).toEqual([]);
    expect(answersFor(CONFIG, "ghost")).toBe(false);
    expect(answersFor(CONFIG, "web")).toBe(true);
    expect(configGroups(undefined, "web")).toEqual([]);
  });
});
