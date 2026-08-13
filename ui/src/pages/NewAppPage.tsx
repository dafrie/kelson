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

import { useClients } from "../api/data";
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
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import {
  buildDocuments,
  defaultNamespace,
  EMPTY_FORM,
  formProblems,
  mapErrors,
  splitFindings,
  workloadKind,
  type FieldKey,
  type NewAppForm,
  type SourceMode,
  type SpecText,
} from "../spec/documents";

/**
 * Creating a component: three fields, then a preview, then a store.
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
 * The build strategy is not asked for either, and that one is a limitation
 * rather than a principle — see BUILD_STRATEGY in ../spec/documents.
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
        <h1>New app</h1>
      </div>
      <div className="k-page-sub">
        <Link to="/apps">← all apps</Link>
        <span>·</span>
        <span>
          {form.sourceMode === "git"
            ? "a name, a repository, a port"
            : "a name, an image, a port"}
        </span>
      </div>

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
                onChange={(v) => update("git", v, "git")}
                placeholder="https://github.com/acme/hello"
                problem={problemFor("git")}
                errors={errorsFor("git")}
                note="cloned inside the cluster at build time; a private repository needs the server's git credential"
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
          ? "kelson builds it in the cluster with rootless BuildKit and pushes to the server's registry — the strategy is `dockerfile`, so the repository needs a Dockerfile at its root"
          : "an image someone or something else already built and pushed"}
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
  const base = `/apps/${encodeURIComponent(created.project)}`;
  const deploy = `${base}/${encodeURIComponent(created.environment)}/deploy`;
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
            View app
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
            a rootless BuildKit Job in the cluster · the registry and the push
            credential are the server's configuration, not this form's · stopping
            stops the watching, not the Job
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
