import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { FakeSocket } from "@/lib/fake-websocket";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { Topology } from "@/lib/types";
import { TopologyPage, unfoldableEmpty } from "./topology";

/**
 * The Time Machine half of /topology. Every case here renders the EMPTY node
 * set, which is also why the page can be rendered at all in jsdom: React Flow
 * needs ResizeObserver and only mounts when there are nodes to draw (see
 * topology.test.tsx's own note about this).
 */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function historical(over: Partial<Topology> = {}): Topology {
  return {
    nodes: [],
    agents: [],
    timestamp: AT,
    historical: true,
    asOf: AT,
    eventsFolded: 0,
    unfoldableEvents: 0,
    truncated: false,
    ...over,
  };
}

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
        <TimeMachineProvider>
          <TopologyPage />
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  window.history.pushState({}, "", `/topology?at=${AT}`);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("unfoldableEmpty", () => {
  it("is null for a live body, which carries none of these fields", () => {
    expect(unfoldableEmpty({ nodes: [], agents: [], timestamp: AT })).toBeNull();
  });

  it("is null when the fold produced nodes, however much it also skipped", () => {
    const topo = historical({ nodes: [{ name: "a", zone: "z", ready: true }], unfoldableEvents: 900 });
    expect(unfoldableEmpty(topo)).toBeNull();
  });

  it("is null for an empty answer with nothing skipped — that is just a quiet past", () => {
    expect(unfoldableEmpty(historical({ unfoldableEvents: 0 }))).toBeNull();
  });

  it("reports both counts when events were seen and none could be folded", () => {
    expect(unfoldableEmpty(historical({ unfoldableEvents: 417, eventsFolded: 3 }))).toEqual({
      unfoldableEvents: 417,
      eventsFolded: 3,
    });
  });

  it("is null for an undefined response — nothing has been answered yet", () => {
    expect(unfoldableEmpty(undefined)).toBeNull();
  });
});

describe("TopologyPage engaged at t", () => {
  it("explains an empty map out of the response's OWN numbers", async () => {
    renderPage(historical({ unfoldableEvents: 417, eventsFolded: 3 }));
    await screen.findByText("Nothing to reconstruct at this time");
    expect(screen.getByText(/417 topology events/)).toBeInTheDocument();
    expect(screen.getByText(/fold 3 of them/)).toBeInTheDocument();
    // Not the live "controller has not reported anything yet" copy, which
    // would send the reader to check a DaemonSet that is running fine.
    expect(screen.queryByText("No nodes reported by the controller yet")).not.toBeInTheDocument();
  });

  it("says an empty-with-nothing-skipped answer is an empty PAST, not a broken fold", async () => {
    renderPage(historical({ unfoldableEvents: 0, eventsFolded: 0 }));
    await screen.findByText("No nodes existed at this time");
    expect(screen.queryByText("Nothing to reconstruct at this time")).not.toBeInTheDocument();
  });

  it("dates the page from the server's asOf rather than from the browser clock", async () => {
    renderPage(historical());
    await screen.findByText("No nodes existed at this time");
    expect(screen.getByText(new RegExp(new Date(AT).toLocaleString().replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeInTheDocument();
  });

  it("admits a truncated fold instead of passing a partial map off as complete", async () => {
    renderPage(historical({ truncated: true, unfoldableEvents: 0 }));
    await screen.findByText("This reconstruction is incomplete");
  });
});

describe("TopologyPage while live", () => {
  it("keeps the original empty-state copy and shows no historical notes", async () => {
    window.history.pushState({}, "", "/topology");
    renderPage({ nodes: [], agents: [], timestamp: AT });
    await screen.findByText("No nodes reported by the controller yet");
    expect(screen.queryByText("Nothing to reconstruct at this time")).not.toBeInTheDocument();
    expect(screen.queryByText("This reconstruction is incomplete")).not.toBeInTheDocument();
  });
});
