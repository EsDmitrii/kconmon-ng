import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TimeMachineProvider } from "@/lib/timemachine";
import { OverviewPage } from "./overview";

/**
 * Overview with the Time Machine engaged (QA round 1, findings #2 and #5).
 *
 * The live cases stay in pages/overview.test.tsx, untouched. What is asserted
 * here is the honesty boundary the QA pass found broken: every panel on this
 * page either resolves through `at` or SAYS it cannot, and a topology fold the
 * server refused is never rendered as an em-dash and nothing else.
 */

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

const TOPOLOGY_RETENTION_DETAIL =
  "no events are retained for that instant, so the topology cannot be reconstructed there -- pick a later time";

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

  /* The swallow itself: the page used to surface `matrix.error ?? topo.error`,
     one slot for two independent dependencies. With BOTH down the topology
     detail — the only actionable one, since it names retentionDays — was the
     one that lost the coin toss, and the tile's em-dash was all that was left
     of it. */
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
  /* The store CAN express "ongoing at t": ListIncidents' from/to bound the
     window an incident's OWN RANGE must overlap (from_at < to AND
     coalesce(to_at,'infinity') >= from), so the one-second window [t, t+1s)
     selects exactly the incidents whose range covers t. `status` is dropped
     with it — status is a NOW fact (resolved_at), and filtering on it would
     hide an incident that was ongoing at t and has since been resolved. */
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
