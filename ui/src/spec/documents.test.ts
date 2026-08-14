import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import {
  bindingEnv,
  buildDocuments,
  DEFAULT_BUILD_STRATEGY,
  EMPTY_FORM,
  fieldForError,
  formProblems,
  IMAGE_UNRESOLVED,
  plainEnv,
  secretEnv,
  secretReference,
  splitFindings,
  workloadKind,
  yamlScalar,
  type NewAppForm,
} from "./documents";

function form(overrides: Partial<NewAppForm>): NewAppForm {
  return { ...EMPTY_FORM, ...overrides };
}

/**
 * The three-field case, byte for byte.
 *
 * These exact bytes are also a fixture on the Go side, in
 * internal/api/uispec_test.go, where the real model validates and renders
 * them. The two copies are kept in step by hand and by comment: a change to
 * the builder that is not mirrored there fails `go test ./internal/api`.
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

/**
 * The git-source case, byte for byte, and also a Go-side fixture
 * (uiSourceProjectDoc in internal/api/uispec_test.go). Same contract as the
 * pair above: only Go can assert that the model accepts these bytes, and only
 * this file can assert that the builder produces them.
 *
 * `strategy: dockerfile` is what an untouched form chooses
 * (DEFAULT_BUILD_STRATEGY), which is why these are still the bytes the Go
 * fixture holds.
 */
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

const THREE_FIELDS = form({
  project: "hello",
  image: "ghcr.io/acme/hello:1.4.2",
  port: "8080",
});

const FROM_GIT = form({
  project: "hello",
  sourceMode: "git",
  git: "https://github.com/acme/hello",
  ref: "main",
  port: "8080",
});

describe("buildDocuments", () => {
  it("writes exactly the three-field documents, and nothing else", () => {
    const built = buildDocuments(THREE_FIELDS);

    expect(built.project).toBe(MINIMAL_PROJECT);
    expect(built.environment).toBe(MINIMAL_ENVIRONMENT);
    expect(built.projectName).toBe("hello");
    expect(built.environmentName).toBe("development");
  });

  it("makes a component with no port a worker", () => {
    const built = buildDocuments(form({ project: "mailroom", image: "acme/mailroom:2" }));

    expect(built.project).toBe(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: mailroom

spec:
  image: acme/mailroom:2

  components:
    - name: worker
`);
  });

  it("emits an optional key only when it is set", () => {
    // One field at a time, against the three-field baseline: each optional
    // input adds exactly its own line and leaves the rest of the document
    // alone. A builder that emitted empty keys would fail every row here.
    const cases: { name: string; form: NewAppForm; added: string[] }[] = [
      {
        name: "health",
        form: form({ ...THREE_FIELDS, health: "/healthz" }),
        added: ["      health: /healthz"],
      },
      {
        name: "domains",
        form: form({ ...THREE_FIELDS, domains: ["hello.dev.acme.run", "hello.acme.run"] }),
        added: [
          "      domains:",
          "        - hello.dev.acme.run",
          "        - hello.acme.run",
        ],
      },
      {
        name: "replicas",
        form: form({ ...THREE_FIELDS, replicas: "3" }),
        added: ["      replicas: { min: 3 }"],
      },
      {
        name: "env",
        form: form({
          ...THREE_FIELDS,
          env: [{ key: "LOG_LEVEL", value: plainEnv("info") }],
        }),
        added: ["  env:", "    LOG_LEVEL: info"],
      },
    ];

    for (const { name, form: input, added } of cases) {
      const lines = buildDocuments(input).project.split("\n");
      const baseline = MINIMAL_PROJECT.split("\n");
      const extra = lines.filter((line, i) => line !== baseline[i]);
      expect(extra.length, name).toBeGreaterThan(0);
      for (const line of added) {
        expect(lines, name).toContain(line);
      }
      // The environment document is untouched by any project-side option.
      expect(buildDocuments(input).environment, name).toBe(MINIMAL_ENVIRONMENT);
    }

    // Blank optional inputs add nothing at all.
    expect(
      buildDocuments(
        form({
          ...THREE_FIELDS,
          health: "  ",
          domains: ["", "   "],
          replicas: "",
          env: [{ key: "", value: plainEnv("ignored") }],
          namespace: "",
        }),
      ),
    ).toEqual(buildDocuments(THREE_FIELDS));
  });

  it("puts the namespace override on the Environment, under the project it binds", () => {
    const built = buildDocuments(form({ ...THREE_FIELDS, namespace: "hello-sandbox" }));

    expect(built.environment).toBe(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development

spec:
  project: hello
  namespace: hello-sandbox
`);
  });

  it("treats a cleared environment name as the default rather than a nameless Environment", () => {
    const built = buildDocuments(form({ ...THREE_FIELDS, environment: "  " }));

    expect(built.environmentName).toBe("development");
    expect(built.environment).toBe(MINIMAL_ENVIRONMENT);
  });

  it("renames the component when a staging environment is chosen", () => {
    const built = buildDocuments(form({ ...THREE_FIELDS, environment: "staging" }));

    expect(built.environmentName).toBe("staging");
    expect(built.environment).toContain("  name: staging\n");
    // The Project document stays environment-agnostic (docs/model.md).
    expect(built.project).toBe(MINIMAL_PROJECT);
  });
});

