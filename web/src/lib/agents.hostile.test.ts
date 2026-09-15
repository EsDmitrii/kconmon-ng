import { describe, expect, it } from "vitest";
import {
  EXTERNAL_LABEL,
  PLANES,
  agentPlanes,
  externalByNode,
  isExternalAgent,
  runsPlane,
  unscrapedExternalNodes,
} from "./agents";
import type { Matrix, Topology, TopologyAgent } from "./types";

/**
 * lib/agents.ts under nonsense.
 *
 * `labels` and `capabilities` are the two fields the console never had before
 * and the two a controller of any age may or may not send: absent from a
 * pre-2.4.0 controller, null from a Go zero value that escaped omitempty,
 * something else entirely from a websocket frame no normalizer saw. The
 * helpers read them the way lib/matrix-plan.ts reads probePlan — never throw,
 * and on any doubt fail toward the ALARMING answer: not external, planes
 * unknown (= every plane), no scrape gap claimed.
 */

/* A cast, not a fixture: every value here is a shape the type forbids. */
const agent = (over: Record<string, unknown>): TopologyAgent =>
  ({ id: "x", nodeName: "node-x", podIP: "10.0.0.1", zone: "z", ...over }) as unknown as TopologyAgent;

const HOSTILE_VALUES: unknown[] = [
  undefined,
  null,
  0,
  1,
  Number.NaN,
  true,
  "",
  "true",
  "plane:tcp",
  [],
  ["true"],
  ["plane:tcp"],
  {},
  { length: 1 },
  () => "true",
  Symbol("x"),
];

describe("isExternalAgent under hostile input", () => {
  it("is false for no agent, in every spelling of no agent", () => {
    for (const value of [undefined, null, 0, "", "true", [], () => true]) {
      expect(isExternalAgent(value as unknown as TopologyAgent)).toBe(false);
    }
  });

  it("is false for labels that are null, an array, a string, a number or a function", () => {
    for (const labels of HOSTILE_VALUES) {
      expect(isExternalAgent(agent({ labels })), `labels = ${String(labels)}`).toBe(false);
    }
  });

  it("is false for an array that happens to hold the word", () => {
    expect(isExternalAgent(agent({ labels: ["true"] }))).toBe(false);
    expect(isExternalAgent(agent({ labels: [EXTERNAL_LABEL, "true"] }))).toBe(false);
  });

  it("counts only the exact value \"true\": not True, TRUE, a boolean, a number or a padded string", () => {
    for (const value of ["True", "TRUE", true, 1, "1", "yes", " true", "true ", "true\n", ["true"], { v: "true" }]) {
      expect(isExternalAgent(agent({ labels: { [EXTERNAL_LABEL]: value } })), `value = ${String(value)}`).toBe(false);
    }
    expect(isExternalAgent(agent({ labels: { [EXTERNAL_LABEL]: "true" } }))).toBe(true);
  });

  it("does not match the label under a different case or spacing", () => {
    expect(isExternalAgent(agent({ labels: { "KCONMON-NG.IO/EXTERNAL": "true" } }))).toBe(false);
    expect(isExternalAgent(agent({ labels: { " kconmon-ng.io/external": "true" } }))).toBe(false);
  });

  it("is not fooled by a label object built on a poisoned prototype", () => {
    // JSON.parse makes "__proto__" an own property; a hand-built object could
    // instead carry the label on its prototype chain. Neither is the agent
    // saying it about itself.
    const inherited = Object.create({ [EXTERNAL_LABEL]: "true" }) as Record<string, string>;
    expect(isExternalAgent(agent({ labels: inherited }))).toBe(false);
  });
});

describe("agentPlanes under hostile input", () => {
  it("answers null — unknown — for capabilities that are not an array", () => {
    for (const capabilities of HOSTILE_VALUES) {
      if (Array.isArray(capabilities)) continue;
      expect(agentPlanes(agent({ capabilities })), `capabilities = ${String(capabilities)}`).toBeNull();
    }
  });

  it("skips members that are not strings without losing the ones that are", () => {
    expect(agentPlanes(agent({ capabilities: [null, 7, {}, [], undefined, "plane:udp", true] }))).toEqual(
      new Set(["udp"]),
    );
  });

  it("answers null when every member is unreadable", () => {
    expect(agentPlanes(agent({ capabilities: [null, 7, {}, []] }))).toBeNull();
  });

  it("reads the prefix exactly: no plane behind a bare, padded or upper-cased spelling", () => {
    // Unreadable is UNKNOWN, never "runs nothing": each of these leaves the
    // agent assumed to run every plane, the alarming direction.
    for (const capabilities of [["plane:"], ["plane: tcp"], ["plane:tcp "], ["PLANE:TCP"], ["Plane:tcp"], ["plane:TCP"], ["plane"], [" plane:tcp"]]) {
      expect(agentPlanes(agent({ capabilities })), capabilities.join(",")).toBeNull();
    }
  });

  it("never returns an empty set — an agent that advertised nothing readable is unknown", () => {
    for (const capabilities of [[], ["external-checks"], ["plane:quic"], [""], ["tcp"]]) {
      expect(agentPlanes(agent({ capabilities }))).toBeNull();
    }
  });

  it("answers null for no agent, in every spelling of no agent", () => {
    for (const value of [undefined, null, 0, "", "plane:tcp", [], ["plane:tcp"], () => ["plane:tcp"]]) {
      expect(agentPlanes(value as unknown as TopologyAgent)).toBeNull();
    }
  });

  it("keeps runsPlane fail-open through every shape it cannot read, and exact through the one it can", () => {
    for (const capabilities of HOSTILE_VALUES) {
      // ["plane:tcp"] is the one readable shape in the list: a real answer, not a doubt.
      if (Array.isArray(capabilities) && capabilities.includes("plane:tcp")) continue;
      for (const plane of PLANES) {
        expect(runsPlane(agent({ capabilities }), plane), `capabilities = ${String(capabilities)}`).toBe(true);
      }
    }
    expect(runsPlane(agent({ capabilities: ["plane:tcp"] }), "tcp")).toBe(true);
    expect(runsPlane(agent({ capabilities: ["plane:tcp"] }), "udp")).toBe(false);
  });
});

