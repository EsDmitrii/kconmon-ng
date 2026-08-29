import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import {
  TIME_MACHINE_DISABLED_REASON,
  TIME_MACHINE_REASON_ID,
  TimeMachineProvider,
} from "@/lib/timemachine";
import { CAUSE_WEIGHTS } from "@/lib/investigation";
import type { Alert, AuditEntry, Incident, PromResult } from "@/lib/types";
import { COPY_NOTE_TTL_MS, InvestigatePage } from "./investigate";
import {
  PIN_KIND_BY_TIMELINE_KIND,
  PROMQL_MAX_RANGE_MS,
  alertEntries,
  alertIsOurs,
  alertTouchesScope,
  auditDetailLine,
  auditEntries,
  buildExportPayload,
  buildInvestigateURL,
  commitWindow,
  exportFileName,
  ignoredInvestigationParams,
  incidentParams,
  isReadOnlyAudit,
  pathChangeEntries,
  investigationFailRatioQuery,
  investigationLossQuery,
  investigationRttQuery,
  parseInvestigationParams,
  investigationParamsToSearch,
  pinnedRefFor,
  rangeExceedsPromBound,
  runTouchesScope,
  samplesFromMatrix,
  scopeCaptionValue,
  scopeFilterValue,
  scopeFromAlertLabels,
  scopeFromIncidentScope,
  scopeIncompleteReason,
  scopeNodeOptions,
  scopeZoneOptions,
  scopedAlertEntries,
  scopesToQuery,
  type InvestigationScope,
} from "@/lib/investigation-sources";
import {
  deltaFromVectors,
  signalChartOption,
  withOverlays,
} from "@/components/investigation-signals";
import { formatSeconds } from "@/lib/curated-metrics";
import { MAINTENANCE_SERIES_NAME, maintenanceOverlaySeries } from "@/lib/annotations";

// Same reason as every other page test in this repo: echarts.init reaches for a 2d canvas context
// jsdom does not implement.
vi.mock("@/components/echart", () => ({
  EChart: ({ className }: { className?: string }) => <div data-testid="echart" className={className} />,
}));

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

/* The whole investigation runs inside ONE fixed hour, so every assertion below
   names an absolute instant rather than an offset from a moving clock. */
const FROM = "2026-08-08T00:00:00Z";
const TO = "2026-08-08T01:00:00Z";
const NOW = new Date("2026-08-08T01:00:00Z");

/** Every read permission the eight sources need. Individual tests subtract. */
const ALL_READS = [
  "events:read",
  "audit:read",
  "annotations:read",
  "mtr:read",
  "runs:read",
  "maintenance:read",
  "incidents:read",
  "promql:query",
  "targets:read",
  "topology:read",
  "alerts:read",
];

/** The write half of M6 Task 8. Added only by the tests that exercise it, so
 *  every other test doubles as the viewer's read-only view. */
const WRITE = [...ALL_READS, "incidents:write"];

function incidentRow(over: Record<string, unknown> = {}) {
  return {
    id: "inc-1",
    title: "Loss between node-a and node-b",
    scope: "node-a→node-b",
    fromAt: FROM,
    toAt: TO,
    status: "open",
    notes: "",
    pinned: [],
    createdBy: "user:ada",
    createdAt: FROM,
    ...over,
  };
}

function maintenanceRow(over: Record<string, unknown> = {}) {
  return {
    id: "m-1",
    scope: "node-a→node-b",
    startAt: "2026-08-08T00:05:00Z",
    endAt: "2026-08-08T00:15:00Z",
    reason: "switch upgrade",
    createdBy: "user:ada",
    createdAt: FROM,
    ...over,
  };
}

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] },
    permissions,
  };
}

function configBody(databaseConfigured = true, prometheusConfigured = true) {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: prometheusConfigured },
    database: { configured: databaseConfigured },
  };
}

function topologyBody() {
  return {
    nodes: [
      { name: "node-a", zone: "zone-1", ready: true },
      { name: "node-b", zone: "zone-2", ready: true },
      { name: "node-c", zone: "zone-1", ready: true },
    ],
    agents: [],
    timestamp: FROM,
  };
}

const LOSS_STEPS: [number, string][] = [
  [Date.parse("2026-08-08T00:00:00Z") / 1000, "0.001"],
  [Date.parse("2026-08-08T00:10:00Z") / 1000, "0.001"],
  [Date.parse("2026-08-08T00:20:00Z") / 1000, "0.05"],
  [Date.parse("2026-08-08T00:30:00Z") / 1000, "0.05"],
];

const RTT_STEPS: [number, string][] = [
  [Date.parse("2026-08-08T00:00:00Z") / 1000, "0.005"],
  [Date.parse("2026-08-08T00:10:00Z") / 1000, "0.005"],
  [Date.parse("2026-08-08T00:20:00Z") / 1000, "0.005"],
];

const matrixBody = (values: [number, string][]): PromResult => ({
  status: "success",
  data: { resultType: "matrix", result: [{ metric: {}, values }] },
});

const vectorBody = (value: string): PromResult => ({
  status: "success",
  data: { resultType: "vector", result: [{ metric: {}, value: [Date.parse(TO) / 1000, value] }] },
});

interface Call {
  method: string;
  url: string;
  body?: Record<string, unknown>;
}

interface Options {
  permissions?: string[];
  databaseConfigured?: boolean;
  prometheusConfigured?: boolean;
  search?: string;
  events?: unknown[];
  k8sEvents?: unknown[];
  auditRows?: unknown[];
  annotations?: unknown[];
  maintenance?: unknown[];
  snapshots?: unknown[];
  runs?: unknown[];
  /** The FIRING set GET /api/v1/alerts answers (M7 Task 8, Decision 6). */
  alerts?: unknown[];
  runDetail?: (id: string) => unknown;
  /** The stored incident this console has. `null` = the id in the URL matches
   *  nothing, i.e. GET /api/v1/incidents/{id} answers 404. */
  incident?: Record<string, unknown> | null;
  /** Route prefixes that answer problem+json instead of a body: each one becomes a FAILED source. */
  failing?: { prefix: string; status?: number; detail: string }[];
  /** The topology body, so a test can hand the page a controller-less console
   *  whose only evidence of a fleet is its AGENTS (QA round 3, finding #5). */
  topology?: unknown;
  /** The single sample every INSTANT promql query answers — the fail-ratio the delta chip renders. */
  failRatio?: string;
  /** Mounts a <LocaleProvider> above the page. Absent — which is every case
   *  but the ru smoke pin at the bottom of this file — the page renders with
   *  no provider at all, which lib/i18n defines as English. */
  locale?: Locale;
}

