import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import {
  GetNodesResponseSchema,
  NodeService,
} from "../gen/kelson/v1alpha1/nodes_pb";
import { renderAt } from "../test/render";
import { NodesSection } from "./NodesSection";

/**
 * The node inventory against a stubbed NodeService. The case that matters is
 * the tri-state one: a node without a usage sample draws no bar and the gap
 * text says why, rather than the gauge lying at 0%.
 */

function nodesTransport(usageGap: string) {
  return createRouterTransport(({ service }) => {
    service(NodeService, {
      getNodes() {
        return create(GetNodesResponseSchema, {
          usageGap,
          nodes: [
            {
              name: "control-a",
              roles: ["control-plane"],
              kubeletVersion: "v1.31.2",
              architecture: "arm64",
              ready: true,
              cpuCapacityMilli: 4000n,
              cpuAllocatableMilli: 3800n,
              memoryCapacityBytes: 8n * 1024n * 1024n * 1024n,
              memoryAllocatableBytes: 7n * 1024n * 1024n * 1024n,
              cpuUsageMilli: 1900n,
              memoryUsageBytes: 3n * 1024n * 1024n * 1024n,
            },
            {
              name: "worker-b",
              ready: false,
              cpuCapacityMilli: 4000n,
              cpuAllocatableMilli: 3800n,
              memoryCapacityBytes: 8n * 1024n * 1024n * 1024n,
              memoryAllocatableBytes: 7n * 1024n * 1024n * 1024n,
            },
          ],
        });
      },
    });
  });
}

describe("NodesSection", () => {
  it("renders count, readiness, capacity and usage — and honesty about gaps", async () => {
    renderAt(
      nodesTransport("metrics.k8s.io has no sample yet for: worker-b"),
      "/cluster",
      "/cluster",
      <NodesSection />,
    );
    await screen.findByText("control-a");

    expect(screen.getByText("2 nodes, 1 ready")).toBeTruthy();
    expect(screen.getByText("ready")).toBeTruthy();
    expect(screen.getByText("not ready")).toBeTruthy();
    expect(screen.getByText(/control-plane · arm64 · v1.31.2/)).toBeTruthy();

    // The sampled node shows used/allocatable with a percentage…
    expect(screen.getByText("1.9 cores / 3.8 cores (50%)")).toBeTruthy();
    expect(screen.getByText("3.0 GiB / 7.0 GiB (43%)")).toBeTruthy();

    // …the unsampled one shows allocatable only, and the section says why.
    expect(screen.getAllByText("— / 3.8 cores allocatable")).toHaveLength(1);
    expect(screen.getByText(/no sample yet for: worker-b/)).toBeTruthy();
  });
});
