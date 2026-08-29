import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider, translate, type Translate } from "@/lib/i18n";
import { topologyDict, type TopologyKey } from "@/lib/i18n/dict/topology";
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
    expect(edges[0].data).toMatchObject({ failLabel: "20.0%" });
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
    expect(edges[0].data).toMatchObject({ failLabel: "15.0%" });
    const kept = edges.map((e) => (e.data as { failLabel: string }).failLabel);
    expect(kept).not.toContain("5.0%"); // the 5 weakest (1–5%) were dropped
    expect(kept).toContain("6.0%");
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
    expect(edges[0].data).toMatchObject({ failLabel: "5.0% loss" });
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

describe("mapNodes ordering", () => {
  /* The controller answers in REGISTRATION order — whichever agent came up
     first — so the map's lanes reshuffled after every rollout and no node was
     ever where the operator last saw it. */
  const node = (name: string) => ({ name, zone: "z1", ready: true });

  /* m02 and m2 are the same NUMBER, so the collator calls them equal and the
     tie-break decides. It has to be the names themselves, not the order the
     controller happened to answer in — that is the very thing this sort exists to
     stop (lib/natural-name). */
  it("reads names the way an operator does, m10 last, and breaks a 02/2 tie the same way every time", () => {
    const registrationOrders = [
      [node("kconmon-prod-m10"), node("kconmon-prod-m2"), node("kconmon-prod-m02")],
      [node("kconmon-prod-m02"), node("kconmon-prod-m10"), node("kconmon-prod-m2")],
    ];

    // A plain codepoint sort puts m10 above m2, because "1" sorts before "2".
    for (const nodes of registrationOrders) {
      const mapped = mapNodes({ nodes, agents: [], timestamp: "t" } as never);
      expect(mapped.nodes.map((n) => n.name)).toEqual([
        "kconmon-prod-m02",
        "kconmon-prod-m2",
        "kconmon-prod-m10",
      ]);
    }
  });

  it("orders the agents-built map too, which is the same map by another route", () => {
    const { nodes, source } = mapNodes({
      nodes: [],
      agents: [
        { agentId: "a1", nodeName: "worker-10", zone: "z1", ready: true },
        { agentId: "a2", nodeName: "worker-2", zone: "z1", ready: true },
      ],
      timestamp: "t",
    } as never);

    expect(source).toBe("agents");
    expect(nodes.map((n) => n.name)).toEqual(["worker-2", "worker-10"]);
  });
});

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

/* ── a payload the map cannot stand behind ─────────────────────────────────
 *
 * QA scope 4. The map drew whatever the two responses said, and four shapes got
 * through that need no hostile server behind them — three of them producing a
 * React Flow graph with colliding ids, which is a duplicate-key warning today
 * and a box that updates as its twin tomorrow.
 */
describe("buildFlow — a node list that repeats itself or names nothing", () => {
  const T = (over: Partial<Topology>): Topology => ({ nodes: [], agents: [], timestamp: "t", ...over });

  /* A node name IS the React Flow id and the /nodes/{name} link, so a repeat is
     not a second node — it is two boxes nothing can tell apart. The agents
     branch had always deduped; the Kubernetes branch had not. */
  it("draws one box per NAME, so the graph's ids stay unique", () => {
    const { nodes } = buildFlow(
      T({ nodes: [
        { name: "dup", zone: "z1", ready: true },
        { name: "dup", zone: "z2", ready: false },
        { name: "other", zone: "z1", ready: true },
      ] }),
    );

    const ids = nodes.map((n) => n.id);
    expect(new Set(ids).size).toBe(ids.length);
    expect(nodes.filter((n) => n.type === "topoNode").map((n) => n.id)).toEqual(["dup", "other"]);
  });

  it("steps over a row that is null or names no node at all", () => {
    const { nodes } = buildFlow(
      T({ nodes: [null, { name: "", zone: "z", ready: true }, { name: "real", zone: "z", ready: true }] as never }),
    );

    expect(nodes.filter((n) => n.type === "topoNode").map((n) => n.id)).toEqual(["real"]);
  });

  it("has nothing to draw, rather than throwing, when the halves are not lists", () => {
    const { nodes, edges } = buildFlow({ nodes: null, agents: "none", timestamp: "t" } as never);
    expect(nodes).toEqual([]);
    expect(edges).toEqual([]);
  });
});