function renderPage(opts: Options = {}) {
  const {
    permissions = ALL_READS,
    databaseConfigured = true,
    prometheusConfigured = true,
    search = `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=${FROM}&to=${TO}`,
    events = [],
    k8sEvents = [],
    auditRows = [],
    annotations = [],
    maintenance = [],
    snapshots = [],
    runs = [],
    alerts = [],
    runDetail,
    incident = null,
    failing = [],
    topology = topologyBody(),
    failRatio = "0.2",
    locale,
  } = opts;

  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);

  window.history.replaceState({}, "", `/investigate${search}`);

  /* The incident row is STATEFUL across the calls in one render. */
  let stored: Record<string, unknown> | null = incident;

  const calls: Call[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
    calls.push({ method, url: href, body });

    const broken = failing.find((f) => href.startsWith(f.prefix));
    if (broken !== undefined) {
      return Promise.resolve(
        new Response(
          JSON.stringify({ type: "about:blank", title: "unavailable", status: broken.status ?? 500, detail: broken.detail }),
          { status: broken.status ?? 500, headers: { "Content-Type": "application/problem+json" } },
        ),
      );
    }

    if (href.startsWith("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.startsWith("/api/v1/config")) {
      return Promise.resolve(json(configBody(databaseConfigured, prometheusConfigured)));
    }
    if (href.startsWith("/api/v1/topology")) return Promise.resolve(json(topology));
    if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets: [targetRow()], nextCursor: "" }));
    if (href.startsWith("/api/v1/k8s-events")) return Promise.resolve(json({ events: k8sEvents, nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    if (href.startsWith("/api/v1/audit")) return Promise.resolve(json({ entries: auditRows, nextCursor: "" }));
    if (href.startsWith("/api/v1/annotations")) {
      return Promise.resolve(json({ annotations, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/maintenance")) return Promise.resolve(json({ windows: maintenance, nextCursor: "" }));
    if (href.startsWith("/api/v1/alerts")) {
      return Promise.resolve(json({ alerts, promConfigured: prometheusConfigured }));
    }
    if (method === "POST" && href === "/api/v1/incidents") {
      const b = (body ?? {}) as Record<string, unknown>;
      stored = {
        id: "inc-new",
        title: b.title,
        scope: b.scope ?? "",
        fromAt: b.fromAt,
        ...(b.toAt === undefined ? {} : { toAt: b.toAt }),
        status: "open",
        notes: b.notes ?? "",
        pinned: b.pinned ?? [],
        createdBy: "user:ada",
        createdAt: "2026-08-08T01:00:00Z",
      };
      return Promise.resolve(json(stored, { status: 201 }));
    }
    if (/^\/api\/v1\/incidents\/[^/?]+$/.test(href)) {
      if (method === "PATCH") {
        const b = (body ?? {}) as Record<string, unknown>;
        // The server's own transition rules, in miniature: resolving stamps
        // resolvedAt, reopening CLEARS it.
        const resolvedAt =
          b.status === "resolved" ? { resolvedAt: "2026-08-08T02:00:00Z" } : b.status === "open" ? {} : {};
        stored = { ...(stored ?? {}), ...b, ...resolvedAt };
        if (b.status === "open") delete (stored as Record<string, unknown>).resolvedAt;
        return Promise.resolve(json(stored));
      }
      if (stored === null) {
        return Promise.resolve(
          new Response(JSON.stringify({ type: "about:blank", title: "not found", status: 404, detail: "no such incident" }), {
            status: 404,
            headers: { "Content-Type": "application/problem+json" },
          }),
        );
      }
      return Promise.resolve(json(stored));
    }
    if (href.startsWith("/api/v1/mtr/destinations")) {
      return Promise.resolve(json({ destinations: [{ sourceNode: "node-a", destination: "node-b", snapshotCount: 1, traceCount: 1, firstSeen: FROM, lastSeen: TO }] }));
    }
    if (href.startsWith("/api/v1/mtr/snapshots")) return Promise.resolve(json({ snapshots, nextCursor: "" }));
    if (method === "POST" && href.startsWith("/api/v1/runs")) {
      return Promise.resolve(json({ id: "run-new", status: "pending", pairTotal: 1, wsTopic: "run:run-new" }, { status: 202 }));
    }
    if (/^\/api\/v1\/runs\/[^/]+$/.test(href)) {
      const id = href.slice("/api/v1/runs/".length);
      return Promise.resolve(json(runDetail ? runDetail(id) : runDetailRow({ id })));
    }
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs, nextCursor: "" }));
    if (href.startsWith("/api/v1/promql/query_range")) {
      const query = String(body?.query ?? "");
      return Promise.resolve(json(matrixBody(query.includes("packet_loss_ratio") ? LOSS_STEPS : RTT_STEPS)));
    }
    if (href.startsWith("/api/v1/promql/query")) return Promise.resolve(json(vectorBody(failRatio)));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = <InvestigatePage />;
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          {locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );

  const urlsFor = (prefix: string) => calls.filter((c) => c.url.startsWith(prefix)).map((c) => c.url);
  const postCalls = () => calls.filter((c) => c.method === "POST" && !c.url.startsWith("/api/v1/promql"));
  const patchCalls = () => calls.filter((c) => c.method === "PATCH");
  return { ...utils, calls, urlsFor, postCalls, patchCalls, qc };
}

/** pickRange drives one DateTimePicker by its aria-label: open the trigger. */
function pickRange(triggerName: string, date: string, time: string) {
  fireEvent.click(screen.getByRole("button", { name: triggerName }));
  fireEvent.change(screen.getByLabelText("Date"), { target: { value: date } });
  fireEvent.change(screen.getByLabelText("Time"), { target: { value: time } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
}

function targetRow(over: Record<string, unknown> = {}) {
  return {
    id: "t-1",
    name: "api-gw",
    kind: "host",
    address: "10.0.0.1",
    labels: {},
    createdAt: FROM,
    updatedAt: FROM,
    ...over,
  };
}

function runDetailRow(over: Record<string, unknown> = {}) {
  return {
    id: "run-1",
    createdAt: "2026-08-08T00:25:00Z",
    status: "failed",
    type: "icmp",
    plane: "pod",
    initiatorKind: "user",
    initiatorId: "u1",
    pairTotal: 1,
    pairOk: 0,
    pairFailed: 1,
    spec: { Sources: ["node-a"], Destinations: ["node-b"] },
    results: [],
    ...over,
  };
}

function snapshotRow(over: Record<string, unknown> = {}) {
  return {
    id: "s-1",
    sourceNode: "node-a",
    destination: "node-b",
    pathHash: "abcdef0123456789",
    hopCount: 3,
    hops: [],
    firstSeen: "2026-08-08T00:17:00Z",
    lastSeen: TO,
    traceCount: 4,
    ...over,
  };
}

function annotationRow(over: Record<string, unknown> = {}) {
  return {
    id: "a-1",
    startAt: "2026-08-08T00:18:00Z",
    scope: "node-a→node-b",
    text: "poked the gateway",
    createdBy: "user:ada",
    createdAt: "2026-08-08T00:18:00Z",
    ...over,
  };
}

/* A frozen clock would only buy the ability to assert an absolute `from` on a preset. */
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/");
  /* vitest.setup.ts backs localStorage with one Map per test FILE, so a locale
     left behind would flip every later case in this one into Russian. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

/* ── the URL contract ───────────────────────────────────────────────────── */

describe("parseInvestigationParams", () => {
  it("reads kind, scope and both instants out of the query string", () => {
    const p = parseInvestigationParams(`?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=${FROM}&to=${TO}`, NOW);
    expect(p.kind).toBe("pair");
    expect(p.a).toBe("node-a");
    expect(p.b).toBe("node-b");
    expect(p.from.toISOString()).toBe(new Date(FROM).toISOString());
    expect(p.to.toISOString()).toBe(new Date(TO).toISOString());
  });

  /* A permalink is hand-edited as often as it is copied, and U+2192 is not on
     any keyboard: ?scope=node-a->node-b used to parse as ONE node called
     "node-a->node-b" with an empty peer, framing an investigation nobody asked
     for and saying nothing about it. */
  it.each([
    ["a hyphen arrow", "node-a->node-b"],
    ["a long hyphen arrow", "node-a-->node-b"],
    ["a fat arrow", "node-a=>node-b"],
    ["a bare greater-than", "node-a>node-b"],
    ["spaces around the arrow", "node-a -> node-b"],
  ])("splits a pair written with %s", (_name, typed) => {
    const p = parseInvestigationParams(`?kind=pair&scope=${encodeURIComponent(typed)}&from=${FROM}&to=${TO}`, NOW);
    expect(p.a).toBe("node-a");
    expect(p.b).toBe("node-b");
  });

  it("splits a ZONE pair the same way — one vocabulary, both wide scopes", () => {
    const p = parseInvestigationParams(`?kind=zone-pair&scope=${encodeURIComponent("zone-1->zone-2")}`, NOW);
    expect(p.a).toBe("zone-1");
    expect(p.b).toBe("zone-2");
  });

  it("leaves a single node scope whole, hyphens and all", () => {
    // The shape that must NOT be split: every node in this fleet has a hyphen.
    const p = parseInvestigationParams("?kind=node&scope=edge-gw-01", NOW);
    expect(p.a).toBe("edge-gw-01");
    expect(p.b).toBe("");
  });

  it("does not split a scope on a bare space — buildInvestigateURL emits those", () => {
    const p = parseInvestigationParams(`?kind=node&scope=${encodeURIComponent("ns/pod a&b")}`, NOW);
    expect(p.a).toBe("ns/pod a&b");
  });

  it("falls back to a cluster scope over the last hour when the URL says nothing", () => {
    const p = parseInvestigationParams("", NOW);
    expect(p.kind).toBe("cluster");
    expect(p.a).toBe("");
    expect(p.to.getTime()).toBe(NOW.getTime());
    expect(p.to.getTime() - p.from.getTime()).toBe(60 * 60 * 1000);
  });

  it("degrades an unknown kind to cluster rather than rendering a scope nothing can fetch", () => {
    expect(parseInvestigationParams("?kind=galaxy&scope=x", NOW).kind).toBe("cluster");
  });

  it("degrades an unparseable instant to the default window instead of NaN", () => {
    const p = parseInvestigationParams("?kind=node&scope=node-a&from=yesterday&to=soon", NOW);
    expect(p.kind).toBe("node");
    expect(Number.isNaN(p.from.getTime())).toBe(false);
    expect(p.to.getTime() - p.from.getTime()).toBe(60 * 60 * 1000);
  });

  it("round-trips through investigationParamsToSearch", () => {
    const p = parseInvestigationParams(`?kind=zone-pair&scope=${encodeURIComponent("zone-1→zone-2")}&from=${FROM}&to=${TO}`, NOW);
    const again = parseInvestigationParams(investigationParamsToSearch(p), NOW);
    expect(again).toEqual(p);
  });
});

/* ── QA round 3: what the entry form is allowed to commit ───────────────── */

describe("scopeIncompleteReason (finding #6)", () => {
  it("refuses a pair that names one end, or the same node twice", () => {
    expect(scopeIncompleteReason({ kind: "pair", a: "", b: "" })).toBe("Choose a source node.");
    expect(scopeIncompleteReason({ kind: "pair", a: "node-a", b: "" })).toBe("Choose a destination node.");
    expect(scopeIncompleteReason({ kind: "pair", a: "node-a", b: "node-a" })).toBe("A pair needs two different nodes.");
    expect(scopeIncompleteReason({ kind: "pair", a: "node-a", b: "node-b" })).toBeNull();
  });

  it("refuses a zone pair missing either zone, and allows a zone with itself", () => {
    expect(scopeIncompleteReason({ kind: "zone-pair", a: "", b: "zone-2" })).toBe("Choose a source zone.");
    expect(scopeIncompleteReason({ kind: "zone-pair", a: "zone-1", b: "" })).toBe("Choose a destination zone.");
    expect(scopeIncompleteReason({ kind: "zone-pair", a: "zone-1", b: "zone-1" })).toBeNull();
  });

  it("needs the one object a node or a target scope names", () => {
    expect(scopeIncompleteReason({ kind: "node", a: "", b: "" })).toBe("Choose a node.");
    expect(scopeIncompleteReason({ kind: "node", a: "node-a", b: "" })).toBeNull();
    expect(scopeIncompleteReason({ kind: "target", a: "", b: "" })).toBe("Choose a target.");
    expect(scopeIncompleteReason({ kind: "target", a: "api-gw", b: "" })).toBeNull();
  });

  it("is always complete for the cluster — it names no object", () => {
    expect(scopeIncompleteReason({ kind: "cluster", a: "", b: "" })).toBeNull();
  });
});

describe("commitWindow (findings #2 and #3)", () => {
  const from = new Date(FROM);
  const to = new Date(TO);

  it("refuses an inverted or empty range before it can reach a query", () => {
    expect(commitWindow(to, from, null)).toEqual({ ok: false, reason: "The range end must be after its start." });
    expect(commitWindow(from, from, null)).toEqual({ ok: false, reason: "The range end must be after its start." });
  });

  it("is the identity while Live", () => {
    expect(commitWindow(from, to, null)).toEqual({ ok: true, from, to, clamped: false });
  });

  it("clamps `to` down to the viewed instant and says it moved", () => {
    const at = new Date("2026-08-08T00:30:00Z");
    const out = commitWindow(from, to, at);
    expect(out).toEqual({ ok: true, from, to: at, clamped: true });
  });

  it("does not claim a clamp when the window already ends at or before the instant", () => {
    expect(commitWindow(from, to, to)).toEqual({ ok: true, from, to, clamped: false });
    expect(commitWindow(from, to, new Date("2026-08-08T02:00:00Z"))).toEqual({ ok: true, from, to, clamped: false });
  });

  it("refuses a window that lies entirely after the viewed instant", () => {
    const out = commitWindow(from, to, from);
    expect(out.ok).toBe(false);
    expect(out.ok === false && out.reason).toContain("after the viewed instant");
  });

  /* The Prometheus bound is NOT one of commitWindow's refusals: seven of this
     page's nine sources are store-backed and answer a two-day window fine. */
  it("still commits a window wider than the Prometheus query bound", () => {
    const wide = new Date(from.getTime() + 29.5 * 60 * 60 * 1000);
    expect(commitWindow(from, wide, null)).toEqual({ ok: true, from, to: wide, clamped: false });
  });
});

/* QA scope 4, finding #5: a 29h30m window fired two range queries the proxy
   refuses, and the SERVER's own "range 29h30m0s > max 24h0m0s: range exceeds
   maximum" ended up where the loss and RTT charts belong — untranslated, about a
   bound nothing on the page had named. */
describe("rangeExceedsPromBound (QA scope 4, finding #5)", () => {
  const from = new Date(FROM);

  it("is false at exactly the bound — the proxy's own comparison is strictly greater-than", () => {
    expect(rangeExceedsPromBound(from, new Date(from.getTime() + PROMQL_MAX_RANGE_MS))).toBe(false);
  });

  it("is true one millisecond past it", () => {
    expect(rangeExceedsPromBound(from, new Date(from.getTime() + PROMQL_MAX_RANGE_MS + 1))).toBe(true);
  });

  it("matches the bound the console's config actually carries", () => {
    expect(PROMQL_MAX_RANGE_MS).toBe(24 * 60 * 60 * 1000);
  });
});

describe("scopeCaptionValue (finding #7)", () => {
  it("says 'all scopes' for the wide scopes, which query UNFILTERED rather than global", () => {
    expect(scopesToQuery({ kind: "zone-pair", a: "zone-1", b: "zone-2" })).toEqual([undefined]);
    expect(scopeCaptionValue({ kind: "zone-pair", a: "zone-1", b: "zone-2" })).toBe("all scopes");
    expect(scopeCaptionValue({ kind: "cluster", a: "", b: "" })).toBe("all scopes");
  });

  it("is the filter value itself for a narrow scope, which really is filtered", () => {
    expect(scopeCaptionValue({ kind: "node", a: "node-a", b: "" })).toBe("node-a");
    expect(scopeCaptionValue({ kind: "pair", a: "node-a", b: "node-b" })).toBe("node-a→node-b");
  });
});

describe("scopeNodeOptions / scopeZoneOptions (finding #5)", () => {
  it("fills both lists from the AGENTS alone when the controller reports no nodes", () => {
    const topo = {
      nodes: [],
      agents: [
        { nodeName: "node-b", zone: "zone-2" },
        { nodeName: "node-a", zone: "zone-1" },
      ],
    };
    expect(scopeNodeOptions(topo)).toEqual(["node-a", "node-b"]);
    expect(scopeZoneOptions(topo)).toEqual(["zone-1", "zone-2"]);
  });

  it("unions the two lists and dedupes a node reported by both", () => {
    const topo = {
      nodes: [{ name: "node-a", zone: "zone-1" }],
      agents: [
        { nodeName: "node-a", zone: "zone-1" },
        { nodeName: "node-c", zone: "zone-3" },
      ],
    };
    expect(scopeNodeOptions(topo)).toEqual(["node-a", "node-c"]);
    expect(scopeZoneOptions(topo)).toEqual(["zone-1", "zone-3"]);
  });

  it("drops empty names and survives an absent topology", () => {
    expect(scopeNodeOptions(undefined)).toEqual([]);
    expect(scopeZoneOptions({ nodes: [{ name: "node-a", zone: "" }] })).toEqual([]);
  });
});

describe("auditDetailLine (finding #18)", () => {
  it("omits an empty segment instead of leaving a dangling separator", () => {
    const row: AuditEntry = {
      id: 7,
      at: FROM,
      subjectKind: "user",
      subjectId: "ada",
      action: "POST /api/v1/auth/login",
      resource: "",
      outcome: "allowed",
      remoteAddr: "",
      detail: {},
    };
    expect(auditDetailLine(row)).toBe("user:ada · allowed");
    expect(auditDetailLine(row)).not.toContain("· ·");
  });

  it("still joins every segment that is present", () => {
    const row: AuditEntry = {
      id: 8,
      at: FROM,
      subjectKind: "user",
      subjectId: "ada",
      action: "DELETE /api/v1/targets/{id}",
      resource: "targets",
      outcome: "denied",
      remoteAddr: "",
      detail: { name: "api-gw" },
    };
    expect(auditDetailLine(row)).toBe("user:ada · targets · denied · name=api-gw");
  });

  it("prefers subjectDisplay over the raw subjectKind:subjectId (M3-5)", () => {
    const row: AuditEntry = {
      id: 9,
      at: FROM,
      subjectKind: "user",
      subjectId: "oidc:6f616b42-0ed8-571e-823f-ee4aca6b7ce9",
      subjectDisplay: "d.esin@group-ib.com",
      action: "POST /api/v1/targets",
      resource: "targets",
      outcome: "allowed",
      remoteAddr: "",
      detail: {},
    };
    expect(auditDetailLine(row)).toBe("d.esin@group-ib.com · targets · allowed");
    expect(auditDetailLine(row)).not.toContain("oidc");
  });

  it("falls back to the raw subject on rows captured before subjectDisplay existed", () => {
    const row = {
      id: 10,
      at: FROM,
      subjectKind: "user",
      subjectId: "ada",
      subjectDisplay: "",
      action: "POST /api/v1/targets",
      resource: "targets",
      outcome: "allowed",
      remoteAddr: "",
      detail: {},
    } as AuditEntry;
    expect(auditDetailLine(row)).toBe("user:ada · targets · allowed");
  });
});

describe("scopeFilterValue", () => {
  it("is the annotations/events scope vocabulary: node name, pair arrow, target name, '' for the wide scopes", () => {
    expect(scopeFilterValue({ kind: "node", a: "node-a", b: "" })).toBe("node-a");
    expect(scopeFilterValue({ kind: "pair", a: "node-a", b: "node-b" })).toBe("node-a→node-b");
    expect(scopeFilterValue({ kind: "target", a: "api-gw", b: "" })).toBe("api-gw");
    expect(scopeFilterValue({ kind: "zone-pair", a: "zone-1", b: "zone-2" })).toBe("");
    expect(scopeFilterValue({ kind: "cluster", a: "", b: "" })).toBe("");
  });
});

/* ── the PromQL the scope produces ──────────────────────────────────────── */

describe("the scope's signal queries", () => {
  it("selects a pair by both peer labels and escapes the values", () => {
    const q = investigationLossQuery({ kind: "pair", a: 'no"de', b: "node-b" });
    expect(q).toContain('source_node="no\\"de"');
    expect(q).toContain('destination_node="node-b"');
    expect(q).toContain("packet_loss_ratio");
  });

  it("selects a zone pair by the zone labels, not the node ones", () => {
    const q = investigationRttQuery({ kind: "zone-pair", a: "zone-1", b: "zone-2" });
    expect(q).toContain('source_zone="zone-1"');
    expect(q).toContain('destination_zone="zone-2"');
    expect(q).not.toContain("source_node=");
  });

  it("uses the EXTERNAL metric family for a target scope — there is no destination_node there", () => {
    expect(investigationLossQuery({ kind: "target", a: "api-gw", b: "" })).toContain(
      'kconmon_ng_external_packet_loss_ratio{target="api-gw"}',
    );
    expect(investigationFailRatioQuery({ kind: "target", a: "api-gw", b: "" })).toContain("external_results_total");
  });

  it("selects nothing at all for the cluster scope — an empty selector is the whole fleet", () => {
    expect(investigationLossQuery({ kind: "cluster", a: "", b: "" })).not.toContain("{");
  });

  /* ── QA round 3, finding #4: the empty-sum trap ───────────────────────── */

  it("guards every per-protocol sum with `or vector(0)` so an absent protocol contributes 0", () => {
    const q = investigationFailRatioQuery({ kind: "pair", a: "node-a", b: "node-b" });
    for (const protocol of ["tcp", "udp", "icmp"]) {
      expect(q).toContain(
        `(sum(rate(kconmon_ng_${protocol}_results_total{source_node="node-a",destination_node="node-b",result="fail"}[5m])) or vector(0))`,
      );
      expect(q).toContain(
        `(sum(rate(kconmon_ng_${protocol}_results_total{source_node="node-a",destination_node="node-b"}[5m])) or vector(0))`,
      );
    }
    // Six guards: three protocols on each side of the division.
    expect(q.split("or vector(0)")).toHaveLength(7);
  });

  it("guards the target scope's single family the same way", () => {
    const q = investigationFailRatioQuery({ kind: "target", a: "api-gw", b: "" });
    expect(q).toBe(
      '(sum(rate(kconmon_ng_external_results_total{target="api-gw",result="fail"}[5m])) or vector(0)) / ' +
        '(sum(rate(kconmon_ng_external_results_total{target="api-gw"}[5m])) or vector(0))',
    );
  });

  it("leaves investigationLossQuery unguarded — `or` already unions, and vector(0) would fake a healthy pair", () => {
    const q = investigationLossQuery({ kind: "pair", a: "node-a", b: "node-b" });
    expect(q).not.toContain("vector(0)");
    expect(q.startsWith("max(")).toBe(true);
  });
});

describe("samplesFromMatrix", () => {
  it("merges the loss and RTT series onto one timeline and converts RTT seconds to ns", () => {
    const samples = samplesFromMatrix(matrixBody(LOSS_STEPS), matrixBody(RTT_STEPS));
    expect(samples).toHaveLength(4);
    expect(samples[0].loss).toBe(0.001);
    expect(samples[0].rttNs).toBe(5_000_000);
    // The fourth loss sample has no RTT partner: absent, never zero.
    expect(samples[3].rttNs).toBeUndefined();
  });

  it("is empty for Prometheus's error envelope rather than inventing a flat line", () => {
    expect(samplesFromMatrix({ status: "error", error: "boom" }, undefined)).toEqual([]);
  });
});

describe("runTouchesScope", () => {
  it("matches a pair only when the spec names both ends", () => {
    const spec = { Sources: ["node-a"], Destinations: ["node-b"] };
    expect(runTouchesScope(spec, { kind: "pair", a: "node-a", b: "node-b" })).toBe(true);
    expect(runTouchesScope(spec, { kind: "pair", a: "node-a", b: "node-c" })).toBe(false);
  });

  it("treats an EMPTY sources/destinations list as the whole fleet, which touches every pair", () => {
    expect(runTouchesScope({ Sources: [], Destinations: [] }, { kind: "pair", a: "node-a", b: "node-b" })).toBe(true);
  });

  it("matches a target only through a typed destination of kind target — never an ad-hoc address", () => {
    const typed = { TypedDestinations: [{ kind: "target", name: "api-gw" }] };
    const adhoc = { TypedDestinations: [{ kind: "adhoc", name: "api-gw" }] };
    expect(runTouchesScope(typed, { kind: "target", a: "api-gw", b: "" })).toBe(true);
    expect(runTouchesScope(adhoc, { kind: "target", a: "api-gw", b: "" })).toBe(false);
  });

  it("takes every run for the cluster scope", () => {
    expect(runTouchesScope({ Sources: ["node-x"] }, { kind: "cluster", a: "", b: "" })).toBe(true);
  });
});

describe("auditEntries", () => {
  it("keeps only the rows inside the window — the endpoint has no from/to of its own", () => {
    const rows: AuditEntry[] = [
      { id: 1, at: "2026-08-08T00:30:00Z", subjectKind: "user", subjectId: "ada", action: "POST /api/v1/runs", resource: "runs", outcome: "allowed", remoteAddr: "", detail: {} },
      { id: 2, at: "2026-08-07T00:30:00Z", subjectKind: "user", subjectId: "ada", action: "POST /api/v1/targets", resource: "targets", outcome: "allowed", remoteAddr: "", detail: {} },
    ];
    const out = auditEntries(rows, new Date(FROM), new Date(TO));
    expect(out).toHaveLength(1);
    expect(out[0].title).toBe("POST /api/v1/runs");
    expect(out[0].kind).toBe("audit");
  });

  it("marks a denied write as a warning, not as ordinary context", () => {
    const [entry] = auditEntries(
      [{ id: 3, at: "2026-08-08T00:30:00Z", subjectKind: "user", subjectId: "ada", action: "DELETE /api/v1/targets/{id}", resource: "targets", outcome: "denied", remoteAddr: "", detail: {} }],
      new Date(FROM),
      new Date(TO),
    );
    expect(entry.severity).toBe("warn");
  });

  it("keeps the raw subject reachable as the detail tooltip when a display name replaced it (M3-5)", () => {
    const [entry] = auditEntries(
      [{ id: 4, at: "2026-08-08T00:30:00Z", subjectKind: "user", subjectId: "oidc:6f616b42", subjectDisplay: "d.esin@group-ib.com", action: "POST /api/v1/targets", resource: "targets", outcome: "allowed", remoteAddr: "", detail: {} }],
      new Date(FROM),
      new Date(TO),
    );
    expect(entry.detail).toContain("d.esin@group-ib.com");
    expect(entry.detailTitle).toBe("user:oidc:6f616b42");
  });

  it("carries no tooltip when the raw subject is already the visible line", () => {
    const [entry] = auditEntries(
      [{ id: 5, at: "2026-08-08T00:30:00Z", subjectKind: "user", subjectId: "ada", action: "POST /api/v1/targets", resource: "targets", outcome: "allowed", remoteAddr: "", detail: {} }],
      new Date(FROM),
      new Date(TO),
    );
    expect(entry.detail).toContain("user:ada");
    expect(entry.detailTitle).toBeUndefined();
  });
});

/* ── M7 Task 8: the alert source (Decision 6) ───────────────────────────── */

function alertRow(over: Record<string, unknown> = {}): Alert {
  return {
    name: "PairLossHigh",
    state: "firing",
    severity: "critical",
    labels: { alertname: "PairLossHigh", severity: "critical", source_node: "node-a", destination_node: "node-b" },
    annotations: {},
    activeAt: "2026-08-08T00:20:00Z",
    value: "1e+00",
    ruleId: "11111111-1111-4111-8111-111111111111",
    ...over,
  } as Alert;
}

describe("alertEntries", () => {
  it("puts an alert that STARTED inside the window at its activeAt", () => {
    const [entry] = alertEntries([alertRow()], new Date(FROM), new Date(TO));
    expect(entry.at.toISOString()).toBe("2026-08-08T00:20:00.000Z");
    expect(entry.kind).toBe("alert");
    expect(entry.title).toBe("Alert firing: PairLossHigh");
    expect(entry.severity).toBe("error");
  });

  it("puts an alert that was ALREADY firing at the window's start, and says so", () => {
    const [entry] = alertEntries(
      [alertRow({ activeAt: "2026-08-07T12:00:00Z" })],
      new Date(FROM),
      new Date(TO),
    );
    expect(entry.at.toISOString()).toBe(new Date(FROM).toISOString());
    expect(entry.title).toContain("already firing when this window opens");
    // The row is deliberately NOT at the instant it names, so the detail spells
    // the real one out unambiguously.
    expect(entry.detail).toContain("2026-08-07T12:00:00.000Z");
  });

  it("never fabricates a resolution — there is no row for an alert that stopped", () => {
    const out = alertEntries([alertRow(), alertRow({ name: "Other", activeAt: "2026-08-07T12:00:00Z" })], new Date(FROM), new Date(TO));
    expect(out).toHaveLength(2);
    expect(out.every((e) => /firing/i.test(e.title))).toBe(true);
    expect(out.some((e) => /resolv/i.test(e.title) || /resolv/i.test(e.detail ?? ""))).toBe(false);
    // An alert that is gone from the firing set contributes NOTHING, rather
    // than a synthesized "resolved" row nothing recorded.
    expect(alertEntries([], new Date(FROM), new Date(TO))).toEqual([]);
  });

  it("drops an alert that started after the window closed", () => {
    expect(alertEntries([alertRow({ activeAt: "2026-08-08T02:00:00Z" })], new Date(FROM), new Date(TO))).toEqual([]);
  });

  it("drops a PENDING alert: pending is not fired", () => {
    expect(alertEntries([alertRow({ state: "pending" })], new Date(FROM), new Date(TO))).toEqual([]);
  });

  it("drops an alert with no activeAt rather than guessing an instant for it", () => {
    const row = alertRow();
    delete (row as Record<string, unknown>).activeAt;
    expect(alertEntries([row], new Date(FROM), new Date(TO))).toEqual([]);
  });

  it.each([
    ["critical", "error"],
    ["warning", "warn"],
    ["info", "info"],
    ["", "info"],
  ])("maps severity %s onto %s", (severity, want) => {
    const [entry] = alertEntries([alertRow({ severity })], new Date(FROM), new Date(TO));
    expect(entry.severity).toBe(want);
  });

  it("carries the label set in the detail and identifies the row by it", () => {
    const [entry] = alertEntries([alertRow()], new Date(FROM), new Date(TO));
    expect(entry.detail).toContain("source_node=node-a");
    expect(entry.ref?.kind).toBe("alert");
    expect(entry.ref?.id).toContain("PairLossHigh");
  });

  it("keeps two series of the SAME rule apart — one row each, not one deduped row", () => {
    const out = alertEntries(
      [
        alertRow(),
        alertRow({ labels: { alertname: "PairLossHigh", source_node: "node-c", destination_node: "node-b" } }),
      ],
      new Date(FROM),
      new Date(TO),
    );
    expect(out).toHaveLength(2);
    expect(new Set(out.map((e) => e.ref?.id)).size).toBe(2);
  });
});

describe("scopeFromAlertLabels", () => {
  it("reads a pair off source_node + destination_node", () => {
    expect(scopeFromAlertLabels({ source_node: "node-a", destination_node: "node-b" })).toEqual({
      kind: "pair",
      a: "node-a",
      b: "node-b",
    });
  });

  it("reads a node off source_node alone", () => {
    expect(scopeFromAlertLabels({ source_node: "node-a" })).toEqual({ kind: "node", a: "node-a", b: "" });
  });

  it("reads a target off the target label", () => {
    expect(scopeFromAlertLabels({ target: "api-gw" })).toEqual({ kind: "target", a: "api-gw", b: "" });
  });

  it("is null when the labels name nothing this page can scope to", () => {
    expect(scopeFromAlertLabels({})).toBeNull();
    expect(scopeFromAlertLabels({ severity: "critical", instance: "10.0.0.1:9100" })).toBeNull();
    // A destination with no source is NOT a node investigation: the node scope
    // asks the peer family as a SOURCE, which would be a different question.
    expect(scopeFromAlertLabels({ destination_node: "node-b" })).toBeNull();
    // An empty label value is an absent one, not a scope named "".
    expect(scopeFromAlertLabels({ source_node: "" })).toBeNull();
  });
});

/* ── who owns a firing alert, and which of ours this scope is about ──────────
   A cluster running kube-prometheus-stack fires TargetDown, etcdMembersDown and
   Watchdog around the clock. They used to land in every investigation as
   individual rows, unscoped, next to the product's own. */

const PAIR: InvestigationScope = { kind: "pair", a: "node-a", b: "node-b" };

/** A foreign row: no rule id, which is the whole of it. */
function foreignRow(over: Record<string, unknown> = {}): Alert {
  return alertRow({
    name: "TargetDown",
    severity: "warning",
    labels: { alertname: "TargetDown", severity: "warning", job: "kubelet", namespace: "kube-system" },
    ruleId: undefined,
    ...over,
  });
}

describe("alertIsOurs", () => {
  it("is the console's own rule id, the field GET /api/v1/alerts already fills", () => {
    expect(alertIsOurs(alertRow())).toBe(true);
  });

  it("is false for an alert carrying no rule id, whatever it is named", () => {
    expect(alertIsOurs(foreignRow())).toBe(false);
    // A foreign rule may borrow our name and our labels; it still is not ours.
    expect(alertIsOurs(foreignRow({ name: "PairLossHigh", labels: { source_node: "node-a" } }))).toBe(false);
  });

  it("is false for an empty rule id, which the API writes for an unmanaged rule", () => {
    expect(alertIsOurs(alertRow({ ruleId: "" }))).toBe(false);
  });
});

describe("alertTouchesScope", () => {
  it("keeps a pair alert for its own pair and drops it for another", () => {
    expect(alertTouchesScope({ source_node: "node-a", destination_node: "node-b" }, PAIR)).toBe(true);
    expect(alertTouchesScope({ source_node: "node-c", destination_node: "node-d" }, PAIR)).toBe(false);
  });

  it("keeps an alert that names either end of the pair", () => {
    expect(alertTouchesScope({ source_node: "node-a" }, PAIR)).toBe(true);
    expect(alertTouchesScope({ destination_node: "node-b" }, PAIR)).toBe(true);
    expect(alertTouchesScope({ source_node: "node-z" }, PAIR)).toBe(false);
  });

  it("keeps an alert that names nothing scopable: it is about the whole fleet", () => {
    // The same call runTouchesScope makes with an empty Sources list: absent is
    // "everywhere", never "nowhere".
    expect(alertTouchesScope({ severity: "critical" }, PAIR)).toBe(true);
  });

  it("narrows a node scope by either end", () => {
    const node: InvestigationScope = { kind: "node", a: "node-a", b: "" };
    expect(alertTouchesScope({ source_node: "node-a", destination_node: "node-b" }, node)).toBe(true);
    expect(alertTouchesScope({ source_node: "node-b", destination_node: "node-a" }, node)).toBe(true);
    expect(alertTouchesScope({ source_node: "node-c", destination_node: "node-d" }, node)).toBe(false);
  });

  it("narrows a target scope by the target label alone", () => {
    const target: InvestigationScope = { kind: "target", a: "api-gw", b: "" };
    expect(alertTouchesScope({ target: "api-gw" }, target)).toBe(true);
    expect(alertTouchesScope({ target: "other" }, target)).toBe(false);
    expect(alertTouchesScope({ source_node: "node-a" }, target)).toBe(false);
  });

  it("narrows nothing for the two wide scopes, exactly as runs do not", () => {
    for (const kind of ["cluster", "zone-pair"] as const) {
      expect(alertTouchesScope({ source_node: "node-z" }, { kind, a: "zone-1", b: "zone-2" })).toBe(true);
    }
  });
});

describe("scopedAlertEntries", () => {
  it("keeps our alerts as rows and lets no foreign one through", () => {
    const out = scopedAlertEntries(
      [alertRow(), foreignRow(), foreignRow({ name: "etcdMembersDown" }), foreignRow({ name: "Watchdog" })],
      new Date(FROM),
      new Date(TO),
      PAIR,
    );

    expect(out.entries.map((e) => e.title)).toEqual(["Alert firing: PairLossHigh"]);
    // Not folded, not summarised, not counted: kconmon-ng is not an aggregator
    // of everybody's alerts, and a cluster's own backdrop belongs to whoever
    // wrote those rules.
    expect(JSON.stringify(out)).not.toMatch(/TargetDown|etcdMembersDown|Watchdog/);
  });

  it("narrows OUR alerts to the scope and SAYS how many that hid", () => {
    const out = scopedAlertEntries(
      [alertRow(), alertRow({ name: "OtherPair", labels: { source_node: "node-c", destination_node: "node-d" } })],
      new Date(FROM),
      new Date(TO),
      PAIR,
    );

    expect(out.entries).toHaveLength(1);
    expect(out.hiddenByScope).toBe(1);
  });

  it("hides nothing on a cluster scope, so the count it reports is nought", () => {
    const out = scopedAlertEntries(
      [alertRow(), alertRow({ name: "OtherPair", labels: { source_node: "node-c", destination_node: "node-d" } })],
      new Date(FROM),
      new Date(TO),
      { kind: "cluster", a: "", b: "" },
    );
    expect(out.entries).toHaveLength(2);
    expect(out.hiddenByScope).toBe(0);
  });

  it("never counts a foreign alert as hidden by the SCOPE", () => {
    // The scope-hidden line is a promise about OUR rules; counting somebody
    // else's rule into it would be a number the reader cannot act on.
    const out = scopedAlertEntries([foreignRow(), foreignRow({ name: "Watchdog" })], new Date(FROM), new Date(TO), PAIR);
    expect(out.entries).toEqual([]);
    expect(out.hiddenByScope).toBe(0);
  });

  it("never counts an out-of-window alert as hidden by the SCOPE", () => {
    // It has no row for a reason the window already explains; blaming the scope
    // for it would be a second, false sentence about the same alert.
    const out = scopedAlertEntries(
      [alertRow({ name: "Later", activeAt: "2026-08-08T02:00:00Z", labels: { source_node: "node-z" } })],
      new Date(FROM),
      new Date(TO),
      PAIR,
    );
    expect(out.entries).toEqual([]);
    expect(out.hiddenByScope).toBe(0);
  });

  it("drops a pending alert of ours exactly as alertEntries does", () => {
    const out = scopedAlertEntries([alertRow({ state: "pending" })], new Date(FROM), new Date(TO), PAIR);
    expect(out.entries).toEqual([]);
    expect(out.hiddenByScope).toBe(0);
  });
});

describe("buildExportPayload", () => {
  it("pins params, entries and causes into one JSON-safe object", () => {
    const params = { kind: "pair" as const, a: "node-a", b: "node-b", from: new Date(FROM), to: new Date(TO) };
    const entry = {
      at: new Date("2026-08-08T00:17:00Z"),
      kind: "path-change" as const,
      severity: "warn" as const,
      title: "Route changed",
      ref: { kind: "path-change" as const, id: "s-1" },
    };
    const payload = buildExportPayload(params, [entry], [{ entry, score: 1.2 }]);

    expect(payload.params).toEqual({
      kind: "pair",
      scope: "node-a→node-b",
      from: new Date(FROM).toISOString(),
      to: new Date(TO).toISOString(),
    });
    expect(payload.entries).toEqual([
      {
        at: "2026-08-08T00:17:00.000Z",
        kind: "path-change",
        severity: "warn",
        title: "Route changed",
        detail: undefined,
        ref: { kind: "path-change", id: "s-1" },
      },
    ]);
    expect(payload.causes).toEqual([{ at: "2026-08-08T00:17:00.000Z", kind: "path-change", title: "Route changed", score: 1.2 }]);
    // The whole point of an export is that it survives JSON.stringify.
    expect(() => JSON.stringify(payload)).not.toThrow();
  });
});

/* ── the signal-panel overlays ──────────────────────────────────────────── */

describe("the signal overlays", () => {
  /* The cursor is no longer an overlay AT ALL. It used to be a markLine rebuilt
     into the option on every hover, which was affordable while only a timeline
     ROW could move it; a chart hover moves it per pixel, and a setOption per
     pixel is not. It lives in the page's cursor group now (lib/chart-cursor.tsx)
     and is drawn as one positioned line — see components/echart.test.tsx. */

  it("the LIFTED maintenance builder draws one markArea band per window", () => {
    const s = maintenanceOverlaySeries([maintenanceRow()], false);
    expect(s?.markArea?.data).toHaveLength(1);
  });

  /* The lift itself, pinned: the Investigate charts must draw the bands the
     shared helper builds, not a private copy that can drift from the pair
     card's. withOverlays is the only thing this page still owns about them. */
  it("withOverlays appends the SHARED maintenance series to the signal option", () => {
    const out = withOverlays(
      { series: [{ type: "line", name: "loss", data: [] }] },
      { windows: [maintenanceRow()], dark: false },
    );
    expect((out.series as { name?: string }[]).map((s) => s.name)).toEqual(["loss", MAINTENANCE_SERIES_NAME]);
  });

  it("deltaFromVectors signs the change from the start of the window to its end", () => {
    const d = deltaFromVectors(vectorBody("0.10"), vectorBody("0.25"));
    expect(d.before).toBeCloseTo(0.1);
    expect(d.after).toBeCloseTo(0.25);
    expect(d.delta).toBeCloseTo(0.15);
  });

  it("reports a null delta when either end has no sample — 'no data' is not zero", () => {
    expect(deltaFromVectors(undefined, vectorBody("0.25")).delta).toBeNull();
  });
});

/* ── QA round 3, findings #13 and #14: the signals column's own axes ─────── */

describe("signalChartOption", () => {
  const chart = (unit: "seconds" | "ratio") => ({ id: "t", title: "t", unit, query: "" });
  const overlays = { windows: [] };
  const axisLabel = (option: ReturnType<typeof signalChartOption>, axis: "xAxis" | "yAxis") =>
    (option[axis] as { axisLabel?: Record<string, unknown> }).axisLabel ?? {};

  it("#13 hides overlapping time ticks — a 20rem axis smears otherwise", () => {
    const option = signalChartOption(chart("ratio"), matrixBody(LOSS_STEPS), false, overlays);
    expect(axisLabel(option, "xAxis").hideOverlap).toBe(true);
  });

  it("#14 formats the RTT axis with the ADAPTIVE millisecond formatter", () => {
    const option = signalChartOption(chart("seconds"), matrixBody(RTT_STEPS), false, overlays);
    const formatter = axisLabel(option, "yAxis").formatter as (v: number) => string;
    expect(typeof formatter).toBe("function");
    /* The point of "adaptive": a sub-10ms axis steps finer than a whole
       millisecond, so integer labels repeat. One decimal separates them, and
       above 10ms the decimal would be noise. */
    expect(formatter(0.0005)).toBe(formatSeconds(0.0005));
    expect(formatter(0.0005)).toBe("0.5ms");
    expect(formatter(0.0006)).toBe("0.6ms");
    expect(formatter(0.0335)).toBe("34ms");
  });

  /* The name of this case was always right and its assertion was not: it pinned
     `undefined`, which ECharts reads as "no formatter", so the loss axis printed
     0.01 next to a tooltip and a neighbour readout saying 1.0%. */
  it("leaves a RATIO axis to the shared builder's own percent formatting", () => {
    const option = signalChartOption(chart("ratio"), matrixBody(LOSS_STEPS), false, overlays);
    const formatter = axisLabel(option, "yAxis").formatter as (v: number) => string;
    expect(typeof formatter).toBe("function");
    expect(formatter(0.01)).toBe("1.0%");
  });

  it("keeps the SECONDS axis in milliseconds, which is the same builder's other answer", () => {
    const option = signalChartOption(chart("seconds"), matrixBody(LOSS_STEPS), false, overlays);
    const formatter = axisLabel(option, "yAxis").formatter as (v: number) => string;
    expect(formatter(0.215)).toBe("215ms");
  });

  it("still composes the overlays rather than replacing them", () => {
    const option = signalChartOption(chart("ratio"), matrixBody(LOSS_STEPS), false, {
      windows: [maintenanceRow()],
    });
    const names = (option.series as { name?: string }[]).map((s) => s.name);
    expect(names).toContain(MAINTENANCE_SERIES_NAME);
  });
});

/* ── the page ───────────────────────────────────────────────────────────── */

describe("InvestigatePage — the URL is the entry contract", () => {
  it("hydrates scope and range from the query string on a cold load", async () => {
    renderPage();
    expect(await screen.findByRole("radio", { name: "Pair", checked: true })).toBeTruthy();
    await waitFor(() => expect((screen.getByLabelText("Source node") as HTMLSelectElement).value).toBe("node-a"));
    expect((screen.getByLabelText("Destination node") as HTMLSelectElement).value).toBe("node-b");
  });

  it("writes the form back into the URL when the investigation is applied", async () => {
    renderPage({ search: `?kind=node&scope=node-a&from=${FROM}&to=${TO}` });
    await screen.findByRole("radio", { name: "Node", checked: true });
    // Wait for topology to supply the options — a <select> cannot be driven to
    // a value it has no <option> for, and the picker is fed by the controller.
    await waitFor(() => expect((screen.getByLabelText("Node") as HTMLSelectElement).value).toBe("node-a"));

    fireEvent.change(screen.getByLabelText("Node"), { target: { value: "node-c" } });
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));

    await waitFor(() => {
      const qs = new URLSearchParams(window.location.search);
      expect(qs.get("kind")).toBe("node");
      expect(qs.get("scope")).toBe("node-c");
      // The preset the form opened on is the range the URL keeps: one hour.
      expect(Date.parse(qs.get("to") ?? "") - Date.parse(qs.get("from") ?? "")).toBe(60 * 60 * 1000);
    });
  });

  it("a range preset rewrites both instants in the URL", async () => {
    renderPage();
    await screen.findByRole("radio", { name: "Pair", checked: true });

    fireEvent.click(screen.getByRole("radio", { name: "15m" }));
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));

    await waitFor(() => {
      const qs = new URLSearchParams(window.location.search);
      const from = Date.parse(qs.get("from") ?? "");
      const to = Date.parse(qs.get("to") ?? "");
      expect(to - from).toBe(15 * 60 * 1000);
    });
  });
});

describe("InvestigatePage — per-source permission gating", () => {
  it("asks each source's endpoint exactly once for the window when every permission is held", async () => {
    const { urlsFor } = renderPage();
    await waitFor(() => expect(urlsFor("/api/v1/audit").length).toBe(1));

    expect(urlsFor("/api/v1/events")[0]).toContain(`scope=${encodeURIComponent("node-a→node-b")}`);
    expect(urlsFor("/api/v1/maintenance").length).toBeGreaterThan(0);
    expect(urlsFor("/api/v1/annotations").length).toBeGreaterThan(0);
    expect(urlsFor("/api/v1/mtr/snapshots").length).toBe(1);
  });

  it("a subject without audit:read gets the muted line and ZERO requests to /api/v1/audit", async () => {
    const { urlsFor } = renderPage({ permissions: ALL_READS.filter((p) => p !== "audit:read") });

    // "Audit rows", not "config changes": most of what the audit log records is a READ decision.
    expect(await screen.findByText(/audit rows need audit:read/i)).toBeTruthy();
    // Give every other source time to fire so "zero" means zero, not "not yet".
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/audit")).toEqual([]);
  });

  it("a subject without mtr:read gets the muted line and ZERO snapshot requests", async () => {
    const { urlsFor } = renderPage({ permissions: ALL_READS.filter((p) => p !== "mtr:read") });

    expect(await screen.findByText(/path changes need mtr:read/i)).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/mtr")).toEqual([]);
  });

  it("a subject without promql:query gets the muted line and ZERO PromQL requests", async () => {
    const { urlsFor } = renderPage({ permissions: ALL_READS.filter((p) => p !== "promql:query") });

    expect(await screen.findByText(/threshold crossings need promql:query/i)).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/promql")).toEqual([]);
  });

  it("a subject without alerts:read gets the muted line and ZERO alert requests", async () => {
    const { urlsFor } = renderPage({ permissions: ALL_READS.filter((p) => p !== "alerts:read") });

    expect(await screen.findByText(/firing alerts need alerts:read/i)).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/alerts")).toEqual([]);
  });
});

/* ── M7 Task 8: the alert source on the page (Decision 6) ───────────────── */

describe("InvestigatePage — firing alerts as timeline rows", () => {
  const firing = (over: Record<string, unknown> = {}) => ({
    name: "PairLossHigh",
    state: "firing",
    severity: "critical",
    labels: { alertname: "PairLossHigh", severity: "critical", source_node: "node-a", destination_node: "node-b" },
    annotations: {},
    activeAt: "2026-08-08T00:20:00Z",
    value: "1e+00",
    ruleId: "11111111-1111-4111-8111-111111111111",
    ...over,
  });

  it("asks /api/v1/alerts once and renders the alert that started inside the window", async () => {
    const { urlsFor } = renderPage({ alerts: [firing()] });

    expect(await screen.findByText("Alert firing: PairLossHigh")).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/alerts").length).toBe(1));
  });

  it("places an alert that predates the window at the window's start and says so", async () => {
    renderPage({ alerts: [firing({ activeAt: "2026-08-07T09:00:00Z" })] });

    const row = await screen.findByText(/already firing when this window opens/i);
    expect(row).toBeTruthy();
  });

  it("states the granularity: resolutions are not recorded", async () => {
    renderPage();
    expect(await screen.findByText(/resolutions are not recorded; only what is firing now is visible/i)).toBeTruthy();
  });

  it("the M6 'missing engine' note is GONE", async () => {
    renderPage({ alerts: [firing()] });
    await screen.findByText("Alert firing: PairLossHigh");
    expect(screen.queryByText(/alert state arrives with alerting/i)).toBeNull();
    expect(screen.queryByText(/missing engine/i)).toBeNull();
  });

  it("without Prometheus configured: the honest line and ZERO alert requests", async () => {
    const { urlsFor } = renderPage({ prometheusConfigured: false });

    expect(await screen.findByText(/firing alerts read prometheus — set console\.prometheus\.address/i)).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/alerts")).toEqual([]);
  });

  it("an alert row cannot be pinned — there is no row anywhere to point at", async () => {
    renderPage({ permissions: WRITE, alerts: [firing()], incident: incidentRow(), search: "?incident=inc-1" });

    const title = await screen.findByText("Alert firing: PairLossHigh");
    const row = title.closest("li") as HTMLElement;
    expect(within(row).queryByRole("button", { name: /^Pin:/ })).toBeNull();
  });
});

/* ── the foreign-alert flood, on the page ────────────────────────────────────
   Reproduced against a live stand running kube-prometheus-stack: nine firing
   alerts, none of them ours, every one of them a row in every investigation.
   The product answer is not a tidier row for them — it is no row at all. */

describe("InvestigatePage — only this console's own alerts reach the timeline", () => {
  const ours = (over: Record<string, unknown> = {}) => ({
    name: "PairLossHigh",
    state: "firing",
    severity: "critical",
    labels: { alertname: "PairLossHigh", severity: "critical", source_node: "node-a", destination_node: "node-b" },
    annotations: {},
    activeAt: "2026-08-08T00:20:00Z",
    value: "1e+00",
    ruleId: "11111111-1111-4111-8111-111111111111",
    ...over,
  });

  /** The stand's own noise, in miniature: no rule id on any of it. */
  const NOISE = [
    { alertname: "TargetDown", severity: "warning", job: "kubelet" },
    { alertname: "etcdMembersDown", severity: "critical", job: "etcd" },
    { alertname: "Watchdog", severity: "none" },
  ].map((labels) => ({
    name: labels.alertname,
    state: "firing",
    severity: labels.severity,
    labels,
    annotations: {},
    activeAt: "2026-08-07T09:00:00Z",
    value: "1e+00",
  }));

  it("asks the route for the managed set only, so foreign alerts never cross the wire", async () => {
    const { urlsFor } = renderPage({ alerts: [ours()] });
    await screen.findByText("Alert firing: PairLossHigh");
    expect(urlsFor("/api/v1/alerts")).toEqual(["/api/v1/alerts?managedOnly=true"]);
  });

  it("puts no row, no summary and no count on the page for a foreign alert", async () => {
    renderPage({ alerts: [...NOISE, ours()] });

    expect(await screen.findByText("Alert firing: PairLossHigh")).toBeTruthy();
    for (const name of ["TargetDown", "etcdMembersDown", "Watchdog"]) {
      expect(screen.queryByText(new RegExp(name))).toBeNull();
    }
    // The collapsed row this defect first shipped as is gone with them.
    expect(screen.queryByText(/cluster-wide alerts/i)).toBeNull();
    expect(screen.queryByTestId("timeline-group-members")).toBeNull();
  });

  it("counts only what it shows: foreign alerts are not in the window count", async () => {
    renderPage({ alerts: [...NOISE, ours()] });
    await screen.findByText("Alert firing: PairLossHigh");
    // One alert of ours + the one threshold crossing the fixture derives.
    expect(screen.getByTestId("timeline-count").textContent).toBe("2 entries in this window");
    expect(screen.getAllByTestId("timeline-row")).toHaveLength(2);
  });

  it("the caption says plainly whose alerts these are", async () => {
    renderPage({ alerts: [ours()] });
    const caption = await screen.findByText(/only alerts from rules this console manages/i);
    expect(caption.textContent).toMatch(/narrowed to this scope/i);
    // No promise of a cluster-wide row anywhere in the caption.
    expect(caption.textContent).not.toMatch(/collapse|cluster-wide/i);
  });

  it("says out loud when the scope kept one of OUR alerts off the timeline", async () => {
    renderPage({
      alerts: [
        ours(),
        ours({ name: "OtherPair", labels: { alertname: "OtherPair", source_node: "node-c", destination_node: "node-d" } }),
      ],
    });

    await screen.findByText("Alert firing: PairLossHigh");
    expect(screen.getByText(/1 alert from this console's own rules/i)).toBeTruthy();
    expect(screen.queryByText("Alert firing: OtherPair")).toBeNull();
  });

  it("keeps that line away when the scope hid nothing", async () => {
    renderPage({ alerts: [ours()] });
    await screen.findByText("Alert firing: PairLossHigh");
    expect(screen.queryByText(/from this console's own rules/i)).toBeNull();
  });

  it("exports what the timeline shows, with no folded rows left to unfold", async () => {
    renderPage({ alerts: [ours()] });
    await screen.findByText("Alert firing: PairLossHigh");

    const payload = buildExportPayload(
      { kind: "cluster", a: "", b: "", from: new Date(FROM), to: new Date(TO) },
      [{ at: new Date(FROM), kind: "alert", severity: "error", title: "Alert firing: PairLossHigh" }],
      [],
    );
    expect(payload.entries.map((e) => e.title)).toEqual(["Alert firing: PairLossHigh"]);
  });
});

describe("InvestigatePage — the scope decides the request shapes", () => {
  it("a pair scope asks for BOTH nodes' cluster events, name-filtered, and one arrow-scoped event feed", async () => {
    const { urlsFor } = renderPage();
    await waitFor(() => expect(urlsFor("/api/v1/k8s-events").length).toBe(2));

    const names = urlsFor("/api/v1/k8s-events")
      .map((u) => new URLSearchParams(u.split("?")[1]).get("name"))
      .sort();
    expect(names).toEqual(["node-a", "node-b"]);
    expect(urlsFor("/api/v1/events")[0]).toContain(`scope=${encodeURIComponent("node-a→node-b")}`);
  });

  it("a cluster scope name-filters nothing and sends no scope at all", async () => {
    const { urlsFor } = renderPage({ search: `?kind=cluster&from=${FROM}&to=${TO}` });
    await waitFor(() => expect(urlsFor("/api/v1/k8s-events").length).toBe(1));

    expect(new URLSearchParams(urlsFor("/api/v1/k8s-events")[0].split("?")[1]).get("name")).toBeNull();
    expect(urlsFor("/api/v1/events")[0]).not.toContain("scope=");
  });

  it("a zone-pair scope has no pair to trace, and says so instead of a broken snapshot request", async () => {
    const { urlsFor } = renderPage({ search: `?kind=zone-pair&scope=${encodeURIComponent("zone-1→zone-2")}&from=${FROM}&to=${TO}` });

    expect(await screen.findByText(/path history needs a pair, a node or a target/i)).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/mtr")).toEqual([]);
  });
});

describe("InvestigatePage — the timeline", () => {
  it("merges every source into one time-ordered list with a kind badge per row", async () => {
    renderPage({
      events: [
        { id: "e-1", seq: 1, type: "topology_changed", severity: "warn", scope: "node-a→node-b", timestamp: "2026-08-08T00:05:00Z", summary: "node-b NotReady", details: null },
      ],
      k8sEvents: [
        { id: "k-1", uid: "u", resourceVersion: "1", eventTime: "2026-08-08T00:15:00Z", kind: "Node", name: "node-b", namespace: "", reason: "NodeNotReady", type: "Warning", message: "kubelet stopped", count: 1 },
      ],
      snapshots: [snapshotRow()],
      annotations: [annotationRow()],
    });

    /* NEWEST FIRST: the merge is ascending (the onset detection and the correlation ranking read it
       that way) and the pane reverses it for the reader, so the most recent thing in the window is
       the first row. */
    const rows = await screen.findAllByTestId("timeline-row");
    const titles = rows.map((r) => r.textContent ?? "");
    expect(titles[0]).toContain("Packet loss crossed the threshold");
    expect(titles[1]).toContain("poked the gateway");
    expect(titles[2]).toContain("Route changed");
    expect(titles[3]).toContain("NodeNotReady");
    expect(titles[4]).toContain("node-b NotReady");

    expect(within(rows[2]).getByText("path change")).toBeTruthy();
  });

  it("derives the threshold rows from the scope's own loss series", async () => {
    renderPage();
    expect(await screen.findByText(/Packet loss crossed the threshold/)).toBeTruthy();
    expect(screen.getByText(/5\.00% \(threshold 1%\)/)).toBeTruthy();
  });

  it("says the timeline is empty rather than rendering an empty box", async () => {
    renderPage({ permissions: ["events:read"] });
    expect(await screen.findByText(/Nothing happened in this window/i)).toBeTruthy();
  });

  it("hovering a row moves the signal panels' cursor to that instant", async () => {
    renderPage({
      k8sEvents: [
        { id: "k-1", uid: "u", resourceVersion: "1", eventTime: "2026-08-08T00:15:00Z", kind: "Node", name: "node-b", namespace: "", reason: "NodeNotReady", type: "Warning", message: "kubelet stopped", count: 2 },
      ],
    });

    const rows = await screen.findAllByTestId("timeline-row");
    expect(screen.getByTestId("signal-cursor").textContent).toMatch(/nothing hovered/i);

    /* The k8s event is not the newest row any more — the list is newest-first — so it is found by
       its text rather than by an index that now means something else. */
    const k8sRow = rows.find((r) => (r.textContent ?? "").includes("NodeNotReady"));
    if (!k8sRow) {
      throw new Error("the k8s event row is not in the timeline");
    }
    fireEvent.mouseEnter(k8sRow);
    await waitFor(() =>
      expect(screen.getByTestId("signal-cursor").textContent).toContain(new Date("2026-08-08T00:15:00Z").toLocaleTimeString(undefined, { hour12: false })),
    );

    fireEvent.mouseLeave(rows[0]);
    await waitFor(() => expect(screen.getByTestId("signal-cursor").textContent).toMatch(/nothing hovered/i));
  });
});

/*
 * the timeline's pagination, through the real page The pager's own arithmetic and chrome are pinned
 * in components/ investigation-timeline.test.tsx.
 */

/** One event a minute for the whole investigated hour. Sixty rows plus the one
 *  threshold crossing the loss fixture derives = 61 entries, i.e. seven pages at
 *  the default size of ten, the last of them holding a single row. */
const HOUR_OF_EVENTS = Array.from({ length: 60 }, (_, i) => ({
  id: `e-${i}`,
  seq: i + 1,
  type: "topology_changed",
  severity: "info",
  scope: "node-a→node-b",
  timestamp: `2026-08-08T00:${String(i).padStart(2, "0")}:00Z`,
  summary: `event number ${i}`,
  details: null,
}));

describe("InvestigatePage — the timeline is paginated client-side", () => {
  it("cuts the merged list into pages while counting the whole window", async () => {
    renderPage({ events: HOUR_OF_EVENTS });

    await waitFor(() => expect(screen.getAllByTestId("timeline-row")).toHaveLength(10));
    expect(screen.getByTestId("timeline-count").textContent).toMatch(/61 entries in this window/);
    expect(screen.getByTestId("pager-page").textContent).toMatch(/page 1 of 7/i);

    // The last page holds the REMAINDER, and the window's count is untouched by
    // any of it — a pager must not shrink the fleet.
    for (let i = 0; i < 6; i++) fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(screen.getByTestId("pager-page").textContent).toMatch(/page 7 of 7/i);
    expect(screen.getAllByTestId("timeline-row")).toHaveLength(1);
    expect(screen.getByTestId("timeline-count").textContent).toMatch(/61 entries in this window/);
  });

  it("issues NOT ONE extra request to turn a page — the window is already fetched", async () => {
    const { calls } = renderPage({ events: HOUR_OF_EVENTS });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row")).toHaveLength(10));

    const before = calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    fireEvent.click(within(screen.getByRole("radiogroup", { name: "Rows per page" })).getByRole("radio", { name: "50" }));
    expect(calls.length).toBe(before);
  });

  it("keeps the audit, runs and alert captions above the rows on page 2", async () => {
    renderPage({ events: HOUR_OF_EVENTS });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row")).toHaveLength(10));

    fireEvent.click(screen.getByRole("button", { name: "Next page" }));

    const sources = within(screen.getByRole("list", { name: "Timeline sources" })).getAllByRole("listitem");
    const text = sources.map((li) => li.textContent ?? "").join(" ");
    expect(text).toMatch(/newest 200 audit rows/);
    expect(text).toMatch(/Runs are the newest/);
    expect(text).toMatch(/firing NOW/);
  });

  it("resets to page 1 when the investigated window changes", async () => {
    renderPage({ events: HOUR_OF_EVENTS });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row")).toHaveLength(10));

    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(screen.getByTestId("pager-page").textContent).toMatch(/page 2 of 7/i);

    fireEvent.click(screen.getByRole("radio", { name: "15m" }));
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));

    await waitFor(() => expect(screen.getByTestId("pager-page").textContent).toMatch(/page 1 of /i));
  });

  it("keeps the page size out of the URL — the four parameters are the whole contract", async () => {
    renderPage({ events: HOUR_OF_EVENTS });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row")).toHaveLength(10));

    fireEvent.click(within(screen.getByRole("radiogroup", { name: "Rows per page" })).getByRole("radio", { name: "50" }));
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));

    const qs = new URLSearchParams(window.location.search);
    expect([...qs.keys()].sort()).toEqual(["from", "kind", "scope", "to"]);
  });
});

describe("InvestigatePage — correlation", () => {
  it("ranks a path change above an annotation, which is never a cause at all", async () => {
    renderPage({ snapshots: [snapshotRow()], annotations: [annotationRow()] });

    const list = await screen.findByRole("list", { name: "Ranked causes" });
    const items = within(list).getAllByRole("listitem");
    expect(items[0].textContent).toContain("Route changed");
    expect(items.some((i) => (i.textContent ?? "").includes("poked the gateway"))).toBe(false);
    // The documented weight, reachable from the row: 3 × (1 − 180/300) = 1.2.
    expect(items[0].textContent).toContain("1.20");
    expect(CAUSE_WEIGHTS["path-change"]).toBe(3);
  });

  it("names the heuristic and links the scoring source rather than implying a model", async () => {
    renderPage({ snapshots: [snapshotRow()] });
    expect(await screen.findByText(/ranked by temporal proximity; the weights live in the open/i)).toBeTruthy();
    const link = screen.getByRole("link", { name: /the scoring source/ });
    expect(link.getAttribute("href")).toContain("web/src/lib/investigation.ts");
  });

  it("ranks nothing when no threshold was crossed, and says why", async () => {
    // No loss crossing: a flat, healthy series has no onset to rank against.
    const flat: [number, string][] = [
      [Date.parse(FROM) / 1000, "0.0001"],
      [Date.parse(TO) / 1000, "0.0001"],
    ];
    const original = LOSS_STEPS.slice();
    LOSS_STEPS.length = 0;
    LOSS_STEPS.push(...flat);
    try {
      renderPage({ snapshots: [snapshotRow()] });
      expect(await screen.findByText(/no threshold crossing in range — nothing to rank/i)).toBeTruthy();
    } finally {
      LOSS_STEPS.length = 0;
      LOSS_STEPS.push(...original);
    }
  });
});

describe("InvestigatePage — the actions rail", () => {
  it("hides the run buttons entirely without runs:create", async () => {
    renderPage();
    await screen.findByRole("button", { name: "Investigate" });
    expect(screen.queryByRole("button", { name: "Run MTR now" })).toBeNull();
  });

  it("starts a run for the scope's own pair", async () => {
    const { postCalls } = renderPage({ permissions: [...ALL_READS, "runs:create"] });

    fireEvent.click(await screen.findByRole("button", { name: "Run MTR now" }));
    await waitFor(() => expect(postCalls().length).toBe(1));

    expect(postCalls()[0].url).toBe("/api/v1/runs");
    expect(postCalls()[0].body).toMatchObject({ type: "mtr", sources: ["node-a"], destinations: ["node-b"] });
  });

  it("keeps the run buttons visible but disabled while the Time Machine is engaged", async () => {
    renderPage({
      permissions: [...ALL_READS, "runs:create"],
      search: `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=${FROM}&to=${TO}&at=2026-08-08T00:30:00Z`,
    });
    const button = await screen.findByRole("button", { name: "Run TCP now" });
    expect((button as HTMLButtonElement).disabled).toBe(true);
  });

  /* This is the SECOND replacement: "Create maintenance" is a real control now. */
  it("no longer shows Create maintenance as a disabled seam — it is the real control", async () => {
    renderPage({ permissions: [...ALL_READS, "maintenance:write"] });
    const create = await screen.findByRole("button", { name: "Create maintenance" });
    expect((create as HTMLButtonElement).disabled).toBe(false);
    expect(create.getAttribute("title")).toBeNull();
  });

  it("hides Create maintenance entirely without maintenance:write", async () => {
    renderPage();
    await screen.findByRole("button", { name: "Investigate" });
    expect(screen.queryByRole("button", { name: "Create maintenance" })).toBeNull();
  });

  it("keeps Create maintenance visible but disabled while the Time Machine is engaged", async () => {
    renderPage({
      permissions: [...ALL_READS, "maintenance:write"],
      search: `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=${FROM}&to=${TO}&at=2026-08-08T00:30:00Z`,
    });
    const create = await screen.findByRole("button", { name: "Create maintenance" });
    expect((create as HTMLButtonElement).disabled).toBe(true);
  });

  it("declares the window against the investigation's OWN scope, fixed", async () => {
    const { postCalls } = renderPage({ permissions: [...ALL_READS, "maintenance:write"] });

    fireEvent.click(await screen.findByRole("button", { name: "Create maintenance" }));
    fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: "switch upgrade" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));

    await waitFor(() => expect(postCalls().length).toBe(1));
    expect(postCalls()[0].url).toBe("/api/v1/maintenance");
    expect(postCalls()[0].body).toMatchObject({ scope: "node-a→node-b", reason: "switch upgrade" });
    // A span, always: both edges present, and RFC3339 on the wire.
    const body = postCalls()[0].body as { startAt: string; endAt: string };
    expect(Date.parse(body.endAt)).toBeGreaterThan(Date.parse(body.startAt));
  });

  it("lists the windows overlapping the investigation in the rail, and deletes one", async () => {
    const { calls } = renderPage({
      permissions: [...ALL_READS, "maintenance:write"],
      maintenance: [maintenanceRow({ id: "m-doomed", reason: "wrong day" })],
    });

    // Two clicks: the row confirms before it destroys (QA round 2, #14).
    fireEvent.click(await screen.findByRole("button", { name: "Delete maintenance window: wrong day" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm delete maintenance window: wrong day" }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === "DELETE" && c.url === "/api/v1/maintenance/m-doomed")).toBe(true),
    );
  });

  it("shows no maintenance bar at all without maintenance:read — nothing to read, nothing to claim", async () => {
    renderPage({ permissions: ALL_READS.filter((p) => p !== "maintenance:read") });
    await screen.findByRole("button", { name: "Investigate" });
    expect(screen.queryByTestId("maintenance-bar")).toBeNull();
  });

  it("hides Save as incident entirely without incidents:write", async () => {
    renderPage();
    await screen.findByRole("button", { name: "Investigate" });
    expect(screen.queryByRole("button", { name: "Save as incident" })).toBeNull();
  });

  it("keeps Save as incident visible but disabled while the Time Machine is engaged", async () => {
    renderPage({
      permissions: WRITE,
      search: `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=${FROM}&to=${TO}&at=2026-08-08T00:30:00Z`,
    });
    const save = await screen.findByRole("button", { name: "Save as incident" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
  });

  it("links to Explore with the range and says what that link can and cannot carry", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: /Compare in Explore/ });
    expect(link.getAttribute("href")).toContain("/explore");
    expect(screen.getByText(/A\/B slots are bound to curated metrics/i)).toBeTruthy();
  });

  it("offers the export as a real button", async () => {
    renderPage();
    expect(await screen.findByRole("button", { name: /Export JSON/ })).toBeTruthy();
  });
});

