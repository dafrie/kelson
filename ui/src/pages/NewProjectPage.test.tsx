import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import { DryRun, ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import type { PutSpecRequest } from "../gen/kelson/v1alpha1/spec_pb";
import { BuildResponseSchema, BuildService } from "../gen/kelson/v1alpha1/build_pb";
import type { BuildRequest } from "../gen/kelson/v1alpha1/build_pb";
import { GitConnectionService } from "../gen/kelson/v1alpha1/gitconnection_pb";
import { renderAt } from "../test/render";
import { NewProjectPage } from "./NewProjectPage";

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
  return renderAt(transport, "/projects/new", "/projects/new", <NewProjectPage />);
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

  components:
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

const SOURCE_PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  source:
    git: https://github.com/acme/hello
    ref: main
  build:
    strategy: dockerfile

  components:
    - name: web
      port: 8080
`;

/** The same document, built the other way. */
const BUILDPACKS_PROJECT = SOURCE_PROJECT.replace(
  "strategy: dockerfile",
  "strategy: buildpacks",
);

/** Fills the git path's three fields and switches the form into it. */
function fromGit() {
  fireEvent.click(screen.getByLabelText("From Git repository"));
  type("Project name", "hello");
  type("Git repository", "https://github.com/acme/hello");
  type("Ref", "main");
  type("Port", "8080");
}

const encoder = new TextEncoder();

type Router = Parameters<Parameters<typeof createRouterTransport>[0]>[0];

/**
 * A SpecService that accepts what it is given and answers a RENDER preflight
 * the way the real one does: a document that builds from source reports
 * `image/unresolved`, because there is no image to render with yet, and one
 * that names an image renders clean.
 *
 * Keying off the document rather than the test is what makes the toggle
 * testable — switching back to an image has to stop producing the finding.
 */
function sourceSpecService(router: Router, requests: PutSpecRequest[]) {
  router.service(SpecService, {
    putSpec: (req) => {
      requests.push(req);
      const doc = decoder.decode(req.documents?.project);
      if (req.dryRun !== DryRun.RENDER || !doc.includes("  source:")) {
        return { spec: { project: "hello", version: "1", environments: ["development"] } };
      }
      return {
        errors: [
          {
            code: "image/unresolved",
            application: "web",
            message:
              "no image yet: the spec builds this component from source and no build result was supplied",
            remediation: "pass the built reference with --image, or set spec.image on the Project",
          },
        ],
      };
    },
  });
}

const BUILT = "ghcr.io/acme/hello@sha256:" + "a".repeat(64);

describe("NewProjectPage", () => {
  it("says a project is a container, and that this is its first component (#214)", () => {
    renderNew(createRouterTransport(() => {}));

    // The screen still asks three questions; what changed is that it no longer
    // implies a project is one app. The rest are added from the project's page.
    expect(
      screen.getByText(/This creates the project and its first component/),
    ).toBeTruthy();
  });

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
                field: "$.spec.components[0].port",
                message: "port must be 1-65535, got 70000",
                remediation: "set a valid TCP port, or omit port for a worker",
                line: 11,
                column: 7,
              },
              {
                code: "schema/invalid-format",
                resource: "Project/hello",
                field: "$.spec.components[0].health",
                message: 'health path "healthz" must start with /',
                remediation: "use a URL path such as /healthz",
              },
              {
                code: "semantic/no-image-source",
                resource: "Project/hello",
                field: "$.spec.components[0]",
                message: 'component "web" has no image source',
                remediation: "set image on the component or the Project",
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
    // A no-image-source error is about the component, but the only thing the
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

  it("writes a secret reference for a credential instead of refusing one", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        putSpec: (req) => {
          requests.push(req);
          return { spec: { project: "hello", version: "1", environments: ["development"] } };
        },
      });
    });
    renderNew(transport);

    threeFields();
    openMore();
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    type("Variable 1 name", "STRIPE_API_KEY");
    // The variable that used to be a dead end: a plain value here is
    // `secret/literal`, and before ADR-0018 there was no other spelling.
    type("Variable 1 form", "secret");
    type("Variable 1 secret name", "payments");
    type("Variable 1 secret key", "api-key");
    submit();

    await waitFor(() => expect(requests).toHaveLength(1));
    const doc = decoder.decode(requests[0]?.documents?.project);
    expect(doc).toContain("  env:\n    STRIPE_API_KEY: { secret: payments, key: api-key }\n");
    // No value input exists for a reference, so there is nothing on this screen
    // that could put a credential into the document.
    expect(screen.queryByLabelText("Variable 1 value")).toBeNull();
    expect(field("Variable 1 name")).toContain(
      "reads one key of a Secret in this environment's namespace",
    );
  });

  it("refuses to write half a reference", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        putSpec: (req) => {
          requests.push(req);
          return { spec: { project: "hello", version: "1", environments: ["development"] } };
        },
      });
    });
    renderNew(transport);

    threeFields();
    openMore();
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    type("Variable 1 name", "STRIPE_API_KEY");
    type("Variable 1 form", "secret");
    type("Variable 1 secret name", "payments");
    submit();

    // Nothing was sent: `{ secret: payments, key: "" }` is a mapping of the
    // right shape carrying no answer.
    expect(requests).toHaveLength(0);
    expect(
      screen.getByText(/a secret reference needs both a Secret name and a key/),
    ).toBeTruthy();
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
    ).toBe("/projects/hello/development/actions/deploy");
    expect(
      screen.getByRole("link", { name: "View project" }).getAttribute("href"),
    ).toBe("/projects/hello");
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
      "/projects/hello",
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
    expect(field("Port")).toContain("a schedule makes this a scheduled job");

    type("Port", "8080");
    expect(field("Schedule")).toContain("port and schedule are mutually exclusive");
  });
});

/**
 * The create-from-a-git-repository path (#63): the same two PutSpec calls, then
 * a build, then the deploy with what the build produced.
 *
 * The interesting difference from the image path is that the preflight comes
 * back with `image/unresolved` and the create is *still* legitimate — storing a
 * spec does not render it. That finding is shown rather than suppressed, and it
 * does not block.
 */
describe("NewProjectPage · from a Git repository", () => {
  it("writes source and build, and creates despite the unrenderable image", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      sourceSpecService(router, requests);
    });
    renderNew(transport);

    fromGit();
    expect(screen.queryByLabelText("Image")).toBeNull();
    submit();

    expect(await screen.findByText("What will be stored")).toBeTruthy();
    expect(decoder.decode(requests[0]?.documents?.project)).toBe(SOURCE_PROJECT);

    // The expected finding is on screen, with its code, and it is not the
    // rejection panel — the Create button is right there.
    expect(
      screen.getByText("The check could not render this yet, and that is expected"),
    ).toBeTruthy();
    expect(screen.getByText("image/unresolved")).toBeTruthy();
    expect(screen.queryByText("The server would not accept this spec")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Create hello" }));
    expect(await screen.findByText("The spec is stored")).toBeTruthy();

    // No "Deploy now": there is nothing to deploy until a build produces an
    // image, and offering it would offer a render that fails.
    expect(screen.queryByRole("link", { name: "Deploy now" })).toBeNull();
    expect(screen.getByRole("button", { name: "Build hello" })).toBeTruthy();
  });

  it("offers both build strategies, explains each, and defaults to the Dockerfile one", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      sourceSpecService(router, requests);
    });
    renderNew(transport);

    // The choice belongs to the git path: an image is not built at all.
    expect(screen.queryByLabelText("Dockerfile")).toBeNull();

    fromGit();
    const dockerfile = screen.getByLabelText("Dockerfile") as HTMLInputElement;
    const buildpacks = screen.getByLabelText("Buildpacks") as HTMLInputElement;
    expect(dockerfile.checked).toBe(true);
    expect(buildpacks.checked).toBe(false);

    // Both sentences are readable before either radio is touched — what decides
    // this choice is what each strategy needs from the repository.
    expect(
      screen.getByText(/the repository has a Dockerfile at its root/),
    ).toBeTruthy();
    expect(
      screen.getByText(/Cloud Native Buildpacks lifecycle detects the language/),
    ).toBeTruthy();

    submit();
    expect(await screen.findByText("What will be stored")).toBeTruthy();
    expect(decoder.decode(requests[0]?.documents?.project)).toBe(SOURCE_PROJECT);
  });

  it("writes the buildpacks strategy when that is the one picked", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      sourceSpecService(router, requests);
    });
    renderNew(transport);

    fromGit();
    fireEvent.click(screen.getByLabelText("Buildpacks"));
    submit();

    expect(await screen.findByText("What will be stored")).toBeTruthy();
    // Explicit, never `auto`: the server has no checkout to detect from (#50),
    // so a document that left the strategy out would store a build that refuses.
    expect(decoder.decode(requests[0]?.documents?.project)).toBe(BUILDPACKS_PROJECT);

    // …and the choice is the document's, so changing it drops the preview the
    // way every other edit does.
    fireEvent.click(screen.getByLabelText("Dockerfile"));
    expect(screen.queryByText("What will be stored")).toBeNull();
  });

  it("streams the build and hands the pinned reference to the deploy", async () => {
    const builds: BuildRequest[] = [];
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      sourceSpecService(router, requests);
      router.service(BuildService, {
        build: async function* (req) {
          builds.push(req);
          yield create(BuildResponseSchema, {
            event: {
              case: "started",
              value: {
                strategy: "dockerfile",
                imageRepository: "ghcr.io/acme/hello",
                tag: "hello-hello-0123456789ab",
                revision: "0123456789abcdef0123456789abcdef01234567",
              },
            },
          });
          yield create(BuildResponseSchema, {
            event: { case: "log", value: { chunk: encoder.encode("#1 [internal] load ") } },
          });
          yield create(BuildResponseSchema, {
            event: { case: "log", value: { chunk: encoder.encode("build definition\n") } },
          });
          yield create(BuildResponseSchema, {
            event: {
              case: "finished",
              value: { reference: BUILT, digest: "sha256:" + "a".repeat(64) },
            },
          });
        },
      });
    });
    renderNew(transport);

    fromGit();
    submit();
    fireEvent.click(await screen.findByRole("button", { name: "Create hello" }));
    fireEvent.click(await screen.findByRole("button", { name: "Build hello" }));

    expect(await screen.findByText("The image is pushed")).toBeTruthy();

    // The build named the stored spec by project — the browser does not resend
    // the documents it just wrote — and asked for the environment it created.
    expect(builds).toHaveLength(1);
    expect(builds[0]?.spec?.spec.case).toBe("project");
    expect(builds[0]?.spec?.spec.value).toBe("hello");
    expect(builds[0]?.environment).toBe("development");
    // The registry and the push secret are the server's, so the form sends
    // neither (docs/build.md).
    expect(builds[0]?.registry).toBe("");
    expect(builds[0]?.pushSecret).toBe("");

    // Started's settled facts are on screen…
    expect(screen.getByText("hello-hello-0123456789ab")).toBeTruthy();
    // …the chunks are one log, joined across the boundary they were split on…
    expect(screen.getByRole("log").textContent).toBe(
      "#1 [internal] load build definition\n",
    );
    // …and the deploy carries the pinned reference as the image override.
    expect(
      screen.getByRole("link", { name: "Deploy this image" }).getAttribute("href"),
    ).toBe(`/projects/hello/development/actions/deploy?image=${encodeURIComponent(BUILT)}`);
  });

  it("renders a failed build as the structured refusal it is", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      sourceSpecService(router, requests);
      router.service(BuildService, {
        // A build that refuses before it starts: no events at all, just the
        // structured error, which is what BuildService does for a build/*
        // refusal (proto/kelson/v1alpha1/build.proto).
        build: async function* () {
          throw new ConnectError(
            "[build/detection-needs-source] the build strategy is `auto` and detecting it needs to read the source tree",
            Code.InvalidArgument,
            undefined,
            [
              {
                desc: ErrorSchema,
                value: {
                  code: "build/detection-needs-source",
                  message:
                    "the build strategy is `auto` and detecting it needs to read the source tree",
                  remediation:
                    "set spec.build.strategy (`dockerfile` needs no detection); in-cluster detection is tracked by issue #50",
                },
              },
            ],
          );
        },
      });
    });
    renderNew(transport);

    fromGit();
    submit();
    fireEvent.click(await screen.findByRole("button", { name: "Create hello" }));
    fireEvent.click(await screen.findByRole("button", { name: "Build hello" }));

    const panel = (await screen.findByText("The build failed")).closest(".k-error");
    expect(panel?.textContent).toContain("build/detection-needs-source");
    expect(panel?.textContent).toContain("in-cluster detection is tracked by issue #50");
    expect(screen.queryByText("The image is pushed")).toBeNull();
  });

  it("aborts the build stream when the screen goes away", async () => {
    const requests: PutSpecRequest[] = [];
    let aborted = false;
    const transport = createRouterTransport((router) => {
      sourceSpecService(router, requests);
      router.service(BuildService, {
        build: async function* (_req, ctx) {
          ctx.signal.addEventListener("abort", () => {
            aborted = true;
          });
          yield create(BuildResponseSchema, {
            event: { case: "started", value: { strategy: "dockerfile" } },
          });
          // A build that never finishes: the only thing that ends this stream
          // is the caller abandoning it, which is exactly what is under test.
          await new Promise<void>((resolve) => {
            ctx.signal.addEventListener("abort", () => resolve());
          });
        },
      });
    });
    const { unmount } = renderNew(transport);

    fromGit();
    submit();
    fireEvent.click(await screen.findByRole("button", { name: "Create hello" }));
    fireEvent.click(await screen.findByRole("button", { name: "Build hello" }));
    await screen.findByText("dockerfile");

    unmount();

    await waitFor(() => expect(aborted).toBe(true));
  });

  it("returns to the image path with the git fields left behind", async () => {
    const requests: PutSpecRequest[] = [];
    const transport = createRouterTransport((router) => {
      sourceSpecService(router, requests);
    });
    renderNew(transport);

    fromGit();
    fireEvent.click(screen.getByLabelText("From image"));
    type("Image", "ghcr.io/acme/hello:1.4.2");
    submit();

    expect(await screen.findByText("What will be stored")).toBeTruthy();
    // The repository the user typed is still in the form's state; it must not
    // reach a document that says image.
    const written = decoder.decode(requests[0]?.documents?.project);
    expect(written).toContain("  image: ghcr.io/acme/hello:1.4.2\n");
    expect(written).not.toContain("source:");
    expect(written).not.toContain("build:");
  });

  /**
   * The repository picker (ADR-0033 decision 3, #248).
   *
   * Every test above renders against a transport with no GitConnectionService
   * at all, which is the state this screen has always been in and is asserted
   * below to still be: the listing fails, nothing is shown about it, and the
   * typed path is the whole form. That is what makes the picker an addition
   * rather than a replacement.
   */
  describe("the repository picker", () => {
    it("fills the source fields from a connection, a repository and a branch", async () => {
      const requests: PutSpecRequest[] = [];
      const branchRequests: string[] = [];
      const transport = createRouterTransport((router) => {
        sourceSpecService(router, requests);
        router.service(GitConnectionService, {
          listConnections: () => ({ connections: [ACME_GITHUB] }),
          listConnectionRepositories: () => ({ repositories: REPOSITORIES }),
          listConnectionBranches: (req) => {
            branchRequests.push(req.repository);
            return { branches: ["main", "release/1.4"] };
          },
        });
      });
      renderNew(transport);

      fireEvent.click(screen.getByLabelText("From Git repository"));
      type("Project name", "hello");
      type("Port", "8080");

      // The private badge is on the row it belongs to, and only there.
      const row = await screen.findByRole("button", { name: /acme\/checkout/ });
      expect(row.textContent).toContain("private");
      expect(
        screen.getByRole("button", { name: /acme\/site/ }).textContent,
      ).not.toContain("private");

      fireEvent.click(row);

      // Picking fills the same inputs the typed path uses — that is the whole
      // design, and it is asserted on the inputs rather than on the document
      // so that "the picker wrote somewhere else" cannot pass.
      await waitFor(() => {
        expect(
          (screen.getByLabelText("Git repository") as HTMLInputElement).value,
        ).toBe("https://github.com/acme/checkout");
      });
      expect((screen.getByLabelText("Ref") as HTMLInputElement).value).toBe(
        "main",
      );
      expect(branchRequests).toEqual(["acme/checkout"]);

      // The branch defaults to the repository's default and can be changed to
      // another the forge reported.
      const branch = await screen.findByLabelText("Branch");
      expect((branch as HTMLSelectElement).value).toBe("main");
      fireEvent.change(branch, { target: { value: "release/1.4" } });
      expect((screen.getByLabelText("Ref") as HTMLInputElement).value).toBe(
        "release/1.4",
      );

      submit();
      expect(await screen.findByText("What will be stored")).toBeTruthy();

      // And the document pins the connection, so resolution is the name the
      // user picked rather than a host match re-derived later (ADR-0033 d4).
      const written = decoder.decode(requests[0]?.documents?.project);
      expect(written).toContain("    git: https://github.com/acme/checkout\n");
      expect(written).toContain("    ref: release/1.4\n");
      expect(written).toContain("    connection: acme-github\n");
    });

    it("filters a long list client-side without asking the forge again", async () => {
      let listings = 0;
      const transport = createRouterTransport((router) => {
        sourceSpecService(router, []);
        router.service(GitConnectionService, {
          listConnections: () => ({ connections: [ACME_GITHUB] }),
          listConnectionRepositories: () => {
            listings++;
            return { repositories: manyRepositories(12) };
          },
          listConnectionBranches: () => ({ branches: ["main"] }),
        });
      });
      renderNew(transport);

      fireEvent.click(screen.getByLabelText("From Git repository"));
      await screen.findByRole("button", { name: /acme\/repo-0/ });

      fireEvent.change(screen.getByLabelText("Filter"), {
        target: { value: "repo-11" },
      });

      await waitFor(() => {
        expect(screen.queryByRole("button", { name: /acme\/repo-0/ })).toBeNull();
      });
      expect(screen.getByRole("button", { name: /acme\/repo-11/ })).toBeTruthy();
      expect(listings).toBe(1);
    });

    /**
     * The capability gate, from the browser's side: the refusal is shown as the
     * server wrote it, and the typed fields are still there to be used. It must
     * not render as a failure — nothing is broken — and it must not render as
     * an empty repository list, which is what an installation with nothing
     * selected looks like.
     */
    it("shows a capability refusal inline and leaves the typed path working", async () => {
      const requests: PutSpecRequest[] = [];
      const transport = createRouterTransport((router) => {
        sourceSpecService(router, requests);
        router.service(GitConnectionService, {
          listConnections: () => ({ connections: [INTERNAL_GIT] }),
          listConnectionRepositories: () => {
            throw new ConnectError(
              "internal-git speaks generic, and that adapter implements no repository browser",
              Code.Unimplemented,
              undefined,
              [
                {
                  desc: ErrorSchema,
                  value: {
                    code: "connection/capability-unsupported",
                    resource: "GitConnection/internal-git",
                    message:
                      'connection "internal-git" speaks "generic", and that adapter implements no repository browser',
                    remediation:
                      "paste the repository's URL instead — that path works for every connection",
                  },
                },
              ],
            );
          },
          listConnectionBranches: () => ({ branches: [] }),
        });
      });
      renderNew(transport);

      fireEvent.click(screen.getByLabelText("From Git repository"));

      expect(
        await screen.findByText(/implements no repository browser/),
      ).toBeTruthy();
      // The way out is on screen, in the server's own words.
      expect(
        screen.getByText(/paste the repository's URL instead/),
      ).toBeTruthy();
      // And it is not the failure panel: nothing here is broken.
      expect(screen.queryByText(/Could not list this connection/)).toBeNull();

      type("Project name", "hello");
      type("Git repository", "https://git.acme.internal/acme/hello");
      type("Ref", "main");
      type("Port", "8080");
      submit();

      expect(await screen.findByText("What will be stored")).toBeTruthy();
      const written = decoder.decode(requests[0]?.documents?.project);
      expect(written).toContain("    git: https://git.acme.internal/acme/hello\n");
      // Nothing was picked, so nothing is pinned: the source resolves by host
      // match, which is ADR-0033 decision 4's default.
      expect(written).not.toContain("connection:");
    });

    it("unpins the connection when the repository is retyped by hand", async () => {
      const requests: PutSpecRequest[] = [];
      const transport = createRouterTransport((router) => {
        sourceSpecService(router, requests);
        router.service(GitConnectionService, {
          listConnections: () => ({ connections: [ACME_GITHUB] }),
          listConnectionRepositories: () => ({ repositories: REPOSITORIES }),
          listConnectionBranches: () => ({ branches: ["main"] }),
        });
      });
      renderNew(transport);

      fireEvent.click(screen.getByLabelText("From Git repository"));
      type("Project name", "hello");
      type("Port", "8080");
      fireEvent.click(await screen.findByRole("button", { name: /acme\/checkout/ }));
      await waitFor(() => {
        expect(screen.getByText(/source.connection: acme-github/)).toBeTruthy();
      });

      // A pin that survived the URL being retyped would authenticate the next
      // build with a credential chosen for a different repository.
      type("Git repository", "https://github.com/other/thing");
      submit();

      expect(await screen.findByText("What will be stored")).toBeTruthy();
      const written = decoder.decode(requests[0]?.documents?.project);
      expect(written).toContain("    git: https://github.com/other/thing\n");
      expect(written).not.toContain("connection:");
    });

    /**
     * The state every other test in this file is in, asserted deliberately: an
     * instance with no connections — or a server that does not answer the
     * listing at all — is the form as it was, with nothing said about it.
     */
    it("is not offered when there are no connections, and says nothing about it", async () => {
      const requests: PutSpecRequest[] = [];
      const transport = createRouterTransport((router) => {
        sourceSpecService(router, requests);
        router.service(GitConnectionService, {
          listConnections: () => ({ connections: [] }),
        });
      });
      renderNew(transport);

      fromGit();

      expect(screen.queryByText("Pick from a connection")).toBeNull();
      expect(screen.queryByLabelText("Connection")).toBeNull();
      // No error either: having no connections is not a problem to report on
      // the screen somebody came to to create a project.
      expect(screen.queryByText(/Could not list/)).toBeNull();

      submit();
      expect(await screen.findByText("What will be stored")).toBeTruthy();
      expect(decoder.decode(requests[0]?.documents?.project)).toBe(SOURCE_PROJECT);
    });

    it("survives a server that does not serve GitConnectionService at all", async () => {
      const requests: PutSpecRequest[] = [];
      const transport = createRouterTransport((router) => {
        sourceSpecService(router, requests);
      });
      renderNew(transport);

      fromGit();
      submit();

      expect(await screen.findByText("What will be stored")).toBeTruthy();
      expect(screen.queryByText("Pick from a connection")).toBeNull();
      expect(decoder.decode(requests[0]?.documents?.project)).toBe(SOURCE_PROJECT);
    });
  });
});

/** A connection whose provider browses, and one whose provider does not. */
const ACME_GITHUB = {
  name: "acme-github",
  provider: "github",
  host: "https://github.com",
  ready: true,
  reachable: true,
};

const INTERNAL_GIT = {
  name: "internal-git",
  provider: "generic",
  host: "https://git.acme.internal",
  ready: true,
  reachable: true,
};

const REPOSITORIES = [
  {
    fullName: "acme/checkout",
    htmlUrl: "https://github.com/acme/checkout",
    defaultBranch: "main",
    private: true,
  },
  {
    fullName: "acme/site",
    htmlUrl: "https://github.com/acme/site",
    defaultBranch: "trunk",
    private: false,
  },
];

/** Enough repositories for the list to earn a filter box. */
function manyRepositories(n: number) {
  return Array.from({ length: n }, (_, i) => ({
    fullName: `acme/repo-${i}`,
    htmlUrl: `https://github.com/acme/repo-${i}`,
    defaultBranch: "main",
    private: false,
  }));
}
