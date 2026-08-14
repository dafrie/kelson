import { useAsync, useClients } from "../api/data";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { formatAge } from "../components/phase";
import type {
  ListPreviewsResponse,
  Preview,
  PreviewSettings,
} from "../gen/kelson/v1alpha1/preview_pb";
import {
  changeRequestUrl,
  lifecycleLine,
  previewHeadline,
  previewStatus,
  settingsLine,
  shortSha,
} from "./previews";
import "./previews.css";

/**
 * The pull requests running as child environments of this one (ADR-0017).
 *
 * A preview is an environment kelson did not record: no Environment document
 * describes it, no delivery history entry exists for it, and what created it is
 * flux-operator reacting to a label on a change request. So this section is not
 * a list of things kelson deployed — it is a read of what flux-operator made out
 * of the manifests kelson published, and it says so.
 *
 * Four things it refuses to do:
 *
 *   - **Offer a button.** There is no "create preview" and no "rebuild": a
 *     preview appears when CI runs `kelson preview publish` for a change request
 *     and disappears when the change request closes. A control here would either
 *     lie about what it did or do the publisher's job without the checkout and
 *     the image reference CI already has (ADR-0017 decision 8).
 *   - **Hide the delivery-mode gate.** An environment in direct mode that
 *     declares previews gets the server's own render/previews-require-flux,
 *     code, message and remediation intact, in the same panel every other
 *     structured refusal reaches the reader through.
 *   - **Read an empty list as "nothing is wrong".** flux-operator missing, the
 *     lifecycle pair never delivered and a poller that cannot reach the forge
 *     all produce no previews, and each gets its own sentence (previews.ts).
 *   - **Show a hostname the spec asks for.** Every hostname here carries the
 *     change request in its first DNS label, because a preview never serves the
 *     parent's hostname (ADR-0017 decision 11). They are read from the
 *     preview's own HTTPRoutes rather than derived, so what is shown is what is
 *     served.
 */
export function Previews({
  project,
  environment,
}: {
  project: string;
  environment: string;
}) {
  const clients = useClients();
  const list = useAsync(
    (signal) =>
      clients.preview.listPreviews(
        { spec: { spec: { case: "project", value: project } }, environment },
        { signal },
      ),
    [clients, project, environment],
  );

  const data = list.data;
  const settings = data?.settings;
  const previews = data?.previews ?? [];

  return (
    <div className="k-previews">
      <div className="k-eyebrow">
        Previews{settings !== undefined ? ` (${previews.length})` : ""}
      </div>

      {list.loading && data === undefined ? (
        <p className="k-env__note">Reading this environment's previews…</p>
      ) : null}

      {list.error !== undefined ? (
        <ErrorPanel
          title="Could not read this environment's previews"
          error={list.error}
        />
      ) : null}

      {/* The gate and any other structured finding, verbatim. */}
      {data !== undefined && data.errors.length > 0 ? (
        <ErrorPanel
          title="This environment cannot run previews"
          errors={data.errors}
        />
      ) : null}

      {data !== undefined && settings === undefined && data.errors.length === 0 ? (
        <NotConfigured mode={data.mode} />
      ) : null}

      {settings !== undefined ? (
        <Configured response={data} settings={settings} previews={previews} />
      ) : null}
    </div>
  );
}

/**
 * The empty state, which is where most readers meet this feature: what previews
 * are, the two halves that have to be in place, and where the rest is written.
 */
function NotConfigured({ mode }: { mode: string }) {
  return (
    <div className="k-previews__empty">
      <p className="k-previews__lede">
        This environment spawns no per-pull-request children. An environment with
        a <code className="k-mono">previews:</code> block renders a flux-operator{" "}
        <span className="k-mono">ResourceSetInputProvider</span> that polls the
        forge and a <span className="k-mono">ResourceSet</span> that stands up one
        namespace per open change request.
      </p>
      <p className="k-previews__lede">
        <strong>Two halves have to be in place.</strong> The block above says
        which change requests get a preview; a CI step running{" "}
        <code className="k-mono">kelson preview publish --pr … --sha …</code> is
        what pushes the manifests each preview applies. Without the CI step
        flux-operator finds the change requests and reports an artifact that does
        not exist.
      </p>
      <p className="k-previews__lede">
        Previews are available in <span className="k-mono">flux</span> delivery
        mode only, and this environment is{" "}
        <span className="k-mono">{mode || "unset"}</span>. Configure them on the
        Environment document — the edit screen has the fields, or the YAML tab
        takes the block whole.
      </p>
      <a
        className="k-previews__docs k-mono"
        href={PREVIEWS_DOCS}
        target="_blank"
        rel="noreferrer"
      >
        docs/model.md · Previews: a child environment per pull request
      </a>
    </div>
  );
}