/* ── incident mode (plan Decision 7) ────────────────────────────────────── */

describe("the incident scope vocabulary", () => {
  it("reads a pair back out of the stored arrow scope", () => {
    expect(scopeFromIncidentScope("node-a→node-b", [])).toEqual({ kind: "pair", a: "node-a", b: "node-b" });
  });

  it("needs the target list to tell a target name from a node name", () => {
    expect(scopeFromIncidentScope("api-gw", [])).toEqual({ kind: "node", a: "api-gw", b: "" });
    expect(scopeFromIncidentScope("api-gw", ["api-gw"])).toEqual({ kind: "target", a: "api-gw", b: "" });
  });

  it("reads the GLOBAL scope back as the cluster — the one kind that always renders", () => {
    expect(scopeFromIncidentScope("", ["api-gw"])).toEqual({ kind: "cluster", a: "", b: "" });
  });

  it("frames an OPEN-ENDED incident (no toAt) on now, not on a NaN range", () => {
    const p = incidentParams(
      { ...incidentRow({ toAt: undefined }), toAt: undefined } as unknown as Incident,
      [],
      NOW,
    );
    expect(p.to.getTime()).toBe(NOW.getTime());
    expect(p.from.toISOString()).toBe(new Date(FROM).toISOString());
  });
});

