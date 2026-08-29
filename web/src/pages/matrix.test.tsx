import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { parseInvestigationParams } from "@/lib/investigation-sources";
import { degradedProtocolParam, MatrixPage, readProtocolFromLocation } from "./matrix";

const matrixBody = {
  protocol: "tcp", plane: "pod", nodes: ["a", "b"],
  cells: [
    { source: "a", destination: "b", failRatio: 0.5, rttP95: 2_000_000 },
    { source: "b", destination: "a", failRatio: null },
  ],
  timestamp: "2026-01-01T00:00:00Z",
};

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
});

// A Response body can only be read once, and MatrixPage now issues two requests (/api/v1/matrix
// plus the /api/v1/version capability probe useMatrix feature-detects realtime with).
const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
    ...init,
  });

/**
 * The default stub answers /api/v1/version with the matrix body, which has no `capabilities` field
 * — realtime reads false.
 */
function stubFetch(body: unknown, init?: ResponseInit) {
  vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json(body, init))));
}

/** Advertises the realtime capability, so the page opens a (fake) socket. */
function stubFetchRealtime() {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) =>
      Promise.resolve(
        String(url).includes("/api/v1/version")
          ? json({ version: "1.6.0", commit: "abc123", capabilities: ["events"] })
          : json(matrixBody),
      ),
    ),
  );
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><MatrixPage /></QueryClientProvider>);
}

