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
 * This module turns that into the sentence a person reading their own project's
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

/** An operator finding: the version and where it runs, as the profile records it. */
export interface OperatorFinding {
  version: string;
  namespace: string;
}

export interface StorageCapability {
  classes: StorageClassCapability[];
  /** Undefined when the profile records no CloudNativePG at all. */
  cnpg: OperatorFinding | undefined;
  /**
   * Flux's helm-controller (ADR-0016), undefined when the profile records none.
   *
   * A separate finding from Flux itself, because a Flux installation need not
   * include it — flux-operator's FluxInstance takes a components subset — so a
   * cluster can be running Flux and still have nothing to reconcile a
   * HelmRelease (internal/clusterprofile/clusterprofile.go: HelmController).
   */
  helmController: OperatorFinding | undefined;
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

  return {
    classes,
    cnpg: operatorFinding(profileYaml, "cnpg"),
    helmController: operatorFinding(profileYaml, "helmController"),
  };
}

/**
 * One operator's finding, or undefined when the profile records none.
 *
 * The absent case is a nil pointer on the Go side, which marshals to nothing at
 * all — so a missing key means "not detected", never "detected with no
 * version". The `crds:` list under either key is a nested block (or a flow
 * sequence) and is not read here: what it answers is whether the API server
 * serves the kind, which is the judgement packages' question
 * (internal/clusterprofile/helm), not this panel's.
 */
function operatorFinding(
  profileYaml: string,
  key: string,
): OperatorFinding | undefined {
  const fields = mappingFields(profileYaml, key);
  if (fields === undefined) return undefined;
  return {
    version: fields.get("version") ?? "",
    namespace: fields.get("namespace") ?? "",
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
        "This storage cannot snapshot, so snapshot-based backups and branching " +
        "cannot run here."
      );
    case "unknown":
      return (
        "Unknown is not the same as unavailable: detection could not establish " +
        "it, and kelson will not guess either way."
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
 * CloudNativePG is the prerequisite for every postgres component (ADR-0005),
 * and its absence is a finding worth stating here: kelson renders the Cluster,
 * the operator is what turns it into a database.
 */
export function cnpgLine(cnpg: OperatorFinding | undefined): string {
  if (cnpg === undefined) {
    return (
      "CloudNativePG: not detected. Without it a postgres component deploys " +
      "and never becomes a database."
    );
  }
  return `CloudNativePG: detected (${where(cnpg)}).`;
}

/**
 * helm-controller is the same kind of statement for a `kind: helm` component
 * (ADR-0016, issue #107): kelson renders a HelmRelease and delegates the chart,
 * so what matters is whether the controller that reconciles it is there.
 *
 * Phrased like the CloudNativePG line above and for the same reason — both are
 * "kelson renders the manifest either way, and this is what makes it do
 * something" — and, like it, this reports detection rather than a verdict.
 * Whether a *particular* chart component will work is
 * internal/clusterprofile/helm's judgement, which reads the served CRDs and the
 * source-controller finding too.
 */
export function helmControllerLine(helm: OperatorFinding | undefined): string {
  if (helm === undefined) {
    return (
      "helm-controller: not detected. Without it a `kind: helm` component " +
      "deploys and installs nothing."
    );
  }
  return `helm-controller: detected (${where(helm)}). A \`kind: helm\` component has something to reconcile it.`;
}

/**
 * An operator's version and namespace, in the parenthetical both lines use. An
 * empty version is "installed, version unknown" — a real state the profile
 * records (internal/clusterprofile: "must not be read as too old") — and it is
 * named rather than left out, because a blank parenthesis reads as a bug.
 */
function where(finding: OperatorFinding): string {
  const version = finding.version === "" ? "version unknown" : finding.version;
  return finding.namespace === "" ? version : `${version}, in ${finding.namespace}`;
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
