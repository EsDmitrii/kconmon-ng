import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { thresholdCrossings } from "@/lib/investigation";
import {
  alertEntries,
  alertTouchesScope,
  annotationEntries,
  auditDetailLine,
  auditEntries,
  buildExportPayload,
  commitWindow,
  eventEntries,
  exportFileName,
  incidentParams,
  investigationParamsToSearch,
  isReadOnlyAudit,
  k8sEntries,
  maintenanceEntries,
  parseInvestigationParams,
  pathChangeEntries,
  runEntries,
  samplesFromMatrix,
  scopeFromAlertLabels,
  shiftInstant,
} from "@/lib/investigation-sources";
import { InvestigatePage } from "./investigate";
import type { Alert, AuditEntry, K8sEvent, LiveEvent, MaintenanceWindow, PromResult } from "@/lib/types";

/**
 * investigate.hostile — QA scope 4, the hostile-user pass over Investigation
 * Mode: hand-edited permalinks, degenerate API rows, and the controls in the
 * states nobody designs for.
 *
 * The rule every case here shares, and the reason they sit in one file: NOTHING
 * a URL or a server row can say may end in a thrown render, a blank page, the
 * word "undefined" on screen, or a verdict the page has not earned. A malformed
 * input is allowed to produce a refusal, a dash or a caveat — it is not allowed
 * to produce silence, and it is not allowed to produce a lie.
 */

vi.mock("@/components/echart", () => ({
  EChart: ({ className }: { className?: string }) => <div data-testid="echart" className={className} />,
}));

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });
const problem = (status: number, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title: "refused", status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

const FROM = "2026-08-08T00:00:00Z";
const TO = "2026-08-08T01:00:00Z";
const AT = "2026-08-08T00:30:00Z";
const NOW = new Date("2026-08-08T01:00:00Z");

/** The two instants a Date can just barely hold, and the ones either side of
 *  them. `?from=` accepts both, and an hour past either used to be Invalid. */
const MAX_ISO = "+275760-09-13T00:00:00.000Z";
const MIN_ISO = "-271821-04-20T00:00:00.000Z";

const ALL_READS = [
  "events:read", "audit:read", "annotations:read", "mtr:read", "runs:read", "maintenance:read",
  "incidents:read", "incidents:write", "promql:query", "targets:read", "topology:read", "alerts:read",
];

interface Options {
  search?: string;
  permissions?: string[];
  events?: unknown[];
  maintenance?: unknown[];
  alerts?: unknown[];
  annotations?: unknown[];
  /** The matrix GET /promql/query_range answers for the LOSS query. Absent: an
   *  empty result, so the derived source contributes no rows. */
  lossMatrix?: unknown;
  incident?: Record<string, unknown> | null;
  failing?: { prefix: string; status?: number; detail: string }[];
  topologyNodes?: { name: string; zone: string; ready: boolean }[];
  locale?: Locale;
}

function renderPage(opts: Options = {}) {
  const {
    search = `?kind=cluster&from=${FROM}&to=${TO}`,
    permissions = ALL_READS,
    events = [], maintenance = [], alerts = [], annotations = [],
    lossMatrix, incident = null, failing = [],
    topologyNodes = [{ name: "node-a", zone: "z1", ready: true }, { name: "node-b", zone: "z2", ready: true }],
    locale,
  } = opts;

  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  window.history.replaceState({}, "", `/investigate${search}`);
  let stored = incident;

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const broken = failing.find((f) => href.startsWith(f.prefix));
    if (broken !== undefined) return Promise.resolve(problem(broken.status ?? 500, broken.detail));

    if (href.startsWith("/api/v1/auth/me")) {
      return Promise.resolve(json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions }));
    }
    if (href.startsWith("/api/v1/config")) {
      return Promise.resolve(json({
        auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
        anonymousBanner: false, controller: { configured: true },
        prometheus: { configured: true }, database: { configured: true },
      }));
    }
    if (href.startsWith("/api/v1/topology")) return Promise.resolve(json({ nodes: topologyNodes, agents: [], timestamp: FROM }));
    if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/k8s-events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    if (href.startsWith("/api/v1/audit")) return Promise.resolve(json({ entries: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/annotations")) return Promise.resolve(json({ annotations, nextCursor: "" }));
    if (href.startsWith("/api/v1/maintenance")) return Promise.resolve(json({ windows: maintenance, nextCursor: "" }));
    if (href.startsWith("/api/v1/alerts")) return Promise.resolve(json({ alerts, promConfigured: true }));
    if (href.startsWith("/api/v1/mtr/destinations")) return Promise.resolve(json({ destinations: [] }));
    if (href.startsWith("/api/v1/mtr/snapshots")) return Promise.resolve(json({ snapshots: [], nextCursor: "" }));
    if (/^\/api\/v1\/incidents\/?[^/?]*$/.test(href) && !href.endsWith("/incidents")) {
      if (method === "PATCH") {
        stored = { ...(stored ?? {}), ...(init?.body ? (JSON.parse(String(init.body)) as object) : {}) };
        return Promise.resolve(json(stored));
      }
      if (stored === null) return Promise.resolve(problem(404, "no incident with that id"));
      return Promise.resolve(json(stored));
    }
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/promql/query_range")) {
      const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {};
      if (String(body.query ?? "").includes("packet_loss_ratio") && lossMatrix !== undefined) {
        return Promise.resolve(json(lossMatrix));
      }
      return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: [] } }));
    }
    if (href.startsWith("/api/v1/promql/query")) {
      return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = <InvestigatePage />;
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>{locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}</TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock };
}

