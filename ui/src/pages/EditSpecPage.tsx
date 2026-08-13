import { useCallback, useEffect, useId, useMemo, useState } from "react";
import { Link, useBlocker, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { isVersionConflict } from "../api/errors";
import { useRun } from "../api/stream";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import type {
  Error as WireError,
  SpecDocuments,
} from "../gen/kelson/v1alpha1/common_pb";
import type { DiffResponse } from "../gen/kelson/v1alpha1/render_pb";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { EmptyState, LoadingState } from "../components/States";
import { DiffView } from "../diff/DiffView";
import { decodeDiff, type Diff } from "../diff/parse";
import {
  appWorkload,
  isRebuildable,
  mapEditErrors,
  readSpec,
  sameText,
  writeSpec,
  type AppEdit,
  type EditFieldKey,
  type EnvironmentEdit,
  type SpecEdit,
  type SpecTextSet,
} from "../spec/edit";
import type { EnvVar } from "../spec/documents";

/**
 * Editing a stored spec: environment variables and configuration (#65).
 *
 * Two tabs, because the store is byte-faithful and the spec is the user's
 * document (ADR-0013 §1), and those two facts pull in opposite directions.
 *
 *   - The **form** is the common case: change an image tag, add a variable,
 *     raise the replica count. It exists only for documents this UI can rebuild
 *     byte-identically (src/spec/edit.ts) — otherwise editing through fields
 *     would silently rewrite someone's file, dropping their comments and their
 *     key order. When it cannot, it says so and goes read-only rather than
 *     offering an edit that costs the reader their formatting.
 *   - The **YAML tab** is always there and always complete. It is the answer to
 *     everything the form does not model, and the only honest place to edit a
 *     hand-written document.
 *
 * Both go through the same pipeline, which is the API's own two rungs plus the
 * one thing an *edit* needs that a create does not: PutSpec at dry_run=RENDER
 * validates and stores nothing; RenderService.Diff against the currently stored
 * documents says what the change actually does; then the real PutSpec carries
 * the version from GetSpec, so a spec that changed underneath the editor is
 * refused rather than overwritten.
 */

const ENCODER = new TextEncoder();
const DECODER = new TextDecoder();

interface EnvDiff {
  environment: string;
  response?: DiffResponse;
  error?: unknown;
}

interface Checked {
  text: SpecTextSet;
  /** Minted once per checked document set, so a retried Save is a replay. */
  idempotencyKey: string;
  diffs: EnvDiff[];
}

export function EditSpecPage() {
  const { project = "" } = useParams();
  const clients = useClients();
  const spec = useAsync(
    (signal) => clients.spec.getSpec({ project }, { signal }),
    [clients, project],
  );

  const stored = spec.data?.spec;
  const documents = stored?.documents;

  return (
    <>
      <div className="k-page-head">
        <h1>{project}</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/apps/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span>edit configuration</span>
        <span>·</span>
        <span>version {stored?.version || "—"}</span>
      </div>

      {spec.loading && spec.data === undefined ? (
        <LoadingState what="the stored spec" />
      ) : null}

      {spec.error !== undefined ? (
        <ErrorPanel title={`Cannot read the spec for ${project}`} error={spec.error} />
      ) : null}

      {spec.data !== undefined && documents === undefined ? (
        <EmptyState title="This project has no stored documents">
          There is nothing to edit. A spec is stored by the create flow or by
          `kelson spec put`.
        </EmptyState>
      ) : null}

      {documents !== undefined ? (
        // Re-keyed on the version: a reload after a conflict brings different
        // bytes, and the editor starts from them rather than merging.
        <Editor
          key={stored?.version ?? ""}
          project={project}
          version={stored?.version ?? ""}
          stored={toText(documents)}
          onReload={spec.reload}
        />
      ) : null}
    </>
  );
}

function Editor({
  project,
  version,
  stored,
  onReload,
}: {
  project: string;
  version: string;
  stored: SpecTextSet;
  onReload: () => void;
}) {
  const clients = useClients();
  const [text, setText] = useState<SpecTextSet>(stored);
  /**
   * What the form shows, alongside the text it writes.
   *
   * The documents are the source of truth — they are what is stored and what
   * the YAML tab edits — but they cannot hold a half-typed row: a variable with
   * no name yet is not `"": ""` in a spec, it is a row the user is still
   * filling in. So the form keeps its own state and the text is derived from
   * it, and a YAML edit re-derives the form from the text.
   */
  const [draft, setDraft] = useState<SpecEdit | undefined>(() => readSpec(stored));
  const [tab, setTab] = useState<"form" | "yaml">(() =>
    // A hand-edited document opens on the tab that can actually edit it.
    isRebuildable(stored) ? "form" : "yaml",
  );
  const [openDoc, setOpenDoc] = useState("project");
  const [wire, setWire] = useState<readonly WireError[]>([]);
  const [checked, setChecked] = useState<Checked | undefined>(undefined);
  const [conflict, setConflict] = useState(false);
  const [saved, setSaved] = useState(false);
  const check = useRun();
  const save = useRun();

  const rebuildable = useMemo(() => isRebuildable(text), [text]);
  const dirty = !sameText(text, stored);
  const mapped = useMemo(() => mapEditErrors(wire), [wire]);
  const environments = useMemo(
    () => Object.keys(text.environments).sort(),
    [text.environments],
  );

  useUnsavedChangesGuard(dirty && !saved);
  const blocker = useBlocker(dirty && !saved);

  /**
   * Every edit drops the previous answer (the #63 pattern): a preview and a
   * diff are statements about bytes that no longer exist once a key is pressed,
   * and Save is what they authorise.
   */
  const replace = useCallback((next: SpecTextSet) => {
    setText(next);
    setWire([]);
    setChecked(undefined);
    setConflict(false);
  }, []);

  const onEdit = useCallback(
    (next: SpecEdit) => {
      setDraft(next);
      replace(writeSpec(next));
    },
    [replace],
  );

  const onYaml = useCallback(
    (name: string, value: string) => {
      const next =
        name === "project"
          ? { ...text, project: value }
          : { ...text, environments: { ...text.environments, [name]: value } };
      setDraft(readSpec(next));
      replace(next);
    },
    [replace, text],
  );

  const validate = useCallback(() => {
    check.start(async (signal) => {
      const res = await clients.spec.putSpec(
        { documents: toWire(text), dryRun: DryRun.RENDER },
        { signal },
      );
      setWire(res.errors);
      if (res.errors.length > 0) {
        setChecked(undefined);
        return;
      }
      // What the change does, per environment. Every environment is shown
      // rather than the selected one: a Project edit reaches all of them, and
      // a destructive change hidden behind an unselected tab is the failure
      // this preview exists to prevent.
      const diffs: EnvDiff[] = [];
      for (const environment of environments) {
        try {
          const response = await clients.render.diff(
            {
              spec: { spec: { case: "documents", value: toWire(text) } },
              environment,
              from: toWire(stored),
            },
            { signal },
          );
          diffs.push({ environment, response });
        } catch (error) {
          diffs.push({ environment, error });
        }
      }
      setChecked({ text, idempotencyKey: crypto.randomUUID(), diffs });
    });
  }, [check, clients, environments, stored, text]);

  const store = useCallback(
    (force: boolean) => {
      if (checked === undefined) return;
      save.start(async (signal) => {
        try {
          await clients.spec.putSpec(
            {
              documents: toWire(checked.text),
              // The version goes along even when forcing: force is what makes
              // the store ignore it (serverstate.PutOptions), and a write that
              // dropped it would be indistinguishable from a create.
              version,
              force,
              idempotencyKey: checked.idempotencyKey,
            },
            { signal },
          );
        } catch (err) {
          if (isVersionConflict(err)) {
            setConflict(true);
            return;
          }
          throw err;
        }
        setConflict(false);
        setSaved(true);
      });
    },
    [checked, clients, save, version],
  );

  if (saved) return <Saved project={project} />;

  const errorsFor = (field: EditFieldKey): WireError[] =>
    mapped.byField.get(field) ?? [];

  return (
    <>
      {blocker.state === "blocked" ? (
        <div className="k-edit__guard" role="alert">
          <span className="k-edit__guard-title">
            This spec has unsaved changes
          </span>
          <span className="k-mono">
            leaving now discards them — nothing has been written to the store
          </span>
          <div className="k-actions">
            <button
              type="button"
              className="k-button k-button--primary"
              onClick={() => blocker.reset?.()}
            >
              Stay on this page
            </button>
            <button
              type="button"
              className="k-button k-button--danger"
              onClick={() => blocker.proceed?.()}
            >
              Discard and leave
            </button>
          </div>
        </div>
      ) : null}

      <nav className="k-tabs" aria-label="Editing mode">
        <button
          type="button"
          className={tab === "form" ? "k-tab k-tab--active" : "k-tab"}
          aria-current={tab === "form" ? "true" : undefined}
          onClick={() => setTab("form")}
        >
          Form
        </button>
        <button
          type="button"
          className={tab === "yaml" ? "k-tab k-tab--active" : "k-tab"}
          aria-current={tab === "yaml" ? "true" : undefined}
          onClick={() => setTab("yaml")}
        >
          YAML
        </button>
      </nav>

      {tab === "form" ? (
        draft === undefined ? (
          <EmptyState title="This document cannot be shown as a form">
            It is outside the shape the form reads. The YAML tab edits it with
            its formatting intact.
          </EmptyState>
        ) : (
          <>
            {rebuildable ? null : (
              <p className="k-note k-edit__readonly" role="status">
                This document was hand-edited — use the YAML tab to keep its
                formatting. Editing through these fields would rewrite the file
                from what the form understands, dropping comments and key order
                the store is keeping for you (ADR-0013 §1). The fields below are
                shown read-only.
              </p>
            )}
            <SpecForm
              edit={draft}
              readOnly={!rebuildable}
              onChange={onEdit}
              errorsFor={errorsFor}
            />
          </>
        )
      ) : (
        <YamlEditor
          text={text}
          open={openDoc}
          onOpen={setOpenDoc}
          onChange={onYaml}
          errors={wire}
        />
      )}

      {tab === "form" && mapped.general.length > 0 ? (
        <ErrorPanel
          title="The server would not accept this spec"
          errors={mapped.general}
        />
      ) : null}

      {check.error !== undefined ? (
        <ErrorPanel title="Could not check the spec" error={check.error} />
      ) : null}

      <div className="k-deploy__confirm k-edit__actions">
        <button
          type="button"
          className="k-button"
          onClick={validate}
          disabled={check.running || !dirty}
        >
          {check.running ? "Checking…" : "Check and preview the diff"}
        </button>
        <span className="k-mono k-deploy__note">
          {dirty
            ? "validates and renders every environment · stores nothing"
            : "nothing has changed yet"}
        </span>
      </div>

      {checked !== undefined ? (
        <>
          <ChangePreview diffs={checked.diffs} />

          {conflict ? (
            <ConflictState
              project={project}
              text={checked.text}
              running={save.running}
              onReload={onReload}
              onForce={() => store(true)}
            />
          ) : null}

          {save.error !== undefined ? (
            <ErrorPanel title="Could not store the spec" error={save.error} />
          ) : null}

          <div className="k-deploy__confirm k-edit__actions">
            <button
              type="button"
              className="k-button k-button--primary k-button--wide"
              onClick={() => store(false)}
              disabled={save.running || conflict}
            >
              {save.running ? "Saving…" : `Save ${project}`}
            </button>
            <span className="k-mono k-deploy__note">
              writes against version {version || "—"} · nothing is deployed by
              saving
            </span>
          </div>
        </>
      ) : (
        <div className="k-deploy__confirm k-edit__actions">
          <button type="button" className="k-button k-button--wide" disabled>
            Save {project}
          </button>
          <span className="k-mono k-deploy__note">
            check the spec first — Save writes what the check and the diff were
            about
          </span>
        </div>
      )}
    </>
  );
}

/**
 * The browser's own guard, for the half `useBlocker` cannot see: a reload, a
 * closed tab, a typed URL. React Router's blocker owns client-side navigation
 * and this owns everything that leaves the document entirely; neither replaces
 * the other.
 */
function useUnsavedChangesGuard(dirty: boolean) {
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);
}

