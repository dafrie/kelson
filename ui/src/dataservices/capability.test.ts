import { describe, expect, it } from "vitest";

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
  type CloneCapability,
  type CloneConfidence,
  type StorageClassCapability,
} from "./capability";

/**
 * The fixture is real `clusterprofile.Marshal` output — yaml.v3's four-space
 * indentation and key order, captured from internal/clusterprofile — because
 * the profile reaches the browser as that document and nothing else
 * (proto/kelson/v1alpha1/profile.proto: the YAML tags are the schema of
 * record). A hand-tidied fixture would test the reader against itself.
 */
const PROFILE = `kubernetes:
    version: v1.31.2
    platform: k3s
storageClasses:
    - name: local-path
      provisioner: rancher.io/local-path
      default: true
      cloneCapability: none
      cloneConfidence: observed
    - name: ceph-rbd
      provisioner: rbd.csi.ceph.com
      volumeSnapshotClass: csi-rbdplugin-snapclass
      snapshotDriver: rbd.csi.ceph.com
      cloneCapability: thin
      cloneConfidence: known-driver
cnpg:
    version: 1.30.0
    namespace: cnpg-system
    crds:
        - clusters
        - databases
flux:
    version: 2.6.4
    namespace: flux-system
helmController:
    version: 1.3.0
    namespace: flux-system
    crds:
        - helmreleases
incomplete:
    - field: storageClasses
      reason: no permission to list volumesnapshotclasses
`;

function classOf(
  capability: CloneCapability,
  confidence: CloneConfidence,
  over: Partial<StorageClassCapability> = {},
): StorageClassCapability {
  return {
    name: "local-path",
    provisioner: "rancher.io/local-path",
    isDefault: true,
    volumeSnapshotClass: "",
    snapshotDriver: "",
    capability,
    confidence,
    ...over,
  };
}

describe("parseCapability", () => {
  it("reads the storage classes and the operator finding", () => {
    const capability = parseCapability(PROFILE);

    expect(capability.classes).toHaveLength(2);
    expect(capability.classes[0]).toEqual({
      name: "local-path",
      provisioner: "rancher.io/local-path",
      isDefault: true,
      volumeSnapshotClass: "",
      snapshotDriver: "",
      capability: "none",
      confidence: "observed",
    });
    expect(capability.classes[1]?.capability).toBe("thin");
    expect(capability.classes[1]?.snapshotDriver).toBe("rbd.csi.ceph.com");
    // The nested `crds:` sequence must not leak into the operator's fields.
    expect(capability.cnpg).toEqual({ version: "1.30.0", namespace: "cnpg-system" });
    // helm-controller is its own finding, read the same way and separate from
    // the `flux:` block above it (internal/clusterprofile: a Flux install need
    // not include it).
    expect(capability.helmController).toEqual({
      version: "1.3.0",
      namespace: "flux-system",
    });
  });

  it("treats an omitted capability as unknown, never as a promise", () => {
    const capability = parseCapability(
      "storageClasses:\n    - name: mystery\n      provisioner: csi.example.com\n",
    );

    expect(capability.classes[0]?.capability).toBe("unknown");
    expect(capability.classes[0]?.confidence).toBe("");
  });

  it("reports the zero profile as nothing detected", () => {
    const capability = parseCapability("");

    expect(capability.classes).toEqual([]);
    expect(capability.cnpg).toBeUndefined();
    expect(capability.helmController).toBeUndefined();
  });

  it("picks the class a database would land on: the cluster default", () => {
    expect(primaryClass(parseCapability(PROFILE).classes)?.name).toBe("local-path");
    // Several classes and no default: there is no honest single answer.
    expect(
      primaryClass([
        classOf("thin", "known-driver", { name: "a", isDefault: false }),
        classOf("none", "observed", { name: "b", isDefault: false }),
      ]),
    ).toBeUndefined();
    // Exactly one class is the answer whether or not it is flagged default.
    expect(
      primaryClass([classOf("thin", "known-driver", { name: "only", isDefault: false })])
        ?.name,
    ).toBe("only");
  });
});

