import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";

/**
 * Building the two spec documents a new application is made of.
 *
 * The stored spec is the user's artifact (ADR-0013 §1: the server keeps the
 * authored bytes, comments and key order included), so what this module writes
 * is what a person would have written by hand: the shape of
 * examples/hello-single, with every key the form did not fill left out
 * entirely. A builder that emitted `health: ""` or `replicas: {min: 1}`
 * "because the schema has the field" would hand the user a document they now
 * have to prune.
 *
 * It is a string builder and not a YAML library on purpose. The output is a
 * fixed skeleton with scalars poured into it — five nesting levels, no
 * anchors, no multi-document streams — and the whole risk of doing it by hand
 * is quoting, which `yamlScalar` handles and its tests pin. Adding a YAML
 * dependency to this package is a decision, not a convenience (ui/README.md).
 *
 * The Go side agrees with this file by fixture: internal/api/uispec_test.go
 * holds the byte-identical minimal document pair from documents.test.ts and
 * asserts the real model accepts it. Change the bytes here and that test fails
 * there.
 */

/** One environment variable row. */
export interface EnvVar {
  key: string;
  value: string;
}

/**
 * Everything the create form can say. Every field is the raw text of an input,
 * including the numeric ones: the form owns strings, the builder owns the
 * document, and "the user has not typed a port yet" and "the user typed 0" are
 * different states that a number would collapse.
 */
export interface NewAppForm {
  project: string;
  image: string;
  port: string;
  environment: string;
  namespace: string;
  domains: string[];
  replicas: string;
  env: EnvVar[];
  health: string;
  schedule: string;
}

export const EMPTY_FORM: NewAppForm = {
  project: "",
  image: "",
  port: "",
  environment: "development",
  namespace: "",
  domains: [],
  replicas: "",
  env: [],
  health: "",
  schedule: "",
};

/** The environment an unfilled environment field means. */
export const DEFAULT_ENVIRONMENT = "development";

/** The workload an Application renders to (docs/model.md, model.Workload). */
export type WorkloadKind = "service" | "worker" | "cron";

/**
 * Mirrors model.Application.Workload in internal/model/project.go: schedule
 * first, then port, else worker. Kept identical so the preview names the
 * workload the server would derive, rather than a second opinion about it.
 *
 * The pair schedule+port is mutually exclusive in the model, and
 * [formProblems] refuses it before anything is built — so this function's
 * schedule-wins branch is a mirror of the Go rule, never a silent choice made
 * on the user's behalf.
 */
export function workloadKind(form: NewAppForm): WorkloadKind {
  if (form.schedule.trim() !== "") return "cron";
  if (form.port.trim() !== "") return "service";
  return "worker";
}

/**
 * The application's name inside the Project.
 *
 * The examples name applications after their role — `web` for the one that
 * serves traffic, `worker` for the one that does not (examples/hello-single,
 * examples/worker-cron) — so a one-application project gets the role name for
 * the workload its shape derives. The project name is already on the document
 * one level up; repeating it here would read as `hello.hello`.
 */
export function applicationName(kind: WorkloadKind): string {
  return kind === "service" ? "web" : kind === "cron" ? "cron" : "worker";
}

/** The namespace the model defaults to (docs/model.md: `<project>-<environment>`). */
export function defaultNamespace(project: string, environment: string): string {
  return `${project || "<project>"}-${environment || DEFAULT_ENVIRONMENT}`;
}

/** The two documents, ready to store. */
export interface SpecText {
  projectName: string;
  environmentName: string;
  project: string;
  environment: string;
}

interface Normal {
  project: string;
  image: string;
  port: string;
  environment: string;
  namespace: string;
  domains: string[];
  replicas: string;
  env: EnvVar[];
  health: string;
  schedule: string;
  kind: WorkloadKind;
}

function normalize(form: NewAppForm): Normal {
  return {
    project: form.project.trim(),
    image: form.image.trim(),
    port: form.port.trim(),
    // Blank means "the default", not "no environment": the field ships filled
    // in and its placeholder repeats the default, so clearing it is an edit
    // that must not produce a document with a nameless Environment.
    environment: form.environment.trim() || DEFAULT_ENVIRONMENT,
    namespace: form.namespace.trim(),
    domains: form.domains.map((d) => d.trim()).filter((d) => d !== ""),
    replicas: form.replicas.trim(),
    env: form.env
      .map(({ key, value }) => ({ key: key.trim(), value }))
      .filter(({ key }) => key !== ""),
    health: form.health.trim(),
    schedule: form.schedule.trim(),
    kind: workloadKind(form),
  };
}

