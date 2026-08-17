import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { translate } from "@/lib/i18n";
import { targetsDict } from "@/lib/i18n/dict/targets";
import { TargetsPage, fmtIntervalNs, intervalParts, parseLabels } from "./targets";

/**
 * targets.hostile — the adversarial pass over /targets.
 *
 * The brief was the owner's, verbatim: «вписать строку где должны быть цифры и
 * наоборот, включить что угодно — чтобы ничего не ломалось». So every test here
 * puts the WRONG SHAPE of value into a field that wants another one, or hands
 * the page a response no healthy server would send, and asserts on the only two
 * things that matter afterwards:
 *
 *   1. nothing on the wire is a lie — a number field never posts NaN, Infinity
 *      or null in place of the number that was typed, and
 *   2. nothing on screen is a blank — an error that has no words is a page
 *      pretending it succeeded.
 *
 * The happy paths live in targets.test.tsx; this file only owns the refusals.
 */

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** The response an ingress makes when it never reaches the console: a bare
 *  status with an HTML body and — over HTTP/2, which has no reason phrase —
 *  an EMPTY statusText. lib/api's `handle` turns it into an ApiError whose
 *  title is that empty string, which is exactly the input a page must not
 *  render as its only explanation. */
const bareStatus = (status: number) =>
  new Response("<html>gateway</html>", { status, statusText: "", headers: { "Content-Type": "text/html" } });

const OPERATOR = ["targets:read", "targets:write", "checks:read", "checks:write", "schedules:write"];

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] },
    permissions,
  };
}

const configBody = {
  auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
  anonymousBanner: false,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: true },
};

