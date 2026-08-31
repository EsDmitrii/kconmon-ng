import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { MatrixPage } from "./matrix";

/*
The sparse-plan surface (M10): pairs the topology plan excludes render a 'not probed' cell —
expected silence — visually and semantically apart from 'no data', which stays the reading for a
pair something SHOULD have measured. The plan rides GET /api/v1/topology's probePlan; the matrix
payload itself never changes shape.
*/

/* Three nodes; one pair measured each way of the diagonal story:
     a → b measured, c → a measured (the plan has since dropped it — data must outrank the plan). */
const matrixBody = {
  protocol: "tcp", plane: "pod", nodes: ["a", "b", "c"],
  cells: [
    { source: "a", destination: "b", failRatio: 0.5, rttP95: 2_000_000 },
    { source: "c", destination: "a", failRatio: 0.02 },
  ],
  timestamp: "2026-01-01T00:00:00Z",
};

const topologyBase = {
  nodes: [
    { name: "a", zone: "z1", ready: true },
    { name: "b", zone: "z1", ready: true },
    { name: "c", zone: "z2", ready: true },
  ],
  agents: [],
  timestamp: "2026-01-01T00:00:00Z",
};

/* a probes b; b probes a; c probes nobody (the plan's fail-closed empty list). Complement among
   the six non-self pairs, minus the measured c→a: a→c, b→c, c→b are the 'not probed' cells. */
const sparseTopology = {
  ...topologyBase,
  probePlan: { a: ["b"], b: ["a"], c: [] },
};

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

/** Routes the three GETs the page now issues; topology is the parameterized one. */
function stubFetchRoutes(topology: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const u = String(url);
      if (u.includes("/api/v1/topology")) return Promise.resolve(json(topology));
      if (u.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
      return Promise.resolve(json({ version: "2.3.0", commit: "abc" }));
    }),
  );
}

function renderPage(ui: React.ReactNode = <MatrixPage />) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

const NOT_PROBED = /: not probed by the topology plan$/;

describe("MatrixPage — sparse topology plan", () => {
  it("full mode renders zero 'not probed' cells and no plan legend row", async () => {
    stubFetchRoutes(topologyBase); // no probePlan field: full mesh
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    expect(screen.queryAllByLabelText(NOT_PROBED)).toHaveLength(0);
    // Every unmeasured non-self pair stays the alarming kind of silence.
    expect(screen.getAllByLabelText(/: no data$/)).toHaveLength(4);
    expect(screen.queryByTestId("legend-not-probed")).not.toBeInTheDocument();
  });

  it("marks exactly the plan's complement as 'not probed', and the legend gains the state", async () => {
    stubFetchRoutes(sparseTopology);
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    await screen.findByTestId("legend-not-probed");

    const notProbed = screen.getAllByLabelText(NOT_PROBED);
    expect(notProbed.map((el) => el.getAttribute("aria-label")).sort()).toEqual([
      "a → c: not probed by the topology plan",
      "b → c: not probed by the topology plan",
      "c → b: not probed by the topology plan",
    ]);

    // The planned-but-silent pair keeps the 'no data' reading — the two states never merge.
    expect(screen.getByLabelText("b → a: no data")).toBeInTheDocument();
    // Data outranks the plan: c → a is excluded NOW but measured, so the measurement renders.
    expect(screen.getByLabelText("c → a: fail 2.0%")).toBeInTheDocument();
  });

  it("keeps a 'not probed' cell off the pair page but leaves Investigate in reach", async () => {
    stubFetchRoutes(sparseTopology);
    renderPage();
    const cell = await screen.findByLabelText("a → c: not probed by the topology plan");
    // The label sits on the <td>: no pair-page link anywhere inside — that page promises
    // continuous history a plan-excluded pair will never grow.
    expect(cell.tagName).toBe("TD");
    expect(cell.querySelector('a[href^="/pairs/"]')).toBeNull();
    // Investigate stays: probing on demand is the promise the cell CAN keep.
    const investigate = cell.querySelector('[data-testid="cell-investigate"]');
    expect(investigate).not.toBeNull();
    expect(investigate?.getAttribute("href") ?? "").toContain("/investigate");

    // A planned cell keeps its pair link, unchanged.
    const planned = screen.getByLabelText("b → a: no data");
    expect(planned).toHaveAttribute("href", "/pairs/b/a");
  });

  it("explains the state in a hover tooltip", async () => {
    stubFetchRoutes(sparseTopology);
    renderPage();
    const cell = await screen.findByLabelText("a → c: not probed by the topology plan");
    const box = cell.querySelector("div.border-dashed");
    expect(box).not.toBeNull();
    fireEvent.mouseEnter(box as Element);
    const tooltip = await screen.findByRole("tooltip");
    expect(tooltip).toHaveTextContent("The sparse topology plan assigns no agent to probe this pair");
  });

  it("speaks the state in Russian", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    stubFetchRoutes(sparseTopology);
    renderPage(
      <LocaleProvider>
        <MatrixPage />
      </LocaleProvider>,
    );
    expect(
      await screen.findByLabelText("a → c: не зондируется по плану топологии"),
    ).toBeInTheDocument();
    expect(screen.getByTestId("legend-not-probed")).toHaveTextContent(
      "Не зондируется · исключено планом топологии",
    );
  });
});
