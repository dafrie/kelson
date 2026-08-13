import { useCallback, useId, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { useClients } from "../api/data";
import { isVersionConflict } from "../api/errors";
import { useRun } from "../api/stream";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import { Disclosure, YamlBlock } from "../components/Disclosure";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import {
  buildDocuments,
  defaultNamespace,
  EMPTY_FORM,
  formProblems,
  mapErrors,
  workloadKind,
  type FieldKey,
  type NewAppForm,
  type SpecText,
} from "../spec/documents";

/**
 * Creating a component: three fields, then a preview, then a store.
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
 * arriving one step later; those components are written on the YAML tab, and
 * the edit form's byte guard sends any document containing one there.
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
}

interface Created {
  project: string;
  environment: string;
}

export function NewAppPage() {
  const clients = useClients();
  const [form, setForm] = useState<NewAppForm>(EMPTY_FORM);
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
  const update = useCallback(
    <K extends keyof NewAppForm>(key: K, value: NewAppForm[K], field?: FieldKey) => {
      setForm((prev) => ({ ...prev, [key]: value }));
      if (field !== undefined) setTouched((prev) => ({ ...prev, [field]: true }));
      setWire([]);
      setChecked(undefined);
    },
    [],
  );

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
      setWire(res.errors);
      setChecked(
        res.errors.length === 0
          ? { documents, idempotencyKey: crypto.randomUUID() }
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
        <h1>New app</h1>
      </div>
      <div className="k-page-sub">
        <Link to="/apps">← all apps</Link>
        <span>·</span>
        <span>a name, an image, a port</span>
      </div>

      <form
        className="k-new"
        onSubmit={(e) => {
          e.preventDefault();
          validate();
        }}
      >
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
                <Link to={`/apps/${encodeURIComponent(form.project.trim())}`}>
                  open it
                </Link>{" "}
                or choose another name
              </span>
            ) : null}
          </Field>

          <Field
            label="Image"
            value={form.image}
            onChange={(v) => update("image", v, "image")}
            placeholder="ghcr.io/acme/hello:1.4.2"
            problem={problemFor("image")}
            errors={errorsFor("image")}
            note="a pre-built image reference; building from source is not wired yet"
          />

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
              errorsFor={errorsFor}
            />

            <p className="k-new__gate k-mono">
              Secret backends, deployment policy and cluster targeting are not
              offered here: kelson validates those fields and renders nothing
              for them, so it rejects them outright (#141). They arrive with the
              milestones that implement them. Data services do render now (#89);
              declare them in the spec editor until this form grows a control
              for them.
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

function EnvRows({
  env,
  onChange,
  errorsFor,
}: {
  env: { key: string; value: string }[];
  onChange: (env: { key: string; value: string }[]) => void;
  errorsFor: (field: FieldKey) => WireError[];
}) {
  return (
    <div className="k-field">
      <span className="k-eyebrow">Environment variables</span>
      {env.map((row, i) => (
        <div className="k-new__pair" key={i}>
          <input
            className="k-input k-mono"
            aria-label={`Variable ${i + 1} name`}
            value={row.key}
            placeholder="LOG_LEVEL"
            onChange={(e) =>
              onChange(env.map((r, j) => (j === i ? { ...r, key: e.target.value } : r)))
            }
          />
          <input
            className="k-input k-mono"
            aria-label={`Variable ${i + 1} value`}
            value={row.value}
            placeholder="info"
            onChange={(e) =>
              onChange(env.map((r, j) => (j === i ? { ...r, value: e.target.value } : r)))
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
      ))}
      {env.map((row, i) =>
        row.key.trim() === "" ? null : (
          <FieldErrors key={i} errors={errorsFor(`env:${row.key.trim()}`)} />
        ),
      )}
      <div className="k-actions">
        <button
          type="button"
          className="k-button"
          onClick={() => onChange([...env, { key: "", value: "" }])}
        >
          Add variable
        </button>
      </div>
      <span className="k-field__note k-mono">
        plain values only — the spec carries references, never credentials
        (ADR-0009), and a secret-shaped name is rejected
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
function Preview({
  documents,
  running,
  onCreate,
  error,
}: {
  documents: SpecText;
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
  const base = `/apps/${encodeURIComponent(created.project)}`;
  return (
    <>
      <div className="k-page-head">
        <h1>{created.project}</h1>
      </div>
      <div className="k-page-sub">
        <Link to="/apps">← all apps</Link>
        <span>·</span>
        <span className="k-chip k-mono">{created.environment}</span>
      </div>

      <div className="k-settled" role="status">
        <div className="k-settled__head">
          <StatusPill status="synced" label="stored" />
          <span className="k-settled__title">The spec is stored</span>
        </div>
        <span className="k-mono">
          nothing has been applied to a cluster yet — deploying is the next,
          separate step
        </span>
        <div className="k-actions">
          <Link
            className="k-button k-button--primary"
            to={`${base}/${encodeURIComponent(created.environment)}/deploy`}
          >
            Deploy now
          </Link>
          <Link className="k-button" to={base}>
            View app
          </Link>
        </div>
      </div>
    </>
  );
}
