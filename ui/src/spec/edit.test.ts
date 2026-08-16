import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import {
  bindingEnv,
  buildDocuments,
  EMPTY_FORM,
  plainEnv,
  secretEnv,
} from "./documents";
import {
  appendComponent,
  buildProjectDocument,
  componentDraftProblems,
  componentSnippet,
  editFieldForError,
  emptyComponentDraft,
  emptyPreviews,
  isRebuildable,
  mapEditErrors,
  parseProjectDocument,
  readSpec,
  writeSpec,
  type ComponentDraft,
  type ProjectEdit,
  type SpecTextSet,
} from "./edit";

/**
 * The create form's own output is the input to the editor.
 *
 * These are the #63 fixture bytes — the same pair pinned in
 * internal/api/uispec_test.go — and the whole edit strategy rests on a document
 * the UI wrote coming back through the parser and the builder unchanged.
 */
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

const MINIMAL: SpecTextSet = {
  project: MINIMAL_PROJECT,
  environments: { development: MINIMAL_ENVIRONMENT },
};

/**
 * The document the edit form writes once it has been used: project-level and
 * per-component env, an image override, autoscaling bounds, a second
 * component.
 *
 * These exact bytes are the second half of the cross-side fixture convention
 * #63 established — internal/api/uispec_test.go holds them byte-identically and
 * asserts the real model validates and renders them. Nothing in TypeScript can
 * prove that a document this builder writes is a document kelson accepts.
 *
 * It declares no domains for the same reason the create fixture does not: the
 * Go test renders against the zero profile, which has no Gateway API, and a
 * declared domain is render/gateway-api-missing there. Domains are covered by
 * the round trip below, which needs no model.
 */
const EDITED_PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  env:
    LOG_LEVEL: info
    PORT: "3000"

  components:
    - name: web
      port: 8080
      health: /healthz
      replicas: { min: 2, max: 10 }
      env:
        ROLE: web

    - name: worker
      image: ghcr.io/acme/hello-worker:1.4.2
      replicas: { min: 1 }
`;

const EDITED_ENVIRONMENT = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development

spec:
  project: hello
  namespace: hello-sandbox
`;

/** The same document with the one field the Go fixture cannot carry. */
const RICH_PROJECT = EDITED_PROJECT.replace(
  "      health: /healthz\n",
  "      health: /healthz\n      domains:\n        - hello.dev.acme.run\n",
);

const RICH_ENVIRONMENT = EDITED_ENVIRONMENT;

/**
 * The Environment the previews form writes (ADR-0017).
 *
 * These bytes are uiPreviewsEnvironmentDoc in internal/api/uispec_test.go under
 * the same cross-side convention as the fixtures above: this half asserts the
 * form reads them and writes them back unchanged, and the Go half asserts the
 * model validates them and the renderer turns them into a
 * ResourceSetInputProvider and a ResourceSet. Neither side can assert the
 * other's.
 *
 * The label lists are flow sequences because that is the styling ADR-0017 and
 * docs/model.md write them in, so a block pasted from the documentation is a
 * block this form can still edit.
 */
const PREVIEWS_ENVIRONMENT = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging

spec:
  project: hello
  namespace: hello-staging
  previews:
    provider: github
    repo: https://github.com/acme/hello
    secretRef: github-auth
    interval: 10m
    filter:
      labels: [deploy/preview]
      includeBranch: "^feat/.*"
      excludeBranch: "^wip/.*"
      limit: 5
    skip:
      labels: [deploy/preview-pause, "!ci/passed"]
    artifacts:
      repository: oci://ghcr.io/acme/hello-previews
      secretRef: ghcr-auth