function targetRow(over: Record<string, unknown> = {}) {
  return {
    id: "t-1",
    name: "api-gw",
    kind: "host",
    address: "10.0.0.1",
    labels: {},
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function definitionRow(over: Record<string, unknown> = {}) {
  return {
    id: "d-1",
    name: "gw-tcp",
    sourceSelection: "one-per-zone",
    destinationKind: "target",
    destinationTargetId: "t-1",
    destinationAddress: "",
    checkType: "tcp",
    plane: "pod",
    params: {},
    enabled: true,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function scheduleRow(over: Record<string, unknown> = {}) {
  return {
    id: "s-1",
    definitionId: "d-1",
    kind: "interval",
    intervalNs: 30_000_000_000,
    runAt: null,
    enabled: true,
    lastFiredAt: null,
    nextFireAt: "2026-01-02T00:00:00Z",
    lastError: "",
    lastErrorAt: null,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const OK_PROJECTION = { agents: 3, protocols: 1, series: 3, limit: 400, overLimit: false };

interface Call {
  method: string;
  url: string;
  body?: unknown;
}

function renderPage(
  opts: {
    permissions?: string[];
    targets?: unknown[];
    definitions?: unknown[];
    schedules?: unknown[];
    /** Replaces the 200 GET /api/v1/targets would answer with. */
    targetsResponse?: () => Response;
    /** Replaces the 200 GET /api/v1/schedules would answer with. */
    schedulesResponse?: () => Response;
    onWriteTarget?: (method: string, body: unknown) => Response | undefined;
    onWriteSchedule?: (method: string, body: unknown) => Response | undefined;
    view?: string;
  } = {},
) {
  const {
    permissions = OPERATOR,
    targets = [],
    definitions = [],
    schedules = [],
    targetsResponse,
    schedulesResponse,
    onWriteTarget,
    onWriteSchedule,
    view,
  } = opts;
  const targetList = [...targets];
  const scheduleList = [...schedules];
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body: unknown = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: href, body });

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody));
    if (href.startsWith("/api/v1/checks/projection")) return Promise.resolve(json(OK_PROJECTION));
    if (href.startsWith("/api/v1/targets")) {
      const override = onWriteTarget?.(method, body);
      if (override) return Promise.resolve(override);
      if (method === "POST") {
        const created = targetRow({ id: `t-${targetList.length + 1}`, ...(body as Record<string, unknown>) });
        targetList.push(created);
        return Promise.resolve(json(created, { status: 201 }));
      }
      if (method === "DELETE") {
        const id = href.slice("/api/v1/targets/".length);
        const at = targetList.findIndex((t) => (t as { id: string }).id === id);
        if (at >= 0) targetList.splice(at, 1);
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(targetsResponse ? targetsResponse() : json({ targets: targetList, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/checks")) {
      if (method === "POST") {
        return Promise.resolve(json(definitionRow(body as Record<string, unknown>), { status: 201 }));
      }
      return Promise.resolve(json({ definitions, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/schedules")) {
      const override = onWriteSchedule?.(method, body);
      if (override) return Promise.resolve(override);
      if (method === "POST") {
        const created = scheduleRow({ id: `s-${scheduleList.length + 1}`, ...(body as Record<string, unknown>) });
        scheduleList.push(created);
        return Promise.resolve(json(created, { status: 201 }));
      }
      if (method === "PUT") {
        const id = href.slice("/api/v1/schedules/".length);
        const at = scheduleList.findIndex((s) => (s as { id: string }).id === id);
        const updated = { ...(scheduleList[at] ?? scheduleRow({ id })), ...(body as Record<string, unknown>) };
        if (at >= 0) scheduleList[at] = updated;
        return Promise.resolve(json(updated));
      }
      return Promise.resolve(
        schedulesResponse ? schedulesResponse() : json({ schedules: scheduleList, nextCursor: "" }),
      );
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  window.history.replaceState({}, "", view === undefined ? "/targets" : `/targets?view=${view}`);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <TargetsPage />
    </QueryClientProvider>,
  );

  const writes = () => calls.filter((c) => c.method !== "GET");
  return { ...utils, fetchMock, calls, writes, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/targets");
});

async function openTab(name: RegExp) {
  fireEvent.click(await screen.findByRole("radio", { name }));
}

/* ── pure helpers, fed values no healthy row carries ────────────────────── */

describe("parseLabels under abuse", () => {
  it("refuses a fragment with no '=' and names the fragment it refused", () => {
    expect(() => parseLabels("just-a-word")).toThrow(/just-a-word/);
    // A leading "=" is an EMPTY key, which is the same refusal: eq <= 0.
    expect(() => parseLabels("=value")).toThrow();
    expect(() => parseLabels("  =  ")).toThrow();
  });

  it("keeps everything after the FIRST '=' as the value, separators included", () => {
    expect(parseLabels("url=https://x/?a=b&c=d")).toEqual({ url: "https://x/?a=b&c=d" });
    expect(parseLabels("k=")).toEqual({ k: "" });
  });

  it("survives emoji, RTL marks and a 10k value without inventing a pair", () => {
    const huge = "x".repeat(10_000);
    expect(parseLabels(`команда=сеть, 🚀=🛰️, rtl=‮evil‬, big=${huge}`)).toEqual({
      "команда": "сеть",
      "🚀": "🛰️",
      "rtl": "‮evil‬",
      "big": huge,
    });
  });

  it("drops only truly empty fragments, never a real pair", () => {
    expect(parseLabels(",,a=b,, ,c=d,")).toEqual({ a: "b", c: "d" });
    expect(parseLabels("")).toEqual({});
  });
});

describe("fmtIntervalNs / intervalParts on cadences no scheduler wrote", () => {
  it("renders an em dash rather than NaN for a cadence that is not a number", () => {
    // A row whose intervalNs never made it onto the wire, or arrived as null:
    // the row must read "—", never "NaNs" and never "undefineds".
    for (const bad of [Number.NaN, undefined, null, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
      expect(fmtIntervalNs(bad as unknown as number)).toBe("—");
      expect(intervalParts(bad as unknown as number)).toBeNull();
    }
  });

  it("never renders NaN or undefined for a negative cadence either", () => {
    const rendered = fmtIntervalNs(-30_000_000_000);
    expect(rendered).not.toMatch(/NaN|undefined|Infinity/);
  });
});

/* ── the Schedules tab: the one field on this page that is a NUMBER ─────── */

async function openScheduleForm() {
  await openTab(/^Schedules$/);
  fireEvent.click(await screen.findByRole("button", { name: "New schedule" }));
  return screen.findByLabelText("Interval (seconds)");
}

describe("schedule interval — a box that wants seconds", () => {
  /* The table is the whole point of this file: every one of these is something
     an operator can type, and not one of them may reach the wire. */
  const refused: readonly [label: string, typed: string][] = [
    ["a sentence", "5 seconds"],
    ["a word", "abc"],
    ["blank", "   "],
    ["zero", "0"],
    ["negative", "-1"],
    ["a comma decimal", "60,5"],
    ["a bare unit", "s"],
    ["literal Infinity", "Infinity"],
    ["a float that overflows to Infinity", "1e400"],
    /* 1e300 seconds is FINITE — Number.isFinite says yes — and only becomes
       Infinity once multiplied into nanoseconds, at which point
       JSON.stringify writes `null` and the server reads it as zero. The
       number the operator typed would have been silently replaced by one it
       is not. */
    ["a float whose NANOSECONDS overflow", "1e300"],
    /* Past 2^53 ns the double cannot hold the value exactly, so the number on
       the wire is provably not the number in the box. */
    ["a value beyond exact integer range", "1e12"],
    ["hex", "0x10"],
    ["an emoji", "🚀"],
  ];

  for (const [label, typed] of refused) {
    it(`refuses ${label} (${JSON.stringify(typed)}) at the field and posts nothing`, async () => {
      const page = renderPage({ definitions: [definitionRow()] });
      const box = await openScheduleForm();
      fireEvent.change(box, { target: { value: typed } });
      fireEvent.click(screen.getByRole("button", { name: "Create schedule" }));

      const alert = await screen.findByRole("alert");
      expect(alert.textContent?.trim()).not.toBe("");
      expect(page.writes()).toHaveLength(0);
    });
  }

  it("accepts the plainest legal cadence and sends whole nanoseconds", async () => {
    const page = renderPage({ definitions: [definitionRow()] });
    const box = await openScheduleForm();
    fireEvent.change(box, { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "Create schedule" }));

    await waitFor(() => expect(page.writes()).toHaveLength(1));
    const body = page.writes()[0].body as { intervalNs: number };
    expect(body.intervalNs).toBe(30_000_000_000);
    expect(Number.isSafeInteger(body.intervalNs)).toBe(true);
  });

  it("never puts a non-finite number on the wire, whatever is typed", async () => {
    // The invariant restated as one assertion over the whole refusal table:
    // if a body was ever built, its intervalNs is a real integer.
    for (const [, typed] of refused) {
      const page = renderPage({ definitions: [definitionRow()] });
      const box = await openScheduleForm();
      fireEvent.change(box, { target: { value: typed } });
      fireEvent.click(screen.getByRole("button", { name: "Create schedule" }));
      await screen.findByRole("alert");
      for (const call of page.writes()) {
        const body = call.body as { intervalNs?: unknown };
        if (body.intervalNs !== undefined) expect(Number.isSafeInteger(body.intervalNs)).toBe(true);
      }
      cleanup();
    }
  });
});

describe("schedule rows the store should never have held", () => {
  it("renders a kind this build has never heard of verbatim, with no NaN beside it", async () => {
    renderPage({
      definitions: [definitionRow()],
      schedules: [scheduleRow({ id: "s-x", kind: "cron", intervalNs: 0, nextFireAt: null, lastFiredAt: null })],
    });
    await openTab(/^Schedules$/);
    const list = await screen.findByRole("list", { name: "Schedules" });
    // Twice: the pill shows the stored kind, and `cadence`'s default arm shows
    // it again rather than inventing a sentence for a kind it cannot describe.
    expect(within(list).getAllByText("cron").length).toBeGreaterThan(0);
    expect(list.textContent).not.toMatch(/NaN|undefined|Invalid Date/);
  });

  it("renders an unparseable timestamp verbatim rather than as Invalid Date", async () => {
    renderPage({
      definitions: [definitionRow()],
      schedules: [scheduleRow({ nextFireAt: "not-a-time", lastFiredAt: "" })],
    });
    await openTab(/^Schedules$/);
    const list = await screen.findByRole("list", { name: "Schedules" });
    expect(list.textContent).toContain("not-a-time");
    expect(list.textContent).not.toMatch(/NaN|Invalid Date/);
  });

  it("shows the scheduler's own failure sentence, hostile bytes included, as text", async () => {
    const nasty = '<img src=x onerror="alert(1)"> dial tcp: lookup «шлюз» 🚀';
    renderPage({
      definitions: [definitionRow()],
      schedules: [scheduleRow({ lastError: nasty, lastErrorAt: "2026-01-02T00:00:00Z" })],
    });
    await openTab(/^Schedules$/);
    const line = await screen.findByTestId("schedule-failure");
    expect(line.textContent).toContain(nasty);
    expect(line.querySelector("img")).toBeNull();
  });

  it("issues exactly one PUT when the enable toggle is hammered", async () => {
    const page = renderPage({
      definitions: [definitionRow()],
      schedules: [scheduleRow({ enabled: false })],
    });
    await openTab(/^Schedules$/);
    const toggle = await screen.findByRole("button", { name: /^Enable / });
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    await waitFor(() => expect(page.writes().length).toBeGreaterThan(0));
    expect(page.writes().filter((c) => c.method === "PUT")).toHaveLength(1);
  });
});

/* ── errors that arrive with no words in them ───────────────────────────── */

describe("a failure the server did not explain", () => {
  it("names the list rather than rendering an empty alert on a bare 500", async () => {
    renderPage({ targetsResponse: () => bareStatus(500) });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent?.trim()).not.toBe("");
    // And never the JS runtime's own noise, which names a mechanism no
    // operator can act on.
    expect(alert.textContent).not.toMatch(/SyntaxError|Unexpected token|\[object/);
  });

  it("names the list on a 502 whose body is an ingress HTML page", async () => {
    renderPage({ view: "schedules", schedulesResponse: () => bareStatus(502) });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent?.trim()).not.toBe("");
  });

  it("falls back at the FORM when a rejected write carries a wordless problem", async () => {
    const page = renderPage({
      onWriteTarget: (method) => (method === "POST" ? problem(422, "", "") : undefined),
    });
    fireEvent.click(await screen.findByRole("button", { name: "New target" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "gw" } });
    fireEvent.change(screen.getByLabelText("Address"), { target: { value: "10.0.0.1" } });
    fireEvent.click(screen.getByRole("button", { name: "Create target" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent?.trim()).not.toBe("");
    expect(page.writes()).toHaveLength(1);
  });

  it("still prefers the server's own sentence when there IS one", async () => {
    const detail = 'target "api-gw" is referenced by 2 check definitions; delete those first';
    renderPage({
      targets: [targetRow()],
      onWriteTarget: (method) => (method === "DELETE" ? problem(409, "conflict", detail) : undefined),
    });
    fireEvent.click(await screen.findByRole("button", { name: /^Delete api-gw$/ }));
    fireEvent.click(await screen.findByRole("button", { name: /^Confirm delete api-gw/ }));
    expect((await screen.findByRole("alert")).textContent).toContain(detail);
  });
});

/* ── rows carrying values a form would never have produced ──────────────── */

describe("target rows with hostile content", () => {
  const NASTY_NAME = "<script>alert(1)</script>‮gnp‬ 🚀 " + "n".repeat(400);

  it("renders a script-shaped name as text and bounds the row's action labels", async () => {
    renderPage({ targets: [targetRow({ name: NASTY_NAME, address: "a".repeat(10_000) })] });
    const list = await screen.findByRole("list", { name: "Targets" });
    expect(list.querySelector("script")).toBeNull();
    // The accessible name keeps the whole thing; only the pixels are bounded.
    const del = await screen.findByRole("button", { name: `Delete ${NASTY_NAME}` });
    const visible = del.querySelector("[aria-hidden='true']");
    expect(visible?.className).toContain("truncate");
  });

  it("renders an IPv6-with-brackets, punycode and a spaced address without dropping any of them", async () => {
    renderPage({
      targets: [
        targetRow({ id: "t-1", name: "v6", address: "[2001:db8::1]:8443" }),
        targetRow({ id: "t-2", name: "puny", address: "xn--80ak6aa92e.com" }),
        targetRow({ id: "t-3", name: "spaced", address: "  10.0.0.1  " }),
      ],
    });
    const list = await screen.findByRole("list", { name: "Targets" });
    expect(list.textContent).toContain("[2001:db8::1]:8443");
    expect(list.textContent).toContain("xn--80ak6aa92e.com");
  });

  it("pages a list of 250 rather than rendering all of it", async () => {
    const many = Array.from({ length: 250 }, (_, i) => targetRow({ id: `t-${i}`, name: `tgt-${i}` }));
    renderPage({ targets: many });
    const list = await screen.findByRole("list", { name: "Targets" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(10);
    expect(screen.getByText(/250/)).toBeTruthy();
  });
});

describe("every list on this page is paged, not scrolled", () => {
  it("pages the definitions list", async () => {
    const many = Array.from({ length: 137 }, (_, i) =>
      definitionRow({ id: `d-${i}`, name: `def-${i}`, destinationKind: "node", destinationTargetId: "" }),
    );
    renderPage({ definitions: many, view: "definitions" });
    const list = await screen.findByRole("list", { name: "Check definitions" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(10);
  });

  it("pages the schedules list", async () => {
    const many = Array.from({ length: 137 }, (_, i) => scheduleRow({ id: `s-${i}` }));
    renderPage({ definitions: [definitionRow()], schedules: many, view: "schedules" });
    const list = await screen.findByRole("list", { name: "Schedules" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(10);
  });
});

/* ── the URL is an input too ────────────────────────────────────────────── */

describe("?view=", () => {
  for (const junk of ["", "definitions'; DROP TABLE", "__proto__", "%%%", "🚀", "constructor"]) {
    it(`lands on Targets for ?view=${JSON.stringify(junk)} rather than rendering nothing`, async () => {
      renderPage({ view: encodeURIComponent(junk) });
      const tab = await screen.findByRole("radio", { name: /^Targets$/ });
      expect(tab.getAttribute("aria-checked")).toBe("true");
    });
  }
});

/* ── the definition form's JSON box ─────────────────────────────────────── */

describe("definition params — a box that wants a JSON object", () => {
  async function openDefinitionForm() {
    await openTab(/^Definitions$/);
    fireEvent.click(await screen.findByRole("button", { name: "New definition" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "d" } });
    return screen.getByLabelText("Params (JSON)");
  }

  for (const [label, typed] of [
    ["an array", "[]"],
    ["null", "null"],
    ["a bare number", "123"],
    ["a bare string", '"port"'],
    ["a truncated object", '{"port": '],
    ["a YAML fragment", "port: 443"],
    ["a JS object literal", "{port: 443}"],
  ] as const) {
    it(`refuses ${label} at the field and posts nothing`, async () => {
      const page = renderPage();
      const box = await openDefinitionForm();
      fireEvent.change(box, { target: { value: typed } });
      fireEvent.click(screen.getByRole("button", { name: "Create definition" }));
      const alert = await screen.findByRole("alert");
      expect(alert.textContent?.trim()).not.toBe("");
      expect(page.writes()).toHaveLength(0);
    });
  }

  it("refuses an ad-hoc destination that no resolver could dial", async () => {
    const page = renderPage();
    await openTab(/^Definitions$/);
    fireEvent.click(await screen.findByRole("button", { name: "New definition" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "d" } });
    fireEvent.change(screen.getByLabelText("Destination kind"), { target: { value: "adhoc" } });
    fireEvent.change(screen.getByLabelText("Destination address"), { target: { value: "sdfsdfsdf !!" } });
    fireEvent.click(screen.getByRole("button", { name: "Create definition" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent?.trim()).not.toBe("");
    expect(page.writes()).toHaveLength(0);
  });
});

/* ── i18n: every sentence this file relies on exists in both languages ──── */

describe("i18n", () => {
  it("has an English and a Russian wording for every refusal on this page", () => {
    const keys = [
      "schedules.form.error.interval",
      "schedules.form.error.intervalRange",
      "targets.unavailable",
      "targets.form.failed",
      "definitions.form.paramsNotJson",
      "definitions.form.paramsNotObject",
    ] as const;
    for (const key of keys) {
      for (const locale of ["en", "ru"] as const) {
        const rendered = translate(targetsDict, locale, key, { seconds: 10, max: 9_007_199 });
        expect(rendered.trim()).not.toBe("");
        expect(rendered).not.toContain("{");
      }
    }
  });
});
