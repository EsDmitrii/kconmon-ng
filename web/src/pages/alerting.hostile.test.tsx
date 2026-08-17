import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { translate } from "@/lib/i18n";
import { alertingDict } from "@/lib/i18n/dict/alerting";
import { TimeMachineProvider } from "@/lib/timemachine";
import { AlertingPage, formatPromDuration, parsePromDuration } from "./alerting";

/**
 * alerting.hostile — the adversarial pass over /alerting.
 *
 * Same brief as targets.hostile.test.tsx and the same two questions of every
 * case: does a wrong-shaped value ever reach the wire as a different value than
 * the one that was typed, and does a refusal ever arrive with no words in it.
 *
 * The builder is the longest form in the console and every one of its boxes is
 * a free string, so most of this file is about the two that get MEASURED —
 * `for` (a duration) and the numeric params (a threshold) — plus the rows,
 * which render bytes this console never wrote.
 */

const NOW = new Date("2026-08-08T12:00:00Z");

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

const ALERT_EDITOR = ["topology:read", "alerts:read", "alerts:manage", "targets:read"];

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["admin"] },
    permissions,
  };
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
    rulesResponse?: () => Response;
    foreignResponse?: () => Response;
    onWrite?: (method: string, url: string, body: unknown) => Response | undefined;
    preview?: (body: unknown) => Response;
    importReport?: unknown;
    rule?: string;
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
    rule,
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
    if (href.startsWith("/api/v1/alert-rules/foreign")) {
      return Promise.resolve(foreignResponse ? foreignResponse() : json({ foreign }));
    }
    if (href.startsWith("/api/v1/alert-rules/preview")) {
      return Promise.resolve(preview ? preview(body) : json({ expr: "rendered_expr > 1", series: 3 }));
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
    if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets: [], nextCursor: "" }));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  const params = new URLSearchParams();
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

  const writes = () => calls.filter((c) => c.method !== "GET" && !c.url.includes("/preview"));
  return { ...utils, fetchMock, calls, writes, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
});

/* ── the duration box ───────────────────────────────────────────────────── */

describe("parsePromDuration under abuse", () => {
  it("refuses prose, punctuation and a signed value", () => {
    for (const bad of ["5 seconds", "five minutes", "-5m", "+5m", "5.5m", "5,5m", "m5", "🚀", "5 m"]) {
      expect(parsePromDuration(bad).ok, bad).toBe(false);
    }
  });

  it("refuses a digit run whose value overflows to Infinity", () => {
    /* The one that mattered: Number("111…1") is Infinity, ns became Infinity,
       the parse said OK, and JSON.stringify wrote `null` — which the server
       reads back as ZERO. The rule would have been saved with a `for` of 0s
       and nothing on screen would ever have said so. */
    const parsed = parsePromDuration("1".repeat(400) + "y");
    expect(parsed.ok).toBe(false);
    if (!parsed.ok) expect(parsed.message.trim()).not.toBe("");
  });

  it("refuses a duration longer than the int64 nanoseconds behind it", () => {
    // Go's time.Duration tops out a shade under 292 years.
    expect(parsePromDuration("293y").ok).toBe(false);
    expect(parsePromDuration("9999999w").ok).toBe(false);
    expect(parsePromDuration("292y").ok).toBe(true);
  });

  it("never answers ok with a nanosecond count that is not a real number", () => {
    const inputs = ["1".repeat(400) + "y", "293y", "99999999999999999999999d", "0s", "5m", "1h30m", ""];
    for (const text of inputs) {
      const parsed = parsePromDuration(text);
      if (parsed.ok) {
        expect(Number.isFinite(parsed.ns), text).toBe(true);
        expect(parsed.ns >= 0, text).toBe(true);
        expect(JSON.parse(JSON.stringify({ ns: parsed.ns })).ns, text).toBe(parsed.ns);
      }
    }
  });
});