describe("MatrixPage", () => {
  it("renders the grid: fail ratio is the primary figure, RTT secondary", async () => {
    stubFetch(matrixBody);
    renderPage();
    const cell = await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    expect(cell).toHaveTextContent("50.0%");
    expect(cell).toHaveTextContent("2.0ms");
    expect(screen.getAllByRole("columnheader").length).toBeGreaterThanOrEqual(2);
    // No realtime capability on this reply, so the header must say so rather
    // than let the grid pass for live.
    expect(screen.getByText("Delayed data")).toBeInTheDocument();
  });

  it("says Delayed data whenever the console is on the polling fallback", async () => {
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    const badge = screen.getByText("Delayed data");
    expect(badge.getAttribute("title")).toMatch(/polling/i);
    expect(screen.queryByText("Live")).not.toBeInTheDocument();
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it("flips to Live once the capability is advertised and the socket opens", async () => {
    stubFetchRealtime();
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");

    // Dialled but not yet established: still honestly delayed.
    expect(FakeSocket.instances).toHaveLength(1);
    expect(screen.getByText("Delayed data")).toBeInTheDocument();

    act(() => {
      FakeSocket.last().emitOpen();
    });
    const badge = screen.getByText("Live");
    expect(badge.getAttribute("title")).toMatch(/pushed/i);
    expect(screen.queryByText("Delayed data")).not.toBeInTheDocument();
  });

  it("renders the no-data cell and the self cell distinctly", async () => {
    stubFetch(matrixBody);
    renderPage();
    expect(await screen.findByLabelText("b → a: no data")).toHaveTextContent("—");
    expect(screen.getByLabelText("a: self")).toBeInTheDocument();
  });

  it("shows a legend that spells out the thresholds", async () => {
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    expect(screen.getByText(/Healthy · fail < 1%/)).toBeInTheDocument();
    expect(screen.getByText(/Degraded · 1–10%/)).toBeInTheDocument();
    expect(screen.getByText(/Failing · ≥ 10%/)).toBeInTheDocument();
    expect(screen.getByText(/No data/)).toBeInTheDocument();
  });

  /* QA scope 2, finding #12 — the green row claims "fail < 1%" while a cell
     with no fail samples is green too. The note has to say on what grounds. */
  it("says what a no-sample cell's green actually rests on", async () => {
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    const note = screen.getByText(/colour = worst of fail/);
    expect(note).toHaveTextContent(/absence of a bad signal/);
    expect(note).toHaveTextContent(/not on a measured zero/);
  });

  it("opens a hover tooltip with the pair's figures", async () => {
    stubFetch(matrixBody);
    renderPage();
    const cell = await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    fireEvent.mouseEnter(cell);
    const tooltip = await screen.findByRole("tooltip");
    expect(tooltip).toHaveTextContent("Failure ratio");
    expect(tooltip).toHaveTextContent("50.0%");
    expect(tooltip).toHaveTextContent("RTT p95");
    fireEvent.mouseLeave(cell);
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
  });

  it("links each non-self cell to its pair card, URL-encoding the node names", async () => {
    stubFetch(matrixBody);
    renderPage();
    const cell = await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    expect(cell.tagName).toBe("A");
    expect(cell).toHaveAttribute("href", "/pairs/a/b");

    const noData = screen.getByLabelText("b → a: no data");
    expect(noData).toHaveAttribute("href", "/pairs/b/a");
  });

  /* QA scope 2, finding #14 — every cell opened a card and the two names
     framing it opened nothing. */
  it("links each row and column header to that node's own card", async () => {
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    const headers = screen.getAllByRole("link", { name: "Open the card for a" });
    // One column header and one row header, both pointing at the same card.
    expect(headers).toHaveLength(2);
    for (const h of headers) expect(h).toHaveAttribute("href", "/nodes/a");
  });

  it("URL-encodes a node name in the header link the same way the cell link does", async () => {
    stubFetch({
      protocol: "tcp",
      plane: "pod",
      nodes: ["ns/pod a"],
      cells: [],
      timestamp: "t",
    });
    renderPage();
    const headers = await screen.findAllByRole("link", { name: "Open the card for ns/pod a" });
    expect(headers[0]).toHaveAttribute("href", `/nodes/${encodeURIComponent("ns/pod a")}`);
  });

  it("URL-encodes node names that need it in the pair link", async () => {
    stubFetch({
      protocol: "tcp",
      plane: "pod",
      nodes: ["ns/pod a", "b"],
      cells: [{ source: "ns/pod a", destination: "b", failRatio: 0.02 }],
      timestamp: "t",
    });
    renderPage();
    // ANCHORED: the same cell now carries a second labelled affordance, "Investigate ns/pod a → b".
    const cell = await screen.findByLabelText(/^ns\/pod a → b/);
    expect(cell).toHaveAttribute("href", `/pairs/${encodeURIComponent("ns/pod a")}/b`);
  });

  it("surfaces problem errors", async () => {
    stubFetch(
      { type: "about:blank", title: "prometheus not configured", status: 503 },
      { status: 503, headers: { "Content-Type": "application/problem+json" } },
    );
    renderPage();
    expect(await screen.findByRole("alert")).toHaveTextContent("prometheus not configured");
  });
});

/* ── M6 Task 8: the Investigate affordance inside the cell ──────────────── */

describe("MatrixPage — Investigate affordance", () => {
  it("gives every non-self cell an Investigate link for that exact pair", async () => {
    stubFetch(matrixBody);
    renderPage();

    const link = await screen.findByLabelText("Investigate a → b");
    const href = link.getAttribute("href") ?? "";
    const p = parseInvestigationParams(href.slice(href.indexOf("?")), new Date());
    expect(p.kind).toBe("pair");
    expect(p.a).toBe("a");
    expect(p.b).toBe("b");
    expect(p.to.getTime() - p.from.getTime()).toBe(60 * 60 * 1000);
  });

  it("keeps the cell itself pointing at the pair card — the affordance is added, not replaced", async () => {
    stubFetch(matrixBody);
    renderPage();

    expect((await screen.findByLabelText(/^a → b:/)).getAttribute("href")).toBe("/pairs/a/b");
    // A cell with no probe data still gets both: an operator investigates a
    // silent pair more often than a noisy one.
    expect(screen.getByLabelText("Investigate b → a")).toBeInTheDocument();
  });

  it("gives the self-cells nothing to investigate", async () => {
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText("Investigate a → b");
    expect(screen.queryByLabelText("Investigate a → a")).toBeNull();
  });

  it("gives the icon a 40px hit area without changing what is drawn (QA scope 3, finding #19)", async () => {
    stubFetch(matrixBody);
    renderPage();

    const link = await screen.findByLabelText("Investigate a → b");
    /* The painted control is a 12px glyph in a 16px box — a target a trackpad
       hits by luck and a touch screen does not hit at all. -inset-3 is 12px on
       each side, i.e. 16 + 24 = 40px of pointer area, and a pseudo-element
       rather than padding so the glyph stays in the cell's corner where the
       affordance is learned. jsdom lays nothing out, so the class is the pin. */
    expect(link.className).toContain("after:-inset-3");
    expect(link.className).toContain("after:absolute");
    // The visual size is untouched: still the small icon, still the tight box.
    expect(link.className).toContain("p-0.5");
    expect(link.querySelector("svg")?.getAttribute("class")).toContain("size-3");
  });
});

/* The grid used to throw all of it away and draw an em-dash. */

const lazyBody = {
  protocol: "udp", plane: "pod", nodes: ["a", "b"],
  cells: [
    // Never failed: latency and loss are there, the failure series is not.
    { source: "a", destination: "b", failRatio: null, rttP95: 2_200_000, lossRatio: 0.05 },
    // Genuinely unprobed: all three vectors silent.
    { source: "b", destination: "a", failRatio: null },
  ],
  timestamp: "2026-01-01T00:00:00Z",
};

describe("MatrixPage — a cell measured without a failure ratio", () => {
  it("renders its RTT instead of an em-dash", async () => {
    stubFetch(lazyBody);
    renderPage();
    const cell = await screen.findByLabelText(/^a → b:/);
    expect(cell).toHaveTextContent("2.2ms");
    expect(cell).not.toHaveTextContent("—");
  });

  it("says in its aria-label what IS known, never 'no data' over the top of data", async () => {
    stubFetch(lazyBody);
    renderPage();
    await screen.findByLabelText("a → b: no failure signal recorded, RTT p95 2.2ms, packet loss 5.0%");
    // The genuinely silent pair keeps the honest "no data".
    expect(screen.getByLabelText("b → a: no data")).toBeInTheDocument();
  });

  it("takes the tier from packet loss when the failure ratio cannot carry it", async () => {
    stubFetch(lazyBody);
    renderPage();
    // loss 5% is the degraded band: a warn fill, not the unknown one the
    // failRatio-only reading painted.
    const cell = await screen.findByLabelText(/^a → b:/);
    expect(cell.className).toContain("bg-health-warn-soft");
    expect(cell.className).not.toContain("unknown");
  });

  it("keeps the figures in the tooltip, marking the failure series as unsampled", async () => {
    stubFetch(lazyBody);
    renderPage();
    fireEvent.mouseEnter(await screen.findByLabelText(/^a → b:/));
    const tooltip = await screen.findByRole("tooltip");
    expect(tooltip).toHaveTextContent("no samples");
    expect(tooltip).toHaveTextContent("2.2ms");
    expect(tooltip).toHaveTextContent("Packet loss");
    expect(tooltip).not.toHaveTextContent("No probe data");
  });
});

/* the cell's second LINE, which is the only prose in the grid A pair with a p95. */

const quietBody = {
  protocol: "tcp", plane: "pod", nodes: ["a", "b"],
  cells: [
    // p95 only: the failure counter never fired and TCP reports no loss ratio.
    { source: "a", destination: "b", failRatio: null, rttP95: 1_800_000 },
    { source: "b", destination: "a", failRatio: null },
  ],
  timestamp: "2026-01-01T00:00:00Z",
};

describe("MatrixPage — the secondary line of a cell with no failure samples", () => {
  it("names the missing half in English under the p95", async () => {
    stubFetch(quietBody);
    renderPage();
    const cell = await screen.findByLabelText(/^a → b:/);
    expect(cell).toHaveTextContent("1.8ms");
    expect(cell).toHaveTextContent("no fail data");
  });
});

/* ── the owner's report: at ten nodes the grid runs off the screen ───────── */

/**
 * The grid used to be as wide as its data made it: a `<th>` holding a
 * twenty-character node name set its whole column, and ten of those was a metre
 * and a half of table on a laptop, with no way to shrink it and no way to pan.
 *
 * The arithmetic is lib/matrix-zoom.ts and has its own unit tests. What this
 * block pins is the WIRING — that the page measures its own box, that "fit" is
 * what an overflowing grid opens at, and that nothing a reader needs (the pair
 * link, the tooltip, the investigate affordance) is traded away for the fit.
 */

/** jsdom lays nothing out, so clientWidth is 0 and any fit derived from it
 *  would be a fabrication. This is the ONE measurement the page takes. */
function stubViewportWidth(px: number) {
  Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, value: px });
}

