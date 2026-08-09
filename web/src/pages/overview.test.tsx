import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OverviewPage, fmtAge, sortFiringAlerts, summarize } from "./overview";
import type { Alert, Matrix, Topology } from "@/lib/types";
import { fmtEventTime } from "@/lib/utils";

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

  /* QA round 1, finding #3: a cell with a p95 RTT and no failure ratio was
     counted as nothing at all, so a page full of latency numbers announced
     "no probe data". A latency sample IS a measurement. */
  describe("what counts as measured", () => {
    const rttOnly: Matrix = {
      ...matrix,
      cells: [
        { source: "a", destination: "b", failRatio: null, rttP95: 2_000_000 },
        { source: "a", destination: "c", failRatio: null },
      ],
    };

    it("counts a pair measured on RTT alone", () => {
      expect(summarize(rttOnly).pairsTotal).toBe(1);
    });

    it("keeps the tiers on the failure ratio — an RTT-only pair is measured, not ranked", () => {
      const s = summarize(rttOnly);
      expect(s.pairsScored).toBe(0);
      expect(s.pairsFailing).toBe(0);
      expect(s.pairsDegraded).toBe(0);
      expect(s.worstPairs).toHaveLength(0);
    });

    it("counts nothing when neither vector has anything for the pair", () => {
      expect(summarize({ ...matrix, cells: [{ source: "a", destination: "b", failRatio: null }] }).pairsTotal).toBe(0);
    });

    it("breaks a failure-ratio tie with the slower pair first", () => {
      const tied: Matrix = {
        ...matrix,
        cells: [
          { source: "a", destination: "b", failRatio: 0.2, rttP95: 1_000_000 },
          { source: "c", destination: "d", failRatio: 0.2, rttP95: 8_000_000 },
        ],
      };
      expect(summarize(tied).worstPairs.map((c) => c.source)).toEqual(["c", "a"]);
    });
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

  /* QA round 1, finding #4: every pair number on this page comes from
     useMatrix("tcp"), and an unlabelled "Failing pairs" claimed UDP and ICMP
     too. Label, not aggregate — the qualifier rides the tiles and the section
     header. */
  it("qualifies the pair numbers as TCP on the pod plane", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(topo) : json(matrix))),
    );
    renderPage();
    await screen.findByText("1/2");

    // Both pair tiles and the worst-pairs header carry it.
    expect(screen.getAllByText("TCP · pod plane").length).toBeGreaterThanOrEqual(3);
  });

  /* QA round 1, finding #3, at the page level: the slate that blamed an empty
     Prometheus while the RTT column was full. */
  it("does not claim there is no probe data when latency is flowing", async () => {
    const rttOnly = {
      ...matrix,
      cells: [{ source: "a", destination: "b", failRatio: null, rttP95: 2_000_000 }],
    };
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(topo) : json(rttOnly))),
    );
    renderPage();

    expect(await screen.findByText("No failure ratio for these pairs")).toBeInTheDocument();
    expect(screen.queryByText("No probe data in Prometheus yet")).toBeNull();
    expect(screen.getByText(/1 measured pair$/)).toBeInTheDocument();
  });
});

/* ── M6: the two panels that replaced the placeholders (Decision 9) ─────── */

function meBody(permissions: string[]) {
  return { subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["viewer"] }, permissions };
}

function configBody(databaseConfigured: boolean) {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: databaseConfigured },
  };
}

/** One entry of GET /api/v1/alerts. `ruleId` present = this console manages the
 *  rule; absent = somebody else's rule, which the card shows and tags. */
function alertRow(over: Record<string, unknown> = {}) {
  return {
    name: "PairLossHigh",
    state: "firing",
    severity: "critical",
    labels: { alertname: "PairLossHigh", severity: "critical", source_node: "node-a", destination_node: "node-b" },
    annotations: { summary: "loss over 10%" },
    activeAt: "2026-01-01T00:00:00Z",
    value: "1e+00",
    ruleId: "11111111-1111-4111-8111-111111111111",
    ...over,
  };
}

const ALL_READS = ["incidents:read", "events:read", "alerts:read"];

