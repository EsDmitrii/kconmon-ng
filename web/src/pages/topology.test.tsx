import { describe, expect, it } from "vitest";
import { buildFlow } from "./topology";
import type { Matrix, Topology } from "@/lib/types";

const topo: Topology = {
  nodes: [
    { name: "n1", zone: "z1", ready: true },
    { name: "n2", zone: "z2", ready: false },
  ],
  agents: [],
  timestamp: "t",
};
const matrix: Matrix = {
  protocol: "tcp", plane: "pod", nodes: ["n1", "n2"],
  cells: [{ source: "n1", destination: "n2", failRatio: 0.2 }],
  timestamp: "t",
};

describe("buildFlow", () => {
  it("creates one zone container per zone plus one node per cluster node", () => {
    const { nodes } = buildFlow(topo, matrix);
    const zones = nodes.filter((n) => n.type === "zone");
    expect(zones).toHaveLength(2);
    expect(zones[0].data).toMatchObject({ label: "z1", count: 1 });
    expect(nodes.filter((n) => n.parentId)).toHaveLength(2);
    expect(nodes.filter((n) => n.parentId).every((n) => n.type === "topoNode")).toBe(true);
  });

  it("colors not-ready and high-fail nodes as failing, and marks readiness in data", () => {
    const { nodes } = buildFlow(topo, matrix);
    expect(nodes.find((n) => n.id === "n1")?.className).toContain("failing"); // fail 0.2 >= 0.1
    expect(nodes.find((n) => n.id === "n2")?.className).toContain("failing"); // not ready
    expect(nodes.find((n) => n.id === "n2")?.data).toMatchObject({ ready: false });
  });

  it("draws edges only for problem pairs, without permanent labels", () => {
    const { edges, problemTotal } = buildFlow(topo, matrix);
    expect(edges).toHaveLength(1);
    expect(edges[0].id).toBe("n1->n2");
    expect(edges[0].label).toBeUndefined(); // percentage appears on hover only
    expect(edges[0].data).toMatchObject({ failLabel: "20%" });
    expect(problemTotal).toBe(1);
  });

  it("caps at the WORST paths, not the first encountered, and reports the total", () => {
    const names = Array.from({ length: 16 }, (_, i) => `m${i}`);
    const bigTopo: Topology = {
      nodes: names.map((name) => ({ name, zone: "z", ready: true })),
      agents: [],
      timestamp: "t",
    };
    // 15 problem cells with ascending failRatio 1%..15% — the worst one is LAST
    // in the input, so a naive first-N slice would drop it.
    const cells = Array.from({ length: 15 }, (_, i) => ({
      source: names[i],
      destination: names[i + 1],
      failRatio: (i + 1) / 100,
    }));
    const bigMatrix: Matrix = { protocol: "tcp", plane: "pod", nodes: names, cells, timestamp: "t" };

    const { edges, problemTotal } = buildFlow(bigTopo, bigMatrix);
    expect(problemTotal).toBe(15);
    expect(edges).toHaveLength(10);
    // Worst first, and the strongest failure (15%) survived the cap.
    expect(edges[0].data).toMatchObject({ failLabel: "15%" });
    const kept = edges.map((e) => (e.data as { failLabel: string }).failLabel);
    expect(kept).not.toContain("5%"); // the 5 weakest (1–5%) were dropped
    expect(kept).toContain("6%");
  });
});