export function buildDocuments(form: NewAppForm): SpecText {
  const f = normalize(form);
  return {
    projectName: f.project,
    environmentName: f.environment,
    project: projectDocument(f),
    environment: environmentDocument(f),
  };
}

/**
 * The Project document, in examples/hello-single's shape: a blank line between
 * each top-level section of `spec`, and nothing under `applications` that the
 * application's derived kind does not use.
 */
function projectDocument(f: Normal): string {
  const lines = [
    "apiVersion: kelson.dev/v1alpha1",
    "kind: Project",
    "metadata:",
    `  name: ${yamlScalar(f.project)}`,
    "",
    "spec:",
    `  image: ${yamlScalar(f.image)}`,
  ];

  // Project-level env is the shared-configuration idiom (rule P1): it already
  // reads correctly when a second application joins this one.
  if (f.env.length > 0) {
    lines.push("", "  env:");
    for (const { key, value } of f.env) {
      lines.push(`    ${yamlScalar(key)}: ${yamlScalar(value)}`);
    }
  }

  lines.push("", "  applications:", `    - name: ${applicationName(f.kind)}`);
  if (f.kind === "service") {
    lines.push(`      port: ${f.port}`);
    if (f.health !== "") lines.push(`      health: ${yamlScalar(f.health)}`);
  }
  if (f.kind === "cron") lines.push(`      schedule: ${yamlScalar(f.schedule)}`);
  if (f.kind === "service" && f.domains.length > 0) {
    lines.push("      domains:");
    for (const domain of f.domains) lines.push(`        - ${yamlScalar(domain)}`);
  }
  if (f.replicas !== "") lines.push(`      replicas: { min: ${f.replicas} }`);

  return lines.join("\n") + "\n";
}

function environmentDocument(f: Normal): string {
  const lines = [
    "apiVersion: kelson.dev/v1alpha1",
    "kind: Environment",
    "metadata:",
    `  name: ${yamlScalar(f.environment)}`,
    "",
    "spec:",
    `  project: ${yamlScalar(f.project)}`,
  ];
  if (f.namespace !== "") lines.push(`  namespace: ${yamlScalar(f.namespace)}`);
  return lines.join("\n") + "\n";
}

/* ------------------------------------------------------------------ quoting */

/**
 * A string as a YAML scalar: plain when that is unambiguous, double-quoted
 * when it is not.
 *
 * The rule is an allow-list, because the failure mode of guessing wrong is
 * silent. `PORT: 3000` decodes as an integer and `model.EnvValue` refuses it —
 * the author wanted a string and the spec says a number. `DEBUG: on` is worse:
 * YAML 1.1 resolves it to a boolean, so a value that looks fine on the page
 * never reaches the container as "on". Everything outside the narrow set of
 * characters that can only be a string is quoted, and over-quoting costs two
 * characters.
 *
 * JSON.stringify is the escape function on purpose: a JSON string literal is a
 * valid YAML double-quoted scalar (same \" \\ \n \t \uXXXX escapes), so there
 * is no second escaping implementation to get wrong.
 */
export function yamlScalar(value: string): string {
  if (
    value !== "" &&
    PLAIN.test(value) &&
    !NOT_A_STRING.test(value) &&
    !NUMERIC.test(value) &&
    !TIMESTAMP.test(value)
  ) {
    return value;
  }
  return JSON.stringify(value);
}

/**
 * Characters that carry no meaning to a YAML parser in a plain scalar. No
 * whitespace (which rules out `#` comments and trailing-space surprises at
 * once), no quotes, and none of the indicators `- ? : , [ ] { } & * ! | > % @`
 * in leading position. A `:` inside is fine — only `: ` opens a mapping — and
 * that is what keeps `ghcr.io/acme/hello:1.4.2` unquoted.
 */
const PLAIN = /^[A-Za-z0-9/._][A-Za-z0-9/._:+@-]*$/;