/** settled waits for the pane to stop loading — the count is suppressed until
 *  then, which is finding #4's whole point. */
const settled = () => waitFor(() => expect(screen.getByTestId("timeline-count")).toBeTruthy());

/** The two words this page may never print at a reader. */
const bodyText = () => document.body.textContent ?? "";
const expectNoGarbage = () => {
  expect(bodyText()).not.toContain("undefined");
  expect(bodyText()).not.toContain("NaN");
  expect(bodyText()).not.toContain("Invalid Date");
};

afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/* ── finding #1: a permalink naming the end of the calendar ─────────────── */

describe("hostile ?from / ?to (finding #1)", () => {
  /* The failure this replaces: parseInvestigationParams derived the missing edge
     by plain arithmetic, `MAX + 1h` overflowed to an Invalid Date, and the page
     then called toISOString() on it to build its own react-query key — a thrown
     RangeError before the first paint, i.e. a blank page from one hand-edited
     parameter. */
  it("clamps the derived edge instead of overflowing it", () => {
    const top = parseInvestigationParams(`?from=${encodeURIComponent(MAX_ISO)}`, NOW);
    expect(Number.isNaN(top.to.getTime())).toBe(false);
    const bottom = parseInvestigationParams(`?to=${encodeURIComponent(MIN_ISO)}`, NOW);
    expect(Number.isNaN(bottom.from.getTime())).toBe(false);
  });

  it("keeps the permalink and the export payload writable at either limit", () => {
    for (const search of [`?from=${encodeURIComponent(MAX_ISO)}`, `?to=${encodeURIComponent(MIN_ISO)}`]) {
      const p = parseInvestigationParams(search, NOW);
      expect(() => investigationParamsToSearch(p)).not.toThrow();
      expect(() => buildExportPayload(p, [], [])).not.toThrow();
      expect(() => exportFileName(p.from)).not.toThrow();
    }
  });

  it("shiftInstant saturates rather than wrapping or going Invalid", () => {
    const top = shiftInstant(new Date(MAX_ISO), 60 * 60 * 1000);
    expect(top.getTime()).toBe(new Date(MAX_ISO).getTime());
    const bottom = shiftInstant(new Date(MIN_ISO), -60 * 60 * 1000);
    expect(bottom.getTime()).toBe(new Date(MIN_ISO).getTime());
    expect(Number.isNaN(shiftInstant(new Date(NaN), 1000).getTime())).toBe(false);
  });

  it("renders the page for a link at either limit, and says the window was refused", async () => {
    renderPage({ search: `?kind=cluster&from=${encodeURIComponent(MAX_ISO)}` });
    await settled();
    /* Clamping collapses the window onto one instant, which commitWindow refuses
       — so the page frames its default hour and states the refusal, which is the
       honest answer to a link naming the end of time. */
    expect(screen.getAllByRole("alert").map((n) => n.textContent).join(" ")).toMatch(/after its start/i);
    expectNoGarbage();
  });

  it("renders the page for a link at the bottom of the calendar", async () => {
    renderPage({ search: `?kind=cluster&to=${encodeURIComponent(MIN_ISO)}` });
    await settled();
    expectNoGarbage();
  });

  it("survives epoch zero, the year 3000 and an inverted pair", async () => {
    for (const search of [
      "?kind=cluster&from=1970-01-01T00:00:00Z&to=1970-01-01T01:00:00Z",
      "?kind=cluster&from=3000-01-01T00:00:00Z&to=3000-01-01T06:00:00Z",
      `?kind=cluster&from=${TO}&to=${FROM}`,
      `?kind=cluster&from=${FROM}&to=${FROM}`,
      "?kind=cluster&from=nonsense&to=alsononsense",
    ]) {
      renderPage({ search });
      await settled();
      expectNoGarbage();
      cleanup();
    }
  });

  it("commitWindow refuses an unreadable edge and ignores an unreadable instant", () => {
    expect(commitWindow(new Date(NaN), new Date(TO), null).ok).toBe(false);
    expect(commitWindow(new Date(FROM), new Date(NaN), null).ok).toBe(false);
    /* A NaN instant compares false against everything, so treating it as a gate
       would answer "not clamped" to every window on the page. */
    const c = commitWindow(new Date(FROM), new Date(TO), new Date(NaN));
    expect(c.ok && c.clamped).toBe(false);
  });
});

