import { useMemo } from "react";
import { Link, useParams } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import { ErrorPanel } from "../components/ErrorPanel";
import { LoadingState } from "../components/States";
import { DiffView } from "../diff/DiffView";
import { decodeDiff, type Diff } from "../diff/parse";

/**
 * What would change, at the only fidelity this screen can offer.
 *
 * RenderService.Diff has two rungs. RENDER compares the spec against the
 * request's `from` documents — the CLI's `--from` — and a stored spec has no
 * `from` to supply: the store keeps the current documents, not the previous
 * ones. Asking for RENDER here would report every resource as an addition,
 * which is technically true and practically a lie about what a deploy would do.
 *
 * So this screen runs SERVER: the live cluster's own verdict, the same preview
 * `kelson diff` produces at L2. It never falls back to a rendered diff when the
 * cluster is unreachable — internal/api/render.go is explicit that a silently
 * downgraded preview gives a CI gate a clean answer it did not earn — so an
 * unreachable cluster shows up here as the error it is.
 */
export function DiffPage() {
  const { project = "", env = "" } = useParams();
  const clients = useClients();

  const result = useAsync(
    (signal) =>
      clients.render.diff(
        {
          spec: { spec: { case: "project", value: project } },
          environment: env,
          dryRun: DryRun.SERVER,
        },
        { signal },
      ),
    [clients, project, env],
  );

  const decoded = useMemo((): { diff?: Diff; error?: string } => {
    const bytes = result.data?.diffJson;
    if (bytes === undefined || bytes.length === 0) return {};
    try {
      return { diff: decodeDiff(bytes) };
    } catch (err) {
      return { error: err instanceof Error ? err.message : String(err) };
    }
  }, [result.data]);

  return (
    <>
      <div className="k-page-head">
        <h1>Diff</h1>
      </div>
      <div className="k-page-sub">
        <Link to={`/apps/${encodeURIComponent(project)}`}>← {project}</Link>
        <span>·</span>
        <span className="k-chip k-mono">{env}</span>
        <span>·</span>
        <span>server dry-run, against the live cluster</span>
      </div>

      <p className="k-note">
        A rendered-vs-rendered diff needs a previous spec to compare against
        (`from` on DiffRequest). The store holds the current documents only, so
        that comparison is a CLI flow for now — <code>kelson diff --from</code>.
        What this screen shows is the cluster's own dry-run verdict.
      </p>

      {result.loading && result.data === undefined ? (
        <LoadingState what="the server dry-run preview" />
      ) : null}

      {result.error !== undefined ? (
        <ErrorPanel title="The preview failed" error={result.error} />
      ) : null}

      {result.data?.errors.length ? (
        <ErrorPanel title="The spec was rejected" errors={result.data.errors} />
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
          exitSemantics={result.data?.exitSemantics}
        />
      ) : null}
    </>
  );
}