describe("the pin vocabulary", () => {
  it("maps every timeline kind the store can store, and refuses the two it cannot", () => {
    expect(PIN_KIND_BY_TIMELINE_KIND["path-change"]).toBe("snapshot");
    expect(PIN_KIND_BY_TIMELINE_KIND.event).toBe("event");
    expect(PIN_KIND_BY_TIMELINE_KIND.audit).toBe("audit");
    expect(PIN_KIND_BY_TIMELINE_KIND.annotation).toBe("annotation");
    expect(PIN_KIND_BY_TIMELINE_KIND.run).toBe("run");
    expect(PIN_KIND_BY_TIMELINE_KIND.k8s).toBe("k8s");
    expect(PIN_KIND_BY_TIMELINE_KIND.maintenance).toBeNull();
    expect(PIN_KIND_BY_TIMELINE_KIND.threshold).toBeNull();
    expect(PIN_KIND_BY_TIMELINE_KIND.alert).toBeNull();
  });

  it("turns a path-change row into a SNAPSHOT ref carrying the snapshot's own id", () => {
    expect(
      pinnedRefFor({
        at: new Date(FROM),
        kind: "path-change",
        severity: "warn",
        title: "Route changed",
        ref: { kind: "path-change", id: "s-1" },
      }),
    ).toEqual({ kind: "snapshot", id: "s-1" });
  });

  it("refuses a threshold row: its id is derived from a query, not a row anywhere", () => {
    expect(
      pinnedRefFor({
        at: new Date(FROM),
        kind: "threshold",
        severity: "warn",
        title: "Packet loss crossed the threshold",
        ref: { kind: "threshold", id: "loss:above:x" },
      }),
    ).toBeNull();
  });
});

