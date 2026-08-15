import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { isVersionConflict } from "../api/errors";
import { useRun } from "../api/stream";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import type {
  BuildResponse_Finished,
  BuildResponse_Started,
} from "../gen/kelson/v1alpha1/build_pb";
import { Copyable } from "../components/Copyable";
import { Disclosure, YamlBlock } from "../components/Disclosure";
import { EnvValueFields, envValueNote } from "../components/EnvValueFields";
import { ErrorPanel } from "../components/ErrorPanel";
import { RepositoryPicker, type RepositoryPick } from "../components/RepositoryPicker";
import { StatusPill } from "../components/StatusPill";
import {
  buildDocuments,
  defaultNamespace,
  EMPTY_FORM,
  formProblems,
  mapErrors,
  plainEnv,
  splitFindings,
  workloadKind,
  type BuildStrategy,
  type EnvVar,
  type FieldKey,
  type NewProjectForm,
  type SourceMode,
  type SpecText,
} from "../spec/documents";

/**
 * Creating a project and its first component: three fields, then a preview,
 * then a store.
 *
 * # Two sources, one form
 *
 * A component's image either exists already or has to be built (#63). "From
 * image" is three fields and stays the default because it is the shorter path
 * to something running. "From Git repository" writes `source:` and `build:`
 * instead of `image:`, and unlocks a build step after the create.
 *
 * The registry and the push credential are *not* asked for. They are the
 * server's configuration (`kelson-server --registry`, `--push-secret`,
 * docs/build.md) for the reason ADR-0010 gives: where an image is pushed is
 * infrastructure, and the same Project must build against a team's ghcr.io and
 * a kind cluster's localhost:5000. Asking each creator would put an operator's
 * decision on a developer's first screen.
 *
 * The build strategy *is* asked for, and only because ADR-0010's own default
 * cannot be answered from here: `auto` means "read the source tree", and the
 * tree only exists inside the build pod, after the clone (#50). So the git path
 * asks the one question detection would have answered — is there a Dockerfile?
 * — and writes the answer, rather than storing a spec whose build refuses.
 *
 * # Two ways to name a repository, and the typed one is not the lesser
 *
 * When this instance holds at least one git connection, the git path offers a
 * picker: a connection, then one of its repositories, then a branch
 * (ADR-0033 decision 3, #248). What it does is *fill in the fields below* — the
 * repository URL, the ref, and `spec.source.connection` — so there is exactly
 * one path from here to a document, and the picker is a shortcut along it
 * rather than a fork in it.
 *
 * The typed path therefore stays first-class, and it is what remains when the
 * picker cannot help: no connections, a connection whose forge has no
 * repository browser (which refuses with `connection/capability-unsupported`
 * and says so in the picker, beside inputs that still work), or a forge that
 * will not answer. None of those is a dead end, because none of them was ever
 * the only way in.
 *
 * The bar this screen is held to (#63) is that a developer who has never seen
 * kelson deploys something without reading anything. So what is *present* is a
 * name, an image and a port — and the port is optional, because an empty port
 * is a worker (docs/model.md derives the component's kind from the shape rather
 * than asking for one). Everything else the model can express is *reachable*
 * behind one disclosure and nothing more. The named failure mode is Coolify's:
 * a first screen that asks forty questions to deploy one container.
 *
 * ADR-0014's kind set is wider than the three this form derives — an `agent`,
 * a `postgres`, a `valkey` are components too — and none of them is offered
 * here on purpose. A kind picker on the first screen is the forty questions
 * arriving one step later. What this creates is a project with its *first*
 * component, and the second one — a worker, a nightly job, the database they
 * share — is added afterwards from the project's own page (#214), which is
 * where a kind picker costs nothing because the project already exists.
 *
 * Two things are deliberately not offered at all: secret backends, deployment
 * policy and multi-cluster targeting, because #141 has kelson reject those
 * fields rather than render nothing for them — a form that collected them
 * would be building a document the server refuses; and a
 * "deploy on create" shortcut, because storing a spec and applying it to a
 * cluster are different acts with different blast radii, and the deploy screen
 * already exists to show the second one happening.
 *
 * The flow is the API's own two rungs. `PutSpec` at dry_run=RENDER validates,
 * renders every environment and stores nothing (ADR-0013 §2), so the check
 * costs nothing and its findings are structured: each carries a JSONPath, and
 * the paths this form's builder wrote are the paths it can map back onto
 * inputs. Only then does the second call store, with an idempotency key so a
 * retried press is a replay rather than a second project.
 */

