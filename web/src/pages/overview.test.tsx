import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LOCALE_STORAGE_KEY, LocaleProvider, translate, type Translate } from "@/lib/i18n";
import { overviewDict, type OverviewKey } from "@/lib/i18n/dict/overview";
import { OverviewPage, fmtAge, foldBounds, nodesTile, sortFiringAlerts, summarize } from "./overview";
import type { Alert, Matrix, Topology } from "@/lib/types";
import { fmtEventStamp, fmtEventTime } from "@/lib/utils";

/** The two translators the pure helpers take, so a unit case can read either
 *  language without mounting a provider. */
const enT: Translate<OverviewKey> = (k, v) => translate(overviewDict, "en", k, v);
const ruT: Translate<OverviewKey> = (k, v) => translate(overviewDict, "ru", k, v);

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

  /* A latency sample IS a measurement. */
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

  /* Label, not aggregate — the qualifier rides the tiles and the section header. */
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

  /* ── #1 on the page: the tile refuses to invent an empty cluster ──────── */
  it("counts the agents when the topology answers with no nodes at all", async () => {
    const nodeless = {
      nodes: null,
      agents: [
        { id: "a-1", nodeName: "a", podIP: "", zone: "z" },
        { id: "a-2", nodeName: "b", podIP: "", zone: "z" },
      ],
      timestamp: "2026-01-01T00:00:00Z",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(nodeless) : json(matrix))),
    );
    renderPage();

    expect(
      await screen.findByText("Counted from agents — no k8s node inventory, so readiness is unknown."),
    ).toBeInTheDocument();
    // Tile order is nodes, failing, degraded.
    expect(screen.getAllByTestId("stat-value")[0]).toHaveTextContent("2");
    expect(screen.queryByText("0/0")).toBeNull();
  });

  /* ── #5: zero over zero renders as an em-dash, not as a clean fleet ───── */
  it("shows an em-dash and the no-data note when nothing was measured", async () => {
    const unmeasured = { ...matrix, cells: [{ source: "a", destination: "b", failRatio: null }] };
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(topo) : json(unmeasured))),
    );
    renderPage();

    await screen.findByText("No probe data in Prometheus yet");
    const notes = screen.getAllByTestId("tile-note");
    expect(notes).toHaveLength(2); // the failing tile and the degraded one
    expect(notes[0]).toHaveTextContent("No pair was measured here, so there is nothing to count.");
    // Three tiles, and the only counted one is the nodes tile.
    expect(screen.getAllByText("—")).toHaveLength(2);
    expect(screen.queryByText("0")).toBeNull();
  });

  /* ── #7: 9 scored out of 90 measured is not a healthy fleet ───────────── */
  it("says how much of the measured set is actually scored", async () => {
    const partly = {
      ...matrix,
      cells: [
        { source: "a", destination: "b", failRatio: 0.001 },
        { source: "a", destination: "c", failRatio: null, rttP95: 2_000_000 },
        { source: "b", destination: "c", failRatio: null, rttP95: 3_000_000 },
      ],
    };
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(topo) : json(partly))),
    );
    renderPage();

    // The healthy slate is what the gap line has to survive next to.
    expect(await screen.findByText("No failing or degraded pairs")).toBeInTheDocument();
    expect(screen.getByTestId("scored-gap")).toHaveTextContent(
      "1 of 3 pairs have a failure ratio; the rest have no failure samples.",
    );
  });

  it("says nothing about a gap when every measured pair is scored", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(topo) : json({
        ...matrix,
        cells: [{ source: "a", destination: "b", failRatio: 0.001 }],
      }))),
    );
    renderPage();

    await screen.findByText("No failing or degraded pairs");
    expect(screen.queryByTestId("scored-gap")).toBeNull();
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

/* jsdom computes no layout, so the 375px overflow is pinned STRUCTURALLY: the
   two panels sit in `grid gap-4 sm:grid-cols-2`, and a grid item's default
   min-width:auto is what refused to shrink below its own min-content.
   Measured in Chrome at 375x812 against the live console, /:
     before  main.scrollWidth 495 / clientWidth 375, both panels 479px wide
     after   main.scrollWidth 375 / clientWidth 375, both panels 343px wide, 0 elements past the viewport
   Losing min-w-0 on either panel brings the horizontal scroll straight back. */
