import { useCallback, useState } from "react";

import { useAsync, useClients } from "../api/data";
import { useRun } from "../api/stream";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { formatAge } from "../components/phase";
import type {
  SecretSummary,
  SetSecretResponse,
} from "../gen/kelson/v1alpha1/secret_pb";
import { secretReference } from "../spec/documents";
import "./secrets.css";

/**
 * The Secrets an environment's references point at (#116), where the references
 * are written.
 *
 * A kelson spec carries `{secret: <name>, key: <key>}` and never a value
 * (ADR-0009, ADR-0018). This panel is the other half: the thing that writes the
 * Secret the reference names, in the environment's own namespace, beside the
 * data services because that is where the credentials of an environment are
 * already being talked about.
 *
 * Four rules, and all four are the schema's rather than this screen's:
 *
 *   - **No value is ever read back.** ListSecrets and SetSecret return names,
 *     keys, namespaces and ages; no message in secret.proto has a field a value
 *     could arrive in. So there is no "show value" control to leave out — there
 *     is nothing to show — and the panel says so in words rather than leaving a
 *     reader to wonder where their value went.
 *   - **Set merges.** Keys the form does not name are preserved, so rotating one
 *     credential leaves the others alone. The response says which keys were
 *     written and which were kept, and both are shown, because a user expecting
 *     a replacement finds out here rather than by wondering later.
 *   - **kelson touches only what it labels.** Listing is a label query, and
 *     deleting or overwriting a Secret kelson did not write is refused with
 *     `secret/not-managed`. That refusal is structured, so it reaches the reader
 *     through the same ErrorPanel every other structured refusal does, code and
 *     remediation intact — the browser must not paraphrase it.
 *   - **A form submit is the confirm.** The API's dry-run is first-class for the
 *     CLI, where a command runs unseen in a script; here the button is pressed
 *     by the person looking at the form, so a preview of "would write these
 *     keys" would be a rung to nowhere. A *delete* still confirms, because the
 *     thing it destroys is not on the screen to be re-typed.
 */
export function SecretsPanel({
  project,
  environment,
}: {
  project: string;
  environment: string;
}) {
  const clients = useClients();
  const list = useAsync(
    (signal) =>
      clients.secret.listSecrets(
        { target: { project, environment } },
        { signal },
      ),
    [clients, project, environment],
  );

  const [name, setName] = useState("");
  const [rows, setRows] = useState<KeyValue[]>([{ key: "", value: "" }]);
  const [written, setWritten] = useState<SetSecretResponse | undefined>(undefined);
  const [confirming, setConfirming] = useState<string | undefined>(undefined);
  const set = useRun();
  const remove = useRun();
  const reload = list.reload;

  const write = useCallback(() => {
    const values: Record<string, string> = {};
    for (const row of rows) {
      const key = row.key.trim();
      if (key !== "") values[key] = row.value;
    }
    set.start(async (signal) => {
      const res = await clients.secret.setSecret(
        { target: { project, environment }, name: name.trim(), values },
        { signal },
      );
      setWritten(res);
      // The values leave the page the moment they leave the browser. Keeping
      // them in inputs would be this UI holding credentials it has no reason to
      // hold, and the reader cannot be shown them again from anywhere else.
      setRows([{ key: "", value: "" }]);
      reload();
    });
  }, [clients, environment, name, project, reload, rows, set]);

  const del = useCallback(
    (target: string) => {
      remove.start(async (signal) => {
        await clients.secret.deleteSecret(
          { target: { project, environment }, name: target },
          { signal },
        );
        setConfirming(undefined);
        setWritten((prev) => (prev?.secret?.name === target ? undefined : prev));
        reload();
      });
    },
    [clients, environment, project, reload, remove],
  );

  const secrets = list.data?.secrets ?? [];
  const namespace = list.data?.namespace ?? "";
  const complete = name.trim() !== "" && rows.some((r) => r.key.trim() !== "");

  return (
    <div className="k-secrets">
      <div className="k-eyebrow">
        Secrets{list.data !== undefined ? ` (${secrets.length})` : ""}
      </div>
      <p className="k-secrets__lede">
        The Secrets kelson manages in this environment's namespace — what a{" "}
        <code className="k-mono">{"{ secret: <name>, key: <key> }"}</code>{" "}
        reference in the spec points at. kelson stores no value: the cluster
        does, and this is the masked read-back (ADR-0009).
      </p>

      {list.loading && list.data === undefined ? (
        <p className="k-env__note">Reading the environment's Secrets…</p>
      ) : null}

      {list.error !== undefined ? (
        <ErrorPanel
          title="Could not list the Secrets kelson manages"
          error={list.error}
        />
      ) : null}

      {list.data !== undefined && secrets.length === 0 ? (
        <p className="k-env__note">
          kelson manages no Secrets in namespace{" "}
          <span className="k-mono">{namespace || "—"}</span>. A namespace's TLS
          material, service-account tokens and image-pull credentials are not
          kelson's to enumerate, so they are absent rather than filtered.
        </p>
      ) : null}

      {secrets.length > 0 ? (
        <ul className="k-secrets__list">
          {secrets.map((secret) => (
            <SecretRow
              key={secret.name}
              secret={secret}
              confirming={confirming === secret.name}
              deleting={remove.running}
              onConfirm={() => setConfirming(secret.name)}
              onCancel={() => setConfirming(undefined)}
              onDelete={() => del(secret.name)}
              onEdit={() => {
                setName(secret.name);
                setRows([{ key: "", value: "" }]);
              }}
            />
          ))}
        </ul>
      ) : null}

      {remove.error !== undefined ? (
        <ErrorPanel title="Could not delete the Secret" error={remove.error} />
      ) : null}

      <form
        className="k-secrets__form"
        onSubmit={(e) => {
          e.preventDefault();
          write();
        }}
      >
        <div className="k-eyebrow">Write a Secret</div>
        <div className="k-new__pair">
          <input
            className="k-input k-mono"
            aria-label="Secret name"
            value={name}
            placeholder="checkout-db"
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        {rows.map((row, i) => (
          <div className="k-new__pair" key={i}>
            <input
              className="k-input k-mono"
              aria-label={`Key ${i + 1}`}
              value={row.key}
              placeholder="url"
              onChange={(e) =>
                setRows(rows.map((r, j) => (j === i ? { ...r, key: e.target.value } : r)))
              }
            />
            <input
              className="k-input k-mono"
              type="password"
              aria-label={`Value ${i + 1}`}
              value={row.value}
              placeholder="the value — never shown again"
              onChange={(e) =>
                setRows(
                  rows.map((r, j) => (j === i ? { ...r, value: e.target.value } : r)),
                )
              }
            />
            {rows.length > 1 ? (
              <button
                type="button"
                className="k-button"
                onClick={() => setRows(rows.filter((_, j) => j !== i))}
              >
                Remove
              </button>
            ) : null}
          </div>
        ))}

        <div className="k-actions">
          <button
            type="button"
            className="k-button"
            onClick={() => setRows([...rows, { key: "", value: "" }])}
          >
            Add key
          </button>
        </div>

        {set.error !== undefined ? (
          <ErrorPanel title="Could not write the Secret" error={set.error} />
        ) : null}

        <div className="k-deploy__confirm">
          <button
            type="submit"
            className="k-button k-button--primary"
            disabled={set.running || !complete}
          >
            {set.running ? "Writing…" : "Write the Secret"}
          </button>
          <span className="k-mono k-deploy__note">
            values are write-only — they go to the cluster's API server and
            nothing reads one back through kelson · keys not named here are kept,
            because a write merges rather than replaces
          </span>
        </div>
      </form>

      {written !== undefined ? <Written response={written} /> : null}
    </div>
  );
}

