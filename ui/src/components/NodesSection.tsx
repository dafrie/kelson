import { ConnectError } from "@connectrpc/connect";

import { useAsync, useClients } from "../api/data";
import { EmptyState, ErrorState, LoadingState } from "./States";
import { StatusPill } from "./StatusPill";

/**
 * The node inventory: how many machines, how big, how loaded.
 *
 * Usage renders only when the server read it. A node without a sample shows a
 * dash and the section says why (metrics-server absent, usually) — the
 * tri-state again: an unread gauge drawn at zero would be a claim nobody made.
 */

export function NodesSection() {
  const clients = useClients();
  const nodes = useAsync((signal) => clients.node.getNodes({}, { signal }), [clients]);

  if (nodes.loading && !nodes.data) {
    return <LoadingState what="nodes" />;
  }
  if (nodes.error && !nodes.data) {
    return <ErrorState title="Node inventory failed" detail={describe(nodes.error)} />;
  }
  const list = nodes.data?.nodes ?? [];
  if (list.length === 0) {
    return <EmptyState title="No nodes — the cluster reported an empty node list" />;
  }
  const ready = list.filter((n) => n.ready).length;

  return (
    <div className="k-nodes">
      <p className="k-nodes__summary">
        {list.length} node{list.length === 1 ? "" : "s"}, {ready} ready
      </p>
      {nodes.data?.usageGap ? <p className="k-note">{nodes.data.usageGap}</p> : null}
      <ul className="k-nodes__list">
        {list.map((n) => (
          <li key={n.name} className="k-panel k-nodes__row">
            <div className="k-nodes__head">
              <span className="k-nodes__name">{n.name}</span>
              <span className="k-nodes__meta">
                {n.roles.length > 0 ? n.roles.join(", ") + " · " : ""}
                {n.architecture} · {n.kubeletVersion}
              </span>
              <StatusPill
                status={n.ready ? "synced" : "failed"}
                label={n.ready ? "ready" : "not ready"}
              />
            </div>
            <Gauge
              label="CPU"
              used={n.cpuUsageMilli}
              total={n.cpuAllocatableMilli}
              format={cores}
            />
            <Gauge
              label="Memory"
              used={n.memoryUsageBytes}
              total={n.memoryAllocatableBytes}
              format={bytes}
            />
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * One capacity gauge. `used` is bigint-or-absent: absent draws no bar and
 * says "not read" rather than 0%.
 */
function Gauge({
  label,
  used,
  total,
  format,
}: {
  label: string;
  used: bigint | undefined;
  total: bigint;
  format: (v: bigint) => string;
}) {
  const totalN = Number(total);
  const usedN = used === undefined ? undefined : Number(used);
  const percent =
    usedN === undefined || totalN <= 0
      ? undefined
      : Math.min(100, Math.round((usedN / totalN) * 100));
  return (
    <div className="k-gauge">
      <span className="k-gauge__label">{label}</span>
      <div
        className="k-gauge__track"
        role="meter"
        aria-label={`${label} usage`}
        aria-valuenow={percent}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        {percent !== undefined ? (
          <div
            className={`k-gauge__fill${percent >= 90 ? " k-gauge__fill--hot" : ""}`}
            style={{ width: `${percent}%` }}
          />
        ) : null}
      </div>
      <span className="k-gauge__value">
        {used !== undefined
          ? `${format(used)} / ${format(total)} (${percent}%)`
          : `— / ${format(total)} allocatable`}
      </span>
    </div>
  );
}

function cores(milli: bigint): string {
  const v = Number(milli) / 1000;
  return `${v >= 10 ? v.toFixed(0) : v.toFixed(1)} cores`;
}

function bytes(b: bigint): string {
  const gib = Number(b) / (1024 * 1024 * 1024);
  return `${gib >= 10 ? gib.toFixed(0) : gib.toFixed(1)} GiB`;
}

function describe(err: unknown): string {
  if (err instanceof ConnectError) {
    return `${err.code}: ${err.rawMessage}`;
  }
  return err instanceof Error ? err.message : String(err);
}
