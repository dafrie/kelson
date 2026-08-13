import type { StatusKind } from "../components/StatusPill";
import { mappingFields, sequenceItems } from "./miniyaml";

/**
 * What the detected storage means for a database, said in words.
 *
 * The ClusterProfile records three facts per storage class — is there a
 * snapshot driver, what does a clone cost, and how confidently was that
 * decided — and internal/clusterprofile/clusterprofile.go is explicit about
 * why all three travel: "a thin copy-on-write clone is seconds and no extra
 * space, a full-copy snapshot is minutes and a second copy of the database,
 * and the k3s default has no snapshot driver at all".
 *
 * This module turns that into the sentence a person reading their own app's
 * page needs, at the point where it matters (issue #107). Two rules shape
 * every string below:
 *
 *   - The confidence is shown, never hidden. `unknown` is not `none`
 *     (internal/clusterprofile's tri-state discipline, issue #144), and a
 *     reader who is told "we could not tell" can go and look; a reader told
 *     "unavailable" cannot.
 *   - Nothing here claims a feature exists. Backups (#94) and branching (#99)
 *     are not built; what is built is the detection that decides whether they
 *     could ever be fast, and that is what this reports.
 */

/** [CloneCapability] in internal/clusterprofile/clusterprofile.go. */
export type CloneCapability = "thin" | "full-copy" | "none" | "unknown";

/** [CloneConfidence] in the same file; "" is a profile that recorded none. */
export type CloneConfidence =
  | "observed"
  | "known-driver"
  | "unknown-driver"
  | "unreadable"
  | "";

/** One storage class as the profile records it. */
export interface StorageClassCapability {
  name: string;
  provisioner: string;
  isDefault: boolean;
  volumeSnapshotClass: string;
  snapshotDriver: string;
  capability: CloneCapability;
  confidence: CloneConfidence;
}

/** The CloudNativePG finding: the operator every postgres component needs. */
export interface OperatorFinding {
  version: string;
  namespace: string;
}

export interface StorageCapability {
  classes: StorageClassCapability[];
  /** Undefined when the profile records no CloudNativePG at all. */
  cnpg: OperatorFinding | undefined;
}

/**
 * Read the capability facts out of the profile document.
 *
 * The profile is the canonical YAML `kelson profile` prints; an empty document
 * is the zero profile, which means "nobody looked", not "nothing is there".
 */
export function parseCapability(profileYaml: string): StorageCapability {
  const classes = sequenceItems(profileYaml, "storageClasses").map((item) => ({
    name: item.get("name") ?? "",
    provisioner: item.get("provisioner") ?? "",
    isDefault: item.get("default") === "true",
    volumeSnapshotClass: item.get("volumeSnapshotClass") ?? "",
    snapshotDriver: item.get("snapshotDriver") ?? "",
    // An omitted capability reads as unknown, exactly as StorageClass.Capability
    // normalises it in Go: a hand-written profile that leaves it out must never
    // read as a promise.
    capability: asCapability(item.get("cloneCapability")),
    confidence: asConfidence(item.get("cloneConfidence")),
  }));

  const cnpg = mappingFields(profileYaml, "cnpg");
  return {
    classes,
    cnpg:
      cnpg === undefined
        ? undefined
        : { version: cnpg.get("version") ?? "", namespace: cnpg.get("namespace") ?? "" },
  };
}

function asCapability(value: string | undefined): CloneCapability {
  switch (value) {
    case "thin":
    case "full-copy":
    case "none":
      return value;
    default:
      return "unknown";
  }
}

function asConfidence(value: string | undefined): CloneConfidence {
  switch (value) {
    case "observed":
    case "known-driver":
    case "unknown-driver":
    case "unreadable":
      return value;
    default:
      return "";
  }
}

/**
 * The class a database would land on: the one the cluster marks default, else
 * the only one, else none.
 *
 * kelson deliberately leaves `spec.storage.storageClass` unset on the Cluster
 * it renders (docs/data-services.md: baking a detection snapshot into a
 * manifest that outlives it would be worse), so the default class *is* the one
 * a database gets. With several non-default classes there is no honest single
 * answer, and the panel lists them instead of picking one.
 */