/* ── finding #2: a malformed PromQL matrix row ──────────────────────────── */

describe("degenerate PromQL matrices (finding #2)", () => {
  const matrix = (values: unknown[]): PromResult =>
    ({ status: "success", data: { resultType: "matrix", result: [{ metric: {}, values }] } }) as unknown as PromResult;

  it("drops a sample whose timestamp is not a number", () => {
    expect(samplesFromMatrix(matrix([["oops", "0.5"]]), undefined)).toEqual([]);
    expect(samplesFromMatrix(matrix([[Number.POSITIVE_INFINITY, "0.5"]]), undefined)).toEqual([]);
    expect(samplesFromMatrix(matrix([[1e18, "0.5"]]), undefined)).toEqual([]);
  });

  it("drops a NULL timestamp rather than dating the sample 1970", () => {
    /* Worse than a throw, because it was silent: Number(null) is 0, so the row
       became a sample at the epoch — outside every window an operator frames,
       yet still the earliest threshold crossing, i.e. the investigation's
       anomaly ONSET and the anchor the whole cause ranking hangs off. */
    expect(samplesFromMatrix(matrix([[null, "0.5"]]), undefined)).toEqual([]);
  });

  it("skips a row that is not a [timestamp, value] pair at all", () => {
    expect(() => samplesFromMatrix(matrix([undefined, 42, {}, []]), undefined)).not.toThrow();
    expect(samplesFromMatrix(matrix([undefined, 42, {}, []]), undefined)).toEqual([]);
  });

  it("keeps the well-formed rows beside the malformed ones", () => {
    const ts = Date.parse(AT) / 1000;
    const s = samplesFromMatrix(matrix([["oops", "0.9"], [ts, "0.5"], [null, "0.9"]]), undefined);
    expect(s).toHaveLength(1);
    expect(s[0].at.toISOString()).toBe(new Date(AT).toISOString());
    expect(s[0].loss).toBe(0.5);
  });

  it("thresholdCrossings never dereferences an unreadable instant", () => {
    /* The ref id is `${signal}:${direction}:${at.toISOString()}`, and that THROWS
       on an Invalid Date. thresholdCrossings is public, so it holds the line
       itself rather than trusting its one caller. */
    expect(() => thresholdCrossings([{ at: new Date(NaN), loss: 0.9 }])).not.toThrow();
    expect(thresholdCrossings([{ at: new Date(NaN), loss: 0.9 }])).toEqual([]);
  });

  it("renders the page over a malformed loss matrix", async () => {
    renderPage({
      lossMatrix: { status: "success", data: { resultType: "matrix", result: [{ metric: {}, values: [["oops", "0.5"], [null, "0.9"]] }] } },
    });
    await settled();
    expectNoGarbage();
  });
});

/* ── finding #3: rows the server sent incomplete ────────────────────────── */

