import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";

/**
 * Building the two spec documents a new component is made of.
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

/**
 * What an environment value is: one of exactly three things (ADR-0018).
 *
 * A scalar is a value; a mapping is a reference, and which reference is decided
 * by its own key. `{secret: <name>, key: <key>}` names a Secret in the
 * environment's namespace directly; `{from: {service, key}}` names a data
 * component this Project declares and lets kelson derive the Secret its
 * operator generates. Both render into the same `valueFrom.secretKeyRef` and
 * neither can carry a value, which is the whole point: the spec carries
 * references, never credentials (ADR-0009).
 *
 * The union is modelled here rather than as "a string that might look like a
 * mapping" so that the form can display a reference *as* a reference. A reader
 * who sees `{ secret: checkout-db, key: url }` rendered into a text input has
 * been shown YAML source, not their configuration.
 */
export type EnvValue =
  | { kind: "plain"; value: string }
  | { kind: "secret"; secret: string; key: string }
  | { kind: "binding"; service: string; key: string };

/** One environment variable row. */
export interface EnvVar {
  key: string;
  value: EnvValue;
}

export function plainEnv(value: string): EnvValue {
  return { kind: "plain", value };
}

export function secretEnv(secret: string, key: string): EnvValue {
  return { kind: "secret", secret, key };
}

export function bindingEnv(service: string, key: string): EnvValue {
  return { kind: "binding", service, key };
}

/**
 * An env value as the text that follows `KEY: `, in the one styling these
 * builders write.
 *
 * The two mapping forms are emitted as single-line flow mappings with the
 * spacing `replicas: { min: 1 }` already uses, because a flow mapping is what
 * ADR-0018 and docs/model.md show an author writing and because it keeps one
 * variable on one line. src/spec/edit.ts's round-trip guard is what makes that
 * choice safe for a document somebody wrote differently — see its module
 * comment for which stylings survive a rebuild and which go to the YAML tab.
 */
export function envValueText(value: EnvValue): string {
  switch (value.kind) {
    case "plain":
      return yamlScalar(value.value);
    case "secret":
      return `{ secret: ${yamlScalar(value.secret)}, key: ${yamlScalar(value.key)} }`;
    case "binding":
      return `{ from: { service: ${yamlScalar(value.service)}, key: ${yamlScalar(value.key)} } }`;
  }
}

/**
 * The ready-to-paste reference for one key of a Secret — the same line
 * `kelson secret set` prints after it writes (cmd/kelson/secret.go).
 *
 * One spelling of a reference, in one place: the UI's Secrets panel offers this
 * to copy and the spec builders write the identical bytes, so what a reader
 * pastes is what the editor would have produced.
 */
export function secretReference(name: string, key: string): string {
  return envValueText(secretEnv(name, key));
}

/**
 * A reference's own fields, trimmed.
 *
 * A plain value is left exactly as typed — trailing whitespace in a value is
 * the author's business and `yamlScalar` quotes it so it survives — but the two
 * halves of a reference are names, and a name with a space around it is a
 * typing artefact that would be quoted into the document and refused by the
 * server.
 */
export function trimEnvValue(value: EnvValue): EnvValue {
  switch (value.kind) {
    case "plain":
      return value;
    case "secret":
      return secretEnv(value.secret.trim(), value.key.trim());
    case "binding":
      return bindingEnv(value.service.trim(), value.key.trim());
  }
}

/**
 * The variables that have a name, with their references trimmed.
 *
 * A row with no name is a row the user has started and not finished, and the
 * form keeps showing it; the document does not, because `"": value` is not a
 * variable. Both builders apply the same rule, which is why it lives here.
 */
export function namedEnv(env: readonly EnvVar[]): EnvVar[] {
  return env
    .map(({ key, value }) => ({ key: key.trim(), value: trimEnvValue(value) }))
    .filter(({ key }) => key !== "");
}

