import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { SetAtLevel, SettingGroup } from "../gen/kelson/v1alpha1/effectiveconfig_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { EffectiveConfigTable } from "./EffectiveConfigTable";

/**
 * The effective-config table (#260).
 *
 * The fixture is the answer `GetEffectiveConfig` sends for a component whose
 * LOG_LEVEL is written in three places and whose image is pinned for one
 * environment: every row here is a value the server merged, with the block it
 * came from beside it.
 */

const ROUTE = "/projects/:project/:env/components/:component";
const PATH = "/projects/checkout/production/components/web";

function setAt(level: SetAtLevel, extra: Record<string, string> = {}) {
  return { level, document: "", environment: "", component: "", field: "", ...extra };
}

const CONFIG = {
  project: "checkout",
  environment: "production",
  settings: [
    {
      name: "secrets.backend",
      group: SettingGroup.ENVIRONMENT,
      value: { value: { case: "literal" as const, value: "cluster" } },
      setAt: setAt(SetAtLevel.BUILT_IN),
    },
  ],
  components: [
    {
      name: "web",
      kind: "service",
      settings: [
        {
          name: "LOG_LEVEL",
          group: SettingGroup.ENV,
          value: { value: { case: "literal" as const, value: "warn" } },
          setAt: setAt(SetAtLevel.ENVIRONMENT_COMPONENT, {
            document: "Environment",
            environment: "production",
            component: "web",
            field: "$.spec.components[0].env.LOG_LEVEL",
          }),
          // Written in three places: the project, the component, and the
          // override that won.
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
                field: "$.spec.components[0].env.LOG_LEVEL",
              }),
            },
          ],
        },
        {
          name: "REGION",
          group: SettingGroup.ENV,
          value: { value: { case: "literal" as const, value: "eu-central" } },
          setAt: setAt(SetAtLevel.PROJECT, {
            document: "Project",
            field: "$.spec.env.REGION",
          }),
        },
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
            field: "$.spec.components[0].env.STRIPE_KEY",
          }),
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
          name: "FEATURE_X",
          group: SettingGroup.ENV,
          value: { value: { case: "literal" as const, value: "" } },
          setAt: setAt(SetAtLevel.ENVIRONMENT_COMPONENT, {
            document: "Environment",
            environment: "production",
            component: "web",
            field: "$.spec.components[0].env.FEATURE_X",
          }),
          // The unset case: the winner is an empty value, so the shadow is the
          // only thing on the row that can say what was unset.
          shadowed: [
            {
              value: { value: { case: "literal" as const, value: "enabled" } },
              setAt: setAt(SetAtLevel.PROJECT, {
                document: "Project",
                field: "$.spec.env.FEATURE_X",
              }),
            },
          ],
        },
        {
          name: "image",
          group: SettingGroup.WORKLOAD,
          value: {
            value: { case: "literal" as const, value: "ghcr.io/acme/checkout:1.4.3" },
          },
          setAt: setAt(SetAtLevel.ENVIRONMENT_COMPONENT, {
            document: "Environment",
            environment: "production",
            component: "web",
            field: "$.spec.components[0].image",
          }),
        },
        {
          name: "replicas",
          group: SettingGroup.WORKLOAD,
          value: { value: { case: "literal" as const, value: "1" } },
          setAt: setAt(SetAtLevel.BUILT_IN),
        },
      ],
    },
  ],
};

function serverWith(answer: () => unknown) {
  return createRouterTransport((router) => {
    router.service(SpecService, {
      getEffectiveConfig: () => answer() as never,
    });
  });
}

function open(transport = serverWith(() => ({ config: CONFIG })), component = "web") {
  return renderAt(
    transport,
    PATH,
    ROUTE,
    <EffectiveConfigTable
      project="checkout"
      environment="production"
      component={component}
    />,
  );
}