function incidentRow(over: Record<string, unknown> = {}) {
  return {
    id: "inc-1",
    title: "Loss between node-a and node-b",
    scope: "node-a→node-b",
    fromAt: "2026-01-01T00:00:00Z",
    status: "open",
    notes: "",
    pinned: [],
    createdBy: "user:ada",
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function eventRow(over: Record<string, unknown> = {}) {
  return {
    id: "e-1",
    seq: 1,
    type: "topology_changed",
    severity: "warn",
    scope: "node-a→node-b",
    timestamp: "2026-01-01T00:05:00Z",
    summary: "node-b NotReady",
    details: null,
    ...over,
  };
}

interface PanelOptions {
  permissions?: string[];
  database?: boolean;
  incidents?: unknown[];
  events?: unknown[];
  alerts?: unknown[];
  /** The `promConfigured` flag GET /api/v1/alerts answers WITH the 200 — the
   *  route's degraded shape, not a status code (M7 Decision 6). */
  promConfigured?: boolean;
  /** A 502 from GET /api/v1/alerts: Prometheus is wired and did not answer. */
  alertsProblem?: string;
}

function renderOverview(opts: PanelOptions = {}) {
  const {
    permissions = ALL_READS,
    database = true,
    incidents = [],
    events = [],
    alerts = [],
    promConfigured = true,
    alertsProblem,
  } = opts;
  const urls: string[] = [];
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    urls.push(href);
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(database)));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topo));
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents, nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    if (href.startsWith("/api/v1/alerts")) {
      if (alertsProblem !== undefined) {
        return Promise.resolve(
          new Response(
            JSON.stringify({ type: "about:blank", title: "alerts unavailable", status: 502, detail: alertsProblem }),
            { status: 502, headers: { "Content-Type": "application/problem+json" } },
          ),
        );
      }
      return Promise.resolve(json({ alerts, promConfigured }));
    }
    return Promise.resolve(json(matrix));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <OverviewPage />
    </QueryClientProvider>,
  );
  return { ...utils, urls, fetchMock };
}