/**
 * Half a reference is not a reference.
 *
 * The server owns validation, and this checks only what the builder must know
 * before it writes: `{ secret: "", key: "" }` is a mapping of the right shape
 * carrying no answer, and writing it would send the user a `schema/*` finding
 * about a document they can see is unfinished.
 */
export function envValueProblem(value: EnvValue): string | undefined {
  if (value.kind === "secret" && (value.secret.trim() === "" || value.key.trim() === "")) {
    return "a secret reference needs both a Secret name and a key: { secret: <name>, key: <key> } points at one key of a Secret in the environment's namespace";
  }
  if (value.kind === "binding" && (value.service.trim() === "" || value.key.trim() === "")) {
    return "a service binding needs both a component name and a key: { from: { service: <component>, key: <key> } }";
  }
  return undefined;
}

/**
 * Where the component's image comes from: a reference someone else built, or a
 * git repository kelson builds itself (#63, docs/build.md).
 */
export type SourceMode = "image" | "git";

/**
 * How kelson turns the repository into an image (ADR-0010), and which of the
 * strategies the browser is allowed to ask for.
 *
 * ADR-0010 has four: `auto`, `dockerfile`, `buildpacks`, `none`. Two of them
 * are not questions this form can put to a user. `none` says "build nothing,
 * use `image:`", which is the other source mode spelled a second way. `auto`
 * says "look at the source tree and decide", and a server has no source tree
 * to look at — the tree exists inside the build pod, after the clone — so
 * BuildService refuses it with `build/detection-needs-source` until in-cluster
 * detection lands (#50, proto/kelson/v1alpha1/build.proto). Offering `auto`
 * would be offering a choice whose only outcome is a refusal.
 *
 * So the form asks the question `auto` would have answered — is there a
 * Dockerfile? — and writes the answer explicitly, which is what makes both
 * strategies reachable from a browser.
 */
export type BuildStrategy = "dockerfile" | "buildpacks";

/**
 * What the form writes for someone who does not touch the choice.
 *
 * ADR-0010 makes buildpacks the eventual zero-config *default*, and this is
 * deliberately not that: `dockerfile` is what this form has written since it
 * had a git path, and changing what an untouched form produces would rebuild
 * everyone's next project a different way without saying so.
 */
export const DEFAULT_BUILD_STRATEGY: BuildStrategy = "dockerfile";

/**
 * Everything the create form can say. Every field is the raw text of an input,
 * including the numeric ones: the form owns strings, the builder owns the
 * document, and "the user has not typed a port yet" and "the user typed 0" are
 * different states that a number would collapse.
 */
export interface NewProjectForm {
  project: string;
  /** Which of `image` and `git`/`ref` the document is written from. */
  sourceMode: SourceMode;
  image: string;
  git: string;
  ref: string;
  /**
   * `spec.source.connection`: the GitConnection this project's source resolves
   * through, written only when something chose one (ADR-0033 decision 4).
   *
   * Empty is the ordinary state and means "resolve by host match", which is
   * what one connection and zero configuration look like. It is filled by the
   * repository picker, because picking a repository *from* a connection has
   * already answered the question the host match would guess at — and a guess
   * that later goes ambiguous, when a second connection covers the same host,
   * would break a project that was created by pointing at one.
   */
  connection: string;
  /** Only written in `git` mode: `spec.build.strategy` (ADR-0010). */
  buildStrategy: BuildStrategy;
  port: string;
  environment: string;
  namespace: string;
  domains: string[];
  replicas: string;
  env: EnvVar[];
  health: string;
  schedule: string;
}