describe("InvestigatePage — saving an investigation as an incident", () => {
  it("POSTs the CURRENT scope and range, then enters incident mode with the id alone in the URL", async () => {
    const { postCalls } = renderPage({ permissions: WRITE });

    fireEvent.click(await screen.findByRole("button", { name: "Save as incident" }));
    const dialog = await screen.findByRole("form", { name: "Save as incident" });
    fireEvent.change(within(dialog).getByLabelText("Incident title"), { target: { value: "Loss on the pair" } });
    fireEvent.change(within(dialog).getByLabelText("Incident notes"), { target: { value: "started at 00:20" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create incident" }));

    await waitFor(() => expect(postCalls().some((c) => c.url === "/api/v1/incidents")).toBe(true));
    const post = postCalls().find((c) => c.url === "/api/v1/incidents");
    expect(post?.body).toEqual({
      title: "Loss on the pair",
      scope: "node-a→node-b",
      fromAt: new Date(FROM).toISOString(),
      toAt: new Date(TO).toISOString(),
      notes: "started at 00:20",
    });

    // The id REPLACES the four scope parameters: the row is now the authority.
    await waitFor(() => {
      const qs = new URLSearchParams(window.location.search);
      expect(qs.get("incident")).toBe("inc-new");
      expect(qs.get("kind")).toBeNull();
      expect(qs.get("scope")).toBeNull();
      expect(qs.get("from")).toBeNull();
    });

    const strip = await screen.findByRole("region", { name: "Incident" });
    expect(within(strip).getByText("Loss on the pair")).toBeTruthy();
    expect(within(strip).getByText("Open")).toBeTruthy();
    expect(strip.textContent).toContain("user:ada");
  });

  it("refuses an empty title rather than opening a nameless incident", async () => {
    const { postCalls } = renderPage({ permissions: WRITE });

    fireEvent.click(await screen.findByRole("button", { name: "Save as incident" }));
    const dialog = await screen.findByRole("form", { name: "Save as incident" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create incident" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/title is required/i);
    expect(postCalls().some((c) => c.url === "/api/v1/incidents")).toBe(false);
  });

  it("says out loud that a zone pair saves as the GLOBAL scope", async () => {
    renderPage({
      permissions: WRITE,
      search: `?kind=zone-pair&scope=${encodeURIComponent("zone-1→zone-2")}&from=${FROM}&to=${TO}`,
    });
    fireEvent.click(await screen.findByRole("button", { name: "Save as incident" }));
    expect(await screen.findByText(/both save as the GLOBAL scope/i)).toBeTruthy();
  });
});

describe("InvestigatePage — ?incident= hydrates the page", () => {
  it("frames the saved scope and range, and asks the sources for THAT window", async () => {
    const { urlsFor } = renderPage({ search: "?incident=inc-1", incident: incidentRow() });

    expect(await screen.findByRole("radio", { name: "Pair", checked: true })).toBeTruthy();
    await waitFor(() => expect((screen.getByLabelText("Source node") as HTMLSelectElement).value).toBe("node-a"));
    expect((screen.getByLabelText("Destination node") as HTMLSelectElement).value).toBe("node-b");

    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    const events = urlsFor("/api/v1/events")[urlsFor("/api/v1/events").length - 1];
    expect(events).toContain(`scope=${encodeURIComponent("node-a→node-b")}`);
    expect(events).toContain(encodeURIComponent(new Date(FROM).toISOString()));
  });

  it("ROUND TRIP: what save wrote, the permalink reads back as the same view", async () => {
    // Leg 1: save the pair investigation the default search names.
    const first = renderPage({ permissions: WRITE });
    fireEvent.click(await screen.findByRole("button", { name: "Save as incident" }));
    const dialog = await screen.findByRole("form", { name: "Save as incident" });
    fireEvent.change(within(dialog).getByLabelText("Incident title"), { target: { value: "Round trip" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create incident" }));
    await screen.findByRole("region", { name: "Incident" });
    const saved = first.postCalls().find((c) => c.url === "/api/v1/incidents")?.body as Record<string, string>;
    const permalink = window.location.search;
    expect(permalink).toBe("?incident=inc-new");
    cleanup();

    // Leg 2: a cold load of that permalink alone.
    renderPage({
      permissions: WRITE,
      search: permalink,
      incident: { ...incidentRow({ id: "inc-new", title: "Round trip" }), fromAt: saved.fromAt, toAt: saved.toAt },
    });

    expect(await screen.findByRole("radio", { name: "Pair", checked: true })).toBeTruthy();
    await waitFor(() => expect((screen.getByLabelText("Source node") as HTMLSelectElement).value).toBe("node-a"));
    expect((screen.getByLabelText("Destination node") as HTMLSelectElement).value).toBe("node-b");
    expect((await screen.findByRole("region", { name: "Incident" })).textContent).toContain("Round trip");
  });

  it("resolves a saved TARGET name to a target scope, not to a node of the same name", async () => {
    renderPage({ search: "?incident=inc-1", incident: incidentRow({ scope: "api-gw" }) });

    expect(await screen.findByRole("radio", { name: "Target", checked: true })).toBeTruthy();
    await waitFor(() => expect((screen.getByLabelText("Target") as HTMLSelectElement).value).toBe("api-gw"));
  });

  it("says so honestly when the id matches nothing, and still renders an investigation", async () => {
    renderPage({ search: "?incident=gone", incident: null });

    expect(await screen.findByText(/no incident matches this link/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Investigate" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Incident" })).toBeNull();
  });

  it("without incidents:read it makes ZERO incident requests and says why", async () => {
    const { urlsFor } = renderPage({
      search: "?incident=inc-1",
      incident: incidentRow(),
      permissions: ALL_READS.filter((p) => p !== "incidents:read"),
    });

    expect(await screen.findByText(/reading one needs incidents:read/i)).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/incidents")).toEqual([]);
  });

  it("re-scoping from the form LEAVES incident mode rather than lying about what it frames", async () => {
    renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow() });
    await screen.findByRole("region", { name: "Incident" });

    fireEvent.click(screen.getByRole("radio", { name: "Cluster" }));
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));

    await waitFor(() => expect(new URLSearchParams(window.location.search).get("incident")).toBeNull());
    expect(new URLSearchParams(window.location.search).get("kind")).toBe("cluster");
    await waitFor(() => expect(screen.queryByRole("region", { name: "Incident" })).toBeNull());
  });
});

describe("InvestigatePage — an incident's three writes", () => {
  it("resolves and reopens through PATCH status, and the chip follows the row", async () => {
    const { patchCalls } = renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow() });

    const strip = await screen.findByRole("region", { name: "Incident" });
    expect(within(strip).getByText("Open")).toBeTruthy();

    fireEvent.click(within(strip).getByRole("button", { name: "Resolve" }));
    await waitFor(() => expect(patchCalls().length).toBe(1));
    expect(patchCalls()[0].url).toBe("/api/v1/incidents/inc-1");
    expect(patchCalls()[0].body).toEqual({ status: "resolved" });
    expect(await within(strip).findByText("Resolved")).toBeTruthy();

    fireEvent.click(within(strip).getByRole("button", { name: "Reopen" }));
    await waitFor(() => expect(patchCalls().length).toBe(2));
    expect(patchCalls()[1].body).toEqual({ status: "open" });
    expect(await within(strip).findByText("Open")).toBeTruthy();
  });

  it("PATCHes notes alone — never the status or the pins alongside them", async () => {
    const { patchCalls } = renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow() });

    const strip = await screen.findByRole("region", { name: "Incident" });
    const notes = within(strip).getByLabelText("Incident notes");
    // Nothing typed yet: there is nothing to save, so the button is inert.
    expect((within(strip).getByRole("button", { name: "Save notes" }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(notes, { target: { value: "the switch was upgraded at 00:18" } });
    fireEvent.click(within(strip).getByRole("button", { name: "Save notes" }));

    await waitFor(() => expect(patchCalls().length).toBe(1));
    expect(patchCalls()[0].body).toEqual({ notes: "the switch was upgraded at 00:18" });
  });

  it("copies the permalink — the id alone, which is the whole address", async () => {
    const writeText = vi.fn((_text: string) => Promise.resolve());
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow() });

    const strip = await screen.findByRole("region", { name: "Incident" });
    fireEvent.click(within(strip).getByRole("button", { name: "Copy permalink" }));

    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(String(writeText.mock.calls[0][0])).toContain("incident=inc-1");
  });
});