describe("building from a git repository", () => {
  it("writes source and build instead of image", () => {
    const built = buildDocuments(FROM_GIT);

    expect(built.project).toBe(SOURCE_PROJECT);
    expect(built.environment).toBe(MINIMAL_ENVIRONMENT);
    expect(built.buildsFromSource).toBe(true);
    // The two sources are exclusive in the document: a Project that names both
    // would have its image win over the build it also asked for (rule P3).
    expect(built.project).not.toContain("  image:");
  });

  it("writes the strategy that was chosen, and defaults to dockerfile", () => {
    // An untouched form builds what it has always built: the git path is not
    // silently rebuilt a different way for anyone who does not touch the choice.
    expect(DEFAULT_BUILD_STRATEGY).toBe("dockerfile");
    expect(EMPTY_FORM.buildStrategy).toBe("dockerfile");
    expect(buildDocuments(FROM_GIT).project).toContain(
      "  build:\n    strategy: dockerfile\n",
    );

    const buildpacks = buildDocuments(form({ ...FROM_GIT, buildStrategy: "buildpacks" }));

    expect(buildpacks.project).toBe(
      SOURCE_PROJECT.replace("strategy: dockerfile", "strategy: buildpacks"),
    );
    // Never `auto`, whichever way the radio is set: detecting a strategy means
    // reading the source tree, and the server has no checkout to read (#50).
    expect(buildpacks.project).not.toContain("auto");
    expect(buildDocuments(FROM_GIT).project).not.toContain("auto");
  });

  it("writes no build stanza at all for an image, whatever the strategy says", () => {
    // The choice survives a toggle back to the image path in the form's state,
    // exactly as the git fields do, and must not reach the document either.
    const built = buildDocuments(
      form({
        ...FROM_GIT,
        sourceMode: "image",
        image: "ghcr.io/acme/hello:1.4.2",
        buildStrategy: "buildpacks",
      }),
    );

    expect(built.project).toBe(MINIMAL_PROJECT);
  });

  it("leaves the ref out when it is blank, which means the default branch", () => {
    const built = buildDocuments(form({ ...FROM_GIT, ref: "  " }));

    expect(built.project).toBe(SOURCE_PROJECT.replace("    ref: main\n", ""));
    expect(built.project).not.toContain("ref:");
  });

  it("keeps writing image when the mode is switched back", () => {
    // The git fields survive the toggle in the form's state, and must not leak
    // into a document that says image.
    const built = buildDocuments(
      form({ ...FROM_GIT, sourceMode: "image", image: "ghcr.io/acme/hello:1.4.2" }),
    );

    expect(built.project).toBe(MINIMAL_PROJECT);
    expect(built.buildsFromSource).toBe(false);
  });

  it("requires a repository, and only that", () => {
    expect(formProblems(form({ ...FROM_GIT, git: "   " }))).toEqual([
      {
        field: "git",
        message:
          "a repository URL is required — kelson clones it in the cluster to build the image",
      },
    ]);
    // An empty image is not a problem in git mode, and a blank ref never is.
    expect(formProblems(form({ ...FROM_GIT, ref: "", image: "" }))).toEqual([]);
  });

  it("points the source findings at the fields that wrote them", () => {
    const wire = (field: string) =>
      create(ErrorSchema, { resource: "Project/hello", field, code: "schema/invalid" });

    expect(fieldForError(wire("$.spec.source.git"))).toBe("git");
    expect(fieldForError(wire("$.spec.source.ref"))).toBe("ref");
    // The strategy choice offers two values the server accepts, so a finding
    // about the build stanza is never a finding a radio can answer.
    expect(fieldForError(wire("$.spec.build.strategy"))).toBeUndefined();
  });
});

