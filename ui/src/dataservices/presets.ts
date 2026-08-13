/**
 * What a data component's preset means, in numbers.
 *
 * The preset *is* the sizing (docs/data-services.md, "Tuning without leaving
 * the preset model"): a data component has three authored fields — name, kind,
 * preset — and everything else about its topology is decided by the table
 * below. So a screen that prints only the preset name prints a word the reader
 * has to go and look up, which is why this module exists.
 *
 * THE TABLE IS A MIRROR, AND IT IS THE ONLY COPY IN THE UI.
 * `DEDICATED_PRESETS` mirrors `dedicatedPresets` in
 * `internal/renderer/dataservice.go`, field for field, and the preset
 * vocabulary mirrors the `ServicePreset` constants in
 * `internal/model/project.go`. The renderer is the source of truth: the browser
 * has no way to compute these numbers and must never guess at them. If the Go
 * table changes, this one is wrong until it is changed with it — nothing here
 * derives, everything here quotes.
 *
 * Memory is one number because the renderer sets the limit equal to the
 * request (an OOM-killed primary is a failover), and there is no CPU limit at
 * all (throttling a database turns a slow query into a slow cluster).
 */

/** The presets `model.ServicePreset` accepts (internal/model/project.go). */
export type Preset = "shared" | "small" | "ha-small" | "ha-medium" | "branch";

/** The component kinds that make a component a data component (ADR-0014). */
export type DataKind = "postgres" | "valkey";

/**
 * The preset a data component resolves to when neither the component nor the
 * environment names one — `model.resolveDataService` in
 * internal/model/resolve.go defaults to `shared`, which is deferred. A postgres
 * component with no preset therefore renders nothing today, and the section
 * says so rather than showing a topology it will not get.
 */
export const DEFAULT_PRESET: Preset = "shared";

/** One rendered topology, as internal/renderer/dataservice.go writes it. */
export interface PresetSizing {
  /** `spec.instances`: 1 for the single-instance preset, 3 for the HA pair. */
  instances: number;
  /** `spec.resources.requests.cpu`. There is no CPU limit, deliberately. */
  cpu: string;
  /** `spec.resources.requests.memory`, and the limit, which equals it. */
  memory: string;
  /** `spec.storage.size`. CNPG cannot decrease this once applied. */
  storage: string;
  /** `minSyncReplicas`/`maxSyncReplicas`. 0 means asynchronous replication. */
  syncReplicas: number;
}

/** The dedicated presets kelson renders today, and nothing else. */
export const DEDICATED_PRESETS: Record<string, PresetSizing> = {
  small: { instances: 1, cpu: "500m", memory: "1Gi", storage: "5Gi", syncReplicas: 0 },
  "ha-small": { instances: 3, cpu: "500m", memory: "1Gi", storage: "5Gi", syncReplicas: 1 },
  "ha-medium": { instances: 3, cpu: "2", memory: "4Gi", storage: "20Gi", syncReplicas: 1 },
};

/**
 * One cache topology, as internal/renderer/dataservice.go writes it. Same
 * preset words, different engine: a ValkeyCluster has shards and replicas per
 * shard instead of instances, a maxmemory deliberately below the container
 * limit (eviction must start before the OOM killer does), and no storage —
 * kelson renders caches with persistence off (ADR-0015).
 */
export interface ValkeySizing {
  /** `spec.shards`: primaries the keyspace is split across. */
  shards: number;
  /** `spec.replicas`: replicas per shard, not in total. */
  replicasPerShard: number;
  /** `spec.resources.requests.cpu`. No CPU limit, same as postgres. */
  cpu: string;
  /** `spec.resources.requests.memory`, and the limit, which equals it. */
  memory: string;
  /** The valkey `maxmemory` directive — 75% of the container limit. */
  maxMemory: string;
}

/** MIRROR of `valkeyPresets` in internal/renderer/dataservice.go. */
export const VALKEY_PRESETS: Record<string, ValkeySizing> = {
  small: { shards: 1, replicasPerShard: 0, cpu: "250m", memory: "512Mi", maxMemory: "384mb" },
  "ha-small": { shards: 3, replicasPerShard: 1, cpu: "250m", memory: "512Mi", maxMemory: "384mb" },
  "ha-medium": { shards: 3, replicasPerShard: 1, cpu: "1", memory: "2Gi", maxMemory: "1536mb" },
};

/** The sizing a preset renders, or undefined when it renders nothing. */
export function sizingFor(preset: string): PresetSizing | undefined {
  return DEDICATED_PRESETS[preset];
}

