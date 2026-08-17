import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TimeMachineProvider } from "@/lib/timemachine";
import {
  AlertingPage,
  alertRuleRequestFrom,
  formatPromDuration,
  KIND_PARAMS,
  parsePromDuration,
  problemField,
  relativeTime,
  reservedLabelMessage,
} from "./alerting";

/**
 * The Alerting page is one read floor (alerts:read) over one write permission (alerts:manage) over
 * TWO independent dependencies that fail in different ways.
 */

const AT = "2026-08-01T12:00:00Z";
const NOW = new Date("2026-08-08T12:00:00Z");

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** The two permissions this page gates on. */
const VIEWER = ["topology:read", "matrix:read", "alerts:read"];
const ALERT_EDITOR = ["topology:read", "alerts:read", "alerts:manage"];
const NO_ALERTS = ["topology:read", "matrix:read", "events:read"];

/** The 409 the sync family answers with alerting switched off, and the 503 the
 *  store family answers with no database — both VERBATIM from
 *  internal/console/httpapi/alertrules.go, because the page renders the
 *  server's sentence rather than a paraphrase of it. */
const ALERTING_DISABLED_DETAIL =
  "prometheus rule sync is not running on this console: the alert rules themselves are unaffected and stay " +
  "readable and editable, but nothing is applying them to the cluster -- set console.alerting.enabled=true " +
  "(Helm: console.alerting.enabled) on a console running in-cluster with the PrometheusRule CRD present";

const NO_DATABASE_DETAIL =
  "alert rules are persisted configuration with no in-memory fallback: set console.database.mode in the " +
  "console config (Helm: console.database.mode) to enable /api/v1/alert-rules";

function meBody(permissions: string[], roles: string[] = ["admin"]) {
  return { subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles }, permissions };
}

const configBody = {
  auth: { mode: "local", role: "viewer", loginPath: "/api/v1/auth/login" },
  anonymousBanner: false,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: true },
};

