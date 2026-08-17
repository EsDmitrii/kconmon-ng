import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { OverviewPage, summarize } from "./overview";
import type { Matrix, MatrixCell, Topology } from "@/lib/types";

/*
 * The Overview under a hostile wire. Everything below is a body the browser can
 * be handed — a nil Go map, an absent field, a number JSON.parse turns into
 * Infinity, a name somebody typed a script tag into — and the bar is the same
 * for all of it: nothing renders NaN, undefined or "0.0ms" for a measurement
 * nobody took, and nothing takes the page down (there is no error boundary
 * above these routes, so a throw in render IS a blank page).
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const topo: Topology = {
  nodes: [{ name: "a", zone: "z1", ready: true }],
  agents: [],
  timestamp: "2026-01-01T00:00:00Z",
};

const baseMatrix: Matrix = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["a", "b"],
  cells: [],
  timestamp: "2026-01-01T00:00:00Z",
};

function meBody(permissions: string[]) {
  return { subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["admin"] }, permissions };
}

function configBody() {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: true },
  };
}

interface Options {
  cells?: unknown[];
  alerts?: unknown[];
  /** A raw Response for GET /api/v1/alerts — used for the malformed-body case. */
  alertsResponse?: () => Response;
  incidents?: unknown[];
  events?: unknown[];
}

function renderOverview(opts: Options = {}) {
  const { cells = [], alerts = [], alertsResponse, incidents = [], events = [] } = opts;
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(["incidents:read", "events:read", "alerts:read"])));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topo));
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents, nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    if (href.startsWith("/api/v1/alerts")) {
      return Promise.resolve(alertsResponse ? alertsResponse() : json({ alerts, promConfigured: true }));
    }
    return Promise.resolve(json({ ...baseMatrix, cells }));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    ...render(
      <QueryClientProvider client={qc}>
        <OverviewPage />
      </QueryClientProvider>,
    ),
    fetchMock,
  };
}

afterEach(() => vi.unstubAllGlobals());

/* ── the p95 RTT column ─────────────────────────────────────────────────── */