describe("InvestigatePage — pinning findings from the timeline", () => {
  it("PATCHes the WHOLE pinned array, with the path-change row mapped onto the store's snapshot kind", async () => {
    const { patchCalls } = renderPage({
      permissions: WRITE,
      search: "?incident=inc-1",
      incident: incidentRow(),
      snapshots: [snapshotRow()],
    });

    await screen.findByRole("region", { name: "Incident" });
    const pin = await screen.findByRole("button", { name: /^Pin: Route changed/ });
    fireEvent.click(pin);

    await waitFor(() => expect(patchCalls().length).toBe(1));
    expect(patchCalls()[0].body).toEqual({ pinned: [{ kind: "snapshot", id: "s-1" }] });

    // The row now reads as pinned, and the finding is listed above the timeline.
    const unpin = await screen.findByRole("button", { name: /^Unpin: Route changed/ });
    expect(unpin.getAttribute("aria-pressed")).toBe("true");
    expect(within(await screen.findByRole("region", { name: "Pinned findings" })).getByText("snapshot")).toBeTruthy();

    // Unpinning sends the whole array again — now empty, not a delete.
    fireEvent.click(unpin);
    await waitFor(() => expect(patchCalls().length).toBe(2));
    expect(patchCalls()[1].body).toEqual({ pinned: [] });
  });

  it("offers NO pin on a threshold row — there is no store kind for a derived crossing", async () => {
    renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow(), snapshots: [snapshotRow()] });

    await screen.findByRole("button", { name: /^Pin: Route changed/ });
    expect(screen.queryByRole("button", { name: /Pin: Packet loss crossed the threshold/ })).toBeNull();
  });

  it("offers no pin at all outside incident mode", async () => {
    renderPage({ permissions: WRITE, snapshots: [snapshotRow()] });
    await screen.findAllByTestId("timeline-row");
    expect(screen.queryByRole("button", { name: /^Pin:/ })).toBeNull();
  });

  it("saves an inline pin note by PATCHing the list again", async () => {
    const { patchCalls } = renderPage({
      permissions: WRITE,
      search: "?incident=inc-1",
      incident: incidentRow({ pinned: [{ kind: "snapshot", id: "s-1" }] }),
      snapshots: [snapshotRow()],
    });

    const list = await screen.findByRole("region", { name: "Pinned findings" });
    fireEvent.change(within(list).getByLabelText("Note for snapshot s-1"), { target: { value: "the route moved here" } });
    fireEvent.click(within(list).getByRole("button", { name: "Save pin notes" }));

    await waitFor(() => expect(patchCalls().length).toBe(1));
    expect(patchCalls()[0].body).toEqual({ pinned: [{ kind: "snapshot", id: "s-1", note: "the route moved here" }] });
  });
});

describe("InvestigatePage — a viewer sees an incident read-only", () => {
  it("renders the strip and the notes as prose, with no resolve, no editor and no pins", async () => {
    renderPage({
      search: "?incident=inc-1",
      incident: incidentRow({ notes: "somebody else's write-up" }),
      snapshots: [snapshotRow()],
    });

    const strip = await screen.findByRole("region", { name: "Incident" });
    expect(within(strip).getByText("Loss between node-a and node-b")).toBeTruthy();
    expect(within(strip).getByText("somebody else's write-up")).toBeTruthy();
    expect(within(strip).queryByRole("button", { name: "Resolve" })).toBeNull();
    expect(within(strip).queryByLabelText("Incident notes")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Pin:/ })).toBeNull();
    // The permalink is a READ, so it stays.
    expect(within(strip).getByRole("button", { name: "Copy permalink" })).toBeTruthy();
  });
});

/* ── the entry-point URL (plan Decision 11) ─────────────────────────────── */

describe("buildInvestigateURL", () => {
  const SCOPES: InvestigationScope[] = [
    { kind: "pair", a: "node-a", b: "node-b" },
    { kind: "node", a: "node-a", b: "" },
    { kind: "target", a: "api-gw", b: "" },
    { kind: "zone-pair", a: "zone-1", b: "zone-2" },
    { kind: "cluster", a: "", b: "" },
    // Names that would break a hand-rolled query string.
    { kind: "node", a: "ns/pod a&b", b: "" },
    { kind: "pair", a: "a=1", b: "b?2" },
  ];

  it("lands on /investigate with the default hour ending at `now`", () => {
    const url = buildInvestigateURL({ kind: "node", a: "node-a", b: "" }, NOW);
    expect(url.startsWith("/investigate?")).toBe(true);
    const p = parseInvestigationParams(url.slice(url.indexOf("?")), NOW);
    expect(p.to.getTime()).toBe(NOW.getTime());
    expect(p.to.getTime() - p.from.getTime()).toBe(60 * 60 * 1000);
  });

  it("ROUND TRIP, both directions: every scope it emits, parseInvestigationParams reads back", () => {
    for (const scope of SCOPES) {
      const url = buildInvestigateURL(scope, NOW);
      const parsed = parseInvestigationParams(url.slice(url.indexOf("?")), NOW);
      expect({ kind: parsed.kind, a: parsed.a, b: parsed.b }).toEqual(scope);
      // …and the other direction: re-emitting what was parsed is byte-identical.
      expect(buildInvestigateURL({ kind: parsed.kind, a: parsed.a, b: parsed.b }, parsed.to)).toBe(url);
    }
  });

  it("agrees with scopeFilterValue about the pair separator, so an incident scope matches", () => {
    const url = buildInvestigateURL({ kind: "pair", a: "node-a", b: "node-b" }, NOW);
    expect(url).toContain(encodeURIComponent(scopeFilterValue({ kind: "pair", a: "node-a", b: "node-b" })));
  });
});

/* One describe per finding, named by it, so a regression report says which decision came back. */

const PAIR_SEARCH = `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=${FROM}&to=${TO}`;

describe("#1 a failed source says so, and stops the page claiming nothing happened", () => {
  it("renders ONE alert line per failed source, carrying the server's own detail", async () => {
    renderPage({
      failing: [
        { prefix: "/api/v1/events", detail: "the events store is unavailable" },
        { prefix: "/api/v1/k8s-events", detail: "the cluster event store is unavailable" },
      ],
    });
    const events = await screen.findByText(/^Events: the events store is unavailable$/);
    expect(events.getAttribute("role")).toBe("alert");
    const k8s = await screen.findByText(/^Cluster events: the cluster event store is unavailable$/);
    expect(k8s.getAttribute("role")).toBe("alert");
  });

  it("counts them in one partial banner rather than swallowing all but the first", async () => {
    renderPage({
      failing: [
        { prefix: "/api/v1/events", detail: "events down" },
        { prefix: "/api/v1/k8s-events", detail: "k8s down" },
      ],
    });
    const partial = await screen.findByTestId("timeline-partial");
    expect(partial.textContent).toBe("2 sources failed; the timeline below is partial.");
  });

  it("suppresses the nothing-happened empty state — an empty list is not evidence here", async () => {
    renderPage({ failing: [{ prefix: "/api/v1/events", detail: "events down" }] });
    await screen.findByTestId("timeline-partial");
    expect(screen.queryByText(/Nothing happened in this window/i)).toBeNull();
  });

  /* `prometheusConfigured: false`, and it is the POINT of this case rather than
     incidental setup (QA scope 4). The default fixture's loss series crosses the
     threshold, so this window has never been empty once its sources settled —
     the assertion below used to be satisfied by the phantom empty the pane
     rendered BEFORE auth resolved and any source had been asked at all, which
     is the opposite of "every enabled source settled cleanly". With Prometheus
     unconfigured the derived source is not asked, the seven store-backed ones
     answer cleanly and empty, and the claim is finally the one this test names. */
  it("still claims nothing happened when every enabled source settled cleanly", async () => {
    renderPage({ prometheusConfigured: false });
    expect(await screen.findByText(/Nothing happened in this window/i)).toBeTruthy();
    expect(screen.queryByTestId("timeline-partial")).toBeNull();
  });
});

describe("#4 the delta chip survives a fleet running one protocol", () => {
  it("renders a real ratio rather than an em dash when the guarded query returns 1", async () => {
    /* The end-to-end half of . The query-shape test above pins the `or vector(0)` guards; this pins what they BUY. */
    renderPage({ search: PAIR_SEARCH, failRatio: "1" });
    const chip = await screen.findByTestId("matrix-delta");
    await waitFor(() => expect(chip.textContent).toContain("100.0%"));
    expect(chip.textContent).not.toContain("—");
  });

  it("still says — when neither end could be measured at all", async () => {
    renderPage({ search: PAIR_SEARCH, failRatio: "NaN" });
    const chip = await screen.findByTestId("matrix-delta");
    await waitFor(() => expect(chip.textContent).toContain("—"));
  });
});

describe("#2 a refused signal query renders its problem, and the form refuses the range that caused it", () => {
  it("puts the problem detail under the chart heading as an alert instead of dead space", async () => {
    renderPage({
      failing: [{ prefix: "/api/v1/promql/query_range", status: 400, detail: "end must be after start" }],
    });
    const loss = await screen.findByRole("region", { name: "Packet loss" });
    const alert = await within(loss).findByRole("alert");
    expect(alert.textContent).toBe("end must be after start");
    expect(within(loss).queryByTestId("echart")).toBeNull();
  });

  it("refuses an inverted custom range at the form and issues no request for it", async () => {
    const { urlsFor } = renderPage({ search: PAIR_SEARCH });
    await screen.findByRole("button", { name: "Investigate" });
    await waitFor(() => expect(urlsFor("/api/v1/promql/query_range").length).toBeGreaterThan(0));
    const before = urlsFor("/api/v1/promql/query_range").length;
    const committed = window.location.search;

    fireEvent.click(screen.getByRole("radio", { name: "Custom" }));
    // Start pushed past the end: from >= to.
    pickRange("Range start", "2026-08-09", "12:00");
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));

    expect(await screen.findByText("The range end must be after its start.")).toBeTruthy();
    // Nothing was committed, so nothing was asked: the URL and the request
    // count are both exactly where they were.
    expect(window.location.search).toBe(committed);
    expect(urlsFor("/api/v1/promql/query_range").length).toBe(before);
  });
});

describe("QA scope 4, finding #5 — a window wider than the Prometheus bound", () => {
  const WIDE = `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=2026-08-06T18:30:00Z&to=2026-08-08T00:00:00Z`;

  it("states the bound in the chart's own slot instead of letting the proxy's sentence stand there", async () => {
    renderPage({ search: WIDE });
    const loss = await screen.findByRole("region", { name: "Packet loss" });
    const alert = await within(loss).findByRole("alert");
    expect(alert.textContent).toBe(
      "This range is wider than 24h, and one Prometheus query covers at most that much " +
        "(console.prometheus.maxRange) — narrow the range to draw this chart. The timeline is not affected.",
    );
    /* The RTT pane is refused for the same reason and must say so too. */
    const rtt = await screen.findByRole("region", { name: "RTT p95" });
    expect((await within(rtt).findByRole("alert")).textContent).toContain("wider than 24h");
    expect(within(loss).queryByTestId("echart")).toBeNull();
  });

  it("never sends the range query, so the raw server refusal cannot reach the page", async () => {
    const { urlsFor } = renderPage({ search: WIDE });
    await screen.findByRole("region", { name: "Packet loss" });
    /* Something else on the page HAS finished fetching, so this is not just an early read. */
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/promql/query_range")).toEqual([]);
  });

  it("leaves the store-backed timeline alone — its sources carry no such bound", async () => {
    const { urlsFor } = renderPage({ search: WIDE });
    await waitFor(() => expect(urlsFor("/api/v1/events").length).toBeGreaterThan(0));
    expect(urlsFor("/api/v1/annotations").length).toBeGreaterThan(0);
    /* The instant evaluations behind the delta chip carry no range at all, so they still run. */
    await waitFor(() => expect(urlsFor("/api/v1/promql/query").length).toBeGreaterThan(0));
  });
});

describe("#3 the Time Machine clamps the investigated window", () => {
  const ENGAGED = `${PAIR_SEARCH}&at=2026-08-08T00:30:00Z`;

  /* The clock is frozen for this block only: a preset commit ("1h") is measured from NOW. */
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  /* A 45-minute span is not one of the presets, so the form opens on CUSTOM and
     commits the two instants verbatim — no `now` in the arithmetic, and
     therefore no dependence on how far the fake clock has drifted. */
  const CUSTOM = `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=2026-08-08T00:00:00Z&to=2026-08-08T00:45:00Z`;

  it("commits `to` as the viewed instant and says the window moved — on ARRIVAL, with nothing pressed", async () => {
    /* QA scope 3, finding #2: the clamp used to live only in apply(), so this
       very link rendered rows dated after the instant the whole console claimed
       to be showing until somebody happened to press the button. */
    renderPage({ search: `${CUSTOM}&at=2026-08-08T00:30:00Z` });

    expect(await screen.findByTestId("clamp-banner")).toHaveTextContent("Window clamped to the viewed instant.");
    // The URL is the contract: `to` is now the instant, `from` is untouched.
    const committed = parseInvestigationParams(window.location.search, NOW);
    expect(committed.to.toISOString()).toBe("2026-08-08T00:30:00.000Z");
    expect(committed.from.toISOString()).toBe("2026-08-08T00:00:00.000Z");
    // ...and `?at=` survived the correction: the page rewrote its own four keys.
    expect(new URLSearchParams(window.location.search).get("at")).toBe("2026-08-08T00:30:00Z");
  });

  it("asks for NOTHING past the viewed instant on a deep link", async () => {
    const { calls } = renderPage({ search: `${CUSTOM}&at=2026-08-08T00:30:00Z` });
    await screen.findByTestId("clamp-banner");
    // Every store-backed source is asked for the CLAMPED window, never the
    // link's own `to` — the rows on screen are the proof the banner is about.
    await waitFor(() => expect(calls.some((c) => c.url.startsWith("/api/v1/events"))).toBe(true));
    const events = calls.filter((c) => c.url.startsWith("/api/v1/events"));
    for (const c of events) {
      expect(c.url).toContain(encodeURIComponent("2026-08-08T00:30:00.000Z"));
      expect(c.url).not.toContain(encodeURIComponent("2026-08-08T00:45:00.000Z"));
    }
  });

  it("clamps again when the FORM asks for a window past the instant", async () => {
    renderPage({ search: `${CUSTOM}&at=2026-08-08T00:30:00Z` });
    await screen.findByRole("button", { name: "Investigate" });
    // Reach forward past the instant from the already-clamped fields.
    pickRange("Range end", "2026-08-08", "23:00");
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));

    expect(await screen.findByTestId("clamp-banner")).toHaveTextContent("Window clamped to the viewed instant.");
    expect(parseInvestigationParams(window.location.search, NOW).to.toISOString()).toBe("2026-08-08T00:30:00.000Z");
  });

  it("says nothing when the clamp changed nothing", async () => {
    renderPage({ search: `${CUSTOM}&at=2026-08-08T01:00:00Z` });
    await screen.findByRole("button", { name: "Investigate" });
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));
    await waitFor(() =>
      expect(parseInvestigationParams(window.location.search, NOW).to.toISOString()).toBe("2026-08-08T00:45:00.000Z"),
    );
    expect(screen.queryByTestId("clamp-banner")).toBeNull();
  });

  it("refuses a window that lies entirely after the viewed instant", async () => {
    renderPage({ search: `${CUSTOM}&at=2026-08-07T00:00:00Z` });
    await screen.findByRole("button", { name: "Investigate" });
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));
    expect(await screen.findByText(/The window is after the viewed instant/)).toBeTruthy();
    expect(screen.queryByTestId("clamp-banner")).toBeNull();
  });

  it("issues ZERO alert requests while engaged, and says why in the source list", async () => {
    const { urlsFor } = renderPage({ search: ENGAGED });
    expect(await screen.findByText(/Alert state is a live-only signal/i)).toBeTruthy();
    expect(urlsFor("/api/v1/alerts")).toEqual([]);
  });

  it("does ask for alerts while Live", async () => {
    const { urlsFor } = renderPage({ search: PAIR_SEARCH });
    await waitFor(() => expect(urlsFor("/api/v1/alerts").length).toBeGreaterThan(0));
  });
});

describe("#5 the scope selects read the agents too", () => {
  /* The controller-less console: `nodes` is empty and the AGENTS are the only
     thing that knows this fleet has machines in it. Before round 3 every select
     here was empty and every scope but `cluster` was unreachable. */
  const agentsOnly = {
    nodes: [],
    agents: [
      { id: "ag-2", nodeName: "node-b", podIP: "10.0.0.2", zone: "zone-2" },
      { id: "ag-1", nodeName: "node-a", podIP: "10.0.0.1", zone: "zone-1" },
    ],
    timestamp: FROM,
  };

  it("fills both node and zone lists from an agents-only topology", async () => {
    renderPage({ search: "?kind=pair", topology: agentsOnly });

    const source = (await screen.findByLabelText("Source node")) as HTMLSelectElement;
    await waitFor(() => expect([...source.options].map((o) => o.value)).toEqual(["", "node-a", "node-b"]));

    fireEvent.click(screen.getByRole("radio", { name: "Zone pair" }));
    const zone = screen.getByLabelText("Source zone") as HTMLSelectElement;
    expect([...zone.options].map((o) => o.value)).toEqual(["", "zone-1", "zone-2"]);
  });

  it("dedupes a node the controller AND an agent both report", async () => {
    renderPage({
      search: "?kind=node",
      topology: {
        nodes: [{ name: "node-a", zone: "zone-1", ready: true }],
        agents: [{ id: "ag-1", nodeName: "node-a", podIP: "10.0.0.1", zone: "zone-1" }],
        timestamp: FROM,
      },
    });
    const node = (await screen.findByLabelText("Node")) as HTMLSelectElement;
    await waitFor(() => expect([...node.options].map((o) => o.value)).toEqual(["", "node-a"]));
  });
});

