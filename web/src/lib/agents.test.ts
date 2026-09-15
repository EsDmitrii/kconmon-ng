import { describe, expect, it } from "vitest";
import {
  EXTERNAL_LABEL,
  EXTERNAL_SCRAPE_DOCS_URL,
  PLANE_CAPABILITY_PREFIX,
  PLANES,
  agentPlanes,
  externalByNode,
  isExternalAgent,
  runsPlane,
  unscrapedExternalNodes,
} from "./agents";
import { matrixDict } from "./i18n/dict/matrix";
import type { Matrix, Topology, TopologyAgent } from "./types";

/**
 * lib/agents.ts against the inputs the console actually receives: an in-cluster
 * agent, a bare-host agent with the external label, and a matrix that either
 * has or has not got a row for it. agents.hostile.test.ts is the other half —
 * the same helpers under shapes no controller sends.
 */

const inCluster: TopologyAgent = { id: "a-1", nodeName: "node-a", podIP: "10.1.0.1", zone: "az-1" };
const inClusterB: TopologyAgent = { id: "a-2", nodeName: "node-b", podIP: "10.1.0.2", zone: "az-1" };
const external: TopologyAgent = {
  id: "ext-1",
  nodeName: "mac-external-01",
  podIP: "192.0.2.10",
  zone: "office",
  labels: { [EXTERNAL_LABEL]: "true" },
  capabilities: ["external-checks", "plane:tcp", "plane:udp", "plane:mtr"],
};
const externalTwo: TopologyAgent = {
  id: "ext-2",
  nodeName: "dc-edge-02",
  podIP: "192.0.2.11",
  zone: "dc",
  labels: { [EXTERNAL_LABEL]: "true" },
};

function topo(...agents: TopologyAgent[]): Topology {
  return {
    nodes: [
      { name: "node-a", zone: "az-1", ready: true },
      { name: "node-b", zone: "az-1", ready: true },
    ],
    agents,
    timestamp: "2026-09-02T12:00:00Z",
  };
}

function matrix(...pairs: Array<[string, string]>): Matrix {
  return {
    protocol: "tcp",
    plane: "pod",
    nodes: [],
    cells: pairs.map(([source, destination]) => ({ source, destination, failRatio: 0 })),
    timestamp: "2026-09-02T12:00:00Z",
  } as unknown as Matrix;
}

describe("the shared vocabulary", () => {
  it("names the label and the capability prefix exactly as internal/model/agent.go does", () => {
    // The agent stamps these strings on itself; the console reads them back verbatim.
    expect(EXTERNAL_LABEL).toBe("kconmon-ng.io/external");
    expect(PLANE_CAPABILITY_PREFIX).toBe("plane:");
  });

  it("knows the six planes the agent can advertise, in the agent's own spelling", () => {
    expect([...PLANES]).toEqual(["tcp", "udp", "icmp", "dns", "http", "mtr"]);
  });

  it("reads the scrape docs URL out of the matrix dictionary, so the target changes in one place", () => {
    expect(EXTERNAL_SCRAPE_DOCS_URL).toBe(matrixDict.en["docs.scrapeExternal"]);
    expect(EXTERNAL_SCRAPE_DOCS_URL).toBe(
      "https://esdmitrii.github.io/kconmon-ng/external-agents/#scraping-external-agents",
    );
  });
});

describe("isExternalAgent", () => {
  it("is true for the label with the exact value \"true\"", () => {
    expect(isExternalAgent(external)).toBe(true);
  });

  it("is false for an in-cluster agent, which carries no labels at all", () => {
    expect(isExternalAgent(inCluster)).toBe(false);
  });

  it("is false for the label set to anything but \"true\"", () => {
    expect(isExternalAgent({ ...inCluster, labels: { [EXTERNAL_LABEL]: "false" } })).toBe(false);
    expect(isExternalAgent({ ...inCluster, labels: { [EXTERNAL_LABEL]: "" } })).toBe(false);
  });

  it("is false when other labels are present but not this one", () => {
    expect(isExternalAgent({ ...inCluster, labels: { site: "dc-1", external: "true" } })).toBe(false);
  });

  it("is false for no agent", () => {
    expect(isExternalAgent(undefined)).toBe(false);
  });
});

describe("agentPlanes", () => {
  it("reads the plane:* capabilities into a set", () => {
    expect(agentPlanes(external)).toEqual(new Set(["tcp", "udp", "mtr"]));
  });

  it("answers null — unknown, assume every plane — for an agent that advertised none", () => {
    // Every agent older than 2.4.0 sends no plane:* at all. Reading that as
    // "runs nothing" would turn every one of its cells into a calming dashed
    // frame during a rolling upgrade.
    expect(agentPlanes(inCluster)).toBeNull();
    expect(agentPlanes({ ...inCluster, capabilities: [] })).toBeNull();
    expect(agentPlanes({ ...inCluster, capabilities: ["external-checks"] })).toBeNull();
  });

  it("ignores capabilities that are not planes, and plane names it does not know", () => {
    expect(agentPlanes({ ...inCluster, capabilities: ["external-checks", "plane:icmp", "plane:quic"] })).toEqual(
      new Set(["icmp"]),
    );
  });

  it("stays null when the only plane names are ones it cannot read — unreadable is unknown, not none", () => {
    expect(agentPlanes({ ...inCluster, capabilities: ["plane:quic"] })).toBeNull();
  });

  it("collapses a repeated plane", () => {
    expect(agentPlanes({ ...inCluster, capabilities: ["plane:tcp", "plane:tcp"] })).toEqual(new Set(["tcp"]));
  });

  it("answers null for no agent", () => {
    expect(agentPlanes(undefined)).toBeNull();
  });
});

