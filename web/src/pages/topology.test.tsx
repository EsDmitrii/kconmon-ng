import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { TopologyPage, buildFlow, mapNodes, nodeNavigationPath, unfoldableEmpty } from "./topology";
import type { Matrix, Topology } from "@/lib/types";

const topo: Topology = {
  nodes: [
    { name: "n1", zone: "z1", ready: true },
    { name: "n2", zone: "z2", ready: false },
  ],
  agents: [],
  timestamp: "t",
};
const matrix: Matrix = {
  protocol: "tcp", plane: "pod", nodes: ["n1", "n2"],
  cells: [{ source: "n1", destination: "n2", failRatio: 0.2 }],
  timestamp: "t",
};

describe("buildFlow", () => {
  it("creates one zone container per zone plus one node per cluster node", () => {
    const { nodes } = buildFlow(topo, matrix);
    const zones = nodes.filter((n) => n.type === "zone");
    expect(zones).toHaveLength(2);
    expect(zones[0].data).toMatchObject({ label: "z1", count: 1 });
    expect(nodes.filter((n) => n.parentId)).toHaveLength(2);
    expect(nodes.filter((n) => n.parentId).every((n) => n.type === "topoNode")).toBe(true);
  });

  it("colors not-ready and high-fail nodes as failing, and marks readiness in data", () => {
    const { nodes } = buildFlow(topo, matrix);
    expect(nodes.find((n) => n.id === "n1")?.className).toContain("failing"); // fail 0.2 >= 0.1
    expect(nodes.find((n) => n.id === "n2")?.className).toContain("failing"); // not ready
    expect(nodes.find((n) => n.id === "n2")?.data).toMatchObject({ ready: false });
  });

  it("draws edges only for problem pairs, without permanent labels", () => {
    const { edges, problemTotal } = buildFlow(topo, matrix);
    expect(edges).toHaveLength(1);
    expect(edges[0].id).toBe("n1->n2");
    expect(edges[0].label).toBeUndefined(); // percentage appears on hover only
    expect(edges[0].data).toMatchObject({ failLabel: "20%" });
    expect(problemTotal).toBe(1);
  });

  it("caps at the WORST paths, not the first encountered, and reports the total", () => {
    const names = Array.from({ length: 16 }, (_, i) => `m${i}`);
    const bigTopo: Topology = {
      nodes: names.map((name) => ({ name, zone: "z", ready: true })),
      agents: [],
      timestamp: "t",
    };
    // 15 problem cells with ascending failRatio 1%..15% — the worst one is LAST
    // in the input, so a naive first-N slice would drop it.
    const cells = Array.from({ length: 15 }, (_, i) => ({
      source: names[i],
      destination: names[i + 1],
      failRatio: (i + 1) / 100,
    }));
    const bigMatrix: Matrix = { protocol: "tcp", plane: "pod", nodes: names, cells, timestamp: "t" };

    const { edges, problemTotal } = buildFlow(bigTopo, bigMatrix);
    expect(problemTotal).toBe(15);
    expect(edges).toHaveLength(10);
    // Worst first, and the strongest failure (15%) survived the cap.
    expect(edges[0].data).toMatchObject({ failLabel: "15%" });
    const kept = edges.map((e) => (e.data as { failLabel: string }).failLabel);
    expect(kept).not.toContain("5%"); // the 5 weakest (1–5%) were dropped
    expect(kept).toContain("6%");
  });
});

describe("nodeNavigationPath", () => {
  it("builds /nodes/<name> for a real node, URL-encoding the name", () => {
    expect(nodeNavigationPath({ id: "node-a", type: "topoNode" })).toBe("/nodes/node-a");
  });

  it("round-trips a node name that needs URL encoding", () => {
    const name = "weird node/name äöü";
    expect(nodeNavigationPath({ id: name, type: "topoNode" })).toBe(`/nodes/${encodeURIComponent(name)}`);
    expect(decodeURIComponent(nodeNavigationPath({ id: name, type: "topoNode" })!.slice("/nodes/".length))).toBe(name);
  });

  it("ignores a zone container click", () => {
    expect(nodeNavigationPath({ id: "zone:z1", type: "zone" })).toBeUndefined();
  });

  it("carries the engaged instant to the node card", () => {
    const at = new Date("2026-08-01T12:00:00Z");
    expect(nodeNavigationPath({ id: "node-a", type: "topoNode" }, at)).toBe(
      `/nodes/node-a?at=${encodeURIComponent("2026-08-01T12:00:00Z")}`,
    );
  });

  it("adds nothing while Live", () => {
    expect(nodeNavigationPath({ id: "node-a", type: "topoNode" }, null)).toBe("/nodes/node-a");
  });
});