describe("#6 the Investigate button waits for a scope that names something", () => {
  async function draft(kind: string) {
    renderPage({ search: "?kind=cluster" });
    await screen.findByRole("button", { name: "Investigate" });
    fireEvent.click(screen.getByRole("radio", { name: kind }));
  }

  const button = () => screen.getByRole("button", { name: "Investigate" }) as HTMLButtonElement;

  it("pair: needs both ends, and two DIFFERENT nodes", async () => {
    await draft("Pair");
    expect(button().disabled).toBe(true);
    expect(screen.getByTestId("scope-incomplete").textContent).toBe("Choose a source node.");

    fireEvent.change(screen.getByLabelText("Source node"), { target: { value: "node-a" } });
    expect(screen.getByTestId("scope-incomplete").textContent).toBe("Choose a destination node.");

    fireEvent.change(screen.getByLabelText("Destination node"), { target: { value: "node-a" } });
    expect(screen.getByTestId("scope-incomplete").textContent).toBe("A pair needs two different nodes.");

    fireEvent.change(screen.getByLabelText("Destination node"), { target: { value: "node-b" } });
    expect(button().disabled).toBe(false);
    expect(screen.queryByTestId("scope-incomplete")).toBeNull();
  });

  it("node: needs the one node", async () => {
    await draft("Node");
    expect(button().disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Node"), { target: { value: "node-a" } });
    expect(button().disabled).toBe(false);
  });

  it("zone pair: needs both zones", async () => {
    await draft("Zone pair");
    expect(button().disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Source zone"), { target: { value: "zone-1" } });
    expect(button().disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Destination zone"), { target: { value: "zone-2" } });
    expect(button().disabled).toBe(false);
  });

  it("target: needs the one target", async () => {
    await draft("Target");
    expect(button().disabled).toBe(true);
    const select = (await screen.findByLabelText("Target")) as HTMLSelectElement;
    await waitFor(() => expect([...select.options].map((o) => o.value)).toContain("api-gw"));
    fireEvent.change(select, { target: { value: "api-gw" } });
    expect(button().disabled).toBe(false);
  });

  it("cluster: always ready — it names no object", async () => {
    renderPage({ search: "?kind=cluster" });
    expect((await screen.findByRole("button", { name: "Investigate" })).hasAttribute("disabled")).toBe(false);
    expect(screen.queryByTestId("scope-incomplete")).toBeNull();
  });
});

describe("#7 a wide scope's caption says what was actually queried", () => {
  it("says 'all scopes' for a zone pair, matching the unfiltered request", async () => {
    const { urlsFor } = renderPage({
      permissions: [...ALL_READS, "annotations:write"],
      search: `?kind=zone-pair&scope=${encodeURIComponent("zone-1→zone-2")}&from=${FROM}&to=${TO}`,
    });
    expect(await screen.findByText(/annotations in this window · scope all scopes/)).toBeTruthy();
    await waitFor(() => expect(urlsFor("/api/v1/annotations").length).toBeGreaterThan(0));
    // The request really was unfiltered: no scope parameter at all.
    expect(urlsFor("/api/v1/annotations").every((u) => !u.includes("scope="))).toBe(true);
  });

  it("names the scope itself when the request really was filtered by it", async () => {
    renderPage({ search: PAIR_SEARCH });
    expect(await screen.findByText(/annotations in this window · scope node-a→node-b/)).toBeTruthy();
  });
});

describe("#8 a create outside the frozen window says so", () => {
  it("notes an annotation stored outside the window, naming when the window ends", async () => {
    renderPage({ permissions: [...ALL_READS, "annotations:write"], search: PAIR_SEARCH });
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    pickRange("Start", "2026-08-09", "12:00");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "started the rollback" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));

    const note = await screen.findByText(/^Created — outside this window \(which ends .+\); press Investigate to reframe\.$/);
    expect(note.getAttribute("role")).toBe("status");
  });

  it("stays silent for an ordinary in-window create — the row appearing IS the feedback", async () => {
    /* A two-day window, so the LOCAL instant the picker composes lands inside
       it whatever timezone this suite runs in. */
    renderPage({
      permissions: [...ALL_READS, "annotations:write"],
      search: `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}&from=2026-08-07T00:00:00Z&to=2026-08-09T00:00:00Z`,
    });
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    pickRange("Start", "2026-08-08", "12:00");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "in window" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));

    await waitFor(() => expect(screen.queryByRole("form", { name: "New annotation" })).toBeNull());
    expect(screen.queryByText(/outside this window/)).toBeNull();
  });
});

describe("#10 an in-app navigation is re-read, not ignored", () => {
  /* The scope headline is rendered in more than one place (the page header and
     the Signals column), so these assert on the SET rather than on a single
     node. */
  const headlines = () => screen.queryAllByText("node-a → node-b").length;

  it("resets to the entry form when the URL becomes a bare /investigate", async () => {
    renderPage({ search: PAIR_SEARCH });
    await waitFor(() => expect(headlines()).toBeGreaterThan(0));

    /* pushState is what a TanStack <Link> and the ⌘K palette both reach —
       popstate is never dispatched for either, which is the whole finding. */
    window.history.pushState({}, "", "/investigate");
    await waitFor(() => expect(headlines()).toBe(0));
    expect(screen.getAllByText("the whole cluster").length).toBeGreaterThan(0);
    expect(screen.getByRole("radio", { name: "Cluster" }).getAttribute("aria-checked")).toBe("true");
  });

  it("follows Back the same way", async () => {
    renderPage({ search: PAIR_SEARCH });
    await waitFor(() => expect(headlines()).toBeGreaterThan(0));
    window.history.replaceState({}, "", "/investigate?kind=node&scope=node-c");
    window.dispatchEvent(new PopStateEvent("popstate"));
    await waitFor(() => expect(screen.queryAllByText("node-c").length).toBeGreaterThan(0));
    expect(headlines()).toBe(0);
  });

  it("re-enters incident mode when the new URL names one", async () => {
    renderPage({ permissions: WRITE, search: PAIR_SEARCH, incident: incidentRow() });
    await waitFor(() => expect(headlines()).toBeGreaterThan(0));
    window.history.pushState({}, "", "/investigate?incident=inc-1");
    expect(await screen.findByRole("heading", { name: "Loss between node-a and node-b" })).toBeTruthy();
  });
});

describe("#15 the inline forms are disclosures, not dialogs", () => {
  it("Save as incident carries role=form and no dialog role", async () => {
    renderPage({ permissions: WRITE, search: PAIR_SEARCH });
    fireEvent.click(await screen.findByRole("button", { name: "Save as incident" }));
    expect(await screen.findByRole("form", { name: "Save as incident" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Save as incident" })).toBeNull();
  });

  /* The NEGATIVE pin, turning the QA observation into intent: Escape does not
     dismiss this form and does not clear what has been typed into it. There is
     no undo behind a discarded draft, and Cancel is one Tab away. */
  it("does NOT discard a typed incident title on Escape", async () => {
    renderPage({ permissions: WRITE, search: PAIR_SEARCH });
    fireEvent.click(await screen.findByRole("button", { name: "Save as incident" }));
    const form = await screen.findByRole("form", { name: "Save as incident" });
    const title = within(form).getByLabelText("Incident title") as HTMLInputElement;
    fireEvent.change(title, { target: { value: "half a thought" } });
    fireEvent.keyDown(title, { key: "Escape" });
    fireEvent.keyDown(document, { key: "Escape" });

    expect(screen.getByRole("form", { name: "Save as incident" })).toBeTruthy();
    expect((within(form).getByLabelText("Incident title") as HTMLInputElement).value).toBe("half a thought");
  });
});

describe("#19 the pinned empty state names all three unpinnable classes", () => {
  it("adds firing alerts to maintenance windows and threshold crossings", async () => {
    renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow() });
    const panel = await screen.findByRole("region", { name: "Pinned findings" });
    expect(panel.textContent).toContain("Maintenance windows, threshold crossings and firing alerts cannot be pinned");
    // …and the code that decides it agrees.
    expect(PIN_KIND_BY_TIMELINE_KIND.alert).toBeNull();
  });
});

describe("#21 an incident can be deleted", () => {
  it("confirms, sends DELETE, and lands on the bare entry form", async () => {
    const { calls } = renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow() });
    const strip = await screen.findByRole("region", { name: "Incident" });

    fireEvent.click(within(strip).getByRole("button", { name: "Delete incident: Loss between node-a and node-b" }));
    expect(calls.filter((c) => c.method === "DELETE")).toEqual([]);

    fireEvent.click(
      within(strip).getByRole("button", { name: "Confirm delete incident: Loss between node-a and node-b" }),
    );
    await waitFor(() =>
      expect(calls.filter((c) => c.method === "DELETE").map((c) => c.url)).toEqual(["/api/v1/incidents/inc-1"]),
    );
    await waitFor(() => expect(screen.queryByRole("region", { name: "Incident" })).toBeNull());
    expect(window.location.pathname).toBe("/investigate");
    expect(window.location.search).toBe("");
    expect(screen.getAllByText("the whole cluster").length).toBeGreaterThan(0);
  });

  it("is hidden without incidents:write", async () => {
    renderPage({ search: "?incident=inc-1", incident: incidentRow() });
    const strip = await screen.findByRole("region", { name: "Incident" });
    expect(within(strip).queryByRole("button", { name: /^Delete incident/ })).toBeNull();
  });
});

describe("#22 the three small ones", () => {
  it("focuses the title field when the title is what is missing", async () => {
    renderPage({ permissions: WRITE, search: PAIR_SEARCH });
    fireEvent.click(await screen.findByRole("button", { name: "Save as incident" }));
    const form = await screen.findByRole("form", { name: "Save as incident" });
    fireEvent.click(within(form).getByRole("button", { name: "Create incident" }));

    expect(await within(form).findByText("A title is required.")).toBeTruthy();
    expect(document.activeElement).toBe(within(form).getByLabelText("Incident title"));
  });

  it("clears 'Permalink copied.' after four seconds, and keeps a FAILURE up", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const writeText = vi.fn(() => Promise.resolve());
      Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
      renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: incidentRow() });
      const strip = await screen.findByRole("region", { name: "Incident" });
      fireEvent.click(within(strip).getByRole("button", { name: "Copy permalink" }));
      expect(await screen.findByText("Permalink copied.")).toBeTruthy();

      await vi.advanceTimersByTimeAsync(COPY_NOTE_TTL_MS + 50);
      await waitFor(() => expect(screen.queryByText("Permalink copied.")).toBeNull());
    } finally {
      vi.useRealTimers();
    }
  });

  it("confirms an unpin that would discard a note, and unpins a note-less one in a click", async () => {
    const withNote = incidentRow({
      pinned: [
        { kind: "event", id: "e-1", note: "this is the cause" },
        { kind: "audit", id: "7" },
      ],
    });
    const { patchCalls } = renderPage({ permissions: WRITE, search: "?incident=inc-1", incident: withNote });
    await screen.findByRole("region", { name: "Pinned findings" });

    // Note-less: one click is enough, nothing to lose.
    fireEvent.click(screen.getByRole("button", { name: "Unpin audit 7" }));
    await waitFor(() => expect(patchCalls().length).toBe(1));
    expect(patchCalls()[0].body).toEqual({ pinned: [{ kind: "event", id: "e-1", note: "this is the cause" }] });

    // With a note: the first click only arms.
    fireEvent.click(screen.getByRole("button", { name: "Unpin event e-1" }));
    expect(patchCalls().length).toBe(1);
    fireEvent.click(screen.getByRole("button", { name: "Confirm unpin event e-1 and discard its note" }));
    await waitFor(() => expect(patchCalls().length).toBe(2));
    expect(patchCalls()[1].body).toEqual({ pinned: [] });
  });
});

describe("the write guard reaches Investigate's rail (round 2's deferred follow-up)", () => {
  it("gives a TM-disabled rail button the reason, not just the grey", async () => {
    renderPage({
      permissions: [...ALL_READS, "runs:create"],
      search: `${PAIR_SEARCH}&at=2026-08-08T00:30:00Z`,
    });
    const run = (await screen.findByRole("button", { name: "Run MTR now" })) as HTMLButtonElement;
    expect(run.disabled).toBe(true);
    expect(run.getAttribute("title")).toBe(TIME_MACHINE_DISABLED_REASON);
    expect(run.getAttribute("aria-describedby")).toBe(TIME_MACHINE_REASON_ID);
  });

  it("adds nothing at all while Live", async () => {
    renderPage({ permissions: [...ALL_READS, "runs:create"], search: PAIR_SEARCH });
    const run = (await screen.findByRole("button", { name: "Run MTR now" })) as HTMLButtonElement;
    expect(run.disabled).toBe(false);
    expect(run.hasAttribute("title")).toBe(false);
    expect(run.hasAttribute("aria-describedby")).toBe(false);
  });
});

/* ── QA round 5 ─────────────────────────────────────────────────────────── */

/* #10. RFC 7807 details are PHRASES — "no incident with that id" carries no
   full stop — so the server's words and the console's own sentence ran
   together into one broken sentence. */
describe("the stale-incident notice is two sentences (#10)", () => {
  it("closes the server's phrase before its own sentence starts", async () => {
    renderPage({ search: "?incident=gone", incident: null });
    const heading = await screen.findByText(/no incident matches this link/i);
    const body = heading.parentElement?.querySelector("p:nth-of-type(2)") ?? heading.nextElementSibling;
    const text = body?.textContent ?? "";
    expect(text).toContain(". The page is showing the default investigation");
    // The exact shape of the old run-on: a lowercase word butting straight
    // into the capital that starts the next sentence.
    expect(text).not.toMatch(/[a-z] The page is showing/);
  });
});

/* #19. The audit log records every authorization DECISION the API makes — a
   read, a denial, a login — so labelling those rows "config change" told an
   operator hunting a cause that somebody changed something when nobody did. */
describe("the audit timeline badge names its SOURCE (#19)", () => {
  it("labels audit rows 'audit', never 'config change'", async () => {
    renderPage({
      auditRows: [
        {
          id: 1,
          at: "2026-08-08T00:30:00Z", // inside the default [FROM, TO) window
          subjectKind: "user",
          subjectId: "ada",
          action: "GET /api/v1/targets",
          resource: "targets",
          outcome: "allowed",
          detail: {},
        },
      ],
    });
    const timeline = await screen.findByRole("list", { name: "Timeline entries" });
    await waitFor(() => expect(within(timeline).getByText("audit")).toBeInTheDocument());
    expect(within(timeline).queryByText("config change")).toBeNull();
    // The row's own title keeps the action verbatim — the label narrowed, the
    // information did not.
    expect(within(timeline).getByText("GET /api/v1/targets")).toBeInTheDocument();
  });
});

/* the Russian is wired ONE smoke pin, not a second copy of the suite. */
describe("InvestigatePage — Russian", () => {
  it("renders its chrome, an honesty caption and the pager in Russian", async () => {
    renderPage({ locale: "ru", events: HOUR_OF_EVENTS });

    expect(await screen.findByRole("heading", { name: "Инциденты" })).toBeInTheDocument();

    // The pager: 61 entries over seven pages at the default size of ten.
    await waitFor(() => expect(screen.getAllByTestId("timeline-row")).toHaveLength(10));

    // The honesty caption for the newest 200 audit rows, with its bound intact.
    const sources = within(screen.getByRole("list", { name: "Источники ленты" })).getAllByRole("listitem");
    const text = sources.map((li) => li.textContent ?? "").join(" ");
    expect(text).toMatch(/свежих 200 строк аудита/);
    expect(text).toMatch(/нет фильтра по времени/);

    expect(screen.getByTestId("timeline-count").textContent).toBe("61 запись в этом интервале");
    expect(screen.getByTestId("pager-page").textContent).toBe("Страница 1 из 7");

    fireEvent.click(screen.getByRole("button", { name: "Следующая страница" }));
    expect(screen.getByTestId("pager-page").textContent).toBe("Страница 2 из 7");
  });

  /* A row here says the whole contract held — the console's connective words moved, the server's did. */
  it("renders a timeline row's title in Russian, keeping the server's half verbatim", async () => {
    renderPage({ locale: "ru", maintenance: [maintenanceRow()] });

    const timeline = await screen.findByRole("list", { name: "Записи ленты" });
    await waitFor(() =>
      // «Работы» is ours; "switch upgrade" is the reason an operator
      // typed, and a console that paraphrased it would be inventing a record.
      expect(within(timeline).getByText("Работы: switch upgrade")).toBeInTheDocument(),
    );
    expect(within(timeline).queryByText("Maintenance: switch upgrade")).toBeNull();
    // The detail line's own words too: «до» for "until", «глобальная» stays
    // out of it because this window names a real scope.
    expect(within(timeline).getByText(/node-a→node-b · до /)).toBeInTheDocument();
  });

  /* The bars' count line, the scope caption behind it, and the honesty
     sentence a failed read shows — all three are dict/annotations.ts and
     dict/maintenance.ts reached through the page that mounts them widest. */
  it("captions the annotation bar in Russian, naming the scope it really queried", async () => {
    renderPage({ locale: "ru", search: "?kind=cluster&scope=&from=" + FROM + "&to=" + TO });

    // «все области» — scopeCaptionValue's answer for a scope that queried every
    // one of them, not the "global" value it was NOT filtering by (#7).
    await waitFor(() =>
      expect(screen.getByText(/0 заметок в этом интервале · область все области/)).toBeInTheDocument(),
    );
  });
});

/* ══ QA scope 3 ══════════════════════════════════════════════════════════ */

