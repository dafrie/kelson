import { useCallback, useEffect, useId, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";

import { notifyUnauthenticated } from "../api/auth";
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
 * # Two connect paths, and only one of them is a plain RPC
 *
 * **Connect GitHub** is the flagship (ADR-0033 decision 2) and it still ends in
 * a browser redirect, not a call: the server builds an app manifest, GitHub
 * asks the user to approve creating the app, and a one-time code comes back to
 * the server's own callback, which mints the Secret and writes the CR. The
 * credential is handed to the *server* by GitHub and never passes through this
 * page, which is exactly why `CreateConnection` has no app variant.
 *
 * What precedes that redirect is one authenticated step (#248's fixed
 * contract): the button is `POST /forge/github/manifest/session`, carrying the
 * session cookie the same way every RPC does (`credentials: "same-origin"`,
 * `src/api/clients.ts`), which answers a short-lived, single-use `startUrl`.
 * Only that URL is navigated to, with `window.location.assign` rather than a
 * `<Link>` or a static anchor `href` — the flow leaves this app for GitHub, so
 * it has to be a real, top-level navigation, and it must go to a ticket this
 * click actually minted rather than to a bare path a second click, a bookmark
 * or a prefetcher could replay. A 401 here means the same thing it means
 * everywhere else in this UI (docs/server.md): the shared session password is
 * wrong or has expired, and the panel says so rather than a bare error code.
 *
 * The callback lands back here as `/connections?connected=…` — with `install`
 * beside it for the post-install hop ADR-0033 decision 2 step 3 still asks for
 * — or `/connections?error=…&message=…` on a refusal. `useManifestOutcome`
 * below reads those once, renders them as a banner, and clears them from the
 * URL (`replace: true`) so a refresh shows the connection list rather than
 * replaying an announcement about a flow that already finished.
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
 * Where the button's fetch goes before anything is navigated to (#248's fixed
 * contract, agreed byte-identical with the server-side agent implementing it).
 *
 * `POST`, not `GET`: a ticket is worth an authenticated request, not something
 * a prefetcher, a link preview or a stray GET could mint by accident. The
 * response's `startUrl` — `/forge/github/manifest/start?ticket=…` — is where
 * the app-manifest flow actually starts; that path is not a constant of its
 * own here because the UI never builds it, it only navigates to what the
 * server minted.
 */
const MANIFEST_SESSION = "/forge/github/manifest/session";

/**
 * Mints a one-time ticket for the app-manifest flow and says where to send the
 * browser next.
 *
 * Authenticated the same way every RPC in this UI is — a same-origin fetch
 * carrying the session cookie, `src/api/clients.ts`'s own rule restated
 * because a plain REST endpoint has no ConnectRPC `Transport` to share. A 401
 * is reported to the same listener the RPC transport's interceptor uses
 * (`notifyUnauthenticated`, `src/api/auth.tsx`) so an expired session flips the
 * whole app to logged-out exactly as an RPC 401 would, and it is also said
 * here in words, because the caller is one button press away from being told
 * nothing more specific than "unauthorized".
 *
 * Until the server side of #248 lands this 404s, which needs no special case:
 * a non-2xx status is a failure like any other fetch's, and it reaches the
 * page through the same `ErrorPanel` every other refusal does.
 */
async function startManifestSession(signal: AbortSignal): Promise<string> {
  const res = await fetch(MANIFEST_SESSION, {
    method: "POST",
    credentials: "same-origin",
    signal,
  });
  if (res.status === 401) {
    notifyUnauthenticated();
    throw new Error(
      "the session password is wrong or has expired — sign in again, then press Connect GitHub",
    );
  }
  if (!res.ok) {
    throw new Error(`${MANIFEST_SESSION} returned ${res.status} ${res.statusText}`);
  }
  const body = (await res.json()) as { startUrl?: string };
  if (!body.startUrl) {
    throw new Error(`${MANIFEST_SESSION} answered with no startUrl`);
  }
  return body.startUrl;
}

/** The four params the manifest callback's redirect can land here with. */
interface ManifestOutcome {
  connected: string | undefined;
  install: string | undefined;
  error: string | undefined;
  message: string | undefined;
}

