import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { EXTERNAL_LABEL } from "@/lib/agents";
import type { Topology } from "@/lib/types";
import { TopologyPage, buildFlow, mapNodes } from "./topology";

/*
 * The topology map under a hostile wire. `labels` is a field the console never
 * read before 2.4.0 and one a controller of any age may send in any shape: a
 * nil Go map that escaped omitempty, an array, a string, a label with the
 * right word under the wrong key. The bar is the one overview.hostile sets:
 * nothing throws (there is no error boundary above these routes, so a throw
 * in render IS a blank page), and no box wears a badge the label did not earn.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const nodes: Topology["nodes"] = [
  { name: "n1", zone: "z1", ready: true },
  { name: "n2", zone: "z1", ready: true },
];

/* A cast, not a fixture: every body here is a shape the type forbids. */
const T = (over: Record<string, unknown>): Topology => ({ nodes, agents: [], timestamp: "t", ...over }) as Topology;

const agent = (over: Record<string, unknown>) =>
  ({ id: "x", nodeName: "edge-01", podIP: "192.0.2.10", zone: "z1", ...over });

function renderPage(topology: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.startsWith("/api/v1/topology")) return Promise.resolve(json(topology));
      if (href.includes("/api/v1/promql/query")) {
        return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
      }
      return Promise.resolve(json({}));
    }),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TopologyPage />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/* Each of these is an agent with no Kubernetes node and NO usable label: what
   it must not become is a box, or a badge. */
const NOT_A_LABEL_MAP: [string, unknown][] = [
  ["null", null],
  ["an empty array", []],
  ["an array holding the word", ["true"]],
  ["a string", "true"],
  ["a number", 1],
  ["a boolean", true],
  ["a map with the word under the wrong key", { external: "true" }],
  ["a map spelling the value True", { [EXTERNAL_LABEL]: "True" }],
  ["a map with a boolean value", { [EXTERNAL_LABEL]: true }],
];

describe("mapNodes under a hostile labels field", () => {
  it.each(NOT_A_LABEL_MAP)("does not throw and draws no extra box when labels is %s", (_, labels) => {
    const { nodes: drawn, source } = mapNodes(T({ agents: [agent({ labels })] }));
    expect(source).toBe("nodes");
    expect(drawn.map((n) => n.name)).toEqual(["n1", "n2"]);
    expect(drawn.every((n) => n.external === false)).toBe(true);
  });

  it("does not throw when the agents half is not a list, or holds things that are not agents", () => {
    for (const agents of [null, undefined, "none", 0, {}, [null, 1, "x", [], { labels: { [EXTERNAL_LABEL]: "true" } }]]) {
      const { nodes: drawn } = mapNodes(T({ agents }));
      expect(drawn.map((n) => n.name)).toEqual(["n1", "n2"]);
    }
  });

  it("does not throw when a labelled agent names no node, or names it with something that is not a string", () => {
    for (const nodeName of [undefined, null, "", 7, ["edge-01"], { name: "edge-01" }]) {
      const { nodes: drawn } = mapNodes(T({ agents: [agent({ nodeName, labels: { [EXTERNAL_LABEL]: "true" } })] }));
      expect(drawn.map((n) => n.name)).toEqual(["n1", "n2"]);
    }
  });

  it("gives a labelled agent with an unreadable zone the unnamed lane, not the word undefined", () => {
    for (const zone of [undefined, null, 7, {}]) {
      const edge = mapNodes(T({ agents: [agent({ zone, labels: { [EXTERNAL_LABEL]: "true" } })] })).nodes.find(
        (n) => n.name === "edge-01",
      );
      expect(edge).toEqual({ name: "edge-01", zone: "", ready: undefined, external: true });
    }
  });

  it("draws one box for a name two labelled agents both claim", () => {
    const twice = T({
      agents: [
        agent({ id: "x1", labels: { [EXTERNAL_LABEL]: "true" } }),
        agent({ id: "x2", zone: "z9", labels: { [EXTERNAL_LABEL]: "true" } }),
      ],
    });
    const { nodes: drawn } = buildFlow(twice);
    const ids = drawn.map((n) => n.id);
    expect(new Set(ids).size).toBe(ids.length);
    expect(drawn.filter((n) => n.type === "topoNode").map((n) => n.id)).toEqual(["edge-01", "n1", "n2"]);
  });
});

describe("TopologyPage under a hostile labels field", () => {
  it.each(NOT_A_LABEL_MAP)("renders the Kubernetes map, badge-less, when an agent's labels is %s", async (_, labels) => {
    renderPage(T({ agents: [agent({ labels })] }));
    await screen.findByTestId("edge-caption");
    expect(document.querySelectorAll(".react-flow__node.topo-node")).toHaveLength(2);
    expect(screen.queryByText("external")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/undefined|NaN|\[object/);
  });

  it("renders when the agents half is null", async () => {
    renderPage({ nodes, agents: null, timestamp: "t" });
    await screen.findByTestId("edge-caption");
    expect(document.querySelectorAll(".react-flow__node.topo-node")).toHaveLength(2);
  });

  it("renders a labelled agent whose labels ALSO carry junk, badge and all", async () => {
    renderPage(T({ agents: [agent({ labels: { [EXTERNAL_LABEL]: "true", site: null, 7: [] } })] }));
    await screen.findByTestId("edge-caption");
    expect(screen.getByLabelText("edge-01, zone z1, healthy, external agent, readiness unknown")).toBeInTheDocument();
    expect(screen.getAllByText("external")).toHaveLength(1);
  });
});