describe("capability phrasing", () => {
  it("says branching is unavailable when there is no snapshot driver", () => {
    const sc = classOf("none", "observed");

    expect(capabilityHeadline(sc)).toBe(
      "Fast branching unavailable — your storage class (local-path) has no snapshot driver.",
    );
    expect(capabilityDetail(sc)).toContain("No VolumeSnapshotClass serves this provisioner");
    expect(snapshotDriverLine(sc)).toBe("snapshot driver: none detected");
    expect(capabilityStatus("none")).toBe("suspended");
  });

  it("says what thin cloning enables", () => {
    const sc = classOf("thin", "known-driver", {
      name: "ceph-rbd",
      provisioner: "rbd.csi.ceph.com",
      snapshotDriver: "rbd.csi.ceph.com",
      volumeSnapshotClass: "csi-rbdplugin-snapclass",
    });

    expect(capabilityHeadline(sc)).toBe(
      "Fast branching possible — your storage class (ceph-rbd) takes copy-on-write clones.",
    );
    expect(capabilityDetail(sc)).toContain("seconds and near-zero extra space");
    expect(snapshotDriverLine(sc)).toBe(
      "snapshot driver: rbd.csi.ceph.com (VolumeSnapshotClass csi-rbdplugin-snapclass)",
    );
    expect(capabilityStatus("thin")).toBe("synced");
  });

  it("names the cost of a full copy instead of calling it branching", () => {
    const sc = classOf("full-copy", "known-driver", {
      name: "gp3",
      provisioner: "ebs.csi.aws.com",
      snapshotDriver: "ebs.csi.aws.com",
    });

    expect(capabilityHeadline(sc)).toBe(
      "Branching possible, but every branch is a full copy — your storage class (gp3) snapshots into a full-size volume.",
    );
    expect(capabilityDetail(sc)).toContain("proportional to the size of the database");
    expect(capabilityStatus("full-copy")).toBe("degraded");
  });

  it("keeps unknown distinct from unavailable", () => {
    const sc = classOf("unknown", "unknown-driver", {
      name: "mystery",
      provisioner: "csi.example.com",
      snapshotDriver: "csi.example.com",
      volumeSnapshotClass: "example-snap",
    });

    expect(capabilityHeadline(sc)).toBe(
      "Snapshot support unknown — kelson could not tell what a snapshot on your storage class (mystery) costs.",
    );
    expect(capabilityDetail(sc)).toContain("Unknown is not the same as unavailable");
    expect(capabilityStatus("unknown")).toBe("unknown");
  });
});

describe("operator findings", () => {
  it("states CloudNativePG as detected or not, and never as a promise", () => {
    expect(cnpgLine(parseCapability(PROFILE).cnpg)).toBe(
      "CloudNativePG: detected (1.30.0, in cnpg-system).",
    );
    expect(cnpgLine(undefined)).toContain("CloudNativePG: not detected.");
    expect(cnpgLine(undefined)).toContain("kelson renders the Cluster manifest either way");
  });

  it("says the same thing about helm-controller, in the same shape (#107)", () => {
    expect(helmControllerLine(parseCapability(PROFILE).helmController)).toBe(
      "helm-controller: detected (1.3.0, in flux-system). A `kind: helm` component has something to reconcile it.",
    );

    // Absent is a finding, and the sentence says what that costs: the manifest
    // still renders, and nothing installs the chart.
    const absent = helmControllerLine(undefined);
    expect(absent).toContain("helm-controller: not detected.");
    expect(absent).toContain("renders a HelmRelease either way");
    expect(absent).toContain("installs nothing");
  });

  it("names an installed operator whose version could not be read", () => {
    // Empty Version is "installed, version unknown" in internal/clusterprofile
    // and must not read as too old — or as a missing operator.
    expect(
      helmControllerLine(
        parseCapability("helmController:\n    namespace: flux-system\n").helmController,
      ),
    ).toContain("detected (version unknown, in flux-system)");
    expect(cnpgLine({ version: "", namespace: "" })).toBe(
      "CloudNativePG: detected (version unknown).",
    );
  });
});

describe("confidence", () => {
  it("shows the why for every way the answer was reached", () => {
    expect(confidenceWhy(classOf("none", "observed"))).toContain(
      "no snapshot class serves rancher.io/local-path",
    );
    expect(
      confidenceWhy(
        classOf("thin", "known-driver", { snapshotDriver: "rbd.csi.ceph.com" }),
      ),
    ).toContain("rbd.csi.ceph.com is in kelson's maintained table");
    expect(
      confidenceWhy(
        classOf("unknown", "unknown-driver", { snapshotDriver: "csi.example.com" }),
      ),
    ).toContain("not in kelson's table");
    expect(confidenceWhy(classOf("unknown", "unreadable"))).toContain(
      "could not be listed",
    );
    // A hand-written profile that records no confidence is not a confident one.
    expect(confidenceWhy(classOf("unknown", ""))).toContain("no detection confidence");
  });
});
