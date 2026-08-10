import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TimeMachineProvider } from "@/lib/timemachine";
import { OverviewPage } from "./overview";

/** Overview with the Time Machine engaged. */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** The instant-vector body GET-at's matrix is folded from (lib/matrix-promql). */
function vector(pairs: { src: string; dst: string; value: string }[]) {
  return {
    status: "success",
    data: {
      resultType: "vector",
      result: pairs.map((p) => ({
        metric: { source_node: p.src, destination_node: p.dst },
        value: [1_785_276_000, p.value],
      })),
    },
  };
}

/* The server's own 422 sentence, verbatim (internal/console/httpapi/data.go). */
const TOPOLOGY_RETENTION_DETAIL =
  "no events are retained for that instant, so the topology cannot be reconstructed there. Pick a later time, " +
  "or raise console.database.retentionDays to keep more history in future";

function meBody(permissions: string[]) {
  return { subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["viewer"] }, permissions };
}

const CONFIG = {
  auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
  anonymousBanner: false,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: true },
};

const ALL_READS = ["incidents:read", "events:read", "alerts:read"];

interface Options {
  permissions?: string[];
  /** Replaces the 200 GET /api/v1/topology?at= would answer with. */
  topologyResponse?: () => Response;
  /** Replaces the 200 POST /api/v1/promql/query would answer with — the
   *  engaged matrix's only source (lib/matrix-promql.ts). */
  promqlResponse?: () => Response;
  incidents?: unknown[];
  events?: unknown[];
  /** false renders the page live, for the side-by-side comparisons. */
  engaged?: boolean;
}

function renderOverview(opts: Options = {}) {
  const {
    permissions = ALL_READS,
    topologyResponse,
    promqlResponse,
    incidents = [],
    events = [],
    engaged = true,
  } = opts;
  const urls: string[] = [];
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    urls.push(href);
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(CONFIG));
    if (href.includes("/api/v1/topology")) {
      return Promise.resolve(
        topologyResponse
          ? topologyResponse()
          : json({ nodes: [{ name: "a", zone: "z", ready: true }], agents: [], timestamp: AT }),
      );
    }
    if (href.startsWith("/api/v1/promql")) {
      return Promise.resolve(promqlResponse ? promqlResponse() : json(vector([{ src: "a", dst: "b", value: "0.2" }])));
    }
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents, nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    if (href.startsWith("/api/v1/alerts")) return Promise.resolve(json({ alerts: [], promConfigured: true }));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  // `?at=` is the only way the app engages the Time Machine, so the tests
  // engage it the same way rather than by faking the context.
  window.history.pushState({}, "", engaged ? `/?at=${AT}` : "/");

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        <OverviewPage />
      </TimeMachineProvider>
    </QueryClientProvider>,
  );
  return { ...utils, urls };
}

