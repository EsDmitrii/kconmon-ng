import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OverviewPage, summarize } from "./overview";
import type { Matrix, Topology } from "@/lib/types";

const matrix: Matrix = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["a", "b", "c"],
  cells: [
    { source: "a", destination: "b", failRatio: null }, // unmeasured — excluded
    { source: "a", destination: "c", failRatio: 0.005 }, // healthy
    { source: "b", destination: "a", failRatio: 0.02 }, // degraded
    { source: "b", destination: "c", failRatio: 0.15, rttP95: 3_000_000 }, // failing
    { source: "c", destination: "a", failRatio: 0.5, rttP95: 9_000_000 }, // failing
  ],
  timestamp: "2026-01-01T00:00:00Z",
};

const topo: Topology = {
  nodes: [
    { name: "a", zone: "z1", ready: true },
    { name: "b", zone: "z1", ready: false },
  ],
  agents: [],
  timestamp: "2026-01-01T00:00:00Z",
};

describe("summarize", () => {
  it("counts failing/degraded/total, excluding null failRatio", () => {
    const s = summarize(matrix, topo);
    expect(s.pairsTotal).toBe(4); // the null cell is excluded
    expect(s.pairsFailing).toBe(2); // 0.15, 0.5
    expect(s.pairsDegraded).toBe(1); // 0.02
  });

  it("returns the top 5 problem pairs ordered by failRatio desc", () => {
    const many: Matrix = {
      ...matrix,
      cells: [0.5, 0.4, 0.3, 0.2, 0.15, 0.12, 0.02].map((r, i) => ({
        source: `s${i}`,
        destination: `d${i}`,
        failRatio: r,
      })),
    };
    const s = summarize(many);
    expect(s.worstPairs).toHaveLength(5);
    expect(s.worstPairs.map((c) => c.failRatio)).toEqual([0.5, 0.4, 0.3, 0.2, 0.15]);
  });

  it("falls back to matrix.nodes when topology is absent", () => {
    const s = summarize(matrix);
    expect(s.totalNodes).toBe(3);
    expect(s.readyNodes).toBe(3);
  });

  it("prefers topology node counts when present", () => {
    const s = summarize(matrix, topo);
    expect(s.totalNodes).toBe(2);
    expect(s.readyNodes).toBe(1);
  });
});

afterEach(() => vi.unstubAllGlobals());

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <OverviewPage />
    </QueryClientProvider>,
  );
}

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

describe("OverviewPage", () => {
  it("shows the ready-nodes tile as readyNodes/totalNodes from the API", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) =>
        Promise.resolve(String(url).includes("/topology") ? json(topo) : json(matrix)),
      ),
    );
    renderPage();
    expect(await screen.findByText("1/2")).toBeInTheDocument();
    expect(screen.getByText("Nodes ready")).toBeInTheDocument();
  });
});
