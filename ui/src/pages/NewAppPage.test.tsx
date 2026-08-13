import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";

import { DryRun, ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import type { PutSpecRequest } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { NewAppPage } from "./NewAppPage";

/**
 * The create flow, against a stubbed SpecService.
 *
 * The two PutSpec calls are the whole screen: the first at dry_run=RENDER,
 * whose structured errors have to land next to the inputs that caused them,
 * and the second with an idempotency key, whose one interesting failure is a
 * name that is already taken.
 */

const decoder = new TextDecoder();

function renderNew(transport: Transport) {
  return renderAt(transport, "/apps/new", "/apps/new", <NewAppPage />);
}

function type(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function openMore() {
  fireEvent.click(screen.getByText("More options"));
}

function submit() {
  fireEvent.click(screen.getByRole("button", { name: "Check and preview" }));
}

/** The text of the field this input belongs to, hints and errors included. */
function field(label: string): string {
  return screen.getByLabelText(label).closest(".k-field")?.textContent ?? "";
}

const MINIMAL_PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  applications:
    - name: web
      port: 8080
`;

const MINIMAL_ENVIRONMENT = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development

spec:
  project: hello
`;

function threeFields() {
  type("Project name", "hello");
  type("Image", "ghcr.io/acme/hello:1.4.2");
  type("Port", "8080");
}

describe("NewAppPage", () => {
  it("refuses a name that cannot be a metadata.name without asking the server", () => {
    let calls = 0;
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        putSpec: () => {
          calls++;
          return {};
        },
      });
    });
    renderNew(transport);

    type("Project name", "My_App");
    expect(field("Project name")).toContain("is not a DNS-1123 label");

    submit();
    expect(calls).toBe(0);
  });

  it("puts each validation error next to the input that caused it", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        putSpec: (req) => {
          requests.push(req);
          return {
            errors: [
              {
                code: "schema/out-of-range",
                resource: "Project/hello",
                field: "$.spec.applications[0].port",
                message: "port must be 1-65535, got 70000",
                remediation: "set a valid TCP port, or omit port for a worker",
                line: 11,
                column: 7,
              },
              {
                code: "schema/invalid-format",
                resource: "Project/hello",
                field: "$.spec.applications[0].health",
                message: 'health path "healthz" must start with /',
                remediation: "use a URL path such as /healthz",
              },
              {
                code: "semantic/no-image-source",
                resource: "Project/hello",
                field: "$.spec.applications[0]",
                message: 'application "web" has no image source',
                remediation: "set image on the application or the Project",
              },
              {
                code: "secret/literal",
                resource: "Project/hello",
                field: "$.spec.env.DB_PASSWORD",
                message: '"DB_PASSWORD" looks like a secret but is a plaintext literal',
                remediation: "the spec carries references, never values",
              },
              // A renderer finding: no field path at all, so it belongs to the
              // panel above the button rather than to any one input.
              {
                code: "render/gateway-api-missing",
                application: "web",
                message: "declares domains but the cluster reports no Gateway API",
                remediation: "install a Gateway API implementation",
              },
            ],
          };
        },
      });
    });
    renderNew(transport);

    type("Project name", "hello");
    type("Port", "70000");
    openMore();
    type("Health path", "healthz");
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    type("Variable 1 name", "DB_PASSWORD");
    type("Variable 1 value", "hunter2");
    submit();

    await waitFor(() => expect(requests).toHaveLength(1));

    expect(field("Port")).toContain("port must be 1-65535, got 70000");
    expect(field("Port")).toContain("schema/out-of-range");
    expect(field("Port")).toContain("set a valid TCP port");
    expect(field("Health path")).toContain('health path "healthz" must start with /');
    // A no-image-source error is about the application, but the only thing the
    // form owns there is the image, so it points at the image.
    expect(field("Image")).toContain("has no image source");
    expect(screen.getByText(/DB_PASSWORD" looks like a secret/)).toBeTruthy();

    // The unmapped renderer finding is the only thing in the general panel:
    // every error whose path names a field this form wrote has been claimed by
    // that field, and this one has no field at all.
    const panel = screen.getByText("The server would not accept this spec").closest(".k-error");
    expect(panel?.textContent).toContain(
      "declares domains but the cluster reports no Gateway API",
    );
    expect(panel?.querySelectorAll(".k-error__item")).toHaveLength(1);

    // Nothing was previewed: the spec was rejected, so there is nothing to
    // create.
    expect(screen.queryByText("What will be stored")).toBeNull();
  });

  it("previews the exact documents, stores them, and offers the deploy", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        putSpec: (req) => {
          requests.push(req);
          return {
            spec: {
              project: "hello",
              version: "1",
              environments: ["development"],
            },
          };
        },
      });
    });
    const { container } = renderNew(transport);

    threeFields();
    submit();

    expect(await screen.findByText("What will be stored")).toBeTruthy();
    // The preview is the bytes, not a re-serialisation of them, so it is
    // compared as bytes — a text query would normalise the indentation away.
    const shown = [...container.querySelectorAll(".k-pre code")].map(
      (el) => el.textContent,
    );
    expect(shown).toEqual([MINIMAL_PROJECT.trimEnd(), MINIMAL_ENVIRONMENT.trimEnd()]);

    // The check stored nothing, and said so on the wire.
    expect(requests).toHaveLength(1);
    expect(requests[0]?.dryRun).toBe(DryRun.RENDER);
    expect(decoder.decode(requests[0]?.documents?.project)).toBe(MINIMAL_PROJECT);
    expect(
      decoder.decode(requests[0]?.documents?.environments["development"]),
    ).toBe(MINIMAL_ENVIRONMENT);

    fireEvent.click(screen.getByRole("button", { name: "Create hello" }));

    expect(await screen.findByText("The spec is stored")).toBeTruthy();
    expect(requests).toHaveLength(2);
    const write = requests[1];
    expect(write?.dryRun).toBe(DryRun.UNSPECIFIED); // this one writes
    expect(write?.version).toBe(""); // a create carries no version
    expect(write?.idempotencyKey).not.toBe("");
    expect(decoder.decode(write?.documents?.project)).toBe(MINIMAL_PROJECT);

    expect(
      screen.getByRole("link", { name: "Deploy now" }).getAttribute("href"),
    ).toBe("/apps/hello/development/deploy");
    expect(
      screen.getByRole("link", { name: "View app" }).getAttribute("href"),
    ).toBe("/apps/hello");
  });

  it("answers a taken name as a taken name, not as a version conflict", async () => {
    let call = 0;
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        putSpec: () => {
          call++;
          if (call === 1) return {};
          throw new ConnectError(
            "spec/hello [store/version-conflict] project \"hello\" already exists and the write carried no version",
            Code.FailedPrecondition,
            undefined,
            [
              {
                desc: ErrorSchema,
                value: {
                  code: "store/version-conflict",
                  resource: "spec/hello",
                  message: 'project "hello" already exists and the write carried no version',
                  remediation:
                    "read the spec with GetSpec and put it back with the version it returned",
                },
              },
            ],
          );
        },
      });
    });
    renderNew(transport);

    threeFields();
    submit();
    fireEvent.click(await screen.findByRole("button", { name: "Create hello" }));

    const taken = await screen.findByText(/already exists/);
    expect(field("Project name")).toContain("a project with this name already exists");
    // …and it offers the project that is in the way, rather than the store's
    // remediation about re-reading a version the user has never seen.
    expect(taken.closest(".k-field")?.querySelector("a")?.getAttribute("href")).toBe(
      "/apps/hello",
    );
    expect(screen.queryByText("Could not store the spec")).toBeNull();
  });

  it("drops the preview when the documents change under it", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SpecService, { putSpec: () => ({}) });
    });
    renderNew(transport);

    threeFields();
    submit();
    expect(await screen.findByText("What will be stored")).toBeTruthy();

    type("Image", "ghcr.io/acme/hello:1.5.0");
    expect(screen.queryByText("What will be stored")).toBeNull();
    expect(screen.getByRole("button", { name: "Check and preview" })).toBeTruthy();
  });

  it("makes an empty port a worker and a schedule a cron, and refuses both", () => {
    const transport = createRouterTransport((router) => {
      router.service(SpecService, { putSpec: () => ({}) });
    });
    renderNew(transport);

    type("Project name", "mailroom");
    type("Image", "acme/mailroom:2");
    openMore();
    expect(field("Port")).toContain("empty: a worker");

    type("Schedule", "0 3 * * *");
    expect(field("Port")).toContain("a schedule makes this a CronJob");

    type("Port", "8080");
    expect(field("Schedule")).toContain("port and schedule are mutually exclusive");
  });
});
