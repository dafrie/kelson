import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Link } from "react-router-dom";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import type { PutSpecRequest } from "../gen/kelson/v1alpha1/spec_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { RenderService } from "../gen/kelson/v1alpha1/render_pb";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import { renderAt } from "../test/render";
import { EditSpecPage } from "./EditSpecPage";

const ENCODER = new TextEncoder();
const DECODER = new TextDecoder();

/** The #63 builder's own output: the document the form is allowed to edit. */
const UI_PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  components:
    - name: web
      port: 8080

    - name: worker
`;

const UI_ENVIRONMENT = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development

spec:
  project: hello
`;

/** The same document carrying an ADR-0018 secret reference in its env. */
const REFERENCED_PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  env:
    DATABASE_URL: { secret: checkout-db, key: url }

  components:
    - name: web
      port: 8080

    - name: worker
`;

const HAND_WRITTEN = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

# the one that serves traffic
spec:
  image: ghcr.io/acme/hello:1.4.2
  components:
    - name: web
      port: 8080
`;

function diffJson(environment: string): Uint8Array {
  return ENCODER.encode(
    JSON.stringify({
      level: "rendered",
      project: "hello",
      environment,
      resources: [
        {
          apiVersion: "apps/v1",
          kind: "Deployment",
          name: "web",
          namespace: `hello-${environment}`,
          op: "modified",
          risk: "restart-required",
          fields: [
            {
              path: "spec.template.spec.containers[0].env[0]",
              after: "LOG_LEVEL=debug",
              origin: "spec",
              specPath: "$.spec.env.LOG_LEVEL",
              risk: "restart-required",
            },
          ],
        },
      ],
      summary: { added: 0, modified: 1, removed: 0, maxRisk: "restart-required" },
    }),
  );
}

interface Stub {
  /** Findings PutSpec at RENDER answers with. */
  findings?: ReturnType<typeof create<typeof ErrorSchema>>[];
  /** How many real (non-dry-run) writes fail with a version conflict first. */
  conflicts?: number;
  project?: string;
  version?: string;
}

interface Recorder {
  writes: PutSpecRequest[];
  diffs: { environment: string; from: string; spec: string }[];
  gets: number;
}

function stubTransport(stub: Stub = {}) {
  const recorder: Recorder = { writes: [], diffs: [], gets: 0 };
  let conflicts = stub.conflicts ?? 0;
  let version = stub.version ?? "42";

  const transport = createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => {
        recorder.gets += 1;
        return {
          spec: {
            project: "hello",
            version,
            environments: ["development"],
            documents: {
              project: ENCODER.encode(stub.project ?? UI_PROJECT),
              environments: { development: ENCODER.encode(UI_ENVIRONMENT) },
            },
          },
        };
      },
      putSpec: (req) => {
        recorder.writes.push(req);
        if (req.dryRun === DryRun.RENDER) {
          return { errors: stub.findings ?? [] };
        }
        if (conflicts > 0 && !req.force) {
          conflicts -= 1;
          // The store's own answer: a stale version is refused, never merged.
          version = "43";
          throw new ConnectError(
            `project "hello" changed while this write was in flight`,
            Code.FailedPrecondition,
            undefined,
            [{ desc: ErrorSchema, value: create(ErrorSchema, {
              code: "store/version-conflict",
              resource: "Spec/hello",
              message: `project "hello" changed while this write was in flight`,
              remediation: "re-read the spec with GetSpec and retry the write with the version it returns",
            }) }],
          );
        }
        return { spec: { project: "hello", version: "44" } };
      },
    });

    router.service(RenderService, {
      diff: (req) => {
        recorder.diffs.push({
          environment: req.environment,
          from: DECODER.decode(req.from?.project ?? new Uint8Array()),
          spec: DECODER.decode(
            req.spec?.spec.case === "documents"
              ? req.spec.spec.value.project
              : new Uint8Array(),
          ),
        });
        return { diffJson: diffJson(req.environment), exitSemantics: 2 };
      },
    });
  });

  return { transport, recorder };
}

function renderEditor(stub: Stub = {}) {
  const { transport, recorder } = stubTransport(stub);
  const view = renderAt(
    transport,
    "/projects/hello/edit",
    "/projects/:project/edit",
    <EditSpecPage />,
    [
      {
        path: "/projects/:project",
        element: (
          <div>
            <span>the project detail screen</span>
            <Link to="/projects/hello/edit">back to the editor</Link>
          </div>
        ),
      },
    ],
  );
  return { ...view, recorder };
}