describe("rows with fields the server did not send (finding #3)", () => {
  const F = new Date(FROM);
  const T = new Date(TO);

  it("a K8s event with no reason and no message", () => {
    const rows = k8sEntries([{ id: "k1", eventTime: AT, kind: "Pod", name: "api-7" }] as unknown as K8sEvent[]);
    expect(rows[0].title).toBe("Pod api-7");
    expect(`${rows[0].title}${rows[0].detail ?? ""}`).not.toContain("undefined");
  });

  it("a live event with no scope, type or summary", () => {
    const rows = eventEntries([{ id: 7, timestamp: AT, severity: "info" }] as unknown as LiveEvent[]);
    expect(`${rows[0].title}${rows[0].detail ?? ""}`).not.toContain("undefined");
    /* The ref id is the dedupe identity AND the pin permalink, so a numeric id
       off the wire has to survive as its own string. */
    expect(rows[0].ref?.id).toBe("7");
  });

  it("an annotation with no author", () => {
    const rows = annotationEntries([{ id: "a1", startAt: AT, text: "x", scope: "" }] as never);
    expect(rows[0].detail).not.toContain("undefined");
  });

  it("a run with no counters", () => {
    const rows = runEntries([{ id: "r1", createdAt: AT, status: "ok", type: "mtr" }] as never);
    expect(`${rows[0].title}${rows[0].detail ?? ""}`).not.toContain("undefined");
  });

  it("a path snapshot with no pathHash — which used to throw outright", () => {
    const rows = pathChangeEntries([{ id: "s1", firstSeen: AT, sourceNode: "a", destination: "b" }] as never, F, T);
    expect(rows).toHaveLength(1);
    expect(rows[0].detail).not.toContain("undefined");
  });

  it("an audit row with no action — which used to empty the whole timeline", () => {
    /* isReadOnlyAudit runs over EVERY scanned row, so one such row threw before
       any of the others could be mapped. */
    expect(() => isReadOnlyAudit(undefined as unknown as string)).not.toThrow();
    const rows = auditEntries([{ id: 1, at: AT, outcome: "allowed" }] as unknown as AuditEntry[], F, T);
    expect(rows).toHaveLength(1);
    expect(`${rows[0].title}${rows[0].detail ?? ""}`).not.toContain("undefined");
    expect(auditDetailLine({ id: 1, at: AT } as unknown as AuditEntry)).not.toContain("undefined");
  });

  it("a maintenance window with no end says so instead of printing Invalid Date", () => {
    const noEnd = [{ id: "m1", scope: "", startAt: AT, reason: "switch", createdBy: "u" }] as unknown as MaintenanceWindow[];
    expect(maintenanceEntries(noEnd)[0].detail).toBe("global · end not stated · u");
    const junkEnd = [{ id: "m1", scope: "", startAt: AT, endAt: "nope", reason: "switch", createdBy: "u" }] as unknown as MaintenanceWindow[];
    expect(maintenanceEntries(junkEnd)[0].detail).not.toContain("Invalid Date");
  });

  it("an alert whose label map arrived as null", () => {
    /* Go marshals a nil map as JSON null, and this transport does not normalise
       it — Object.keys(null) threw and took the page down. */
    const a = [{ name: "KconmonLoss", state: "firing", severity: "warning", activeAt: AT, ruleId: "r1", labels: null }] as unknown as Alert[];
    expect(() => alertEntries(a, F, T)).not.toThrow();
    expect(alertEntries(a, F, T)).toHaveLength(1);
    expect(alertTouchesScope(null, { kind: "node", a: "node-a", b: "" })).toBe(true);
    expect(scopeFromAlertLabels(null)).toBeNull();
  });

  it("renders the page over every one of those rows at once", async () => {
    renderPage({
      events: [{ id: 1, timestamp: AT, severity: "info" }],
      maintenance: [{ id: "m1", scope: "", startAt: AT, reason: "switch", createdBy: "u" }],
      alerts: [{ name: "KconmonLoss", state: "firing", severity: "warning", activeAt: AT, ruleId: "r1", labels: null }],
      annotations: [{ id: "a1", startAt: AT, text: "note", scope: "" }],
    });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBeGreaterThan(2));
    expectNoGarbage();
  });
});

/* ── finding #4: a verdict rendered before anything was asked ───────────── */