function unstubViewportWidth() {
  Reflect.deleteProperty(HTMLElement.prototype, "clientWidth");
}

const gridOf = (n: number) => ({
  protocol: "tcp",
  plane: "pod",
  nodes: Array.from({ length: n }, (_, i) => `node-${String(i).padStart(2, "0")}`),
  cells: [{ source: "node-00", destination: "node-01", failRatio: 0.5, rttP95: 2_000_000 }],
  timestamp: "2026-01-01T00:00:00Z",
});

const viewport = () => screen.getByTestId("matrix-viewport");
const varOf = (name: string) => viewport().style.getPropertyValue(name);
const zoomLevel = () => screen.getByTestId("matrix-zoom-level").textContent;

describe("MatrixPage — fitting the grid to the viewport", () => {
  afterEach(unstubViewportWidth);

  it("opens an overflowing grid at the largest step that fits, not at full size", async () => {
    // Ten nodes at full size is 1144px of table; 700px of column is not that.
    stubViewportWidth(700);
    stubFetch(gridOf(10));
    renderPage();

    await screen.findByLabelText(/^node-00 → node-01:/);
    expect(zoomLevel()).toBe("50%");
    expect(varOf("--m-col-w")).toBe("48px");
    expect(varOf("--m-cell-h")).toBe("24px");
  });

  it("leaves a grid that already fits at its own size rather than inflating it", async () => {
    stubViewportWidth(1600);
    stubFetch(gridOf(3));
    renderPage();

    await screen.findByLabelText(/^node-00 → node-01:/);
    expect(zoomLevel()).toBe("100%");
    expect(varOf("--m-col-w")).toBe("96px");
  });

  it("stops shrinking at the floor and hands over to panning, which is the honest answer for a big fleet", async () => {
    stubViewportWidth(700);
    stubFetch(gridOf(50));
    renderPage();

    await screen.findByLabelText(/^node-00 → node-01:/);
    // 40% is the floor; below it a cell is smaller than the smallest legible
    // figure, so the container scrolls from here instead.
    expect(zoomLevel()).toBe("40%");
    expect(viewport().className).toMatch(/overflow-auto/);
  });

  it("says nothing about a scale it has not measured", async () => {
    // No clientWidth stub: an unmeasured box must not produce a guessed fit.
    stubFetch(gridOf(10));
    renderPage();
    await screen.findByLabelText(/^node-00 → node-01:/);
    expect(zoomLevel()).toBe("100%");
  });
});