/**
 * What the manifest callback said, read once from the query string
 * `internal/forgehttp`'s `redirectToConnections` landed here with, and cleared
 * from the URL the moment it is captured.
 *
 * The clearing is `replace: true`, not a plain navigation: a `push` would
 * leave a back button that lands on the announcement a second time, and
 * leaving the params in place at all would mean a refresh re-announces a flow
 * that already finished. Reading happens once, at mount — the `[]` dependency
 * list is deliberate, not an omission — because these params describe the
 * redirect that just landed, not something to keep re-deriving as the reader
 * clicks around the same screen afterwards.
 */
function useManifestOutcome(): ManifestOutcome | undefined {
  const [params, setSearchParams] = useSearchParams();
  const [outcome, setOutcome] = useState<ManifestOutcome | undefined>(undefined);

  useEffect(() => {
    const connected = params.get("connected") ?? undefined;
    const install = params.get("install") ?? undefined;
    const error = params.get("error") ?? undefined;
    const message = params.get("message") ?? undefined;
    if (
      connected === undefined &&
      install === undefined &&
      error === undefined &&
      message === undefined
    ) {
      return;
    }

    setOutcome({ connected, install, error, message });
    const next = new URLSearchParams(params);
    next.delete("connected");
    next.delete("install");
    next.delete("error");
    next.delete("message");
    setSearchParams(next, { replace: true });
    // `[]` is deliberate: this reads the params the redirect landed with,
    // once, rather than re-deriving them as the reader clicks around after.
  }, []);

  return outcome;
}