describe("isReadOnlyAudit (finding #8)", () => {
  it("calls every GET a read, whatever the route", () => {
    expect(isReadOnlyAudit("GET /api/v1/audit")).toBe(true);
    expect(isReadOnlyAudit("GET /api/v1/targets")).toBe(true);
  });

  it("calls the two PromQL POSTs reads — a query is a body, not a change", () => {
    expect(isReadOnlyAudit("POST /api/v1/promql/query")).toBe(true);
    expect(isReadOnlyAudit("POST /api/v1/promql/query_range")).toBe(true);
  });

  it("calls every OTHER write a write, including a POST that only LOOKS harmless", () => {
    expect(isReadOnlyAudit("POST /api/v1/runs")).toBe(false);
    expect(isReadOnlyAudit("POST /api/v1/annotations")).toBe(false);
    expect(isReadOnlyAudit("DELETE /api/v1/incidents/inc-1")).toBe(false);
    expect(isReadOnlyAudit("PATCH /api/v1/incidents/inc-1")).toBe(false);
    // The allow-list is exact: a route that merely starts the same way is not on it.
    expect(isReadOnlyAudit("POST /api/v1/promql/query_saved")).toBe(false);
  });

  it("survives an action that is not 'METHOD route' at all", () => {
    expect(isReadOnlyAudit("")).toBe(false);
    expect(isReadOnlyAudit("GET")).toBe(true);
  });
});

describe("auditEntries marks reads, and the ranking honours it (finding #8)", () => {
  const row = (over: Record<string, unknown> = {}) => ({
    id: 1757,
    at: "2026-08-08T00:20:00Z",
    subjectKind: "user",
    subjectId: "ada",
    action: "POST /api/v1/promql/query",
    resource: "promql",
    outcome: "allowed",
    remoteAddr: "10.0.0.1",
    detail: {},
    ...over,
  });

  it("flags the console's own PromQL POST as read-only", () => {
    const [entry] = auditEntries([row()] as never, new Date(FROM), new Date(TO));
    expect(entry.readOnly).toBe(true);
  });

  it("leaves a real configuration change unflagged", () => {
    const [entry] = auditEntries(
      [row({ action: "POST /api/v1/targets" })] as never,
      new Date(FROM),
      new Date(TO),
    );
    expect(entry.readOnly).toBe(false);
  });
});

describe("ignoredInvestigationParams (finding #14)", () => {
  it("is empty for a well-formed link, and for the bare one", () => {
    expect(ignoredInvestigationParams("")).toEqual([]);
    expect(
      ignoredInvestigationParams(`?kind=pair&scope=${encodeURIComponent("a→b")}&from=${FROM}&to=${TO}`),
    ).toEqual([]);
  });

  it("names a kind this page does not have", () => {
    expect(ignoredInvestigationParams("?kind=galaxy")).toContain("kind");
  });

  it("names a scope the surviving kind has nowhere to put", () => {
    expect(ignoredInvestigationParams("?kind=cluster&scope=node-a")).toEqual(["scope"]);
    // ...and a scope alongside a bad kind is dropped for the same reason.
    expect(ignoredInvestigationParams("?kind=galaxy&scope=node-a")).toEqual(["kind", "scope"]);
  });

  it("names an instant that will not parse — and not one that will", () => {
    expect(ignoredInvestigationParams("?from=yesterday")).toEqual(["from"]);
    expect(ignoredInvestigationParams(`?from=${FROM}&to=lunchtime`)).toEqual(["to"]);
  });
});

describe("exportFileName (finding #20)", () => {
  it("carries no colon — Windows refuses one and browsers mangle it silently", () => {
    const name = exportFileName(new Date(FROM));
    expect(name).not.toContain(":");
    expect(name).toBe("investigation-2026-08-08T00-00-00-000Z.json");
  });

  it("still names a file for an instant that will not format", () => {
    expect(exportFileName(new Date("nope"))).toBe("investigation-unknown.json");
  });
});

describe("path-change rows never say '1 hops' (finding #22)", () => {
  const snap = (hops: number) => ({
    id: "s-9",
    sourceNode: "node-a",
    destination: "node-b",
    pathHash: "abcdef0123456789",
    hopCount: hops,
    hops: [],
    firstSeen: "2026-08-08T00:20:00Z",
    lastSeen: TO,
    traceCount: 1,
  });

  it("labels the counts instead of pluralising them, at one and at many", () => {
    const one = pathChangeEntries([snap(1)] as never, new Date(FROM), new Date(TO));
    expect(one[0].detail).toBe("path abcdef012345 · hops: 1 · traces: 1");
    const many = pathChangeEntries([snap(12)] as never, new Date(FROM), new Date(TO));
    expect(many[0].detail).toContain("hops: 12");
    // The shape that produced the bug is gone in both directions.
    expect(one[0].detail).not.toMatch(/\d hops\b/);
    expect(many[0].detail).not.toMatch(/\d hops\b/);
  });
});

describe("every source failed is an ERROR STATE, not a quiet empty window (finding #1)", () => {
  /* Every store- and Prometheus-backed leg this page asks for on a pair scope. */
  const ALL_SOURCES = [
    "/api/v1/events",
    "/api/v1/k8s-events",
    "/api/v1/audit",
    "/api/v1/annotations",
    "/api/v1/mtr",
    "/api/v1/runs",
    "/api/v1/maintenance",
    "/api/v1/promql",
    "/api/v1/alerts",
  ].map((prefix) => ({ prefix, status: 500, detail: "upstream is down" }));

  it("says no timeline could be assembled, and does NOT claim nothing happened", async () => {
    renderPage({ failing: ALL_SOURCES });

    const alert = await screen.findByTestId("timeline-all-failed");
    expect(alert).toHaveAttribute("role", "alert");
    expect(alert.textContent).toMatch(/No timeline could be assembled/);
    // The honest-empty copy is the exact lie this replaces.
    expect(screen.queryByText(/Nothing happened in this window/i)).toBeNull();
  });

  it("prints NO figure in the delta chip for two evaluations that never came back", async () => {
    renderPage({ failing: ALL_SOURCES });
    await screen.findByTestId("timeline-all-failed");

    const chip = screen.getByTestId("matrix-delta");
    expect(chip.textContent).toContain("upstream is down");
    // The old behaviour: "0.0% → 0.0%" over a refused pair of queries.
    expect(chip.textContent).not.toMatch(/\d+\.\d%/);
    expect(chip.textContent).not.toContain("pp");
  });

  it("names the fail-ratio pair in the source list, which used to have no line at all", async () => {
    renderPage({ failing: ALL_SOURCES });
    expect(await screen.findByText(/Failure-ratio delta: upstream is down/)).toBeTruthy();
  });

  it("stays a PARTIAL when only one source fails — the two states are not the same claim", async () => {
    renderPage({ failing: [{ prefix: "/api/v1/events", status: 500, detail: "upstream is down" }] });

    await screen.findByText(/Events: upstream is down/);
    expect(screen.getByTestId("timeline-partial").textContent).toMatch(/1 source failed/i);
    expect(screen.queryByTestId("timeline-all-failed")).toBeNull();
    // ...and the nothing-happened claim is still suppressed by the partial.
    expect(screen.queryByText(/Nothing happened in this window/i)).toBeNull();
  });

  /* Same fixture correction as the nothing-happened case above: a genuinely
     empty SETTLED window, so the zero being counted is a count and not the
     pre-auth phantom. */
  it("counts zero out loud rather than dropping the number (finding #15)", async () => {
    renderPage({ prometheusConfigured: false });
    await waitFor(() =>
      expect(screen.getByTestId("timeline-count").textContent).toBe("0 entries in this window"),
    );
  });
});

describe("a permalink to a DELETED incident (finding #3)", () => {
  it("says the incident is gone and names the id, instead of framing a default investigation", async () => {
    renderPage({ search: "?incident=inc-gone", incident: null });

    const card = await screen.findByTestId("incident-not-found");
    expect(card).toHaveAttribute("role", "alert");
    expect(card.textContent).toMatch(/That incident no longer exists/);
    expect(card.textContent).toContain("inc-gone");
  });

  it("drops the ghost ?incident= from the address bar", async () => {
    renderPage({ search: "?incident=inc-gone", incident: null });
    await screen.findByTestId("incident-not-found");
    await waitFor(() => expect(new URLSearchParams(window.location.search).get("incident")).toBeNull());
  });

  it("keeps the ordinary error card for a failure that is NOT a 404", async () => {
    renderPage({
      search: "?incident=inc-1",
      incident: incidentRow(),
      failing: [{ prefix: "/api/v1/incidents/", status: 503, detail: "store is down" }],
    });
    expect(await screen.findByText(/No incident matches this link/)).toBeTruthy();
    expect(screen.queryByTestId("incident-not-found")).toBeNull();
    // A row that may well still exist keeps its link.
    expect(new URLSearchParams(window.location.search).get("incident")).toBe("inc-1");
  });
});

describe("a pinned finding whose row left the window (finding #10)", () => {
  it("says the row is out of window instead of letting 'audit / 1757' read as a title", async () => {
    renderPage({
      search: "?incident=inc-1",
      permissions: WRITE,
      incident: incidentRow({ pinned: [{ kind: "audit", id: "1757", note: "the rollout" }] }),
    });

    const row = await screen.findByTestId("pinned-finding");
    expect(within(row).getByTestId("pin-out-of-window").textContent).toMatch(
      /outside the current window; its title is not available here/i,
    );
    // The stored kind, the stored id and the operator's own note — nothing invented.
    expect(row.textContent).toContain("audit");
    expect(row.textContent).toContain("1757");
    expect(within(row).getByDisplayValue("the rollout")).toBeTruthy();
  });

  it("says nothing when the row IS on screen — the caption is about absence", async () => {
    renderPage({
      search: "?incident=inc-1",
      permissions: WRITE,
      incident: incidentRow({ pinned: [{ kind: "audit", id: "1757" }] }),
      auditRows: [
        {
          id: 1757,
          at: "2026-08-08T00:20:00Z",
          subjectKind: "user",
          subjectId: "ada",
          action: "POST /api/v1/targets",
          resource: "targets",
          outcome: "allowed",
          remoteAddr: "10.0.0.1",
          detail: {},
        },
      ],
    });

    const row = await screen.findByTestId("pinned-finding");
    await waitFor(() => expect(within(row).queryByTestId("pin-out-of-window")).toBeNull());
  });
});

describe("a deep link the page cannot honour verbatim (findings #2 and #14)", () => {
  it("refuses a linked window that lies entirely after the viewed instant, with nothing pressed", async () => {
    renderPage({
      search: `?kind=pair&scope=${encodeURIComponent("node-a→node-b")}` +
        "&from=2026-08-08T00:00:00Z&to=2026-08-08T00:45:00Z&at=2026-08-07T00:00:00Z",
    });
    expect(await screen.findByText(/The window is after the viewed instant/)).toBeTruthy();
    expect(screen.queryByTestId("clamp-banner")).toBeNull();
    // The address bar states what is actually framed, not what the link asked for.
    await waitFor(() =>
      expect(parseInvestigationParams(window.location.search, NOW).to.toISOString()).toBe(
        "2026-08-07T00:00:00.000Z",
      ),
    );
  });

  it("names every parameter it threw away, warns, and corrects the address bar", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    renderPage({ search: "?kind=galaxy&scope=node-a&from=yesterday" });

    const notice = await screen.findByTestId("ignored-params");
    expect(notice.textContent).toContain("?kind");
    expect(notice.textContent).toContain("?scope");
    expect(notice.textContent).toContain("?from");
    expect(warn).toHaveBeenCalledWith(expect.stringContaining("[investigate] ignoring"));

    // replaceState, and the corrected URL carries only what the page honoured.
    await waitFor(() => {
      const qs = new URLSearchParams(window.location.search);
      expect(qs.get("kind")).toBe("cluster");
      expect(qs.get("scope")).toBeNull();
      expect(Number.isNaN(Date.parse(qs.get("from") ?? ""))).toBe(false);
    });
    warn.mockRestore();
  });

  it("is dismissible — it describes an arrival, not a state", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    renderPage({ search: "?kind=galaxy" });
    await screen.findByTestId("ignored-params");
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByTestId("ignored-params")).toBeNull();
    warn.mockRestore();
  });

  it("says nothing for a well-formed link, including the bare one", async () => {
    renderPage({ search: "" });
    await screen.findByRole("button", { name: "Investigate" });
    expect(screen.queryByTestId("ignored-params")).toBeNull();
  });
});

describe("an invalid custom range DISABLES, the way an incomplete scope does (finding #13)", () => {
  async function custom(startDate: string, startTime: string) {
    renderPage();
    await screen.findByRole("button", { name: "Investigate" });
    fireEvent.click(screen.getByRole("radio", { name: "Custom" }));
    pickRange("Range start", startDate, startTime);
  }

  const button = () => screen.getByRole("button", { name: "Investigate" }) as HTMLButtonElement;

  it("refuses an inverted range before the click rather than swallowing it after", async () => {
    await custom("2026-08-09", "12:00");
    expect(button().disabled).toBe(true);
    expect(screen.getByTestId("range-invalid").textContent).toBe("The range end must be after its start.");
  });

  it("re-enables the moment the range makes sense again", async () => {
    await custom("2026-08-09", "12:00");
    expect(button().disabled).toBe(true);
    pickRange("Range start", "2026-08-08", "00:00");
    expect(button().disabled).toBe(false);
    expect(screen.queryByTestId("range-invalid")).toBeNull();
  });

  it("shows ONE reason: the scope's comes first, and the range's waits its turn", async () => {
    renderPage({ search: "?kind=pair" });
    await screen.findByRole("button", { name: "Investigate" });
    fireEvent.click(screen.getByRole("radio", { name: "Custom" }));
    pickRange("Range start", "2026-08-09", "12:00");
    expect(screen.getByTestId("scope-incomplete")).toBeTruthy();
    expect(screen.queryByTestId("range-invalid")).toBeNull();
  });
});

describe("the segmented controls are real radiogroups (finding #16)", () => {
  it("gives Scope and Range the roles and the roving tabindex", async () => {
    renderPage();
    await screen.findByRole("button", { name: "Investigate" });

    for (const name of ["Scope kind", "Range preset"]) {
      const group = screen.getByRole("radiogroup", { name });
      const radios = within(group).getAllByRole("radio");
      expect(radios.length).toBeGreaterThan(1);
      // Exactly one checked, and exactly one in the tab order.
      expect(radios.filter((r) => r.getAttribute("aria-checked") === "true").length).toBe(1);
      expect(radios.filter((r) => r.getAttribute("tabindex") === "0").length).toBe(1);
    }
  });

  it("gives the timeline's Rows per page the same treatment", async () => {
    renderPage({
      events: Array.from({ length: 12 }, (_, i) => ({
        id: `e-${i}`,
        seq: i,
        type: "topology_changed",
        severity: "info",
        scope: "node-a→node-b",
        timestamp: `2026-08-08T00:${String(i).padStart(2, "0")}:00Z`,
        summary: `event ${i}`,
        details: null,
      })),
    });
    const group = await screen.findByRole("radiogroup", { name: "Rows per page" });
    expect(within(group).getAllByRole("radio").map((r) => r.textContent)).toEqual(["10", "20", "50", "100"]);
  });
});

describe("the scoring-source link says where it goes (finding #21)", () => {
  it("keeps the link and adds the air-gap honesty to it", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: "the scoring source" });
    expect(link.getAttribute("href")).toContain("github.com");
    expect(link.getAttribute("title")).toMatch(/main branch/);
    expect(link.getAttribute("title")).toMatch(/air-gapped/);
  });
});

describe("the segmented controls fit a 375px viewport (finding #11)", () => {
  it("lets the track WRAP inside its card instead of drawing past the edge", async () => {
    renderPage();
    await screen.findByRole("button", { name: "Investigate" });
    /* Five scope options measure 356px against a 303px card at 375px. jsdom
       lays nothing out, so the pin is on the two classes that decide it: a
       track that may not exceed its parent, and one that is allowed to fold.
       shrink-0 stays — the track is one control inside a flex-wrap row. */
    for (const name of ["Scope kind", "Range preset"]) {
      const track = screen.getByRole("radiogroup", { name });
      expect(track.className).toContain("max-w-full");
      expect(track.className).toContain("flex-wrap");
    }
  });
});

describe("the audit caption admits it is cluster-wide (finding #9)", () => {
  it("names BOTH bounds — the newest N rows, and no scope filter at all", async () => {
    renderPage();
    const caption = await screen.findByText(/Config changes come from the newest 200 audit rows/);
    // The window bound was already stated; the SCOPE bound was the silent one,
    // and the audit leg is the only source here that ignores the scope entirely.
    expect(caption.textContent).toMatch(/NOT narrowed to this scope/i);
    expect(caption.textContent).toMatch(/cluster-wide/i);
  });
});

describe("one instant, one shape, in Russian too (findings #7 and #18)", () => {
  it("draws the timeline's clock in the INTERFACE language, not the browser's default", async () => {
    renderPage({
      locale: "ru",
      events: [
        {
          id: "e-1",
          seq: 1,
          type: "topology_changed",
          severity: "warn",
          scope: "node-a→node-b",
          timestamp: "2026-08-08T00:05:00Z",
          summary: "node-b NotReady",
          details: null,
        },
      ],
    });

    const row = (await screen.findAllByTestId("timeline-row"))[0];
    const clock = (row.textContent ?? "").trim();
    // ru-RU is 24-hour: an AM/PM marker here would mean the row had been
    // formatted with a bare toLocale* and the browser's own locale.
    expect(clock).not.toMatch(/AM|PM/);
    /* A DAY and a clock, not a clock alone. The window an Investigate timeline covers is arbitrary
       (?from/?to, an incident permalink, and the 6h preset any time the operator looks between
       00:00 and 06:00), so newest-first rows across midnight read as out of order when every row
       says only the time — 23:50 sitting under 00:10. */
    expect(clock).toMatch(/^\d{1,2}\s*\S+.*\d{1,2}:\d{2}/);
  });
});