/* --------------------------------------------------------------- the form */

function SpecForm({
  edit,
  readOnly,
  onChange,
  errorsFor,
}: {
  edit: SpecEdit;
  readOnly: boolean;
  onChange: (next: SpecEdit) => void;
  errorsFor: (field: EditFieldKey) => WireError[];
}) {
  const setProject = (patch: Partial<SpecEdit["project"]>) =>
    onChange({ ...edit, project: { ...edit.project, ...patch } });

  const setApp = (index: number, patch: Partial<AppEdit>) =>
    onChange({
      ...edit,
      project: {
        ...edit.project,
        applications: edit.project.applications.map((a, i) =>
          i === index ? { ...a, ...patch } : a,
        ),
      },
    });

  const setEnvironment = (index: number, patch: Partial<EnvironmentEdit>) =>
    onChange({
      ...edit,
      environments: edit.environments.map((e, i) =>
        i === index ? { ...e, ...patch } : e,
      ),
    });

  return (
    <div className="k-new k-edit__form">
      <section className="k-section">
        <div className="k-eyebrow">Project</div>
        <div className="k-section__body k-edit__group">
          <div className="k-new__row">
            <EditField
              label="Image"
              value={edit.project.image}
              onChange={(v) => setProject({ image: v })}
              readOnly={readOnly}
              placeholder="ghcr.io/acme/hello:1.4.2"
              errors={errorsFor("project.image")}
              note="shared by every application; an application's own image wins (rule P3)"
            />
          </div>
          <EnvRows
            label="Environment variables"
            env={edit.project.env}
            readOnly={readOnly}
            onChange={(env) => setProject({ env })}
            errorsFor={(name) => errorsFor(`project.env.${name}`)}
            note="shared by every application (rule P1); plain values only — the spec carries references, never credentials (ADR-0009)"
          />
        </div>
      </section>

      {edit.project.applications.map((app, i) => (
        <ApplicationForm
          key={`${i}:${app.name}`}
          app={app}
          index={i}
          readOnly={readOnly}
          onChange={(patch) => setApp(i, patch)}
          errorsFor={errorsFor}
        />
      ))}

      {edit.environments.map((env, i) => (
        <section className="k-section" key={env.name}>
          <div className="k-eyebrow">
            Environment · <span className="k-mono">{env.name}</span>
          </div>
          <div className="k-section__body k-edit__group">
            <div className="k-new__row">
              <EditField
                label="Namespace"
                value={env.namespace}
                onChange={(v) => setEnvironment(i, { namespace: v })}
                readOnly={readOnly}
                placeholder={`${env.project}-${env.name}`}
                errors={errorsFor(`environment.${env.name}.namespace`)}
                note="override the model's default; blank leaves the default in place"
              />
            </div>
          </div>
        </section>
      ))}
    </div>
  );
}