const PREVIEWS_DOCS =
  "https://github.com/dafrie/kelson/blob/main/docs/model.md#previews-a-child-environment-per-pull-request";

function Configured({
  response,
  settings,
  previews,
}: {
  response: ListPreviewsResponse | undefined;
  settings: PreviewSettings;
  previews: readonly Preview[];
}) {
  const lifecycle = lifecycleLine(response?.lifecycle);
  const gated = (response?.errors.length ?? 0) > 0;

  return (
    <>
      <p className="k-previews__lede">{settingsLine(settings)}</p>
      <p className="k-mono k-previews__artifacts">
        artifacts: {settings.artifactsRepository} · tagged with each change
        request's head commit
      </p>
      {settings.skipLabels.length > 0 ? (
        <p className="k-mono k-previews__artifacts">
          updates paused by: {settings.skipLabels.join(", ")}
        </p>
      ) : null}

      {/* Behind a closed gate there is no cluster state to describe, and the
          error panel above has already said why. */}
      {gated ? null : (
        <div className="k-previews__lifecycle">
          <StatusPill status={lifecycle.status} label="lifecycle" />
          <span className="k-previews__lifecycle-text">{lifecycle.text}</span>
        </div>
      )}

      {!gated && previews.length === 0 && response?.lifecycle?.present === true ? (
        <p className="k-env__note">
          No change request has a preview right now. That is the answer when
          nothing open matches the filter — and also when CI has not published an
          artifact for any of them, in which case flux-operator has not created
          the objects this list reads.
        </p>
      ) : null}

      {previews.length > 0 ? (
        <ul className="k-previews__list">
          {previews.map((preview) => (
            <PreviewRow
              key={preview.namespace}
              preview={preview}
              settings={settings}
            />
          ))}
        </ul>
      ) : null}
    </>
  );
}

function PreviewRow({
  preview,
  settings,
}: {
  preview: Preview;
  settings: PreviewSettings;
}) {
  const url = changeRequestUrl(settings, preview.id);
  const label = `pr${preview.id}`;

  return (
    <li className="k-previews__item">
      <div className="k-previews__head">
        {url === "" ? (
          <span className="k-chip k-mono">{label}</span>
        ) : (
          <a
            className="k-chip k-mono k-previews__link"
            href={url}
            target="_blank"
            rel="noreferrer"
          >
            {label}
          </a>
        )}
        <StatusPill
          status={previewStatus(preview.phase, preview.suspended)}
          label={preview.suspended ? "suspended" : preview.phase || "unknown"}
        />
        {/* The full commit is what the tag is, so the abbreviation is a label
            over the whole value rather than a value of its own. */}
        {preview.sha ? (
          <Copyable
            value={preview.sha}
            label={shortSha(preview.sha)}
            title={preview.sha}
            className="k-previews__sha"
          />
        ) : (
          <span className="k-mono k-previews__muted">no commit pinned</span>
        )}
        <span className="k-mono k-previews__age">
          {formatAge(preview.ageSeconds)}
        </span>
      </div>

      <div className="k-previews__namespace">
        <Copyable value={preview.namespace} />
      </div>

      <p className="k-previews__headline">{previewHeadline(preview)}</p>

      {preview.message ? (
        <p className="k-mono k-previews__detail">
          {preview.reason ? `${preview.reason}: ` : ""}
          {preview.message}
        </p>
      ) : null}

      {preview.hosts.length > 0 ? (
        <div className="k-previews__hosts">
          {preview.hosts.map((host) => (
            <a
              key={host}
              className="k-mono k-previews__host"
              href={`https://${host}`}
              target="_blank"
              rel="noreferrer"
            >
              {host}
            </a>
          ))}
        </div>
      ) : (
        <p className="k-mono k-previews__muted">
          no hostnames — this preview's set declares no routes, or kelson cannot
          read routes in its namespace. Absence here is not a claim that the
          preview serves nothing.
        </p>
      )}

      {preview.revision ? (
        <p className="k-mono k-previews__muted">
          applied revision {preview.revision}
        </p>
      ) : null}
    </li>
  );
}