function ruleRow(over: Record<string, unknown> = {}) {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    name: "PairLossHigh",
    kind: "pair-loss",
    params: { protocol: "udp", thresholdPercent: 5 },
    severity: "warning",
    forNs: 300_000_000_000,
    labels: { team: "net" },
    annotations: { summary: "loss is up" },
    enabled: true,
    renderedExpr: "kconmon_ng_udp_packet_loss_ratio * 100 > 5",
    syncStatus: "synced",
    syncMessage: "",
    lastSyncedAt: "2026-08-08T11:59:00Z",
    createdAt: "2026-08-01T00:00:00Z",
    updatedAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

function foreignRow(over: Record<string, unknown> = {}) {
  return { name: "kube-prometheus-rules", groups: 2, rules: 7, managedBy: "prometheus-operator", ...over };
}

const EMPTY_REPORT = { created: [], skipped: [], notes: [] };

interface Call {
  method: string;
  url: string;
  body?: unknown;
}

function renderPage(
  opts: {
    permissions?: string[];
    rules?: Record<string, unknown>[];
    foreign?: Record<string, unknown>[];
    /** Replaces the 200 the rules list would otherwise answer with. */
    rulesResponse?: () => Response;
    /** Replaces the 200 the foreign list would otherwise answer with. */
    foreignResponse?: () => Response;
    onWrite?: (method: string, url: string, body: unknown) => Response | undefined;
    preview?: (body: unknown) => Response;
    importReport?: Record<string, unknown>;
    engaged?: boolean;
    /** ?rule=<id>, the deep link the Overview's firing rows point at. */
    rule?: string;
    /** The rows GET /api/v1/targets answers, for the external-target-down builder's target select. */
    targets?: { name: string }[];
  } = {},
) {
  const {
    permissions = ALERT_EDITOR,
    rules = [],
    foreign = [],
    rulesResponse,
    foreignResponse,
    onWrite,
    preview,
    importReport,
    engaged = false,
    rule,
    targets = [],
  } = opts;
  const rows = [...rules];
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body: unknown = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: href, body });

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody));

    // Longest-prefix first: /alert-rules/foreign, /import and /preview all
    // start with /alert-rules, and the list route would swallow them.
    if (href.startsWith("/api/v1/alert-rules/foreign")) {
      return Promise.resolve(foreignResponse ? foreignResponse() : json({ foreign }));
    }
    if (href.startsWith("/api/v1/alert-rules/preview")) {
      return Promise.resolve(
        preview ? preview(body) : json({ expr: "rendered_expr > 1", series: 3 }),
      );
    }
    if (href.startsWith("/api/v1/alert-rules/import")) {
      const override = onWrite?.(method, href, body);
      if (override) return Promise.resolve(override);
      return Promise.resolve(json(importReport ?? EMPTY_REPORT));
    }
    if (href.startsWith("/api/v1/alert-rules")) {
      const override = onWrite?.(method, href, body);
      if (override) return Promise.resolve(override);
      if (href.endsWith("/sync") && method === "POST") {
        return Promise.resolve(json({ status: "kicked" }, { status: 202 }));
      }
      if (method === "POST") {
        const created = ruleRow({ id: "created-id", ...(body as Record<string, unknown>) });
        rows.push(created);
        return Promise.resolve(json(created, { status: 201 }));
      }
      if (method === "PUT") {
        const id = href.slice("/api/v1/alert-rules/".length);
        const at = rows.findIndex((r) => r.id === id);
        const updated = ruleRow({ ...(rows[at] ?? {}), ...(body as Record<string, unknown>), id });
        if (at >= 0) rows[at] = updated;
        return Promise.resolve(json(updated));
      }
      if (method === "DELETE") {
        const id = href.slice("/api/v1/alert-rules/".length);
        const at = rows.findIndex((r) => r.id === id);
        if (at >= 0) rows.splice(at, 1);
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(rulesResponse ? rulesResponse() : json({ rules: rows }));
    }
    if (href.startsWith("/api/v1/targets")) {
      return Promise.resolve(json({ targets, nextCursor: "" }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  // `?at=` is the only way the app itself engages the Time Machine, so the
  // tests engage it the same way rather than by faking the context.
  const params = new URLSearchParams();
  if (engaged) params.set("at", AT);
  if (rule !== undefined) params.set("rule", rule);
  window.history.pushState({}, "", `/alerting${params.size > 0 ? `?${params}` : ""}`);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        <AlertingPage />
      </TimeMachineProvider>
    </QueryClientProvider>,
  );

  /** Every request the PAGE itself makes, i.e. excluding the /auth/me and
   *  /config chrome every route fetches regardless of what it renders. */
  const resourceCalls = () => calls.filter((c) => c.url.startsWith("/api/v1/alert-rules"));
  const previewCalls = () => calls.filter((c) => c.url.startsWith("/api/v1/alert-rules/preview"));
  return { ...utils, fetchMock, calls, resourceCalls, previewCalls, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
});

/* ── pure helpers ───────────────────────────────────────────────────────── */

describe("parsePromDuration", () => {
  it("reads the single-unit strings an operator actually types", () => {
    expect(parsePromDuration("30s")).toEqual({ ok: true, ns: 30_000_000_000 });
    expect(parsePromDuration("5m")).toEqual({ ok: true, ns: 300_000_000_000 });
    expect(parsePromDuration("2h")).toEqual({ ok: true, ns: 7_200_000_000_000 });
  });

  it("reads a composite in descending order, Prometheus's own grammar", () => {
    expect(parsePromDuration("1h30m")).toEqual({ ok: true, ns: 5_400_000_000_000 });
  });

  it("reads ms as ms and never as m followed by a stray s", () => {
    expect(parsePromDuration("500ms")).toEqual({ ok: true, ns: 500_000_000 });
  });

  it("treats an empty box as 0 — fire as soon as the expression holds", () => {
    expect(parsePromDuration("")).toEqual({ ok: true, ns: 0 });
    expect(parsePromDuration("   ")).toEqual({ ok: true, ns: 0 });
  });

  it("refuses a bare number: a duration without a unit is a guess", () => {
    const parsed = parsePromDuration("30");
    expect(parsed.ok).toBe(false);
    if (!parsed.ok) expect(parsed.message).toContain("unit");
  });

  it("refuses an unknown unit and an ascending composite", () => {
    expect(parsePromDuration("5x").ok).toBe(false);
    expect(parsePromDuration("30s5m").ok).toBe(false);
  });
});

describe("formatPromDuration", () => {
  it("renders the largest unit that divides evenly, never a compound", () => {
    expect(formatPromDuration(0)).toBe("0s");
    expect(formatPromDuration(300_000_000_000)).toBe("5m");
    expect(formatPromDuration(90_000_000_000)).toBe("90s");
    expect(formatPromDuration(500_000_000)).toBe("500ms");
  });

  it("round-trips every value the input box can produce", () => {
    for (const text of ["30s", "5m", "2h", "500ms"]) {
      const parsed = parsePromDuration(text);
      expect(parsed.ok).toBe(true);
      if (parsed.ok) expect(formatPromDuration(parsed.ns)).toBe(text);
    }
  });
});

describe("alertRuleRequestFrom", () => {
  it("writes `enabled` explicitly, because omitting it on a PUT ENABLES the rule", () => {
    const req = alertRuleRequestFrom(ruleRow({ enabled: false }) as never);
    expect(req.enabled).toBe(false);
    expect("enabled" in req).toBe(true);
  });

  it("carries the builder half and nothing the server owns", () => {
    const req = alertRuleRequestFrom(ruleRow() as never);
    expect(req).toEqual({
      name: "PairLossHigh",
      kind: "pair-loss",
      params: { protocol: "udp", thresholdPercent: 5 },
      severity: "warning",
      forNs: 300_000_000_000,
      labels: { team: "net" },
      annotations: { summary: "loss is up" },
      enabled: true,
    });
  });
});

describe("reservedLabelMessage", () => {
  it("refuses both reserved names in the server's own words", () => {
    expect(reservedLabelMessage("severity")).toBe('label "severity" is reserved by the console');
    expect(reservedLabelMessage("kconmon_ng_rule_id")).toBe('label "kconmon_ng_rule_id" is reserved by the console');
  });

  it("says nothing about any other name: the server is the authority on those", () => {
    expect(reservedLabelMessage("team")).toBeUndefined();
    expect(reservedLabelMessage("")).toBeUndefined();
  });
});

describe("problemField", () => {
  it("routes a renderer error to the param it names", () => {
    expect(problemField('alert rule: cannot render an expression from these fields: pair-loss: param "protocol" is required')).toBe(
      "protocol",
    );
  });

  it("routes the store's own field errors to their fields", () => {
    expect(problemField('alert rule: name "x y" must match ...')).toBe("name");
    expect(problemField('alert rule: severity "loud" must be one of info, warning, critical')).toBe("severity");
  });

  it("returns undefined for anything it cannot place, so the page banners it", () => {
    expect(problemField("alert rules are persisted configuration with no in-memory fallback")).toBeUndefined();
  });
});

describe("relativeTime", () => {
  it("says — when there is no instant, rather than inventing one", () => {
    expect(relativeTime(undefined, NOW)).toBe("—");
  });

  it("counts back in the largest whole unit", () => {
    expect(relativeTime("2026-08-08T11:59:30Z", NOW)).toBe("30s ago");
    expect(relativeTime("2026-08-08T11:30:00Z", NOW)).toBe("30m ago");
    expect(relativeTime("2026-08-08T09:00:00Z", NOW)).toBe("3h ago");
    expect(relativeTime("2026-08-05T12:00:00Z", NOW)).toBe("3d ago");
  });
});

describe("KIND_PARAMS", () => {
  it("mirrors internal/console/alerting/render.go's closed schemas exactly", () => {
    expect(KIND_PARAMS["pair-loss"].map((p) => p.key)).toEqual([
      "protocol",
      "thresholdPercent",
      "scope.sourceNode",
      "scope.destNode",
    ]);
    expect(KIND_PARAMS["zone-latency"].map((p) => p.key)).toEqual([
      "protocol",
      "quantile",
      "thresholdMs",
      "sourceZone",
      "destZone",
    ]);
    expect(KIND_PARAMS["dns-failures"].map((p) => p.key)).toEqual(["thresholdPercent"]);
    expect(KIND_PARAMS["http-ttfb"].map((p) => p.key)).toEqual(["thresholdMs", "url"]);
    // agent-missing takes NO params: `for` lives on the rule, so a forMinutes
    // param here would be a second place that means the same thing.
    expect(KIND_PARAMS["agent-missing"]).toEqual([]);
    expect(KIND_PARAMS["external-target-down"].map((p) => p.key)).toEqual(["targetName"]);
    expect(KIND_PARAMS["raw"].map((p) => p.key)).toEqual(["expr"]);
  });

  it("marks exactly the required ones required", () => {
    const required = (kind: keyof typeof KIND_PARAMS) =>
      KIND_PARAMS[kind].filter((p) => p.required).map((p) => p.key);
    expect(required("pair-loss")).toEqual(["protocol", "thresholdPercent"]);
    expect(required("zone-latency")).toEqual(["protocol", "quantile", "thresholdMs"]);
    expect(required("http-ttfb")).toEqual(["thresholdMs"]);
    expect(required("external-target-down")).toEqual([]);
    expect(required("raw")).toEqual(["expr"]);
  });
});

/* ── the page: permission floor ─────────────────────────────────────────── */

describe("AlertingPage gating", () => {
  it("a subject without alerts:read gets one card and the page asks for NOTHING", async () => {
    const { resourceCalls } = renderPage({ permissions: NO_ALERTS });
    expect(await screen.findByText(/Requires the alerts:read permission/)).toBeTruthy();
    await waitFor(() => expect(screen.queryByLabelText("Alert rules")).toBeNull());
    expect(resourceCalls()).toEqual([]);
  });

  it("viewer reads everything and can write nothing — HIDE, not disable", async () => {
    renderPage({ permissions: VIEWER, rules: [ruleRow()], foreign: [foreignRow()] });

    const list = await screen.findByLabelText("Alert rules");
    expect(within(list).getByText("PairLossHigh")).toBeTruthy();
    // The STATUS is still readable; only the control is gone.
    expect(within(list).getByText("enabled")).toBeTruthy();
    expect(screen.queryByLabelText("Enabled PairLossHigh")).toBeNull();
    expect(screen.queryByRole("button", { name: "New rule" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit PairLossHigh" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete PairLossHigh" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Sync PairLossHigh now" })).toBeNull();

    const foreign = await screen.findByLabelText("Foreign PrometheusRule objects");
    expect(within(foreign).getByText("kube-prometheus-rules")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Import kube-prometheus-rules" })).toBeNull();
  });

  it("alert-editor gets every write control: alerts:manage is that role's charter", async () => {
    renderPage({ permissions: ALERT_EDITOR, rules: [ruleRow()], foreign: [foreignRow()] });
    expect(await screen.findByRole("button", { name: "New rule" })).toBeTruthy();
    expect(await screen.findByLabelText("Enabled PairLossHigh")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Edit PairLossHigh" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sync PairLossHigh now" })).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Import kube-prometheus-rules" })).toBeTruthy();
  });
});

/* ── the page: the two dependencies, which fail differently ─────────────── */

describe("AlertingPage degraded states", () => {
  it("no database: BOTH families report the store 503 in the server's own words", async () => {
    renderPage({
      rulesResponse: () => problem(503, "alert rules not available", NO_DATABASE_DETAIL),
      foreignResponse: () => problem(503, "alert rules not available", NO_DATABASE_DETAIL),
    });
    await waitFor(() => expect(screen.getAllByText(NO_DATABASE_DETAIL).length).toBe(2));
  });

  it("alerting off: the rules list keeps WORKING and only the sync family 409s", async () => {
    renderPage({
      rules: [ruleRow()],
      foreignResponse: () => problem(409, "prometheus rule sync is disabled", ALERTING_DISABLED_DETAIL),
    });

    // CRUD is untouched: the rule is listed and the builder is offered.
    const list = await screen.findByLabelText("Alert rules");
    expect(within(list).getByText("PairLossHigh")).toBeTruthy();
    expect(screen.getByRole("button", { name: "New rule" })).toBeTruthy();

    // Both halves that need the cluster say so: the managed section discloses
    // it up front (finding 8) and the foreign list reports why it is empty.
    expect(await screen.findByTestId("sync-disabled-notice")).toHaveTextContent(ALERTING_DISABLED_DETAIL);
    expect(screen.getAllByText(ALERTING_DISABLED_DETAIL).length).toBeGreaterThan(0);
    expect(screen.queryByLabelText("Foreign PrometheusRule objects")).toBeNull();
  });

  it("a 409 from Sync now becomes the rules section's own banner", async () => {
    renderPage({
      rules: [ruleRow()],
      onWrite: (method, url) =>
        method === "POST" && url.endsWith("/sync")
          ? problem(409, "prometheus rule sync is disabled", ALERTING_DISABLED_DETAIL)
          : undefined,
    });
    fireEvent.click(await screen.findByRole("button", { name: "Sync PairLossHigh now" }));
    const banner = await screen.findByTestId("rules-sync-banner");
    expect(banner.textContent).toContain(ALERTING_DISABLED_DETAIL);
  });

  /* QA scope 4, finding #6: sync-being-off is ONE state, and the console gave it
     three verdicts at once — the managed section's amber role=status notice, the
     foreign section's red role=alert line and the click banner's red line, all
     printing the SAME server paragraph. Amber wins: nothing is broken and no
     rule is at risk, sync is simply not a thing this console does. */
  it("gives the sync-disabled state ONE tone wherever it is reported", async () => {
    renderPage({
      rules: [ruleRow()],
      foreignResponse: () => problem(409, "prometheus rule sync is disabled", ALERTING_DISABLED_DETAIL),
    });

    const managed = await screen.findByTestId("sync-disabled-notice");
    const foreign = await screen.findByTestId("foreign-sync-disabled-notice");
    expect(foreign).toHaveTextContent(ALERTING_DISABLED_DETAIL);
    expect(foreign.className).toBe(managed.className);
    expect(foreign.getAttribute("role")).toBe(managed.getAttribute("role"));
    expect(managed.getAttribute("role")).toBe("status");
    expect(managed.className).toContain("text-health-warn");
    expect(managed.className).not.toContain("text-health-bad");
  });

  it("keeps the RED line for a failure that really is one", async () => {
    renderPage({ foreignResponse: () => problem(503, "alert rules not available", NO_DATABASE_DETAIL) });
    const line = await screen.findByText(NO_DATABASE_DETAIL);
    expect(line.getAttribute("role")).toBe("alert");
    expect(line.className).toContain("text-health-bad");
  });

  it("uses the same tone for the 409 the Sync button itself comes back with", async () => {
    renderPage({
      rules: [ruleRow()],
      onWrite: (method, url) =>
        method === "POST" && url.endsWith("/sync")
          ? problem(409, "prometheus rule sync is disabled", ALERTING_DISABLED_DETAIL)
          : undefined,
    });
    fireEvent.click(await screen.findByRole("button", { name: "Sync PairLossHigh now" }));
    const banner = await screen.findByTestId("rules-sync-banner");
    expect(banner.getAttribute("role")).toBe("status");
    expect(banner.className).toContain("text-health-warn");
  });

  /* The disabled Sync button sat at disabled:opacity-50. Measured in Chrome
     against the live console, light theme, #1e212f label on #ffffff:
       opacity .50 → 3.18:1  (under WCAG AA's 4.5:1 for 14px text)
       opacity .65 → 5.03:1
     Dark was already 4.89:1 at .50 and goes to 7.44:1 at .65, so one value
     serves both themes. */
  it("keeps a disabled button readable rather than half-erased", async () => {
    renderPage({
      rules: [ruleRow()],
      foreignResponse: () => problem(409, "prometheus rule sync is disabled", ALERTING_DISABLED_DETAIL),
    });
    const btn = await screen.findByRole("button", { name: "Sync PairLossHigh now" });
    await waitFor(() => expect(btn).toBeDisabled());
    expect(btn.className).toContain("disabled:opacity-65");
    expect(btn.className).not.toContain("disabled:opacity-50");
  });
});

/* ── the page: the list ─────────────────────────────────────────────────── */

describe("AlertingPage rule list", () => {
  it("renders drift as its own chip and keeps the reconciler's sentence reachable", async () => {
    renderPage({
      permissions: VIEWER,
      rules: [
        ruleRow({
          syncStatus: "drift",
          syncMessage: "the live object diverged and was re-asserted",
          lastSyncedAt: "2026-08-08T11:30:00Z",
        }),
      ],
    });
    const list = await screen.findByLabelText("Alert rules");
    const chip = within(list).getByTestId("sync-status");
    expect(chip.textContent).toContain("drift");
    // Reachable without expanding (title), and visible once expanded.
    expect(chip.getAttribute("title")).toBe("the live object diverged and was re-asserted");
    fireEvent.click(within(list).getByRole("button", { name: "Details for PairLossHigh" }));
    expect(within(list).getByText("the live object diverged and was re-asserted")).toBeTruthy();
  });

  it("shows the rendered expression on expand, and never before", async () => {
    renderPage({ permissions: VIEWER, rules: [ruleRow()] });
    const list = await screen.findByLabelText("Alert rules");
    expect(within(list).queryByText("kconmon_ng_udp_packet_loss_ratio * 100 > 5")).toBeNull();
    fireEvent.click(within(list).getByRole("button", { name: "Details for PairLossHigh" }));
    expect(within(list).getByText("kconmon_ng_udp_packet_loss_ratio * 100 > 5")).toBeTruthy();
  });

  it("the details toggle names the block it opens", async () => {
    renderPage({ permissions: VIEWER, rules: [ruleRow()] });
    const list = await screen.findByLabelText("Alert rules");
    const toggle = within(list).getByRole("button", { name: "Details for PairLossHigh" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    const target = toggle.getAttribute("aria-controls");
    expect(target).toBeTruthy();
    // Collapsed there is nothing to point at; expanded the id must resolve, or
    // aria-controls is a dangling reference.
    expect(document.getElementById(target as string)).toBeNull();
    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(document.getElementById(target as string)).toBeTruthy();
  });

  it("an unsynced rule is not an error state", async () => {
    renderPage({ permissions: VIEWER, rules: [ruleRow({ syncStatus: "unsynced", lastSyncedAt: undefined })] });
    const chip = await screen.findByTestId("sync-status");
    expect(chip.textContent).toContain("unsynced");
    expect(screen.getByTestId("last-synced").textContent).toBe("—");
  });

  it("the enabled toggle PUTs the WHOLE rule back, flipping one field", async () => {
    const { resourceCalls } = renderPage({ rules: [ruleRow()] });
    fireEvent.click(await screen.findByLabelText("Enabled PairLossHigh"));
    await waitFor(() => expect(resourceCalls().some((c) => c.method === "PUT")).toBe(true));
    const put = resourceCalls().find((c) => c.method === "PUT");
    expect(put?.url).toBe("/api/v1/alert-rules/11111111-1111-1111-1111-111111111111");
    expect(put?.body).toEqual({
      name: "PairLossHigh",
      kind: "pair-loss",
      params: { protocol: "udp", thresholdPercent: 5 },
      severity: "warning",
      forNs: 300_000_000_000,
      labels: { team: "net" },
      annotations: { summary: "loss is up" },
      enabled: false,
    });
  });

  it("delete asks once, then DELETEs the id", async () => {
    const { resourceCalls } = renderPage({ rules: [ruleRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Delete PairLossHigh" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm delete PairLossHigh" }));
    await waitFor(() => expect(resourceCalls().some((c) => c.method === "DELETE")).toBe(true));
    expect(resourceCalls().find((c) => c.method === "DELETE")?.url).toBe(
      "/api/v1/alert-rules/11111111-1111-1111-1111-111111111111",
    );
  });

  it("a 202 sync ack says the work was REQUESTED and claims no outcome", async () => {
    const { resourceCalls } = renderPage({ rules: [ruleRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Sync PairLossHigh now" }));
    const ack = await screen.findByTestId("sync-ack");
    // aria-live, so a screen reader hears the ack it cannot see appear.
    expect(ack.getAttribute("role")).toBe("status");
    expect(ack.textContent).toMatch(/requested/i);
    expect(ack.textContent).not.toMatch(/synced|succeeded|applied/i);
    expect(resourceCalls().some((c) => c.method === "POST" && c.url.endsWith("/sync"))).toBe(true);
  });
});

/* ── the page: the builder ──────────────────────────────────────────────── */

async function openBuilder() {
  fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
  return screen.findByRole("form", { name: "New alert rule" });
}

function setKind(kind: string) {
  fireEvent.change(screen.getByLabelText("Kind"), { target: { value: kind } });
}

describe("AlertingPage builder", () => {
  it("renders the param fields of the SELECTED kind and nothing else", async () => {
    renderPage();
    await openBuilder();

    // pair-loss is the default kind.
    expect(screen.getByLabelText("Protocol")).toBeTruthy();
    expect(screen.getByLabelText("Loss threshold (%)")).toBeTruthy();
    expect(screen.getByLabelText("Source node")).toBeTruthy();
    expect(screen.queryByLabelText("PromQL expression")).toBeNull();

    setKind("raw");
    expect(screen.getByLabelText("PromQL expression")).toBeTruthy();
    expect(screen.queryByLabelText("Protocol")).toBeNull();

    setKind("agent-missing");
    expect(screen.getByTestId("no-params").textContent).toMatch(/no parameters/i);
  });

  it("opens a row whose kind has no template in this build without falling over", async () => {
    // alert_rules.kind's CHECK constraint accepts cert-expiry; no template
    // renders it and the API enum leaves it out, so such a row can only arrive
    // from another build. Listing it must work and editing it must say so.
    renderPage({ rules: [ruleRow({ kind: "cert-expiry", params: {} })] });
    fireEvent.click(await screen.findByRole("button", { name: "Edit PairLossHigh" }));
    expect((await screen.findByTestId("unknown-kind")).textContent).toContain("cert-expiry");
    expect(screen.queryByTestId("no-params")).toBeNull();
  });

  it("refuses a reserved label CLIENT-side, in the server's words, without a request", async () => {
    const { resourceCalls } = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "PairLossHigh" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.change(screen.getByLabelText("Label name 1"), { target: { value: "severity" } });
    fireEvent.change(screen.getByLabelText("Label value 1"), { target: { value: "critical" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));

    expect(await screen.findByText('label "severity" is reserved by the console')).toBeTruthy();
    await waitFor(() => expect(resourceCalls().some((c) => c.method === "POST" && !c.url.includes("preview"))).toBe(false));
  });

  it("previews on a debounce: three keystrokes are one request", async () => {
    const { previewCalls } = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "1" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "12" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "123" } });
    await waitFor(() => expect(previewCalls().length).toBe(1), { timeout: 3000 });
    expect(previewCalls()[0]?.body).toMatchObject({ kind: "pair-loss", params: { protocol: "udp", thresholdPercent: 123 } });
  });

  it("a preview that rendered but could not be EVALUATED says so, and does not claim 0 series", async () => {
    renderPage({
      preview: () =>
        json({
          expr: "kconmon_ng_udp_packet_loss_ratio * 100 > 5",
          series: 0,
          error: "prometheus is not configured on this console, so the expression could not be evaluated",
        }),
    });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "5" } });

    const panel = await screen.findByLabelText("Expression preview");
    await waitFor(() => expect(panel.textContent).toContain("kconmon_ng_udp_packet_loss_ratio * 100 > 5"));
    expect(panel.textContent).toContain(
      "prometheus is not configured on this console, so the expression could not be evaluated",
    );
    expect(panel.textContent).toMatch(/unknown/i);
    expect(panel.textContent).not.toMatch(/matches 0 series/i);
  });

  it("a clean preview states the count as the answer it is", async () => {
    renderPage({ preview: () => json({ expr: "up > 0", series: 0 }) });
    await openBuilder();
    setKind("raw");
    fireEvent.change(screen.getByLabelText("PromQL expression"), { target: { value: "up > 0" } });
    const panel = await screen.findByLabelText("Expression preview");
    await waitFor(() => expect(panel.textContent).toMatch(/0 series/));
    expect(panel.textContent).not.toMatch(/unknown/i);
  });

  it("does not preview until the required params are there — no request to collect a 422", async () => {
    const { previewCalls } = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Half" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    await new Promise((r) => setTimeout(r, 600));
    expect(previewCalls()).toEqual([]);
  });

  it("creates with the whole builder body, params TYPED and the duration in nanoseconds", async () => {
    const { resourceCalls } = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "PairLossHigh" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "5" } });
    fireEvent.change(screen.getByLabelText("Source node"), { target: { value: "node-a" } });
    fireEvent.change(screen.getByLabelText("Severity"), { target: { value: "critical" } });
    fireEvent.change(screen.getByLabelText("For"), { target: { value: "5m" } });
    fireEvent.click(screen.getByRole("button", { name: "Add annotation" }));
    fireEvent.change(screen.getByLabelText("Annotation name 1"), { target: { value: "summary" } });
    fireEvent.change(screen.getByLabelText("Annotation value 1"), { target: { value: "loss is up" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));

    await waitFor(() =>
      expect(resourceCalls().some((c) => c.method === "POST" && !c.url.includes("preview"))).toBe(true),
    );
    const post = resourceCalls().find((c) => c.method === "POST" && !c.url.includes("preview"));
    expect(post?.url).toBe("/api/v1/alert-rules");
    expect(post?.body).toEqual({
      name: "PairLossHigh",
      kind: "pair-loss",
      params: { protocol: "udp", thresholdPercent: 5, scope: { sourceNode: "node-a" } },
      severity: "critical",
      forNs: 300_000_000_000,
      labels: {},
      annotations: { summary: "loss is up" },
      enabled: true,
    });
  });

  it("omits an empty optional param entirely: params are CLOSED per kind", async () => {
    const { resourceCalls } = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Bare" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "tcp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "1" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() =>
      expect(resourceCalls().some((c) => c.method === "POST" && !c.url.includes("preview"))).toBe(true),
    );
    const post = resourceCalls().find((c) => c.method === "POST" && !c.url.includes("preview"));
    expect((post?.body as { params: unknown }).params).toEqual({ protocol: "tcp", thresholdPercent: 1 });
  });

  it("edit loads the stored rule and PUTs a full replace", async () => {
    const { resourceCalls } = renderPage({ rules: [ruleRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Edit PairLossHigh" }));
    const form = await screen.findByRole("form", { name: "Edit PairLossHigh" });
    expect((within(form).getByLabelText("For") as HTMLInputElement).value).toBe("5m");
    expect((within(form).getByLabelText("Loss threshold (%)") as HTMLInputElement).value).toBe("5");
    fireEvent.change(within(form).getByLabelText("Severity"), { target: { value: "critical" } });
    fireEvent.click(within(form).getByRole("button", { name: "Save rule" }));

    await waitFor(() => expect(resourceCalls().some((c) => c.method === "PUT")).toBe(true));
    const put = resourceCalls().find((c) => c.method === "PUT");
    expect(put?.url).toBe("/api/v1/alert-rules/11111111-1111-1111-1111-111111111111");
    expect(put?.body).toEqual({
      name: "PairLossHigh",
      kind: "pair-loss",
      params: { protocol: "udp", thresholdPercent: 5 },
      severity: "critical",
      forNs: 300_000_000_000,
      labels: { team: "net" },
      annotations: { summary: "loss is up" },
      enabled: true,
    });
  });

  it("renders a server 422 on the field it names, verbatim", async () => {
    const detail =
      'alert rule: cannot render an expression from these fields: pair-loss: param "thresholdPercent" must be ' +
      "between 0 and 100, got 400";
    renderPage({
      onWrite: (method, url) =>
        method === "POST" && url === "/api/v1/alert-rules" ? problem(422, "invalid alert rule", detail) : undefined,
    });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "TooBig" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "400" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
    const field = await screen.findByTestId("field-error-thresholdPercent");
    expect(field.textContent).toBe(detail);
  });

  it("banners a 422 it cannot place on a field", async () => {
    const detail = 'alert rule: name "PairLossHigh" is already taken; alert rule names are unique, case-insensitively';
    renderPage({
      onWrite: (method, url) =>
        method === "POST" && url === "/api/v1/alert-rules" ? problem(422, "invalid alert rule", detail) : undefined,
    });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "PairLossHigh" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
    expect((await screen.findByTestId("field-error-name")).textContent).toBe(detail);
  });
});

/* ── the page: foreign rules and adoption ───────────────────────────────── */

describe("AlertingPage foreign rules", () => {
  it("lists the four facts and writes — for an unlabelled object, not blank", async () => {
    renderPage({ permissions: VIEWER, foreign: [foreignRow({ name: "hand-written", managedBy: "" })] });
    const list = await screen.findByLabelText("Foreign PrometheusRule objects");
    const row = within(list).getByText("hand-written").closest("li");
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain("2 groups");
    expect(row?.textContent).toContain("7 rules");
    expect(within(row as HTMLElement).getByTestId("managed-by").textContent).toBe("—");
  });

  it("renders created, skipped AND notes — all three, verbatim, never a toast", async () => {
    renderPage({
      foreign: [foreignRow()],
      importReport: {
        created: ["HighLatency", "PodLoss"],
        skipped: [
          { name: "job:rate5m", reason: "recording rule -- the console builder has no recording model, only alerting rules" },
          { name: "PairLossHigh", reason: "name already taken: alert rule names are unique case-insensitively, and adoption will not rename a rule to make room for it" },
        ],
        notes: [
          { name: "HighLatency", note: "severity \"page\" is outside the closed set; stored as warning" },
        ],
      },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Import kube-prometheus-rules" }));

    const report = await screen.findByTestId("import-report");
    // Each array lands in its OWN block. A note names a rule that was created,
    // so the same name legitimately appears twice — scoping the query is what
    // proves the page did not fold the three lists into one.
    const created = within(report).getByTestId("import-created");
    expect(within(created).getByText("HighLatency")).toBeTruthy();
    expect(within(created).getByText("PodLoss")).toBeTruthy();

    const skipped = within(report).getByTestId("import-skipped");
    expect(
      within(skipped).getByText("recording rule -- the console builder has no recording model, only alerting rules"),
    ).toBeTruthy();
    expect(
      within(skipped).getByText(
        "name already taken: alert rule names are unique case-insensitively, and adoption will not rename a rule to make room for it",
      ),
    ).toBeTruthy();

    const notes = within(report).getByTestId("import-notes");
    expect(within(notes).getByText('severity "page" is outside the closed set; stored as warning')).toBeTruthy();

    expect(report.textContent).toContain("the original object is untouched");
    expect(report.textContent).toContain("until its owner removes it");
  });

  it("an import that adopted nothing still shows all three headings", async () => {
    renderPage({ foreign: [foreignRow()], importReport: EMPTY_REPORT });
    fireEvent.click(await screen.findByRole("button", { name: "Import kube-prometheus-rules" }));
    const report = await screen.findByTestId("import-report");
    for (const heading of ["Created", "Skipped", "Notes"]) {
      expect(within(report).getByText(heading)).toBeTruthy();
    }
  });

  /* Polite, not assertive — the import succeeded; its refusal is the role="alert" line beside it. */
  it("announces the report politely instead of landing silently", async () => {
    renderPage({ foreign: [foreignRow()], importReport: EMPTY_REPORT });
    fireEvent.click(await screen.findByRole("button", { name: "Import kube-prometheus-rules" }));
    const report = await screen.findByTestId("import-report");
    expect(report).toHaveAttribute("role", "status");
  });

  it("sends the object NAME and nothing else", async () => {
    const { resourceCalls } = renderPage({ foreign: [foreignRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Import kube-prometheus-rules" }));
    await waitFor(() => expect(resourceCalls().some((c) => c.url.includes("/import"))).toBe(true));
    expect(resourceCalls().find((c) => c.url.includes("/import"))?.body).toEqual({ name: "kube-prometheus-rules" });
  });
});

/* ── the page: Time Machine ─────────────────────────────────────────────── */

describe("AlertingPage under the Time Machine", () => {
  it("disables every write and leaves every read alone", async () => {
    const { resourceCalls } = renderPage({ rules: [ruleRow()], foreign: [foreignRow()], engaged: true });

    // Reads still happened, and still rendered.
    const list = await screen.findByLabelText("Alert rules");
    expect(within(list).getByText("PairLossHigh")).toBeTruthy();
    expect(resourceCalls().some((c) => c.method === "GET" && c.url === "/api/v1/alert-rules")).toBe(true);
    expect(resourceCalls().some((c) => c.url.startsWith("/api/v1/alert-rules/foreign"))).toBe(true);

    // Writes EXIST (the permission is held) and are DISABLED (the time is not now).
    expect((screen.getByRole("button", { name: "New rule" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByLabelText("Enabled PairLossHigh") as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Edit PairLossHigh" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Delete PairLossHigh" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Sync PairLossHigh now" }) as HTMLButtonElement).disabled).toBe(true);
    expect(
      (await screen.findByRole("button", { name: "Import kube-prometheus-rules" })) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);
  });
});

/* ── QA round 1, finding #17: the firing alert's link names its rule ─────── */

describe("AlertingPage — ?rule= opens the row it names", () => {
  const OTHER = "22222222-2222-2222-2222-222222222222";

  it("expands that rule's details on arrival and leaves the others closed", async () => {
    renderPage({
      rules: [ruleRow(), ruleRow({ id: OTHER, name: "PairRttHigh" })],
      rule: OTHER,
    });

    const rows = await screen.findAllByTestId("rule-row");
    expect(within(rows[1]).getByRole("button", { name: "Details for PairRttHigh" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    expect(within(rows[0]).getByRole("button", { name: "Details for PairLossHigh" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
  });

  it("scrolls the named row into view, guarded for a DOM that has no scrollIntoView", async () => {
    const scrollIntoView = vi.fn();
    // jsdom defines no scrollIntoView at all; the page calls it optionally, so
    // this both proves the call and proves the guard is what stands in for it.
    Element.prototype.scrollIntoView = scrollIntoView;
    try {
      renderPage({ rules: [ruleRow()], rule: ruleRow().id as string });
      await screen.findByTestId("rule-row");
      expect(scrollIntoView).toHaveBeenCalled();
    } finally {
      delete (Element.prototype as Partial<Element>).scrollIntoView;
    }
  });

  it("changes nothing without the param — every row starts collapsed", async () => {
    renderPage({ rules: [ruleRow()] });

    const row = await screen.findByTestId("rule-row");
    expect(within(row).getByRole("button", { name: "Details for PairLossHigh" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
  });

  it("ignores an id that matches nothing rather than opening something arbitrary", async () => {
    renderPage({ rules: [ruleRow()], rule: "not-a-rule" });

    const row = await screen.findByTestId("rule-row");
    expect(within(row).getByRole("button", { name: "Details for PairLossHigh" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
  });
});

/* ── QA round 5 ─────────────────────────────────────────────────────────── */

/* #14. The native checkbox rendered in the OS accent colour — a blue box beside
   this console's own controls, and a bright white one in dark mode. */
describe("the enabled checkbox wears the console's own theming (#14)", () => {
  it("carries the shared class on the rule row", async () => {
    renderPage({ rules: [ruleRow()] });
    const box = await screen.findByRole("checkbox", { name: "Enabled PairLossHigh" });
    expect(box.className).toContain("accent-primary");
    expect(box.className).toContain("size-4");
  });

  it("and on the builder's own Enabled box", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    const box = screen.getByRole("checkbox");
    expect(box.className).toContain("accent-primary");
  });
});

/* #15. "Add label" produced TWO identical empty boxes with nothing on screen
   saying which is which; the aria-labels were always right, but the order is
   the one thing a two-box row does not communicate. */
describe("the builder's K/V rows name their boxes (#15)", () => {
  it("places name/value placeholders on both label boxes", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    expect(screen.getByLabelText("Label name 1")).toHaveAttribute("placeholder", "name");
    expect(screen.getByLabelText("Label value 1")).toHaveAttribute("placeholder", "value");
  });

  it("and on the annotation boxes, which are the same row shape", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.click(screen.getByRole("button", { name: "Add annotation" }));
    expect(screen.getByLabelText("Annotation name 1")).toHaveAttribute("placeholder", "name");
    expect(screen.getByLabelText("Annotation value 1")).toHaveAttribute("placeholder", "value");
  });
});

/* #15, second half. The value has to match a target's NAME exactly or the
   rendered expression selects nothing, and a free box gave an operator no way
   to know what the names are. */
describe("external-target-down picks a real target (#15)", () => {
  async function openBuilder(opts: Parameters<typeof renderPage>[0] = {}) {
    renderPage(opts);
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "external-target-down" } });
  }

  it("offers a select over the console's targets when targets:read is held", async () => {
    await openBuilder({
      permissions: [...ALERT_EDITOR, "targets:read"],
      targets: [{ name: "edge-gw" }, { name: "status-page" }],
    });
    // The select appears once the list ARRIVES: rendering it while the query is
    // in flight would show an empty dropdown that silently omits the stored value.
    await waitFor(() => expect(screen.getByLabelText("Target name").tagName).toBe("SELECT"));
    const select = screen.getByLabelText("Target name") as HTMLSelectElement;
    expect([...select.options].map((o) => o.textContent)).toEqual([
      "every external target",
      "edge-gw",
      "status-page",
    ]);
    // "" is a real, meaningful value here and is NAMED rather than left blank.
    expect(select.options[0].value).toBe("");
  });

  it("falls back to the free box, with a hint, when targets:read is not held", async () => {
    await openBuilder({ permissions: ALERT_EDITOR, targets: [{ name: "edge-gw" }] });
    const input = await screen.findByLabelText("Target name");
    expect(input.tagName).toBe("INPUT");
    expect(screen.getByText(/type the exact target name/i)).toBeInTheDocument();
  });

  it("asks for NO targets at all when the kind has no target field", async () => {
    const { resourceCalls } = renderPage({ permissions: [...ALERT_EDITOR, "targets:read"] });
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    await waitFor(() => expect(screen.getByLabelText("Kind")).toBeInTheDocument());
    expect(resourceCalls().some((c) => c.url.startsWith("/api/v1/targets"))).toBe(false);
  });

  it("keeps a stored value the list no longer contains, rather than widening the rule", async () => {
    renderPage({
      permissions: [...ALERT_EDITOR, "targets:read"],
      targets: [{ name: "edge-gw" }],
      rules: [ruleRow({ kind: "external-target-down", params: { targetName: "deleted-gw" } })],
    });
    fireEvent.click(await screen.findByRole("button", { name: "Edit PairLossHigh" }));
    await waitFor(() => expect(screen.getByLabelText("Target name").tagName).toBe("SELECT"));
    const select = screen.getByLabelText("Target name") as HTMLSelectElement;
    expect(select.value).toBe("deleted-gw");
    expect([...select.options].map((o) => o.textContent)).toContain("deleted-gw (no such target)");
  });
});

/* #18. Following a link to a rule somebody had since deleted produced the
   ordinary list with nothing opened and nothing said — indistinguishable from
   a link that worked. */
describe("?rule= naming nothing says so (#18)", () => {
  it("shows the notice once the list has settled", async () => {
    renderPage({ rules: [ruleRow()], rule: "99999999-9999-9999-9999-999999999999" });
    const notice = await screen.findByTestId("unknown-rule-notice");
    expect(notice).toHaveTextContent("No rule matches this link — it may have been deleted.");
  });

  it("says nothing when the param names a rule that IS there", async () => {
    renderPage({ rules: [ruleRow()], rule: ruleRow().id as string });
    expect(await screen.findByLabelText("Alert rules")).toBeInTheDocument();
    expect(screen.queryByTestId("unknown-rule-notice")).toBeNull();
  });

  it("says nothing with no ?rule= at all", async () => {
    renderPage({ rules: [ruleRow()] });
    expect(await screen.findByLabelText("Alert rules")).toBeInTheDocument();
    expect(screen.queryByTestId("unknown-rule-notice")).toBeNull();
  });

  it("stays silent while the list is still loading — a flash on cold load is worse", async () => {
    renderPage({ rulesResponse: () => problem(502, "unavailable", "boom"), rule: "nope" });
    await screen.findByText("boom");
    // The list ERRORED, so it never settled: nothing is known about this id.
    expect(screen.queryByTestId("unknown-rule-notice")).toBeNull();
  });
});

/* #17, the alerting family. */
describe("one rule per click storm (#17)", () => {
  it("POSTs once for three rapid clicks", async () => {
    const { resourceCalls } = renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "LossHigh" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "udp" } });
    fireEvent.change(screen.getByLabelText(/loss threshold/i), { target: { value: "5" } });

    const submit = screen.getByRole("button", { name: "Create rule" });
    await act(async () => {
      submit.click();
      submit.click();
      submit.click();
    });

    await waitFor(() => expect(resourceCalls().filter((c) => c.method === "POST").length).toBeGreaterThan(0));
    expect(resourceCalls().filter((c) => c.method === "POST" && c.url === "/api/v1/alert-rules")).toHaveLength(1);
  });
});

/* ── QA scope 5 ─────────────────────────────────────────────────────────── */

/* #7. The preview PROVED Prometheus refuses the expression, and Create saved it
   anyway. On a sync-enabled console that expression goes into the rule bundle,
   and a bundle Prometheus cannot load stops applying every OTHER rule in it. */
describe("a proven-bad expression cannot be saved (#7)", () => {
  const rejected = () =>
    json({ expr: "up >", series: 0, error: "prometheus rejected the expression: parse error", rejected: true });

  async function draftRaw(expr: string) {
    setKind("raw");
    fireEvent.change(screen.getByLabelText("PromQL expression"), { target: { value: expr } });
  }

  it("blocks Create, and says why, once the preview comes back rejected", async () => {
    const { resourceCalls } = renderPage({ preview: rejected });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Bad" } });
    await draftRaw("up >");

    expect(await screen.findByTestId("rejected-expr-block")).toBeInTheDocument();
    const create = screen.getByRole("button", { name: "Create rule" });
    await waitFor(() => expect(create).toBeDisabled());

    fireEvent.click(create);
    await new Promise((r) => setTimeout(r, 50));
    // The preview POSTs to /alert-rules/preview; a SAVE would POST to the
    // collection itself, and that is the call that must not have happened.
    expect(resourceCalls().filter((c) => c.method === "POST" && c.url === "/api/v1/alert-rules")).toEqual([]);
  });

  it("lifts the block the moment the expression is edited", async () => {
    let reject = true;
    renderPage({
      preview: () => (reject ? rejected() : json({ expr: "up > 0", series: 2 })),
    });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Bad" } });
    await draftRaw("up >");
    await screen.findByTestId("rejected-expr-block");

    reject = false;
    await draftRaw("up > 0");
    await waitFor(() => expect(screen.queryByTestId("rejected-expr-block")).toBeNull());
    expect(screen.getByRole("button", { name: "Create rule" })).toBeEnabled();
  });

  it("stays permissive when the preview merely could NOT be evaluated", async () => {
    renderPage({
      preview: () =>
        json({
          expr: "up > 0",
          series: 0,
          error: "prometheus could not be reached to evaluate the expression",
        }),
    });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Unchecked" } });
    await draftRaw("up > 0");

    const panel = await screen.findByLabelText("Expression preview");
    await waitFor(() => expect(panel.textContent).toContain("could not be reached"));
    // Not known to be bad is not bad: there is no PromQL parser here to say
    // otherwise, so the save stays available.
    expect(screen.queryByTestId("rejected-expr-block")).toBeNull();
    expect(screen.getByRole("button", { name: "Create rule" })).toBeEnabled();
  });

  it("never blocks a draft that was never previewed at all", async () => {
    renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Fresh" } });
    expect(screen.getByRole("button", { name: "Create rule" })).toBeEnabled();
    expect(screen.queryByTestId("rejected-expr-block")).toBeNull();
  });
});

/* #8. With sync off the managed section said nothing up front and every Sync
   button was live, answering a 409 only once clicked. */
describe("a console with rule sync off says so up front (#8)", () => {
  const off = () => problem(409, "prometheus rule sync is disabled", ALERTING_DISABLED_DETAIL);

  it("discloses the reason at the top of the managed section", async () => {
    renderPage({ rules: [ruleRow()], foreignResponse: off });
    const notice = await screen.findByTestId("sync-disabled-notice");
    // The server's own paragraph, not a paraphrase of it.
    expect(notice).toHaveTextContent(ALERTING_DISABLED_DETAIL);
  });

  it("disables every Sync button, carrying the reason", async () => {
    renderPage({ rules: [ruleRow()], foreignResponse: off });
    const sync = await screen.findByRole("button", { name: "Sync PairLossHigh now" });
    await waitFor(() => expect(sync).toBeDisabled());
    expect(sync).toHaveAttribute("title", ALERTING_DISABLED_DETAIL);
  });

  it("says nothing, and keeps Sync live, on a console where sync IS running", async () => {
    renderPage({ rules: [ruleRow()] });
    const sync = await screen.findByRole("button", { name: "Sync PairLossHigh now" });
    expect(sync).toBeEnabled();
    expect(screen.queryByTestId("sync-disabled-notice")).toBeNull();
  });
});

/* #18. ?rule= opened a row on arrival, but expanding a row could not produce
   the link — so a rule found by scrolling could not be handed to anyone. */
describe("expanding a row writes ?rule= (#18)", () => {
  const FIRST_ID = "11111111-1111-1111-1111-111111111111";
  const SECOND_ID = "22222222-2222-2222-2222-222222222222";
  const ruleParam = () => new URLSearchParams(window.location.search).get("rule");

  it("writes the rule id when the details open, and clears it when they close", async () => {
    renderPage({ rules: [ruleRow()] });
    const details = await screen.findByRole("button", { name: "Details for PairLossHigh" });

    fireEvent.click(details);
    await waitFor(() => expect(ruleParam()).toBe(FIRST_ID));

    fireEvent.click(details);
    await waitFor(() => expect(ruleParam()).toBeNull());
  });

  it("leaves the param alone when a DIFFERENT row is collapsed", async () => {
    renderPage({ rules: [ruleRow(), ruleRow({ id: SECOND_ID, name: "Other" })] });
    const first = await screen.findByRole("button", { name: "Details for PairLossHigh" });
    const second = screen.getByRole("button", { name: "Details for Other" });

    fireEvent.click(first);
    await waitFor(() => expect(ruleParam()).toBe(FIRST_ID));

    // Opening the second takes the link; closing the second clears it, and
    // must not have been able to clear the FIRST row's link on its way.
    fireEvent.click(second);
    await waitFor(() => expect(ruleParam()).toBe(SECOND_ID));
    fireEvent.click(first);
    // The first row collapsing while the param names the SECOND leaves it be.
    await waitFor(() => expect(ruleParam()).toBe(SECOND_ID));
  });
});

/* #23. Cancel on the longest form in the console threw away every field the
   operator had filled in, without a word. */
describe("the rule builder asks before discarding unsaved work (#23)", () => {
  it("closes immediately when nothing was typed", async () => {
    renderPage();
    await openBuilder();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByLabelText("Name")).toBeNull());
  });

  it("asks once when the draft is dirty, and keeps the form on Keep editing", async () => {
    renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "HalfTyped" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(await screen.findByText("Discard the changes?")).toBeInTheDocument();
    expect(screen.getByLabelText("Name")).toHaveValue("HalfTyped");

    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.getByLabelText("Name")).toHaveValue("HalfTyped");
    expect(screen.queryByText("Discard the changes?")).toBeNull();
  });

  it("closes on the second, explicit answer", async () => {
    renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "HalfTyped" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));

    await waitFor(() => expect(screen.queryByLabelText("Name")).toBeNull());
  });
});

/* ── the owner's rule: every list on every page is paged ────────────────── */

describe("the managed rules list is PAGED", () => {
  const rules = (n: number) =>
    Array.from({ length: n }, (_, i) =>
      ruleRow({ id: `rule-${String(i).padStart(3, "0")}`, name: `Rule${String(i).padStart(3, "0")}` }),
    );

  it("shows one page-worth and counts it against the whole rule set", async () => {
    renderPage({ permissions: VIEWER, rules: rules(70), foreign: [] });

    expect(await screen.findByText("Rule000")).toBeInTheDocument();
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 10 of 70 rules");
    expect(screen.queryByText("Rule060")).not.toBeInTheDocument();
  });

  it("reaches the rules past the first page, in the order the server sent them", async () => {
    renderPage({ permissions: VIEWER, rules: rules(70), foreign: [] });
    expect(await screen.findByText("Rule000")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(screen.getByText("Rule010")).toBeInTheDocument();
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 10 of 70 rules");
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 2 of 7");
  });
});
