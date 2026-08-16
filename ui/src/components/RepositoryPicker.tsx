import { useCallback, useEffect, useId, useMemo, useState } from "react";

import { useAsync, useClients } from "../api/data";
import { browseRefusal } from "../api/errors";
import { ErrorPanel } from "./ErrorPanel";
import type {
  GitConnection,
  GitRepository,
} from "../gen/kelson/v1alpha1/gitconnection_pb";
import "./RepositoryPicker.css";

/**
 * Choosing a repository from a connection instead of typing its URL
 * (ADR-0033 decision 3, #248).
 *
 * # It fills the fields, it does not replace them
 *
 * Picking writes the same three values a person types — the repository URL, the
 * ref, and now `spec.source.connection` — into the same form the manual path
 * owns, and the manual inputs stay on screen and stay editable. That is the
 * whole design: the picker is a faster way to fill in a form, not a second way
 * to create a project, so there is no state it can get into that leaves someone
 * unable to finish by typing. Editing the URL by hand afterwards unpins the
 * connection, because a pinned name that no longer matches the repository under
 * it is worse than no pin at all.
 *
 * The connection *is* written when a repository is picked, and that is
 * deliberate rather than incidental. ADR-0033 decision 4 resolves an absent
 * `connection` by longest host-then-owner match, which is right for a project
 * whose author pasted a URL and knows nothing about connections — but this user
 * has just said which connection they mean, and re-deriving it later would let
 * a second connection covering the same host turn that answer into an
 * ambiguity error on a project that was created by pointing at one.
 *
 * # A connection that cannot be browsed is not an error state
 *
 * `RepoBrowser` is optional. A connection whose provider has none refuses with
 * `connection/capability-unsupported`, and what this renders for it is the
 * server's own sentence — which names what the connection can do and says that
 * pasting a URL works — as a note, beside inputs that are still there. It is
 * not an ErrorPanel and not an empty list: the first would say something is
 * broken when nothing is, and the second is indistinguishable from an
 * installation with no repositories selected, which is the confusion these RPCs
 * exist to prevent.
 *
 * A forge that accepted the credential and then failed *is* a failure, and gets
 * the panel.
 */

/** The three source fields a pick fills in. */
export interface RepositoryPick {
  connection: string;
  git: string;
  ref: string;
}

/**
 * Above this many repositories, the list gets a filter box.
 *
 * An installation on an organisation of any size runs to hundreds, and a picker
 * that hides the one the user came for behind a scroll is the failure
 * internal/forge's pagination comment names. Below it the filter is noise: it
 * would be a control that saves nobody a scroll.
 */
const FILTER_ABOVE = 8;

