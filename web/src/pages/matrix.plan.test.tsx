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
function stubFetchRoutes(topology: unknown, matrix: unknown = matrixBody) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const u = String(url);
      if (u.includes("/api/v1/topology")) return Promise.resolve(json(topology));
      if (u.includes("/api/v1/matrix")) return Promise.resolve(json(matrix));
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

/* ── the plan outranks the other two silences (2.4.0) ─────────────────────────
 *
 * An unmeasured cell can now be quiet for three reasons — the plan excludes the
 * pair, the source advertised planes without this protocol, or the source is an
 * external agent Prometheus never scrapes — and a cell says ONE of them. The
 * plan comes first: it is the operator's own statement about the pair, and the
 * other two are inferences.
 */
describe("MatrixPage — the plan outranks the other silences", () => {
  /* a → b is the only measured pair. a runs UDP only (no TCP); c is a bare host with no series of
     its own. The plan: a probes b, b probes a, c probes nobody. */
  const layeredMatrix = {
    ...matrixBody,
    cells: [{ source: "a", destination: "b", failRatio: 0.5, rttP95: 2_000_000 }],
  };
  const layeredTopology = {
    ...sparseTopology,
    agents: [
      { id: "ag-a", nodeName: "a", podIP: "10.0.0.1", zone: "z1", capabilities: ["plane:udp"] },
      { id: "ag-b", nodeName: "b", podIP: "10.0.0.2", zone: "z1" },
      {
        id: "ag-c", nodeName: "c", podIP: "192.0.2.10", zone: "office",
        labels: { "kconmon-ng.io/external": "true" },
        capabilities: ["plane:tcp"],
      },
    ],
  };

  it("reads a pair that is excluded AND unsupported as 'not probed'", async () => {
    stubFetchRoutes(layeredTopology, layeredMatrix);
    renderPage();
    const cell = await screen.findByLabelText("a → c: not probed by the topology plan");
    fireEvent.mouseEnter(cell.querySelector("div.border-dashed") as Element);
    const tooltip = await screen.findByRole("tooltip");
    expect(tooltip).toHaveTextContent("The sparse topology plan assigns no agent to probe this pair");
    expect(tooltip).not.toHaveTextContent("does not run");
    expect(screen.queryAllByLabelText(/does not run TCP probes$/)).toHaveLength(0);
  });

  it("reads a pair that is excluded AND unscraped as 'not probed', not as a scrape gap", async () => {
    stubFetchRoutes(layeredTopology, layeredMatrix);
    renderPage();
    const cell = await screen.findByLabelText("c → a: not probed by the topology plan");
    fireEvent.mouseEnter(cell.querySelector("div.border-dashed") as Element);
    const tooltip = await screen.findByRole("tooltip");
    expect(tooltip).toHaveTextContent("The sparse topology plan assigns no agent to probe this pair");
    expect(tooltip).not.toHaveTextContent("No series");
  });

  it("gates each legend row on its own cells: the plan's row is there, the unsupported one is not", async () => {
    stubFetchRoutes(layeredTopology, layeredMatrix);
    renderPage();
    await screen.findByTestId("legend-not-probed");
    // a's one planned pair is measured and its other is the plan's: no unsupported cell survives.
    expect(screen.queryByTestId("legend-unsupported")).not.toBeInTheDocument();
    // The planned-but-silent pair is still the alarming kind of silence.
    expect(screen.getByLabelText("b → a: no data")).toBeInTheDocument();
  });
});

/* ── the note above the grid obeys the same precedence as the cells ─────────
 *
 * The 'unscraped' reading is per plane: an external agent that advertised planes
 * without this protocol has a KNOWN cause for its silent row (unsupported), and
 * the note must not tell the operator to add a scrape job for it. The same
 * agent, on a grid for a plane it does run, is a scrape gap and is named.
 */