/** The bytes the last write carried for the Project document. */
function writtenProject(recorder: Recorder, index = -1): string {
  const write = recorder.writes.at(index);
  return DECODER.decode(write?.documents?.project ?? new Uint8Array());
}

async function openedOnForm() {
  return screen.findAllByRole("button", { name: "Add variable" });
}

/** The bytes the last write carried for the one Environment document. */
function writtenEnvironment(recorder: Recorder, index = -1): string {
  const write = recorder.writes.at(index);
  return DECODER.decode(
    write?.documents?.environments["development"] ?? new Uint8Array(),
  );
}

describe("EditSpecPage", () => {
  it("opens the form for a document the UI wrote", async () => {
    renderEditor();

    await openedOnForm();
    expect(screen.getByRole("button", { name: "Form" }).className).toContain(
      "k-tab--active",
    );
    expect(screen.queryByText(/was hand-edited/)).toBeNull();
    // Both components are reachable, not only the first: each has its own
    // section, named, with the workload its shape derives.
    expect(screen.getAllByText("web").length).toBeGreaterThan(0);
    expect(screen.getAllByText("worker").length).toBeGreaterThan(0);
    expect(screen.getAllByLabelText("Replicas (min)")).toHaveLength(2);
  });

  it("authors spec.previews from the form, in the styling ADR-0017 documents", async () => {
    const { recorder } = renderEditor();
    await openedOnForm();

    // Previews render in flux mode only, so the mode and its deployment
    // repository are part of the same act of turning them on.
    fireEvent.change(screen.getByLabelText("Delivery mode"), {
      target: { value: "flux" },
    });
    fireEvent.change(screen.getByLabelText("Deployment repository"), {
      target: { value: "git@github.com:acme/deploy.git" },
    });

    fireEvent.click(
      screen.getByRole("checkbox", {
        name: /Spawn a preview environment per open pull request/,
      }),
    );
    fireEvent.change(screen.getByLabelText("Source repository"), {
      target: { value: "https://github.com/acme/hello" },
    });
    fireEvent.change(screen.getByLabelText("Forge credential"), {
      target: { value: "github-auth" },
    });
    fireEvent.change(screen.getByLabelText("Labels"), {
      target: { value: "deploy/preview, deploy/db" },
    });
    fireEvent.change(screen.getByLabelText("Artifact repository"), {
      target: { value: "oci://ghcr.io/acme/hello-previews" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));

    const doc = writtenEnvironment(recorder);
    expect(doc).toContain("  delivery:\n    mode: flux\n");
    expect(doc).toContain("      repo: git@github.com:acme/deploy.git\n");
    expect(doc).toContain("  previews:\n    provider: github\n");
    expect(doc).toContain("    repo: https://github.com/acme/hello\n");
    // A Secret name, never a token: there is no field here a value could go in.
    expect(doc).toContain("    secretRef: github-auth\n");
    // The documented flow styling, so a block written here and a block copied
    // from docs/model.md are the same bytes.
    expect(doc).toContain("      labels: [deploy/preview, deploy/db]\n");
    expect(doc).toContain("      repository: oci://ghcr.io/acme/hello-previews\n");
    // The defaults are kelson's, not the form's: an unset interval and limit
    // are absent rather than written out as 10m and 10.
    expect(doc).not.toContain("    interval:");
    expect(doc).not.toContain("      limit:");
  });

  it("removes the whole previews block when it is turned off", async () => {
    renderEditor();
    await openedOnForm();

    const toggle = screen.getByRole("checkbox", {
      name: /Spawn a preview environment per open pull request/,
    });
    fireEvent.click(toggle);
    expect(screen.getByLabelText("Artifact repository")).toBeTruthy();

    fireEvent.click(toggle);
    // The schema has no `enabled:` key and the form does not invent one: an
    // environment either declares previews or does not.
    expect(screen.queryByLabelText("Artifact repository")).toBeNull();
    // And the document is the stored one again, byte for byte — which the
    // editor states by having nothing to check.
    expect(screen.getByText("nothing has changed yet")).toBeTruthy();
  });

  it("says previews need flux mode instead of disabling the control", async () => {
    renderEditor();
    await openedOnForm();

    fireEvent.click(
      screen.getByRole("checkbox", {
        name: /Spawn a preview environment per open pull request/,
      }),
    );
    // The mode is still unset, and the note says what will happen rather than
    // a disabled checkbox that cannot explain itself.
    expect(
      screen.getByText(/previews render in flux delivery mode only/),
    ).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Delivery mode"), {
      target: { value: "flux" },
    });
    // And once the mode is right, the note becomes the other half nobody
    // remembers: the CI step that publishes the artifacts.
    expect(screen.getAllByText(/kelson preview publish/).length).toBeGreaterThan(0);
  });

  it("maps a previews finding onto the input that holds it", async () => {
    renderEditor({
      findings: [
        create(ErrorSchema, {
          code: "schema/invalid-format",
          resource: "Environment/development",
          field: "$.spec.previews.artifacts.repository",
          message: '"oci://ghcr.io/acme/p:latest" carries a tag or a digest',
          remediation: "drop the tag: each preview is pulled at its head commit SHA",
        }),
      ],
    });
    await openedOnForm();

    fireEvent.click(
      screen.getByRole("checkbox", {
        name: /Spawn a preview environment per open pull request/,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));

    expect(await screen.findByText("schema/invalid-format")).toBeTruthy();
    expect(
      screen.getByLabelText("Artifact repository").getAttribute("aria-invalid"),
    ).toBe("true");
  });

  it("shows a stored secret reference as a reference, and writes it back unchanged", async () => {
    const { recorder } = renderEditor({ project: REFERENCED_PROJECT });
    await openedOnForm();

    // Displayed as what it is: the form picker on the reference, the Secret's
    // name and key in their own inputs, and no value input anywhere.
    expect(
      (screen.getByLabelText("Environment variables 1 form") as HTMLSelectElement).value,
    ).toBe("secret");
    expect(
      (screen.getByLabelText("Environment variables 1 secret name") as HTMLInputElement)
        .value,
    ).toBe("checkout-db");
    expect(
      (screen.getByLabelText("Environment variables 1 secret key") as HTMLInputElement)
        .value,
    ).toBe("url");
    expect(screen.queryByLabelText("Environment variables 1 value")).toBeNull();
    // The document round-trips, so the form is live rather than read-only.
    expect(screen.queryByText(/was hand-edited/)).toBeNull();

    // Turning a second variable into a reference writes the same spelling the
    // Secrets panel offers to copy.
    const adds = screen.getAllByRole("button", { name: "Add variable" });
    fireEvent.click(adds[0]!);
    fireEvent.change(screen.getByLabelText("Environment variables 2 name"), {
      target: { value: "STRIPE_API_KEY" },
    });
    fireEvent.change(screen.getByLabelText("Environment variables 2 form"), {
      target: { value: "secret" },
    });
    fireEvent.change(screen.getByLabelText("Environment variables 2 secret name"), {
      target: { value: "payments" },
    });
    fireEvent.change(screen.getByLabelText("Environment variables 2 secret key"), {
      target: { value: "api-key" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));

    expect(writtenProject(recorder)).toBe(
      REFERENCED_PROJECT.replace(
        "    DATABASE_URL: { secret: checkout-db, key: url }\n",
        "    DATABASE_URL: { secret: checkout-db, key: url }\n" +
          "    STRIPE_API_KEY: { secret: payments, key: api-key }\n",
      ),
    );
  });

  it("sends a block-styled reference to the YAML tab rather than restyling it", async () => {
    // The same reference, authored as a block mapping. It parses — the form
    // shows it — but rebuilding it would rewrite the file into the flow styling
    // this builder emits, so the byte guard refuses the write.
    renderEditor({
      project: REFERENCED_PROJECT.replace(
        "    DATABASE_URL: { secret: checkout-db, key: url }\n",
        "    DATABASE_URL:\n      secret: checkout-db\n      key: url\n",
      ),
    });

    const textarea = (await screen.findByRole("textbox", {
      name: "Project document",
    })) as HTMLTextAreaElement;
    expect(textarea.value).toContain("    DATABASE_URL:\n      secret: checkout-db\n");

    fireEvent.click(screen.getByRole("button", { name: "Form" }));
    expect(screen.getByText(/was hand-edited — use the YAML tab/)).toBeTruthy();
    // Still shown correctly, just not editable here.
    expect(
      (screen.getByLabelText("Environment variables 1 secret name") as HTMLInputElement)
        .value,
    ).toBe("checkout-db");
  });

  it("makes the YAML tab primary and the form read-only for a hand-edited document", async () => {
    renderEditor({ project: HAND_WRITTEN });

    const textarea = (await screen.findByRole("textbox", {
      name: "Project document",
    })) as HTMLTextAreaElement;
    expect(screen.getByRole("button", { name: "YAML" }).className).toContain(
      "k-tab--active",
    );
    // Byte-faithful: the comment is still there, which is the whole reason the
    // form stepped aside.
    expect(textarea.value).toBe(HAND_WRITTEN);

    fireEvent.click(screen.getByRole("button", { name: "Form" }));
    expect(screen.getByText(/was hand-edited — use the YAML tab/)).toBeTruthy();
    const image = screen.getByLabelText("Image") as HTMLInputElement;
    expect(image.readOnly).toBe(true);
    expect(screen.queryByRole("button", { name: "Add variable" })).toBeNull();
  });

  it("puts a project-level variable on the Project document and a per-component one on its component", async () => {
    const { recorder } = renderEditor();
    await openedOnForm();

    // Project scope: the first "Add variable" belongs to the Project section.
    const adds = screen.getAllByRole("button", { name: "Add variable" });
    fireEvent.click(adds[0]!);
    fireEvent.change(screen.getByLabelText("Environment variables 1 name"), {
      target: { value: "LOG_LEVEL" },
    });
    fireEvent.change(screen.getByLabelText("Environment variables 1 value"), {
      target: { value: "debug" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));

    expect(writtenProject(recorder)).toBe(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  env:
    LOG_LEVEL: debug

  components:
    - name: web
      port: 8080

    - name: worker
`);
  });

  it("removes and changes variables, and quotes a value YAML would not keep as a string", async () => {
    const { recorder } = renderEditor();
    await openedOnForm();

    // Per-component scope: the worker's own section, the last one on screen.
    const adds = screen.getAllByRole("button", { name: "Add variable" });
    fireEvent.click(adds.at(-1)!);
    fireEvent.change(screen.getByLabelText("Environment variables 1 name"), {
      target: { value: "PORT" },
    });
    fireEvent.change(screen.getByLabelText("Environment variables 1 value"), {
      target: { value: "3000" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));
    expect(writtenProject(recorder)).toContain(
      '    - name: worker\n      env:\n        PORT: "3000"\n',
    );

    // …and removing it puts the document back exactly as it was stored.
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(
      (screen.getByRole("button", {
        name: "Check and preview the diff",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("maps findings onto the field that caused them, including components[1]", async () => {
    const { recorder } = renderEditor({
      findings: [
        create(ErrorSchema, {
          code: "schema/invalid-value",
          resource: "Project/hello",
          field: "$.spec.components[1].replicas.min",
          message: "replicas.min must not be negative",
          line: 14,
          column: 7,
        }),
        create(ErrorSchema, {
          code: "schema/unknown-field",
          resource: "Project/hello",
          field: "$.spec.overlays[0].patch",
          message: "overlays are not implemented yet",
        }),
      ],
    });
    await openedOnForm();

    fireEvent.change(screen.getAllByLabelText("Replicas (min)").at(-1)!, {
      target: { value: "-1" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));

    // Next to the input that wrote the path…
    const inline = await screen.findByText("replicas.min must not be negative");
    expect(inline.closest(".k-field")?.textContent).toContain("Replicas (min)");
    // …and the rule the form does not model reaches the panel whole.
    expect(screen.getByText("overlays are not implemented yet")).toBeTruthy();
    // No diff was requested: an invalid spec has nothing to preview.
    expect(recorder.diffs).toHaveLength(0);
    expect(
      (screen.getByRole("button", { name: "Save hello" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  it("shows a YAML-tab finding with the line and column the server gave it", async () => {
    renderEditor({
      project: HAND_WRITTEN,
      findings: [
        create(ErrorSchema, {
          code: "schema/invalid-format",
          resource: "Project/hello",
          field: "$.spec.components[0].port",
          message: "port must be an integer",
          line: 10,
          column: 13,
        }),
      ],
    });

    const textarea = await screen.findByRole("textbox", { name: "Project document" });
    fireEvent.change(textarea, {
      target: { value: HAND_WRITTEN.replace("port: 8080", "port: eighty-eighty") },
    });
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));

    expect(await screen.findByText("port must be an integer")).toBeTruthy();
    expect(screen.getByText("line 10:13")).toBeTruthy();
  });

  it("previews the diff against the stored documents before saving, then saves with the version and an idempotency key", async () => {
    const { recorder } = renderEditor();
    await openedOnForm();

    fireEvent.change(screen.getByLabelText("Image"), {
      target: { value: "ghcr.io/acme/hello:2.0.0" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));

    // The diff is asked per environment, `from` the bytes that are stored now.
    await waitFor(() => expect(recorder.diffs).toHaveLength(1));
    expect(recorder.diffs[0]?.environment).toBe("development");
    expect(recorder.diffs[0]?.from).toBe(UI_PROJECT);
    expect(recorder.diffs[0]?.spec).toContain("image: ghcr.io/acme/hello:2.0.0");
    expect(await screen.findByText("What changes (1)")).toBeTruthy();
    expect(screen.getByText("Deployment/web")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Save hello" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(2));

    const write = recorder.writes[1];
    expect(write?.dryRun).toBe(DryRun.UNSPECIFIED);
    expect(write?.version).toBe("42");
    expect(write?.force).toBe(false);
    expect(write?.idempotencyKey).not.toBe("");
    expect(await screen.findByText("The spec is stored")).toBeTruthy();
  });

  it("surfaces a version conflict as its own state, with reload and a labelled force", async () => {
    const { recorder } = renderEditor({ conflicts: 1 });
    await openedOnForm();

    fireEvent.change(screen.getByLabelText("Image"), {
      target: { value: "ghcr.io/acme/hello:2.0.0" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await screen.findByText("What changes (1)");
    fireEvent.click(screen.getByRole("button", { name: "Save hello" }));

    expect(
      await screen.findByText("The spec changed while you were editing"),
    ).toBeTruthy();
    // Nothing was silently lost: the edited bytes are one click away.
    expect(
      screen.getByRole("button", { name: /copy your edited Project document/ }),
    ).toBeTruthy();

    const force = screen.getByRole("button", { name: "Overwrite their version" });
    expect(force.className).toContain("k-button--danger");
    expect(screen.getByText(/their change is lost/)).toBeTruthy();

    fireEvent.click(force);
    await waitFor(() => expect(recorder.writes).toHaveLength(3));
    expect(recorder.writes[2]?.force).toBe(true);
    expect(await screen.findByText("The spec is stored")).toBeTruthy();
  });

  it("reloads fresh bytes on a conflict rather than merging the edits", async () => {
    const { recorder } = renderEditor({ conflicts: 1 });
    await openedOnForm();

    fireEvent.change(screen.getByLabelText("Image"), {
      target: { value: "ghcr.io/acme/hello:2.0.0" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await screen.findByText("What changes (1)");
    fireEvent.click(screen.getByRole("button", { name: "Save hello" }));
    await screen.findByText("The spec changed while you were editing");

    const gets = recorder.gets;
    fireEvent.click(screen.getByRole("button", { name: "Reload the stored spec" }));

    await waitFor(() => expect(recorder.gets).toBe(gets + 1));
    await waitFor(() =>
      expect((screen.getByLabelText("Image") as HTMLInputElement).value).toBe(
        "ghcr.io/acme/hello:1.4.2",
      ),
    );
    expect(screen.queryByText("The spec changed while you were editing")).toBeNull();
  });

  it("blocks a navigation away from unsaved changes until it is answered", async () => {
    renderEditor();
    await openedOnForm();

    fireEvent.change(screen.getByLabelText("Image"), {
      target: { value: "ghcr.io/acme/hello:2.0.0" },
    });
    fireEvent.click(screen.getByRole("link", { name: "← hello" }));

    expect(await screen.findByText("This spec has unsaved changes")).toBeTruthy();
    expect(screen.queryByText("the project detail screen")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Stay on this page" }));
    await waitFor(() =>
      expect(screen.queryByText("This spec has unsaved changes")).toBeNull(),
    );
    expect(screen.getByLabelText("Image")).toBeTruthy();

    fireEvent.click(screen.getByRole("link", { name: "← hello" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard and leave" }));
    expect(await screen.findByText("the project detail screen")).toBeTruthy();
  });

  it("lets a navigation through when nothing has been edited", async () => {
    renderEditor();
    await openedOnForm();

    fireEvent.click(screen.getByRole("link", { name: "← hello" }));
    expect(await screen.findByText("the project detail screen")).toBeTruthy();
  });
});