describe("MatrixPage — the zoom controls", () => {
  afterEach(unstubViewportWidth);

  async function renderFitted(nodes = 10, width = 700) {
    stubViewportWidth(width);
    stubFetch(gridOf(nodes));
    renderPage();
    await screen.findByLabelText(/^node-00 → node-01:/);
  }

  it("steps out and back in one stop at a time", async () => {
    await renderFitted();
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(zoomLevel()).toBe("60%");
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(zoomLevel()).toBe("75%");
    fireEvent.click(screen.getByRole("button", { name: "Zoom out" }));
    expect(zoomLevel()).toBe("60%");
  });

  it("clamps at both ends rather than wrapping, and says so with a dead control", async () => {
    await renderFitted();
    for (let i = 0; i < 8; i++) fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(zoomLevel()).toBe("150%");
    expect(screen.getByRole("button", { name: "Zoom in" })).toBeDisabled();

    for (let i = 0; i < 10; i++) fireEvent.click(screen.getByRole("button", { name: "Zoom out" }));
    expect(zoomLevel()).toBe("40%");
    expect(screen.getByRole("button", { name: "Zoom out" })).toBeDisabled();
  });

  it("returns to the FITTED scale on reset, not to 100%", async () => {
    await renderFitted();
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(zoomLevel()).toBe("75%");

    fireEvent.click(screen.getByRole("button", { name: "Fit to view" }));
    expect(zoomLevel()).toBe("50%");
  });

  it("zooms on ctrl+wheel and leaves a plain wheel to scroll the grid", async () => {
    await renderFitted();
    // A plain wheel is how the reader PANS a grid bigger than its box; taking
    // it for zoom would leave a fitted-at-the-floor grid unpannable.
    fireEvent.wheel(viewport(), { deltaY: -100 });
    expect(zoomLevel()).toBe("50%");

    fireEvent.wheel(viewport(), { deltaY: -100, ctrlKey: true });
    expect(zoomLevel()).toBe("60%");
    fireEvent.wheel(viewport(), { deltaY: 100, ctrlKey: true });
    expect(zoomLevel()).toBe("50%");
  });
});

describe("MatrixPage — what a cell keeps at every scale", () => {
  afterEach(unstubViewportWidth);

  it("keeps the pair link and its whole aria-label at the floor", async () => {
    stubViewportWidth(700);
    stubFetch(gridOf(50));
    renderPage();

    const cell = await screen.findByLabelText("node-00 → node-01: fail 50.0%, RTT p95 2.0ms");
    expect(cell).toHaveAttribute("href", "/pairs/node-00/node-01");
  });

  it("keeps the figures one hover away once the cell is too small to print them", async () => {
    stubViewportWidth(700);
    stubFetch(gridOf(50));
    renderPage();

    const cell = await screen.findByLabelText(/^node-00 → node-01:/);
    // A 38px tile cannot hold "50.0%", and it does not pretend to...
    expect(cell).not.toHaveTextContent("50.0%");
    // ...but the tooltip still carries every figure the cell ever showed.
    fireEvent.mouseEnter(cell);
    const tip = await screen.findByRole("tooltip");
    expect(tip).toHaveTextContent("50.0%");
    expect(tip).toHaveTextContent("2.0ms");
  });

  it("still prints the figures at a scale that can hold them", async () => {
    stubViewportWidth(1600);
    stubFetch(gridOf(3));
    renderPage();

    const cell = await screen.findByLabelText(/^node-00 → node-01:/);
    expect(cell).toHaveTextContent("50.0%");
    expect(cell).toHaveTextContent("2.0ms");
  });

  /**
   * The honest bound on this approach. The grid is DOM, one element per cell
   * plus whatever the cell draws, so a fifty-node fleet is 2 500 cells. Keeping
   * a tile down to the LINK ITSELF is what makes that affordable — and it is
   * also why a hundred nodes (10 000 cells) is out of this markup's reach and
   * wants windowing rather than a smaller font.
   */
  it("draws a tile as the link and nothing else, which is what bounds a big fleet", async () => {
    stubViewportWidth(700);
    stubFetch(gridOf(50));
    const { container } = renderPage();

    await screen.findByLabelText(/^node-00 → node-01:/);
    const cell = container.querySelector('td:not([aria-label])') as HTMLElement;
    expect(cell.querySelectorAll("*")).toHaveLength(1);
    // No investigate glyph at this size: a 12px icon in a 38px tile is a target
    // nothing but luck hits, and it is back as soon as the cell can hold it.
    expect(screen.queryAllByTestId("cell-investigate")).toHaveLength(0);
  });

  it("brings the investigate affordance back at a scale that has room for it", async () => {
    stubViewportWidth(1600);
    stubFetch(gridOf(3));
    const { container } = renderPage();

    await screen.findByLabelText(/^node-00 → node-01:/);
    expect(screen.getAllByTestId("cell-investigate").length).toBeGreaterThan(0);
    /* The other end of the budget, pinned so the honest number stays honest: a
       FULL cell is eight elements (the td plus seven) against a tile's two,
       which is the whole reason a fifty-node grid opens at the floor. */
    const cell = container.querySelector("td:not([aria-label])") as HTMLElement;
    expect(cell.querySelectorAll("*")).toHaveLength(7);
  });
});