describe("MatrixPage — the unscraped note is gated by the plane on show", () => {
  /* ext-1 runs TCP and MTR only; a is an in-cluster agent with no plane list (every plane).
     The one cell is a → ext-1: ext-1 is a destination everywhere and a source nowhere. */
  const gatedTopology = {
    nodes: [{ name: "a", zone: "z1", ready: true }],
    agents: [
      { id: "ag-a", nodeName: "a", podIP: "10.0.0.1", zone: "z1" },
      {
        id: "ag-ext", nodeName: "ext-1", podIP: "192.0.2.10", zone: "office",
        labels: { "kconmon-ng.io/external": "true" },
        capabilities: ["external-checks", "plane:tcp", "plane:mtr"],
      },
    ],
    timestamp: "2026-01-01T00:00:00Z",
  };
  const gatedMatrix = (protocol: string) => ({
    protocol, plane: "pod", nodes: ["a", "ext-1"],
    cells: [{ source: "a", destination: "ext-1", failRatio: 0 }],
    timestamp: "2026-01-01T00:00:00Z",
  });

  afterEach(() => window.history.pushState({}, "", "/"));

  it("on the UDP grid: the row is 'unsupported' and no scrape note is shown", async () => {
    window.history.pushState({}, "", "/matrix?protocol=udp");
    stubFetchRoutes(gatedTopology, gatedMatrix("udp"));
    renderPage();
    expect(await screen.findByLabelText("ext-1 → a: the source does not run UDP probes")).toBeInTheDocument();
    expect(screen.queryByTestId("matrix-unscraped-note")).not.toBeInTheDocument();
    expect(screen.queryByText(/Prometheus is not scraping/)).not.toBeInTheDocument();
  });

  it("on the TCP grid the same host IS a scrape gap: the note names it and the row says why", async () => {
    window.history.pushState({}, "", "/matrix?protocol=tcp");
    stubFetchRoutes(gatedTopology, gatedMatrix("tcp"));
    renderPage();
    const note = await screen.findByTestId("matrix-unscraped-note");
    expect(note).toHaveTextContent("ext-1 is an external agent Prometheus is not scraping");
    expect(screen.getByLabelText(/^ext-1 → a: /)).not.toHaveAccessibleName(/does not run/);
    expect(screen.queryAllByLabelText(/does not run TCP probes$/)).toHaveLength(0);
  });
});

/* A plane switched off fleet-wide: every agent re-advertises without it, Prometheus stops
   carrying its series and the matrix comes back with no nodes. The known cause wins over the
   generic "no probe data yet". */
describe("MatrixPage — a plane no agent runs", () => {
  const offTopology = {
    nodes: [
      { name: "a", zone: "z1", ready: true },
      { name: "b", zone: "z1", ready: true },
    ],
    agents: [
      { id: "ag-a", nodeName: "a", podIP: "10.0.0.1", zone: "z1", capabilities: ["plane:tcp"] },
      { id: "ag-b", nodeName: "b", podIP: "10.0.0.2", zone: "z1", capabilities: ["plane:tcp", "plane:udp"] },
    ],
    timestamp: "2026-01-01T00:00:00Z",
  };
  const emptyMatrix = { protocol: "icmp", plane: "pod", nodes: [], cells: [], timestamp: "2026-01-01T00:00:00Z" };

  afterEach(() => window.history.pushState({}, "", "/"));

  it("draws the fleet's rows as 'does not run' instead of 'No probe data yet'", async () => {
    window.history.pushState({}, "", "/matrix?protocol=icmp");
    stubFetchRoutes(offTopology, emptyMatrix);
    renderPage();
    expect(await screen.findByLabelText("a → b: the source does not run ICMP probes")).toBeInTheDocument();
    expect(screen.getByLabelText("b → a: the source does not run ICMP probes")).toBeInTheDocument();
    expect(screen.getByTestId("legend-unsupported")).toBeInTheDocument();
    expect(screen.queryByText(/No probe data/)).not.toBeInTheDocument();
  });

  /* What the API now answers for a plane switched off fleet-wide: the fleet as nodes, no cells. */
  it("says the fleet does not run the protocol when the API names the fleet with no cells", async () => {
    window.history.pushState({}, "", "/matrix?protocol=icmp");
    stubFetchRoutes(offTopology, { ...emptyMatrix, nodes: ["a", "b"] });
    renderPage();
    expect(await screen.findByLabelText("a → b: the source does not run ICMP probes")).toBeInTheDocument();
    expect(screen.getByLabelText("b → a: the source does not run ICMP probes")).toBeInTheDocument();
    expect(screen.getByTestId("legend-unsupported")).toBeInTheDocument();
    expect(screen.queryAllByLabelText(/: no data$/)).toHaveLength(0);
    expect(screen.queryByText(/No probe data/)).not.toBeInTheDocument();
  });

  it("keeps the empty state while one agent still runs the plane or advertises no planes", async () => {
    window.history.pushState({}, "", "/matrix?protocol=icmp");
    stubFetchRoutes(
      { ...offTopology, agents: [...offTopology.agents, { id: "ag-c", nodeName: "c", podIP: "10.0.0.3", zone: "z1" }] },
      emptyMatrix,
    );
    renderPage();
    expect(await screen.findByText("No probe data in Prometheus yet")).toBeInTheDocument();
    expect(screen.queryAllByLabelText(/does not run/)).toHaveLength(0);
  });
});