describe("formatPromDuration on values no rule should carry", () => {
  it("renders an em dash rather than the words NaN or undefined", () => {
    for (const bad of [Number.NaN, undefined, null, Number.POSITIVE_INFINITY]) {
      expect(formatPromDuration(bad as unknown as number)).toBe("—");
    }
  });

  it("still mirrors render.go for every value a rule really carries", () => {
    expect(formatPromDuration(0)).toBe("0s");
    expect(formatPromDuration(300_000_000_000)).toBe("5m");
    expect(formatPromDuration(500_000_000)).toBe("500ms");
  });
});

/* ── the builder ────────────────────────────────────────────────────────── */

async function openBuilder() {
  fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
  return screen.findByRole("form", { name: /New alert rule|Create/ }).catch(() => null);
}

describe("the `for` box", () => {
  it("says what is wrong beside the box and refuses to save", async () => {
    const page = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "R" } });
    fireEvent.change(screen.getByLabelText(/^For/), { target: { value: "5 seconds" } });
    expect((await screen.findByTestId("field-error-for")).textContent?.trim()).not.toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() => expect(screen.getByTestId("field-error-for")).toBeTruthy());
    expect(page.writes()).toHaveLength(0);
  });

  it("refuses an overflowing duration rather than quietly saving zero", async () => {
    const page = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "R" } });
    fireEvent.change(screen.getByLabelText(/^For/), { target: { value: "1".repeat(400) + "y" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() => expect(screen.getByTestId("field-error-for")).toBeTruthy());
    expect(page.writes()).toHaveLength(0);
  });
});

describe("a numeric param fed a string", () => {
  it("sends the raw text rather than NaN, and renders the server's refusal at the field", async () => {
    const page = renderPage({
      onWrite: (method) =>
        method === "POST"
          ? problem(422, "unprocessable", 'alert rule: param "thresholdPercent" must be a number')
          : undefined,
    });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "R" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "tcp" } });
    // 1e400 parses as a finite-looking string and coerces to Infinity, so it
    // must travel as the TEXT it is and let the server name it.
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "1e400" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));

    await waitFor(() => expect(page.writes().length).toBeGreaterThan(0));
    const body = page.writes()[0].body as { params: Record<string, unknown> };
    expect(Number.isNaN(body.params.thresholdPercent as number)).toBe(false);
    expect(JSON.stringify(body)).not.toContain("null");
    expect((await screen.findByTestId("field-error-thresholdPercent")).textContent).toContain("thresholdPercent");
  });
});

describe("label pairs", () => {
  it("refuses two rows with the same key rather than silently keeping the last", async () => {
    const page = renderPage();
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "R" } });
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.change(screen.getByLabelText("Label name 1"), { target: { value: "team" } });
    fireEvent.change(screen.getByLabelText("Label value 1"), { target: { value: "net" } });
    fireEvent.change(screen.getByLabelText("Label name 2"), { target: { value: "team" } });
    fireEvent.change(screen.getByLabelText("Label value 2"), { target: { value: "sre" } });

    const notice = await screen.findByTestId("builder-error");
    expect(notice.textContent).toContain("team");
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() => expect(screen.getByTestId("builder-error")).toBeTruthy());
    expect(page.writes()).toHaveLength(0);
  });

  it("still refuses a reserved label name", async () => {
    renderPage();
    await openBuilder();
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.change(screen.getByLabelText("Label name 1"), { target: { value: "severity" } });
    expect((await screen.findByTestId("builder-error")).textContent).toContain("reserved");
  });
});

/* ── rows carrying bytes this console never wrote ───────────────────────── */

