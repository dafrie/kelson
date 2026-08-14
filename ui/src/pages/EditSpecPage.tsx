import { useCallback, useEffect, useId, useMemo, useState } from "react";
import { Link, useBlocker, useParams, useSearchParams } from "react-router-dom";

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
import { YamlBlock } from "../components/Disclosure";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { EmptyState, LoadingState } from "../components/States";
import { EnvValueFields, envValueNote } from "../components/EnvValueFields";
import { DiffView } from "../diff/DiffView";
import { decodeDiff, type Diff } from "../diff/parse";
import {
  appendComponent,
  componentDraftProblems,
  componentWorkload,
  emptyComponentDraft,
  isDataComponent,
  isDataDraft,
  isRebuildable,
  mapEditErrors,
  readSpec,
  sameText,
  writeSpec,
  type ComponentDraft,
  type ComponentDraftField,
  type ComponentEdit,
  type EditFieldKey,
  type EnvironmentEdit,
  type PreviewsEdit,
  type SpecEdit,
  type SpecTextSet,
} from "../spec/edit";
import { parseComponents } from "../spec/components";
import { plainEnv, type EnvVar } from "../spec/documents";

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
 * **Adding a component** (#214) is the third thing, and it hangs off the same
 * guard. A Project is a container of components (ADR-0014) and until now this
 * screen could edit the ones a document already had but never add one, so a
 * second component meant hand-writing YAML nobody advertised. The add panel
 * appends one entry to `spec.components` of the *stored* document — the append
 * is the stored bytes plus the new block, asserted rather than assumed
 * (src/spec/edit.ts: appendComponent) — and when the document is one this UI
 * cannot rebuild, it does what every other path here does: says so, and hands
 * over the exact block to paste in the YAML tab. Storing it is still the check
 * and the save below, because adding a component changes what would be deployed
 * and nothing else.
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
        <Link to={`/projects/${encodeURIComponent(project)}`}>← {project}</Link>
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
  const [params] = useSearchParams();
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
  /**
   * The add panel opens by itself when the project page sent the reader here to
   * add something (`?add=component`), so the action they pressed there is the
   * thing in front of them when the screen arrives.
   */
  const [adding, setAdding] = useState(() => params.get("add") === "component");
  const [addedName, setAddedName] = useState<string | undefined>(undefined);
  /** The block to paste when this document cannot be rebuilt — see onAdd. */
  const [handoff, setHandoff] = useState<string | undefined>(undefined);
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
  /**
   * The names already taken, for the add panel's uniqueness check.
   *
   * Read from the document rather than from the parsed draft: a hand-written
   * document has no draft at all, and a reader adding a component to one still
   * deserves to be told the name is taken before the server tells them.
   */
  const existingNames = useMemo(
    () => parseComponents(text.project).map((c) => c.name),
    [text.project],
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

  /**
   * One more entry in `spec.components`, or the honest refusal.
   *
   * The append is byte-faithful or it does not happen: `appendComponent`
   * returns the whole document set only when the stored bytes are ones this UI
   * can rebuild, and otherwise returns the block that would have been added so
   * the reader can paste it into the YAML tab with their formatting intact
   * (ADR-0013 §1). Nothing is written to the store either way — the check and
   * the save below are still the only things that write.
   */
  const onAdd = useCallback(
    (draft: ComponentDraft): boolean => {
      const result = appendComponent(text, draft);
      if (!result.ok) {
        setHandoff(result.added);
        setAddedName(undefined);
        return false;
      }
      setHandoff(undefined);
      setAdding(false);
      setAddedName(draft.name.trim());
      setDraft(result.edit);
      replace(result.text);
      return true;
    },
    [replace, text],
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

      {/* Under both tabs: adding a component is an act on the document, not on
          a view of it, and the document that cannot be rebuilt is exactly the
          one whose reader is standing on the YAML tab when they want one. */}
      <AddComponent
        existing={existingNames}
        open={adding}
        onOpen={setAdding}
        onAdd={onAdd}
        handoff={handoff}
        added={addedName}
        onOpenYaml={() => setTab("yaml")}
      />

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

  const setComponent = (index: number, patch: Partial<ComponentEdit>) =>
    onChange({
      ...edit,
      project: {
        ...edit.project,
        components: edit.project.components.map((c, i) =>
          i === index ? { ...c, ...patch } : c,
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
              note="shared by every component; a component's own image wins (rule P3)"
            />
          </div>
          <EnvRows
            label="Environment variables"
            env={edit.project.env}
            project={edit.project.name}
            readOnly={readOnly}
            onChange={(env) => setProject({ env })}
            errorsFor={(name) => errorsFor(`project.env.${name}`)}
            note="shared by every component (rule P1); a value is a plain string, a reference is a mapping — never a credential (ADR-0009, ADR-0018)"
          />
        </div>
      </section>

      {edit.project.components.map((component, i) =>
        isDataComponent(component) ? (
          <DataComponentRow key={`${i}:${component.name}`} component={component} />
        ) : (
          <ComponentForm
            key={`${i}:${component.name}`}
            component={component}
            index={i}
            project={edit.project.name}
            readOnly={readOnly}
            onChange={(patch) => setComponent(i, patch)}
            errorsFor={errorsFor}
          />
        ),
      )}

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
              <EditField
                label="Delivery mode"
                narrow
                value={env.delivery.mode}
                onChange={(v) =>
                  setEnvironment(i, { delivery: { ...env.delivery, mode: v } })
                }
                readOnly={readOnly}
                placeholder="direct"
                errors={errorsFor(`environment.${env.name}.delivery.mode`)}
                note="direct or flux; blank is the model's default (direct)"
              />
            </div>

            {env.delivery.mode.trim() === "" ? null : (
              <div className="k-new__row">
                <EditField
                  label="Deployment repository"
                  value={env.delivery.gitRepo}
                  onChange={(v) =>
                    setEnvironment(i, { delivery: { ...env.delivery, gitRepo: v } })
                  }
                  readOnly={readOnly}
                  placeholder="git@github.com:acme/deploy.git"
                  errors={errorsFor(`environment.${env.name}.delivery.gitRepo`)}
                  note="where kelson commits rendered manifests — required in flux mode, and not the previews source repository"
                />
                <EditField
                  label="Branch"
                  narrow
                  value={env.delivery.gitBranch}
                  onChange={(v) =>
                    setEnvironment(i, { delivery: { ...env.delivery, gitBranch: v } })
                  }
                  readOnly={readOnly}
                  placeholder="main"
                  errors={errorsFor(`environment.${env.name}.delivery.gitBranch`)}
                />
                <EditField
                  label="Path"
                  value={env.delivery.gitPath}
                  onChange={(v) =>
                    setEnvironment(i, { delivery: { ...env.delivery, gitPath: v } })
                  }
                  readOnly={readOnly}
                  placeholder={`${env.project}/${env.name}`}
                  errors={errorsFor(`environment.${env.name}.delivery.gitPath`)}
                />
              </div>
            )}

            <PreviewsFields
              environment={env}
              readOnly={readOnly}
              onChange={(previews) => setEnvironment(i, { previews })}
              errorsFor={errorsFor}
            />
          </div>
        </section>
      ))}
    </div>
  );
}

/**
 * `spec.previews` on an Environment (ADR-0017), authored here.
 *
 * Turning it on writes the block; turning it off removes it. There is no
 * `enabled:` key in the schema and this does not invent one — an environment
 * either declares previews or does not.
 *
 * Two things this form states rather than enforces. Previews render in flux
 * delivery mode only, so a block written under any other mode is
 * `render/previews-require-flux` from the server on Check — the note says so
 * and the refusal arrives with its own remediation, which is better than a
 * disabled control that cannot explain itself. And the block alone stands up no
 * preview: a CI step running `kelson preview publish` is the other half, and
 * the field notes point at it, because an environment configured here and
 * nowhere else gets change requests whose artifacts never arrive.
 */
function PreviewsFields({
  environment: env,
  readOnly,
  onChange,
  errorsFor,
}: {
  environment: EnvironmentEdit;
  readOnly: boolean;
  onChange: (previews: PreviewsEdit) => void;
  errorsFor: (field: EditFieldKey) => WireError[];
}) {
  const p = env.previews;
  const set = (patch: Partial<PreviewsEdit>) => onChange({ ...p, ...patch });
  const field = (name: string) =>
    errorsFor(`environment.${env.name}.previews.${name}`);
  const fluxMode = env.delivery.mode.trim() === "flux";

  return (
    <div className="k-edit__previews">
      <label className="k-field__check">
        <input
          type="checkbox"
          checked={p.enabled}
          disabled={readOnly}
          onChange={(e) => set({ enabled: e.target.checked })}
        />
        <span>Spawn a preview environment per open pull request</span>
      </label>
      <span className="k-field__note k-mono">
        {p.enabled
          ? fluxMode
            ? "flux-operator polls the forge and stands up <project>-<environment>-pr<id> per change request. The manifests come from a CI step running `kelson preview publish` — without it, every preview waits for an artifact nobody pushed."
            : "previews render in flux delivery mode only (ADR-0017): set the mode above, or Check will refuse this document with render/previews-require-flux."
          : "off: this environment has no per-pull-request children."}
      </span>

      {!p.enabled ? null : (
        <>
          <div className="k-new__row">
            <EditField
              label="Forge"
              narrow
              value={p.provider}
              onChange={(v) => set({ provider: v })}
              readOnly={readOnly}
              placeholder="github"
              errors={field("provider")}
              note="github or gitlab"
            />
            <EditField
              label="Source repository"
              value={p.repo}
              onChange={(v) => set({ repo: v })}
              readOnly={readOnly}
              placeholder="https://github.com/acme/checkout"
              errors={field("repo")}
              note="whose pull requests become previews — the HTTP(S) URL, and not the deployment repository above"
            />
            <EditField
              label="Forge credential"
              value={p.secretRef}
              onChange={(v) => set({ secretRef: v })}
              readOnly={readOnly}
              placeholder="github-auth"
              errors={field("secretRef")}
              note="the NAME of a Secret in this environment's namespace; never a token (ADR-0009)"
            />
          </div>

          <div className="k-new__row">
            <EditField
              label="Poll interval"
              narrow
              value={p.interval}
              onChange={(v) => set({ interval: v })}
              readOnly={readOnly}
              placeholder="10m"
              errors={field("interval")}
              note="blank means 10m"
            />
            <EditField
              label="Labels"
              value={p.filterLabels}
              onChange={(v) => set({ filterLabels: v })}
              readOnly={readOnly}
              placeholder="deploy/preview"
              errors={field("filterLabels")}
              note="comma-separated; blank means every open change request, which is why the ceiling exists"
            />
            <EditField
              label="Simultaneous previews"
              narrow
              value={p.limit}
              onChange={(v) => set({ limit: v })}
              readOnly={readOnly}
              placeholder="10"
              errors={field("limit")}
              note="blank means 10 — a cost control, deliberately below flux-operator's own 100"
            />
          </div>

          <div className="k-new__row">
            <EditField
              label="Include branches"
              value={p.includeBranch}
              onChange={(v) => set({ includeBranch: v })}
              readOnly={readOnly}
              placeholder="^feat/.*"
              errors={field("includeBranch")}
              note="a Go regular expression matched against the branch name"
            />
            <EditField
              label="Exclude branches"
              value={p.excludeBranch}
              onChange={(v) => set({ excludeBranch: v })}
              readOnly={readOnly}
              placeholder="^wip/.*"
              errors={field("excludeBranch")}
            />
            <EditField
              label="Pause updates while labelled"
              value={p.skipLabels}
              onChange={(v) => set({ skipLabels: v })}
              readOnly={readOnly}
              placeholder="deploy/preview-pause, !ci/passed"
              errors={field("skipLabels")}
              note="comma-separated; a leading ! pauses while the label is ABSENT, which is how a tests-passed gate is written"
            />
          </div>

          <div className="k-new__row">
            <EditField
              label="Artifact repository"
              value={p.artifactsRepository}
              onChange={(v) => set({ artifactsRepository: v })}
              readOnly={readOnly}
              placeholder="oci://ghcr.io/acme/checkout-previews"
              errors={field("artifactsRepository")}
              note="where `kelson preview publish` pushes each preview's manifests. No tag: kelson pins each one to its change request's head commit"
            />
            <EditField
              label="Artifact pull secret"
              value={p.artifactsSecretRef}
              onChange={(v) => set({ artifactsSecretRef: v })}
              readOnly={readOnly}
              placeholder="ghcr-auth"
              errors={field("artifactsSecretRef")}
              note="only for a private artifact repository"
            />
          </div>
        </>
      )}
    </div>
  );
}

/**
 * A data component, stated rather than edited.
 *
 * It is in the list because a Project's components are one list (ADR-0014) and
 * leaving it out would make the form disagree with the document about what this
 * project contains. It has no fields because its whole configuration is a
 * preset an operator implements (ADR-0005, ADR-0007): there is no image to
 * roll, no replica count to set, and offering either would invite exactly the
 * wrong instinct — which is the same reason the project page gives databases
 * their own section (#107). The preset itself lives in that section, and this
 * says so instead of growing a second control for it.
 */
function DataComponentRow({ component }: { component: ComponentEdit }) {
  return (
    <section className="k-section">
      <div className="k-eyebrow">
        Component · <span className="k-mono">{component.name}</span>
        <span className="k-chip k-mono k-edit__kind">{component.kind}</span>
      </div>
      <div className="k-section__body k-edit__group">
        <p className="k-note">
          A managed data service. Its topology is a preset its operator
          implements, so there is nothing here to scale or roll — the preset and
          what it means are on the project page, in the data services section,
          and the YAML tab is where a `preset:` is written. A workload reaches
          it with{" "}
          <span className="k-mono">
            {`{ from: { service: ${component.name}, key: uri } }`}
          </span>
          .
        </p>
      </div>
    </section>
  );
}

/**
 * Adding one entry to `spec.components` (#214).
 *
 * The kind is the question, and the shape follows from it — the inverse of the
 * create form, which asks for a port and derives the kind, because "add the
 * worker" is a different sentence from "get something running". Only the kinds
 * this form can write whole are offered:
 *
 *   - **service, worker, cron** — the derived kinds. They are asked as the
 *     shape questions the model derives them from (docs/model.md), so the
 *     document that gets written states a port or a schedule and never a
 *     `kind:` that repeats what the shape already says.
 *   - **postgres, valkey** — a name and a `kind:`, which is a whole data
 *     component: every other field on one is `schema/mutually-exclusive` and
 *     its preset defaults, so there is nothing else to ask here. The preset is
 *     the data-services panel's subject (#107, ADR-0007).
 *
 * `helm` and `agent` are not offered, and are named rather than shown disabled:
 * a chart component is a `chart:` plus `values:` this form has no fields for
 * (ADR-0016), and an agent is worker-shaped whose one distinguishing field,
 * `tools:`, kelson rejects until M7 (#75) — so a picker entry for either would
 * be a control that cannot finish its own job.
 *
 * Nothing here writes to the store. The append changes the document in this
 * editor, and the check and the save below are what store it, exactly as every
 * other edit on this screen.
 */
function AddComponent({
  existing,
  open,
  onOpen,
  onAdd,
  handoff,
  added,
  onOpenYaml,
}: {
  /** The component names already in the document, for the uniqueness rule. */
  existing: readonly string[];
  open: boolean;
  onOpen: (open: boolean) => void;
  /** Appends, and answers whether it happened. */
  onAdd: (draft: ComponentDraft) => boolean;
  /** The block to paste, when the document could not be rebuilt. */
  handoff: string | undefined;
  /** The name the last successful add wrote, if this session made one. */
  added: string | undefined;
  onOpenYaml: () => void;
}) {
  const [draft, setDraft] = useState<ComponentDraft>(emptyComponentDraft);
  const [attempted, setAttempted] = useState(false);
  const set = (patch: Partial<ComponentDraft>) =>
    setDraft((prev) => ({ ...prev, ...patch }));

  const problems = componentDraftProblems(draft, existing);
  const problemFor = (field: ComponentDraftField): string | undefined =>
    attempted ? problems.find((p) => p.field === field)?.message : undefined;

  const submit = () => {
    setAttempted(true);
    if (componentDraftProblems(draft, existing).length > 0) return;
    // The draft is kept when the append could not happen: the reader is being
    // handed a snippet, and clearing the fields under it would take away the
    // thing the snippet describes.
    if (!onAdd(draft)) return;
    setDraft(emptyComponentDraft());
    setAttempted(false);
  };

  const data = isDataDraft(draft);

  return (
    // A named region: this panel's fields have the same names as the ones the
    // component sections above carry (a port is a port), and the landmark is
    // what tells a screen reader — and a test — which one is which.
    <section className="k-section k-edit__add" aria-label="Add a component">
      <div className="k-env__head">
        <div className="k-eyebrow">Add a component</div>
        {open ? null : (
          <button type="button" className="k-button" onClick={() => onOpen(true)}>
            Add component
          </button>
        )}
      </div>

      {added !== undefined ? (
        <p className="k-note" role="status">
          <span className="k-mono">{added}</span> was added to this project's
          components. Nothing is stored yet — check and save below.
        </p>
      ) : null}

      {handoff !== undefined ? (
        <div className="k-edit__handoff" role="alert">
          <span className="k-edit__handoff-title">
            This document cannot be rebuilt from the form
          </span>
          <p className="k-note">
            It carries something these fields do not model — a comment, a key
            order, a kind this form does not write — so appending through them
            would rewrite your file from what the form understood (ADR-0013 §1).
            Here is the entry instead: paste it under{" "}
            <span className="k-mono">spec.components:</span> in the YAML tab,
            where the rest of the document keeps its formatting.
          </p>
          <YamlBlock bytes={handoff} />
          <div className="k-actions">
            <Copyable value={handoff} label="copy the component entry" />
            <button type="button" className="k-button" onClick={onOpenYaml}>
              Edit the YAML
            </button>
          </div>
        </div>
      ) : null}

      {!open ? (
        <span className="k-field__note k-mono">
          a project is a container of components (ADR-0014): a service, its
          worker, a nightly job and the database they share are one Project and
          one spec
        </span>
      ) : (
        <div className="k-section__body k-edit__group">
          <fieldset className="k-panel k-new__source">
            <legend className="k-eyebrow">What it is</legend>
            {KIND_CHOICES.map((choice) => (
              <label className="k-check k-mono" key={choice.kind}>
                <input
                  type="radio"
                  name="new-component-kind"
                  checked={draft.kind === choice.kind}
                  onChange={() => set({ kind: choice.kind })}
                />
                {choice.label}
              </label>
            ))}
            <span className="k-field__note k-mono">
              {KIND_CHOICES.find((c) => c.kind === draft.kind)?.note}
            </span>
            <span className="k-field__note k-mono">
              `helm` and `agent` are components too and are not offered here: a
              chart needs its own `chart:` and `values:`, which these fields do
              not have (ADR-0016), and an agent is worker-shaped until its
              `tools:` policy stops being rejected (#75, M7). Both are written on
              the YAML tab.
            </span>
          </fieldset>

          <div className="k-new__row">
            <EditField
              label="Component name"
              value={draft.name}
              onChange={(v) => set({ name: v })}
              readOnly={false}
              placeholder={data ? "db" : "worker"}
              problem={problemFor("name")}
              errors={[]}
              note="a DNS-1123 label, unique within this project — it names the workload or the database in the namespace"
            />
            {draft.kind === "service" ? (
              <EditField
                label="Port"
                narrow
                value={draft.port}
                onChange={(v) => set({ port: v })}
                readOnly={false}
                placeholder="8080"
                problem={problemFor("port")}
                errors={[]}
                note="the port the container listens on — what makes this a service: Deployment + Service + routing"
              />
            ) : null}
            {draft.kind === "cron" ? (
              <EditField
                label="Schedule"
                value={draft.schedule}
                onChange={(v) => set({ schedule: v })}
                readOnly={false}
                placeholder="0 3 * * *"
                problem={problemFor("schedule")}
                errors={[]}
                note="a five-field cron expression — what makes this a CronJob"
              />
            ) : null}
          </div>

          {data ? (
            <p className="k-field__note k-mono">
              that is the whole component: a data service's configuration is its
              preset, and leaving it out takes the model's default. What each
              preset means — instances, CPU, storage, replication — is on the
              project page, in the data services section, and a preset is
              written there or on the YAML tab.
            </p>
          ) : (
            <>
              <fieldset className="k-panel k-new__source">
                <legend className="k-eyebrow">Which image it runs</legend>
                <label className="k-check k-mono">
                  <input
                    type="radio"
                    name="new-component-image"
                    checked={!draft.ownImage}
                    onChange={() => set({ ownImage: false })}
                  />
                  The project's image
                </label>
                <label className="k-check k-mono">
                  <input
                    type="radio"
                    name="new-component-image"
                    checked={draft.ownImage}
                    onChange={() => set({ ownImage: true })}
                  />
                  Its own image
                </label>
                <span className="k-field__note k-mono">
                  {draft.ownImage
                    ? "an `image:` on the component, which wins over the project's (rule P3)"
                    : "no `image:` at all: this component runs the project's image, or the artifact its source and build produce (rule P3) — which is how one repository ships a web process and a worker"}
                </span>
              </fieldset>

              {draft.ownImage ? (
                <div className="k-new__row">
                  <EditField
                    label="Image"
                    value={draft.image}
                    onChange={(v) => set({ image: v })}
                    readOnly={false}
                    placeholder="ghcr.io/acme/hello-worker:1.4.2"
                    problem={problemFor("image")}
                    errors={[]}
                    note="a pre-built image reference — a tag works, a digest is reproducible"
                  />
                </div>
              ) : null}
            </>
          )}

          <div className="k-deploy__confirm k-edit__actions">
            <button
              type="button"
              className="k-button k-button--primary"
              onClick={submit}
            >
              Add to the spec
            </button>
            <button type="button" className="k-button" onClick={() => onOpen(false)}>
              Cancel
            </button>
            <span className="k-mono k-deploy__note">
              appends one entry to spec.components · stores nothing, deploys
              nothing — the check and the save below do that
            </span>
          </div>
        </div>
      )}
    </section>
  );
}

/** The kinds the add panel writes, with what each one is. */
const KIND_CHOICES: {
  kind: ComponentDraft["kind"];
  label: string;
  note: string;
}[] = [
  {
    kind: "service",
    label: "Web service",
    note: "a port makes it a service: Deployment + Service + routing",
  },
  {
    kind: "worker",
    label: "Worker",
    note: "no port and no schedule: a Deployment that serves no traffic",
  },
  {
    kind: "cron",
    label: "Cron job",
    note: "a schedule makes it a CronJob; a job that also serves traffic is two components",
  },
  {
    kind: "postgres",
    label: "PostgreSQL",
    note: "a managed database, rendered as a CloudNativePG Cluster its operator runs (ADR-0007)",
  },
  {
    kind: "valkey",
    label: "Valkey",
    note: "a managed cache, rendered as a ValkeyCluster its operator runs (ADR-0015)",
  },
];

function ComponentForm({
  component,
  index,
  project,
  readOnly,
  onChange,
  errorsFor,
}: {
  component: ComponentEdit;
  index: number;
  project: string;
  readOnly: boolean;
  onChange: (patch: Partial<ComponentEdit>) => void;
  errorsFor: (field: EditFieldKey) => WireError[];
}) {
  const kind = componentWorkload(component);
  return (
    <section className="k-section">
      <div className="k-eyebrow">
        Component · <span className="k-mono">{component.name}</span>
        <span className="k-chip k-mono k-edit__kind">{kind}</span>
      </div>
      <div className="k-section__body k-edit__group">
        <div className="k-new__row">
          <EditField
            label="Image override"
            value={component.image}
            onChange={(v) => onChange({ image: v })}
            readOnly={readOnly}
            placeholder="inherits the project image"
            errors={errorsFor(`component.${index}.image`)}
            note="blank means the project's image (rule P3)"
          />
          {kind === "cron" ? (
            <EditField
              label="Schedule"
              value={component.schedule}
              onChange={(v) => onChange({ schedule: v })}
              readOnly={readOnly}
              placeholder="0 3 * * *"
              errors={errorsFor(`component.${index}.schedule`)}
              note="a five-field cron expression; clearing it makes this a worker"
            />
          ) : (
            <EditField
              label="Port"
              narrow
              value={component.port}
              onChange={(v) => onChange({ port: v })}
              readOnly={readOnly}
              placeholder="8080"
              errors={errorsFor(`component.${index}.port`)}
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
              value={component.health}
              onChange={(v) => onChange({ health: v })}
              readOnly={readOnly}
              placeholder="/healthz"
              errors={errorsFor(`component.${index}.health`)}
              note="an HTTP path used for both probes"
            />
          ) : null}
          <EditField
            label="Replicas (min)"
            narrow
            value={component.replicasMin}
            onChange={(v) => onChange({ replicasMin: v })}
            readOnly={readOnly}
            placeholder="1"
            errors={errorsFor(`component.${index}.replicas`)}
            note="blank leaves the model's default"
          />
          <EditField
            label="Replicas (max)"
            narrow
            value={component.replicasMax}
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
            values={component.domains}
            readOnly={readOnly}
            placeholder="hello.dev.acme.run"
            errors={errorsFor(`component.${index}.domains`)}
            onChange={(domains) => onChange({ domains })}
            note="explicit FQDNs, which win over the Environment's derived hostname"
          />
        ) : null}

        <EnvRows
          label="Environment variables"
          env={component.env}
          project={project}
          readOnly={readOnly}
          onChange={(env) => onChange({ env })}
          errorsFor={(name) => errorsFor(`component.${index}.env.${name}`)}
          note="this component only; it overrides a project-level key of the same name (rule P1)"
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
  problem,
  errors,
  narrow,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  readOnly: boolean;
  placeholder?: string;
  note?: string;
  /**
   * What the browser itself refuses to send, as the create form words it: a
   * name that is not a label, a port that is not a number. The server's own
   * findings arrive separately, in `errors`, and both can be true at once.
   */
  problem?: string | undefined;
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
        aria-invalid={problem !== undefined || errors.length > 0 ? true : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {problem !== undefined ? (
        <span className="k-field__problem k-mono" role="alert">
          {problem}
        </span>
      ) : null}
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
  project,
  readOnly,
  onChange,
  errorsFor,
  note,
}: {
  label: string;
  env: EnvVar[];
  /** The project these variables belong to, for the Secrets panel link. */
  project: string;
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
        <div key={i}>
          <div className="k-new__pair">
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
            <EnvValueFields
              name={`${label} ${i + 1}`}
              value={row.value}
              readOnly={readOnly}
              onChange={(value) =>
                onChange(env.map((r, j) => (j === i ? { ...r, value } : r)))
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
          {row.value.kind === "plain" ? null : (
            <span className="k-field__note k-mono">{envValueNote(row.value)}</span>
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
            onClick={() => onChange([...env, { key: "", value: plainEnv("") }])}
          >
            Add variable
          </button>
        </div>
      )}
      <span className="k-field__note k-mono">{note}</span>
      {/* Where a credential actually goes, one link away. A reference names a
          Secret in this environment's namespace and the panel that writes it is
          on the project page (#116). */}
      <span className="k-field__note k-mono">
        a reference carries no value — write the Secret itself in the{" "}
        <Link to={`/projects/${encodeURIComponent(project)}`}>Secrets panel</Link> on
        this project's page, or with `kelson secret set`
      </span>
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
  const base = `/projects/${encodeURIComponent(project)}`;
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
          View project
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