describe("buildFlow — the lane for nodes with no reported zone", () => {
  /* Not a hypothetical: a cluster whose nodes carry no topology.kubernetes.io/zone
     label answers with `zone: ""` for every one of them, and the lane header
     «{zone} · N nodes» then read « · 10 nodes» — a heading naming nothing, in a
     row of headings that do. */
  const unzoned: Topology = {
    nodes: [{ name: "n1", zone: "", ready: true }, { name: "n2", zone: "", ready: true }],
    agents: [],
    timestamp: "t",
  };

  it("gives the lane a name instead of an empty string", () => {
    const zone = buildFlow(unzoned).nodes.find((n) => n.type === "zone");
    expect(zone?.data).toMatchObject({ label: "no zone reported", count: 2 });
    // The id stays the raw key — it is a handle, not a word.
    expect(zone?.id).toBe("zone:");
  });

  it("carries the same name into what a screen reader hears about the box", () => {
    const box = buildFlow(unzoned).nodes.find((n) => n.id === "n1");
    // The word "zone" belongs to the label, not to the value: it used to be said
    // twice — "zone no zone reported".
    expect(box?.ariaLabel).toBe("n1, no zone reported, healthy");
  });

  it("says it in Russian when that is the reader's language", () => {
    const ruT: Translate<TopologyKey> = (key, vars) => translate(topologyDict, "ru", key, vars);
    const { nodes } = buildFlow(unzoned, undefined, ruT);
    expect(nodes.find((n) => n.type === "zone")?.data).toMatchObject({ label: "зона не указана" });
    expect(nodes.find((n) => n.id === "n1")?.ariaLabel).toBe("n1, зона не указана, в норме");
  });

  it("leaves a zone the controller DID report exactly as it is", () => {
    const zone = buildFlow(topo).nodes.find((n) => n.type === "zone");
    expect(zone?.data).toMatchObject({ label: "z1" });
  });
});

describe("buildFlow — edges the matrix says twice, or names it cannot join", () => {
  const pair = (nodes: string[]): Topology => ({
    nodes: nodes.map((name) => ({ name, zone: "z", ready: true })),
    agents: [],
    timestamp: "t",
  });
  const m = (cells: Matrix["cells"]): Matrix => ({
    protocol: "tcp", plane: "pod", nodes: [], cells, timestamp: "t",
  });

  /* Two cells for one A→B is a matrix the console did not write and cannot
     arbitrate; drawing both gave React Flow two edges under one id and counted
     the same path twice in the "showing N of M" caption. */
  it("draws one edge per ordered pair, keeping the WORST reading of it", () => {
    const { edges, problemTotal } = buildFlow(pair(["a", "b"]), m([
      { source: "a", destination: "b", failRatio: 0.05 },
      { source: "a", destination: "b", failRatio: 0.4 },
    ]));

    expect(edges).toHaveLength(1);
    expect(problemTotal).toBe(1);
    expect(edges[0].data).toMatchObject({ failLabel: "40.0%" });
    expect(edges[0].className).toContain("failing");
  });

  /* `${source}->${destination}` is only injective while no name contains the
     arrow: "a->b"→"c" and "a"→"b->c" are both "a->b->c". */
  it("keeps the edge id injective when a node name contains the arrow", () => {
    const { edges } = buildFlow(pair(["a->b", "c", "a", "b->c"]), m([
      { source: "a->b", destination: "c", failRatio: 0.5 },
      { source: "a", destination: "b->c", failRatio: 0.4 },
    ]));

    expect(edges).toHaveLength(2);
    expect(new Set(edges.map((e) => e.id)).size).toBe(2);
  });

  it("leaves an ordinary pair's id in the shape it always had", () => {
    expect(buildFlow(topo, matrix).edges[0].id).toBe("n1->n2");
  });

  /* Same gate the grid reads its figures through: an edge labelled "NaN%" or
     "Infinity%" is worse than no edge at all. */
  it("refuses a severity that is not a finite number rather than labelling it Infinity", () => {
    const raw = JSON.parse('[{"source":"a","destination":"b","failRatio":1e999}]') as Matrix["cells"];
    const { edges, problemTotal } = buildFlow(pair(["a", "b"]), m(raw));

    expect(edges).toEqual([]);
    expect(problemTotal).toBe(0);
  });

  it("refuses a severity that arrived as a string", () => {
    const { edges } = buildFlow(pair(["a", "b"]), m([
      { source: "a", destination: "b", failRatio: "0.5" },
    ] as never));

    expect(edges).toEqual([]);
  });

  it("steps over a null cell instead of dereferencing it", () => {
    const { edges } = buildFlow(pair(["a", "b"]), m([
      null,
      { source: "a", destination: "b", failRatio: 0.3 },
    ] as never));

    expect(edges).toHaveLength(1);
  });

  it("colours a node from a loss ratio over 100%, which is a claim rather than a corruption", () => {
    const { nodes } = buildFlow(pair(["a", "b"]), m([
      { source: "a", destination: "b", failRatio: null, lossRatio: 1.5 },
    ]));

    expect(nodes.find((n) => n.id === "a")?.className).toContain("failing");
  });
});

