import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { Copyable } from "../components/Copyable";
import { ErrorPanel } from "../components/ErrorPanel";
import { formatAge } from "../components/format";
import { EmptyState, LoadingState } from "../components/States";
import { Detail as KubeDetail, KubeFact } from "../expert/Detail";
import type {
  ListPreviewsResponse,
  Preview,
  PreviewSettings,
} from "../gen/kelson/v1alpha1/preview_pb";
import { LifecycleStatus, PreviewPill } from "../previews/Previews";
import {
  changeRequestUrl,
  previewHeadline,
  shortSha,
} from "../previews/phase";
import "../previews/previews.css";
import { formatWhen } from "./history";

/**
 * One preview: the page a commit status and a PR comment link to (ADR-0017
 * stage 3, #248).
 *
 * `internal/api/build.go`'s `reportPreviewStatus` has pointed its `Path` at the
 * project page since decision 12 landed, with a comment saying plainly that
 * there was nowhere better to send it: "There is no preview detail page yet
 * (ADR-0017 stage 3 built the read RPC, not the route)". ADR-0034 decision 5
 * names the same gap for the PR comment it has not built yet. This route is
 * that page.
 *
 * There is still no `GetPreview` RPC — ADR-0017 decision 12 kept `Preview` a
 * thing the cluster describes, read in a batch of one environment's previews,
 * not addressed one at a time. So this page calls the same `ListPreviews` the
 * project page's Previews section does and finds the one row whose `id`
 * matches the route's `:pr`, which is exactly what a reader typing a PR number
 * into the URL bar is asking for (ADR-0017: "the id ... is what a human types
 * when they go looking").
 *
 * Every field shown here is one `Preview` already carries — phase, hosts, the
 * two Ready conditions, the pinned commit and applied revision, when it
 * appeared. There is no branch name on the wire (`Preview` has none — a
 * preview is addressed by its PR number, not by ref) and no per-preview skip
 * label (`skip_labels` is the environment's poll-wide pause list, and
 * `suspended` is the one per-preview fact derived from it), so neither is
 * invented here.
 */
export function PreviewDetailPage() {
  const { project = "", env = "", pr = "" } = useParams();
  const clients = useClients();

  const list = useAsync(
    (signal) =>
      clients.preview.listPreviews(
        { spec: { spec: { case: "project", value: project } }, environment: env },
        { signal },
      ),
    [clients, project, env],
  );

  const data = list.data;
  const settings = data?.settings;
  const gated = (data?.errors.length ?? 0) > 0;
  const preview = data?.previews.find((p) => p.id === pr);
  const projectBase = `/projects/${encodeURIComponent(project)}`;

  return (
    <>
      <div className="k-page-head">
        <h1>Preview {pr ? `pr${pr}` : ""}</h1>
      </div>
      <div className="k-page-sub">
        <Link to={projectBase}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
        <span>·</span>
        <span>pull request {pr || "unknown"}</span>
      </div>

      {list.loading && data === undefined ? (
        <LoadingState what="this preview" />
      ) : null}

      {list.error !== undefined ? (
        <ErrorPanel
          title="Could not read this environment's previews"
          error={list.error}
        />
      ) : null}

      {data !== undefined && gated ? (
        <ErrorPanel
          title="This environment cannot run previews"
          errors={data.errors}
        />
      ) : null}

      {data !== undefined && !gated && settings === undefined ? (
        <EmptyState title="This environment declares no previews">
          Pull request {pr || "this one"} was never a candidate for one. The
          edit screen is where previews are turned on.
        </EmptyState>
      ) : null}

      {data !== undefined && !gated && settings !== undefined && preview === undefined ? (
        <NotFound pr={pr} response={data} />
      ) : null}

      {preview !== undefined ? (
        <Detail preview={preview} settings={settings} />
      ) : null}
    </>
  );
}

/**
 * The honest absence: a preview that is not in the list right now, for any of
 * the reasons that all look identical from here — closed, unlabelled, filtered
 * out, or simply not created yet. The lifecycle line is shown underneath so a
 * reader can at least tell "the poller itself is broken" from "nothing matched".
 */
function NotFound({
  pr,
  response,
}: {
  pr: string;
  response: ListPreviewsResponse;
}) {
  return (
    <div className="k-previews__empty">
      <p className="k-previews__lede">
        No preview {pr ? `pr${pr}` : "for this pull request"} is currently
        reported for {response.environment || "this environment"}. It may be
        closed, excluded by the filter, or waiting on manifests CI has not
        published — those look alike from here.
      </p>
      <LifecycleStatus lifecycle={response.lifecycle} />
    </div>
  );
}

function Detail({
  preview,
  settings,
}: {
  preview: Preview;
  settings: PreviewSettings | undefined;
}) {
  const url = changeRequestUrl(settings, preview.id);
  const created = formatWhen(preview.createdAt);

  return (
    <section className="k-section">
      <div className="k-env__head">
        <div className="k-eyebrow">pr{preview.id}</div>
        {url !== "" ? (
          <a
            className="k-button"
            href={url}
            target="_blank"
            rel="noreferrer"
          >
            View pull request
          </a>
        ) : null}
      </div>

      <div className="k-section__body k-previews__detail-body">
        <div className="k-previews__head">
          <PreviewPill preview={preview} />
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

        <p className="k-previews__headline">{previewHeadline(preview)}</p>

        {preview.message ? (
          <p className="k-mono k-previews__detail">
            {preview.reason ? `${preview.reason}: ` : ""}
            {preview.message}
          </p>
        ) : null}

        <div className="k-kv">
          <span className="k-kv__key">namespace</span>
          <span>
            <Copyable value={preview.namespace} />
            {/* The proto documents this namespace as also being the name of
                the OCIRepository and the Kustomization this preview reads
                from — an object is named only where the response actually
                named it, which here is the same string three times. */}
            <KubeDetail>
              <span className="k-kfacts">
                <KubeFact name="OCIRepository" value={preview.namespace} />
                <KubeFact name="Kustomization" value={preview.namespace} />
              </span>
            </KubeDetail>
          </span>

          <span className="k-kv__key">artifact ready</span>
          <span>{preview.artifactReady || "unknown"}</span>

          <span className="k-kv__key">applied ready</span>
          <span>{preview.appliedReady || "unknown"}</span>

          {preview.revision ? (
            <>
              <span className="k-kv__key">applied revision</span>
              <span>
                <Copyable value={preview.revision} />
              </span>
            </>
          ) : null}

          {created !== "" ? (
            <>
              <span className="k-kv__key">created</span>
              <span>{created}</span>
            </>
          ) : null}
        </div>

        <div className="k-previews__hostsblock">
          <div className="k-eyebrow">Hosts</div>
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
              no hostnames — this preview declares no routes, or kelson cannot
              read them in its namespace
            </p>
          )}
        </div>

        {settings !== undefined && settings.skipLabels.length > 0 ? (
          <p className="k-mono k-previews__artifacts">
            updates paused by: {settings.skipLabels.join(", ")} — set for the
            whole environment, not for this preview
          </p>
        ) : null}
      </div>
    </section>
  );
}