describe("externalByNode under hostile input", () => {
  it("is empty for no topology, in every spelling of no topology", () => {
    for (const value of HOSTILE_VALUES) {
      expect(externalByNode(value as unknown as Topology).size, `topo = ${String(value)}`).toBe(0);
    }
  });

  it("is empty for agents that are null or not an array", () => {
    for (const agents of HOSTILE_VALUES) {
      if (Array.isArray(agents)) continue;
      const topo = { nodes: [], agents, timestamp: "" } as unknown as Topology;
      expect(externalByNode(topo).size, `agents = ${String(agents)}`).toBe(0);
    }
  });

  it("skips agent entries that are not objects, keeping the real one beside them", () => {
    const real = agent({ nodeName: "edge-1", labels: { [EXTERNAL_LABEL]: "true" } });
    const agents = [null, undefined, 7, "agent", [], real, { labels: { [EXTERNAL_LABEL]: "true" } }];
    const topo = { nodes: [], agents, timestamp: "" } as unknown as Topology;
    expect([...externalByNode(topo).keys()]).toEqual(["edge-1"]);
  });

  it("skips an external agent whose node name is not a string", () => {
    for (const nodeName of [null, undefined, 7, [], {}, ""]) {
      const topo = {
        nodes: [],
        agents: [agent({ nodeName, labels: { [EXTERNAL_LABEL]: "true" } })],
        timestamp: "",
      } as unknown as Topology;
      expect(externalByNode(topo).size, `nodeName = ${String(nodeName)}`).toBe(0);
    }
  });

  it("reads an agent with labels: null as in-cluster, not as a crash", () => {
    const topo = { nodes: [], agents: [agent({ labels: null })], timestamp: "" } as unknown as Topology;
    expect(externalByNode(topo).size).toBe(0);
  });
});

describe("unscrapedExternalNodes under hostile input", () => {
  const external = agent({ nodeName: "edge-1", labels: { [EXTERNAL_LABEL]: "true" } });
  const fleet: Topology = { nodes: [], agents: [external], timestamp: "" };
  const cells = [{ source: "node-a", destination: "node-b", failRatio: 0 }];
  const withCells = { protocol: "tcp", plane: "pod", nodes: [], cells, timestamp: "" } as unknown as Matrix;

  it("claims no scrape gap for no matrix, in every spelling of no matrix", () => {
    for (const value of HOSTILE_VALUES) {
      expect(unscrapedExternalNodes(fleet, value as unknown as Matrix, "tcp"), `matrix = ${String(value)}`).toEqual([]);
    }
  });

  it("claims no scrape gap for cells that are null or not an array", () => {
    for (const bad of HOSTILE_VALUES) {
      if (Array.isArray(bad)) continue;
      const matrix = { ...withCells, cells: bad } as unknown as Matrix;
      expect(unscrapedExternalNodes(fleet, matrix, "tcp"), `cells = ${String(bad)}`).toEqual([]);
    }
  });

  it("is empty for no topology, in every spelling of no topology", () => {
    for (const value of HOSTILE_VALUES) {
      expect(unscrapedExternalNodes(value as unknown as Topology, withCells, "tcp"), `topo = ${String(value)}`).toEqual([]);
    }
  });

  it("ignores cell entries that are not objects or carry a non-string source", () => {
    const matrix = {
      ...withCells,
      cells: [null, 7, "cell", [], { destination: "x" }, { source: 7 }, { source: null }, ...cells],
    } as unknown as Matrix;
    expect(unscrapedExternalNodes(fleet, matrix, "tcp")).toEqual(["edge-1"]);
  });

  it("still sees the external agent's own row when it is the only readable cell", () => {
    const matrix = { ...withCells, cells: [null, { source: "edge-1", destination: "node-a" }] } as unknown as Matrix;
    expect(unscrapedExternalNodes(fleet, matrix, "tcp")).toEqual([]);
  });

  it("never throws for any pairing of hostile topology and hostile matrix", () => {
    for (const t of HOSTILE_VALUES) {
      for (const m of HOSTILE_VALUES) {
        expect(() => unscrapedExternalNodes(t as unknown as Topology, m as unknown as Matrix, "tcp")).not.toThrow();
      }
    }
  });
});