describe("mapNodes — names that do not sort themselves", () => {
  const of = (...names: string[]) =>
    mapNodes({ nodes: names.map((name) => ({ name, zone: "z", ready: true })), agents: [], timestamp: "t" })
      .nodes.map((n) => n.name);

  it("reads a run of numbers as numbers, however long they get", () => {
    expect(of("m100", "m10", "m2", "m1")).toEqual(["m1", "m2", "m10", "m100"]);
  });

  it("does not let case decide the order", () => {
    expect(of("M3", "m1", "m2")).toEqual(["m1", "m2", "M3"]);
  });

  it("orders bare numbers and non-Latin names without falling over", () => {
    expect(of("10", "2", "узел-10", "узел-2")).toEqual(["2", "10", "узел-2", "узел-10"]);
  });
});

/* ── a stamp the controller mangled ────────────────────────────────────────
 *
 * `new Date` answers an Invalid Date for anything it cannot read rather than
 * throwing, and toLocaleString prints that verbatim: the header read "as of
 * Invalid Date" for one bad field on an otherwise perfectly good map.
 */
describe("TopologyPage — an asOf the console cannot read", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    onlineManager.setOnline(true);
    localStorage.removeItem(LOCALE_STORAGE_KEY);
  });

  const renderWith = (asOf: unknown) => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.startsWith("/api/v1/topology")) {
          return Promise.resolve(
            json({ nodes: [{ name: "n1", zone: "z", ready: true }], agents: [], timestamp: "t", asOf }),
          );
        }
        return Promise.resolve(json({}));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <ThemeProvider><TopologyPage /></ThemeProvider>
      </QueryClientProvider>,
    );
  };

  it("falls back to the Live sentence rather than printing Invalid Date", async () => {
    renderWith("not-a-date");
    expect(await screen.findByText(/^Live zone\/node map\./)).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/Invalid Date/);
  });

  it("says the same for a stamp that is not even a string", async () => {
    renderWith(12345);
    expect(await screen.findByText(/^Live zone\/node map\./)).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/Invalid Date|NaN/);
  });

  it("still quotes a stamp it CAN read", async () => {
    renderWith("2026-08-01T12:00:00Z");
    expect(await screen.findByText(/^Zone\/node map as of /)).toBeInTheDocument();
  });
});

