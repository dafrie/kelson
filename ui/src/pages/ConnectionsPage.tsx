import { useCallback, useId, useRef, useState } from "react";

import { useAsync, useClients } from "../api/data";
import { useRun } from "../api/stream";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { EmptyState, LoadingState } from "../components/States";
import type {
  DeleteConnectionResponse,
  GitConnection,
  TestConnectionResponse,
} from "../gen/kelson/v1alpha1/gitconnection_pb";
import {
  appIdentity,
  authLabel,
  connectionHealth,
  EMPTY_FORM,
  formProblems,
  hostForProvider,
  ownerIsInstance,
  ownerLabel,
  PROVIDERS,
  repositoriesLabel,
  type ConnectionField,
  type ConnectionForm,
  type GitProviderName,
} from "./connections";

/**
 * The forges kelson can pull from: what is connected, and the two ways to
 * connect one (ADR-0033).
 *
 * A connection is a forge, a host and the *name* of a Secret. No screen in this
 * UI has ever carried a credential value and this one does not start: every RPC
 * behind it takes and returns references and provider-reported fact, and there
 * is no field in `gitconnection.proto` a token could travel in either
 * direction (ADR-0009, ADR-0033 decision 1). What the token form below asks for
 * is the name of a Secret that already exists in `kelson-system`.
 *
 * # Two connect paths, and only one of them is an RPC
 *
 * **Connect GitHub** is the flagship (ADR-0033 decision 2) and it is a browser
 * redirect, not a call: the server builds an app manifest, GitHub asks the user
 * to approve creating the app, and a one-time code comes back to the server's
 * own callback, which mints the Secret and writes the CR. The credential is
 * handed to the *server* by GitHub and never passes through this page, which is
 * exactly why `CreateConnection` has no app variant. So the button is an
 * ordinary anchor to a server path and the SPA router is deliberately not
 * involved — a `<Link>` would keep the navigation client-side and nothing would
 * happen.
 *
 * That endpoint is server-side work that lands in a later slice, and the copy
 * beside the button says so rather than letting a 404 be the first thing anyone
 * learns about it. Two things will make it answer: the handler itself, and a
 * `/forge/` entry in the dev proxy (`ui/vite.config.ts` forwards
 * `/kelson.v1alpha1.`, `/auth/` and `/healthz` and nothing else), so under
 * `npm run dev` the link reaches Vite rather than kelson-server until then.
 *
 * **A token connection** is the universal fallback and the only path for every
 * forge whose adapter has not landed. It is one `CreateConnection` — four
 * fields, exactly the request's own — and the submit is the confirm, as it is
 * in the Secrets panel: there is no rendered artifact to preview, so a dry-run
 * rung would be a preview of a form the user is already looking at.
 *
 * # Ownership is shown and is not a boundary
 *
 * ADR-0033 decision 6 fixes the ownership semantics now and enforces none of
 * them until principals exist (#231). The recorded owner is displayed, and the
 * page says in words that every connection here is visible to — and deletable
 * by — everyone who can reach this server. Anything else would be authorization
 * theatre, which is the failure ADR-0031 refuses.
 *
 * # Where the server's refusals go
 *
 * Into one `ErrorPanel`, code and remediation intact, rather than pinned under
 * the input that caused them. The paths a `GitConnection` refusal carries are
 * the *document's* (`$.spec.auth.token.secretRef`), not this form's four
 * fields, and the panel already prints the field beside the code — so nothing
 * is lost, and no mapping has to guess at a handler that is still being
 * written.
 */

/**
 * Where the app-manifest flow starts (ADR-0033 decision 2, #248).
 *
 * A path on kelson-server, not a URL: the flow's redirect URL has to come back
 * to this instance, and the server is the only thing that knows what it is
 * reachable at. The manifest itself is built there too, because it carries the
 * webhook URL and the permission set — none of which is a browser's to decide.
 */
const MANIFEST_START = "/forge/github/manifest/start";