describe("splitFindings", () => {
  const unresolved = create(ErrorSchema, {
    code: IMAGE_UNRESOLVED,
    message: "no image yet: the spec builds this component from source",
  });
  const real = create(ErrorSchema, {
    code: "schema/unknown-field",
    resource: "Project/hello",
    field: "$.spec.components[0].portt",
  });

  it("separates the expected no-image-yet finding for a source build", () => {
    const { blocking, expected } = splitFindings([unresolved, real], true);

    expect(expected).toEqual([unresolved]);
    expect(blocking).toEqual([real]);
  });

  it("treats it as a blocker for a project that named an image", () => {
    const { blocking, expected } = splitFindings([unresolved], false);

    expect(blocking).toEqual([unresolved]);
    expect(expected).toEqual([]);
  });
});

describe("cron and port are mutually exclusive", () => {
  it("derives cron from a schedule and drops routing with it", () => {
    const built = buildDocuments(
      form({ project: "mailroom", image: "acme/mailroom:2", schedule: "0 3 * * *" }),
    );

    expect(built.project).toBe(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: mailroom

spec:
  image: acme/mailroom:2

  components:
    - name: cron
      schedule: "0 3 * * *"
`);
  });

  it("mirrors model.Application.Workload when both are set — and refuses to send it", () => {
    const both = form({
      project: "mailroom",
      image: "acme/mailroom:2",
      port: "8080",
      schedule: "0 3 * * *",
      health: "/healthz",
      domains: ["mailroom.acme.run"],
    });

    // Workload() puts schedule first, so this is what the model would derive.
    expect(workloadKind(both)).toBe("cron");
    // …and the port, health and domains it would silently ignore are exactly
    // why the form refuses the pair before a document is ever built.
    expect(formProblems(both).map((p) => p.field)).toContain("schedule");
    const built = buildDocuments(both).project;
    expect(built).not.toContain("port:");
    expect(built).not.toContain("health:");
    expect(built).not.toContain("domains:");
  });
});

describe("yamlScalar", () => {
  it("leaves an unambiguous string plain", () => {
    for (const value of [
      "hello",
      "ghcr.io/acme/hello:1.4.2",
      "/healthz",
      "hello.dev.acme.run",
      "1.4.2",
      "v2",
      "LOG_LEVEL",
      "postgres://db.svc:5432/app",
    ]) {
      expect(yamlScalar(value), value).toBe(value);
    }
  });

  it("quotes anything YAML would resolve as something other than a string", () => {
    // A boolean-looking name: `on` is true in YAML 1.1, and a project called
    // "on" must survive the round trip as the string the user typed.
    expect(yamlScalar("on")).toBe('"on"');
    expect(yamlScalar("no")).toBe('"no"');
    expect(yamlScalar("true")).toBe('"true"');
    expect(yamlScalar("null")).toBe('"null"');
    expect(yamlScalar("~")).toBe('"~"');
    // A numeric env value: model.EnvValue is a string union and refuses an
    // int node, so `PORT: 3000` unquoted is a rejected spec.
    expect(yamlScalar("3000")).toBe('"3000"');
    expect(yamlScalar("0755")).toBe('"0755"');
    expect(yamlScalar("1e6")).toBe('"1e6"');
    expect(yamlScalar("12:30")).toBe('"12:30"');
    expect(yamlScalar("2026-08-13")).toBe('"2026-08-13"');
  });

  it("quotes anything a plain scalar cannot hold", () => {
    expect(yamlScalar("")).toBe('""');
    expect(yamlScalar("host: value")).toBe('"host: value"');
    expect(yamlScalar("value # not a comment")).toBe('"value # not a comment"');
    expect(yamlScalar("0 3 * * *")).toBe('"0 3 * * *"');
    expect(yamlScalar(" padded ")).toBe('" padded "');
    expect(yamlScalar("- dash")).toBe('"- dash"');
    expect(yamlScalar("{a: b}")).toBe('"{a: b}"');
    expect(yamlScalar('say "hi"')).toBe('"say \\"hi\\""');
    expect(yamlScalar("two\nlines")).toBe('"two\\nlines"');
  });

  it("carries a quoted value into the document intact", () => {
    const built = buildDocuments(
      form({
        project: "on",
        image: "acme/on:1",
        port: "8080",
        env: [
          { key: "PORT", value: plainEnv("3000") },
          { key: "GREETING", value: plainEnv("hello: world") },
          { key: "EMPTY", value: plainEnv("") },
        ],
      }),
    );

    expect(built.project).toContain('  name: "on"\n');
    expect(built.project).toContain('    PORT: "3000"\n');
    expect(built.project).toContain('    GREETING: "hello: world"\n');
    expect(built.project).toContain('    EMPTY: ""\n');
    expect(built.environment).toContain('  project: "on"\n');
  });
});

describe("env values are one of three things (ADR-0018)", () => {
  it("writes a secret reference as the mapping the model reads", () => {
    const built = buildDocuments(
      form({
        ...THREE_FIELDS,
        env: [
          { key: "LOG_LEVEL", value: plainEnv("info") },
          { key: "DATABASE_URL", value: secretEnv("checkout-db", "url") },
        ],
      }),
    );

    expect(built.project).toContain(
      "  env:\n    LOG_LEVEL: info\n    DATABASE_URL: { secret: checkout-db, key: url }\n",
    );
    // The spelling is the one `kelson secret set` prints and the one the
    // Secrets panel offers to copy, byte for byte.
    expect(secretReference("checkout-db", "url")).toBe(
      "{ secret: checkout-db, key: url }",
    );
  });

  it("writes a service binding nested under `from`, as docs/model.md shows", () => {
    const built = buildDocuments(
      form({ ...THREE_FIELDS, env: [{ key: "CACHE_URL", value: bindingEnv("cache", "uri") }] }),
    );

    expect(built.project).toContain(
      "    CACHE_URL: { from: { service: cache, key: uri } }\n",
    );
  });

  it("trims a reference's names and leaves a plain value exactly as typed", () => {
    const built = buildDocuments(
      form({
        ...THREE_FIELDS,
        env: [
          { key: " DATABASE_URL ", value: secretEnv(" checkout-db ", " url ") },
          { key: "MOTD", value: plainEnv("hello ") },
        ],
      }),
    );

    expect(built.project).toContain("    DATABASE_URL: { secret: checkout-db, key: url }\n");
    expect(built.project).toContain('    MOTD: "hello "\n');
  });

  it("refuses half a reference rather than writing an empty one", () => {
    const problems = formProblems(
      form({
        ...THREE_FIELDS,
        env: [
          { key: "DATABASE_URL", value: secretEnv("checkout-db", "") },
          { key: "CACHE_URL", value: bindingEnv("", "uri") },
          { key: "LOG_LEVEL", value: plainEnv("") },
        ],
      }),
    );

    expect(problems.map((p) => p.field)).toEqual([
      "env:DATABASE_URL",
      "env:CACHE_URL",
    ]);
    expect(problems[0]?.message).toContain("{ secret: <name>, key: <key> }");
  });
});

describe("formProblems", () => {
  it("rejects a name that cannot be a metadata.name", () => {
    expect(formProblems(form({})).map((p) => p.field)).toEqual(["project"]);
    expect(formProblems(form({ project: "" }))[0]?.message).toBe(
      "a project name is required",
    );
    expect(formProblems(form({ project: "My_App" }))[0]?.message).toContain(
      "is not a DNS-1123 label",
    );
    expect(formProblems(form({ project: "a".repeat(64) }))[0]?.message).toContain(
      "is not a DNS-1123 label",
    );
    expect(formProblems(THREE_FIELDS)).toEqual([]);
  });

  it("refuses a port or replica count that is not a whole number", () => {
    expect(formProblems(form({ ...THREE_FIELDS, port: "80a0" }))[0]?.field).toBe("port");
    expect(formProblems(form({ ...THREE_FIELDS, replicas: "two" }))[0]?.field).toBe(
      "replicas",
    );
    // An empty port is a worker, not a mistake.
    expect(formProblems(form({ project: "hello", image: "acme/hello:1" }))).toEqual([]);
  });

  it("checks the optional names it would write into a document", () => {
    expect(formProblems(form({ ...THREE_FIELDS, environment: "Prod" }))[0]?.field).toBe(
      "environment",
    );
    expect(formProblems(form({ ...THREE_FIELDS, namespace: "Not A Namespace" }))[0]?.field).toBe(
      "namespace",
    );
  });

  it("leaves the rest to the server", () => {
    // An empty image, a malformed health path and a nonsense cron all pass the
    // browser: the server owns the taxonomy, the line numbers and the fixes.
    expect(
      formProblems(form({ project: "hello", image: "", health: "healthz" })),
    ).toEqual([]);
  });
});

describe("fieldForError", () => {
  const wire = (resource: string, field: string, code = "schema/invalid-format") =>
    create(ErrorSchema, { resource, field, code });

  it("maps a path onto the input that wrote it", () => {
    expect(fieldForError(wire("Project/hello", "$.metadata.name"))).toBe("project");
    expect(fieldForError(wire("Project/hello", "$.spec.image"))).toBe("image");
    expect(fieldForError(wire("Project/hello", "$.spec.components[0].port"))).toBe("port");
    expect(fieldForError(wire("Project/hello", "$.spec.components[0].health"))).toBe("health");
    expect(fieldForError(wire("Project/hello", "$.spec.components[0].schedule"))).toBe(
      "schedule",
    );
    expect(fieldForError(wire("Project/hello", "$.spec.components[0].domains[1]"))).toBe(
      "domains",
    );
    expect(fieldForError(wire("Project/hello", "$.spec.components[0].replicas.min"))).toBe(
      "replicas",
    );
    expect(fieldForError(wire("Project/hello", "$.spec.env.DB_PASSWORD", "secret/literal"))).toBe(
      "env:DB_PASSWORD",
    );
    expect(
      fieldForError(wire("Project/hello", "$.spec.components[0]", "semantic/no-image-source")),
    ).toBe("image");
  });

  it("keeps the two documents apart on the paths they share", () => {
    expect(fieldForError(wire("Environment/development", "$.metadata.name"))).toBe(
      "environment",
    );
    expect(fieldForError(wire("Environment/development", "$.spec.namespace"))).toBe(
      "namespace",
    );
  });

  it("returns undefined for a path no input owns", () => {
    // These reach the general panel whole, with their code and remediation.
    expect(fieldForError(wire("Project/hello", "$.spec.components[0].name"))).toBeUndefined();
    expect(fieldForError(wire("Environment/development", "$.spec.project"))).toBeUndefined();
    expect(fieldForError(wire("Project/hello", "$.apiVersion"))).toBeUndefined();
    expect(fieldForError(wire("Project/hello", "$.spec.components[0]"))).toBeUndefined();
  });
});