/* ── QA round 2, findings #1, #9 and #22 ─────────────────────────────────── */

describe("buildFlow — a pair measured without a failure ratio", () => {
  // Loss 5%, and the fail counter has never fired: exactly the state that used
  // to make the pair invisible on this map.
  const lossy: Matrix = {
    protocol: "udp", plane: "pod", nodes: ["n1", "n2"],
    cells: [{ source: "n1", destination: "n2", failRatio: null, rttP95: 2e6, lossRatio: 0.05 }],
    timestamp: "t",
  };

  it("draws the problem edge from packet loss alone", () => {
    const { edges, problemTotal } = buildFlow(topo, lossy);
    expect(problemTotal).toBe(1);
    expect(edges[0].id).toBe("n1->n2");
    expect(edges[0].className).toBe("topo-edge--degraded");
  });

  it("names the vector the percentage came from", () => {
    const { edges } = buildFlow(topo, lossy);
    expect(edges[0].data).toMatchObject({ failLabel: "5% loss" });
  });

  it("colours the source node from the loss it cannot see in the fail series", () => {
    const { nodes } = buildFlow(topo, lossy);
    expect(nodes.find((n) => n.id === "n1")?.className).toContain("degraded");
  });
});

describe("buildFlow — back edges", () => {
  const mutual: Matrix = {
    protocol: "tcp", plane: "pod", nodes: ["n1", "n2"],
    cells: [
      { source: "n1", destination: "n2", failRatio: 0.2 },
      { source: "n2", destination: "n1", failRatio: 0.15 },
    ],
    timestamp: "t",
  };

  it("gives an opposite pair symmetric, direction-keyed routing", () => {
    const { edges } = buildFlow(topo, mutual);
    const forward = edges.find((e) => e.id === "n1->n2");
    const back = edges.find((e) => e.id === "n2->n1");
    expect(forward).toMatchObject({ pathOptions: { stepPosition: 0.35, offset: 16 } });
    expect(back).toMatchObject({ pathOptions: { stepPosition: 0.65, offset: 44 } });
  });

  it("leaves a lone edge on the default routing — nothing to bow around", () => {
    const { edges } = buildFlow(topo, matrix);
    expect(edges[0]).not.toHaveProperty("pathOptions");
  });
});

describe("buildFlow — node accessibility", () => {
  it("labels every node with who, where and how", () => {
    const { nodes } = buildFlow(topo, matrix);
    // n1's worst outbound path is 20%, so it reads failing; n2 is not ready.
    expect(nodes.find((n) => n.id === "n1")?.ariaLabel).toBe("n1, zone z1, failing");
    expect(nodes.find((n) => n.id === "n2")?.ariaLabel).toBe("n2, zone z2, failing, not ready");
  });

  it("says healthy in words rather than in the class name's vocabulary", () => {
    const quiet: Matrix = { protocol: "tcp", plane: "pod", nodes: ["n1"], cells: [], timestamp: "t" };
    const oneReady: Topology = { nodes: [{ name: "n1", zone: "z1", ready: true }], agents: [], timestamp: "t" };
    expect(buildFlow(oneReady, quiet).nodes.find((n) => n.id === "n1")?.ariaLabel).toBe("n1, zone z1, healthy");
  });
});

/* ── QA scope 2, findings #1 and #2: nodes:null, agents:full ─────────────── */

const agentsOnly: Topology = {
  nodes: [],
  agents: [
    { id: "a-1", nodeName: "n1", podIP: "10.0.0.1", zone: "z1" },
    { id: "a-2", nodeName: "n2", podIP: "10.0.0.2", zone: "z2" },
    /* A second agent on a node already seen — one lane, not two. */
    { id: "a-3", nodeName: "n1", podIP: "10.0.0.3", zone: "z1" },
  ],
  timestamp: "t",
};