`;

describe("previews on an Environment (ADR-0017)", () => {
  const withPreviews: SpecTextSet = {
    project: MINIMAL_PROJECT,
    environments: { staging: PREVIEWS_ENVIRONMENT },
  };

  it("reads the whole block and writes it back byte-identically", () => {
    const edit = readSpec(withPreviews);
    expect(edit).toBeDefined();

    const env = edit?.environments[0];
    expect(env?.previews).toEqual({
      enabled: true,
      provider: "github",
      repo: "https://github.com/acme/hello",
      secretRef: "github-auth",
      interval: "10m",
      filterLabels: "deploy/preview",
      includeBranch: "^feat/.*",
      excludeBranch: "^wip/.*",
      limit: "5",
      skipLabels: "deploy/preview-pause, !ci/passed",
      artifactsRepository: "oci://ghcr.io/acme/hello-previews",
      artifactsSecretRef: "ghcr-auth",
    });

    expect(writeSpec(edit!)).toEqual(withPreviews);
    expect(isRebuildable(withPreviews)).toBe(true);
  });

  it("writes nothing at all when previews are off", () => {
    const edit = readSpec(withPreviews)!;
    const off = {
      ...edit,
      environments: edit.environments.map((e) => ({
        ...e,
        previews: { ...e.previews, enabled: false },
      })),
    };
    const doc = writeSpec(off).environments.staging ?? "";
    expect(doc).not.toContain("previews:");
    // Everything else is untouched: turning previews off is not a rewrite of
    // the environment.
    expect(doc).toContain("  namespace: hello-staging");
  });

  it("keeps a required field even when it is empty, so the server names it", () => {
    const edit = readSpec(withPreviews)!;
    const blank = {
      ...edit,
      environments: edit.environments.map((e) => ({
        ...e,
        previews: { ...e.previews, repo: "", filterLabels: "", skipLabels: "" },
      })),
    };
    const doc = writeSpec(blank).environments.staging ?? "";
    // An empty required field is written as an empty scalar rather than
    // omitted: schema/missing-required then lands on `$.spec.previews.repo`,
    // which the form has an input for, instead of on the block.
    expect(doc).toContain('    repo: ""');
    // Optional lists disappear entirely rather than becoming empty sequences.
    expect(doc).not.toContain("labels:");
    expect(doc).not.toContain("    skip:");
  });

  it("refuses a label spacing it could not reproduce", () => {
    // `[a,b]` would have to be guessed at, so the document goes to the YAML tab
    // whole rather than reaching the form as one label called "a,b".
    const ambiguous = PREVIEWS_ENVIRONMENT.replace(
      "labels: [deploy/preview]",
      "labels: [deploy/preview,deploy/other]",
    );
    expect(
      readSpec({ project: MINIMAL_PROJECT, environments: { staging: ambiguous } }),
    ).toBeUndefined();
  });

  it("shows a block in another styling, then declines to rewrite it", () => {
    // A block sequence parses into the same edit state — the reader sees their
    // own labels — and the rebuild emits the flow styling, so the bytes differ
    // and the byte guard sends the document to the YAML tab. Nothing is lost
    // and nothing is silently reformatted.
    const blockStyle = PREVIEWS_ENVIRONMENT.replace(
      "      labels: [deploy/preview]\n",
      "      labels:\n        - deploy/preview\n",
    );
    const text = { project: MINIMAL_PROJECT, environments: { staging: blockStyle } };
    expect(readSpec(text)?.environments[0]?.previews.filterLabels).toBe(
      "deploy/preview",
    );
    expect(isRebuildable(text)).toBe(false);
  });

  it("maps the server's previews findings onto the inputs that hold them", () => {
    const at = (field: string) =>
      editFieldForError(
        create(ErrorSchema, {
          code: "schema/missing-required",
          resource: "Environment/staging",
          field,
        }),
      );
    expect(at("$.spec.previews.repo")).toBe("environment.staging.previews.repo");
    expect(at("$.spec.previews.artifacts.repository")).toBe(
      "environment.staging.previews.artifactsRepository",
    );
    expect(at("$.spec.previews.filter.limit")).toBe(
      "environment.staging.previews.limit",
    );
    // An indexed label finding lands on the one input that holds the list.
    expect(at("$.spec.previews.skip.labels[1]")).toBe(
      "environment.staging.previews.skipLabels",
    );
    // The retired `delivery:` block has no input to land on: the model's
    // answer is "delete the whole block", so the finding goes to the general
    // panel with that remediation instead of onto a field (ADR-0028, #234).
    expect(at("$.spec.delivery")).toBeUndefined();
  });
});

describe("round trip", () => {
  it("parses a document the create form built and rebuilds it byte-identically", () => {
    const built = buildDocuments({
      ...EMPTY_FORM,
      project: "hello",
      image: "ghcr.io/acme/hello:1.4.2",
      port: "8080",
    });
    // The fixture is the create form's own output, not a hand-copied lookalike.
    expect(built.project).toBe(MINIMAL_PROJECT);
    expect(built.environment).toBe(MINIMAL_ENVIRONMENT);

    const edit = readSpec(MINIMAL);
    expect(edit).toBeDefined();
    expect(edit?.project.name).toBe("hello");
    expect(edit?.project.image).toBe("ghcr.io/acme/hello:1.4.2");
    expect(edit?.project.components).toHaveLength(1);
    expect(edit?.project.components[0]?.port).toBe("8080");
    expect(edit?.environments).toEqual([
      {
        name: "development",
        project: "hello",
        namespace: "",
        previews: emptyPreviews(),
      },
    ]);

    expect(writeSpec(edit!)).toEqual(MINIMAL);
    expect(isRebuildable(MINIMAL)).toBe(true);
  });

  it("round-trips every shape the edit form can write", () => {
    const rich: SpecTextSet = {
      project: RICH_PROJECT,
      environments: { development: RICH_ENVIRONMENT },
    };
    const edit = readSpec(rich);
    expect(edit).toBeDefined();

    const web = edit?.project.components[0];
    expect(web?.name).toBe("web");
    expect(web?.health).toBe("/healthz");
    expect(web?.domains).toEqual(["hello.dev.acme.run"]);
    expect(web?.replicasMin).toBe("2");
    expect(web?.replicasMax).toBe("10");
    expect(web?.env).toEqual([{ key: "ROLE", value: plainEnv("web") }]);

    const worker = edit?.project.components[1];
    expect(worker?.name).toBe("worker");
    expect(worker?.image).toBe("ghcr.io/acme/hello-worker:1.4.2");
    expect(worker?.replicasMin).toBe("1");
    expect(worker?.replicasMax).toBe("");

    // A quoted env value survives as its string: `PORT: "3000"` is the value
    // model.EnvValue accepts, and rewriting it as an integer would break it.
    expect(edit?.project.env).toEqual([
      { key: "LOG_LEVEL", value: plainEnv("info") },
      { key: "PORT", value: plainEnv("3000") },
    ]);

    expect(writeSpec(edit!)).toEqual(rich);
    expect(isRebuildable(rich)).toBe(true);
  });

  it("round-trips the document the Go side validates, byte for byte", () => {
    const edited: SpecTextSet = {
      project: EDITED_PROJECT,
      environments: { development: EDITED_ENVIRONMENT },
    };
    const edit = readSpec(edited);
    expect(edit).toBeDefined();
    expect(writeSpec(edit!)).toEqual(edited);
    expect(isRebuildable(edited)).toBe(true);
  });

  it("preserves the order of environment variables rather than sorting them", () => {
    const edit = readSpec({
      project: RICH_PROJECT.replace(
        "    LOG_LEVEL: info\n    PORT: \"3000\"",
        "    ZEBRA: last\n    ALPHA: first",
      ),
      environments: { development: RICH_ENVIRONMENT },
    });
    expect(edit?.project.env.map((e) => e.key)).toEqual(["ZEBRA", "ALPHA"]);
  });
});

describe("hand-edited detection", () => {
  const cases: { name: string; project: string }[] = [
    {
      name: "a comment",
      project: MINIMAL_PROJECT.replace(
        "spec:",
        "# the thing that serves traffic\nspec:",
      ),
    },
    {
      name: "a different key order",
      project: `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  components:
    - name: web
      port: 8080

  image: ghcr.io/acme/hello:1.4.2
