import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { MatrixPage } from "./matrix";

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

// A Response body can only be read once, and MatrixPage now issues two requests
// (/api/v1/matrix plus the /api/v1/version capability probe useMatrix feature-
// detects realtime with), so the stub has to mint a fresh Response per call
// rather than resolve the same instance twice. Same convention as
// overview.test.tsx.
const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
    ...init,
  });

/**
 * The default stub answers /api/v1/version with the matrix body, which has no
 * `capabilities` field — realtime reads false, useWsTopic stays disabled and no
 * socket is constructed. That is the M1 fallback path, and it is what every
 * test here but the last one exercises.
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

  it("surfaces problem errors", async () => {
    stubFetch(
      { type: "about:blank", title: "prometheus not configured", status: 503 },
      { status: 503, headers: { "Content-Type": "application/problem+json" } },
    );
    renderPage();
    expect(await screen.findByRole("alert")).toHaveTextContent("prometheus not configured");
  });
});