function ApplicationForm({
  app,
  index,
  readOnly,
  onChange,
  errorsFor,
}: {
  app: AppEdit;
  index: number;
  readOnly: boolean;
  onChange: (patch: Partial<AppEdit>) => void;
  errorsFor: (field: EditFieldKey) => WireError[];
}) {
  const kind = appWorkload(app);
  return (
    <section className="k-section">
      <div className="k-eyebrow">
        Application · <span className="k-mono">{app.name}</span>
        <span className="k-chip k-mono k-edit__kind">{kind}</span>
      </div>
      <div className="k-section__body k-edit__group">
        <div className="k-new__row">
          <EditField
            label="Image override"
            value={app.image}
            onChange={(v) => onChange({ image: v })}
            readOnly={readOnly}
            placeholder="inherits the project image"
            errors={errorsFor(`app.${index}.image`)}
            note="blank means the project's image (rule P3)"
          />
          {kind === "cron" ? (
            <EditField
              label="Schedule"
              value={app.schedule}
              onChange={(v) => onChange({ schedule: v })}
              readOnly={readOnly}
              placeholder="0 3 * * *"
              errors={errorsFor(`app.${index}.schedule`)}
              note="a five-field cron expression; clearing it makes this a worker"
            />
          ) : (
            <EditField
              label="Port"
              narrow
              value={app.port}
              onChange={(v) => onChange({ port: v })}
              readOnly={readOnly}
              placeholder="8080"
              errors={errorsFor(`app.${index}.port`)}
              note={
                kind === "service"
                  ? "a port makes this a web service: Deployment + Service + routing"
                  : "empty: a worker — Deployment, no routing"
              }
            />
          )}
        </div>

        <div className="k-new__row">
          {kind === "service" ? (
            <EditField
              label="Health path"
              value={app.health}
              onChange={(v) => onChange({ health: v })}
              readOnly={readOnly}
              placeholder="/healthz"
              errors={errorsFor(`app.${index}.health`)}
              note="an HTTP path used for both probes"
            />
          ) : null}
          <EditField
            label="Replicas (min)"
            narrow
            value={app.replicasMin}
            onChange={(v) => onChange({ replicasMin: v })}
            readOnly={readOnly}
            placeholder="1"
            errors={errorsFor(`app.${index}.replicas`)}
            note="blank leaves the model's default"
          />
          <EditField
            label="Replicas (max)"
            narrow
            value={app.replicasMax}
            onChange={(v) => onChange({ replicasMax: v })}
            readOnly={readOnly}
            placeholder="—"
            errors={[]}
            note="above min, this autoscales between the two bounds"
          />
        </div>

        {kind === "service" ? (
          <ListRows
            label="Domains"
            addLabel="Add domain"
            values={app.domains}
            readOnly={readOnly}
            placeholder="hello.dev.acme.run"
            errors={errorsFor(`app.${index}.domains`)}
            onChange={(domains) => onChange({ domains })}
            note="explicit FQDNs, which win over the Environment's derived hostname"
          />
        ) : null}

        <EnvRows
          label="Environment variables"
          env={app.env}
          readOnly={readOnly}
          onChange={(env) => onChange({ env })}
          errorsFor={(name) => errorsFor(`app.${index}.env.${name}`)}
          note="this application only; it overrides a project-level key of the same name (rule P1)"
        />
      </div>
    </section>
  );
}