interface KeyValue {
  key: string;
  value: string;
}

function SecretRow({
  secret,
  confirming,
  deleting,
  onConfirm,
  onCancel,
  onDelete,
  onEdit,
}: {
  secret: SecretSummary;
  confirming: boolean;
  deleting: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  onDelete: () => void;
  onEdit: () => void;
}) {
  return (
    <li className="k-secrets__item">
      <div className="k-secrets__head">
        <span className="k-secrets__name k-mono">{secret.name}</span>
        <span className="k-mono k-secrets__age">{formatAge(secret.ageSeconds)}</span>
        <div className="k-actions">
          <button type="button" className="k-button" onClick={onEdit}>
            Add or rotate a key
          </button>
          {confirming ? null : (
            <button
              type="button"
              className="k-button"
              aria-label={`Delete ${secret.name}`}
              onClick={onConfirm}
            >
              Delete
            </button>
          )}
        </div>
      </div>

      <div className="k-secrets__keys">
        {secret.keys.length === 0 ? (
          <span className="k-mono k-secrets__age">no keys</span>
        ) : (
          secret.keys.map((key) => (
            <span className="k-chip k-mono" key={key}>
              {key}
            </span>
          ))
        )}
      </div>

      {confirming ? (
        <div className="k-secrets__confirm" role="alert">
          <span>
            Delete <span className="k-mono">{secret.name}</span> from{" "}
            <span className="k-mono">{secret.namespace}</span>? kelson does not
            look for referrers: a component whose env names this Secret keeps
            rendering and fails at pod start instead (ADR-0018).
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
              {deleting ? "Deleting…" : `Delete ${secret.name}`}
            </button>
          </div>
        </div>
      ) : null}
    </li>
  );
}

/**
 * What a write did, and the reference that now works.
 *
 * The snippet is the same line `kelson secret set` prints and the same bytes
 * the spec editor writes (src/spec/documents.ts: secretReference), because a
 * reader who pastes it into the YAML tab must get a document the form can still
 * read back.
 */
function Written({ response }: { response: SetSecretResponse }) {
  const secret = response.secret;
  if (secret === undefined) return null;
  const kept = secret.keys.filter((k) => !response.writtenKeys.includes(k));

  return (
    <div className="k-settled" role="status">
      <div className="k-settled__head">
        <StatusPill status="synced" label={response.dryRun ? "checked" : "written"} />
        <span className="k-settled__title">
          {response.dryRun ? "Nothing was stored" : `${secret.name} is written`}
        </span>
      </div>
      <span className="k-mono">
        keys set: {response.writtenKeys.join(", ") || "none"} · namespace{" "}
        {secret.namespace}
      </span>
      {kept.length > 0 ? (
        <span className="k-mono">
          keys kept: {kept.join(", ")} — a write merges, so the keys this one did
          not name are still there
        </span>
      ) : null}
      <span className="k-mono">
        values are write-only: nothing in kelson reads one back, and this
        read-back is names, keys and ages only. Reading a value is `kubectl get
        secret`, with the cluster's own RBAC behind it.
      </span>

      <div className="k-eyebrow">Reference a key from a component's env</div>
      <div className="k-secrets__snippets">
        {response.writtenKeys.map((key) => (
          <Copyable key={key} value={secretReference(secret.name, key)} />
        ))}
      </div>
    </div>
  );
}