describe("EffectiveConfigTable", () => {
  it("states each effective value and the block that set it", async () => {
    open();

    expect(await screen.findByText("LOG_LEVEL")).toBeTruthy();
    expect(screen.getByText("warn")).toBeTruthy();
    // The three-level merge, said in one sentence per row.
    expect(
      screen.getAllByText("set on production, for this component", {
        selector: ".k-config__setat",
      }).length,
    ).toBe(3); // LOG_LEVEL, FEATURE_X and the image pin
    expect(screen.getByText("eu-central")).toBeTruthy();
    expect(
      screen.getByText("set on the project", { selector: ".k-config__setat" }),
    ).toBeTruthy();
    expect(
      screen.getByText("set on the component", { selector: ".k-config__setat" }),
    ).toBeTruthy();

    // The groups a reader scans by.
    expect(screen.getByText("Environment variables")).toBeTruthy();
    expect(screen.getByText("Runtime")).toBeTruthy();
    expect(screen.getByText("This environment")).toBeTruthy();
  });

  it("renders a secret as the reference it is, and never as a value", async () => {
    open();

    expect(await screen.findByText("STRIPE_KEY")).toBeTruthy();
    // The one spelling of a reference this UI writes, unchanged: a name and a
    // key. There is no value on this wire and none on the screen.
    expect(
      screen.getByText("{ secret: checkout-stripe, key: secretKey }"),
    ).toBeTruthy();
  });

  it("names an empty value rather than leaving the cell blank", async () => {
    open();

    expect(await screen.findByText("FEATURE_X")).toBeTruthy();
    // Overriding a variable to "" is how a project-level one is unset for an
    // environment, so it is a value somebody set and it says so.
    expect(screen.getByText("empty", { selector: ".k-config__empty" })).toBeTruthy();
  });

  it("dims what nobody wrote and keeps the document path off the table", async () => {
    const { container } = open();

    await screen.findByText("replicas");
    const defaults = container.querySelectorAll(".k-config__row--default");
    expect(defaults.length).toBe(2); // replicas and the secret backend
    expect(
      screen.getAllByText("kelson's default", { selector: ".k-config__setat" })
        .length,
    ).toBe(2);

    // The JSONPath is a fact for the reader who wants the exact line, and it is
    // on the row rather than in a column of its own.
    const authored = screen.getAllByText("set on production, for this component", {
      selector: ".k-config__setat",
    })[0];
    expect(authored?.getAttribute("title")).toBe(
      "$.spec.components[0].env.LOG_LEVEL",
    );
    const builtIn = screen.getAllByText("kelson's default", {
      selector: ".k-config__setat",
    })[0];
    expect(builtIn?.getAttribute("title")).toBeNull();
  });

  it("draws what a value replaced under it, struck through and quiet", async () => {
    const { container } = open();

    await screen.findByText("LOG_LEVEL");
    // The chain, in the wire's order, under the one value that runs.
    expect(screen.getByText("info")).toBeTruthy();
    expect(screen.getByText("debug")).toBeTruthy();
    // The sentence names both blocks, so which one took the value away is part
    // of it: LOG_LEVEL and FEATURE_X lost theirs to the environment's override
    // of this component, STRIPE_KEY's to the component itself.
    expect(
      screen.getAllByText("set on the project, overridden for production", {
        selector: ".k-config__setat",
      }).length,
    ).toBe(2);
    expect(
      screen.getByText("set on the project, overridden on the component", {
        selector: ".k-config__setat",
      }),
    ).toBeTruthy();
    expect(
      screen.getByText("set on the component, overridden for production", {
        selector: ".k-config__setat",
      }),
    ).toBeTruthy();

    // Struck through, named as past, and carrying no name of its own — it is
    // the same setting as the row above it.
    const struck = container.querySelectorAll("s.k-config__shadow-value");
    expect(struck.length).toBe(4); // LOG_LEVEL twice, STRIPE_KEY, FEATURE_X
    expect(struck[0]?.textContent).toBe("info");
    expect(container.querySelectorAll(".k-config__was").length).toBe(4);
    const shadowRows = container.querySelectorAll("tr.k-config__shadow");
    expect(shadowRows[0]?.querySelector(".k-config__name")).toBeNull();

    // The document path rides on the shadow's own sentence, the way it does on
    // the winner's.
    expect(
      screen
        .getAllByText("set on the project, overridden for production", {
          selector: ".k-config__setat",
        })[0]
        ?.getAttribute("title"),
    ).toBe("$.spec.env.LOG_LEVEL");
  });

  it("leaves a row nothing overrode exactly as it was", async () => {
    const { container } = open();

    await screen.findByText("REGION");
    // REGION, image and replicas are unshadowed, so the table gains no line for
    // them: only the four rows with a chain behind them do.
    const rows = container.querySelectorAll("tbody tr");
    const shadows = container.querySelectorAll("tr.k-config__shadow");
    expect(shadows.length).toBe(4);
    // Three group headings, six settings on the component, one on the
    // environment, and four shadow lines.
    expect(rows.length).toBe(3 + 6 + 1 + 4);
    const region = screen.getByText("REGION").closest("tr");
    expect(region?.classList.contains("k-config__row--shadowed")).toBe(false);
    expect(region?.nextElementSibling?.classList.contains("k-config__shadow")).toBe(
      false,
    );
  });

  it("says what an empty override unset, which the winner cannot", async () => {
    open();

    await screen.findByText("FEATURE_X");
    // The winner is an empty value and reads as one; the shadow is what says
    // the project had set something here.
    expect(screen.getByText("empty", { selector: ".k-config__empty" })).toBeTruthy();
    expect(screen.getByText("enabled")).toBeTruthy();
  });

  it("keeps a shadowed secret a reference", async () => {
    open();

    await screen.findByText("STRIPE_KEY");
    // Both halves of the chain print as references: kelson reads neither
    // Secret, so there is no value on either line.
    expect(
      screen.getByText("{ secret: checkout-stripe-test, key: secretKey }"),
    ).toBeTruthy();
  });

  it("omits a setting the answer did not mention", async () => {
    // No `command` and no `domains` in the fixture: nothing in either document
    // names one, so there is no row rather than a blank one.
    open();

    await screen.findByText("image");
    expect(screen.queryByText("command")).toBeNull();
    expect(screen.queryByText("domains")).toBeNull();
  });

  it("says so when the answer covers no settings for this component", async () => {
    open(
      serverWith(() => ({
        config: { ...CONFIG, settings: [], components: [] },
      })),
      "ingress",
    );

    expect(
      await screen.findByText(
        "Nothing is configured for ingress in production.",
      ),
    ).toBeTruthy();
  });

  it("goes loud only when the call fails", async () => {
    open(
      serverWith(() => {
        throw new ConnectError("the spec store is unreachable", Code.Unavailable, undefined, [
          {
            desc: ErrorSchema,
            value: {
              code: "store/unavailable",
              message: "the spec store did not answer",
              remediation: "check that kelson-server can reach the cluster",
            },
          },
        ]);
      }),
    );

    await waitFor(() => {
      expect(
        screen.getByText("Could not read the effective configuration"),
      ).toBeTruthy();
    });
    expect(screen.getByText("store/unavailable")).toBeTruthy();
    expect(screen.getByText("the spec store did not answer")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("reports a stored spec that no longer resolves as an answer, not a crash", async () => {
    open(
      serverWith(() => ({
        errors: [
          {
            code: "ref/unknown-component",
            field: "$.spec.components[0].name",
            message: 'environment production overrides component "ghost", which the project does not declare',
            remediation: "remove the override, or add the component to the project",
          },
        ],
      })),
    );

    await waitFor(() => {
      expect(screen.getByText("These documents no longer resolve")).toBeTruthy();
    });
    expect(screen.getByText("ref/unknown-component")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });
});
