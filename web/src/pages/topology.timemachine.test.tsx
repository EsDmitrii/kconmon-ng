import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { FakeSocket } from "@/lib/fake-websocket";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { Topology } from "@/lib/types";
import { TopologyPage, unfoldableEmpty } from "./topology";

/** The Time Machine half of /topology; every case here renders the EMPTY node set. */

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

  // The controller attributes its topology events now, so the only history that folds to nothing is
  // what an older one wrote.
  it("blames the age of the events, not the controller that is running", async () => {
    renderPage(historical({ unfoldableEvents: 417, eventsFolded: 3 }));
    await screen.findByText("Nothing to reconstruct at this time");
    expect(screen.getByText(/recorded before the controller started attributing/)).toBeInTheDocument();
    expect(screen.getByText(/pick a more recent instant/i)).toBeInTheDocument();
    expect(screen.queryByText(/after the controller starts attributing/)).not.toBeInTheDocument();
  });

  it("says an empty-with-nothing-skipped answer is an empty PAST, not a broken fold", async () => {
    renderPage(historical({ unfoldableEvents: 0, eventsFolded: 0 }));
    await screen.findByText("No nodes existed at this time");
    expect(screen.queryByText("Nothing to reconstruct at this time")).not.toBeInTheDocument();
  });

  it("dates the page from the server's asOf rather than from the browser clock", async () => {
    renderPage(historical());
    await screen.findByText("No nodes existed at this time");
    /* The page's own DESCRIPTION, not just any stamp on screen: the Time
       Machine trigger in the header names the same instant, and it is a
       different claim (where you are looking FROM, vs what the server folded to). */
    const stamped = new RegExp(new Date(AT).toLocaleString().replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
    expect(screen.getByText(stamped, { selector: "p" })).toBeInTheDocument();
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

/* ── QA round 2, finding #2: a failed topology query is not a blank page ──── */

function renderFailure(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.startsWith("/api/v1/topology")) {
        return Promise.resolve(
          new Response(JSON.stringify(body), {
            status,
            headers: { "Content-Type": "application/problem+json" },
          }),
        );
      }
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

describe("TopologyPage when the topology query fails", () => {
  it("renders the 422's own detail verbatim — the retention sentence is the actionable one", async () => {
    renderFailure(422, {
      type: "about:blank",
      title: "instant outside retention",
      status: 422,
      detail: "at is older than console.database.retentionDays (30d)",
    });
    const problem = await screen.findByTestId("topology-problem");
    expect(problem).toHaveTextContent("console.database.retentionDays");
    expect(screen.getByRole("alert")).toHaveTextContent("Topology is unavailable");
  });

  it("falls back to the problem's title when it carries no detail", async () => {
    renderFailure(500, { type: "about:blank", title: "topology fold failed", status: 500 });
    expect(await screen.findByTestId("topology-problem")).toHaveTextContent("topology fold failed");
  });

  it("still describes itself as a view of t — a failed fold does not make the page Live", async () => {
    renderFailure(422, { type: "about:blank", title: "nope", status: 422, detail: "too old" });
    await screen.findByTestId("topology-problem");
    expect(screen.getByText(/Zone\/node map as of/)).toBeInTheDocument();
    expect(screen.queryByText(/^Live zone\/node map/)).not.toBeInTheDocument();
  });
});

/* ── QA scope 2, findings #1 and #2: agents without a node view ──────────── */

describe("TopologyPage with agents but no nodes", () => {
  it("DRAWS the map from the agents instead of showing an empty state", async () => {
    window.history.pushState({}, "", "/topology");
    renderPage({
      nodes: [],
      agents: [
        { id: "a1", nodeName: "n1", podIP: "10.0.0.1", zone: "z1" },
        { id: "a2", nodeName: "n2", podIP: "10.0.0.2", zone: "z1" },
      ],
      timestamp: AT,
    });
    // The edge caption only renders alongside a drawn map.
    expect(await screen.findByTestId("edge-caption")).toBeInTheDocument();
    expect(screen.queryByText("No nodes reported by the controller yet")).not.toBeInTheDocument();
    // And the sentence that claimed lanes were undrawable is gone with it.
    expect(screen.queryByText(/zone lanes cannot be drawn/)).not.toBeInTheDocument();
  });

  it("says where the boxes came from, and that readiness is the field they lack", async () => {
    window.history.pushState({}, "", "/topology");
    renderPage({
      nodes: [],
      agents: [
        { id: "a1", nodeName: "n1", podIP: "10.0.0.1", zone: "z1" },
        { id: "a2", nodeName: "n2", podIP: "10.0.0.2", zone: "z1" },
      ],
      timestamp: AT,
    });
    const note = await screen.findByTestId("topology-from-agents");
    expect(note).toHaveTextContent("2 registered agents");
    expect(note).toHaveTextContent(/readiness is a Kubernetes node condition/);
  });

  /* QA scope 2, finding #13 — React Flow's own trio ships English aria/title
     and takes no per-button override, so the map renders our own. */
  it("names the map's own controls out of this page's dictionary", async () => {
    window.history.pushState({}, "", "/topology");
    renderPage({
      nodes: [],
      agents: [{ id: "a1", nodeName: "n1", podIP: "10.0.0.1", zone: "z1" }],
      timestamp: AT,
    });
    await screen.findByTestId("edge-caption");
    expect(screen.getByRole("button", { name: "Zoom in" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Zoom out" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Fit the whole map" })).toBeInTheDocument();
    // The library's own fourth button (and its baked-in label) is switched off.
    expect(screen.queryByRole("button", { name: /toggle interactivity/i })).not.toBeInTheDocument();
  });

  it("keeps the DaemonSet copy for the case that IS the DaemonSet's — nothing on either side", async () => {
    window.history.pushState({}, "", "/topology");
    renderPage({ nodes: [], agents: [], timestamp: AT });
    await screen.findByText("No nodes reported by the controller yet");
    expect(screen.getByText(/check that the DaemonSet is running/)).toBeInTheDocument();
  });
});