beforeEach(() => {
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

/* ── #5: a refused topology fold is a stated problem, not an em-dash ────── */

describe("OverviewPage engaged — a 422 from the topology fold", () => {
  it("renders the server's retention detail in the page's error surface", async () => {
    renderOverview({
      topologyResponse: () => problem(422, "topology not retained", TOPOLOGY_RETENTION_DETAIL),
    });

    expect(await screen.findByText(TOPOLOGY_RETENTION_DETAIL)).toBeInTheDocument();
  });

  /* With BOTH down the topology detail — the only actionable one, since it names retentionDays. */
  it("states BOTH failures when the matrix is down too, rather than one of them", async () => {
    const { urls } = renderOverview({
      topologyResponse: () => problem(422, "topology not retained", TOPOLOGY_RETENTION_DETAIL),
      promqlResponse: () => problem(502, "prometheus unavailable", "prometheus did not answer the instant query"),
    });
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/promql"))).toBe(true));

    expect(await screen.findByText(TOPOLOGY_RETENTION_DETAIL)).toBeInTheDocument();
    expect(await screen.findByText("prometheus did not answer the instant query")).toBeInTheDocument();
  });

  it("keeps the rest of the page — the matrix answered, so its tiles still render", async () => {
    renderOverview({
      topologyResponse: () => problem(422, "topology not retained", TOPOLOGY_RETENTION_DETAIL),
    });

    await screen.findByText(TOPOLOGY_RETENTION_DETAIL);
    // NODES READY has genuinely nothing to say and keeps its em-dash; the
    // failing-pairs tile is matrix-fed and is unaffected.
    expect(screen.getByText("Nodes ready")).toBeInTheDocument();
    expect(screen.getByText("Failing pairs")).toBeInTheDocument();
  });
});

/* ── #2: the three summary panels resolve through `at`, or say they cannot ─ */

describe("OverviewPage engaged — Recent events", () => {
  it("bounds the panel with to=t rather than showing events from after it", async () => {
    const { urls } = renderOverview();
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(true));

    const call = new URLSearchParams((urls.find((u) => u.startsWith("/api/v1/events")) ?? "").split("?")[1] ?? "");
    expect(call.get("to")).toBe(new Date(AT).toISOString());
  });

  it("sends no bound while live", async () => {
    const { urls } = renderOverview({ engaged: false });
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(true));

    const call = new URLSearchParams((urls.find((u) => u.startsWith("/api/v1/events")) ?? "").split("?")[1] ?? "");
    expect(call.get("to")).toBeNull();
  });
});

describe("OverviewPage engaged — Open incidents", () => {
  /*
   * The store CAN express "ongoing at t": ListIncidents' from/to bound the window an incident's OWN
   * RANGE must overlap (from_at < to AND coalesce(to_at,'infinity') >= from).
   */
  it("asks for the incidents whose range covers t, not the ones open now", async () => {
    const { urls } = renderOverview();
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(true));

    const call = new URLSearchParams((urls.find((u) => u.startsWith("/api/v1/incidents")) ?? "").split("?")[1] ?? "");
    expect(call.get("from")).toBe(new Date(AT).toISOString());
    expect(call.get("to")).toBe(new Date(Date.parse(AT) + 1000).toISOString());
    expect(call.get("status")).toBeNull();
  });

  it("still asks for the open ones while live", async () => {
    const { urls } = renderOverview({ engaged: false });
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(true));

    const call = new URLSearchParams((urls.find((u) => u.startsWith("/api/v1/incidents")) ?? "").split("?")[1] ?? "");
    expect(call.get("status")).toBe("open");
    expect(call.get("from")).toBeNull();
  });
});

/* ── round 6: the page must stop speaking in the present tense ──────────── */

describe("OverviewPage engaged — the page's own subtitle (#6)", () => {
  it("does not promise a 15s refresh while every poll on the page is off", async () => {
    renderOverview();
    expect(
      await screen.findByText("Cluster health at the instant you are viewing. Nothing here refreshes."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/recomputed from Prometheus every 15s/)).toBeNull();
  });

  it("says the live sentence again once the console returns to Live", async () => {
    renderOverview({ engaged: false });
    expect(
      await screen.findByText("Cluster health at a glance, recomputed from Prometheus every 15s."),
    ).toBeInTheDocument();
  });
});