describe("MatrixPage — headers stay put while the grid pans", () => {
  afterEach(unstubViewportWidth);

  it("pins the row names to the left edge and the column names to the top", async () => {
    stubViewportWidth(700);
    stubFetch(gridOf(10));
    renderPage();

    await screen.findByLabelText(/^node-00 → node-01:/);
    const corner = screen.getAllByRole("columnheader")[0];
    expect(corner.className).toMatch(/sticky/);
    expect(corner.className).toMatch(/left-0/);
    expect(corner.className).toMatch(/top-0/);
    const rowHeader = screen.getAllByRole("rowheader")[0];
    expect(rowHeader.className).toMatch(/sticky/);
    expect(rowHeader.className).toMatch(/left-0/);
  });
});

describe("MatrixPage — the legend reads as one block", () => {
  it("puts the colour note under the tiers it explains, not against the far edge", async () => {
    stubFetch(matrixBody);
    renderPage();

    await screen.findByLabelText(/^a → b:/);
    const note = screen.getByTestId("matrix-legend-note");
    // `ml-auto` pushed a sentence ABOUT the legend a screen away from it.
    expect(note.className).not.toMatch(/ml-auto/);
    // And it is no longer hidden from the readers most likely to need it.
    expect(note.className).not.toMatch(/hidden/);
    expect(note).toHaveTextContent(/worst of/i);
  });
});

/* ── QA round 2, finding #15: the protocol belongs in the URL ────────────── */

describe("readProtocolFromLocation", () => {
  it("reads a protocol the console probes", () => {
    expect(readProtocolFromLocation("?protocol=icmp")).toBe("icmp");
    expect(readProtocolFromLocation("?protocol=udp")).toBe("udp");
  });

  it("falls back to tcp for anything else, rather than an unanswerable grid", () => {
    expect(readProtocolFromLocation("?protocol=sctp")).toBe("tcp");
    expect(readProtocolFromLocation("")).toBe("tcp");
    expect(readProtocolFromLocation("?protocol=")).toBe("tcp");
  });
});

describe("MatrixPage — protocol in the URL", () => {
  afterEach(() => window.history.replaceState({}, "", "/"));

  it("opens on the protocol the link named", async () => {
    window.history.replaceState({}, "", "/matrix?protocol=icmp");
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText(/^a → b:/);
    expect(screen.getByRole("radio", { name: "ICMP" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "TCP" })).not.toBeChecked();
  });

  it("REPLACES the protocol in the URL on a switch, leaving one history entry", async () => {
    window.history.replaceState({}, "", "/matrix");
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText(/^a → b:/);
    const before = window.history.length;
    fireEvent.click(screen.getByRole("radio", { name: "UDP" }));
    expect(new URLSearchParams(window.location.search).get("protocol")).toBe("udp");
    expect(window.history.length).toBe(before);
  });
});

/* ── QA scope 2, finding #17: a degraded param must not stay in the URL ──── */

describe("degradedProtocolParam", () => {
  it("names the protocol the page actually shows when the URL asked for another", () => {
    expect(degradedProtocolParam("?protocol=sctp")).toBe("tcp");
    expect(degradedProtocolParam("?protocol=")).toBe("tcp");
    expect(degradedProtocolParam("?protocol=TCP")).toBe("tcp");
  });

  it("stays quiet when the URL and the view already agree", () => {
    expect(degradedProtocolParam("?protocol=icmp")).toBeNull();
    expect(degradedProtocolParam("?protocol=tcp")).toBeNull();
  });

  it("stays quiet with no param at all — the default needs no spelling out", () => {
    expect(degradedProtocolParam("")).toBeNull();
    expect(degradedProtocolParam("?at=2026-08-08T12:00:00Z")).toBeNull();
  });
});

describe("MatrixPage — a protocol the console cannot probe", () => {
  afterEach(() => window.history.replaceState({}, "", "/"));

  it("rewrites the URL to the protocol it degraded to, without a new history entry", async () => {
    window.history.replaceState({}, "", "/matrix?protocol=sctp");
    stubFetch(matrixBody);
    const before = window.history.length;
    renderPage();
    await screen.findByLabelText(/^a → b:/);
    expect(new URLSearchParams(window.location.search).get("protocol")).toBe("tcp");
    expect(window.history.length).toBe(before);
    expect(screen.getByRole("radio", { name: "TCP" })).toBeChecked();
  });

  it("leaves an honest URL alone", async () => {
    window.history.replaceState({}, "", "/matrix?protocol=udp");
    stubFetch(matrixBody);
    renderPage();
    await screen.findByLabelText(/^a → b:/);
    expect(window.location.search).toBe("?protocol=udp");
  });
});

/* ru The only case in this file with a LocaleProvider. */