const ENCODER = new TextEncoder();

interface Checked {
  documents: SpecText;
  /** Minted once per checked document set, so pressing Create twice replays. */
  idempotencyKey: string;
  /**
   * Findings that are true and expected rather than wrong — for a source-built
   * project, `image/unresolved` (see splitFindings). Shown, never hidden.
   */
  expected: readonly WireError[];
}

interface Created {
  project: string;
  environment: string;
  buildsFromSource: boolean;
}

export function NewProjectPage() {
  const clients = useClients();
  const [form, setForm] = useState<NewProjectForm>(EMPTY_FORM);
  const [touched, setTouched] = useState<Partial<Record<FieldKey, true>>>({});
  const [attempted, setAttempted] = useState(false);
  const [more, setMore] = useState(false);
  const [wire, setWire] = useState<readonly WireError[]>([]);
  const [checked, setChecked] = useState<Checked | undefined>(undefined);
  const [created, setCreated] = useState<Created | undefined>(undefined);
  const check = useRun();
  const store = useRun();

  /**
   * Every edit drops the previous answer.
   *
   * The preview and the server's findings are both statements about a document
   * that no longer exists the moment a key is pressed. Keeping either on screen
   * would let someone press Create on bytes they have since edited, or read an
   * error against a line they have already fixed.
   */
  const updateMany = useCallback(
    (patch: Partial<NewProjectForm>, fields: FieldKey[] = []) => {
      setForm((prev) => ({ ...prev, ...patch }));
      if (fields.length > 0) {
        setTouched((prev) => {
          const next = { ...prev };
          for (const field of fields) next[field] = true;
          return next;
        });
      }
      setWire([]);
      setChecked(undefined);
    },
    [],
  );

  const update = useCallback(
    <K extends keyof NewProjectForm>(key: K, value: NewProjectForm[K], field?: FieldKey) => {
      updateMany({ [key]: value } as Partial<NewProjectForm>, field === undefined ? [] : [field]);
    },
    [updateMany],
  );

  /**
   * A pick writes the three source fields at once, because they are one
   * statement: this repository, on this branch, through this connection.
   * Writing them one at a time would leave the form momentarily describing a
   * repository with the previous one's branch.
   */
  const pick = useCallback(
    (picked: RepositoryPick) => {
      updateMany(
        { git: picked.git, ref: picked.ref, connection: picked.connection },
        ["git", "ref"],
      );
    },
    [updateMany],
  );

  /**
   * The connections this instance holds, or none.
   *
   * A failure here is deliberately not shown and deliberately not fatal. A
   * server with no connection store answers Unimplemented, an older one does
   * not know the RPC, and neither is a reason to put an error on the screen
   * somebody came to to create a project: what it means is that there is
   * nothing to pick from, which is the state this form has always been in. The
   * read is skipped entirely outside the git path, where a repository picker
   * would have nothing to fill in.
   */
  const connections = useAsync(async (signal) => {
    if (form.sourceMode !== "git") return [];
    const res = await clients.gitConnection.listConnections({}, { signal });
    return res.connections;
  }, [clients, form.sourceMode]);

  const problems = useMemo(() => formProblems(form), [form]);
  const mapped = useMemo(() => mapErrors(wire), [wire]);
  const kind = workloadKind(form);

  // A conflict on a blind create has exactly one cause: the name is taken. The
  // store's own message ("the write carried no version") is true and useless to
  // someone who has never written a spec, so it is answered here instead.
  const taken = store.error !== undefined && isNameTaken(store.error);

  const problemFor = (field: FieldKey): string | undefined => {
    if (!attempted && touched[field] === undefined) return undefined;
    return problems.find((p) => p.field === field)?.message;
  };
  const errorsFor = (field: FieldKey): WireError[] => mapped.byField.get(field) ?? [];

  const validate = useCallback(() => {
    setAttempted(true);
    if (formProblems(form).length > 0) return;
    const documents = buildDocuments(form);
    check.start(async (signal) => {
      const res = await clients.spec.putSpec(
        { documents: specDocuments(documents), dryRun: DryRun.RENDER },
        { signal },
      );
      const { blocking, expected } = splitFindings(
        res.errors,
        documents.buildsFromSource,
      );
      setWire(blocking);
      setChecked(
        blocking.length === 0
          ? { documents, idempotencyKey: crypto.randomUUID(), expected }
          : undefined,
      );
    });
  }, [check, clients, form]);

  const createIt = useCallback(() => {
    if (checked === undefined) return;
    store.start(async (signal) => {
      await clients.spec.putSpec(
        {
          documents: specDocuments(checked.documents),
          idempotencyKey: checked.idempotencyKey,
        },
        { signal },
      );
      setCreated({
        project: checked.documents.projectName,
        environment: checked.documents.environmentName,
        buildsFromSource: checked.documents.buildsFromSource,
      });
    });
  }, [checked, clients, store]);

  if (created !== undefined) {
    return <Stored created={created} />;
  }

  const routed = kind === "service";

  return (
    <>
      <div className="k-page-head">
        <h1>New project</h1>
      </div>
      <div className="k-page-sub">
        <Link to="/projects">← all projects</Link>
        <span>·</span>
        <span>
          {form.sourceMode === "git"
            ? "a name, a repository, a port"
            : "a name, an image, a port"}
        </span>
      </div>

      {/* A project is a container of components (ADR-0014), and this screen
          creates it with the first one. Saying so here is what keeps the next
          component from looking like a second project. */}
      <p className="k-note">
        A project holds its components — a service, its worker, a nightly job,
        the database they share. This creates the project and its first
        component; the rest are added afterwards from the project's page.
      </p>

      <form
        className="k-new"
        onSubmit={(e) => {
          e.preventDefault();
          validate();
        }}
      >
        <SourceToggle
          mode={form.sourceMode}
          onChange={(mode) => update("sourceMode", mode)}
        />

        {form.sourceMode === "git" ? (
          <StrategyToggle
            strategy={form.buildStrategy}
            onChange={(strategy) => update("buildStrategy", strategy)}
          />
        ) : null}

        {/* Offered only when there is something to pick from. With no
            connections this is the form it has always been, which is the
            property that keeps the typed path first-class rather than a
            fallback nobody maintains. */}
        {form.sourceMode === "git" && (connections.data ?? []).length > 0 ? (
          <>
            <RepositoryPicker
              connections={connections.data ?? []}
              picked={{
                connection: form.connection,
                git: form.git,
                ref: form.ref,
              }}
              onPick={pick}
            />
            <FieldErrors errors={errorsFor("connection")} />
          </>
        ) : null}

        <div className="k-panel k-new__row">
          <Field
            label="Project name"
            value={form.project}
            onChange={(v) => update("project", v, "project")}
            placeholder="hello"
            problem={problemFor("project")}
            errors={errorsFor("project")}
            note="becomes metadata.name — a DNS-1123 label"
          >
            {taken ? (
              <span className="k-field__problem k-mono" role="alert">
                a project with this name already exists —{" "}
                <Link to={`/projects/${encodeURIComponent(form.project.trim())}`}>
                  open it
                </Link>{" "}
                or choose another name
              </span>
            ) : null}
          </Field>

          {form.sourceMode === "image" ? (
            <Field
              label="Image"
              value={form.image}
              onChange={(v) => update("image", v, "image")}
              placeholder="ghcr.io/acme/hello:1.4.2"
              problem={problemFor("image")}
              errors={errorsFor("image")}
              note="a pre-built image reference — a tag works, a digest is reproducible"
            />
          ) : (
            <>
              <Field
                label="Git repository"
                value={form.git}
                // Typing over the URL unpins the connection a pick wrote. The
                // two are one statement about where the source comes from, and
                // a name left pinned to a repository somebody has since
                // retyped would authenticate the next build with a credential
                // chosen for a different forge.
                onChange={(v) => updateMany({ git: v, connection: "" }, ["git"])}
                placeholder="https://github.com/acme/hello"
                problem={problemFor("git")}
                errors={errorsFor("git")}
                note={
                  form.connection === ""
                    ? "cloned inside the cluster at build time; a private repository resolves its credential by matching this host against the instance's git connections"
                    : `cloned inside the cluster at build time, through connection ${form.connection}`
                }
              />
              <Field
                label="Ref"
                narrow
                value={form.ref}
                onChange={(v) => update("ref", v, "ref")}
                placeholder="main"
                problem={problemFor("ref")}
                errors={errorsFor("ref")}
                note="branch, tag or commit — empty leaves it out, which means the repository's default branch"
              />
            </>
          )}

          <Field
            label="Port"
            narrow
            value={form.port}
            onChange={(v) => update("port", v, "port")}
            placeholder="8080"
            problem={problemFor("port")}
            errors={errorsFor("port")}
            note={
              kind === "cron"
                ? "unused: a schedule makes this a CronJob"
                : routed
                  ? "a port makes this a web service: Deployment + Service + routing"
                  : "empty: a worker — Deployment, no routing"
            }
          />
        </div>

        <details
          className="k-disclosure"
          open={more}
          onToggle={(e) => setMore(e.currentTarget.open)}
        >
          <summary className="k-disclosure__summary">
            <span className="k-disclosure__label">More options</span>
            <span className="k-disclosure__meta k-mono">
              environment · namespace · replicas · env · {routed ? "domains · health" : "schedule"}
            </span>
          </summary>

          <div className="k-disclosure__body k-new__more-body">
            <div className="k-new__row">
              <Field
                label="Environment"
                value={form.environment}
                onChange={(v) => update("environment", v, "environment")}
                placeholder="development"
                problem={problemFor("environment")}
                errors={errorsFor("environment")}
                note="the Environment document's name; every flow in this UI is addressed by (project, environment)"
              />
              <Field
                label="Namespace"
                value={form.namespace}
                onChange={(v) => update("namespace", v, "namespace")}
                placeholder={defaultNamespace(
                  form.project.trim(),
                  form.environment.trim(),
                )}
                problem={problemFor("namespace")}
                errors={errorsFor("namespace")}
                note="override the model's default; blank leaves the default in place"
              />
              <Field
                label="Replicas"
                narrow
                value={form.replicas}
                onChange={(v) => update("replicas", v, "replicas")}
                placeholder="1"
                problem={problemFor("replicas")}
                errors={errorsFor("replicas")}
                note="a fixed count"
              />
            </div>

            {routed ? (
              <div className="k-new__row">
                <Field
                  label="Health path"
                  value={form.health}
                  onChange={(v) => update("health", v, "health")}
                  placeholder="/healthz"
                  problem={problemFor("health")}
                  errors={errorsFor("health")}
                  note="an HTTP path used for both probes"
                />
              </div>
            ) : null}

            {routed ? (
              <Rows
                label="Domains"
                note="explicit FQDNs. A cluster with no Gateway API cannot serve them, and the check below says so rather than falling back to Ingress (#140)."
                addLabel="Add domain"
                problem={problemFor("domains")}
                errors={errorsFor("domains")}
                values={form.domains}
                placeholder="hello.dev.acme.run"
                onChange={(next) => update("domains", next, "domains")}
              />
            ) : null}

            {!routed ? (
              <div className="k-new__row">
                <Field
                  label="Schedule"
                  value={form.schedule}
                  onChange={(v) => update("schedule", v, "schedule")}
                  placeholder="0 3 * * *"
                  problem={problemFor("schedule")}
                  errors={errorsFor("schedule")}
                  note="a five-field cron expression makes this a CronJob instead of a worker"
                />
              </div>
            ) : null}

            <EnvRows
              env={form.env}
              onChange={(next) => update("env", next)}
              problemFor={problemFor}
              errorsFor={errorsFor}
            />

            <p className="k-new__gate k-mono">
              Secret backends, deployment policy and cluster targeting are not
              offered here: kelson validates those fields and renders nothing
              for them, so it rejects them outright (#141). They arrive with the
              milestones that implement them. Data services do render now (#89)
              and are one of the kinds “Add component” offers on the project's
              page, once this project exists.
            </p>
          </div>
        </details>

        {mapped.general.length > 0 ? (
          <ErrorPanel
            title="The server would not accept this spec"
            errors={mapped.general}
          />
        ) : null}

        {check.error !== undefined ? (
          <ErrorPanel title="Could not check the spec" error={check.error} />
        ) : null}

        {checked === undefined ? (
          <div className="k-deploy__confirm k-new__submit">
            <button
              type="submit"
              className="k-button k-button--primary k-button--wide"
              disabled={check.running}
            >
              {check.running ? "Checking…" : "Check and preview"}
            </button>
            <span className="k-mono k-deploy__note">
              validates and renders every environment · stores nothing
            </span>
          </div>
        ) : null}
      </form>

      {checked !== undefined ? (
        <Preview
          documents={checked.documents}
          expected={checked.expected}
          running={store.running}
          onCreate={createIt}
          error={taken ? undefined : store.error}
        />
      ) : null}
    </>
  );
}

function specDocuments(built: SpecText) {
  return {
    project: ENCODER.encode(built.project),
    environments: { [built.environmentName]: ENCODER.encode(built.environment) },
  };
}

/**
 * A create carries no version, so store/version-conflict can only mean the
 * project is already there (internal/serverstate: an empty expected version
 * against an existing object is refused rather than treated as an overwrite).
 */
function isNameTaken(err: unknown): boolean {
  return isVersionConflict(err);
}

function Field({
  label,
  value,
  onChange,
  placeholder,
  note,
  problem,
  errors,
  narrow,
  children,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  note?: string;
  problem: string | undefined;
  errors: WireError[];
  narrow?: boolean;
  children?: ReactNode;
}) {
  // htmlFor/id rather than a wrapping label: the field carries a hint, any
  // number of server findings and sometimes a link, and none of that belongs
  // in the input's accessible name — nor inside a label, which would swallow
  // the link's click.
  const id = useId();
  const bad = problem !== undefined || errors.length > 0;
  return (
    <div className={narrow ? "k-field k-field--narrow" : "k-field"}>
      <label className="k-eyebrow" htmlFor={id}>
        {label}
      </label>
      <input
        id={id}
        className="k-input k-mono"
        value={value}
        placeholder={placeholder}
        aria-invalid={bad ? true : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {problem !== undefined ? (
        <span className="k-field__problem k-mono" role="alert">
          {problem}
        </span>
      ) : null}
      <FieldErrors errors={errors} />
      {children}
      {note !== undefined ? (
        <span className="k-field__note k-mono">{note}</span>
      ) : null}
    </div>
  );
}

/**
 * A structured error shown where it happened. The code and the remediation come
 * along: the code is what an agent branches on and the remediation is the CLI's
 * own "fix:" line, and neither is worth losing just because there is an input
 * to point at.
 */
function FieldErrors({ errors }: { errors: WireError[] }) {
  if (errors.length === 0) return null;
  return (
    <>
      {errors.map((e, i) => (
        <span
          key={`${e.code}:${e.field}:${i}`}
          className="k-field__problem k-mono"
          role="alert"
        >
          <code className="k-field__code">{e.code}</code> {e.message}
          {e.remediation ? (
            <>
              {" "}
              <span className="k-field__fix">fix: {e.remediation}</span>
            </>
          ) : null}
        </span>
      ))}
    </>
  );
}

function Rows({
  label,
  note,
  addLabel,
  values,
  placeholder,
  problem,
  errors,
  onChange,
}: {
  label: string;
  note: string;
  addLabel: string;
  values: string[];
  placeholder: string;
  problem: string | undefined;
  errors: WireError[];
  onChange: (values: string[]) => void;
}) {
  return (
    <div className="k-field">
      <span className="k-eyebrow">{label}</span>
      {values.map((value, i) => (
        <div className="k-new__pair" key={i}>
          <input
            className="k-input k-mono"
            aria-label={`${label} ${i + 1}`}
            value={value}
            placeholder={placeholder}
            onChange={(e) =>
              onChange(values.map((v, j) => (j === i ? e.target.value : v)))
            }
          />
          <button
            type="button"
            className="k-button"
            onClick={() => onChange(values.filter((_, j) => j !== i))}
          >
            Remove
          </button>
        </div>
      ))}
      <div className="k-actions">
        <button
          type="button"
          className="k-button"
          onClick={() => onChange([...values, ""])}
        >
          {addLabel}
        </button>
      </div>
      {problem !== undefined ? (
        <span className="k-field__problem k-mono" role="alert">
          {problem}
        </span>
      ) : null}
      <FieldErrors errors={errors} />
      <span className="k-field__note k-mono">{note}</span>
    </div>
  );
}

/**
 * The env rows, with the reference forms ADR-0018 gave an author a spelling for.
 *
 * "secret ref" is offered here rather than left to the YAML tab because the
 * variable that sends someone looking for one — a Stripe key, an SMTP password
 * — is exactly the variable this form used to reject with `secret/literal` and
 * no next step. Picking it emits `{ secret: <name>, key: <key> }`, which is a
 * pointer; the credential itself is written in the Secrets panel on the
 * project's page, or with `kelson secret set`, and the note below says so
 * because the Secret does not exist yet at create time.
 */
function EnvRows({
  env,
  onChange,
  problemFor,
  errorsFor,
}: {
  env: EnvVar[];
  onChange: (env: EnvVar[]) => void;
  problemFor: (field: FieldKey) => string | undefined;
  errorsFor: (field: FieldKey) => WireError[];
}) {
  return (
    <div className="k-field">
      <span className="k-eyebrow">Environment variables</span>
      {env.map((row, i) => (
        <div key={i}>
          <div className="k-new__pair">
            <input
              className="k-input k-mono"
              aria-label={`Variable ${i + 1} name`}
              value={row.key}
              placeholder="LOG_LEVEL"
              onChange={(e) =>
                onChange(env.map((r, j) => (j === i ? { ...r, key: e.target.value } : r)))
              }
            />
            <EnvValueFields
              name={`Variable ${i + 1}`}
              value={row.value}
              onChange={(value) =>
                onChange(env.map((r, j) => (j === i ? { ...r, value } : r)))
              }
            />
            <button
              type="button"
              className="k-button"
              onClick={() => onChange(env.filter((_, j) => j !== i))}
            >
              Remove
            </button>
          </div>
          {row.value.kind === "plain" ? null : (
            <span className="k-field__note k-mono">{envValueNote(row.value)}</span>
          )}
        </div>
      ))}
      {env.map((row, i) => {
        const name = row.key.trim();
        if (name === "") return null;
        const problem = problemFor(`env:${name}`);
        return (
          <span key={i}>
            {problem !== undefined ? (
              <span className="k-field__problem k-mono" role="alert">
                {problem}
              </span>
            ) : null}
            <FieldErrors errors={errorsFor(`env:${name}`)} />
          </span>
        );
      })}
      <div className="k-actions">
        <button
          type="button"
          className="k-button"
          onClick={() => onChange([...env, { key: "", value: plainEnv("") }])}
        >
          Add variable
        </button>
      </div>
      <span className="k-field__note k-mono">
        a plain value is stored in the spec as written, so it is never a
        credential (ADR-0009) — a secret-shaped name is rejected. Choose “secret
        ref” for a credential: it writes {"{ secret: <name>, key: <key> }"} and
        the value goes into the Secret itself, in the project's Secrets panel or
        with `kelson secret set`.
      </span>
    </div>
  );
}

/**
 * What will be stored, before it is stored.
 *
 * The documents are shown as the bytes that go over the wire, because that is
 * what the store keeps: the server holds the authored document, not a
 * normalized re-serialisation of it, and this screen is where the user first
 * meets the file they now own.
 */
/**
 * The source of the image, as a choice rather than a mode nobody can see.
 *
 * Radios and not a segmented control or a select: there are two options, both
 * fit on the line, and which one is active decides which fields exist below —
 * that is exactly the case a radio group already communicates to a screen
 * reader without any help.
 */
function SourceToggle({
  mode,
  onChange,
}: {
  mode: SourceMode;
  onChange: (mode: SourceMode) => void;
}) {
  return (
    <fieldset className="k-panel k-new__source">
      <legend className="k-eyebrow">Where the image comes from</legend>
      <label className="k-check k-mono">
        <input
          type="radio"
          name="source-mode"
          checked={mode === "image"}
          onChange={() => onChange("image")}
        />
        From image
      </label>
      <label className="k-check k-mono">
        <input
          type="radio"
          name="source-mode"
          checked={mode === "git"}
          onChange={() => onChange("git")}
        />
        From Git repository
      </label>
      <span className="k-field__note k-mono">
        {mode === "git"
          ? "kelson clones it and builds the image in the cluster, rootless, then pushes it to the server's registry — how it is built is the choice below"
          : "an image someone or something else already built and pushed"}
      </span>
    </fieldset>
  );
}

/**
 * How the repository becomes an image (ADR-0010).
 *
 * Both sentences are on screen, not only the selected one: this is the one
 * choice on this page a reader may genuinely not know the answer to, and what
 * decides it is what each strategy needs from their repository. Hiding the
 * other half behind the radio would make it a guess.
 *
 * `dockerfile` is the default because it is what this form wrote before there
 * was a choice — ADR-0010's own zero-config default is buildpacks, and quietly
 * adopting it here would build everybody's next project a different way.
 * `auto` is not offered at all: see the module comment.
 */
function StrategyToggle({
  strategy,
  onChange,
}: {
  strategy: BuildStrategy;
  onChange: (strategy: BuildStrategy) => void;
}) {
  return (
    <fieldset className="k-panel k-new__strategy">
      <legend className="k-eyebrow">How the image is built</legend>
      <div className="k-new__strategy-option">
        <label className="k-check k-mono">
          <input
            type="radio"
            name="build-strategy"
            checked={strategy === "dockerfile"}
            onChange={() => onChange("dockerfile")}
          />
          Dockerfile
        </label>
        <span className="k-field__note k-mono">
          the repository has a Dockerfile at its root, and kelson builds that
        </span>
      </div>
      <div className="k-new__strategy-option">
        <label className="k-check k-mono">
          <input
            type="radio"
            name="build-strategy"
            checked={strategy === "buildpacks"}
            onChange={() => onChange("buildpacks")}
          />
          Buildpacks
        </label>
        <span className="k-field__note k-mono">
          no Dockerfile: the Cloud Native Buildpacks lifecycle detects the
          language and builds a rootless image
        </span>
      </div>
      <span className="k-field__note k-mono k-new__strategy-note">
        the answer is written into the spec as `spec.build.strategy`. ADR-0010's
        own default — `auto`, look at the tree and decide — is not offered here,
        because the tree only exists inside the build pod and the server has
        nothing to look at until it does (#50).
      </span>
    </fieldset>
  );
}

function Preview({
  documents,
  expected,
  running,
  onCreate,
  error,
}: {
  documents: SpecText;
  expected: readonly WireError[];
  running: boolean;
  onCreate: () => void;
  error: unknown;
}) {
  return (
    <section className="k-section">
      <div className="k-eyebrow">What will be stored</div>
      <div className="k-section__body k-docs">
        <Disclosure
          summary="Project"
          meta={`${documents.project.length} bytes`}
          open
        >
          <YamlBlock bytes={documents.project} />
        </Disclosure>
        <Disclosure
          summary={`Environment · ${documents.environmentName}`}
          meta={`${documents.environment.length} bytes`}
        >
          <YamlBlock bytes={documents.environment} />
        </Disclosure>

        {expected.length > 0 ? (
          <div className="k-new__expected" role="note">
            <span className="k-new__expected-title">
              The check could not render this yet, and that is expected
            </span>
            {expected.map((e, i) => (
              <span className="k-mono" key={`${e.code}:${i}`}>
                <code className="k-field__code">{e.code}</code> {e.message}
              </span>
            ))}
            <span className="k-mono">
              storing a spec does not render it, so this create succeeds — the
              image arrives when a build produces one, which is the next step
              after Create.
            </span>
          </div>
        ) : null}

        {error !== undefined ? (
          <ErrorPanel title="Could not store the spec" error={error} />
        ) : null}

        <div className="k-deploy__confirm k-new__submit">
          <button
            type="button"
            className="k-button k-button--primary k-button--wide"
            onClick={onCreate}
            disabled={running}
          >
            {running ? "Creating…" : `Create ${documents.projectName}`}
          </button>
          <span className="k-mono k-deploy__note">
            the check passed · nothing has been written yet, and nothing is
            deployed by creating
          </span>
        </div>
      </div>
    </section>
  );
}

function Stored({ created }: { created: Created }) {
  const base = `/projects/${encodeURIComponent(created.project)}`;
  const deploy = `${base}/${encodeURIComponent(created.environment)}/deploy`;
  return (
    <>
      <div className="k-page-head">
        <h1>{created.project}</h1>
      </div>
      <div className="k-page-sub">
        <Link to="/projects">← all projects</Link>
        <span>·</span>
        <span className="k-chip k-mono">{created.environment}</span>
      </div>

      <div className="k-settled" role="status">
        <div className="k-settled__head">
          <StatusPill status="synced" label="stored" />
          <span className="k-settled__title">The spec is stored</span>
        </div>
        <span className="k-mono">
          {created.buildsFromSource
            ? "nothing has been built and nothing has been applied to a cluster — building comes first, because this project has no image until a build produces one"
            : "nothing has been applied to a cluster yet — deploying is the next, separate step"}
        </span>
        <div className="k-actions">
          {created.buildsFromSource ? null : (
            <Link className="k-button k-button--primary" to={deploy}>
              Deploy now
            </Link>
          )}
          <Link className="k-button" to={base}>
            View project
          </Link>
        </div>
      </div>

      {created.buildsFromSource ? (
        <BuildAndDeploy created={created} deployPath={deploy} />
      ) : null}
    </>
  );
}

/**
 * The build, watched.
 *
 * BuildService.Build streams `Started` (the resolved strategy, the destination
 * and the commit — all settled facts by the time it is sent), then the build's
 * own output as raw byte chunks, then `Finished` with a digest-pinned
 * reference. A failure is a ConnectRPC error rather than an event, because a
 * failed build produced no image; it renders in the same error panel every
 * other structured failure does, so the `build/*` code reaches the reader.
 *
 * Finishing does not navigate on its own. It offers the deploy with the built
 * reference attached, which is the same separation the create step makes:
 * building an image and applying it to a cluster are different acts with
 * different blast radii, and auto-navigating would also throw away the log
 * someone may be reading.
 */
function BuildAndDeploy({
  created,
  deployPath,
}: {
  created: Created;
  deployPath: string;
}) {
  const clients = useClients();
  const run = useRun();
  const [started, setStarted] = useState<BuildResponse_Started | undefined>(undefined);
  const [finished, setFinished] = useState<BuildResponse_Finished | undefined>(undefined);
  const [log, setLog] = useState("");
  const [pinned, setPinned] = useState(true);
  const box = useRef<HTMLDivElement | null>(null);

  // Auto-scroll only while the reader is at the bottom, exactly as the log
  // screen does: scrolling up is how someone reads a line that went past, and
  // yanking them back down on the next chunk makes a live tail unreadable.
  useEffect(() => {
    const el = box.current;
    if (el && pinned) el.scrollTop = el.scrollHeight;
  }, [log, pinned]);

  const onScroll = useCallback(() => {
    const el = box.current;
    if (!el) return;
    setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 24);
  }, []);

  const build = useCallback(() => {
    setStarted(undefined);
    setFinished(undefined);
    setLog("");
    setPinned(true);
    run.start(async (signal) => {
      // One decoder for the whole stream: a chunk boundary is a transport
      // boundary and may fall inside a multi-byte character, which is what
      // `stream: true` carries across calls. Decoding each chunk on its own
      // would put replacement characters in the log.
      const decoder = new TextDecoder();
      for await (const res of clients.build.build(
        {
          spec: { spec: { case: "project", value: created.project } },
          environment: created.environment,
        },
        { signal },
      )) {
        const event = res.event;
        switch (event.case) {
          case "started":
            setStarted(event.value);
            break;
          case "log":
            setLog((prev) => prev + decoder.decode(event.value.chunk, { stream: true }));
            break;
          case "finished":
            setFinished(event.value);
            break;
        }
      }
    });
  }, [run, clients, created]);

  return (
    <section className="k-section">
      <div className="k-eyebrow">Build the image</div>
      <div className="k-section__body">
        <div className="k-deploy__confirm">
          <button
            type="button"
            className="k-button k-button--primary k-button--wide"
            onClick={build}
            disabled={run.running}
          >
            {run.running ? "Building…" : `Build ${created.project}`}
          </button>
          {run.running ? (
            <button type="button" className="k-button" onClick={run.stop}>
              Stop watching
            </button>
          ) : null}
          <span className="k-mono k-deploy__note">
            a rootless Job in the cluster, running the strategy the spec asked
            for · the registry and the push credential are the server's
            configuration, not this form's · stopping stops the watching, not
            the Job
          </span>
        </div>

        {started !== undefined ? (
          <div className="k-panel k-kv">
            <span className="k-kv__key">strategy</span>
            <span className="k-mono">{started.strategy}</span>
            <span className="k-kv__key">repository</span>
            <span className="k-mono">{started.imageRepository}</span>
            <span className="k-kv__key">tag</span>
            <span className="k-mono">{started.tag}</span>
            <span className="k-kv__key">revision</span>
            <span className="k-mono">{started.revision}</span>
          </div>
        ) : null}

        {log !== "" ? (
          <div className="k-build__box" ref={box} onScroll={onScroll} role="log">
            <pre className="k-build__log">{log}</pre>
          </div>
        ) : null}

        {run.error !== undefined ? (
          <ErrorPanel title="The build failed" error={run.error} />
        ) : null}

        {finished !== undefined ? (
          <div className="k-settled" role="status">
            <div className="k-settled__head">
              <StatusPill status="synced" label="built" />
              <span className="k-settled__title">The image is pushed</span>
            </div>
            <span className="k-mono">
              <Copyable value={finished.reference} />
            </span>
            <span className="k-mono">
              pinned by digest, so the deploy below and any repeat of it get the
              same bytes
            </span>
            <div className="k-actions">
              <Link
                className="k-button k-button--primary"
                to={`${deployPath}?image=${encodeURIComponent(finished.reference)}`}
              >
                Deploy this image
              </Link>
            </div>
          </div>
        ) : null}
      </div>
    </section>
  );
}