describe("rule rows with hostile content", () => {
  const NASTY = "<script>alert(1)</script> ‮eman‬ 🚀 " + "r".repeat(400);

  it("renders a script-shaped rule name as text and bounds every row action", async () => {
    renderPage({ rules: [ruleRow({ name: NASTY })] });
    const list = await screen.findByRole("list", { name: "Alert rules" });
    expect(list.querySelector("script")).toBeNull();
    for (const accessibleName of [
      `Details for ${NASTY}`,
      `Sync ${NASTY} now`,
      `Edit ${NASTY}`,
      `Delete ${NASTY}`,
    ]) {
      const button = await screen.findByRole("button", { name: accessibleName });
      const visible = button.querySelector("[aria-hidden='true']");
      expect(visible?.className, accessibleName.slice(0, 12)).toContain("truncate");
    }
  });

  it("renders a sync status this build has never heard of as itself", async () => {
    renderPage({ rules: [ruleRow({ syncStatus: "reconciling", syncMessage: "" })] });
    const chip = await screen.findByTestId("sync-status");
    expect(chip.textContent).toBe("reconciling");
  });

  it("renders every real sync state with words and never a blank pill", async () => {
    renderPage({
      rules: [
        ruleRow({ id: "a", name: "A", syncStatus: "synced" }),
        ruleRow({ id: "b", name: "B", syncStatus: "error", syncMessage: "PrometheusRule apply failed: forbidden" }),
        ruleRow({ id: "c", name: "C", syncStatus: "unsynced", lastSyncedAt: undefined }),
        ruleRow({ id: "d", name: "D", syncStatus: "drift" }),
      ],
    });
    await screen.findByRole("list", { name: "Alert rules" });
    for (const chip of screen.getAllByTestId("sync-status")) {
      expect(chip.textContent?.trim()).not.toBe("");
    }
    // A rule that has never been applied shows an em dash, not "NaN" or "Invalid Date".
    expect(screen.getAllByTestId("last-synced").map((n) => n.textContent)).toContain("—");
  });

  it("renders a rule whose `for` never arrived without printing NaN", async () => {
    renderPage({ rules: [ruleRow({ forNs: Number.NaN as unknown as number })] });
    fireEvent.click(await screen.findByRole("button", { name: /^Details for PairLossHigh/ }));
    const row = await screen.findByTestId("rule-row");
    expect(row.textContent).not.toMatch(/NaN|undefined/);
  });

  it("opens a rule whose kind this build has no template for, with no param boxes", async () => {
    /* The alert_rules CHECK constraint allows a kind the API's enum does not,
       so a row written by another build opens here. It must say so rather than
       take the page down on a missing schema entry. */
    renderPage({ rules: [ruleRow({ kind: "cert-expiry", params: { days: 7 } })] });
    fireEvent.click(await screen.findByRole("button", { name: /^Edit PairLossHigh/ }));
    const notice = await screen.findByTestId("unknown-kind");
    expect(notice.textContent).toContain("cert-expiry");
  });

  it("pages hundreds of rules rather than rendering all of them", async () => {
    const many = Array.from({ length: 300 }, (_, i) => ruleRow({ id: `r-${i}`, name: `Rule${i}` }));
    renderPage({ rules: many });
    const list = await screen.findByRole("list", { name: "Alert rules" });
    expect(within(list).getAllByTestId("rule-row")).toHaveLength(10);
  });
});

/* ── lifecycle, including the last rule there is ────────────────────────── */