export function ConnectionsPage() {
  const clients = useClients();
  const list = useAsync(
    (signal) => clients.gitConnection.listConnections({}, { signal }),
    [clients],
  );

  const [confirming, setConfirming] = useState<string | undefined>(undefined);
  const [removed, setRemoved] = useState<Removed | undefined>(undefined);
  const remove = useRun();
  const manifestStart = useRun();
  const outcome = useManifestOutcome();
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

  const startManifest = useCallback(() => {
    manifestStart.start(async (signal) => {
      const startUrl = await startManifestSession(signal);
      // A real top-level navigation, not `navigate()`: the ticket is
      // single-use and the destination is off this app entirely.
      window.location.assign(startUrl);
    });
  }, [manifestStart]);

  const connections = list.data?.connections ?? [];

  return (
    <>
      <div className="k-page-head">
        <h1>Git connections</h1>
        {/* Not an anchor: the ticket this navigates with is minted per click
            by an authenticated fetch, so a static href would either go stale
            or have to be pre-fetched on every render for nothing. It stays in
            the head whether the list is full or empty, exactly as "New
            project" does — it is the primary way in. */}
        <button
          type="button"
          className="k-button k-button--primary"
          onClick={startManifest}
          disabled={manifestStart.running}
        >
          {manifestStart.running ? "Connecting…" : "Connect GitHub"}
        </button>
      </div>

      <div className="k-page-sub">
        <span>
          {connections.length}{" "}
          {connections.length === 1 ? "connection" : "connections"}
        </span>
        <span>·</span>
        <span>the forges kelson can clone from</span>
      </div>

      {manifestStart.error !== undefined ? (
        <ErrorPanel
          title="Could not start the GitHub connection"
          error={manifestStart.error}
        />
      ) : null}

      {outcome !== undefined ? <ManifestOutcomeBanner outcome={outcome} /> : null}

      {/* The visibility sentence is not decoration: the owner column looks
          like a permission and is not one until #231 gives it a subject. */}
      <p className="k-note">
        A connection names a forge and the Secret holding its credential, never
        the credential itself. <strong>Every connection below is visible to
        everyone on this instance</strong>, and anyone can test or delete it.
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
        <EmptyState title="No connections yet">
          Without one, kelson can clone public repositories and nothing else.
          Use “Connect GitHub” above, or the token form below.
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
 * A reader who is asked to press a button and leave the app for GitHub is
 * entitled to know where the credential ends up — the redirect makes this
 * unrecoverable to read afterwards, so it is said in two lines beforehand.
 */
function ManifestNote() {
  return (
    <div className="k-panel k-connect">
      <div className="k-eyebrow">Connect GitHub</div>
      <p className="k-connect__body">
        GitHub asks you to approve an app of your own, then sends you back here.
        Its key is written to this instance and never leaves it. You pick the
        repositories the app may see last.
      </p>
    </div>
  );
}

/**
 * The banner that answers "what just happened" for a browser that returned
 * from the app-manifest flow.
 *
 * `error` and `connected` are mutually exclusive on the wire
 * (`internal/forgehttp`'s two `redirectToConnections` call sites never set
 * both), so this reads as an if/else rather than stacking two banners that
 * could never both be true.
 */
function ManifestOutcomeBanner({ outcome }: { outcome: ManifestOutcome }) {
  if (outcome.error !== undefined) {
    const detail =
      outcome.message?.trim() || `the flow reported "${outcome.error}" with no further detail`;
    return (
      <ErrorPanel title="Connect GitHub did not finish" error={new Error(detail)} />
    );
  }
  if (outcome.connected !== undefined) {
    return <ManifestConnected name={outcome.connected} install={outcome.install} />;
  }
  return null;
}

/**
 * The app exists on GitHub and its key is stored here — the row below, named
 * the same, is the proof. What it does not yet have is a reader.
 *
 * `install` names ADR-0033 decision 2 step 3, the half of the ceremony that
 * happens on GitHub's own site: no repositories are chosen yet, so the
 * connection reads as unreachable on its row below until that finishes, and
 * the button here is the honest continuation rather than a second surprise.
 * The link opens in a new tab because GitHub's installation page has nowhere
 * configured to send the browser back to — the `installation` webhook is what
 * closes this loop, not a redirect — so closing this tab must not cost the
 * connections list.
 */
function ManifestConnected({
  name,
  install,
}: {
  name: string;
  install: string | undefined;
}) {
  return (
    <div className="k-settled" role="status">
      <div className="k-settled__head">
        <StatusPill status="synced" label="connected" />
        <span className="k-settled__title">{name} is connected</span>
      </div>
      <span className="k-mono">
        the app exists on GitHub and its key is stored here — {name}, in the
        list below, is it.
      </span>
      {install !== undefined ? (
        <>
          <span className="k-mono">
            no repositories are chosen yet, so its row shows unreachable. Pick
            them on GitHub, then “Test connection” confirms it.
          </span>
          <a
            className="k-button k-button--primary"
            href={install}
            target="_blank"
            rel="noreferrer"
          >
            Install the app on GitHub
          </a>
        </>
      ) : null}
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
            their next build has no credential and fails. Point each at another
            connection with <code>spec.source.connection</code>, or make one
            that matches the host again.
          </span>
        </>
      ) : (
        <span className="k-mono">
          no stored project resolved its source through it
        </span>
      )}

      <span className="k-mono">
        the Secret it named was not deleted — kelson did not write it.
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
          a recorded owner, not a boundary: everyone on this instance can edit
          or delete this connection
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
            resolving through it keep working until their next build, and are
            named once it is gone. The Secret it references stays.
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
            Works with any forge. Put the token in a Secret in{" "}
            <span className="k-mono">kelson-system</span> — key{" "}
            <code className="k-mono">token</code>, optionally{" "}
            <code className="k-mono">username</code> — then name that Secret
            here. Either order works; a connection naming a Secret that does not
            exist yet reports not-ready until it does.
          </p>

          <div className="k-new__row">
            <Field
              label="Name"
              value={form.name}
              onChange={(v) => update("name", v, "name")}
              placeholder="acme-github"
              problem={problemFor("name")}
              note="what a project names in spec.source.connection"
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
              note="the forge's base URL, scheme included — a self-hosted GHE, GitLab or Forgejo sets its own"
            />

            <Field
              label="Secret"
              value={form.secretRef}
              onChange={(v) => update("secretRef", v, "secretRef")}
              placeholder="acme-git-token"
              problem={problemFor("secretRef")}
              note="the name of a Secret in kelson-system — a reference, never a token"
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
              owned by the instance · visible to everyone on it
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
        “Test connection” on the row above asks the forge now.
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
          ? "github.com or GitHub Enterprise: clones, webhooks, commit statuses and PR comments"
          : "any git host reachable with a token: private clones only, no webhooks or commit statuses"}
      </span>
    </div>
  );
}