describe("MatrixPage — ru", () => {
  afterEach(() => {
    /* vitest.setup.ts backs localStorage with ONE Map per test FILE. */
    localStorage.removeItem(LOCALE_STORAGE_KEY);
  });

  it("renders the grid chrome and the colour caveat in Russian", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    stubFetch(matrixBody);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <MatrixPage />
        </LocaleProvider>
      </QueryClientProvider>,
    );

    // The grid is what the awaited element proves has landed.
    expect(await screen.findByText("откуда \\ куда")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Матрица", level: 1 })).toBeInTheDocument();
    const note = screen.getByText(/^цвет = худшее из доли сбоев/);
    expect(note).toHaveTextContent("ячейка без выборок сбоев показывает свой p95");
    // The grounds for that green, in Russian: absence of a bad signal, not a
    // measured zero (QA scope 2, finding #12).
    expect(note).toHaveTextContent("плохого сигнала нет, а не потому, что измерен ноль");
  });

  /* cellSummary is the SHARED reading of a cell — the grid's aria-label after the "src → dst: " part. */
  it("speaks a cell's whole aria-label in Russian, leaving the names and figures alone", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    stubFetch(matrixBody);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <MatrixPage />
        </LocaleProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByLabelText("a → b: сбой 50.0%, RTT p95 2.0ms")).toBeInTheDocument();
    // The reverse leg measured nothing at all — the ONE case allowed to claim
    // absence (lib/matrix-cells.ts's isMeasured).
    expect(screen.getByLabelText("b → a: нет данных")).toBeInTheDocument();
    expect(screen.queryByLabelText(/no data/)).toBeNull();

    // The transport badge in the same header, from dict/realtime.ts.
    expect(screen.getByText("Данные с задержкой")).toBeInTheDocument();
  });

  /*
   * The phrase had to get SHORTER without getting less true, so this pins both halves of that
   * bargain — the words that fit.
   */
  it("scopes the cell's second line to сбои instead of opening with «нет данных»", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    stubFetch(quietBody);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <MatrixPage />
        </LocaleProvider>
      </QueryClientProvider>,
    );

    const cell = await screen.findByLabelText(/^a → b:/);
    expect(cell).toHaveTextContent("1.8ms");
    expect(cell).toHaveTextContent("сбои: н/д");
    // Scoped, so the reserved absence phrase cannot lead the line — and the
    // console still never turns a silent counter into a measured zero.
    expect(cell).not.toHaveTextContent("нет данных");
    expect(cell).not.toHaveTextContent("нет сбоев");

    // The unabbreviated sentence is still what the cell is CALLED: the visible
    // line and the aria-label may differ in length, never in meaning.
    expect(
      screen.getByLabelText("a → b: данных о сбоях не записано, RTT p95 1.8ms"),
    ).toBeInTheDocument();
    // And the pair nothing measured keeps «нет данных» to itself.
    expect(screen.getByLabelText("b → a: нет данных")).toBeInTheDocument();
  });
});

/* ── a payload the console cannot stand behind ──────────────────────────────
 *
 * QA scope 4 ("прокликать всякое, вписать строку где должны быть цифры"). The
 * grid used to render whatever arrived in a numeric field, and three shapes that
 * need no hostile server got through:
 *
 *   - `1e999` is legal JSON and JSON.parse hands back Infinity, which
 *     fmtRatio/fmtRtt printed as "Infinity%" / "Infinityms";
 *   - anything non-numeric printed as "NaN%" / "NaNms";
 *   - a bare `null` in `rttP95` was the worst of the three, because
 *     lib/matrix-cells.ts counts the FIELD as present and 0/1e6 formats as a
 *     confident "0.0ms" — a measurement nobody took, in a cell the tier then
 *     painted green.
 *
 * Each of the three now lands on the one honest reading there already was for a
 * fact nobody measured. Note what is NOT rejected: a loss ratio above 1 is a
 * finite number the server actually reported, and dropping it would turn a red
 * cell green — the console errs toward the alarm, not away from it.
 */

/** A body with the literal bytes in it: JSON.stringify would turn Infinity back
 *  into null, which is a different attack, so the wire text is written out. */
const rawBody = (text: string) =>
  new Response(text, { status: 200, headers: { "Content-Type": "application/json" } });

function stubFetchRaw(text: string) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) =>
      Promise.resolve(String(url).includes("/api/v1/version") ? json({ version: "1" }) : rawBody(text)),
    ),
  );
}

const wire = (cells: string) =>
  `{"protocol":"tcp","plane":"pod","nodes":["a","b"],"cells":[${cells}],"timestamp":"t"}`;

