import { useCallback, useId, useMemo, useState } from "react";

import { useAsync, useClients } from "../api/data";
import { useRun } from "../api/stream";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import type { Error as WireError } from "../gen/kelson/v1alpha1/common_pb";
import type { GitOpsOwnership } from "../gen/kelson/v1alpha1/spec_pb";
import type { SpecTextSet } from "../spec/edit";
import {
  bundle,
  exportedDocuments,
  joinNames,
  kustomizationName,
  owningKustomizations,
  rememberedPaths,
  rememberedTarget,
  rememberPaths,
  rememberTarget,
  suggestedPath,
  type ExportedDocument,
  type ProposalTarget,
} from "../spec/gitops";

/**
 * The GitOps half of the edit screen (#248, ADR-0033 decision 3).
 *
 * Three things, in the order they are worth to a reader who has just been told
 * they cannot save.
 *
 *   1. **The banner.** Which documents git owns, which Kustomization owns them,
 *      and what that means for the button they were about to press. Plus
 *      ADR-0036 decision 5's warning where it applies: `autoDeploy` writes
 *      specs, so a tracked component and a git-reconciled Environment overwrite
 *      each other on a loop.
 *   2. **The export.** The stored document, copyable and downloadable. It needs
 *      no server call — GetSpec already returned the bytes — and it is the path
 *      that works for every connection, every forge and no connection at all.
 *   3. **The proposal.** The same bytes, as a pull request, through a
 *      GitConnection that can open one.
 *
 * # Why the paths are typed and not derived
 *
 * kelson knows the Kustomization reconciling a document, because
 * kustomize-controller labels what it applies. It does not know which
 * repository that Kustomization reads or where in it the file sits: that is on
 * the Kustomization's own `sourceRef` and `path`, which needs RBAC on the Flux
 * kinds in whatever namespace the user's Flux runs in — kelson-server's is one
 * namespace by design (ADR-0013 §3). So the repository and the paths are asked
 * for, prefilled with a suggestion and remembered in this browser, and the
 * screen says why rather than pretending it guessed.
 */

export function GitOpsBanner({
  gitops,
  autoDeploy,
}: {
  gitops: readonly GitOpsOwnership[];
  /** Environment documents declaring `autoDeploy` (src/spec/gitops.ts). */
  autoDeploy: readonly string[];
}) {
  const kustomizations = owningKustomizations(gitops);
  const documents = gitops.map((o) =>
    o.document === "project" ? "the project document" : `the ${o.document} environment`,
  );

  return (
    <div className="k-gitops" role="status">
      <p className="k-gitops__title">
        {joinNames(documents)}{" "}
        {gitops.length === 1 ? "is" : "are"} reconciled from a repository by{" "}
        {joinNames(kustomizations.map((k) => k))}
      </p>
      <p className="k-gitops__body">
        Saving here would be undone by the next reconcile, so Save is off.
        Export the documents and commit them yourself, or propose them as a
        pull request.
      </p>
      {/* ADR-0036 decision 5: a trigger deploys by writing the spec, so the
          two writers fight on every push. */}
      {autoDeploy.length > 0 ? (
        <p className="k-gitops__warn">
          {joinNames(autoDeploy.map((name) => `The ${name} environment`))}{" "}
          {autoDeploy.length === 1 ? "declares" : "declare"} <code>autoDeploy</code>,
          which does not compose with a git-reconciled Environment: kelson
          writes each built image into the spec, and the repository writes it
          back. Track the image in the repository instead, or take these
          documents out of git.
        </p>
      ) : null}
    </div>
  );
}

/* ------------------------------------------------------------------ export */

