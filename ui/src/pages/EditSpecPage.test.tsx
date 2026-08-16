import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Link } from "react-router-dom";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import type {
  ProposeSpecRequest,
  PutSpecRequest,
} from "../gen/kelson/v1alpha1/spec_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { GitConnectionService } from "../gen/kelson/v1alpha1/gitconnection_pb";
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
  environment?: string;
  version?: string;
  /** Documents somebody else's Flux reconciles, as GetSpec reports them (#248). */
  gitops?: { document: string; kustomization: string; namespace: string }[];
  /** Findings ProposeSpec answers with instead of opening anything. */
  proposalFindings?: ReturnType<typeof create<typeof ErrorSchema>>[];
}

interface Recorder {
  writes: PutSpecRequest[];
  diffs: { environment: string; from: string; spec: string }[];
  gets: number;
  proposals: ProposeSpecRequest[];
}

function stubTransport(stub: Stub = {}) {
  const recorder: Recorder = { writes: [], diffs: [], gets: 0, proposals: [] };
  let conflicts = stub.conflicts ?? 0;
  let version = stub.version ?? "42";

  const transport = createRouterTransport((router) => {
    router.service(GitConnectionService, {
      listConnections: () => ({
        connections: [{ name: "acme-github", host: "https://github.com", provider: "github" }],
      }),
    });

    router.service(SpecService, {
      proposeSpec: (req) => {
        recorder.proposals.push(req);
        if ((stub.proposalFindings ?? []).length > 0) {
          return { errors: stub.proposalFindings ?? [] };
        }
        return {
          url: "https://github.test/acme/gitops/pull/7",
          branch: "kelson/hello-abcd1234",
          baseBranch: req.baseBranch,
        };
      },
      getSpec: () => {
        recorder.gets += 1;
        return {
          spec: {
            project: "hello",
            version,
            environments: ["development"],
            gitops: stub.gitops ?? [],
            documents: {
              project: ENCODER.encode(stub.project ?? UI_PROJECT),
              environments: {
                development: ENCODER.encode(stub.environment ?? UI_ENVIRONMENT),
              },
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

function renderEditor(stub: Stub = {}, path = "/projects/hello/edit") {
  const { transport, recorder } = stubTransport(stub);
  const view = renderAt(
    transport,
    path,
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

  it("offers no delivery mode, which the model no longer has", async () => {
    renderEditor();
    await openedOnForm();

    // `spec.delivery` — the mode, the git target, the enum — was deleted from
    // the model (ADR-0028, #234) and the server refuses a document carrying
    // one, so the form must not be able to author it.
    expect(screen.queryByLabelText("Delivery mode")).toBeNull();
    expect(screen.queryByLabelText("Deployment repository")).toBeNull();

    fireEvent.click(
      screen.getByRole("checkbox", {
        name: /Spawn a preview environment per open pull request/,
      }),
    );
    // Turning previews on needs nothing set above it. The note is the other
    // half nobody remembers: the CI step that publishes the artifacts.
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

/**
 * Adding a component (#214).
 *
 * The assertion that matters is on the bytes: the stored document is the
 * user's file, so an append has to be the stored bytes plus one entry, and a
 * document this UI cannot rebuild has to get the entry handed over instead of
 * being rewritten into something the form understood.
 */
describe("EditSpecPage · adding a component", () => {
  /**
   * The panel's own fields, by its landmark. They are named what the component
   * sections above name theirs — a port is a port — so the region is what tells
   * the two apart, for a screen reader and for this test alike.
   */
  function panel() {
    return within(screen.getByRole("region", { name: "Add a component" }));
  }

  async function openAdd() {
    await openedOnForm();
    fireEvent.click(panel().getByRole("button", { name: "Add component" }));
  }

  it("appends a service to the stored document without touching the rest of it", async () => {
    const { recorder } = renderEditor();
    await openAdd();

    fireEvent.change(panel().getByLabelText("Component name"), {
      target: { value: "api" },
    });
    fireEvent.change(panel().getByLabelText("Port"), { target: { value: "9090" } });
    fireEvent.click(panel().getByRole("button", { name: "Add to the spec" }));

    // It is in the form immediately, as a component like any other…
    expect(await screen.findByText(/was added to this project's components/)).toBeTruthy();
    expect(screen.getAllByLabelText("Replicas (min)")).toHaveLength(3);

    // …and nothing is stored until the same check and save every other edit
    // goes through.
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));
    expect(writtenProject(recorder)).toBe(
      `${UI_PROJECT}\n    - name: api\n      port: 9090\n`,
    );
  });

  it("writes a data component as a name and a kind, and states it rather than editing it", async () => {
    const { recorder } = renderEditor();
    await openAdd();

    fireEvent.click(panel().getByRole("radio", { name: "PostgreSQL" }));
    fireEvent.change(panel().getByLabelText("Component name"), {
      target: { value: "db" },
    });
    // A data component has no image question: what it runs is its operator's
    // business (ADR-0005).
    expect(panel().queryByRole("radio", { name: "Its own image" })).toBeNull();
    fireEvent.click(panel().getByRole("button", { name: "Add to the spec" }));

    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));
    expect(writtenProject(recorder)).toBe(`${UI_PROJECT}\n    - name: db\n      kind: postgres\n`);

    // In the form it is a statement, not a set of fields: no image to roll and
    // no replicas to scale (#107). Three components, still two editable ones.
    expect(screen.getAllByLabelText("Replicas (min)")).toHaveLength(2);
    expect(screen.getByText(/A managed data service/)).toBeTruthy();
    // And the form is still live: the parser and the builder learned the same
    // key, so the document is still one this UI can rebuild.
    expect(screen.queryByText(/was hand-edited/)).toBeNull();
  });

  it("writes no image for a component that inherits the project's, and one for a component that does not", async () => {
    const { recorder } = renderEditor();
    await openAdd();

    fireEvent.click(panel().getByRole("radio", { name: "Worker" }));
    fireEvent.change(panel().getByLabelText("Component name"), {
      target: { value: "mailer" },
    });
    // The default is inheritance, which is rule P3 and how one repository ships
    // a web process and a worker.
    expect(
      (panel().getByRole("radio", { name: "The project's image" }) as HTMLInputElement)
        .checked,
    ).toBe(true);
    fireEvent.click(panel().getByRole("radio", { name: "Its own image" }));
    fireEvent.change(panel().getByLabelText("Image"), {
      target: { value: "ghcr.io/acme/hello-mailer:1.4.2" },
    });
    fireEvent.click(panel().getByRole("button", { name: "Add to the spec" }));

    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await waitFor(() => expect(recorder.writes).toHaveLength(1));
    expect(writtenProject(recorder)).toContain(
      "\n    - name: mailer\n      image: ghcr.io/acme/hello-mailer:1.4.2\n",
    );
  });

  it("refuses a name the project already uses, and one that is not a label", async () => {
    renderEditor();
    await openAdd();

    fireEvent.click(panel().getByRole("radio", { name: "Worker" }));
    fireEvent.change(panel().getByLabelText("Component name"), {
      target: { value: "worker" },
    });
    fireEvent.click(panel().getByRole("button", { name: "Add to the spec" }));

    expect(
      screen.getByText(/already has a component called "worker"/),
    ).toBeTruthy();
    expect(
      panel().getByLabelText("Component name").getAttribute("aria-invalid"),
    ).toBe("true");
    // Nothing was appended: the document is still the stored one.
    expect(
      (screen.getByRole("button", {
        name: "Check and preview the diff",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);

    fireEvent.change(panel().getByLabelText("Component name"), {
      target: { value: "Mail Worker" },
    });
    expect(screen.getByText(/not a DNS-1123 label/)).toBeTruthy();
  });

  it("hands over the entry rather than rewriting a hand-edited document", async () => {
    renderEditor({ project: HAND_WRITTEN });

    // This document opens on the YAML tab, which is exactly where the reader
    // who wants a second component is standing.
    await screen.findByRole("textbox", { name: "Project document" });
    fireEvent.click(panel().getByRole("button", { name: "Add component" }));
    fireEvent.click(panel().getByRole("radio", { name: "Worker" }));
    fireEvent.change(panel().getByLabelText("Component name"), {
      target: { value: "worker" },
    });
    fireEvent.click(panel().getByRole("button", { name: "Add to the spec" }));

    expect(
      screen.getByText("This document cannot be rebuilt from the form"),
    ).toBeTruthy();
    // The block to paste, and a way to take it…
    expect(screen.getByText(/- name: worker/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: /copy the component entry/ }),
    ).toBeTruthy();
    // …and the document itself is untouched, comment and all.
    expect(
      (screen.getByRole("textbox", { name: "Project document" }) as HTMLTextAreaElement)
        .value,
    ).toBe(HAND_WRITTEN);
    expect(
      (screen.getByRole("button", {
        name: "Check and preview the diff",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("opens on the add panel when the project page sent the reader to add one", async () => {
    renderEditor({}, "/projects/hello/edit?add=component");

    // No press needed: the action pressed on the project page is the thing in
    // front of the reader when the screen arrives.
    await openedOnForm();
    expect(panel().getByLabelText("Component name")).toBeTruthy();
    expect(panel().queryByRole("button", { name: "Add component" })).toBeNull();
  });
});

/**
 * The GitOps half (#248, ADR-0033 decision 3).
 *
 * The screen's contract for a document another Flux reconciles: say who owns
 * it, take Save away, and put the two things that do survive in its place.
 */
describe("EditSpecPage, GitOps-managed", () => {
  const OWNED = [
    { document: "project", kustomization: "apps", namespace: "flux-system" },
    { document: "development", kustomization: "apps", namespace: "flux-system" },
  ];

  /** An environment document declaring the field ADR-0036 decision 5 warns about. */
  const TRACKING_ENVIRONMENT = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development

spec:
  project: hello
  autoDeploy: true
`;

  async function edited(stub: Stub) {
    const view = renderEditor(stub);
    await openedOnForm();
    fireEvent.change(screen.getByLabelText("Image"), {
      target: { value: "ghcr.io/acme/hello:2.0.0" },
    });
    return view;
  }

  const saveButton = /^Save hello$/;
  const proposeHeading = "Propose as a pull request";

  it("names the Kustomization and takes Save away", async () => {
    renderEditor({ gitops: OWNED });
    await openedOnForm();

    expect(screen.getByText(/reconciled from a repository by/)).toBeTruthy();
    // Named in the banner and again beside each document in the export.
    expect(screen.getAllByText(/flux-system.apps/).length).toBeGreaterThan(0);
    // No Save at all, rather than a Save that can never be pressed: a dead
    // button above a panel explaining why is two things to read for one answer.
    expect(screen.queryByRole("button", { name: saveButton })).toBeNull();
    // …and the export is there without checking anything first, because it is
    // bytes the page already holds.
    expect(screen.getByRole("heading", { name: "Export" })).toBeTruthy();
  });

  it("leaves an ordinary project alone", async () => {
    renderEditor();
    await openedOnForm();

    expect(screen.queryByText(/reconciled from a repository by/)).toBeNull();
    expect(screen.getByRole("button", { name: saveButton })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Export" })).toBeNull();
  });

  it("warns that autoDeploy does not compose with a git-reconciled Environment", async () => {
    renderEditor({ gitops: OWNED, environment: TRACKING_ENVIRONMENT });

    // Not `openedOnForm`: an environment document carrying `autoDeploy` is
    // outside the shape the form rebuilds, so this screen opens read-only —
    // which is exactly the reader who most needs to be told the field does not
    // compose with what git is doing to the same document.
    expect(
      await screen.findByText(/does not compose with a git-reconciled Environment/),
    ).toBeTruthy();
    // The remedy, not the reasoning: the ADR is a code comment now.
    expect(
      screen.getByText(/Track the image in the repository instead/),
    ).toBeTruthy();
  });

  it("does not warn about autoDeploy when no document declares it", async () => {
    renderEditor({ gitops: OWNED });
    await openedOnForm();

    expect(
      screen.queryByText(/does not compose with a git-reconciled Environment/),
    ).toBeNull();
  });

  it("shows the diff before it offers the proposal", async () => {
    await edited({ gitops: OWNED });

    expect(screen.queryByRole("heading", { name: proposeHeading })).toBeNull();
    expect(screen.getByText(/Check the spec first/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await screen.findByRole("heading", { name: proposeHeading });
  });

  it("proposes the edited documents at the paths the reader named", async () => {
    const { recorder } = await edited({ gitops: OWNED });
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await screen.findByRole("heading", { name: proposeHeading });

    // The connection list is an async read, so the option has to exist before
    // a change event can select it.
    await screen.findByRole("option", { name: /acme-github/ });
    fireEvent.change(screen.getByLabelText("Connection"), {
      target: { value: "acme-github" },
    });
    fireEvent.change(screen.getByLabelText("Repository"), {
      target: { value: "acme/gitops" },
    });
    fireEvent.change(screen.getByLabelText("Base branch"), {
      target: { value: "main" },
    });
    fireEvent.change(
      screen.getByLabelText("repository path for the project document"),
      { target: { value: "clusters/prod/hello.yaml" } },
    );

    const propose = screen.getByRole("button", { name: proposeHeading });
    // The whole-file disclosure is a gate, not decoration: kelson holds the
    // document and not the file, and the reviewer is the last person who can
    // catch a path that also held something else.
    expect((propose as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("checkbox", { name: /replaced whole/ }));
    fireEvent.click(propose);

    await screen.findByRole("link", { name: "Open the pull request" });
    expect(recorder.proposals).toHaveLength(1);
    const sent = recorder.proposals[0]!;
    expect(sent.project).toBe("hello");
    expect(sent.connection).toBe("acme-github");
    expect(sent.repository).toBe("acme/gitops");
    expect(sent.baseBranch).toBe("main");
    // Both documents ride because git owns both; the unit test covers a
    // document kelson writes itself staying out.
    expect(sent.files.map((f) => f.path).sort()).toEqual([
      "clusters/prod/hello.yaml",
      "hello-development.yaml",
    ]);
    const project = sent.files.find((f) => f.path === "clusters/prod/hello.yaml")!;
    expect(DECODER.decode(project.content)).toContain("ghcr.io/acme/hello:2.0.0");
    // Nothing was stored: a managed project's edit never reaches PutSpec except
    // as the dry run that produced the diff.
    expect(recorder.writes.every((w) => w.dryRun === DryRun.RENDER)).toBe(true);
  });

  it("shows the findings when the server would not propose the documents", async () => {
    await edited({
      gitops: OWNED,
      proposalFindings: [
        create(ErrorSchema, {
          code: "schema/invalid-name",
          resource: "Project/hello",
          message: "component names must be DNS labels",
          remediation: "rename it",
        }),
      ],
    });
    fireEvent.click(screen.getByRole("button", { name: "Check and preview the diff" }));
    await screen.findByRole("heading", { name: proposeHeading });

    // The connection list is an async read, so the option has to exist before
    // a change event can select it.
    await screen.findByRole("option", { name: /acme-github/ });
    fireEvent.change(screen.getByLabelText("Connection"), {
      target: { value: "acme-github" },
    });
    fireEvent.change(screen.getByLabelText("Repository"), {
      target: { value: "acme/gitops" },
    });
    fireEvent.click(screen.getByRole("checkbox", { name: /replaced whole/ }));
    fireEvent.click(screen.getByRole("button", { name: proposeHeading }));

    expect(
      await screen.findByText("The server would not propose these documents"),
    ).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Open the pull request" })).toBeNull();
  });
});