function EditField({
  label,
  value,
  onChange,
  readOnly,
  placeholder,
  note,
  errors,
  narrow,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  readOnly: boolean;
  placeholder?: string;
  note?: string;
  errors: WireError[];
  narrow?: boolean;
}) {
  const id = useId();
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
        readOnly={readOnly}
        aria-invalid={errors.length > 0 ? true : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      <FieldErrors errors={errors} />
      {note !== undefined ? (
        <span className="k-field__note k-mono">{note}</span>
      ) : null}
    </div>
  );
}

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

function EnvRows({
  label,
  env,
  readOnly,
  onChange,
  errorsFor,
  note,
}: {
  label: string;
  env: EnvVar[];
  readOnly: boolean;
  onChange: (env: EnvVar[]) => void;
  errorsFor: (name: string) => WireError[];
  note: string;
}) {
  return (
    <div className="k-field">
      <span className="k-eyebrow">{label}</span>
      {env.length === 0 ? (
        <span className="k-field__note k-mono">none</span>
      ) : null}
      {env.map((row, i) => (
        <div className="k-new__pair" key={i}>
          <input
            className="k-input k-mono"
            aria-label={`${label} ${i + 1} name`}
            value={row.key}
            readOnly={readOnly}
            placeholder="LOG_LEVEL"
            onChange={(e) =>
              onChange(env.map((r, j) => (j === i ? { ...r, key: e.target.value } : r)))
            }
          />
          <input
            className="k-input k-mono"
            aria-label={`${label} ${i + 1} value`}
            value={row.value}
            readOnly={readOnly}
            placeholder="info"
            onChange={(e) =>
              onChange(
                env.map((r, j) => (j === i ? { ...r, value: e.target.value } : r)),
              )
            }
          />
          {readOnly ? null : (
            <button
              type="button"
              className="k-button"
              onClick={() => onChange(env.filter((_, j) => j !== i))}
            >
              Remove
            </button>
          )}
        </div>
      ))}
      {env.map((row, i) =>
        row.key.trim() === "" ? null : (
          <FieldErrors key={i} errors={errorsFor(row.key.trim())} />
        ),
      )}
      {readOnly ? null : (
        <div className="k-actions">
          <button
            type="button"
            className="k-button"
            onClick={() => onChange([...env, { key: "", value: "" }])}
          >
            Add variable
          </button>
        </div>
      )}
      <span className="k-field__note k-mono">{note}</span>
    </div>
  );
}