export const EMPTY_FORM: NewProjectForm = {
  project: "",
  sourceMode: "image",
  image: "",
  git: "",
  ref: "",
  connection: "",
  buildStrategy: DEFAULT_BUILD_STRATEGY,
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

/**
 * The workload kinds this form can write (docs/model.md, ADR-0014).
 *
 * The model's kind enum is wider — `agent`, `postgres` and `valkey` are
 * components too — but those are not shapes a three-field create form derives,
 * and offering a kind picker here would be the forty-question first screen this
 * page exists to avoid. They are written on the YAML tab.
 */
export type WorkloadKind = "service" | "worker" | "cron";

/**
 * Mirrors model.Component.DerivedKind in internal/model/project.go: schedule
 * first, then port, else worker. Kept identical so the preview names the
 * workload the server would derive, rather than a second opinion about it.
 *
 * The pair schedule+port is mutually exclusive in the model, and
 * [formProblems] refuses it before anything is built — so this function's
 * schedule-wins branch is a mirror of the Go rule, never a silent choice made
 * on the user's behalf.
 */
export function workloadKind(form: NewProjectForm): WorkloadKind {
  if (form.schedule.trim() !== "") return "cron";
  if (form.port.trim() !== "") return "service";
  return "worker";
}

/**
 * The component's name inside the Project.
 *
 * The examples name components after their role — `web` for the one that
 * serves traffic, `worker` for the one that does not (examples/hello-single,
 * examples/worker-cron) — so a one-component project gets the role name for
 * the kind its shape derives. The project name is already on the document
 * one level up; repeating it here would read as `hello.hello`.
 */
export function componentName(kind: WorkloadKind): string {
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
  /** True when the Project names a source repository instead of an image. */
  buildsFromSource: boolean;
}

interface Normal {
  project: string;
  sourceMode: SourceMode;
  image: string;
  git: string;
  ref: string;
  connection: string;
  buildStrategy: BuildStrategy;
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

function normalize(form: NewProjectForm): Normal {
  return {
    project: form.project.trim(),
    sourceMode: form.sourceMode,
    image: form.image.trim(),
    git: form.git.trim(),
    ref: form.ref.trim(),
    connection: form.connection.trim(),
    buildStrategy: form.buildStrategy,
    port: form.port.trim(),
    // Blank means "the default", not "no environment": the field ships filled
    // in and its placeholder repeats the default, so clearing it is an edit
    // that must not produce a document with a nameless Environment.
    environment: form.environment.trim() || DEFAULT_ENVIRONMENT,
    namespace: form.namespace.trim(),
    domains: form.domains.map((d) => d.trim()).filter((d) => d !== ""),
    replicas: form.replicas.trim(),
    env: namedEnv(form.env),
    health: form.health.trim(),
    schedule: form.schedule.trim(),
    kind: workloadKind(form),
  };
}

export function buildDocuments(form: NewProjectForm): SpecText {
  const f = normalize(form);
  return {
    projectName: f.project,
    environmentName: f.environment,
    project: projectDocument(f),
    environment: environmentDocument(f),
    buildsFromSource: f.sourceMode === "git",
  };
}

/**
 * The Project document, in examples/hello-single's shape: a blank line between
 * each top-level section of `spec`, and nothing under `components` that the
 * component's derived kind does not use.
 *
 * The two source modes write the two shapes the examples use. A pre-built image
 * is one `image:` line; a git source is the `source:`/`build:` pair of
 * examples/three-environments, adjacent with no blank line between them,
 * because they are one statement about where the image comes from.
 */
function projectDocument(f: Normal): string {
  const lines = [
    "apiVersion: kelson.dev/v1alpha1",
    "kind: Project",
    "metadata:",
    `  name: ${yamlScalar(f.project)}`,
    "",
    "spec:",
  ];

  if (f.sourceMode === "git") {
    lines.push("  source:", `    git: ${yamlScalar(f.git)}`);
    // No ref means the repository's default branch, which is what leaving the
    // key out says. Writing `ref: main` for someone whose default branch is
    // `master` would be a guess with a failure mode.
    if (f.ref !== "") lines.push(`    ref: ${yamlScalar(f.ref)}`);
    // Written only when a connection was chosen. An absent key is ADR-0033
    // decision 4's default — resolve by longest host-then-owner match — and
    // writing the connection the match would have picked anyway would pin a
    // project to a name that is free to be deleted, for no gain.
    if (f.connection !== "") {
      lines.push(`    connection: ${yamlScalar(f.connection)}`);
    }
    // The strategy is written even when it is the one ADR-0010 would have
    // picked anyway: a document that leaves it out means `auto`, and `auto` is
    // the one answer the server cannot act on for a remote repository (#50).
    lines.push("  build:", `    strategy: ${f.buildStrategy}`);
  } else {
    lines.push(`  image: ${yamlScalar(f.image)}`);
  }

  // Project-level env is the shared-configuration idiom (rule P1): it already
  // reads correctly when a second component joins this one.
  if (f.env.length > 0) {
    lines.push("", "  env:");
    for (const { key, value } of f.env) {
      lines.push(`    ${yamlScalar(key)}: ${envValueText(value)}`);
    }
  }

  lines.push("", "  components:", `    - name: ${componentName(f.kind)}`);
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
  | "git"
  | "ref"
  | "connection"
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
export function formProblems(form: NewProjectForm): FieldProblem[] {
  const out: FieldProblem[] = [];

  const name = dnsLabelProblem(form.project.trim(), "a project name");
  if (name !== undefined) out.push({ field: "project", message: name });

  // The document needs one source or the other. Beyond "there is something
  // here" the server owns the judgement: it holds the URL rules and the image
  // reference grammar, and a second copy of either in TypeScript would drift.
  if (form.sourceMode === "git" && form.git.trim() === "") {
    out.push({
      field: "git",
      message:
        "a repository URL is required — kelson clones it in the cluster to build the image",
    });
  }

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

  for (const row of form.env) {
    const name = row.key.trim();
    if (name === "") continue;
    const problem = envValueProblem(row.value);
    if (problem !== undefined) out.push({ field: `env:${name}`, message: problem });
  }

  if (port !== "" && form.schedule.trim() !== "") {
    out.push({
      field: "schedule",
      message:
        "port and schedule are mutually exclusive: a port makes this a web service, a schedule makes it a scheduled job. Clear one — a job that also serves traffic is two components.",
    });
  }

  return out;
}

/* ------------------------------------------------------- server error paths */

/** A component subfield a structured error can be about. */
export type ComponentField =
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
 * `$.spec.components[0].port` and env at `$.spec.env.NAME`, so it is the
 * builder side that knows how to get back. `resource` disambiguates the two
 * documents, which share paths — `$.metadata.name` is the project's name on one
 * and the environment's on the other.
 *
 * It stops at the document's own vocabulary and says nothing about forms: the
 * create form (#63) has one component and no per-component env, the edit
 * form (#65) has both, and each projects this onto its own inputs. A path
 * neither recognises still reaches the reader whole in the general panel, with
 * its code, its remediation and its line number intact.
 */
export type ErrorTarget =
  | {
      doc: "project";
      on: "name" | "image" | "git" | "ref" | "connection" | "build";
    }
  | { doc: "project"; on: "env"; name: string }
  | { doc: "project"; on: "component"; index: number; field: ComponentField }
  | { doc: "project"; on: "component-env"; index: number; name: string }
  | { doc: "environment"; on: "name" | "namespace"; environment: string }
  // The nested Environment stanza the edit form reaches. `field` is the path
  // after the stanza — "artifacts.repository", "filter.labels[1]" — left whole
  // here and projected onto inputs by whichever form has them, exactly as a
  // component's subfield is.
  | {
      doc: "environment";
      on: "previews";
      environment: string;
      field: string;
    };

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
    // `$.spec.delivery` is deliberately absent: the block is retired
    // (ADR-0028, #234) and the model answers a document carrying one with
    // "delete the whole `delivery:` block". No input can fix that, so the
    // finding falls through to the general panel where its remediation is.
    const stanza = /^\$\.spec\.previews(?:\.(.+))?$/.exec(path);
    if (stanza !== null) {
      return {
        doc: "environment",
        on: "previews",
        environment: name,
        field: stanza[1] ?? "",
      };
    }
    return undefined;
  }
  if (kind !== "Project") return undefined;

  if (path === "$.metadata.name") return { doc: "project", on: "name" };
  if (path === "$.spec.image") return { doc: "project", on: "image" };
  if (path === "$.spec.source.git") return { doc: "project", on: "git" };
  if (path === "$.spec.source.ref") return { doc: "project", on: "ref" };
  // Where a resolution refusal lands: internal/forgeconn points both of them —
  // two connections cover this repository, or the named one does not exist —
  // at this path, and the control that fixes either is the repository picker.
  if (path === "$.spec.source.connection") {
    return { doc: "project", on: "connection" };
  }
  // The only input the build stanza has is the strategy choice, and both of
  // its answers are values the server accepts — so a finding here is about
  // something no radio can fix (a dockerfile path, a strategy this release does
  // not implement). It is named rather than pointed at, and lands in the
  // general panel with its code intact.
  if (path.startsWith("$.spec.build")) return { doc: "project", on: "build" };

  const env = /^\$\.spec\.env\.([^.]+)/.exec(path);
  if (env?.[1] !== undefined) {
    return { doc: "project", on: "env", name: env[1] };
  }

  const component = /^\$\.spec\.components\[(\d+)\](?:\.(.+))?$/.exec(path);
  if (component?.[1] === undefined) return undefined;
  const index = Number(component[1]);
  const rest = component[2];
  // A component-level error with no subfield is about the component as a
  // whole — `semantic/no-image-source` is the one this project's forms answer.
  if (rest === undefined) return { doc: "project", on: "component", index, field: "whole" };

  const componentEnv = /^env\.([^.]+)/.exec(rest);
  if (componentEnv?.[1] !== undefined) {
    return { doc: "project", on: "component-env", index, name: componentEnv[1] };
  }

  const field = componentField(rest);
  return field === undefined
    ? undefined
    : { doc: "project", on: "component", index, field };
}

function componentField(rest: string): ComponentField | undefined {
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
 * The create form writes one component and no per-component env, so those
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
    case "git":
      return "git";
    case "ref":
      return "ref";
    case "connection":
      return "connection";
    case "build":
      return undefined;
    case "env":
      return `env:${target.name}`;
    case "component-env":
      return undefined;
    case "component": {
      if (target.index !== 0) return undefined;
      if (target.field === "whole") {
        return error.code === "semantic/no-image-source" ? "image" : undefined;
      }
      // The create form shares the project image across the one component it
      // writes, so a per-component image override has no input of its own.
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

/**
 * The renderer's code for "this component has no image yet". It is the one
 * finding a source-built project is *expected* to report before its first
 * build.
 */
export const IMAGE_UNRESOLVED = "image/unresolved";

export interface Findings {
  /** Findings that mean the document is wrong. */
  blocking: WireError[];
  /** Findings that are true, expected, and not a reason to stop. */
  expected: WireError[];
}

/**
 * Splitting the preflight's findings for a project that builds from source.
 *
 * `PutSpec` at `dry_run=RENDER` validates *and renders*, and a project whose
 * image comes from a build has no image to render with — so it always comes
 * back with `image/unresolved` (internal/api/spec.go's renderEveryEnvironment).
 * That finding is correct and the create is still legitimate: storing a spec
 * does not render it, so the write succeeds and the image arrives when a build
 * produces one (#136).
 *
 * Suppressing it would be lying about what the server said; treating it as a
 * blocker would make the git path impossible. So it is separated and shown as
 * what it is — with its code, because that is what a reader can look up — while
 * every other finding still stops the create. Nothing is filtered for a project
 * that names an image: there, `image/unresolved` would be a real problem.
 */
export function splitFindings(
  errors: readonly WireError[],
  buildsFromSource: boolean,
): Findings {
  if (!buildsFromSource) return { blocking: [...errors], expected: [] };
  const blocking: WireError[] = [];
  const expected: WireError[] = [];
  for (const error of errors) {
    (error.code === IMAGE_UNRESOLVED ? expected : blocking).push(error);
  }
  return { blocking, expected };
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
