import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
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
  alertEntries,
  auditDetailLine,
  auditEntries,
  buildExportPayload,
  buildInvestigateURL,
  commitWindow,
  incidentParams,
  investigationFailRatioQuery,
  investigationLossQuery,
  investigationRttQuery,
  parseInvestigationParams,
  investigationParamsToSearch,
  pinnedRefFor,
  runTouchesScope,
  samplesFromMatrix,
  scopeCaptionValue,
  scopeFilterValue,
  scopeFromAlertLabels,
  scopeFromIncidentScope,
  scopeIncompleteReason,
  scopeNodeOptions,
  scopeZoneOptions,
  scopesToQuery,
  type InvestigationScope,
} from "@/lib/investigation-sources";
import {
  CURSOR_SERIES_NAME,
  cursorSeries,
  deltaFromVectors,
  signalChartOption,
  withOverlays,
} from "@/components/investigation-signals";
import { formatSeconds } from "@/lib/curated-metrics";
import { MAINTENANCE_SERIES_NAME, maintenanceOverlaySeries } from "@/lib/annotations";

// Same reason as every other page test in this repo: echarts.init() reaches for
// a 2d canvas context jsdom does not implement. The signal panels' OPTION is
// asserted through the pure builders (cursorSeries + the lifted
// maintenanceOverlaySeries) rather than through a mounted chart.
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
  /** Route prefixes that answer problem+json instead of a body (QA round 3,
   *  finding #1): each one becomes a FAILED source, which is a different thing
   *  from an absent one and the page now says which. */
  failing?: { prefix: string; status?: number; detail: string }[];
  /** The topology body, so a test can hand the page a controller-less console
   *  whose only evidence of a fleet is its AGENTS (QA round 3, finding #5). */
  topology?: unknown;
  /** The single sample every INSTANT promql query answers — the fail-ratio the
   *  delta chip renders (QA round 3, finding #4). */
  failRatio?: string;
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
  } = opts;

  window.history.replaceState({}, "", `/investigate${search}`);

  /* The incident row is STATEFUL across the calls in one render, because that
     is the only way to exercise what this task actually built: create, then
     read the permalink back, then patch it three times. A fixed body would let
     an assertion pass against a page that never sent the write. */
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
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          <InvestigatePage />
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );

  const urlsFor = (prefix: string) => calls.filter((c) => c.url.startsWith(prefix)).map((c) => c.url);
  const postCalls = () => calls.filter((c) => c.method === "POST" && !c.url.startsWith("/api/v1/promql"));
  const patchCalls = () => calls.filter((c) => c.method === "PATCH");
  return { ...utils, calls, urlsFor, postCalls, patchCalls, qc };
}

/**
 * pickRange drives one DateTimePicker by its aria-label: open the trigger, type
 * the local day and wall clock into the picker's manual fields, apply. The same
 * helper components/annotations.test.tsx and components/maintenance.test.tsx
 * use — the console asks for an instant exactly one way now (QA round 3, #12),
 * so the tests drive it one way too.
 */
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