export function primaryClass(
  classes: readonly StorageClassCapability[],
): StorageClassCapability | undefined {
  return classes.find((c) => c.isDefault) ?? (classes.length === 1 ? classes[0] : undefined);
}

/**
 * The headline: what this storage does or does not enable, named at the point
 * of use rather than as a capability abstraction.
 */
export function capabilityHeadline(sc: StorageClassCapability): string {
  const where = sc.name === "" ? "your storage class" : `your storage class (${sc.name})`;
  switch (sc.capability) {
    case "thin":
      return `Fast branching possible — ${where} takes copy-on-write clones.`;
    case "full-copy":
      return `Branching possible, but every branch is a full copy — ${where} snapshots into a full-size volume.`;
    case "none":
      return `Fast branching unavailable — ${where} has no snapshot driver.`;
    case "unknown":
      return `Snapshot support unknown — kelson could not tell what a snapshot on ${where} costs.`;
  }
}

/** What the capability means once the feature exists, in one sentence. */
export function capabilityDetail(sc: StorageClassCapability): string {
  switch (sc.capability) {
    case "thin":
      return (
        "A branch would be seconds and near-zero extra space, and the same snapshots are what " +
        "scheduled backups will use."
      );
    case "full-copy":
      return (
        "Snapshots work, so backups and branching are on the table, but each one costs time and " +
        "disk proportional to the size of the database."
      );
    case "none":
      return (
        "No VolumeSnapshotClass serves this provisioner, so snapshot-based backups and branching " +
        "cannot run here at all — a restore-based path is the only option, and it is not built yet."
      );
    case "unknown":
      return (
        "Unknown is not the same as unavailable: this storage may well snapshot fine. It means the " +
        "detection could not establish it, and kelson will not guess in either direction."
      );
  }
}

/** The "why" behind the verdict, which the panel shows rather than hides. */
export function confidenceWhy(sc: StorageClassCapability): string {
  const driver = sc.snapshotDriver !== "" ? sc.snapshotDriver : sc.provisioner;
  switch (sc.confidence) {
    case "observed":
      return `Observed in the cluster: no snapshot class serves ${sc.provisioner || "this provisioner"}. No lookup table was involved.`;
    case "known-driver":
      return `The CSI driver ${driver || "in use"} is in kelson's maintained table of snapshot drivers.`;
    case "unknown-driver":
      return `Snapshots exist here, but the driver ${driver || "in use"} is not in kelson's table, so the cost is reported as unknown rather than assumed.`;
    case "unreadable":
      return "The cluster's VolumeSnapshotClasses could not be listed — the profile's gaps name the permission that would settle it.";
    case "":
      return "This profile records no detection confidence for the class, so the answer stays unknown.";
  }
}

/** Whether a snapshot driver was detected, stated as the fact it is. */
export function snapshotDriverLine(sc: StorageClassCapability): string {
  if (sc.snapshotDriver === "") {
    return sc.volumeSnapshotClass === ""
      ? "snapshot driver: none detected"
      : `snapshot driver: unnamed (VolumeSnapshotClass ${sc.volumeSnapshotClass})`;
  }
  return sc.volumeSnapshotClass === ""
    ? `snapshot driver: ${sc.snapshotDriver}`
    : `snapshot driver: ${sc.snapshotDriver} (VolumeSnapshotClass ${sc.volumeSnapshotClass})`;
}

/**
 * The pill a capability wears.
 *
 * `none` is `suspended` rather than `failed`: storage without snapshots is a
 * cluster working as configured, not a broken one. `unknown` keeps the sixth
 * pill the design system added for exactly this — "we could not tell" must
 * never be painted as one of the answers that claim to know.
 */
export function capabilityStatus(capability: CloneCapability): StatusKind {
  switch (capability) {
    case "thin":
      return "synced";
    case "full-copy":
      return "degraded";
    case "none":
      return "suspended";
    case "unknown":
      return "unknown";
  }
}