function ListRows({
  label,
  addLabel,
  values,
  readOnly,
  placeholder,
  errors,
  onChange,
  note,
}: {
  label: string;
  addLabel: string;
  values: string[];
  readOnly: boolean;
  placeholder: string;
  errors: WireError[];
  onChange: (values: string[]) => void;
  note: string;
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
            readOnly={readOnly}
            placeholder={placeholder}
            onChange={(e) =>
              onChange(values.map((v, j) => (j === i ? e.target.value : v)))
            }
          />
          {readOnly ? null : (
            <button
              type="button"
              className="k-button"
              onClick={() => onChange(values.filter((_, j) => j !== i))}
            >
              Remove
            </button>
          )}
        </div>
      ))}
      {readOnly ? null : (
        <div className="k-actions">
          <button
            type="button"
            className="k-button"
            onClick={() => onChange([...values, ""])}
          >
            {addLabel}
          </button>
        </div>
      )}
      <FieldErrors errors={errors} />
      <span className="k-field__note k-mono">{note}</span>
    </div>
  );
}

/* --------------------------------------------------------------- the bytes */

/**
 * The documents as text, edited directly.
 *
 * Structured errors are rendered whole here rather than mapped onto fields:
 * this tab is a view of lines, and `line:column` — which the server puts on
 * every finding it can locate — is the coordinate that matters when the reader
 * is looking at the file.
 */