describe("the timeline before the page is ready to ask (finding #4)", () => {
  it("shows the skeleton rather than counting zero and claiming a quiet fleet", () => {
    /* Every source is gated on auth AND the database capability, so before those
       land react-query holds eleven DISABLED queries — none of them "loading".
       The pane read that as a settled empty result and printed «0 entries in
       this window» over "Nothing happened in this window", which is a verdict on
       a page that had not asked a single question. */
    renderPage();
    expect(screen.queryByTestId("timeline-count")).toBeNull();
    expect(screen.queryByText(/Nothing happened in this window/i)).toBeNull();
    expect(screen.getByRole("status", { name: "" })).toBeTruthy();
  });

  /* `allFailed`'s mirror: nothing failed, nothing was pending, and nothing was
     asked either — a subject holding none of the eight read permissions. The
     source notes name every missing permission; the verdict must not contradict
     them. */
  it("does not claim a quiet fleet for a subject who may read nothing", async () => {
    renderPage({ permissions: ["topology:read"] });
    await settled();
    expect(screen.queryByText(/Nothing happened in this window/i)).toBeNull();
    expect(screen.queryByTestId("timeline-all-failed")).toBeNull();
    expect(screen.getByText(/Fleet events and Kubernetes events need events:read/i)).toBeTruthy();
    expectNoGarbage();
  });

  it("counts and claims once the sources have actually settled", async () => {
    renderPage();
    await settled();
    expect(screen.getByTestId("timeline-count").textContent).toBe("0 entries in this window");
    expect(screen.getByText(/Nothing happened in this window/i)).toBeTruthy();
  });
});

/* ── finding #5: ?incident= with nothing after the equals sign ──────────── */

describe("an empty ?incident= (finding #5)", () => {
  it("is not an id: no request, no ghost not-found card", async () => {
    const { fetchMock } = renderPage({ search: "?incident=" });
    await settled();
    await new Promise((r) => setTimeout(r, 20));
    /* It used to fire GET /api/v1/incidents/ , read the 404 as "somebody deleted
       it", and render a card whose sentence named nothing at all. */
    expect(fetchMock.mock.calls.map((c) => String(c[0])).filter((u) => u.includes("/incidents"))).toEqual([]);
    expect(screen.queryByTestId("incident-not-found")).toBeNull();
    expectNoGarbage();
  });

  it("stops shadowing the URL correction the rest of the link needed", async () => {
    /* The parameter's mere presence made correctURL bow out, so a `?kind=` this
       page cannot honour went unreported beside it. */
    renderPage({ search: "?incident=&kind=galaxy&scope=node-a" });
    await settled();
    expect(screen.getByTestId("ignored-params").textContent).toMatch(/\?kind/);
  });

  it("still honours a real id", async () => {
    renderPage({
      search: "?incident=inc-1",
      incident: { id: "inc-1", title: "Loss on node-a", scope: "node-a", fromAt: FROM, toAt: TO, status: "open", notes: "", pinned: [], createdBy: "user:ada", createdAt: FROM },
    });
    expect(await screen.findByRole("heading", { name: "Loss on node-a", level: 2 })).toBeTruthy();
  });
});

/* ── finding #6: a picker that showed nothing and committed something ───── */

describe("a scope naming an object the fleet no longer has (finding #6)", () => {
  it("draws the committed name as a marked option rather than showing blank", async () => {
    renderPage({ search: `?kind=node&scope=ghost-node&from=${FROM}&to=${TO}` });
    const select = await waitFor(() => {
      const s = screen.getByLabelText("Node") as HTMLSelectElement;
      expect(s.options.length).toBeGreaterThan(2);
      return s;
    });
    /* The failure this replaces: a select whose value matches no option renders
       blank, so the picker said "—" while the headline beside the page title
       said "ghost-node" and Investigate committed "ghost-node". */
    expect(select.value).toBe("ghost-node");
    expect([...select.options].map((o) => o.textContent)).toContain("ghost-node — not in the current topology");
  });

  it("leaves a name the fleet DOES have unmarked", async () => {
    renderPage({ search: `?kind=node&scope=node-a&from=${FROM}&to=${TO}` });
    const select = await waitFor(() => {
      const s = screen.getByLabelText("Node") as HTMLSelectElement;
      expect(s.options.length).toBeGreaterThan(1);
      return s;
    });
    expect(select.value).toBe("node-a");
    expect(bodyText()).not.toContain("not in the current topology");
  });

  it("marks a target the gated list could not name either", async () => {
    /* Without targets:read the option list is empty outright, which is the same
       lying-blank control by a different route. */
    renderPage({
      search: `?kind=target&scope=api-gw&from=${FROM}&to=${TO}`,
      permissions: ALL_READS.filter((p) => p !== "targets:read"),
    });
    await settled();
    expect((screen.getByLabelText("Target") as HTMLSelectElement).value).toBe("api-gw");
  });
});