`,
    },
    {
      name: "a key this module does not model",
      project: MINIMAL_PROJECT.replace(
        "  components:",
        "  labels:\n    team: platform\n\n  components:",
      ),
    },
    {
      // ADR-0014 widened the kind set; the parser deliberately did not follow.
      // A data component is a key the parser captures nowhere, so the rebuild
      // drops it and the byte guard sends the whole document to the YAML tab.
      name: "a data component",
      project: MINIMAL_PROJECT.replace(
        "    - name: web\n",
        "    - name: db\n      kind: postgres\n      preset: small\n\n    - name: web\n",
      ),
    },
    {
      name: "an agent component's tool policy",
      project: MINIMAL_PROJECT.replace(
        "      port: 8080\n",
        "      port: 8080\n\n    - name: triage\n      kind: agent\n      tools: [search]\n",
      ),
    },
    {
      name: "a flow sequence",
      project: MINIMAL_PROJECT.replace(
        "      port: 8080\n",
        "      port: 8080\n      domains: [hello.acme.run]\n",
      ),
    },
    {
      name: "four-space indentation",
      project: MINIMAL_PROJECT.replace("  name: hello", "    name: hello"),
    },
  ];

  for (const { name, project } of cases) {
    it(`refuses the form for ${name}`, () => {
      expect(isRebuildable({ ...MINIMAL, project })).toBe(false);
    });
  }

  it("refuses when an environment document is hand-edited, not only the project", () => {
    expect(
      isRebuildable({
        ...MINIMAL,
        environments: {
          development: `# staging is a copy of this\n${MINIMAL_ENVIRONMENT}`,
        },
      }),
    ).toBe(false);
  });

  it("refuses when the document's own name disagrees with the store's key", () => {
    // The store keys an environment document by name; a document whose
    // metadata.name says otherwise cannot be rewritten without moving it.
    expect(
      isRebuildable({ ...MINIMAL, environments: { staging: MINIMAL_ENVIRONMENT } }),
    ).toBe(false);
  });

  it("still fills the form from a commented document, read-only", () => {
    // A comment is skipped, not refused: the reader gets to see their own
    // configuration in the form. What they do not get is to edit it there,
    // because the rebuild would not carry the comment — and the byte guard,
    // not a special case for comments, is what says so.
    const handEdited = MINIMAL_PROJECT.replace("spec:", "# hand-written\nspec:");
    expect(parseProjectDocument(handEdited)?.image).toBe("ghcr.io/acme/hello:1.4.2");
    expect(isRebuildable({ ...MINIMAL, project: handEdited })).toBe(false);
  });

  it("reads a flow sequence and then declines to rewrite it", () => {
    // `domains: [a]` is a list this reader can hold — the previews block writes
    // its label lists in exactly that styling (ADR-0017) — but the component
    // builder writes domains as a block sequence, so the rebuild differs and the
    // byte guard says read-only. The reader sees their own domains either way.
    const flow = MINIMAL_PROJECT.replace(
      "      port: 8080\n",
      "      port: 8080\n      domains: [hello.acme.run]\n",
    );
    expect(parseProjectDocument(flow)?.components[0]?.domains).toEqual([
      "hello.acme.run",
    ]);
    expect(isRebuildable({ ...MINIMAL, project: flow })).toBe(false);
  });

  it("refuses outright what it cannot represent at all", () => {
    // A spacing this reader cannot reproduce is not a formatting difference the
    // byte guard can catch later: it would have to guess where one element ends
    // and the next begins. So there is no form to be read-only, and the YAML tab
    // is the whole answer.
    expect(
      parseProjectDocument(
        MINIMAL_PROJECT.replace(
          "      port: 8080\n",
          "      port: 8080\n      domains: [a.acme.run,b.acme.run]\n",
        ),
      ),
    ).toBeUndefined();
  });
});