export function RepositoryPicker({
  connections,
  picked,
  onPick,
}: {
  connections: readonly GitConnection[];
  /** The current source fields, so the controls reflect what the form holds. */
  picked: RepositoryPick;
  onPick: (pick: RepositoryPick) => void;
}) {
  const clients = useClients();
  const [connection, setConnection] = useState(() => connections[0]?.name ?? "");
  const [query, setQuery] = useState("");
  const [repository, setRepository] = useState<GitRepository | undefined>(
    undefined,
  );
  const connectionId = useId();
  const filterId = useId();
  const branchId = useId();

  const repositories = useAsync(async (signal) => {
    if (connection === "") return [];
    const res = await clients.gitConnection.listConnectionRepositories(
      { connection },
      { signal },
    );
    return res.repositories;
  }, [clients, connection]);

  // Branches are a property of one repository, so there is nothing to ask until
  // one is chosen. The empty answer is a value rather than a skipped call
  // because the hook is keyed by deps, and "no repository yet" has to clear the
  // previous repository's branches rather than leave them on screen.
  const branches = useAsync(async (signal) => {
    if (connection === "" || repository === undefined) return [];
    const res = await clients.gitConnection.listConnectionBranches(
      { connection, repository: repository.fullName },
      { signal },
    );
    return res.branches;
  }, [clients, connection, repository]);

  // Switching connection drops the repository chosen from the previous one: its
  // branches belong to a credential that is no longer selected, and leaving it
  // selected would let the branch list and the repository disagree.
  const switchTo = useCallback((name: string) => {
    setConnection(name);
    setRepository(undefined);
    setQuery("");
  }, []);

  const choose = useCallback(
    (repo: GitRepository) => {
      setRepository(repo);
      onPick({
        connection,
        git: repo.htmlUrl,
        // The default branch is preselected rather than left empty. Leaving
        // `ref` out means "the repository's default branch" and would resolve
        // to the same commit today — but the picker knows the name, and a
        // written ref is what makes the project's YAML say out loud what it
        // builds.
        ref: repo.defaultBranch,
      });
    },
    [connection, onPick],
  );

  const refusal = browseRefusal(repositories.error);
  const found = repositories.data ?? [];
  const shown = useMemo(() => filterRepositories(found, query), [found, query]);

  // A repository the form no longer points at was cleared by an edit to the URL
  // field, and the branch control has to let go of it too.
  useEffect(() => {
    if (repository !== undefined && picked.git !== repository.htmlUrl) {
      setRepository(undefined);
    }
  }, [picked.git, repository]);

  return (
    <div className="k-panel k-picker">
      <div className="k-picker__head">
        <span className="k-eyebrow">Pick from a connection</span>
        <span className="k-picker__note">
          fills the fields below · the repository, its default branch, and the
          connection the build authenticates with
        </span>
      </div>

      {connections.length > 1 ? (
        <div className="k-field k-field--narrow">
          <label className="k-eyebrow" htmlFor={connectionId}>
            Connection
          </label>
          <select
            id={connectionId}
            className="k-input k-mono"
            value={connection}
            onChange={(e) => switchTo(e.target.value)}
          >
            {connections.map((c) => (
              <option key={c.name} value={c.name}>
                {c.name} · {c.provider}
              </option>
            ))}
          </select>
        </div>
      ) : null}

      {refusal !== undefined ? (
        <p className="k-picker__refusal" role="status">
          {refusal}
        </p>
      ) : null}

      {refusal === undefined && repositories.error !== undefined ? (
        <ErrorPanel
          title="Could not list this connection's repositories"
          error={repositories.error}
        />
      ) : null}

      {refusal === undefined && repositories.loading ? (
        <p className="k-picker__note">Reading the forge…</p>
      ) : null}

      {refusal === undefined &&
      !repositories.loading &&
      repositories.error === undefined &&
      found.length === 0 ? (
        <p className="k-picker__note k-mono">
          this connection can see no repositories. For a GitHub App that is an
          installation with nothing selected — add repositories to it, or paste
          the URL below.
        </p>
      ) : null}

      {found.length > FILTER_ABOVE ? (
        <div className="k-field">
          <label className="k-eyebrow" htmlFor={filterId}>
            Filter
          </label>
          <input
            id={filterId}
            className="k-input k-mono"
            value={query}
            placeholder="checkout"
            onChange={(e) => setQuery(e.target.value)}
          />
          <span className="k-field__note k-mono">
            {shown.length} of {found.length} shown
          </span>
        </div>
      ) : null}

      {shown.length > 0 ? (
        <ul className="k-picker__list">
          {shown.map((repo) => (
            <li key={repo.fullName}>
              <button
                type="button"
                className={
                  repo.htmlUrl === picked.git
                    ? "k-picker__repo k-picker__repo--picked"
                    : "k-picker__repo"
                }
                aria-pressed={repo.htmlUrl === picked.git}
                onClick={() => choose(repo)}
              >
                <span className="k-picker__repo-name k-mono">
                  {repo.fullName}
                </span>
                {repo.private ? (
                  <span className="k-chip k-mono">private</span>
                ) : null}
                <span className="k-picker__repo-branch k-mono">
                  {repo.defaultBranch || "no default branch reported"}
                </span>
              </button>
            </li>
          ))}
        </ul>
      ) : null}

      {found.length > 0 && shown.length === 0 ? (
        <p className="k-picker__note k-mono">
          nothing matches “{query}” among the {found.length} repositories this
          connection can see
        </p>
      ) : null}

      {repository !== undefined ? (
        <div className="k-field k-field--narrow">
          <label className="k-eyebrow" htmlFor={branchId}>
            Branch
          </label>
          <select
            id={branchId}
            className="k-input k-mono"
            value={picked.ref}
            onChange={(e) =>
              onPick({
                connection,
                git: repository.htmlUrl,
                ref: e.target.value,
              })
            }
          >
            {branchOptions(branches.data ?? [], picked.ref).map((branch) => (
              <option key={branch} value={branch}>
                {branch}
              </option>
            ))}
          </select>
          <span className="k-field__note k-mono">
            {branches.loading
              ? "reading the branches…"
              : `${repository.defaultBranch} is this repository's default`}
          </span>
        </div>
      ) : null}

      {picked.connection !== "" ? (
        <p className="k-picker__pinned k-mono">
          the project will carry{" "}
          <code className="k-field__code">
            source.connection: {picked.connection}
          </code>{" "}
          — its builds always authenticate through this connection, whatever
          else is added later.
        </p>
      ) : null}
    </div>
  );
}

/**
 * The client-side filter: a case-insensitive substring of `owner/name`.
 *
 * It is deliberately not a search of the forge. The list is already in the
 * browser — both listings page through everything the credential can see — so
 * filtering it here is instant and, more importantly, honest about its scope: a
 * server-side search would answer over repositories this connection cannot
 * clone.
 */
function filterRepositories(
  repositories: readonly GitRepository[],
  query: string,
): GitRepository[] {
  const needle = query.trim().toLowerCase();
  if (needle === "") return [...repositories];
  return repositories.filter((r) =>
    r.fullName.toLowerCase().includes(needle),
  );
}

/**
 * The branch names to offer, with whatever is selected guaranteed among them.
 *
 * The repository's default branch is chosen the moment a repository is picked,
 * which is before the branch listing has answered — so without this the select
 * would render with a value none of its options carry, and browsers resolve
 * that by silently showing the first option instead. The form would then say
 * one branch and the screen another.
 */
function branchOptions(
  branches: readonly string[],
  selected: string,
): string[] {
  if (selected === "" || branches.includes(selected)) return [...branches];
  return [selected, ...branches];
}