describe("create → disable → delete, down to zero", () => {
  it("deletes the LAST rule once, then states the empty list as a settled answer", async () => {
    const page = renderPage({ rules: [ruleRow()] });
    fireEvent.click(await screen.findByRole("button", { name: /^Delete PairLossHigh/ }));
    const confirm = await screen.findByRole("button", { name: /^Confirm delete PairLossHigh/ });
    fireEvent.click(confirm);
    fireEvent.click(confirm);

    await screen.findByText(/No rules yet/i);
    expect(page.writes().filter((c) => c.method === "DELETE")).toHaveLength(1);
    expect(screen.queryAllByTestId("rule-row")).toHaveLength(0);
  });

  it("keeps the row and says why when the delete is refused", async () => {
    const detail = "alert rule is referenced by a PrometheusRule this console does not own";
    renderPage({
      rules: [ruleRow()],
      onWrite: (method) => (method === "DELETE" ? problem(409, "conflict", detail) : undefined),
    });
    fireEvent.click(await screen.findByRole("button", { name: /^Delete PairLossHigh/ }));
    fireEvent.click(await screen.findByRole("button", { name: /^Confirm delete PairLossHigh/ }));
    expect((await screen.findByRole("alert")).textContent).toContain(detail);
    expect(screen.getAllByTestId("rule-row")).toHaveLength(1);
  });

  it("issues one PUT when the enable checkbox is hammered", async () => {
    const page = renderPage({ rules: [ruleRow({ enabled: true })] });
    const box = await screen.findByRole("checkbox", { name: /^Enabled/ });
    fireEvent.click(box);
    fireEvent.click(box);
    fireEvent.click(box);
    await waitFor(() => expect(page.writes().length).toBeGreaterThan(0));
    expect(page.writes().filter((c) => c.method === "PUT")).toHaveLength(1);
  });
});

/* ── errors with no words in them ───────────────────────────────────────── */

describe("a failure the server did not explain", () => {
  it("never renders an empty alert for a wordless problem on the list", async () => {
    renderPage({ rulesResponse: () => problem(500, "", "") });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent?.trim()).not.toBe("");
  });

  it("never renders an empty alert for a wordless problem on a rejected save", async () => {
    renderPage({ onWrite: (method) => (method === "POST" ? problem(422, "", "") : undefined) });
    await openBuilder();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "R" } });
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "tcp" } });
    fireEvent.change(screen.getByLabelText("Loss threshold (%)"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
    const banner = await screen.findByTestId("builder-error");
    expect(banner.textContent?.trim()).not.toBe("");
  });

  it("never renders an empty alert for a wordless refusal of a row delete", async () => {
    renderPage({
      rules: [ruleRow()],
      onWrite: (method) => (method === "DELETE" ? problem(500, "", "") : undefined),
    });
    fireEvent.click(await screen.findByRole("button", { name: /^Delete PairLossHigh/ }));
    fireEvent.click(await screen.findByRole("button", { name: /^Confirm delete PairLossHigh/ }));
    expect((await screen.findByRole("alert")).textContent?.trim()).not.toBe("");
  });
});

/* ── the foreign list, which is whatever the cluster happens to hold ────── */