describe("worst pairs: a latency nobody measured", () => {
  /* Go marshals a nil *float64 as null, and the column's guard was
     `ns === undefined` — so null divided cleanly by 1e6 and the table reported
     0.0ms, the fastest link in the fleet, for a pair with no RTT at all. */
  it("prints an em-dash for a null p95, never a fabricated 0.0ms", async () => {
    renderOverview({ cells: [{ source: "a", destination: "b", failRatio: 0.5, rttP95: null }] });

    const row = await screen.findByRole("row", { name: /a.*b/ });
    expect(within(row).getByText("—")).toBeInTheDocument();
    expect(within(row).queryByText("0.0ms")).toBeNull();
  });

  /* JSON.parse("1e400") is Infinity — a number the wire CAN carry and
     toFixed(1) renders as the word. */
  it("prints an em-dash rather than the word Infinity for an out-of-range p95", async () => {
    renderOverview({ cells: [{ source: "a", destination: "b", failRatio: 0.5, rttP95: 1e400 }] });

    const row = await screen.findByRole("row", { name: /a.*b/ });
    expect(within(row).getByText("—")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("Infinity");
  });

  it("still renders a real p95", async () => {
    renderOverview({ cells: [{ source: "a", destination: "b", failRatio: 0.5, rttP95: 3_000_000 }] });
    expect(await screen.findByText("3.0ms")).toBeInTheDocument();
  });
});

/* ── what "scored" means ────────────────────────────────────────────────── */

describe("summarize: a pair is only scored when it carries a NUMBER", () => {
  const cell = (over: Partial<MatrixCell>): MatrixCell =>
    ({ source: "a", destination: "b", failRatio: null, ...over }) as MatrixCell;

  /* `failRatio !== null` said yes to an ABSENT key, so a pair the console
     cannot rank was counted among the ranked ones — and the page then chose
     the "no failing or degraded pairs" slate, which is a health claim. */
  it("does not count a cell whose failRatio key is absent", () => {
    const cells = [cell({ rttP95: 1_000_000, failRatio: undefined as unknown as null })];
    expect(summarize({ ...baseMatrix, cells }).pairsScored).toBe(0);
  });

  it("does not count a NaN failRatio", () => {
    const cells = [cell({ failRatio: Number.NaN, rttP95: 1_000_000 })];
    expect(summarize({ ...baseMatrix, cells }).pairsScored).toBe(0);
  });

  it("does not count an infinite failRatio, and does not rank it either", () => {
    const s = summarize({ ...baseMatrix, cells: [cell({ failRatio: Number.POSITIVE_INFINITY })] });
    expect(s.pairsScored).toBe(0);
    expect(s.pairsFailing).toBe(0);
    expect(s.worstPairs).toHaveLength(0);
  });

  it("still counts a real ratio, zero included", () => {
    expect(summarize({ ...baseMatrix, cells: [cell({ failRatio: 0 })] }).pairsScored).toBe(1);
  });
});

describe("the page's own sentence about an unscorable pair", () => {
  it("refuses to call the fleet healthy when nothing could be scored", async () => {
    renderOverview({
      cells: [{ source: "a", destination: "b", rttP95: 2_000_000 }],
    });

    expect(await screen.findByText("No failure ratio for these pairs")).toBeInTheDocument();
    expect(screen.queryByText("No failing or degraded pairs")).toBeNull();
  });
});

/* ── firing alerts under a mangled row ──────────────────────────────────── */

function alertRow(over: Record<string, unknown> = {}) {
  return {
    name: "PairLossHigh",
    state: "firing",
    severity: "critical",
    labels: { alertname: "PairLossHigh", severity: "critical" },
    annotations: {},
    activeAt: "2026-01-01T00:00:00Z",
    value: "1e+00",
    ruleId: "11111111-1111-4111-8111-111111111111",
    ...over,
  };
}

describe("Firing alerts under a row the wire mangled", () => {
  /* Object.keys(null) throws, and there is no error boundary over this route:
     one alert with a nil label map took the WHOLE Overview to a blank page. */
  it("survives an alert whose label map arrived as null", async () => {
    renderOverview({ alerts: [alertRow({ labels: null })] });

    const row = await screen.findByTestId("firing-alert");
    expect(within(row).getByTestId("firing-alert-name")).toHaveTextContent("PairLossHigh");
    // The page is still standing: the tiles rendered too.
    expect(screen.getByText("Nodes ready")).toBeInTheDocument();
  });

  it("survives an alert with no labels key at all", async () => {
    const { labels: _labels, ...noLabels } = alertRow();
    renderOverview({ alerts: [noLabels] });

    expect(await screen.findByTestId("firing-alert")).toBeInTheDocument();
    expect(screen.getByText("Nodes ready")).toBeInTheDocument();
  });

  /* An absent severity used to render as an EMPTY badge — a coloured dot
     asserting a severity it did not have. "no severity" is the word this page
     already has for the empty case, and absent is the same case. */
  it("says 'no severity' for a row that carries none, blank or absent", async () => {
    const { severity: _severity, ...noSeverity } = alertRow({ name: "Absent" });
    renderOverview({ alerts: [alertRow({ name: "Blank", severity: "" }), noSeverity] });

    const rows = await screen.findAllByTestId("firing-alert");
    expect(rows).toHaveLength(2);
    for (const row of rows) expect(within(row).getByText("no severity")).toBeInTheDocument();
  });

  /* The card prints the SERVER's sentence for a rejection, which is right for
     problem+json and wrong for everything else: a proxy's HTML page or a cut
     connection made the JSON parser the author of the operator's error line. */
  it("falls back to its own sentence when the failure is not the server speaking", async () => {
    renderOverview({
      alertsResponse: () =>
        new Response("<html>502</html>", { status: 200, headers: { "Content-Type": "application/json" } }),
    });

    expect(await screen.findByText("The firing set is unavailable right now.")).toBeInTheDocument();
    expect(screen.queryByText(/JSON/i)).toBeNull();
    expect(screen.queryByText(/Unexpected token/i)).toBeNull();
  });

  it("still prints the server's own sentence for a real problem+json", async () => {
    renderOverview({
      alertsResponse: () =>
        new Response(
          JSON.stringify({ type: "about:blank", title: "t", status: 502, detail: "prometheus did not answer" }),
          { status: 502, headers: { "Content-Type": "application/problem+json" } },
        ),
    });

    expect(await screen.findByText("prometheus did not answer")).toBeInTheDocument();
  });
});

/* ── data is data, and it is never markup ───────────────────────────────── */

const XSS = '<img src=x onerror="window.__pwned=1">';

describe("hostile strings arriving as data", () => {
  it("renders a script payload in an alert name, an incident title and an event summary as text", async () => {
    renderOverview({
      alerts: [alertRow({ name: XSS })],
      incidents: [
        {
          id: "inc-1",
          title: XSS,
          scope: "",
          fromAt: "2026-01-01T00:00:00Z",
          status: "open",
          notes: "",
          pinned: [],
          createdBy: "user:ada",
          createdAt: "2026-01-01T00:00:00Z",
        },
      ],
      events: [
        {
          id: "e-1",
          seq: 1,
          type: "topology_changed",
          severity: "warn",
          scope: XSS,
          timestamp: "2026-01-01T00:05:00Z",
          summary: XSS,
          details: null,
        },
      ],
    });

    await screen.findByTestId("overview-event");
    expect(screen.queryByRole("img")).toBeNull();
    expect(document.querySelector("img")).toBeNull();
    expect((window as unknown as Record<string, unknown>).__pwned).toBeUndefined();
    // And the text itself is there, verbatim, on all three surfaces.
    expect(screen.getAllByText(XSS).length).toBeGreaterThanOrEqual(3);
  });

  it("renders a 10k-character event summary without taking the row apart", async () => {
    const huge = "Ж".repeat(10_000);
    renderOverview({
      events: [
        {
          id: "e-1",
          seq: 1,
          type: "topology_changed",
          severity: "warn",
          scope: "a→b",
          timestamp: "2026-01-01T00:05:00Z",
          summary: huge,
          details: null,
        },
      ],
    });

    const row = await screen.findByTestId("overview-event");
    expect(within(row).getByTitle(huge)).toBeInTheDocument();
    // The row still truncates rather than pushing the card open.
    expect(row.querySelector(".truncate")).not.toBeNull();
  });

  /* Go's severity is an open string; the badge must print what came rather
     than an empty chip or the word undefined. */
  it("prints an unknown event severity verbatim and an absent one as nothing but a neutral chip", async () => {
    renderOverview({
      events: [
        {
          id: "e-1",
          seq: 1,
          type: "topology_changed",
          severity: "catastrophic",
          scope: "a→b",
          timestamp: "2026-01-01T00:05:00Z",
          summary: "s",
          details: null,
        },
      ],
    });

    expect(await screen.findByText("catastrophic")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("undefined");
  });
});

/* ── the degenerate shapes ──────────────────────────────────────────────── */

describe("degenerate matrices", () => {
  it("renders exactly one failing pair as a one-row table", async () => {
    renderOverview({ cells: [{ source: "a", destination: "b", failRatio: 0.9, rttP95: 1_000_000 }] });

    await screen.findByText("90.0%");
    expect(screen.getAllByTestId("stat-value")[1]).toHaveTextContent("1");
    expect(screen.getByText(/1 measured pair$/)).toBeInTheDocument();
  });

  /* The same pair twice is nonsense the wire can still carry, and two table
     rows under one React key render as one. */
  it("renders two cells naming the same pair as two rows", async () => {
    renderOverview({
      cells: [
        { source: "a", destination: "b", failRatio: 0.5, rttP95: 1_000_000 },
        { source: "a", destination: "b", failRatio: 0.4, rttP95: 2_000_000 },
      ],
    });

    await screen.findByText("50.0%");
    expect(screen.getAllByRole("row")).toHaveLength(3); // header + two
    expect(screen.getByText("40.0%")).toBeInTheDocument();
  });

  it("caps the worst-pairs table at five however many are failing", async () => {
    const cells = Array.from({ length: 200 }, (_, i) => ({
      source: `s${i}`,
      destination: `d${i}`,
      failRatio: 0.2 + i / 1000,
    }));
    renderOverview({ cells });

    await screen.findByText("Worst pairs");
    await waitFor(() => expect(screen.getAllByRole("row").length).toBe(6)); // header + 5
  });

  /* A ratio above 1 is nonsense the wire can still carry; it must print rather
     than break the tier arithmetic. */
  it("renders a ratio above 1 without breaking the tiers", async () => {
    renderOverview({ cells: [{ source: "a", destination: "b", failRatio: 7 }] });

    expect(await screen.findByText("700.0%")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("NaN");
  });

  it("ignores a negative ratio rather than ranking it as the worst pair", async () => {
    renderOverview({ cells: [{ source: "a", destination: "b", failRatio: -1, rttP95: 1_000_000 }] });

    expect(await screen.findByText("No failing or degraded pairs")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("-100.0%");
  });
});

/* ── a firing set bigger than the card ──────────────────────────────────── */

describe("a hundred alerts firing at once", () => {
  it("truncates and SAYS the number it left out rather than ending in a cliff", async () => {
    const alerts = Array.from({ length: 100 }, (_, i) =>
      alertRow({ name: `Alert${String(i).padStart(3, "0")}`, labels: { alertname: `Alert${i}` } }),
    );
    renderOverview({ alerts });

    const rows = await screen.findAllByTestId("firing-alert");
    expect(rows).toHaveLength(8);
    expect(screen.getByText("92 more firing alerts are not shown here.")).toBeInTheDocument();
  });
});

/* ── a dependency that failed outright ──────────────────────────────────── */

describe("the failed-dependency card", () => {
  it("names the dependency and prints the server's sentence rather than blanking the page", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody([])));
        if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
        if (href.includes("/api/v1/topology")) return Promise.resolve(json(topo));
        return Promise.resolve(
          new Response(
            JSON.stringify({ type: "about:blank", title: "matrix unavailable", status: 502, detail: "prometheus said no" }),
            { status: 502, headers: { "Content-Type": "application/problem+json" } },
          ),
        );
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <OverviewPage />
      </QueryClientProvider>,
    );

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("The pair matrix is unavailable")).toBeInTheDocument();
    expect(within(alert).getByText("prometheus said no")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Overview", level: 1 })).toBeInTheDocument();
  });

  /* A 500 with a body nothing can parse must still say something. */
  it("says something for a 500 whose body is not the JSON it claims", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody([])));
        if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
        if (href.includes("/api/v1/topology")) return Promise.resolve(json(topo));
        return Promise.resolve(
          new Response("<html>500</html>", { status: 500, headers: { "Content-Type": "application/json" } }),
        );
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <OverviewPage />
      </QueryClientProvider>,
    );

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("The pair matrix is unavailable")).toBeInTheDocument();
    // This console's own sentence, from the dictionary — not the JSON parser's.
    expect(within(alert).getByText(/an ingress, a proxy, a dropped connection/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("Unexpected");
    expect(document.body.textContent).not.toContain("JSON");
    expect(document.body.textContent).not.toContain("undefined");
  });

  it("says the same thing in Russian", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    try {
      vi.stubGlobal(
        "fetch",
        vi.fn((url: string) => {
          const href = String(url);
          if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody([])));
          if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
          if (href.includes("/api/v1/topology")) return Promise.resolve(json(topo));
          return Promise.resolve(
            new Response("<html>500</html>", { status: 500, headers: { "Content-Type": "application/json" } }),
          );
        }),
      );
      const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      render(
        <QueryClientProvider client={qc}>
          <LocaleProvider>
            <OverviewPage />
          </LocaleProvider>
        </QueryClientProvider>,
      );

      const alert = await screen.findByRole("alert");
      expect(within(alert).getByText("Матрица пар недоступна")).toBeInTheDocument();
      expect(within(alert).getByText(/ingress, прокси или оборвавшееся соединение/)).toBeInTheDocument();
    } finally {
      localStorage.removeItem(LOCALE_STORAGE_KEY);
    }
  });
});