describe("MatrixPage — a payload the console cannot stand behind", () => {
  it("refuses an Infinity a JSON 1e999 parsed into, rather than printing it into a cell", async () => {
    stubFetchRaw(wire('{"source":"a","destination":"b","failRatio":1e999,"rttP95":1e999,"lossRatio":1e999}'));
    renderPage();

    expect(await screen.findByLabelText("a → b: no data")).toHaveTextContent("—");
    expect(screen.getByRole("table").textContent).not.toMatch(/Infinity/);
  });

  it("prints nothing for a figure that arrived as a string", async () => {
    stubFetchRaw(wire('{"source":"a","destination":"b","failRatio":"0.5","rttP95":"abc","lossRatio":"x"}'));
    renderPage();

    expect(await screen.findByLabelText("a → b: no data")).toBeInTheDocument();
    expect(screen.getByRole("table").textContent).not.toMatch(/NaN/);
  });

  /* The one that fabricated a measurement rather than mangling one: `null`
     divided by 1e6 is 0, and "0.0ms" reads as a fast, healthy pair. */
  it("reads a null rttP95 as a measurement nobody took, not as 0.0ms", async () => {
    stubFetch({
      ...matrixBody,
      cells: [{ source: "a", destination: "b", failRatio: null, rttP95: null, lossRatio: null }],
    });
    renderPage();

    expect(await screen.findByLabelText("a → b: no data")).toBeInTheDocument();
    expect(screen.getByRole("table").textContent).not.toMatch(/0\.0ms|0\.0%/);
  });

  it("keeps a ratio or a duration that runs backwards out of the grid", async () => {
    stubFetch({
      ...matrixBody,
      cells: [{ source: "a", destination: "b", failRatio: -0.5, rttP95: -5_000_000 }],
    });
    renderPage();

    expect(await screen.findByLabelText("a → b: no data")).toBeInTheDocument();
    expect(screen.getByRole("table").textContent).not.toMatch(/-5\.0ms|-50\.0%/);
  });

  /* A loss ratio over 100% is nonsense, but it is the server's OWN nonsense and
     it lands in the failing tier; hiding it would hide a red cell. */
  it("still reports an over-range loss ratio, which is a claim rather than a corruption", async () => {
    stubFetch({ ...matrixBody, cells: [{ source: "a", destination: "b", failRatio: null, lossRatio: 1.5 }] });
    renderPage();

    expect(await screen.findByLabelText("a → b: no failure signal recorded, packet loss 150.0%")).toBeInTheDocument();
  });

  it("steps over a null in the cell list instead of taking the whole grid down with it", async () => {
    stubFetch({
      ...matrixBody,
      cells: [null, { failRatio: 0.5 }, { source: "a", destination: "b", failRatio: 0.2 }],
    });
    renderPage();

    // The page renders AND the one addressable cell in that list still lands.
    expect(await screen.findByLabelText("a → b: fail 20.0%")).toBeInTheDocument();
  });

  it("draws one column per NAME when the payload repeats one, without colliding React keys", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    stubFetch({ ...matrixBody, nodes: ["a", "a", "b", ""], cells: [] });
    renderPage();

    await screen.findByRole("table");
    expect(screen.getAllByRole("columnheader")).toHaveLength(3); // corner + a + b
    expect(errors.mock.calls.map((c) => String(c[0])).join("")).not.toMatch(/same key/);
    errors.mockRestore();
  });

  it("renders a single-node fleet, which has an axis and no pair at all", async () => {
    stubFetch({ ...matrixBody, nodes: ["solo"], cells: [] });
    renderPage();

    expect(await screen.findByLabelText("solo: self")).toBeInTheDocument();
    expect(screen.queryByTestId("cell-investigate")).not.toBeInTheDocument();
  });
});

/* ── the pushed snapshot, which does not pass through lib/api.ts ────────────
 *
 * `GET /api/v1/matrix` is normalized in lib/api.ts — a Go nil slice marshals to
 * `null`, so `nodes` and `cells` are defaulted to `[]` there. The WEBSOCKET
 * frame is written straight into the same cache entry by hooks/use-matrix.ts and
 * never sees that normalizer, so the two transports could disagree about the
 * shape of the very same matrix. One pushed `{"nodes":null}` and the page went
 * blank mid-session, with no way back but a reload.
 */
describe("MatrixPage — a pushed snapshot that disagrees with the REST shape", () => {
  /** The socket is a snapshot topic and drops a frame it has already seen, so
   *  the sequence has to move. */
  let seq = 0;
  const push = (data: unknown) => {
    seq += 1;
    act(() => {
      FakeSocket.last().emitEnvelope({ topic: "matrix:tcp:pod", type: "snapshot", seq, data });
    });
  };

  async function connected() {
    stubFetchRealtime();
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    act(() => FakeSocket.last().emitOpen());
  }

  it("lands an ordinary pushed snapshot, which is what the two below are measured against", async () => {
    await connected();
    push({ protocol: "tcp", plane: "pod", nodes: ["z1", "z2"], cells: [], timestamp: "t" });
    expect(screen.getAllByLabelText("Open the card for z1")).toHaveLength(2); // one column, one row
  });

  it("degrades a pushed nil slice to the empty state instead of a blank page", async () => {
    await connected();
    push({ protocol: "tcp", plane: "pod", nodes: null, cells: null, timestamp: "t" });

    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.getByText("No probe data in Prometheus yet")).toBeInTheDocument();
  });

  it("survives a pushed nodes field that is not a list at all", async () => {
    await connected();
    push({ protocol: "tcp", plane: "pod", nodes: "a,b", cells: [], timestamp: "t" });

    expect(screen.getByText("No probe data in Prometheus yet")).toBeInTheDocument();
  });
});

/* ── the wheel storm ────────────────────────────────────────────────────────
 *
 * A trackpad emits a wheel event every few pixels, so one flick arrives as a
 * burst inside a single React batch. Each event used to read the scale off a ref
 * that only updates on RENDER, so the whole burst read the same pre-batch value
 * and collapsed into one stop: the grid felt stuck under exactly the gesture
 * meant to drive it.
 */