describe("runsPlane", () => {
  it("is true for an advertised plane and false for one the agent left out", () => {
    expect(runsPlane(external, "tcp")).toBe(true);
    expect(runsPlane(external, "icmp")).toBe(false);
  });

  it("is true for every plane when the agent advertised none — the fail-open rule in one call", () => {
    for (const plane of PLANES) expect(runsPlane(inCluster, plane)).toBe(true);
    for (const plane of PLANES) expect(runsPlane(undefined, plane)).toBe(true);
  });
});

describe("externalByNode", () => {
  it("maps each external agent by the node name it registered under, and nobody else", () => {
    const byNode = externalByNode(topo(inCluster, external, inClusterB, externalTwo));
    expect([...byNode.keys()]).toEqual(["mac-external-01", "dc-edge-02"]);
    expect(byNode.get("mac-external-01")).toBe(external);
    expect(byNode.has("node-a")).toBe(false);
  });

  it("is empty for a fleet with no external agent", () => {
    expect(externalByNode(topo(inCluster, inClusterB)).size).toBe(0);
  });

  it("keeps the first agent when two external agents claim one node name", () => {
    const twin = { ...external, id: "ext-1-twin", podIP: "192.0.2.99" };
    expect(externalByNode(topo(external, twin)).get("mac-external-01")).toBe(external);
  });

  it("skips an external agent with no node name — there is nothing to key it by", () => {
    expect(externalByNode(topo({ ...external, nodeName: "" })).size).toBe(0);
  });
});

describe("unscrapedExternalNodes", () => {
  const fleet = topo(inCluster, external, inClusterB, externalTwo);

  it("lists the external agents that are never a cell's source while the matrix has cells", () => {
    expect(unscrapedExternalNodes(fleet, matrix(["node-a", "node-b"], ["node-b", "node-a"]), "tcp")).toEqual([
      "mac-external-01",
      "dc-edge-02",
    ]);
  });

  it("drops an external agent the moment it is the source of a single cell", () => {
    expect(unscrapedExternalNodes(fleet, matrix(["node-a", "node-b"], ["mac-external-01", "node-a"]), "tcp")).toEqual([
      "dc-edge-02",
    ]);
  });

  it("still lists an external agent that appears only as a DESTINATION", () => {
    // Its column fills from the in-cluster agents' probes; its row needs its own
    // metrics scraped, and that is the half that is missing.
    expect(
      unscrapedExternalNodes(fleet, matrix(["node-a", "mac-external-01"], ["node-b", "dc-edge-02"]), "tcp"),
    ).toEqual(["mac-external-01", "dc-edge-02"]);
  });

  it("answers nothing while the matrix has no cells at all — silence everywhere is not a scrape gap", () => {
    expect(unscrapedExternalNodes(fleet, matrix(), "tcp")).toEqual([]);
  });

  it("never lists an in-cluster agent, even one with no source cell", () => {
    expect(unscrapedExternalNodes(topo(inCluster, inClusterB), matrix(["node-a", "node-b"]), "tcp")).toEqual([]);
  });

  it("follows the topology's agent order and names each node once", () => {
    const twin = { ...externalTwo, id: "ext-2-twin" };
    expect(unscrapedExternalNodes(topo(externalTwo, external, twin), matrix(["node-a", "node-b"]), "tcp")).toEqual([
      "dc-edge-02",
      "mac-external-01",
    ]);
  });

  /* Unscraped is silence with NO other known cause. An agent that advertised its planes and left
     this one out has a cause — its row is 'unsupported' on that grid — and a scrape job would not
     fill it, so the note must not send the operator to write one. */
  describe("is read per plane", () => {
    const silent = matrix(["node-a", "node-b"], ["node-b", "node-a"]);

    it("drops an external agent that advertised planes without this one", () => {
      // mac-external-01 runs tcp, udp and mtr; dc-edge-02 advertised nothing (every plane).
      expect(unscrapedExternalNodes(fleet, silent, "icmp")).toEqual(["dc-edge-02"]);
    });

    it("keeps it on every plane it did advertise", () => {
      expect(unscrapedExternalNodes(fleet, silent, "udp")).toEqual(["mac-external-01", "dc-edge-02"]);
    });

    it("keeps an agent that advertised no plane at all — the fail-open rule, so a rolling upgrade hides nothing", () => {
      expect(unscrapedExternalNodes(topo(externalTwo), silent, "icmp")).toEqual(["dc-edge-02"]);
    });

    it("answers nothing when every external agent left the plane out", () => {
      expect(unscrapedExternalNodes(topo(inCluster, external), silent, "icmp")).toEqual([]);
    });

    it("reads the plane off the FIRST agent claiming the name, the same one externalByNode keeps", () => {
      const twin = { ...external, id: "ext-1-twin", capabilities: ["plane:icmp"] };
      expect(unscrapedExternalNodes(topo(external, twin), silent, "icmp")).toEqual([]);
      expect(unscrapedExternalNodes(topo(twin, external), silent, "icmp")).toEqual(["mac-external-01"]);
    });
  });
});