describe("OverviewPage — Open incidents (Decision 9)", () => {
  it("lists the open incidents newest-first, each one a permalink into incident mode", async () => {
    renderOverview({ incidents: [incidentRow(), incidentRow({ id: "inc-2", title: "Target flapping", scope: "" })] });

    const panel = await screen.findByRole("region", { name: "Open incidents" });
    const rows = await within(panel).findAllByTestId("open-incident");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByRole("link", { name: "Loss between node-a and node-b" }).getAttribute("href")).toBe(
      "/investigate?incident=inc-1",
    );
    // "" is the GLOBAL scope, and the chip says so rather than showing nothing.
    expect(within(rows[1]).getByText("global")).toBeInTheDocument();
  });

  it("asks for exactly the open ones, five of them", async () => {
    const { urls } = renderOverview({ incidents: [incidentRow()] });
    await screen.findByTestId("open-incident");

    const call = urls.find((u) => u.startsWith("/api/v1/incidents"));
    expect(call).toContain("status=open");
    expect(call).toContain("limit=5");
  });

  it("says there are none rather than rendering an empty box", async () => {
    renderOverview({ incidents: [] });
    expect(await screen.findByText(/no open incidents/i)).toBeInTheDocument();
  });

  it("without incidents:read: one muted line and ZERO requests", async () => {
    const { urls } = renderOverview({ permissions: ["events:read"] });

    expect(await screen.findByText(/open incidents need incidents:read/i)).toBeInTheDocument();
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(false);
  });

  it("without a database: the one-line database note and ZERO requests", async () => {
    const { urls } = renderOverview({ database: false });

    expect((await screen.findAllByText(/set console\.database\.mode/i)).length).toBeGreaterThan(0);
    await waitFor(() => expect(urls.some((u) => u.includes("/api/v1/config"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(false);
  });
});

describe("OverviewPage — Recent events (Decision 9)", () => {
  it("renders the newest ten in the Live feed's own vocabulary, and links to Live", async () => {
    renderOverview({ events: [eventRow(), eventRow({ id: "e-2", summary: "agent restarted", severity: "info" })] });

    const panel = await screen.findByRole("region", { name: "Recent events" });
    const rows = await within(panel).findAllByTestId("overview-event");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText("node-b NotReady")).toBeInTheDocument();
    // Live's capitalized vocabulary, not a second one for the same fact.
    expect(within(rows[0]).getByText("Warn")).toBeInTheDocument();
    expect(within(panel).getByRole("link", { name: /open Live/i }).getAttribute("href")).toBe("/live");
  });

  /* QA round 1, finding #10: this card and /live showed the same event at two
     different times. ONE formatter now, in lib/utils — pages/live.test.tsx
     pins the same call for the same input on the other side. */
  it("stamps a row with the shared event clock, not a private one", async () => {
    renderOverview({ events: [eventRow()] });

    const row = await screen.findByTestId("overview-event");
    expect(within(row).getByText(fmtEventTime("2026-01-01T00:05:00Z"))).toBeInTheDocument();
  });

  it("asks for ten", async () => {
    const { urls } = renderOverview({ events: [eventRow()] });
    await screen.findByTestId("overview-event");
    expect(urls.find((u) => u.startsWith("/api/v1/events"))).toContain("limit=10");
  });

  it("without events:read: one muted line and ZERO requests", async () => {
    const { urls } = renderOverview({ permissions: ["incidents:read"] });

    expect(await screen.findByText(/fleet events need events:read/i)).toBeInTheDocument();
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(false);
  });

  it("says nothing has happened rather than looking broken", async () => {
    renderOverview({ events: [] });
    expect(await screen.findByText(/nothing has happened yet/i)).toBeInTheDocument();
  });
});

/* ── M7 Task 8: the Firing alerts card, replacing the placeholder ───────── */

describe("OverviewPage — Firing alerts (Decision 6)", () => {
  it("the M6 placeholder is GONE — the card reads the real firing set", async () => {
    renderOverview({ alerts: [alertRow()] });

    expect(await screen.findByText("Firing alerts")).toBeInTheDocument();
    expect(screen.queryByText(/arrives with a later milestone/i)).toBeNull();
    expect(await screen.findByTestId("firing-alert")).toBeInTheDocument();
  });

  it("asks for the WHOLE fleet's firing state, not only the rules this console manages", async () => {
    const { urls } = renderOverview({ alerts: [alertRow()] });
    await screen.findByTestId("firing-alert");

    const call = urls.find((u) => u.startsWith("/api/v1/alerts"));
    expect(call).toBe("/api/v1/alerts");
    expect(call).not.toContain("managedOnly");
  });

  it("sorts critical over warning over info", async () => {
    renderOverview({
      alerts: [
        alertRow({ name: "Info", severity: "info" }),
        alertRow({ name: "Warn", severity: "warning" }),
        alertRow({ name: "Crit", severity: "critical" }),
      ],
    });

    const rows = await screen.findAllByTestId("firing-alert");
    expect(rows.map((r) => within(r).getByTestId("firing-alert-name").textContent)).toEqual(["Crit", "Warn", "Info"]);
  });

  it("links a MANAGED alert to /alerting and tags a foreign one as unmanaged", async () => {
    renderOverview({
      alerts: [alertRow(), alertRow({ name: "SomebodyElsesRule", ruleId: undefined, severity: "warning" })],
    });

    const rows = await screen.findAllByTestId("firing-alert");
    // ?rule= names the row rather than dropping the reader at the top of the
    // list to find it themselves (QA round 1, finding #17).
    expect(within(rows[0]).getByRole("link", { name: "PairLossHigh" }).getAttribute("href")).toBe(
      "/alerting?rule=11111111-1111-4111-8111-111111111111",
    );
    expect(within(rows[0]).queryByText("unmanaged")).toBeNull();

    // The console never implies it owns somebody else's rule: no /alerting link.
    expect(within(rows[1]).queryByRole("link", { name: "SomebodyElsesRule" })).toBeNull();
    expect(within(rows[1]).getByText("unmanaged")).toBeInTheDocument();
  });

  it("offers an Investigate link ONLY when the labels carry a scope this page can open", async () => {
    renderOverview({
      alerts: [
        alertRow(),
        alertRow({ name: "NoScope", severity: "warning", labels: { alertname: "NoScope", severity: "warning" } }),
      ],
    });

    const rows = await screen.findAllByTestId("firing-alert");
    const href = within(rows[0]).getByRole("link", { name: /investigate/i }).getAttribute("href") ?? "";
    expect(href).toContain("/investigate?kind=pair");
    expect(href).toContain(encodeURIComponent("node-a→node-b"));
    expect(within(rows[1]).queryByRole("link", { name: /investigate/i })).toBeNull();
  });

  it("shows the severity and how long it has been firing", async () => {
    renderOverview({ alerts: [alertRow()] });

    const row = await screen.findByTestId("firing-alert");
    expect(within(row).getByText("critical")).toBeInTheDocument();
    // The label set travels in the row's title attribute, the worst-pairs
    // card's own idiom for detail that does not fit.
    expect(within(row).getByTestId("firing-alert-labels").getAttribute("title")).toContain("source_node=node-a");
  });

  it("says nothing is firing — a real answer, not an empty box", async () => {
    renderOverview({ alerts: [] });
    expect(await screen.findByText(/nothing is firing/i)).toBeInTheDocument();
  });

  it("does not list a PENDING alert under a card called Firing alerts", async () => {
    renderOverview({ alerts: [alertRow({ state: "pending" })] });

    expect(await screen.findByText(/nothing is firing/i)).toBeInTheDocument();
    expect(screen.queryByTestId("firing-alert")).toBeNull();
  });

  it("without Prometheus: says nobody is watching rather than that nothing is firing", async () => {
    renderOverview({ alerts: [], promConfigured: false });

    expect(await screen.findByText(/prometheus is not configured/i)).toBeInTheDocument();
    expect(screen.queryByText(/nothing is firing/i)).toBeNull();
  });

  it("surfaces a 502 verbatim — a failing Prometheus is never a quiet fleet", async () => {
    renderOverview({ alertsProblem: "prometheus is configured but did not answer /api/v1/alerts" });

    expect(
      await screen.findByText("prometheus is configured but did not answer /api/v1/alerts"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/nothing is firing/i)).toBeNull();
  });

  it("without alerts:read: one muted line and ZERO requests", async () => {
    const { urls } = renderOverview({ permissions: ["incidents:read", "events:read"], alerts: [alertRow()] });

    expect(await screen.findByText(/firing alerts need alerts:read/i)).toBeInTheDocument();
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/alerts"))).toBe(false);
  });
});

describe("sortFiringAlerts", () => {
  const row = (severity: string, name: string, activeAt?: string): Alert =>
    ({ name, state: "firing", severity, labels: {}, annotations: {}, value: "1", ...(activeAt ? { activeAt } : {}) }) as Alert;

  it("orders critical, warning, info, then anything else", () => {
    const out = sortFiringAlerts([row("info", "i"), row("nonsense", "x"), row("critical", "c"), row("warning", "w")]);
    expect(out.map((a) => a.name)).toEqual(["c", "w", "i", "x"]);
  });

  it("breaks a severity tie with the OLDEST first — longest firing leads", () => {
    const out = sortFiringAlerts([
      row("critical", "new", "2026-01-01T00:10:00Z"),
      row("critical", "old", "2026-01-01T00:00:00Z"),
    ]);
    expect(out.map((a) => a.name)).toEqual(["old", "new"]);
  });

  it("is total and does not mutate its input", () => {
    const input = [row("critical", "b"), row("critical", "a")];
    const snapshot = [...input];
    expect(sortFiringAlerts(input).map((a) => a.name)).toEqual(["a", "b"]);
    expect(input).toEqual(snapshot);
  });
});

describe("fmtAge", () => {
  it("coarsens: seconds, minutes, hours, then days", () => {
    const now = new Date("2026-01-02T00:00:00Z");
    expect(fmtAge("2026-01-01T23:59:30Z", now)).toBe("30s");
    expect(fmtAge("2026-01-01T23:30:00Z", now)).toBe("30m");
    expect(fmtAge("2026-01-01T00:00:00Z", now)).toBe("24h");
    expect(fmtAge("2025-12-28T00:00:00Z", now)).toBe("5d");
  });

  it("is a dash for an unparseable instant, never NaN", () => {
    expect(fmtAge("never", new Date())).toBe("—");
  });
});