describe("buildFlow — a fleet far bigger than the picture", () => {
  it("keeps every id unique and every box inside its own lane at 120 nodes", () => {
    const names = Array.from({ length: 120 }, (_, i) => `worker-${String(i).padStart(3, "0")}`);
    const big: Topology = {
      nodes: names.map((name, i) => ({ name, zone: `z${i % 3}`, ready: true })),
      agents: [],
      timestamp: "t",
    };
    /* Every ordered pair a problem, which is 14 280 cells — far past the cap,
       and the shape that used to make the caption count paths twice. */
    const cells = names.flatMap((s) =>
      names.filter((d) => d !== s).slice(0, 3).map((d) => ({ source: s, destination: d, failRatio: 0.5 })),
    );
    const { nodes, edges, problemTotal } = buildFlow(big, {
      protocol: "tcp", plane: "pod", nodes: names, cells, timestamp: "t",
    });

    const boxes = nodes.filter((n) => n.type === "topoNode");
    expect(boxes).toHaveLength(120);
    expect(new Set(nodes.map((n) => n.id)).size).toBe(nodes.length);
    expect(new Set(edges.map((e) => e.id)).size).toBe(edges.length);
    expect(edges).toHaveLength(10); // EDGE_CAP; the rest are counted, not drawn
    expect(problemTotal).toBe(360);
    expect(boxes.every((n) => n.parentId?.startsWith("zone:") && n.extent === "parent")).toBe(true);
  });
});

/*
 * The map, the grid and the cards print ONE number.
 *
 * The edge label rounded to whole percent while every other surface prints one decimal, so a path
 * measured at 9.6% was labelled "10%" and spoken as "10%" by its aria-label — while the edge was
 * drawn degraded and the legend two lines above read "Failing · ≥ 10%". It also collapsed 1.0%, 1.4%
 * and 1.9% into one indistinguishable "1%".
 */
describe("edge label precision", () => {
  it("prints the same one-decimal ratio the grid and the cards print", () => {
    const nearTopo: Topology = {
      nodes: [
        { name: "a", zone: "z", ready: true },
        { name: "b", zone: "z", ready: true },
      ],
      agents: [],
      timestamp: "t",
    };
    const nearMatrix: Matrix = {
      protocol: "tcp",
      plane: "pod",
      nodes: ["a", "b"],
      cells: [{ source: "a", destination: "b", failRatio: 0.096 }],
      timestamp: "t",
    };

    const { edges } = buildFlow(nearTopo, nearMatrix);
    expect(edges[0].data).toMatchObject({ failLabel: "9.6%" });
    // The class and the legend both say sub-10%; the label must not say 10%.
    expect(edges[0].className).toContain("degraded");
    expect(edges[0].ariaLabel).toContain("9.6%");
  });
});

/* ── M4-1/M4-5: the tool surface and the mono data face ──────────────────────
 *
 * Class pins, because jsdom lays nothing out: the map must sit straight on the
 * page (no Card between the shell's column and the working surface) under the
 * slim tool header, and the node labels — identifiers — must wear mono-data
 * (.topo-node's own 13px already matches the face's size, so nothing shifts).
 */
describe("TopologyPage — the tool surface", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    onlineManager.setOnline(true);
  });

  const renderDrawn = () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.startsWith("/api/v1/topology")) {
          return Promise.resolve(
            json({ nodes: [{ name: "n1", zone: "z", ready: true }], agents: [], timestamp: "t" }),
          );
        }
        return Promise.resolve(json({}));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <ThemeProvider><TopologyPage /></ThemeProvider>
      </QueryClientProvider>,
    );
  };

  it("draws the map on the page itself: slim tool header, no card around the working surface", async () => {
    const { container } = renderDrawn();
    await screen.findByTitle("n1");
    expect(screen.getByRole("heading", { level: 1 }).className).toContain("text-lg");
    const pane = container.querySelector(".react-flow");
    expect(pane).not.toBeNull();
    expect((pane as HTMLElement).closest(".shadow-card")).toBeNull();
  });

  it("sets the node labels in the mono data face", async () => {
    renderDrawn();
    const label = await screen.findByTitle("n1");
    expect(label.className).toContain("mono-data");
    expect(label).toHaveTextContent("n1");
  });
});