export function ExportPanel({
  project,
  text,
  gitops,
}: {
  project: string;
  text: SpecTextSet;
  gitops: readonly GitOpsOwnership[];
}) {
  const documents = useMemo(() => exportedDocuments(text, gitops), [text, gitops]);
  const [open, setOpen] = useState<string | undefined>(undefined);

  return (
    <section className="k-gitops__section">
      <h2 className="k-gitops__heading">Export</h2>
      {/* Not the authored bytes: a custom resource has no memory of comments
          or key order (ADR-0027 decision 6). */}
      <p className="k-note">
        The stored documents, ready to commit. They are equivalent to what was
        authored rather than byte-for-byte — comments and key order live in
        your repository.
      </p>
      <div className="k-gitops__actions">
        <Copyable
          value={bundle(documents)}
          label={`copy all ${documents.length} documents`}
          className="k-gitops__copy"
        />
        <DownloadButton
          name={`${project}.yaml`}
          text={bundle(documents)}
          label="Download all as one file"
        />
      </div>
      <ul className="k-gitops__docs">
        {documents.map((doc) => (
          <li key={doc.name}>
            <div className="k-gitops__doc">
              <button
                type="button"
                className="k-gitops__toggle"
                aria-expanded={open === doc.name}
                onClick={() => setOpen(open === doc.name ? undefined : doc.name)}
              >
                {doc.label}
              </button>
              {doc.owner ? (
                <span className="k-mono k-gitops__owner">
                  {kustomizationName(doc.owner)}
                </span>
              ) : (
                <span className="k-mono k-gitops__owner">kelson writes this one</span>
              )}
              <Copyable value={doc.text} label={`copy ${doc.label}`} />
              <DownloadButton
                name={`${project}${doc.name === "project" ? "" : `-${doc.name}`}.yaml`}
                text={doc.text}
                label="Download"
              />
            </div>
            {open === doc.name ? (
              <pre className="k-pre k-panel k-panel--dim">
                <code>{doc.text.replace(/\n+$/, "")}</code>
              </pre>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * A download of text the page already holds.
 *
 * An object URL rather than a `data:` one, because a spec document runs to
 * kilobytes and browsers cap `data:` URL length in ways that fail silently. The
 * URL is revoked on the next tick: revoking synchronously races the navigation
 * the click starts, and never revoking leaks the blob for the life of the tab.
 */
function DownloadButton({
  name,
  text,
  label,
}: {
  name: string;
  text: string;
  label: string;
}) {
  const download = useCallback(() => {
    const url = URL.createObjectURL(new Blob([text], { type: "text/yaml" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = name;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }, [name, text]);

  return (
    <button type="button" className="k-button" onClick={download}>
      {label}
    </button>
  );
}

/* ----------------------------------------------------------------- propose */

export function ProposePanel({
  project,
  text,
  gitops,
  dirty,
}: {
  project: string;
  text: SpecTextSet;
  gitops: readonly GitOpsOwnership[];
  /** Whether the editor holds changes. A proposal of the stored bytes is a no-op pull request. */
  dirty: boolean;
}) {
  const clients = useClients();
  const documents = useMemo(() => exportedDocuments(text, gitops), [text, gitops]);
  const connections = useAsync(
    (signal) => clients.gitConnection.listConnections({}, { signal }),
    [clients],
  );

  const [target, setTarget] = useState<ProposalTarget>(() => rememberedTarget(project));
  const [paths, setPaths] = useState<Record<string, string>>(() => rememberedPaths(project));
  /**
   * Which documents this proposal carries.
   *
   * Managed documents are checked by default and an unmanaged one is not: the
   * unmanaged one is the environment kelson writes itself, and sweeping it into
   * a pull request would hand git a document that was working fine without it.
   */
  const [chosen, setChosen] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(documents.map((d) => [d.name, d.owner !== undefined])),
  );
  const [title, setTitle] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [findings, setFindings] = useState<readonly WireError[]>([]);
  const [opened, setOpened] = useState<{ url: string; branch: string } | undefined>(undefined);
  const propose = useRun();
  const ids = useId();

  const selected = documents.filter((d) => chosen[d.name]);
  const pathOf = (doc: ExportedDocument) =>
    (paths[doc.name] ?? suggestedPath(project, doc.name)).trim();
  const missingPath = selected.some((d) => pathOf(d) === "");
  const ready =
    dirty &&
    confirmed &&
    selected.length > 0 &&
    !missingPath &&
    target.connection !== "" &&
    target.repository.trim() !== "";

  /**
   * The proposal itself.
   *
   * The paths are remembered *before* the call rather than after it succeeds,
   * and that is deliberate: the value of remembering is highest exactly when
   * the call fails. Somebody whose first attempt was refused for a permission
   * must not have to retype four paths to try again once it is granted.
   */
  const files = selected.map((doc) => ({
    path: pathOf(doc),
    content: new TextEncoder().encode(doc.text),
  }));

  const open = () => {
    setFindings([]);
    setOpened(undefined);
    rememberTarget(project, target);
    rememberPaths(project, Object.fromEntries(files.map((f, i) => [selected[i]!.name, f.path])));
    propose.start(async (signal) => {
      const res = await clients.spec.proposeSpec(
        {
          project,
          connection: target.connection,
          repository: target.repository.trim(),
          baseBranch: target.baseBranch.trim(),
          title: title.trim(),
          files,
          idempotencyKey: crypto.randomUUID(),
        },
        { signal },
      );
      if (res.errors.length > 0) {
        setFindings(res.errors);
        return;
      }
      setOpened({ url: res.url, branch: res.branch });
    });
  };

  if (opened !== undefined) {
    return (
      <section className="k-gitops__section">
        <h2 className="k-gitops__heading">Proposed</h2>
        <p className="k-note">
          Branch <code>{opened.branch}</code> was pushed to{" "}
          <code>{target.repository}</code> and a pull request is open. Nothing
          has changed in the cluster — merging is what deploys it.
        </p>
        <p>
          <a href={opened.url} target="_blank" rel="noreferrer" className="k-link">
            Open the pull request
          </a>
        </p>
      </section>
    );
  }

  return (
    <section className="k-gitops__section">
      <h2 className="k-gitops__heading">Propose as a pull request</h2>
      {/* kelson cannot read the owning Kustomization's sourceRef and path:
          that needs RBAC on the Flux kinds in the namespace the user's Flux
          runs in, and kelson-server holds one namespace by design. */}
      <p className="k-note">
        kelson cannot see which repository these documents live in. Name it and
        the paths once — this browser remembers them.
      </p>

      <div className="k-field">
        <label htmlFor={`${ids}-connection`}>Connection</label>
        <select
          id={`${ids}-connection`}
          className="k-select"
          value={target.connection}
          onChange={(e) => setTarget({ ...target, connection: e.target.value })}
        >
          <option value="">choose a connection…</option>
          {(connections.data?.connections ?? []).map((c) => (
            <option key={c.name} value={c.name}>
              {c.name} · {c.host}
            </option>
          ))}
        </select>
        <span className="k-field__note">
          the credential the pull request is opened with — it needs write access
          to the repository
        </span>
      </div>

      {connections.error !== undefined ? (
        <ErrorPanel title="Could not list the connections" error={connections.error} />
      ) : null}

      <div className="k-field">
        <label htmlFor={`${ids}-repository`}>Repository</label>
        <input
          id={`${ids}-repository`}
          className="k-input"
          value={target.repository}
          placeholder="acme/gitops"
          onChange={(e) => setTarget({ ...target, repository: e.target.value })}
        />
        <span className="k-field__note">
          owner/name — the repository these documents are reconciled from, often
          not the one this project builds from
        </span>
      </div>

      <div className="k-field k-field--narrow">
        <label htmlFor={`${ids}-base`}>Base branch</label>
        <input
          id={`${ids}-base`}
          className="k-input"
          value={target.baseBranch}
          placeholder="the repository's default"
          onChange={(e) => setTarget({ ...target, baseBranch: e.target.value })}
        />
      </div>

      <div className="k-field">
        <label htmlFor={`${ids}-title`}>Title</label>
        <input
          id={`${ids}-title`}
          className="k-input"
          value={title}
          placeholder={`Update the kelson documents for ${project}`}
          onChange={(e) => setTitle(e.target.value)}
        />
      </div>

      <fieldset className="k-gitops__files">
        <legend>Files this proposal writes</legend>
        {documents.map((doc) => (
          <div className="k-gitops__file" key={doc.name}>
            <label className="k-field__check">
              <input
                type="checkbox"
                checked={chosen[doc.name] ?? false}
                onChange={(e) => setChosen({ ...chosen, [doc.name]: e.target.checked })}
              />
              <span>{doc.label}</span>
            </label>
            <input
              className="k-input"
              aria-label={`repository path for the ${doc.label} document`}
              value={paths[doc.name] ?? suggestedPath(project, doc.name)}
              disabled={!(chosen[doc.name] ?? false)}
              onChange={(e) => setPaths({ ...paths, [doc.name]: e.target.value })}
            />
          </div>
        ))}
      </fieldset>

      <label className="k-field__check k-gitops__confirm">
        <input
          type="checkbox"
          checked={confirmed}
          onChange={(e) => setConfirmed(e.target.checked)}
        />
        <span>
          I understand each file is replaced whole — anything else living at
          these paths is removed. The pull request&apos;s diff shows it.
        </span>
      </label>

      {findings.length > 0 ? (
        <ErrorPanel
          title="The server would not propose these documents"
          errors={[...findings]}
        />
      ) : null}
      {propose.error !== undefined ? (
        <ErrorPanel title="Could not open the pull request" error={propose.error} />
      ) : null}

      <div className="k-deploy__confirm k-edit__actions">
        <button
          type="button"
          className="k-button k-button--primary k-button--wide"
          onClick={open}
          disabled={!ready || propose.running}
        >
          {propose.running ? "Proposing…" : "Propose as a pull request"}
        </button>
        <span className="k-mono k-deploy__note">{proposeNote(dirty, confirmed, ready)}</span>
      </div>
    </section>
  );
}

function proposeNote(dirty: boolean, confirmed: boolean, ready: boolean): string {
  if (!dirty) return "nothing has changed yet — the pull request would be empty";
  if (!confirmed) return "confirm the whole-file write first";
  if (!ready) return "choose a connection, a repository and a path for every file";
  return "opens a branch and a pull request in your repository · nothing is deployed";
}