describe("OverviewPage — 375px overflow (QA finding #3)", () => {
  it("gives both bottom panels min-w-0 so the grid column cannot blow past the viewport", async () => {
    renderOverview({ incidents: [incidentRow()], alerts: [alertRow()] });

    const incidents = await screen.findByTestId("open-incidents-panel");
    const alerts = await screen.findByTestId("firing-alerts-panel");
    expect(incidents.className).toContain("min-w-0");
    expect(alerts.className).toContain("min-w-0");
    /* Both must be children of the SAME grid — min-w-0 on an element that is not a grid item fixes nothing. */
    expect(incidents.parentElement).toBe(alerts.parentElement);
    expect(incidents.parentElement?.className).toContain("grid");
  });
});

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

  /* ONE formatter now, in lib/utils — pages/live.test.tsx pins the same call for the same input on the other side. */
  /* The DAY, not a bare clock. This card has no lower bound on the window it asks for, and with the
     Time Machine engaged its `to` is the viewed instant — so its ten newest rows can be days old
     under a heading that says "Recent events". A bare "03:05" then reads as this afternoon, which is
     the one reading an operator must not make from a change feed. The two sibling feeds print it
     this way already. */
  it("stamps a row with the shared event clock, DAY included, not a private one", async () => {
    renderOverview({ events: [eventRow()] });

    const row = await screen.findByTestId("overview-event");
    expect(within(row).getByText(fmtEventStamp("2026-01-01T00:05:00Z"))).toBeInTheDocument();
    // A row from another day carries its date; the bare-clock form would not.
    expect(within(row).queryByText(fmtEventTime("2026-01-01T00:05:00Z"))).toBeNull();
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

  /* The product decision, at the transport: kconmon-ng is not an aggregator of
     everybody's alerts. A cluster's own backdrop belongs in Alertmanager and in
     Grafana; this console shows the rules it manages and nothing else. The route
     does the filtering, so a foreign alert never crosses the wire. */
  it("asks ONLY for the rules this console manages", async () => {
    const { urls } = renderOverview({ alerts: [alertRow()] });
    await screen.findByTestId("firing-alert");

    const call = urls.find((u) => u.startsWith("/api/v1/alerts"));
    expect(call).toBe("/api/v1/alerts?managedOnly=true");
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

  it("links every row to its own rule — there is no unmanaged row left to tag", async () => {
    renderOverview({ alerts: [alertRow()] });

    const rows = await screen.findAllByTestId("firing-alert");
    // ?rule= names the row rather than dropping the reader at the top of the
    // list to find it themselves (QA round 1, finding #17).
    expect(within(rows[0]).getByRole("link", { name: "PairLossHigh" }).getAttribute("href")).toBe(
      "/alerting?rule=11111111-1111-4111-8111-111111111111",
    );
    // The badge and its two dictionary keys are GONE with the rows they explained.
    expect(screen.queryByText("unmanaged")).toBeNull();
  });

  it("says the card is bounded to this console's rules rather than implying the cluster is quiet", async () => {
    renderOverview({ alerts: [alertRow()] });
    await screen.findByTestId("firing-alert");
    expect(screen.getByText(/only the rules this console manages/i)).toBeInTheDocument();
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

  /* "Nothing is firing" would be a claim about the whole cluster, and this card
     no longer reads the whole cluster: it has to say WHOSE rules are quiet. */
  it("says none of THIS CONSOLE's rules is firing, not that nothing is", async () => {
    renderOverview({ alerts: [] });
    const note = await screen.findByText(/none of this console's rules is firing/i);
    expect(note).toBeInTheDocument();
    expect(screen.queryByText(/^Nothing is firing/i)).toBeNull();
  });

  it("does not list a PENDING alert under a card called Firing alerts", async () => {
    renderOverview({ alerts: [alertRow({ state: "pending" })] });

    expect(await screen.findByText(/none of this console's rules is firing/i)).toBeInTheDocument();
    expect(screen.queryByTestId("firing-alert")).toBeNull();
  });

  it("without Prometheus: says nobody is watching rather than that nothing is firing", async () => {
    renderOverview({ alerts: [], promConfigured: false });

    expect(await screen.findByText(/prometheus is not configured/i)).toBeInTheDocument();
    expect(screen.queryByText(/none of this console's rules is firing/i)).toBeNull();
  });

  it("surfaces a 502 verbatim — a failing Prometheus is never a quiet fleet", async () => {
    renderOverview({ alertsProblem: "prometheus is configured but did not answer /api/v1/alerts" });

    expect(
      await screen.findByText("prometheus is configured but did not answer /api/v1/alerts"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/none of this console's rules is firing/i)).toBeNull();
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
    expect(fmtAge("2026-01-01T23:59:30Z", now, enT)).toBe("30s");
    expect(fmtAge("2026-01-01T23:30:00Z", now, enT)).toBe("30m");
    expect(fmtAge("2026-01-01T00:00:00Z", now, enT)).toBe("24h");
    expect(fmtAge("2025-12-28T00:00:00Z", now, enT)).toBe("5d");
  });

  /* s/m/h/d is English, not arithmetic (QA round 6, finding #9). The letters
     are dict/alerting.ts's own с/м/ч/д, so one age reads alike on both. */
  it("takes its unit letters from the dictionary", () => {
    const now = new Date("2026-01-02T00:00:00Z");
    expect(fmtAge("2026-01-01T23:59:30Z", now, ruT)).toBe("30 с");
    expect(fmtAge("2026-01-01T23:30:00Z", now, ruT)).toBe("30 м");
    expect(fmtAge("2026-01-01T00:00:00Z", now, ruT)).toBe("24 ч");
    expect(fmtAge("2025-12-28T00:00:00Z", now, ruT)).toBe("5 д");
  });

  it("is a dash for an unparseable instant, never NaN", () => {
    expect(fmtAge("never", new Date(), enT)).toBe("—");
  });
});

/* ── #1: a topology that answered with NO nodes is not a fleet of zero ──── */

describe("nodesTile", () => {
  const withNodes: Topology = topo;
  const empty = (over: Partial<Topology> = {}): Topology => ({
    nodes: [],
    agents: [],
    timestamp: "2026-01-01T00:00:00Z",
    ...over,
  });
  const agents = (names: string[]) => names.map((n) => ({ id: `a-${n}`, nodeName: n, podIP: "", zone: "z" }));

  it("prefers the k8s node inventory when it has one", () => {
    expect(nodesTile(withNodes, false)).toEqual({ kind: "counts", ready: 1, total: 2 });
  });

  it("counts distinct agent nodes rather than claiming 0/0", () => {
    // The repro: nodes came back null (api.ts normalizes it to []) while ten
    // agents were plainly registered.
    expect(nodesTile(empty({ agents: agents(["a", "b", "b", "c"]) }), false)).toEqual({
      kind: "noInventory",
      nodes: 3,
      source: "agents",
    });
  });

  it("falls back to the matrix's nodes when even the agent list is empty", () => {
    expect(nodesTile(empty(), false, matrix)).toEqual({ kind: "noInventory", nodes: 3, source: "matrix" });
  });

  it("says 0/0 only when NOTHING knows of a node", () => {
    expect(nodesTile(empty(), false, { ...matrix, nodes: [] })).toEqual({ kind: "counts", ready: 0, total: 0 });
  });

  it("keeps loading and unavailable apart", () => {
    expect(nodesTile(undefined, true)).toEqual({ kind: "loading" });
    expect(nodesTile(undefined, false)).toEqual({ kind: "unavailable" });
  });
});

/* ── #2: a historical fold says how bounded it was ──────────────────────── */

describe("foldBounds", () => {
  const folded = (over: Partial<Topology>): Topology => ({
    nodes: [],
    agents: [],
    timestamp: "2026-01-01T00:00:00Z",
    historical: true,
    ...over,
  });

  it("says nothing for a live snapshot, whatever counters it carries", () => {
    expect(foldBounds({ ...folded({ truncated: true }), historical: false }, enT)).toBeUndefined();
    expect(foldBounds(undefined, enT)).toBeUndefined();
  });

  it("says nothing for a fold that lost nothing", () => {
    expect(foldBounds(folded({ eventsFolded: 40, unfoldableEvents: 0, truncated: false }), enT)).toBeUndefined();
  });

  it("names a truncated window", () => {
    expect(foldBounds(folded({ truncated: true }), enT)).toBe(
      "The event window was truncated, so this reconstruction is partial.",
    );
  });

  it("counts the events it could not fold", () => {
    expect(foldBounds(folded({ unfoldableEvents: 7 }), enT)).toBe(
      "7 events carried no node detail and could not be folded in.",
    );
  });

  it("states both bounds when both bit", () => {
    const both = foldBounds(folded({ truncated: true, unfoldableEvents: 2 }), enT) ?? "";
    expect(both).toContain("truncated");
    expect(both).toContain("2 events");
  });
});

/*
 * ru One smoke pin that the Russian half is actually wired to this page; every assertion above this
 * block reads English and needs no provider.
 */

describe("OverviewPage — ru", () => {
  afterEach(() => {
    /* vitest.setup.ts backs localStorage with ONE Map per test FILE, so a
       locale left behind here would translate nothing that follows in this
       file today and everything that follows tomorrow. */
    localStorage.removeItem(LOCALE_STORAGE_KEY);
  });

  it("renders the page, the pair qualifier and the unranked caption in Russian", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    const rttOnly = {
      ...matrix,
      cells: [{ source: "a", destination: "b", failRatio: null, rttP95: 2_000_000 }],
    };
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(topo) : json(rttOnly))),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <OverviewPage />
        </LocaleProvider>
      </QueryClientProvider>,
    );

    // The caption is the LAST thing to arrive (it needs the matrix), so it is
    // what the await hangs on — the title renders before any request lands.
    expect(
      await screen.findByText(/у серии доли сбоев здесь нет ни одной выборки\..*а не выдаёт себя за норму\.$/),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Обзор", level: 1 })).toBeInTheDocument();
    expect(screen.getAllByText("TCP · плоскость pod").length).toBeGreaterThanOrEqual(3);
  });
});

/* ── M3-1: the worst-pairs rows join the golden path ──────────────────────
   A ranked problem row must open its pair card and its investigation without
   a detour through the matrix — the same two affordances a matrix cell and a
   firing-alert row already carry. */
describe("OverviewPage — worst pairs are links (M3-1)", () => {
  const stubMatrix = () =>
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(String(url).includes("/topology") ? json(topo) : json(matrix))),
    );

  it("links every pair cell to its pair card, worst first", async () => {
    stubMatrix();
    renderPage();
    const links = await screen.findAllByTestId("worst-pair-link");
    expect(links).toHaveLength(3);
    expect(links[0].getAttribute("href")).toBe("/pairs/c/a");
    expect(links[1].getAttribute("href")).toBe("/pairs/b/c");
    expect(links[2].getAttribute("href")).toBe("/pairs/b/a");
  });

  it("carries the viewed instant on the pair link, exactly as the matrix cells do", async () => {
    window.history.pushState({}, "", "/?at=2026-01-01T00:00:00Z");
    try {
      stubMatrix();
      renderPage();
      const links = await screen.findAllByTestId("worst-pair-link");
      expect(links[0].getAttribute("href") ?? "").toContain("at=");
    } finally {
      window.history.pushState({}, "", "/");
    }
  });

  it("offers an investigate link per row — the firing-alert rows' own affordance", async () => {
    stubMatrix();
    renderPage();
    const links = await screen.findAllByTestId("worst-pair-investigate");
    expect(links).toHaveLength(3);
    const href = links[0].getAttribute("href") ?? "";
    expect(href).toContain("/investigate?kind=pair");
    expect(href).toContain(encodeURIComponent("c→a"));
  });
});