describe("foreign PrometheusRules", () => {
  it("renders two objects that share a name as two rows", async () => {
    renderPage({
      foreign: [
        foreignRow({ name: "recording-rules", groups: 1, rules: 1, managedBy: "" }),
        foreignRow({ name: "recording-rules", groups: 9, rules: 90, managedBy: "flux" }),
      ],
    });
    const list = await screen.findByRole("list", { name: "Foreign PrometheusRule objects" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    // The one with no owner reads as an em dash rather than as a blank cell.
    expect(within(list).getAllByTestId("managed-by").map((n) => n.textContent)).toContain("—");
  });

  it("renders a script-shaped object name as text and bounds its import button", async () => {
    const nasty = "<img src=x onerror=alert(1)> " + "f".repeat(300);
    renderPage({ foreign: [foreignRow({ name: nasty })] });
    const list = await screen.findByRole("list", { name: "Foreign PrometheusRule objects" });
    expect(list.querySelector("img")).toBeNull();
    const button = await screen.findByRole("button", { name: `Import ${nasty}` });
    expect(button.querySelector("[aria-hidden='true']")?.className).toContain("truncate");
  });

  it("renders an import report whose three arrays arrived as null", async () => {
    /* The API contract says all three are present; Go marshals a nil slice as
       `null`, so ONE server-side `var xs []string` is the whole distance
       between this report and a white screen. */
    renderPage({ foreign: [foreignRow()], importReport: { created: null, skipped: null, notes: null } });
    fireEvent.click(await screen.findByRole("button", { name: /^Import kube-prometheus-rules/ }));
    const report = await screen.findByTestId("import-report");
    expect(report.textContent).toBeTruthy();
    for (const testId of ["import-created", "import-skipped", "import-notes"]) {
      expect(within(screen.getByTestId(testId)).getByText("none")).toBeTruthy();
    }
  });

  it("says why when the import itself is refused, and shows no report", async () => {
    const detail = "PrometheusRule \"weird\" holds no expression this console can adopt";
    renderPage({
      foreign: [foreignRow()],
      onWrite: (method, url) => (url.includes("/import") && method === "POST" ? problem(422, "unprocessable", detail) : undefined),
    });
    fireEvent.click(await screen.findByRole("button", { name: /^Import kube-prometheus-rules/ }));
    expect((await screen.findByRole("alert")).textContent).toContain(detail);
    expect(screen.queryByTestId("import-report")).toBeNull();
  });

  it("renders a report with two hundred skipped entries without losing the headings", async () => {
    renderPage({
      foreign: [foreignRow()],
      importReport: {
        created: [],
        skipped: Array.from({ length: 200 }, (_, i) => ({ name: `r-${i}`, reason: "not a kconmon-ng metric" })),
        notes: [],
      },
    });
    fireEvent.click(await screen.findByRole("button", { name: /^Import kube-prometheus-rules/ }));
    const skipped = await screen.findByTestId("import-skipped");
    expect(within(skipped).getAllByRole("term")).toHaveLength(200);
    expect(within(screen.getByTestId("import-created")).getByText("none")).toBeTruthy();
  });

  it("renders an import report whose entries carry no name", async () => {
    renderPage({
      foreign: [foreignRow()],
      importReport: {
        created: [],
        skipped: [{ name: "", reason: "expression is not a kconmon-ng metric" }],
        notes: [{ name: "x", note: "severity was defaulted to warning" }],
      },
    });
    fireEvent.click(await screen.findByRole("button", { name: /^Import kube-prometheus-rules/ }));
    const skipped = await screen.findByTestId("import-skipped");
    expect(skipped.textContent).toContain("(unnamed entry)");
  });
});

/* ── the URL is an input too ────────────────────────────────────────────── */

describe("?rule=", () => {
  for (const junk of ["not-a-uuid", "__proto__", "%%%", "🚀", "'; DROP TABLE"]) {
    it(`says the link names nothing for ?rule=${JSON.stringify(junk)}`, async () => {
      renderPage({ rules: [ruleRow()], rule: junk });
      expect((await screen.findByTestId("unknown-rule-notice")).textContent?.trim()).not.toBe("");
      expect(screen.getAllByTestId("rule-row")).toHaveLength(1);
    });
  }
});

/* ── i18n ───────────────────────────────────────────────────────────────── */

describe("i18n", () => {
  it("has an English and a Russian wording for every refusal this file relies on", () => {
    const keys = ["duration.tooLong", "pairs.duplicate", "rules.unavailable", "form.failed", "row.deleteFailed"] as const;
    for (const key of keys) {
      for (const locale of ["en", "ru"] as const) {
        const rendered = translate(alertingDict, locale, key, { text: "9999y", name: "team" });
        expect(rendered.trim(), `${key}/${locale}`).not.toBe("");
        expect(rendered, `${key}/${locale}`).not.toContain("{");
      }
    }
  });

  it("words both duration refusals differently enough to tell them apart", () => {
    const bad = parsePromDuration("nonsense");
    const long = parsePromDuration("9999y");
    expect(bad.ok).toBe(false);
    expect(long.ok).toBe(false);
    if (!bad.ok && !long.ok) expect(bad.message).not.toBe(long.message);
  });
});

/* NOW is referenced so the constant does not drift out of the file unnoticed. */
void NOW;
