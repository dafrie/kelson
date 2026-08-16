import { Link } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import { formatAge } from "../components/format";
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
} from "./phase";
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
 *   - **Swallow a structured refusal.** Whatever ListPreviews reports about
 *     this environment arrives with its code, message and remediation intact,
 *     in the same panel every other structured refusal reaches the reader
 *     through.
 *   - **Read an empty list as "nothing is wrong".** flux-operator missing, the
 *     lifecycle pair never delivered and a poller that cannot reach the forge
 *     all produce no previews, and each gets its own sentence (previews.ts).
 *   - **Show a hostname the spec asks for.** Every hostname here carries the
 *     change request in its first DNS label, because a preview never serves the
 *     parent's hostname (ADR-0017 decision 11). They are read from the
 *     preview's own HTTPRoutes rather than derived, so what is shown is what is
 *     served.
 *
 * Each row's "details →" is the one internal navigation on the panel — to
 * `PreviewDetailPage`, the route a commit status and a PR comment link to
 * (ADR-0017 stage 3, #248) — and it is deliberately a second link rather than a
 * relabelling of the chip above it: that chip leaves kelson for the forge, and
 * conflating the two would make one of them lie about where it goes.
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

      {/* Any structured finding, verbatim. */}
      {data !== undefined && data.errors.length > 0 ? (
        <ErrorPanel
          title="This environment cannot run previews"
          errors={data.errors}
        />
      ) : null}

      {data !== undefined && settings === undefined && data.errors.length === 0 ? (
        <NotConfigured />
      ) : null}

      {settings !== undefined ? (
        <Configured
          response={data}
          settings={settings}
          previews={previews}
          project={project}
          environment={environment}
        />
      ) : null}
    </div>
  );
}

/**
 * The empty state, which is where most readers meet this feature: what previews
 * are, the two halves that have to be in place, and where the rest is written.
 */
function NotConfigured() {
  return (
    <div className="k-previews__empty">
      <p className="k-previews__lede">
        This environment spawns no per-pull-request children. Turning previews
        on gives every open pull request its own copy of it.
      </p>
      <p className="k-previews__lede">
        <strong>Two halves have to be in place.</strong> A{" "}
        <code className="k-mono">previews:</code> block on the Environment says
        which pull requests qualify — the edit screen has the fields — and a CI
        step running{" "}
        <code className="k-mono">kelson preview publish --pr … --sha …</code>{" "}
        pushes the manifests each preview applies.
      </p>
      <a
        className="k-previews__docs"
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
  project,
  environment,
}: {
  response: ListPreviewsResponse | undefined;
  settings: PreviewSettings;
  previews: readonly Preview[];
  project: string;
  environment: string;
}) {
  const lifecycle = lifecycleLine(response?.lifecycle);
  const gated = (response?.errors.length ?? 0) > 0;

  return (
    <>
      <p className="k-previews__lede">{settingsLine(settings)}</p>
      <p className="k-mono k-previews__artifacts">
        artifacts: {settings.artifactsRepository}
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
          No change request has a preview right now — nothing open matches the
          filter, or CI has published for none of them.
        </p>
      ) : null}

      {previews.length > 0 ? (
        <ul className="k-previews__list">
          {previews.map((preview) => (
            <PreviewRow
              key={preview.namespace}
              preview={preview}
              settings={settings}
              project={project}
              environment={environment}
            />
          ))}
        </ul>
      ) : null}
    </>
  );
}

/**
 * A preview's word, with flux-operator's own phase kept beside it as a labelled
 * fact. The word answers "is this change request up"; the phase is what to
 * quote at the operator when it is not.
 */
export function PreviewPill({ preview }: { preview: Preview }) {
  const state = previewStatus(preview.phase, preview.suspended);
  return (
    <>
      <StatusPill status={state.tone} label={state.word} />
      {preview.phase !== "" ? (
        <span className="k-previews__muted">
          phase <span className="k-mono">{preview.phase}</span>
        </span>
      ) : null}
    </>
  );
}

function PreviewRow({
  preview,
  settings,
  project,
  environment,
}: {
  preview: Preview;
  settings: PreviewSettings;
  project: string;
  environment: string;
}) {
  const url = changeRequestUrl(settings, preview.id);
  const label = `pr${preview.id}`;
  const detailHref = `/projects/${encodeURIComponent(project)}/${encodeURIComponent(
    environment,
  )}/previews/${encodeURIComponent(preview.id)}`;

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
        {/* This row's own page (ADR-0017 stage 3, #248) — the identifier a
            commit status and a PR comment link to, distinct from the forge
            link above, which leaves kelson entirely. */}
        <Link
          className="k-mono k-previews__link"
          to={detailHref}
          title={`${label}'s own page`}
        >
          details →
        </Link>
        <PreviewPill preview={preview} />
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
        <p className="k-previews__detail">
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
        <p className="k-previews__muted">
          no hostnames — this preview declares no routes, or kelson cannot read
          them in its namespace
        </p>
      )}

      {preview.revision ? (
        <p className="k-previews__muted">
          applied revision <span className="k-mono">{preview.revision}</span>
        </p>
      ) : null}
    </li>
  );
}