/* ── the rest of the hostile pass ───────────────────────────────────────── */

describe("hostile ?scope= and ?kind=", () => {
  it("normalises every arrow a keyboard can type into the one the API filters by", () => {
    for (const typed of ["node-a->node-b", "node-a -> node-b", "node-a→node-b", "node-a  ->  node-b"]) {
      const p = parseInvestigationParams(`?kind=pair&scope=${encodeURIComponent(typed)}`, NOW);
      expect([p.a, p.b]).toEqual(["node-a", "node-b"]);
    }
  });

  it("degrades an unparseable kind to the cluster and says which parameters it dropped", async () => {
    renderPage({ search: "?kind=galaxy&scope=node-a&from=nonsense" });
    await settled();
    const notice = screen.getByTestId("ignored-params").textContent ?? "";
    for (const key of ["?kind", "?scope", "?from"]) expect(notice).toContain(key);
    expectNoGarbage();
  });

  it("carries a thousand-character scope without breaking anything", async () => {
    renderPage({ search: `?kind=node&scope=${"n".repeat(1000)}&from=${FROM}&to=${TO}` });
    await settled();
    expectNoGarbage();
  });

  it("says which end of a half-written pair is missing rather than committing it silently", async () => {
    renderPage({ search: `?kind=pair&scope=node-a&from=${FROM}&to=${TO}` });
    await settled();
    expect(screen.getByTestId("scope-incomplete").textContent).toMatch(/destination node/i);
    expect((screen.getByRole("button", { name: "Investigate" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("the timeline under volume and abuse", () => {
  const eventRows = (n: number) =>
    Array.from({ length: n }, (_, i) => ({
      id: `e${i}`, timestamp: new Date(Date.parse(FROM) + i).toISOString(),
      severity: "info", type: "t", scope: "", summary: `row ${i}`,
    }));

  it("keeps ten thousand entries off the DOM while counting all of them", async () => {
    renderPage({ events: eventRows(10000) });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBe(10));
    expect(screen.getByTestId("timeline-count").textContent).toBe("10000 entries in this window");
  });

  it("clamps a page walked past the last one", async () => {
    renderPage({ events: eventRows(45) });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBe(10));
    const next = screen.getByRole("button", { name: "Next page" });
    for (let i = 0; i < 12; i++) fireEvent.click(next);
    expect(screen.getByTestId("pager-page").textContent).toBe("Page 5 of 5");
    expect(screen.getAllByTestId("timeline-row").length).toBe(5);
    expect((next as HTMLButtonElement).disabled).toBe(true);
  });

  it("anchors the last page's first row when the size changes under it", async () => {
    renderPage({ events: eventRows(45) });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBe(10));
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    fireEvent.click(screen.getByRole("radio", { name: "50" }));
    expect(screen.getByTestId("pager-page").textContent).toBe("Page 1 of 1");
    expect(screen.getAllByTestId("timeline-row").length).toBe(45);
  });

  it("survives a hover storm without losing the list", async () => {
    renderPage({ events: eventRows(30) });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBe(10));
    const rows = screen.getAllByTestId("timeline-row");
    for (let i = 0; i < 150; i++) {
      const row = rows[i % rows.length];
      fireEvent.mouseEnter(row);
      fireEvent.focus(row);
      fireEvent.blur(row);
      fireEvent.mouseLeave(row);
    }
    expect(screen.getAllByTestId("timeline-row").length).toBe(10);
    expectNoGarbage();
  });

  it("keeps a same-instant batch as separate rows and drops the undated ones", async () => {
    renderPage({
      events: [
        { id: "e1", timestamp: AT, severity: "warn", type: "t", scope: "", summary: "one" },
        { id: "e2", timestamp: AT, severity: "warn", type: "t", scope: "", summary: "two" },
        { id: "e3", timestamp: "garbage", severity: "warn", type: "t", scope: "", summary: "three" },
      ],
    });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBe(2));
    expectNoGarbage();
  });
});

describe("the incident strip under hostile content", () => {
  const HOSTILE = `🔥<script>alert(1)</script>${"я".repeat(600)}`;

  const openIncident = () =>
    renderPage({
      search: "?incident=inc-1",
      incident: { id: "inc-1", title: HOSTILE, scope: "", fromAt: FROM, toAt: TO, status: "open", notes: "", pinned: [], createdBy: "user:ada", createdAt: FROM },
    });

  it("renders a script-shaped title as text and keeps every control addressable", async () => {
    openIncident();
    await screen.findByRole("heading", { level: 2 });
    expect(document.querySelector("script")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: `Delete incident: ${HOSTILE}` }));
    expect(screen.getByRole("button", { name: `Confirm delete incident: ${HOSTILE}` })).toBeTruthy();
  });

  it("hydrates a stored window whose stamps are junk without going Invalid", () => {
    const p = incidentParams(
      { id: "i", title: "t", scope: "", fromAt: "??", toAt: "??", status: "open", notes: "", pinned: [], createdBy: "u", createdAt: "" } as never,
      [],
      NOW,
    );
    expect(Number.isNaN(p.from.getTime())).toBe(false);
    expect(Number.isNaN(p.to.getTime())).toBe(false);
  });

  it("hydrates a stored window pinned to the bottom of the calendar", () => {
    const p = incidentParams(
      { id: "i", title: "t", scope: "", toAt: MIN_ISO, status: "open", notes: "", pinned: [], createdBy: "u", createdAt: "" } as never,
      [],
      NOW,
    );
    expect(Number.isNaN(p.from.getTime())).toBe(false);
  });

  it("names a hostile title in the export file name without illegal characters", () => {
    const name = exportFileName(new Date(FROM));
    expect(name).not.toContain(":");
    expect(name).toMatch(/^investigation-.*\.json$/);
  });
});

describe("degenerate API states", () => {
  it("states a 401 arriving mid-flow at the source that took it", async () => {
    renderPage({ failing: [{ prefix: "/api/v1/events", status: 401, detail: "your session expired" }] });
    await waitFor(() => expect(screen.getByTestId("timeline-partial")).toBeTruthy());
    expect(bodyText()).toContain("your session expired");
    expect(bodyText()).not.toContain("undefined");
  });

  it("calls a total blackout an error state rather than a quiet window", async () => {
    renderPage({
      failing: ["/api/v1/events", "/api/v1/k8s-events", "/api/v1/audit", "/api/v1/annotations", "/api/v1/mtr",
        "/api/v1/runs", "/api/v1/maintenance", "/api/v1/promql", "/api/v1/alerts"]
        .map((prefix) => ({ prefix, status: 500, detail: "upstream is down" })),
    });
    await screen.findByTestId("timeline-all-failed");
    expect(screen.queryByText(/Nothing happened in this window/i)).toBeNull();
    expectNoGarbage();
  });
});

describe("i18n", () => {
  it("says the missing-option mark and the no-end window in Russian too", async () => {
    renderPage({
      locale: "ru",
      search: `?kind=node&scope=ghost-node&from=${FROM}&to=${TO}`,
      maintenance: [{ id: "m1", scope: "", startAt: AT, reason: "работы", createdBy: "u" }],
    });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBeGreaterThan(0));
    expect(bodyText()).toContain("нет в текущей топологии");
    expect(bodyText()).toContain("конец не указан");
    expectNoGarbage();
  });

  it("counts a Russian window at zero with the right plural form", async () => {
    renderPage({ locale: "ru" });
    await settled();
    expect(screen.getByTestId("timeline-count").textContent).toBe("0 записей в этом интервале");
  });

  it("renders a Russian page over the whole degenerate row set", async () => {
    renderPage({
      locale: "ru",
      events: [{ id: 1, timestamp: AT, severity: "info" }],
      alerts: [{ name: "KconmonLoss", state: "firing", severity: "warning", activeAt: AT, ruleId: "r1", labels: null }],
      lossMatrix: { status: "success", data: { resultType: "matrix", result: [{ metric: {}, values: [["oops", "0.5"]] }] } },
    });
    await waitFor(() => expect(screen.getAllByTestId("timeline-row").length).toBeGreaterThan(0));
    expectNoGarbage();
  });
});
