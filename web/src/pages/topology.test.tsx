import { describe, expect, it } from "vitest";
import { buildFlow, nodeNavigationPath } from "./topology";
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

describe("nodeNavigationPath", () => {
  it("builds /nodes/<name> for a real node, URL-encoding the name", () => {
    expect(nodeNavigationPath({ id: "node-a", type: "topoNode" })).toBe("/nodes/node-a");
  });

  it("round-trips a node name that needs URL encoding", () => {
    const name = "weird node/name äöü";
    expect(nodeNavigationPath({ id: name, type: "topoNode" })).toBe(`/nodes/${encodeURIComponent(name)}`);
    expect(decodeURIComponent(nodeNavigationPath({ id: name, type: "topoNode" })!.slice("/nodes/".length))).toBe(name);
  });

  it("ignores a zone container click", () => {
    expect(nodeNavigationPath({ id: "zone:z1", type: "zone" })).toBeUndefined();
  });

  /* QA round 2, finding #7: a click on a map of 12:00 must land on a card of
     12:00. The stamp is the Time Machine's own (RFC 3339, UTC, seconds). */
  it("carries the engaged instant to the node card", () => {
    const at = new Date("2026-08-01T12:00:00Z");
    expect(nodeNavigationPath({ id: "node-a", type: "topoNode" }, at)).toBe(
      `/nodes/node-a?at=${encodeURIComponent("2026-08-01T12:00:00Z")}`,
    );
  });

  it("adds nothing while Live", () => {
    expect(nodeNavigationPath({ id: "node-a", type: "topoNode" }, null)).toBe("/nodes/node-a");
  });
});

/* ── QA round 2, findings #1, #9 and #22 ─────────────────────────────────── */

describe("buildFlow — a pair measured without a failure ratio", () => {
  // Loss 5%, and the fail counter has never fired: exactly the state that used
  // to make the pair invisible on this map.
  const lossy: Matrix = {
    protocol: "udp", plane: "pod", nodes: ["n1", "n2"],
    cells: [{ source: "n1", destination: "n2", failRatio: null, rttP95: 2e6, lossRatio: 0.05 }],
    timestamp: "t",
  };

  it("draws the problem edge from packet loss alone", () => {
    const { edges, problemTotal } = buildFlow(topo, lossy);
    expect(problemTotal).toBe(1);
    expect(edges[0].id).toBe("n1->n2");
    expect(edges[0].className).toBe("topo-edge--degraded");
  });

  it("names the vector the percentage came from", () => {
    const { edges } = buildFlow(topo, lossy);
    expect(edges[0].data).toMatchObject({ failLabel: "5% loss" });
  });

  it("colours the source node from the loss it cannot see in the fail series", () => {
    const { nodes } = buildFlow(topo, lossy);
    expect(nodes.find((n) => n.id === "n1")?.className).toContain("degraded");
  });
});

describe("buildFlow — back edges", () => {
  const mutual: Matrix = {
    protocol: "tcp", plane: "pod", nodes: ["n1", "n2"],
    cells: [
      { source: "n1", destination: "n2", failRatio: 0.2 },
      { source: "n2", destination: "n1", failRatio: 0.15 },
    ],
    timestamp: "t",
  };

  it("gives an opposite pair symmetric, direction-keyed routing", () => {
    const { edges } = buildFlow(topo, mutual);
    const forward = edges.find((e) => e.id === "n1->n2");
    const back = edges.find((e) => e.id === "n2->n1");
    expect(forward).toMatchObject({ pathOptions: { stepPosition: 0.35, offset: 16 } });
    expect(back).toMatchObject({ pathOptions: { stepPosition: 0.65, offset: 44 } });
  });

  it("leaves a lone edge on the default routing — nothing to bow around", () => {
    const { edges } = buildFlow(topo, matrix);
    expect(edges[0]).not.toHaveProperty("pathOptions");
  });
});

describe("buildFlow — node accessibility", () => {
  it("labels every node with who, where and how", () => {
    const { nodes } = buildFlow(topo, matrix);
    // n1's worst outbound path is 20%, so it reads failing; n2 is not ready.
    expect(nodes.find((n) => n.id === "n1")?.ariaLabel).toBe("n1, zone z1, failing");
    expect(nodes.find((n) => n.id === "n2")?.ariaLabel).toBe("n2, zone z2, failing, not ready");
  });

  it("says healthy in words rather than in the class name's vocabulary", () => {
    const quiet: Matrix = { protocol: "tcp", plane: "pod", nodes: ["n1"], cells: [], timestamp: "t" };
    const oneReady: Topology = { nodes: [{ name: "n1", zone: "z1", ready: true }], agents: [], timestamp: "t" };
    expect(buildFlow(oneReady, quiet).nodes.find((n) => n.id === "n1")?.ariaLabel).toBe("n1, zone z1, healthy");
  });
});