describe("MatrixPage — a zoom gesture that arrives all at once", () => {
  afterEach(unstubViewportWidth);

  /** fireEvent flushes between events; a real burst does not, so the burst is
   *  dispatched natively inside ONE act(). */
  const burst = (el: HTMLElement, n: number, deltaY: number) =>
    act(() => {
      for (let i = 0; i < n; i++) {
        el.dispatchEvent(new WheelEvent("wheel", { deltaY, ctrlKey: true, cancelable: true, bubbles: true }));
      }
    });

  async function renderFitted() {
    stubViewportWidth(700);
    stubFetch(gridOf(10));
    renderPage();
    await screen.findByLabelText(/^node-00 → node-01:/);
  }

  it("counts every event in a burst, not just the one that opened it", async () => {
    await renderFitted();
    expect(zoomLevel()).toBe("50%");
    // 50 → 60 → 75 → 90 → 100 → 125, five stops for five events.
    burst(viewport(), 5, -100);
    expect(zoomLevel()).toBe("125%");
  });

  it("clamps a storm at both ends rather than running off the scale", async () => {
    await renderFitted();
    burst(viewport(), 40, -100);
    expect(zoomLevel()).toBe("150%");
    expect(varOf("--m-col-w")).toBe("144px");

    burst(viewport(), 40, 100);
    expect(zoomLevel()).toBe("40%");
    // The floor is a real geometry, not a NaN or a zero-width column.
    expect(varOf("--m-col-w")).toBe("38px");
    expect(varOf("--m-cell-h")).toBe("19px");
  });
});

describe("MatrixPage — a payload the console cannot stand behind, ru", () => {
  afterEach(() => localStorage.removeItem(LOCALE_STORAGE_KEY));

  it("says «нет данных» about a figure it refused, in the reader's own language", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    stubFetchRaw(wire('{"source":"a","destination":"b","failRatio":1e999,"rttP95":"nope"}'));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <MatrixPage />
        </LocaleProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByLabelText("a → b: нет данных")).toBeInTheDocument();
    expect(screen.getByRole("table").textContent).not.toMatch(/NaN|Infinity/);
  });
});

describe("MatrixPage — node names the grid did not choose", () => {
  /* Node names are DNS labels today. The grid does not depend on that: the
     column is a fixed width (lib/matrix-zoom.ts) and every name it puts in a
     path is encoded, so a name with dots, a name in another script and a name
     far longer than the column all stay inside the box and address the right
     card. */
  const NAMES = ["node.with.dots", "узел-Кириллица", "w".repeat(120)];

  beforeEach(() => {
    stubFetch({
      ...matrixBody,
      nodes: NAMES,
      cells: [{ source: NAMES[0], destination: NAMES[1], failRatio: 0.5 }],
    });
  });

  it("sends every header and every pair link to the encoded name", async () => {
    renderPage();
    await screen.findByRole("table");

    for (const name of NAMES) {
      expect(screen.getAllByLabelText(`Open the card for ${name}`)[0]).toHaveAttribute(
        "href",
        `/nodes/${encodeURIComponent(name)}`,
      );
    }
    expect(screen.getByLabelText(`${NAMES[0]} → ${NAMES[1]}: fail 50.0%`)).toHaveAttribute(
      "href",
      `/pairs/${encodeURIComponent(NAMES[0])}/${encodeURIComponent(NAMES[1])}`,
    );
  });

  it("keeps a name longer than its column inside the column, whole in the tooltip", async () => {
    renderPage();
    await screen.findByRole("table");

    const label = screen.getAllByLabelText(`Open the card for ${NAMES[2]}`)[0];
    // Truncation is what keeps one long name from setting the table's width —
    // the report this whole zoom exists for.
    expect(label.className).toMatch(/truncate/);
    expect(label.className).toMatch(/max-w-\[var\(--m-(col|label)-w\)\]/);
    expect(label).toHaveTextContent(NAMES[2]);
  });
});

/* ── QA round 4, finding #21: every column header read the same string ──────
   The prefix rule's own unit tests moved with it to lib/matrix-zoom.test.ts;
   what stays here is the call site — the rendered headers. */

describe("MatrixPage — column headers stay distinguishable", () => {
  it("drops the shared prefix and names it once, keeping the whole name in the tooltip", async () => {
    stubFetch({
      ...matrixBody,
      nodes: ["kconmon-prod.node-01", "kconmon-prod.node-02"],
      cells: [{ source: "kconmon-prod.node-01", destination: "kconmon-prod.node-02", failRatio: 0 }],
    });
    renderPage();

    await screen.findByLabelText(/^kconmon-prod\.node-01 → kconmon-prod\.node-02:/);
    const columns = screen.getAllByRole("columnheader").slice(1);
    const shown = columns.map((th) => th.textContent);
    // The point of the fix: the two headers no longer render the identical string.
    expect(new Set(shown).size).toBe(columns.length);
    expect(shown).toEqual(["…01", "…02"]);
    // And the whole name is still the accessible name, so nothing is lost to a screen reader.
    expect(columns[0].querySelector("a")).toHaveAttribute(
      "aria-label",
      expect.stringContaining("kconmon-prod.node-01"),
    );
    // The prefix is named once, above the grid, so nothing is left to guess at.
    expect(screen.getByText(/drop the shared prefix kconmon-prod\.node-/)).toBeInTheDocument();
  });
});