describe("mapNodes", () => {
  it("prefers the Kubernetes node set whenever the controller has one", () => {
    const { nodes, source } = mapNodes(topo);
    expect(source).toBe("nodes");
    expect(nodes.map((n) => n.name)).toEqual(["n1", "n2"]);
    expect(nodes[1].ready).toBe(false);
  });

  it("falls back to the agents, deduped by node name, when nodes is empty", () => {
    const { nodes, source } = mapNodes(agentsOnly);
    expect(source).toBe("agents");
    expect(nodes.map((n) => n.name)).toEqual(["n1", "n2"]);
    expect(nodes.map((n) => n.zone)).toEqual(["z1", "z2"]);
  });

  it("leaves readiness UNDEFINED off-cluster rather than guessing either way", () => {
    expect(mapNodes(agentsOnly).nodes.every((n) => n.ready === undefined)).toBe(true);
  });

  it("has nothing to draw when both halves are empty", () => {
    expect(mapNodes({ nodes: [], agents: [], timestamp: "t" }).nodes).toHaveLength(0);
  });
});

describe("buildFlow — built from agents", () => {
  it("draws the zone lanes the old empty state said could not be drawn", () => {
    const { nodes, source } = buildFlow(agentsOnly, matrix);
    expect(source).toBe("agents");
    expect(nodes.filter((n) => n.type === "zone").map((n) => (n.data as { label: string }).label)).toEqual([
      "z1",
      "z2",
    ]);
    expect(nodes.filter((n) => n.type === "topoNode").map((n) => n.id)).toEqual(["n1", "n2"]);
  });

  it("takes per-node health from the matrix, the one signal that survives the fallback", () => {
    const { nodes } = buildFlow(agentsOnly, matrix);
    // n1's worst outbound path is 20%.
    expect(nodes.find((n) => n.id === "n1")?.className).toContain("failing");
    // n2 has no outbound problem and, crucially, is NOT condemned for an
    // unknown readiness the way `!n.ready` used to condemn it.
    expect(nodes.find((n) => n.id === "n2")?.className).toContain("topo-node--ok");
  });

  it("announces the readiness gap instead of sounding like a confirmed-ready node", () => {
    const { nodes } = buildFlow(agentsOnly, matrix);
    expect(nodes.find((n) => n.id === "n2")?.ariaLabel).toBe("n2, zone z2, healthy, readiness unknown");
  });

  it("still draws the problem edges between two agent-derived boxes", () => {
    const { edges, problemTotal } = buildFlow(agentsOnly, matrix);
    expect(problemTotal).toBe(1);
    expect(edges[0].id).toBe("n1->n2");
  });
});

describe("unfoldableEmpty", () => {
  it("stays silent when the map has boxes to draw, however they were built", () => {
    expect(
      unfoldableEmpty({ ...agentsOnly, historical: true, unfoldableEvents: 417, eventsFolded: 3 }),
    ).toBeNull();
  });

  it("still fires for a reconstruction with nothing on either side", () => {
    expect(
      unfoldableEmpty({ nodes: [], agents: [], timestamp: "t", historical: true, unfoldableEvents: 417, eventsFolded: 3 }),
    ).toEqual({ unfoldableEvents: 417, eventsFolded: 3 });
  });
});

/* ru The only case in this file that renders the page at all, and the only one with a LocaleProvider. */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