/* No fake clock: every assertion below either names an absolute instant that
   came out of a fixture, or checks a SPAN (which a preset computes from one
   Date.now() call and is therefore exact whatever the wall clock says). A
   frozen clock would only buy the ability to assert an absolute `from` on a
   preset — and it would fight @testing-library's timer-aware waitFor for it. */
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/");
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
  it("cursorSeries draws exactly one markLine at the hovered instant", () => {
    const s = cursorSeries(new Date("2026-08-08T00:20:00Z"), true);
    expect(s?.markLine?.data).toEqual([{ xAxis: Date.parse("2026-08-08T00:20:00Z") }]);
  });

  it("cursorSeries is null when nothing is hovered, so the option keeps its identity", () => {
    expect(cursorSeries(null, true)).toBeNull();
  });

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
      { cursorAt: null, windows: [maintenanceRow()], dark: false },
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
  const overlays = { cursorAt: null, windows: [] };
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

  it("leaves a RATIO axis to the shared builder's own percent formatting", () => {
    const option = signalChartOption(chart("ratio"), matrixBody(LOSS_STEPS), false, overlays);
    expect(axisLabel(option, "yAxis").formatter).toBeUndefined();
  });

  it("still composes the overlays rather than replacing them", () => {
    const option = signalChartOption(chart("ratio"), matrixBody(LOSS_STEPS), false, {
      cursorAt: new Date("2026-08-08T00:20:00Z"),
      windows: [maintenanceRow()],
    });
    const names = (option.series as { name?: string }[]).map((s) => s.name);
    expect(names).toContain(MAINTENANCE_SERIES_NAME);
    expect(names).toContain(CURSOR_SERIES_NAME);
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

    // "Audit rows", not "config changes" (QA round 5, finding #19): most of
    // what the audit log records is a READ decision, and calling those rows
    // config changes sent an operator hunting a change nobody made.
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

    const rows = await screen.findAllByTestId("timeline-row");
    const titles = rows.map((r) => r.textContent ?? "");
    expect(titles[0]).toContain("node-b NotReady");
    expect(titles[1]).toContain("NodeNotReady");
    expect(titles[2]).toContain("Route changed");
    expect(titles[3]).toContain("poked the gateway");
    // …and the threshold crossing derived from the loss series lands at 00:20.
    expect(titles[4]).toContain("Packet loss crossed the threshold");

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
    expect(screen.getByTestId("signal-cursor").textContent).toMatch(/no row hovered/i);

    fireEvent.mouseEnter(rows[0]);
    await waitFor(() =>
      expect(screen.getByTestId("signal-cursor").textContent).toContain(new Date("2026-08-08T00:15:00Z").toLocaleTimeString()),
    );

    fireEvent.mouseLeave(rows[0]);
    await waitFor(() => expect(screen.getByTestId("signal-cursor").textContent).toMatch(/no row hovered/i));
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

  /* Task 7 shipped BOTH writes as disabled seams; Task 8 replaced the incident
     one. This is the SECOND replacement: "Create maintenance" is a real control
     now, gated on maintenance:write exactly the way "Save as incident" is gated
     on incidents:write — so the seam's assertions become its opposite. */
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
    // M7 Task 8: an alert lives in Prometheus, not in a table the pin
    // vocabulary has a member for.
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

/* ══════════════════════════════════════════════════════════════════════════
   QA round 3 — the page-level pins. One describe per finding, named by it, so
   a regression report says which decision came back.
   ══════════════════════════════════════════════════════════════════════════ */

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

  it("still claims nothing happened when every enabled source settled cleanly", async () => {
    renderPage();
    expect(await screen.findByText(/Nothing happened in this window/i)).toBeTruthy();
    expect(screen.queryByTestId("timeline-partial")).toBeNull();
  });
});

describe("#4 the delta chip survives a fleet running one protocol", () => {
  it("renders a real ratio rather than an em dash when the guarded query returns 1", async () => {
    /* The end-to-end half of finding #4. The query-shape test above pins the
       `or vector(0)` guards; this pins what they BUY: on an ICMP-only fleet the
       two missing protocols now contribute 0 instead of erasing the whole
       expression, so a scope failing every probe reads 100.0% instead of "—"
       — which was indistinguishable from "nothing measured". */
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

describe("#3 the Time Machine clamps the investigated window", () => {
  const ENGAGED = `${PAIR_SEARCH}&at=2026-08-08T00:30:00Z`;

  /* The clock is frozen for this block only: a preset commit ("1h") is
     measured from NOW, and against a real wall clock the window would land
     months after the instant under test — a different branch of commitWindow
     entirely. */
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

  it("commits `to` as the viewed instant and says the window moved", async () => {
    renderPage({ search: `${CUSTOM}&at=2026-08-08T00:30:00Z` });
    await screen.findByRole("button", { name: "Investigate" });
    fireEvent.click(screen.getByRole("button", { name: "Investigate" }));

    expect(await screen.findByTestId("clamp-banner")).toHaveTextContent("Window clamped to the viewed instant.");
    // The URL is the contract: `to` is now the instant, `from` is untouched.
    const committed = parseInvestigationParams(window.location.search, NOW);
    expect(committed.to.toISOString()).toBe("2026-08-08T00:30:00.000Z");
    expect(committed.from.toISOString()).toBe("2026-08-08T00:00:00.000Z");
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