describe("environment variables through the form", () => {
  const base = (): ProjectEdit => {
    const edit = readSpec(MINIMAL);
    if (edit === undefined) throw new Error("the fixture must parse");
    return edit.project;
  };

  it("adds a project-level variable under spec.env, shared by every component", () => {
    const project = base();
    project.env = [{ key: "LOG_LEVEL", value: plainEnv("info") }];

    expect(buildProjectDocument(project)).toBe(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  env:
    LOG_LEVEL: info

  components:
    - name: web
      port: 8080
`);
  });

  it("adds a per-component variable under the component, not the project", () => {
    const project = base();
    const web = project.components[0];
    if (web === undefined) throw new Error("the fixture must have a component");
    web.env = [{ key: "ROLE", value: plainEnv("web") }];

    const doc = buildProjectDocument(project);
    expect(doc).toContain("    - name: web\n      port: 8080\n      env:\n        ROLE: web\n");
    // Nothing landed at project level: the two scopes are different rules (P1).
    expect(doc).not.toContain("\n  env:\n");
  });

  it("quotes a value YAML would resolve as something other than a string", () => {
    const project = base();
    project.env = [
      { key: "PORT", value: plainEnv("3000") },
      { key: "DEBUG", value: plainEnv("on") },
    ];
    const doc = buildProjectDocument(project);
    expect(doc).toContain('    PORT: "3000"\n');
    expect(doc).toContain('    DEBUG: "on"\n');
    // …and it comes back as the string that was typed.
    expect(parseProjectDocument(doc)?.env).toEqual(project.env);
  });

  it("changes and removes variables in place", () => {
    const project = base();
    project.env = [
      { key: "LOG_LEVEL", value: plainEnv("info") },
      { key: "REGION", value: plainEnv("eu") },
    ];
    const changed = parseProjectDocument(buildProjectDocument(project));
    expect(changed?.env).toEqual(project.env);

    project.env = [{ key: "LOG_LEVEL", value: plainEnv("debug") }];
    const after = parseProjectDocument(buildProjectDocument(project));
    expect(after?.env).toEqual([{ key: "LOG_LEVEL", value: plainEnv("debug") }]);

    project.env = [];
    expect(buildProjectDocument(project)).toBe(MINIMAL_PROJECT);
  });
});

/**
 * The two mapping forms an env value can take (ADR-0018), and the exact line
 * between "the form may write this back" and "the YAML tab owns this".
 *
 * The fixture is the styling ADR-0018 and docs/model.md show an author writing,
 * which is also what `kelson secret set` prints and what this builder emits —
 * one variable, one line.
 */
const REFERENCED_PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  env:
    LOG_LEVEL: info
    DATABASE_URL: { secret: checkout-db, key: url }

  components:
    - name: web
      port: 8080
      env:
        CACHE_URL: { from: { service: cache, key: uri } }
`;

const REFERENCED: SpecTextSet = {
  project: REFERENCED_PROJECT,
  environments: { development: MINIMAL_ENVIRONMENT },
};

describe("secret references and bindings", () => {
  it("reads both mapping forms as themselves, not as strings", () => {
    const project = parseProjectDocument(REFERENCED_PROJECT);

    expect(project?.env).toEqual([
      { key: "LOG_LEVEL", value: plainEnv("info") },
      { key: "DATABASE_URL", value: secretEnv("checkout-db", "url") },
    ]);
    expect(project?.components[0]?.env).toEqual([
      { key: "CACHE_URL", value: bindingEnv("cache", "uri") },
    ]);
  });

  it("rewrites a reference-carrying document byte for byte", () => {
    const edit = readSpec(REFERENCED);
    expect(edit).toBeDefined();
    expect(writeSpec(edit!)).toEqual(REFERENCED);
    expect(isRebuildable(REFERENCED)).toBe(true);
  });

  it("edits a plain value into a reference and back", () => {
    const edit = readSpec(REFERENCED);
    if (edit === undefined) throw new Error("the fixture must parse");
    edit.project.env = [
      { key: "LOG_LEVEL", value: secretEnv("app-config", "log-level") },
      { key: "DATABASE_URL", value: plainEnv("postgres://localhost/dev") },
    ];

    const written = writeSpec(edit).project;
    expect(written).toContain("    LOG_LEVEL: { secret: app-config, key: log-level }\n");
    expect(written).toContain("    DATABASE_URL: postgres://localhost/dev\n");
    // And the rewrite is readable by the same parser, which is what makes the
    // guard a proof rather than a hope.
    expect(parseProjectDocument(written)?.env).toEqual(edit.project.env);
  });

  /**
   * The stylings that are shown but not written back. Each parses into the same
   * edit state as the canonical form — so the reader sees their reference as a
   * reference — and each rebuilds into the canonical flow line, so the bytes
   * differ and the byte guard sends the document to the YAML tab whole.
   */
  const readOnlyStylings: { name: string; env: string }[] = [
    {
      name: "a block-styled secret reference",
      env: "    DATABASE_URL:\n      secret: checkout-db\n      key: url\n",
    },
    {
      name: "a block-styled binding",
      env: "    DATABASE_URL:\n      from:\n        service: db\n        key: uri\n",
    },
    {
      name: "a binding whose `from` is block and whose inner mapping is flow",
      env: "    DATABASE_URL:\n      from: { service: db, key: uri }\n",
    },
    {
      name: "a flow mapping with the keys the other way round",
      env: "    DATABASE_URL: { key: url, secret: checkout-db }\n",
    },
  ];

  for (const { name, env } of readOnlyStylings) {
    it(`shows ${name} in the form, and refuses to rewrite it`, () => {
      const project = MINIMAL_PROJECT.replace(
        "spec:\n",
        `spec:\n\n  env:\n${env}`,
      );

      // Displayed faithfully…
      const parsed = parseProjectDocument(project);
      expect(parsed?.env[0]?.value.kind, name).not.toBe("plain");
      // …and read-only, because the rebuild would restyle the file.
      expect(isRebuildable({ ...MINIMAL, project }), name).toBe(false);
    });
  }

  /**
   * The stylings with no form at all. A value this reader cannot decode must
   * never arrive in the form as something else — a reference shown as the
   * string "{ secret: … }" would be a lie about what the spec says — so the
   * parser gives up and the whole document goes to the YAML tab.
   */
  const refused: { name: string; env: string }[] = [
    {
      name: "a flow mapping with different spacing",
      env: "    DATABASE_URL: {secret: checkout-db, key: url}\n",
    },
    {
      name: "a quoted scalar inside the mapping",
      env: '    DATABASE_URL: { secret: "checkout-db", key: url }\n',
    },
    {
      name: "a mapping that is neither reference form",
      env: "    DATABASE_URL: { vault: checkout-db, key: url }\n",
    },
    {
      name: "a `from` binding missing half of itself",
      env: "    DATABASE_URL: { from: { service: db } }\n",
    },
  ];

  for (const { name, env } of refused) {
    it(`refuses the form outright for ${name}`, () => {
      const project = MINIMAL_PROJECT.replace(
        "spec:\n",
        `spec:\n\n  env:\n${env}`,
      );

      expect(parseProjectDocument(project), name).toBeUndefined();
      expect(isRebuildable({ ...MINIMAL, project }), name).toBe(false);
    });
  }
});

/**
 * Adding a component to a project that already exists (#214).
 *
 * The property under test is not "the document contains the new component" — a
 * rewrite would satisfy that. It is that the *stored bytes are untouched*: the
 * rebuilt document is the one that was stored, followed by the new entry and
 * nothing else. That is what makes an append safe on a file the user owns
 * (ADR-0013 §1), and it is asserted as a string equality rather than inferred
 * from a round trip.
 */
describe("appending a component", () => {
  const draft = (patch: Partial<ComponentDraft> = {}): ComponentDraft => ({
    ...emptyComponentDraft(),
    ...patch,
  });

  it("leaves the stored bytes alone and adds one entry after them", () => {
    const result = appendComponent(
      MINIMAL,
      draft({ name: "worker", kind: "worker" }),
    );
    expect(result.ok).toBe(true);
    if (!result.ok) return;

    // The whole contract, spelled out: prefix, separator, entry.
    expect(result.text.project).toBe(`${MINIMAL_PROJECT}\n    - name: worker\n`);
    expect(result.text.project.startsWith(MINIMAL_PROJECT)).toBe(true);
    expect(result.text.environments).toEqual(MINIMAL.environments);
    // …and the result is a document the form can go on editing, which is what
    // keeps a project editable after it grows a second component.
    expect(isRebuildable(result.text)).toBe(true);
  });

  it("writes the shape each kind is made of, and never a kind the shape derives", () => {
    const shapes: { draft: ComponentDraft; entry: string }[] = [
      {
        draft: draft({ name: "api", kind: "service", port: "9090" }),
        entry: "    - name: api\n      port: 9090\n",
      },
      {
        draft: draft({ name: "nightly", kind: "cron", schedule: "0 3 * * *" }),
        // Quoted, because `0 3 * * *` is not a plain scalar: the same rule that
        // keeps `PORT: "3000"` a string (documents.ts: yamlScalar).
        entry: '    - name: nightly\n      schedule: "0 3 * * *"\n',
      },
      { draft: draft({ name: "worker", kind: "worker" }), entry: "    - name: worker\n" },
      {
        draft: draft({ name: "db", kind: "postgres" }),
        entry: "    - name: db\n      kind: postgres\n",
      },
      {
        draft: draft({ name: "cache", kind: "valkey" }),
        entry: "    - name: cache\n      kind: valkey\n",
      },
    ];

    for (const { draft: d, entry } of shapes) {
      const result = appendComponent(MINIMAL, d);
      expect(result.ok, d.kind).toBe(true);
      if (!result.ok) continue;
      expect(result.text.project, d.kind).toBe(`${MINIMAL_PROJECT}\n${entry}`);
      // The snippet the handoff hands over is the same block, byte for byte.
      expect(componentSnippet(d), d.kind).toBe(entry);
      // A data component is as editable as anything else here — the parser and
      // the builder learned the same one key, so the guard still holds.
      expect(isRebuildable(result.text), d.kind).toBe(true);
    }
  });

  it("writes no image at all for a component that inherits the project's (rule P3)", () => {
    const inherited = appendComponent(MINIMAL, draft({ name: "worker", kind: "worker" }));
    expect(inherited.ok && inherited.added).toBe("    - name: worker\n");

    const own = appendComponent(
      MINIMAL,
      draft({
        name: "worker",
        kind: "worker",
        ownImage: true,
        image: "ghcr.io/acme/hello-worker:1.4.2",
      }),
    );
    expect(own.ok && own.added).toBe(
      "    - name: worker\n      image: ghcr.io/acme/hello-worker:1.4.2\n",
    );
  });

  it("drops the fields the chosen kind does not use", () => {
    // A port typed before the reader chose "worker" is not a port the document
    // should carry: the kind is the question and the shape follows it.
    const result = appendComponent(
      MINIMAL,
      draft({ name: "worker", kind: "worker", port: "8080", schedule: "0 3 * * *" }),
    );
    expect(result.ok && result.text.project).toBe(`${MINIMAL_PROJECT}\n    - name: worker\n`);
  });

  it("hands over the block instead of rewriting a document it cannot rebuild", () => {
    const handWritten = MINIMAL_PROJECT.replace(
      "spec:",
      "# the thing that serves traffic\nspec:",
    );
    const text = { ...MINIMAL, project: handWritten };
    expect(isRebuildable(text)).toBe(false);

    const result = appendComponent(text, draft({ name: "worker", kind: "worker" }));
    expect(result.ok).toBe(false);
    // The comment is still there, because nothing was rewritten…
    expect(text.project).toContain("# the thing that serves traffic");
    // …and what the reader gets instead is the exact entry to paste.
    expect(result.added).toBe("    - name: worker\n");
  });

  it("refuses a document whose environment half is hand-edited, not only the project", () => {
    const result = appendComponent(
      {
        ...MINIMAL,
        environments: { development: `# staging copies this\n${MINIMAL_ENVIRONMENT}` },
      },
      draft({ name: "worker", kind: "worker" }),
    );
    expect(result.ok).toBe(false);
  });
});

describe("what the browser refuses to send about a new component", () => {
  const draft = (patch: Partial<ComponentDraft> = {}): ComponentDraft => ({
    ...emptyComponentDraft(),
    ...patch,
  });
  const messages = (d: ComponentDraft, existing: string[] = ["web", "worker"]) =>
    componentDraftProblems(d, existing).map((p) => `${p.field}: ${p.message}`);

  it("holds the name to what makes it a component name", () => {
    expect(messages(draft({ name: "", kind: "worker" }))[0]).toContain(
      "a component name is required",
    );
    expect(messages(draft({ name: "Web Worker", kind: "worker" }))[0]).toContain(
      "not a DNS-1123 label",
    );
    // The model's uniqueness rule (internal/model/validate.go), answered while
    // someone types rather than after a round trip.
    expect(messages(draft({ name: "web", kind: "worker" }))[0]).toContain(
      'already has a component called "web"',
    );
    expect(messages(draft({ name: "web-2", kind: "worker" }))).toEqual([]);
  });

  it("requires the one field the chosen kind is made of", () => {
    expect(messages(draft({ name: "api", kind: "service" }))[0]).toContain(
      "a service is a component with a port",
    );
    expect(messages(draft({ name: "api", kind: "service", port: "http" }))[0]).toContain(
      "not a whole number",
    );
    expect(messages(draft({ name: "nightly", kind: "cron" }))[0]).toContain(
      "a cron job is a component with a schedule",
    );
    // A data component is a name and a kind; there is nothing else to require.
    expect(messages(draft({ name: "db", kind: "postgres" }))).toEqual([]);
  });

  it("asks for an image only from a component that said it has its own", () => {
    expect(messages(draft({ name: "mailer", kind: "worker", ownImage: true }))[0]).toContain(
      "an image reference is required",
    );
    expect(messages(draft({ name: "mailer", kind: "worker" }))).toEqual([]);
  });
});

describe("editFieldForError", () => {
  const wire = (resource: string, field: string, code = "schema/invalid-format") =>
    create(ErrorSchema, { resource, field, code });

  it("reaches components beyond the first", () => {
    expect(editFieldForError(wire("Project/hello", "$.spec.components[1].port"))).toBe(
      "component.1.port",
    );
    expect(
      editFieldForError(wire("Project/hello", "$.spec.components[1].replicas.max")),
    ).toBe("component.1.replicas");
    expect(
      editFieldForError(wire("Project/hello", "$.spec.components[2].domains[0]")),
    ).toBe("component.2.domains");
    expect(editFieldForError(wire("Project/hello", "$.spec.components[0].health"))).toBe(
      "component.0.health",
    );
  });

  it("keeps project env and per-component env apart", () => {
    expect(
      editFieldForError(wire("Project/hello", "$.spec.env.DB_PASSWORD", "secret/literal")),
    ).toBe("project.env.DB_PASSWORD");
    expect(
      editFieldForError(
        wire("Project/hello", "$.spec.components[1].env.DB_PASSWORD", "secret/literal"),
      ),
    ).toBe("component.1.env.DB_PASSWORD");
  });

  it("names the environment a namespace error belongs to", () => {
    expect(editFieldForError(wire("Environment/staging", "$.spec.namespace"))).toBe(
      "environment.staging.namespace",
    );
  });

  it("sends a path no input owns to the general panel", () => {
    const mapped = mapEditErrors([
      wire("Project/hello", "$.spec.components[0].name"),
      wire("Project/hello", "$.spec.overlays[0].patch"),
      wire("Project/hello", "$.spec.components[1].port"),
    ]);
    expect(mapped.general).toHaveLength(2);
    expect([...mapped.byField.keys()]).toEqual(["component.1.port"]);
  });

  it("points a missing image source at the component that has none", () => {
    expect(
      editFieldForError(
        wire("Project/hello", "$.spec.components[1]", "semantic/no-image-source"),
      ),
    ).toBe("component.1.image");
  });
});