export function ConnectionsPage() {
  const clients = useClients();
  const list = useAsync(
    (signal) => clients.gitConnection.listConnections({}, { signal }),
    [clients],
  );

  const [confirming, setConfirming] = useState<string | undefined>(undefined);
  const [removed, setRemoved] = useState<Removed | undefined>(undefined);
  const remove = useRun();
  const reload = list.reload;

  const del = useCallback(
    (name: string) => {
      remove.start(async (signal) => {
        const res = await clients.gitConnection.deleteConnection(
          { name },
          { signal },
        );
        setConfirming(undefined);
        setRemoved({ name, response: res });
        reload();
      });
    },
    [clients, reload, remove],
  );

  const connections = list.data?.connections ?? [];

  return (
    <>
      <div className="k-page-head">
        <h1>Git connections</h1>
        {/* An anchor and not a Link: the flow leaves this app for GitHub and
            comes back through the server's own callback, so the navigation has
            to be a real one. It stays in the head whether the list is full or
            empty, exactly as "New project" does — it is the primary way in. */}
        <a className="k-button k-button--primary" href={MANIFEST_START}>
          Connect GitHub
        </a>
      </div>

      <div className="k-page-sub">
        <span>
          {connections.length}{" "}
          {connections.length === 1 ? "connection" : "connections"}
        </span>
        <span>·</span>
        <span>what this instance can clone, and act as, on a forge</span>
      </div>

      <p className="k-note">
        A connection names a forge, its host and the Secret holding the
        credential — never the credential itself. kelson reads that Secret in
        the planes that have cluster access to mint short-lived tokens; nothing
        here can show you a token, and no field on this wire could carry one
        (ADR-0009, ADR-0033). <strong>Every connection below is visible to
        everyone on this instance</strong>, and anyone who can reach this server
        can test or delete any of them. The owner column records who a
        connection will belong to; it enforces nothing until principals exist
        (#231).
      </p>

      <ManifestNote />

      {list.loading && list.data === undefined ? (
        <LoadingState what="git connections" />
      ) : null}

      {list.error !== undefined ? (
        <ErrorPanel title="Cannot list git connections" error={list.error} />
      ) : null}

      {removed !== undefined ? <Removal removed={removed} /> : null}

      {remove.error !== undefined ? (
        <ErrorPanel title="Could not delete the connection" error={remove.error} />
      ) : null}

      {list.data !== undefined && connections.length === 0 ? (
        <EmptyState title="No git connections — kelson can clone public repositories and nothing else">
          A connection is what lets this instance clone a private repository,
          and what a preview, a commit status or a pull-request comment is made
          with. There are two ways to make one: “Connect GitHub” above runs
          GitHub's app-manifest flow and creates a per-instance app, and the
          token form below takes a personal or project access token that is
          already in a Secret — the only path for a forge whose adapter has not
          landed.
        </EmptyState>
      ) : null}

      {connections.length > 0 ? (
        <ul className="k-connections">
          {connections.map((connection) => (
            <ConnectionRow
              key={connection.name}
              connection={connection}
              confirming={confirming === connection.name}
              deleting={remove.running}
              onConfirm={() => setConfirming(connection.name)}
              onCancel={() => setConfirming(undefined)}
              onDelete={() => del(connection.name)}
            />
          ))}
        </ul>
      ) : null}

      <CreateForm onCreated={reload} />
    </>
  );
}

/**
 * What pressing “Connect GitHub” actually does, said before it is pressed.
 *
 * The flow leaves for github.com and finishes on a server endpoint this slice
 * does not implement. A button that silently 404s teaches somebody that the
 * feature is broken; saying which half is missing costs three sentences and
 * keeps the token form below legible as the path that works today.
 */
function ManifestNote() {
  return (
    <div className="k-panel k-connect">
      <div className="k-eyebrow">Connect GitHub · the app-manifest flow</div>
      <p className="k-connect__body">
        kelson sends you to GitHub with a manifest for an app of your own. You
        approve creating it on your account or an organisation, GitHub redirects
        back here with a one-time code, and the server exchanges it for the app
        id, the private key and the webhook secret — which it writes into a
        Secret on this instance. The key never leaves it and no kelson-operated
        relay is involved. You then pick the repositories the app may see.
      </p>
      <p className="k-connect__body k-mono">
        The endpoint that starts it —{" "}
        <code className="k-field__code">{MANIFEST_START}</code> — is
        server-side and lands in a later slice of #248. Until it does, the
        button answers 404 and a token connection is the path that works.
      </p>
    </div>
  );
}

interface Removed {
  name: string;
  response: DeleteConnectionResponse;
}

/**
 * What a delete broke, named rather than discovered later.
 *
 * `DeleteConnection` does not refuse because projects resolve through the
 * connection — a connection is deleted because it is wrong, and refusing until
 * every project is edited would make a leaked credential harder to revoke than
 * to keep. What it does instead is name them, and this is where they are named:
 * these are the builds that start failing, on screen, at the moment the choice
 * was made.
 */
function Removal({ removed }: { removed: Removed }) {
  const affected = removed.response.affectedProjects;
  const broken = affected.length > 0;
  return (
    <div
      className={broken ? "k-settled k-settled--other" : "k-settled"}
      role="status"
    >
      <div className="k-settled__head">
        <StatusPill status={broken ? "degraded" : "synced"} label="deleted" />
        <span className="k-settled__title">
          {broken
            ? `${removed.name} is gone, and ${affected.length} ${
                affected.length === 1 ? "project" : "projects"
              } resolved through it`
            : `${removed.name} is gone`}
        </span>
      </div>

      {broken ? (
        <>
          <div className="k-connections__affected">
            {affected.map((project) => (
              <span className="k-chip k-mono" key={project}>
                {project}
              </span>
            ))}
          </div>
          <span className="k-mono">
            their next build has no credential for the repository and fails.
            Point each at another connection with{" "}
            <code>spec.source.connection</code>, or make one that matches the
            host again.
          </span>
        </>
      ) : (
        <span className="k-mono">
          no stored project resolved its source through it
        </span>
      )}

      <span className="k-mono">
        the Secret it named was not deleted: kelson did not write it, and
        removing somebody else's object on the way out is not this call's to do.
      </span>
    </div>
  );
}

function ConnectionRow({
  connection,
  confirming,
  deleting,
  onConfirm,
  onCancel,
  onDelete,
}: {
  connection: GitConnection;
  confirming: boolean;
  deleting: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  onDelete: () => void;
}) {
  const clients = useClients();
  const probe = useRun();
  const [result, setResult] = useState<TestConnectionResponse | undefined>(
    undefined,
  );

  const test = useCallback(() => {
    probe.start(async (signal) => {
      const res = await clients.gitConnection.testConnection(
        { name: connection.name },
        { signal },
      );
      setResult(res);
    });
  }, [clients, connection.name, probe]);

  const health = connectionHealth(connection);
  const repositories = repositoriesLabel(connection);
  const app = appIdentity(connection);

  return (
    <li className="k-panel k-connection">
      <div className="k-connection__head">
        <span className="k-connection__name">{connection.name}</span>
        <span className="k-chip k-mono">{connection.provider}</span>
        <span className="k-chip k-mono">{authLabel(connection.authKind)}</span>
        <StatusPill status={health.status} label={health.label} />
      </div>

      <div className="k-kv">
        <span className="k-kv__key">host</span>
        <span>{connection.host || "—"}</span>
        <span className="k-kv__key">account</span>
        <span>{connection.account || "not reported — no probe has succeeded"}</span>
        <span className="k-kv__key">repositories</span>
        <span>{repositories ?? "not reported — no probe has succeeded"}</span>
        <span className="k-kv__key">secret</span>
        <span>{connection.secretRef || "—"} · namespace kelson-system</span>
        <span className="k-kv__key">owner</span>
        <span>{ownerLabel(connection.owner)}</span>
        {app !== undefined ? (
          <>
            <span className="k-kv__key">app</span>
            <span>{app}</span>
          </>
        ) : null}
      </div>

      <p className="k-connection__health k-mono">{health.detail}</p>

      {/* Said on the row that carries a principal, because that is the row
          somebody would otherwise read as a permission (ADR-0033 decision 6). */}
      {ownerIsInstance(connection.owner) ? null : (
        <p className="k-connection__owner k-mono">
          a recorded owner and not a boundary: this connection is visible to
          everyone on this instance, and everyone can edit or delete it, until
          tenancy gives the field a subject (#231)
        </p>
      )}

      <div className="k-actions">
        <button
          type="button"
          className="k-button"
          aria-label={`Test ${connection.name}`}
          onClick={test}
          disabled={probe.running}
        >
          {probe.running ? "Probing…" : "Test connection"}
        </button>
        {confirming ? null : (
          <button
            type="button"
            className="k-button"
            aria-label={`Delete ${connection.name}`}
            onClick={onConfirm}
          >
            Delete
          </button>
        )}
      </div>

      {probe.error !== undefined ? (
        <ErrorPanel title="The probe could not be made" error={probe.error} />
      ) : null}

      {result !== undefined ? <Probe result={result} /> : null}

      {confirming ? (
        <div className="k-connection__confirm" role="alert">
          <span>
            Delete <span className="k-mono">{connection.name}</span>? Projects
            that resolve through it are not checked first and the delete is not
            blocked by them — the response names them afterwards, because a
            connection is deleted when it is wrong and a leaked credential must
            not be harder to revoke than to keep. The Secret it references stays
            where it is.
          </span>
          <div className="k-actions">
            <button type="button" className="k-button" onClick={onCancel}>
              Cancel
            </button>
            <button
              type="button"
              className="k-button k-button--danger"
              onClick={onDelete}
              disabled={deleting}
            >
              {deleting ? "Deleting…" : `Delete ${connection.name}`}
            </button>
          </div>
        </div>
      ) : null}
    </li>
  );
}

/**
 * One probe's answer, beside the connection it was about.
 *
 * The probe is made server-side with the Secret the connection references, and
 * what comes back is the same triple the status subresource carries:
 * reachability, the account the credential acts as, and a count. A failure is
 * an *answer* here rather than an error — the RPC succeeded and the forge said
 * no — so it renders as a result with the forge's own refusal, and only a
 * failure to ask at all reaches the error panel above.
 */
function Probe({ result }: { result: TestConnectionResponse }) {
  const repositories = `${result.repositories} ${
    result.repositories === 1 ? "repository" : "repositories"
  }`;
  return (
    <div className="k-connection__probe" role="status">
      <div className="k-settled__head">
        <StatusPill
          status={result.reachable ? "synced" : "failed"}
          label={result.reachable ? "reachable" : "unreachable"}
        />
        <span className="k-mono">
          {result.reachable
            ? `${result.account || "an account the provider did not name"} · ${repositories}`
            : "the forge did not authenticate this credential"}
        </span>
      </div>
      {result.message ? (
        <p className="k-connection__health k-mono">{result.message}</p>
      ) : null}
    </div>
  );
}

/**
 * The token path: four fields, which are `CreateConnectionRequest`'s own.
 *
 * There is no owner picker. `owner` defaults to the instance and the instance
 * is the only kind that means anything today, so a control offering `user` or
 * `team` would be asking somebody to make a choice that changes nothing —
 * ADR-0033 decision 6's field is stored and shown, not solicited, until it has
 * a subject.
 *
 * The idempotency key is minted once per attempt and kept across a retry of the
 * same four values, which is what the field is for: a create is not convergent,
 * and a replay after a timeout must be the same operation rather than a second
 * connection or an "already exists" for a call that succeeded.
 */
function CreateForm({ onCreated }: { onCreated: () => void }) {
  const clients = useClients();
  const [form, setForm] = useState<ConnectionForm>(EMPTY_FORM);
  const [touched, setTouched] = useState<Partial<Record<ConnectionField, true>>>({});
  const [attempted, setAttempted] = useState(false);
  const [created, setCreated] = useState<GitConnection | undefined>(undefined);
  const key = useRef<string | undefined>(undefined);
  const create = useRun();

  const update = useCallback(
    <K extends keyof ConnectionForm>(
      field: K,
      value: ConnectionForm[K],
      touchedField?: ConnectionField,
    ) => {
      setForm((prev) => ({ ...prev, [field]: value }));
      if (touchedField !== undefined) {
        setTouched((prev) => ({ ...prev, [touchedField]: true }));
      }
      // The answer on screen described a form that no longer exists.
      setCreated(undefined);
      key.current = undefined;
    },
    [],
  );

  const problems = formProblems(form);
  const problemFor = (field: ConnectionField): string | undefined => {
    if (!attempted && touched[field] === undefined) return undefined;
    return problems.find((p) => p.field === field)?.message;
  };

  const submit = useCallback(() => {
    setAttempted(true);
    if (formProblems(form).length > 0) return;
    if (key.current === undefined) key.current = crypto.randomUUID();
    const idempotencyKey = key.current;
    create.start(async (signal) => {
      const res = await clients.gitConnection.createConnection(
        {
          name: form.name.trim(),
          provider: form.provider,
          host: form.host.trim(),
          secretRef: form.secretRef.trim(),
          idempotencyKey,
        },
        { signal },
      );
      setCreated(res.connection);
      setForm(EMPTY_FORM);
      setTouched({});
      setAttempted(false);
      key.current = undefined;
      onCreated();
    });
  }, [clients, create, form, onCreated]);

  return (
    <section className="k-section">
      <div className="k-eyebrow">Connect with a token</div>
      <div className="k-section__body">
        <form
          className="k-panel k-connections__form"
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <p className="k-connect__body">
            The fallback every forge answers, and the only path for one whose
            adapter has not landed: a personal, project or deploy token. Write
            the token into a Secret in <span className="k-mono">kelson-system</span>{" "}
            first — <code className="k-mono">token</code>, and optionally{" "}
            <code className="k-mono">username</code> — then name it here. A
            connection that names a Secret which does not exist yet is created
            and reports not-ready with the reason, so either order works.
          </p>

          <div className="k-new__row">
            <Field
              label="Name"
              value={form.name}
              onChange={(v) => update("name", v, "name")}
              placeholder="acme-github"
              problem={problemFor("name")}
              note="the CR's metadata.name in kelson-system, and what spec.source.connection names when a project disambiguates"
            />

            <ProviderField
              value={form.provider}
              onChange={(provider) => {
                setForm((prev) => ({
                  ...prev,
                  provider,
                  host: hostForProvider(provider, prev.host),
                }));
                setCreated(undefined);
                key.current = undefined;
              }}
            />

            <Field
              label="Host"
              value={form.host}
              onChange={(v) => update("host", v, "host")}
              placeholder="https://github.com"
              problem={problemFor("host")}
              note="the forge base URL — a self-hosted GHE, GitLab or Forgejo sets its own, and it is half of the host match that resolves a project's source"
            />

            <Field
              label="Secret"
              value={form.secretRef}
              onChange={(v) => update("secretRef", v, "secretRef")}
              placeholder="acme-git-token"
              problem={problemFor("secretRef")}
              note="the name of an existing Secret in kelson-system — a reference, never a value. Nothing on this page accepts a token."
            />
          </div>

          {create.error !== undefined ? (
            <ErrorPanel
              title="The server would not create this connection"
              error={create.error}
            />
          ) : null}

          <div className="k-deploy__confirm">
            <button
              type="submit"
              className="k-button k-button--primary"
              disabled={create.running}
            >
              {create.running ? "Connecting…" : "Create the connection"}
            </button>
            <span className="k-mono k-deploy__note">
              owned by the instance · visible to everyone on it · no credential
              value leaves this page, because there is no field on the wire for
              one
            </span>
          </div>
        </form>

        {created !== undefined ? <Created connection={created} /> : null}
      </div>
    </section>
  );
}

function Created({ connection }: { connection: GitConnection }) {
  const health = connectionHealth(connection);
  return (
    <div className="k-settled" role="status">
      <div className="k-settled__head">
        <StatusPill status="synced" label="created" />
        <span className="k-settled__title">{connection.name} is connected</span>
      </div>
      <span className="k-mono">
        {connection.provider} · {connection.host} · secret{" "}
        {connection.secretRef}
      </span>
      <span className="k-mono">{health.detail}</span>
      <span className="k-mono">
        “Test connection” on the row above asks the forge now, rather than
        reading a condition written at some earlier reconcile.
      </span>
    </div>
  );
}

function Field({
  label,
  value,
  onChange,
  placeholder,
  note,
  problem,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
  note: string;
  problem: string | undefined;
}) {
  const id = useId();
  return (
    <div className="k-field">
      <label className="k-eyebrow" htmlFor={id}>
        {label}
      </label>
      <input
        id={id}
        className="k-input k-mono"
        value={value}
        placeholder={placeholder}
        aria-invalid={problem !== undefined ? true : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {problem !== undefined ? (
        <span className="k-field__problem k-mono" role="alert">
          {problem}
        </span>
      ) : null}
      <span className="k-field__note k-mono">{note}</span>
    </div>
  );
}

/**
 * The provider, as a select over the enum internal/model closes.
 *
 * Two values, and the second one is honest about what it costs: `generic` is
 * private clones and nothing else — no repo picker, no webhook, no commit
 * status — because those are optional capabilities the GitHub adapter has and a
 * bare token against an arbitrary host does not (ADR-0033 decision 3).
 */
function ProviderField({
  value,
  onChange,
}: {
  value: GitProviderName;
  onChange: (value: GitProviderName) => void;
}) {
  const id = useId();
  return (
    <div className="k-field">
      <label className="k-eyebrow" htmlFor={id}>
        Provider
      </label>
      <select
        id={id}
        className="k-select k-mono"
        value={value}
        onChange={(e) => onChange(e.target.value as GitProviderName)}
      >
        {PROVIDERS.map((provider) => (
          <option key={provider} value={provider}>
            {provider}
          </option>
        ))}
      </select>
      <span className="k-field__note k-mono">
        {value === "github"
          ? "github.com or GitHub Enterprise with a token — the adapter that also does webhooks, statuses and PR comments"
          : "any git host reachable with a token: private clones and nothing else, because a repo picker and a webhook are the adapter's, not the token's"}
      </span>
    </div>
  );
}