/**
 * The factual lines for a data component of either kind, or undefined when the
 * preset renders nothing for that kind. The two engines share preset words but
 * not shapes, so the lines differ by kind and both quote their own Go table.
 */
export function sizingLinesFor(kind: DataKind, preset: string): string[] | undefined {
  if (kind === "valkey") {
    const s = VALKEY_PRESETS[preset];
    if (s === undefined) return undefined;
    const pods = s.shards * (1 + s.replicasPerShard);
    return [
      s.shards === 1
        ? "1 shard, no replicas — works with any Redis-compatible client"
        : `${s.shards} shards × ${s.replicasPerShard} ${s.replicasPerShard === 1 ? "replica" : "replicas"} each (${pods} pods) — needs a cluster-aware client`,
      `${s.cpu} CPU requested per pod, no limit`,
      `${s.memory} memory, request and limit`,
      `maxmemory ${s.maxMemory} with allkeys-lru eviction — evicts before the OOM killer would`,
      "no persistence — a lost cache refills, and using it as a durable store is not supported",
    ];
  }
  const s = DEDICATED_PRESETS[preset];
  return s === undefined ? undefined : sizingLines(s);
}

/**
 * The sizing as the short factual lines a panel prints. Each is one fact from
 * the table above; none of them is an opinion about it.
 */
export function sizingLines(sizing: PresetSizing): string[] {
  return [
    `${sizing.instances} ${sizing.instances === 1 ? "instance" : "instances"}`,
    `${sizing.cpu} CPU requested per instance, no limit`,
    `${sizing.memory} memory, request and limit`,
    `${sizing.storage} storage per instance`,
    sizing.syncReplicas > 0
      ? `synchronous replication — ${sizing.syncReplicas} standby must confirm every commit`
      : "asynchronous — a single instance, no standby",
  ];
}

/**
 * A preset or kind kelson accepts in a spec and does not render yet.
 *
 * The renderer answers these with a structured not-implemented error naming the
 * issue rather than rendering something else quietly (issue #141), and the UI
 * says the same thing in the same place: what is deferred, and where the work
 * is tracked.
 */
export interface Deferral {
  /** Short label for the badge: what is deferred. */
  label: string;
  /** One sentence, in the vocabulary of docs/data-services.md. */
  summary: string;
  issue: number;
  url: string;
}

export function issueUrl(issue: number): string {
  return `https://github.com/dafrie/kelson/issues/${issue}`;
}

/**
 * What, if anything, stops this component from rendering — read from the spec
 * alone, so the section can be honest before any RPC answers. The server's own
 * structured error is what the section *shows* when it has one (that is the
 * text a reader should act on); this decides whether to ask for it at all.
 */
export function deferralFor(kind: DataKind, preset: string): Deferral | undefined {
  if (kind === "valkey" && preset === "branch") {
    return {
      label: "preset: branch renders nothing for a cache",
      summary:
        "A branch copies durable state, and a kelson cache has none — it renders with " +
        "persistence off, and an empty cache is what starting one already gives you.",
      issue: 99,
      url: issueUrl(99),
    };
  }
  if (preset === "shared") {
    return {
      label: "preset: shared is deferred",
      summary:
        "The shared cluster was a cost optimization nobody asked for; the dedicated presets " +
        "(small, ha-small, ha-medium) work today, bindings included.",
      issue: 93,
      url: issueUrl(93),
    };
  }
  if (preset === "branch") {
    return {
      label: "preset: branch is not implemented",
      summary:
        "A branch needs a source cluster, a snapshot mechanism chosen from the cluster profile " +
        "and a TTL, none of which exists yet.",
      issue: 99,
      url: issueUrl(99),
    };
  }
  return undefined;
}

/**
 * The two capabilities the owner decided to show as coming soon rather than
 * build (#94, #99). They are surfaced here — beside the databases they would
 * apply to — because a feature nobody can find is not a promise, and a button
 * that does nothing is worse than a badge that says "not yet".
 */
export interface ComingSoon {
  title: string;
  summary: string;
  issue: number;
  url: string;
}

export const COMING_SOON: ComingSoon[] = [
  {
    title: "Backups",
    summary:
      "Scheduled backups will use CloudNativePG volume snapshots against a destination " +
      "configured per environment; nothing is backed up by kelson today.",
    issue: 94,
    url: issueUrl(94),
  },
  {
    title: "Branching",
    summary:
      "Branching a database depends on what your storage can snapshot, which is why the " +
      "capability below is detected and reported before the feature exists.",
    issue: 99,
    url: issueUrl(99),
  },
];