function YamlEditor({
  text,
  open,
  onOpen,
  onChange,
  errors,
}: {
  text: SpecTextSet;
  open: string;
  onOpen: (name: string) => void;
  onChange: (name: string, value: string) => void;
  errors: readonly WireError[];
}) {
  const names = ["project", ...Object.keys(text.environments).sort()];
  const selected = names.includes(open) ? open : "project";
  const value =
    selected === "project" ? text.project : (text.environments[selected] ?? "");

  return (
    <section className="k-section">
      <nav className="k-tabs" aria-label="Documents">
        {names.map((name) => (
          <button
            key={name}
            type="button"
            className={name === selected ? "k-tab k-tab--active" : "k-tab"}
            aria-current={name === selected ? "true" : undefined}
            onClick={() => onOpen(name)}
          >
            {name === "project" ? "Project" : `Environment · ${name}`}
          </button>
        ))}
      </nav>
      <div className="k-section__body">
        <textarea
          className="k-textarea k-mono"
          aria-label={
            selected === "project"
              ? "Project document"
              : `Environment document ${selected}`
          }
          spellCheck={false}
          value={value}
          onChange={(e) => onChange(selected, e.target.value)}
        />
        <span className="k-field__note k-mono">
          {value.length} bytes · stored exactly as written, comments and key
          order included
        </span>
        {errors.length > 0 ? (
          <ErrorPanel
            title="The server would not accept this spec"
            errors={errors}
          />
        ) : null}
      </div>
    </section>
  );
}

/* ------------------------------------------------------------ what changes */

function ChangePreview({ diffs }: { diffs: EnvDiff[] }) {
  return (
    <section className="k-section">
      <div className="k-eyebrow">What changes ({diffs.length})</div>
      <p className="k-note">
        Today's stored documents against the edited ones, rendered — every
        environment, because a Project edit reaches all of them. This is a pure
        render and says nothing about what is live in a cluster; the Diff screen
        is the one that asks a cluster.
      </p>
      <div className="k-section__body k-edit__diffs">
        {diffs.map((d) => (
          <EnvironmentDiff key={d.environment} diff={d} />
        ))}
      </div>
    </section>
  );
}