describe("TopologyPage — ru", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    /* onlineManager is module-global, so a paused-query case that did not reset it would pause every test after it. */
    onlineManager.setOnline(true);
    /* vitest.setup.ts backs localStorage with ONE Map per test FILE. */
    localStorage.removeItem(LOCALE_STORAGE_KEY);
  });

  it("renders the header and the unfoldable-history note in Russian", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    const unfoldable: Topology = {
      nodes: [],
      agents: [],
      timestamp: AT,
      historical: true,
      asOf: AT,
      eventsFolded: 3,
      unfoldableEvents: 417,
      truncated: false,
    };
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.startsWith("/api/v1/topology")) return Promise.resolve(json(unfoldable));
        if (href.includes("/api/v1/promql/query")) {
          return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
        }
        return Promise.resolve(json({}));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <LocaleProvider>
            <TopologyPage />
          </LocaleProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByText("На этот момент восстанавливать нечего")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Топология", level: 1 })).toBeInTheDocument();
    expect(
      screen.getByText(
        /^Консоль нашла событий топологии на этот момент и раньше: 417, а свернуть в набор узлов смогла 3: остальные не называют узел, и строить карту не из чего\..*этот отрезок истории восстановить нельзя/,
      ),
    ).toBeInTheDocument();
  });

  /* A dead controller is the one failure an operator opening this page is most
     likely to hit, and the page answered it with a BLANK main: heading, subtitle
     and nothing else. Both branches are pinned — with and without a node set to
     fall back on — because they are two different honest answers, not one. */
  const problem = (status: number, title: string, detail: string) =>
    new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
      status,
      headers: { "Content-Type": "application/problem+json" },
    });

  it("shows the error card, not a blank page, when the controller is down and nothing was ever loaded", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.startsWith("/api/v1/topology")) {
          return Promise.resolve(problem(502, "controller unavailable", "no controller leader answered after retries"));
        }
        if (href.includes("/api/v1/promql/query")) {
          return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
        }
        return Promise.resolve(json({}));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <LocaleProvider>
            <TopologyPage />
          </LocaleProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    );

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Топология недоступна");
    /* The server's own detail, verbatim — that sentence is the only thing that
       tells the operator WHICH half of the system is down. */
    expect(screen.getByTestId("topology-problem")).toHaveTextContent("no controller leader answered after retries");
  });

  it("shows the error card for a proxy 502 that carries no problem+json body", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.startsWith("/api/v1/topology")) {
          return Promise.resolve(
            new Response("<html>502 Bad Gateway</html>", {
              status: 502,
              statusText: "Bad Gateway",
              headers: { "Content-Type": "text/html" },
            }),
          );
        }
        return Promise.resolve(json({}));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <LocaleProvider>
            <TopologyPage />
          </LocaleProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("alert")).toHaveTextContent("Топология недоступна");
  });

  it("shows the error card when the request never reaches the console at all", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.startsWith("/api/v1/topology")) return Promise.reject(new TypeError("Failed to fetch"));
        return Promise.resolve(json({}));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <LocaleProvider>
            <TopologyPage />
          </LocaleProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("alert")).toHaveTextContent("Топология недоступна");
  });

  /* The state that actually produced the blank page: react-query's default
     networkMode pauses a query when the browser reports no connection, and a
     PAUSED query is pending WITHOUT fetching — so `isLoading` is false, there
     is no data and no error, and every branch on this page used to opt out at
     once. */
  it("says the request was never sent instead of going blank while the query is paused", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    onlineManager.setOnline(false);
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json({}))));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <LocaleProvider>
            <TopologyPage />
          </LocaleProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("alert")).toHaveTextContent("Браузер сообщает, что соединения нет");
  });

  /* The other half of finding #1: an error ON TOP of a node set that did load
     is not the same answer as an error with nothing behind it, and saying
     "Топология недоступна" over a map that is on screen is the dishonest one. */
  it("keeps the loaded node set and downgrades the error to a stale notice", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.startsWith("/api/v1/topology")) {
          return Promise.resolve(problem(502, "controller unavailable", "no controller leader answered after retries"));
        }
        return Promise.resolve(json({}));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(["topology"], { nodes: [], agents: [], timestamp: AT } satisfies Topology);
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <LocaleProvider>
            <TopologyPage />
          </LocaleProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    );

    /* The data-derived state survives the failed refresh… */
    expect(await screen.findByText("Контроллер пока не сообщил ни одного узла")).toBeInTheDocument();
    /* …and the failure is a notice about the REFRESH, not a claim that the page has nothing. */
    const stale = await screen.findByRole("status");
    expect(stale).toHaveTextContent("Данные не обновляются");
    expect(screen.getByTestId("topology-problem")).toHaveTextContent("no controller leader answered after retries");
    expect(screen.queryByText("Топология недоступна")).not.toBeInTheDocument();
  });
});