/** YAML 1.1 scalars that resolve to a boolean, a null, or a float constant. */
const NOT_A_STRING =
  /^(?:~|[Nn]ull|NULL|[Tt]rue|TRUE|[Ff]alse|FALSE|[Yy]|[Nn]|[Yy]es|YES|[Nn]o|NO|[Oo]n|ON|[Oo]ff|OFF|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$/;

/**
 * Everything YAML resolves as a number: decimal, octal (`0755` and `0o755`),
 * hex, binary, underscored, exponent — and sexagesimal, where `1:30` is 90.
 */
const NUMERIC =
  /^[-+]?(?:0b[01_]+|0o[0-7_]+|0x[0-9a-fA-F_]+|[0-9][0-9_]*(?::[0-5]?[0-9])+(?:\.[0-9_]*)?|(?:[0-9][0-9_]*)?\.?[0-9_]+(?:[eE][-+]?[0-9]+)?)$/;

/** A leading ISO date is enough for YAML to resolve the scalar as a timestamp. */
const TIMESTAMP = /^\d{4}-\d{1,2}-\d{1,2}/;

/* ---------------------------------------------------------- form validation */

/**
 * The form fields errors can be attached to. `env:NAME` is one environment
 * variable row, keyed by the variable's name because that is how the server
 * addresses it (`$.spec.env.LOG_LEVEL`).
 */
export type FieldKey =
  | "project"
  | "image"
  | "port"
  | "environment"
  | "namespace"
  | "domains"
  | "replicas"
  | "health"
  | "schedule"
  | `env:${string}`;

export interface FieldProblem {
  field: FieldKey;
  message: string;
}

const DNS_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const WHOLE_NUMBER = /^\d+$/;

/** Mirrors model's dnsLabelRE, which is what makes this name a metadata.name. */
export function dnsLabelProblem(value: string, what: string): string | undefined {
  if (value === "") return `${what} is required`;
  if (value.length > 63 || !DNS_LABEL.test(value)) {
    return `"${value}" is not a DNS-1123 label — lowercase letters, digits and dashes, starting and ending with a letter or digit`;
  }
  return undefined;
}

/**
 * What the browser refuses to send, and nothing more.
 *
 * The server owns validation — it holds the taxonomy, the line numbers and the
 * remediations, and a second copy of those rules in TypeScript would drift.
 * What is checked here is only what the browser must check to write a
 * well-formed document at all: a name that becomes `metadata.name` (which the
 * user is told about as they type, so the first thing they touch does not fail
 * on a round trip), the two fields that must be YAML integers rather than
 * strings, and the one pair the model calls mutually exclusive — because the
 * builder would otherwise have to pick a winner, and picking silently is the
 * failure this project refuses (#141).
 */
export function formProblems(form: NewAppForm): FieldProblem[] {
  const out: FieldProblem[] = [];

  const name = dnsLabelProblem(form.project.trim(), "a project name");
  if (name !== undefined) out.push({ field: "project", message: name });

  const environment = form.environment.trim();
  if (environment !== "") {
    const problem = dnsLabelProblem(environment, "an environment name");
    if (problem !== undefined) out.push({ field: "environment", message: problem });
  }

  const namespace = form.namespace.trim();
  if (namespace !== "") {
    const problem = dnsLabelProblem(namespace, "a namespace");
    if (problem !== undefined) out.push({ field: "namespace", message: problem });
  }

  const port = form.port.trim();
  if (port !== "" && !WHOLE_NUMBER.test(port)) {
    out.push({
      field: "port",
      message: `"${port}" is not a whole number — leave the port empty for a worker, or give the port the container listens on`,
    });
  }

  const replicas = form.replicas.trim();
  if (replicas !== "" && !WHOLE_NUMBER.test(replicas)) {
    out.push({ field: "replicas", message: `"${replicas}" is not a whole number` });
  }

  if (port !== "" && form.schedule.trim() !== "") {
    out.push({
      field: "schedule",
      message:
        "port and schedule are mutually exclusive: a port makes this a web service, a schedule makes it a CronJob. Clear one — a job that also serves traffic is two applications (docs/model.md).",
    });
  }

  return out;
}

/* ------------------------------------------------------- server error paths */

/** An application subfield a structured error can be about. */
export type AppField =
  | "whole"
  | "image"
  | "port"
  | "health"
  | "schedule"
  | "domains"
  | "replicas";

/**
 * Where in the two documents a structured error points, decoded from the
 * JSONPath the server put in `field` and the document `resource` names.
 *
 * This is the inverse of what the builders write, which is why it lives beside
 * them: the builder decides that a port ends up at
 * `$.spec.applications[0].port` and env at `$.spec.env.NAME`, so it is the
 * builder side that knows how to get back. `resource` disambiguates the two
 * documents, which share paths — `$.metadata.name` is the project's name on one
 * and the environment's on the other.
 *
 * It stops at the document's own vocabulary and says nothing about forms: the
 * create form (#63) has one application and no per-application env, the edit
 * form (#65) has both, and each projects this onto its own inputs. A path
 * neither recognises still reaches the reader whole in the general panel, with
 * its code, its remediation and its line number intact.
 */
export type ErrorTarget =
  | { doc: "project"; on: "name" | "image" }
  | { doc: "project"; on: "env"; name: string }
  | { doc: "project"; on: "app"; index: number; field: AppField }
  | { doc: "project"; on: "app-env"; index: number; name: string }
  | { doc: "environment"; on: "name" | "namespace"; environment: string };

export function errorTarget(error: WireError): ErrorTarget | undefined {
  const [kind, name = ""] = error.resource.split("/");
  const path = error.field;

  if (kind === "Environment") {
    if (path === "$.metadata.name") {
      return { doc: "environment", on: "name", environment: name };
    }
    if (path === "$.spec.namespace") {
      return { doc: "environment", on: "namespace", environment: name };
    }
    return undefined;
  }
  if (kind !== "Project") return undefined;

  if (path === "$.metadata.name") return { doc: "project", on: "name" };
  if (path === "$.spec.image") return { doc: "project", on: "image" };

  const env = /^\$\.spec\.env\.([^.]+)/.exec(path);
  if (env?.[1] !== undefined) {
    return { doc: "project", on: "env", name: env[1] };
  }

  const app = /^\$\.spec\.applications\[(\d+)\](?:\.(.+))?$/.exec(path);
  if (app?.[1] === undefined) return undefined;
  const index = Number(app[1]);
  const rest = app[2];
  // An application-level error with no subfield is about the application as a
  // whole — `semantic/no-image-source` is the one this project's forms answer.
  if (rest === undefined) return { doc: "project", on: "app", index, field: "whole" };

  const appEnv = /^env\.([^.]+)/.exec(rest);
  if (appEnv?.[1] !== undefined) {
    return { doc: "project", on: "app-env", index, name: appEnv[1] };
  }

  const field = appField(rest);
  return field === undefined
    ? undefined
    : { doc: "project", on: "app", index, field };
}

function appField(rest: string): AppField | undefined {
  if (rest === "image") return "image";
  if (rest === "port") return "port";
  if (rest === "health") return "health";
  if (rest === "schedule") return "schedule";
  if (rest.startsWith("domains")) return "domains";
  if (rest.startsWith("replicas")) return "replicas";
  return undefined;
}

/**
 * Which create-form field a structured error belongs to.
 *
 * The create form writes one application and no per-application env, so those
 * targets have no input to point at here and go to the general panel.
 */
export function fieldForError(error: WireError): FieldKey | undefined {
  const target = errorTarget(error);
  if (target === undefined) return undefined;
  if (target.doc === "environment") {
    return target.on === "name" ? "environment" : "namespace";
  }
  switch (target.on) {
    case "name":
      return "project";
    case "image":
      return "image";
    case "env":
      return `env:${target.name}`;
    case "app-env":
      return undefined;
    case "app": {
      if (target.index !== 0) return undefined;
      if (target.field === "whole") {
        return error.code === "semantic/no-image-source" ? "image" : undefined;
      }
      // The create form shares the project image across the one application it
      // writes, so a per-application image override has no input of its own.
      return target.field === "image" ? undefined : target.field;
    }
  }
}

export interface MappedErrors {
  /** Errors the form can show next to the input that caused them. */
  byField: Map<FieldKey, WireError[]>;
  /** Everything else, for the panel above the button. */
  general: WireError[];
}

export function mapErrors(errors: readonly WireError[]): MappedErrors {
  const byField = new Map<FieldKey, WireError[]>();
  const general: WireError[] = [];
  for (const error of errors) {
    const field = fieldForError(error);
    if (field === undefined) {
      general.push(error);
      continue;
    }
    const existing = byField.get(field);
    if (existing === undefined) byField.set(field, [error]);
    else existing.push(error);
  }
  return { byField, general };
}