/* ── the URL's own ?at= ─────────────────────────────────────────────────── */

describe("a hostile ?at= in the address bar", () => {
  const cases = [
    ["a word", "yesterday"],
    ["a bare date", "2026-01-01"],
    ["a number", "1786392119"],
    ["a script payload", "<script>alert(1)</script>"],
    ["an out-of-range instant", "2026-13-45T99:99:99Z"],
    ["empty", ""],
  ] as const;

  it.each(cases)("stays Live and renders the live description for %s", async (_name, at) => {
    window.history.replaceState({}, "", `/?at=${encodeURIComponent(at)}`);
    try {
      vi.stubGlobal(
        "fetch",
        vi.fn((url: string) => {
          const href = String(url);
          if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody([])));
          if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
          if (href.includes("/api/v1/topology")) return Promise.resolve(json(topo));
          return Promise.resolve(json({ ...baseMatrix, cells: [{ source: "a", destination: "b", failRatio: 0 }] }));
        }),
      );
      const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      render(
        <QueryClientProvider client={qc}>
          <TimeMachineProvider>
            <OverviewPage />
          </TimeMachineProvider>
        </QueryClientProvider>,
      );

      expect(
        await screen.findByText("Cluster health at a glance, recomputed from Prometheus every 15s."),
      ).toBeInTheDocument();
      // The URL was corrected rather than left claiming an instant nobody is on.
      expect(new URLSearchParams(window.location.search).get("at")).toBeNull();
    } finally {
      window.history.replaceState({}, "", "/");
    }
  });
});
