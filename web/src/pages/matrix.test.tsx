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
