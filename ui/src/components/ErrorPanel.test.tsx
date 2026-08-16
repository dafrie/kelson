import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { createClients } from "../api/clients";
import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { ErrorPanel } from "./ErrorPanel";

/**
 * The structured error is the contract, so this test takes the long way round:
 * a stub server raises a ConnectError carrying a kelson.v1alpha1.Error detail,
 * a real client receives it over the real serialisation, and the panel renders
 * what came back. A hand-built ConnectError would prove only that the component
 * reads its props.
 */
async function catchFromServer(detail: {
  code: string;
  message: string;
  remediation?: string;
  docsUrl?: string;
  field?: string;
}): Promise<unknown> {
  const transport = createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => {
        throw new ConnectError(
          "spec rejected",
          Code.InvalidArgument,
          undefined,
          [{ desc: ErrorSchema, value: detail }],
        );
      },
    });
  });
  try {
    await createClients(transport).spec.getSpec({ project: "checkout" });
  } catch (err) {
    return err;
  }
  throw new Error("the stub was supposed to fail");
}

describe("ErrorPanel", () => {
  it("renders the code, message and remediation from wire error details", async () => {
    // internal/model's retired-field answer, which is what a stored spec still
    // carrying `delivery:` now gets (ADR-0028, #234) — a real remediation
    // rather than an invented one, kept whole by the panel.
    const err = await catchFromServer({
      code: "schema/unknown-field",
      message: 'unknown field "delivery" on EnvironmentSpec',
      remediation:
        "delete the whole `delivery:` block — kelson renders, publishes an OCI artifact and lets Flux reconcile it, so there is no mode to select and no git target to name",
      docsUrl: "https://example.invalid/unknown-field",
      field: "$.spec.delivery",
    });

    render(<ErrorPanel title="Save refused" error={err} />);

    expect(screen.getByText("schema/unknown-field")).toBeTruthy();
    expect(
      screen.getByText('unknown field "delivery" on EnvironmentSpec'),
    ).toBeTruthy();
    // The remediation is labelled "fix:", mirroring the CLI.
    expect(screen.getByText("fix:")).toBeTruthy();
    expect(
      screen.getByText(/delete the whole `delivery:` block/),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "https://example.invalid/unknown-field" }),
    ).toBeTruthy();
    expect(screen.getByText("$.spec.delivery")).toBeTruthy();
    // The RPC code is shown too — it says which layer answered.
    expect(screen.getByText("rpc invalid_argument")).toBeTruthy();
  });

  it("renders inline structured errors the same way as thrown ones", () => {
    render(
      <ErrorPanel
        title="The spec was rejected"
        errors={[
          {
            $typeName: "kelson.v1alpha1.Error",
            code: "schema/unknown-field",
            resource: "Project/checkout",
            field: "$.spec.port",
            application: "",
            overlay: "",
            target: "",
            message: "unknown field port",
            remediation: "move it under a component",
            docsUrl: "",
            line: 12,
            column: 3,
            cause: "",
          },
        ]}
      />,
    );

    expect(screen.getByText("schema/unknown-field")).toBeTruthy();
    expect(screen.getByText("unknown field port")).toBeTruthy();
    expect(screen.getByText("line 12:3")).toBeTruthy();
  });

  it("falls back to the message, and names kelson-server when nothing answered", () => {
    render(
      <ErrorPanel
        title="Cannot reach the server"
        error={new Error("Failed to fetch")}
      />,
    );

    expect(screen.getByText("Failed to fetch")).toBeTruthy();
    expect(screen.getByText(/kelson-server --listen 127.0.0.1:8420/)).toBeTruthy();
  });
});
