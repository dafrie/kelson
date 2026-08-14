import { useMemo } from "react";

import { useAsync, useClients } from "../api/data";
import { ErrorPanel } from "../components/ErrorPanel";
import { StatusPill } from "../components/StatusPill";
import type { ProfileGap } from "../gen/kelson/v1alpha1/profile_pb";
import {
  capabilityDetail,
  capabilityHeadline,
  capabilityStatus,
  cnpgLine,
  confidenceWhy,
  helmControllerLine,
  parseCapability,
  primaryClass,
  snapshotDriverLine,
  type StorageClassCapability,
} from "./capability";
import { issueUrl } from "./presets";

/**
 * What the detected storage means for a database, next to the databases.
 *
 * Issue #107 asks for the capability to be stated at the point of use rather
 * than as a row in a capability matrix somewhere else — "Fast branching
 * unavailable — your storage class (local-path) has no snapshot driver" is the
 * whole idea. /cluster keeps the full profile document; this panel answers the
 * one question a data component raises.
 *
 * It lives on the app detail page because that is where a spec's postgres
 * components are. The create flow does not author a data component yet
 * (src/spec/documents.ts writes workload fields only), so there is no preset
 * being chosen there to attach this to; when there is, this component is what
 * goes beside that control, unchanged.
 *
 * Never rendered as a promise. Backups (#94) and branching (#99) do not exist;
 * what exists is the detection that decides whether they could be cheap, and
 * saying so early is what stops someone from designing around a feature their
 * cluster could never serve well.
 */
export function CapabilityPanel() {
  const clients = useClients();
  const profile = useAsync(
    (signal) => clients.profile.getProfile({}, { signal }),
    [clients],
  );

  const yaml = profile.data?.yaml;
  const capability = useMemo(
    () => parseCapability(yaml === undefined ? "" : new TextDecoder().decode(yaml)),
    [yaml],
  );
  const gaps = (profile.data?.gaps ?? []).filter(
    (gap) =>
      gap.field.startsWith("storageClasses") ||
      gap.field.startsWith("cnpg") ||
      gap.field.startsWith("helmController"),
  );
  const primary = primaryClass(capability.classes);

  return (
    <div className="k-cap">
      <div className="k-eyebrow">Storage capability</div>

      {profile.loading && profile.data === undefined ? (
        <p className="k-env__note">Reading the cluster profile…</p>
      ) : null}

      {profile.error !== undefined ? (
        <ErrorPanel
          title="Could not read the cluster profile"
          error={profile.error}
        />
      ) : null}

      {profile.data !== undefined ? (
        <>
          <p className="k-cap__operator">{cnpgLine(capability.cnpg)}</p>
          {/* The other operator a rendered manifest is delegated to (#107).
              Same shape of statement as the line above: what kelson emits, and
              what has to be running for it to become anything. */}
          <p className="k-cap__operator">
            {helmControllerLine(capability.helmController)}
          </p>

          {capability.classes.length === 0 ? (
            <p className="k-env__note">
              Detection reported no storage classes, so what a snapshot would cost
              here is unknown rather than unavailable. Nothing is claimed either
              way.
            </p>
          ) : primary !== undefined ? (
            <ClassCapability sc={primary} />
          ) : (
            <>
              <p className="k-env__note">
                No storage class is marked default, so kelson cannot say which one
                a database would land on. Each detected class:
              </p>
              {capability.classes.map((sc) => (
                <ClassCapability key={sc.name} sc={sc} />
              ))}
            </>
          )}

          {gaps.length > 0 ? (
            <ul className="k-gaps k-cap__gaps">
              {gaps.map((gap) => (
                <GapRow key={`${gap.field}:${gap.reason}`} gap={gap} />
              ))}
            </ul>
          ) : null}

          <p className="k-cap__foot">
            Storage capability is detected, not configured: it is what decides
            whether{" "}
            <a href={issueUrl(94)} target="_blank" rel="noreferrer">
              backups (#94)
            </a>{" "}
            and{" "}
            <a href={issueUrl(99)} target="_blank" rel="noreferrer">
              branching (#99)
            </a>{" "}
            can be cheap once they exist.
          </p>
        </>
      ) : null}
    </div>
  );
}

function ClassCapability({ sc }: { sc: StorageClassCapability }) {
  return (
    <div className="k-cap__class">
      <div className="k-cap__head">
        <StatusPill status={capabilityStatus(sc.capability)} label={sc.capability} />
        <span className="k-mono k-cap__name">
          {sc.name}
          {sc.isDefault ? " · cluster default" : ""}
        </span>
      </div>
      <p className="k-cap__headline">{capabilityHeadline(sc)}</p>
      <p className="k-cap__detail">{capabilityDetail(sc)}</p>
      <p className="k-mono k-cap__driver">{snapshotDriverLine(sc)}</p>
      {/* The confidence is the "why", and it is shown rather than folded into
          the verdict: a reader who knows the answer came from a driver table
          can check the table, and one told only "unknown" cannot. */}
      <p className="k-cap__why">
        <span className="k-cap__why-label">why:</span> {confidenceWhy(sc)}
      </p>
    </div>
  );
}

function GapRow({ gap }: { gap: ProfileGap }) {
  return (
    <li>
      <div className="k-gaps__field">{gap.field}</div>
      <div className="k-gaps__reason">{gap.reason}</div>
    </li>
  );
}