describe("OverviewPage engaged — the nodes tile discloses the fold's bounds (#2)", () => {
  const folded = (over: Record<string, unknown>) =>
    json({
      nodes: [
        { name: "a", zone: "z", ready: true },
        { name: "b", zone: "z", ready: true },
      ],
      agents: [],
      timestamp: AT,
      historical: true,
      asOf: AT,
      eventsFolded: 12,
      ...over,
    });

  it("says the window was truncated instead of presenting 2/2 as the whole fleet", async () => {
    renderOverview({ topologyResponse: () => folded({ truncated: true, unfoldableEvents: 0 }) });

    expect(await screen.findByText("2/2")).toBeInTheDocument();
    expect(screen.getByTestId("tile-note")).toHaveTextContent(
      "The event window was truncated, so this reconstruction is partial.",
    );
  });

  it("counts the events that could not be folded", async () => {
    renderOverview({ topologyResponse: () => folded({ truncated: false, unfoldableEvents: 4 }) });

    expect(await screen.findByText("2/2")).toBeInTheDocument();
    expect(screen.getByTestId("tile-note")).toHaveTextContent(
      "4 events carried no node detail and could not be folded in.",
    );
  });

  it("stays quiet when the fold lost nothing", async () => {
    renderOverview({ topologyResponse: () => folded({ truncated: false, unfoldableEvents: 0 }) });

    expect(await screen.findByText("2/2")).toBeInTheDocument();
    expect(screen.queryByTestId("tile-note")).toBeNull();
  });
});

describe("OverviewPage engaged — an incident's age is measured from t (#3)", () => {
  it("ages the row against the viewed instant, not the wall clock", async () => {
    renderOverview({
      incidents: [
        {
          id: "inc-1",
          title: "Loss between node-a and node-b",
          scope: "",
          fromAt: "2026-08-01T11:30:00Z", // half an hour before AT
          status: "open",
          notes: "",
          pinned: [],
          createdBy: "user:ada",
          createdAt: "2026-08-01T11:30:00Z",
        },
      ],
    });

    const row = await screen.findByTestId("open-incident");
    expect(within(row).getByText("30m")).toBeInTheDocument();
  });
});

describe("OverviewPage engaged — the empty states drop the live framing (#4)", () => {
  /* An instant-vector with no samples: measured pairs = 0. */
  const noSamples = () => json({ status: "success", data: { resultType: "vector", result: [] } });

  it("does not tell a past instant to wait for the DaemonSet", async () => {
    renderOverview({ promqlResponse: noSamples });

    expect(await screen.findByText("No probe data at this instant")).toBeInTheDocument();
    expect(screen.queryByText(/within a minute of the DaemonSet becoming ready/)).toBeNull();
  });

  it("does not claim nothing has ever happened when it asked for events up to t", async () => {
    renderOverview({ events: [] });

    expect(
      await screen.findByText(
        "No events at or before the instant you are viewing. Return to Live, or pick another instant.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Nothing has happened yet/)).toBeNull();
  });

  it("keeps the live wording while live", async () => {
    renderOverview({ engaged: false, promqlResponse: noSamples });

    expect(await screen.findByText("No probe data in Prometheus yet")).toBeInTheDocument();
    expect(await screen.findByText(/Nothing has happened yet/)).toBeInTheDocument();
  });
});

/* ── #5: zero over zero, under the Time Machine as well ─────────────────── */

describe("OverviewPage engaged — the pair tiles at zero measured pairs", () => {
  it("renders an em-dash with the no-data note rather than a confident 0", async () => {
    renderOverview({ promqlResponse: () => json({ status: "success", data: { resultType: "vector", result: [] } }) });

    await screen.findByText("No probe data at this instant");
    const values = screen.getAllByTestId("stat-value");
    expect(values[1]).toHaveTextContent("—");
    expect(values[2]).toHaveTextContent("—");
    expect(screen.getAllByTestId("tile-note")[0]).toHaveTextContent(
      "No pair was measured here, so there is nothing to count.",
    );
  });
});

describe("OverviewPage engaged — Firing alerts", () => {
  it("says the firing set is live-only instead of showing now's alerts under a past instant", async () => {
    renderOverview();
    expect(
      await screen.findByText(/Alert state is a live-only signal — Prometheus keeps no firing history here\./),
    ).toBeInTheDocument();
  });

  it("issues ZERO requests to /api/v1/alerts while engaged", async () => {
    const { urls } = renderOverview();
    await screen.findByText(/Alert state is a live-only signal/);
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/alerts"))).toBe(false);
  });
});