function EnvironmentDiff({ diff }: { diff: EnvDiff }) {
  const decoded = useMemo((): { diff?: Diff; error?: string } => {
    const bytes = diff.response?.diffJson;
    if (bytes === undefined || bytes.length === 0) return {};
    try {
      return { diff: decodeDiff(bytes) };
    } catch (err) {
      return { error: err instanceof Error ? err.message : String(err) };
    }
  }, [diff.response]);

  return (
    <div className="k-edit__diff">
      <div className="k-eyebrow">
        <span className="k-chip k-mono">{diff.environment}</span>
      </div>
      {diff.error !== undefined ? (
        <ErrorPanel
          title={`Could not preview ${diff.environment}`}
          error={diff.error}
        />
      ) : null}
      {diff.response?.errors.length ? (
        <ErrorPanel
          title={`${diff.environment} was rejected`}
          errors={diff.response.errors}
        />
      ) : null}
      {decoded.error !== undefined ? (
        <ErrorPanel
          title="The diff payload could not be decoded"
          error={new Error(decoded.error)}
        />
      ) : null}
      {decoded.diff !== undefined ? (
        <DiffView
          diff={decoded.diff}
          exitSemantics={diff.response?.exitSemantics}
        />
      ) : null}
    </div>
  );
}

/* -------------------------------------------------------------- the states */

/**
 * Someone else wrote this spec while it was being edited.
 *
 * No merge is offered, and that is the decision: reapplying form edits onto
 * bytes that changed underneath them is a three-way merge, and a wrong one
 * silently produces a document nobody wrote. So the two honest actions are
 * offered instead — take theirs and start again, or take yours and say so —
 * and the edited text is one click from the clipboard either way, because
 * "your edits were discarded" must never mean "your edits are gone".
 */
function ConflictState({
  project,
  text,
  running,
  onReload,
  onForce,
}: {
  project: string;
  text: SpecTextSet;
  running: boolean;
  onReload: () => void;
  onForce: () => void;
}) {
  return (
    <div className="k-edit__conflict" role="alert">
      <div className="k-settled__head">
        <StatusPill status="degraded" label="store/version-conflict" />
        <span className="k-settled__title">
          The spec changed while you were editing
        </span>
      </div>
      <p className="k-edit__conflict-body">
        Someone — another browser, the CLI, a controller — stored a new version
        of {project} after this page read it, so the write was refused rather
        than silently overwriting theirs. Nothing has been saved.
      </p>

      <div className="k-eyebrow">Keep your work first</div>
      <div className="k-actions">
        <Copyable
          value={text.project}
          label="copy your edited Project document"
        />
        {Object.keys(text.environments)
          .sort()
          .map((name) => (
            <Copyable
              key={name}
              value={text.environments[name] ?? ""}
              label={`copy your edited ${name} document`}
            />
          ))}
      </div>

      <div className="k-actions k-edit__conflict-actions">
        <button type="button" className="k-button" onClick={onReload}>
          Reload the stored spec
        </button>
        <button
          type="button"
          className="k-button k-button--danger"
          onClick={onForce}
          disabled={running}
        >
          {running ? "Overwriting…" : "Overwrite their version"}
        </button>
      </div>
      <span className="k-mono k-deploy__note">
        Reloading fetches the current bytes and starts over — your edits here are
        discarded, not merged. Overwriting stores your documents with force=true
        and their change is lost.
      </span>
    </div>
  );
}

function Saved({ project }: { project: string }) {
  const base = `/apps/${encodeURIComponent(project)}`;
  return (
    <div className="k-settled" role="status">
      <div className="k-settled__head">
        <StatusPill status="synced" label="stored" />
        <span className="k-settled__title">The spec is stored</span>
      </div>
      <span className="k-mono">
        nothing has been applied to a cluster — deploying is the next, separate
        step
      </span>
      <div className="k-actions">
        <Link className="k-button k-button--primary" to={base}>
          View app
        </Link>
      </div>
    </div>
  );
}

/* --------------------------------------------------------------- the wire */

function toText(documents: SpecDocuments): SpecTextSet {
  const environments: Record<string, string> = {};
  for (const [name, bytes] of Object.entries(documents.environments)) {
    environments[name] = DECODER.decode(bytes);
  }
  return { project: DECODER.decode(documents.project), environments };
}

function toWire(text: SpecTextSet) {
  const environments: Record<string, Uint8Array> = {};
  for (const [name, doc] of Object.entries(text.environments)) {
    environments[name] = ENCODER.encode(doc);
  }
  return { project: ENCODER.encode(text.project), environments };
}
