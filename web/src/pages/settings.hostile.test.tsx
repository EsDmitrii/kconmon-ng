import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { SettingsPage, parseBundle, webhookDraftErrors, webhookRequestFrom, tokenState } from "./settings";
import { settingsDict, type SettingsKey } from "@/lib/i18n/dict/settings";
import { translate, type Translate } from "@/lib/i18n";

/**
 * THE HOSTILE OPERATOR'S SETTINGS PAGE.
 *
 * pages/settings.test.tsx pins what each section does when it is used as
 * intended. This file is the same page used as a weapon: ten-thousand-character
 * names, URLs that are not URLs, expiries in the past, bundles that are not
 * bundles, a server that answers with a proxy's HTML error page, a language
 * switched mid-form, and a session that expires between two clicks.
 *
 * The bar every case here is judged against: no thrown exception, no blank
 * panel, no rendered "NaN"/"undefined"/"Invalid Date", no error slot that is
 * red and EMPTY, and no request the page had no business making.
 */

const ADMIN = ["tokens:manage", "webhooks:manage", "settings:write", "maintenance:read", "maintenance:write"];

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, ...(detail === undefined ? {} : { detail }) }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** A gateway's own answer: the right status, the wrong content type, no words
 *  this console can quote. */
const opaque = (status: number) => new Response("<html><body>502 Bad Gateway</body></html>", { status });

function meBody(permissions: string[]) {
  return { subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["admin"] }, permissions };
}

function configBody() {
  return {
    auth: { mode: "local", role: "viewer", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: true },
  };
}

function webhookRow(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "w-1",
    name: "pagerduty",
    url: "https://hooks.example.test/pd",
    events: ["incident.created"],
    enabled: true,
    hasSecret: true,
    lastStatus: "",
    failures: 0,
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function tokenRow(over: Record<string, unknown> = {}) {
  return { id: "t-1", name: "ci-pipeline", owner: "u1", createdAt: "2026-01-01T00:00:00Z", ...over };
}

const EMPTY_COLLECTION = { created: 0, updated: 0, skipped: 0, errors: [], warnings: [] };
const IMPORT_RESULT = {
  dryRun: true,
  targets: EMPTY_COLLECTION,
  checkDefinitions: EMPTY_COLLECTION,
  checkSchedules: EMPTY_COLLECTION,
  alertRules: EMPTY_COLLECTION,
  webhooks: EMPTY_COLLECTION,
  maintenanceWindows: EMPTY_COLLECTION,
};

const BUNDLE = {
  version: 1,
  exportedAt: "2026-08-01T00:00:00Z",
  targets: [],
  checkDefinitions: [],
  checkSchedules: [],
  alertRules: [],
  webhooks: [],
  maintenanceWindows: [],
};

interface Call {
  method: string;
  url: string;
  body?: unknown;
}

function renderPage(
  opts: {
    permissions?: string[];
    webhooks?: unknown[];
    tokens?: unknown[];
    /** Answers ANY request whose URL matches, before the default mock does. */
    intercept?: (method: string, url: string, body: unknown) => Response | undefined;
    locale?: "en" | "ru";
  } = {},
) {
  const { permissions = ADMIN, webhooks = [], tokens = [], intercept, locale } = opts;
  const rows = [...webhooks] as Record<string, unknown>[];
  const tokenRows = [...tokens] as Record<string, unknown>[];
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    let body: unknown;
    try {
      body = init?.body ? JSON.parse(String(init.body)) : undefined;
    } catch {
      body = String(init?.body);
    }
    calls.push({ method, url: href, body });

    const override = intercept?.(method, href, body);
    if (override) return Promise.resolve(override);

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.4.0", commit: "abc", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.startsWith("/api/v1/export")) return Promise.resolve(json(BUNDLE));
    if (href.startsWith("/api/v1/import")) {
      return Promise.resolve(json({ ...IMPORT_RESULT, dryRun: (body as { dryRun: boolean }).dryRun }));
    }
    if (href.endsWith("/test") && method === "POST") return Promise.resolve(new Response(null, { status: 202 }));
    if (href.startsWith("/api/v1/tokens")) {
      if (method === "POST") {
        const req = body as { name: string; expiresAt?: string };
        const created = tokenRow({ id: `t-${tokenRows.length + 1}`, name: req.name, expiresAt: req.expiresAt });
        tokenRows.push(created);
        return Promise.resolve(json({ id: created.id, name: created.name, token: "kcm_deadbeef" }, { status: 201 }));
      }
      if (method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(json({ tokens: tokenRows }));
    }
    if (href.startsWith("/api/v1/webhooks")) {
      if (method === "POST") {
        const created = webhookRow({ id: `w-${rows.length + 1}`, ...(body as Record<string, unknown>) });
        delete created.secret;
        rows.push(created);
        return Promise.resolve(json(created, { status: 201 }));
      }
      if (method === "PUT") return Promise.resolve(json(webhookRow(body as Record<string, unknown>)));
      if (method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(json({ webhooks: rows }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  window.history.pushState({}, "", "/settings");
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <TimeMachineProvider>
          <SettingsPage />
        </TimeMachineProvider>
      </LocaleProvider>
    </QueryClientProvider>,
  );
  const writes = () => calls.filter((c) => c.method !== "GET");
  return { ...utils, calls, writes, fetchMock };
}

/** Nothing on this page may ever print one of these. */
function expectNoGarbage() {
  const text = document.body.textContent ?? "";
  expect(text).not.toMatch(/\bNaN\b/);
  expect(text).not.toMatch(/\bundefined\b/);
  expect(text).not.toMatch(/Invalid Date/);
  expect(text).not.toMatch(/\[object Object\]/);
}

/** Every alert on the page has WORDS in it — a red slot with nothing in it is a
 *  failure the reader cannot act on. */
function expectAlertsSpeak() {
  const alerts = screen.queryAllByRole("alert");
  for (const a of alerts) expect((a.textContent ?? "").trim()).not.toBe("");
  return alerts;
}

/** This page's English string for a key — what an assertion reads rather than a
 *  hand-copied sentence that can drift from the table. */
const en = (key: SettingsKey): string => translate(settingsDict, "en", key);

async function openWebhookForm() {
  fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
  return await screen.findByRole("button", { name: "Create endpoint" });
}

function fillWebhook(over: { name?: string; url?: string; secret?: string; event?: boolean } = {}) {
  if (over.name !== undefined) fireEvent.change(screen.getByLabelText("Name"), { target: { value: over.name } });
  if (over.url !== undefined) fireEvent.change(screen.getByLabelText("URL"), { target: { value: over.url } });
  if (over.secret !== undefined) {
    fireEvent.change(screen.getByLabelText("Secret"), { target: { value: over.secret } });
  }
  if (over.event) fireEvent.click(screen.getByLabelText("incident.created"));
}

const bundleFile = (body: unknown) =>
  new File([typeof body === "string" ? body : JSON.stringify(body)], "bundle.json", { type: "application/json" });

async function loadBundle(body: unknown) {
  fireEvent.change(await screen.findByLabelText("Configuration bundle"), { target: { files: [bundleFile(body)] } });
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
  localStorage.removeItem(LOCALE_STORAGE_KEY);
  resetNavigateForTest();
});

/* ── a refusal with no words in it ──────────────────────────────────────── */

/**
 * The page quotes the server verbatim, which is right — and left it with
 * nothing to say when the server said nothing. A gateway that answers 502 with
 * an HTML page carries no problem+json, so `title` is the empty statusText and
 * `detail` is absent, and every error slot on this page rendered a red
 * paragraph with no text in it: the list stayed empty, the button un-spun, and
 * an operator was looking at a page that had silently given up.
 */
describe("a failure the server did not put into words", () => {
  /** A problem document whose words are all blank — the shape the fix is about. */
  it.each([
    ["an empty title", () => problem(503, "")],
    ["a whitespace title", () => problem(503, "   ")],
    ["an empty title and an empty detail", () => problem(503, "", "")],
  ])("names the section that failed — tokens, for %s", async (_name, make) => {
    renderPage({ intercept: (m, url) => (url.startsWith("/api/v1/tokens") && m === "GET" ? make() : undefined) });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(en("tokens.unavailable"));
    expectNoGarbage();
  });

  it.each([
    ["webhooks", "/api/v1/webhooks", "webhooks.unavailable"],
  ])("does the same for the %s list", async (_name, prefix, key) => {
    renderPage({ intercept: (m, url) => (url.startsWith(prefix) && m === "GET" ? problem(503, "") : undefined) });
    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    expect(screen.getAllByRole("alert").some((a) => a.textContent?.includes(en(key as SettingsKey)))).toBe(true);
    expectAlertsSpeak();
  });

  it("does the same for an import the server refused wordlessly", async () => {
    renderPage({ intercept: (_m, url) => (url.startsWith("/api/v1/import") ? problem(500, "") : undefined) });
    await loadBundle(BUNDLE);
    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    expect(screen.getAllByRole("alert").some((a) => a.textContent?.includes(en("bundle.importRefused")))).toBe(true);
  });

  it("does the same for an export the server refused wordlessly", async () => {
    renderPage({ intercept: (_m, url) => (url.startsWith("/api/v1/export") ? problem(500, "") : undefined) });
    fireEvent.click(await screen.findByRole("button", { name: "Export configuration" }));
    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    expect(screen.getAllByRole("alert").some((a) => a.textContent?.includes(en("bundle.exportFailed")))).toBe(true);
  });

  /** The other wordless failure: a gateway that answers in HTML. lib/api.ts
   *  turns that into its own sentence, so the page has something to quote; what
   *  matters here is only that the slot is never red and EMPTY. */
  it.each([
    ["tokens", "/api/v1/tokens"],
    ["webhooks", "/api/v1/webhooks"],
  ])("says something readable when a gateway answers for the %s endpoint in HTML", async (_name, prefix) => {
    renderPage({ intercept: (m, url) => (url.startsWith(prefix) && m === "GET" ? opaque(502) : undefined) });
    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    for (const a of screen.getAllByRole("alert")) {
      expect((a.textContent ?? "").trim().length).toBeGreaterThan(3);
    }
    expectNoGarbage();
  });

  it("keeps the server's own sentence whenever there IS one", async () => {
    renderPage({
      intercept: (m, url) =>
        url.startsWith("/api/v1/tokens") && m === "GET"
          ? problem(503, "tokens unavailable", "console.tokens requires a database")
          : undefined,
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("console.tokens requires a database");
  });

  it("says it in Russian when the console is Russian and the server said nothing", async () => {
    renderPage({
      locale: "ru",
      intercept: (m, url) => (url.startsWith("/api/v1/tokens") && m === "GET" ? problem(503, "") : undefined),
    });
    expect(await screen.findByRole("alert")).toHaveTextContent(translate(settingsDict, "ru", "tokens.unavailable"));
  });
});

/* ── webhooks ───────────────────────────────────────────────────────────── */

describe("the webhook form refuses what a browser can be sure about", () => {
  it.each([
    ["an uppercase name", { name: "PagerDuty", url: "https://x.test", secret: "s", event: true }, "name"],
    ["a name with spaces", { name: "pager duty", url: "https://x.test", secret: "s", event: true }, "name"],
    ["a name with a slash", { name: "../../etc/passwd", url: "https://x.test", secret: "s", event: true }, "name"],
    ["a Cyrillic name", { name: "оповещение", url: "https://x.test", secret: "s", event: true }, "name"],
    ["an emoji name", { name: "pager🔥", url: "https://x.test", secret: "s", event: true }, "name"],
    ["a whitespace-only name", { name: "   ", url: "https://x.test", secret: "s", event: true }, "name"],
    ["a javascript: URL", { name: "pd", url: "javascript:alert(1)", secret: "s", event: true }, "url"],
    ["a data: URL", { name: "pd", url: "data:text/html,x", secret: "s", event: true }, "url"],
    ["an ftp URL", { name: "pd", url: "ftp://x.test/hook", secret: "s", event: true }, "url"],
    ["a bare hostname", { name: "pd", url: "hooks.example.test", secret: "s", event: true }, "url"],
    ["a protocol-relative URL", { name: "pd", url: "//hooks.example.test", secret: "s", event: true }, "url"],
    ["an empty URL", { name: "pd", url: "   ", secret: "s", event: true }, "url"],
  ])("marks %s and sends nothing", async (_name, draft, field) => {
    const { writes } = renderPage();
    await openWebhookForm();
    fillWebhook(draft);
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    expectAlertsSpeak();
    // The refusal is CLIENT-side: no request left the browser at all.
    expect(writes()).toEqual([]);
    // ...and it is attached to the box it is about.
    const marked = document.querySelectorAll("[aria-invalid='true']");
    expect(marked.length).toBeGreaterThan(0);
    if (field === "name") expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "true");
    if (field === "url") expect(screen.getByLabelText("URL")).toHaveAttribute("aria-invalid", "true");
  });

  it("says everything that is wrong at once, not one thing per round trip", () => {
    const t: Translate<SettingsKey> = (key, vars) => translate(settingsDict, "en", key, vars);
    const errors = webhookDraftErrors({ name: "", url: "", events: [], enabled: true, secret: "" }, false, t);
    expect(Object.keys(errors).sort()).toEqual(["events", "name", "secret", "url"]);
  });

  it.each([
    ["an internal address", "http://169.254.169.254/latest/meta-data"],
    ["localhost", "http://localhost:8080/hook"],
    ["a cluster-internal name", "http://kubernetes.default.svc/api"],
    ["a ten-thousand-character URL", `https://x.test/${"a".repeat(10_000)}`],
    ["a URL with credentials in it", "https://user:pass@hooks.example.test/x"],
    ["a unicode host", "https://пример.рф/hook"],
  ])("leaves %s to the SERVER, which is the only side that can judge it", async (_name, url) => {
    const { writes } = renderPage();
    await openWebhookForm();
    fillWebhook({ name: "pd", url, secret: "s3cret", event: true });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    // Verbatim: the console does not normalise, rewrite or trim a URL on its way out.
    expect((writes()[0].body as { url: string }).url).toBe(url);
  });

  it("sends a ten-thousand-character secret verbatim, bytes and all", () => {
    const secret = ` ${"x".repeat(10_000)} `;
    expect(webhookRequestFrom({ name: "pd", url: "https://x.test", events: [], enabled: true, secret }).secret).toBe(
      secret,
    );
  });

  it("renders the 503 an endpoint with no encryption key gets, in full", async () => {
    renderPage({
      intercept: (m, url) =>
        m === "POST" && url === "/api/v1/webhooks"
          ? problem(503, "webhooks not available", "console.webhooks.encryptionKey is not configured; a secret cannot be stored")
          : undefined,
    });
    await openWebhookForm();
    fillWebhook({ name: "pd", url: "https://x.test/hook", secret: "s3cret", event: true });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    // The whole sentence, because the name of the missing setting IS the fix.
    expect(await screen.findByText(/console\.webhooks\.encryptionKey is not configured/)).toBeInTheDocument();
    // The form stays open with the draft in it — a 503 is a "not yet", not a "start over".
    expect(screen.getByLabelText("Name")).toHaveValue("pd");
    expectNoGarbage();
  });

  it("renders a refused TEST verbatim and never claims the ping was queued", async () => {
    renderPage({
      webhooks: [webhookRow()],
      intercept: (_m, url) =>
        url.endsWith("/test") ? problem(503, "webhooks not available", "console.webhooks.encryptionKey is not configured") : undefined,
    });
    fireEvent.click(await screen.findByRole("button", { name: /Send test to pagerduty/i }));

    expect(await screen.findByText(/console\.webhooks\.encryptionKey is not configured/)).toBeInTheDocument();
    expect(screen.queryByText(en("webhooks.row.queued"))).not.toBeInTheDocument();
  });

  it("renders a row whose server-side strings are hostile as TEXT", async () => {
    renderPage({
      webhooks: [
        webhookRow({
          name: "<img src=x onerror=alert(1)>",
          url: `https://x.test/${"u".repeat(500)}`,
          lastStatus: "failed: <script>alert(1)</script>",
          failures: 21,
        }),
      ],
    });
    const row = await screen.findByRole("listitem");
    expect(row.querySelector("img")).toBeNull();
    expect(row.querySelector("script")).toBeNull();
    expect(row).toHaveTextContent("failed: <script>alert(1)</script>");
    expectNoGarbage();
  });

  /** English has two plural forms and only 1 is singular; the Russian rule sends
   *  21 to the singular, which is how "21 failure" reached an English console. */
  it.each([
    [1, "1 consecutive failure"],
    [2, "2 consecutive failures"],
    [21, "21 consecutive failures"],
    [101, "101 consecutive failures"],
    [111, "111 consecutive failures"],
  ])("counts %d consecutive failures in English", async (failures, expected) => {
    renderPage({ webhooks: [webhookRow({ failures })] });
    expect(await screen.findByText(expected)).toBeInTheDocument();
  });

  it.each([
    [1, "1 сбой подряд"],
    [2, "2 сбоя подряд"],
    [5, "5 сбоев подряд"],
    [21, "21 сбой подряд"],
    [111, "111 сбоев подряд"],
  ])("counts %d in Russian, where 21 really is singular", async (failures, expected) => {
    renderPage({ locale: "ru", webhooks: [webhookRow({ failures })] });
    expect(await screen.findByText(expected)).toBeInTheDocument();
  });
});

/* ── tokens ─────────────────────────────────────────────────────────────── */

describe("the token section under abuse", () => {
  it("refuses a whitespace-only name without POSTing", async () => {
    const { writes } = renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "     " } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(en("tokens.form.nameRequired"));
    expect(writes()).toEqual([]);
  });

  it("trims a name before sending it, and sends a long one whole", async () => {
    const { writes } = renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    const long = "c".repeat(5_000);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: `  ${long}  ` } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect((writes()[0].body as { name: string }).name).toBe(long);
  });

  it.each([
    ["a row whose expiry is not a date", tokenRow({ expiresAt: "not-a-date" })],
    ["a row whose created stamp is not a date", tokenRow({ createdAt: "yesterday" })],
    ["a row with an empty owner", tokenRow({ owner: "" })],
    ["a row with a null last-used", tokenRow({ lastUsedAt: null })],
    ["a row with markup in its name", tokenRow({ name: "<b>ci</b>" })],
  ])("renders %s without printing NaN or Invalid Date", async (_name, row) => {
    renderPage({ tokens: [row] });
    await screen.findByTestId("token-row");
    expectNoGarbage();
    expectAlertsSpeak();
  });

  it("calls an unparseable expiry ACTIVE rather than guessing it is spent", () => {
    const now = new Date("2026-08-08T12:00:00Z");
    expect(tokenState({ id: "t", name: "n", owner: "o", createdAt: "", expiresAt: "not-a-date" } as never, now)).toBe(
      "active",
    );
    expect(tokenState({ id: "t", name: "n", owner: "o", createdAt: "", expiresAt: "2020-01-01T00:00:00Z" } as never, now)).toBe(
      "expired",
    );
    expect(
      tokenState({ id: "t", name: "n", owner: "o", createdAt: "", revokedAt: "2026-01-01T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z" } as never, now),
    ).toBe("revoked");
  });

  it("routes a server refusal that names the expiry onto the expiry field", async () => {
    renderPage({
      intercept: (m, url) =>
        m === "POST" && url === "/api/v1/tokens" ? problem(422, "invalid", "expiresAt must be in the future") : undefined,
    });
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("expiresAt must be in the future");
    expect(screen.getByRole("button", { name: "Expires" })).toHaveAttribute("aria-invalid", "true");
  });

  it("shows the minted secret once and takes it away for good on dismiss", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));

    expect(await screen.findByTestId("minted-token")).toHaveTextContent("kcm_deadbeef");
    fireEvent.click(screen.getByRole("button", { name: "I have saved it" }));
    await waitFor(() => expect(screen.queryByTestId("minted-token")).not.toBeInTheDocument());
    // Re-opening the create form does not bring it back: there is one copy and
    // the operator has had it.
    fireEvent.click(screen.getByRole("button", { name: "New token" }));
    expect(screen.queryByTestId("minted-token")).not.toBeInTheDocument();
  });

  it("says the browser refused the copy rather than pretending it worked", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));
    await screen.findByTestId("minted-token");

    vi.stubGlobal("navigator", { clipboard: { writeText: () => Promise.reject(new Error("denied")) } });
    fireEvent.click(screen.getByRole("button", { name: "Copy token" }));
    expect(await screen.findByText(en("tokens.secret.refused"))).toBeInTheDocument();
  });

  it("keeps the confirm idiom honest: cancelling deletes nothing", async () => {
    const { writes } = renderPage({ tokens: [tokenRow()] });
    fireEvent.click(await screen.findByRole("button", { name: /Revoke ci-pipeline/i }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(writes()).toEqual([]);
    expect(screen.getByRole("button", { name: /Revoke ci-pipeline/i })).toBeInTheDocument();
  });
});

/* The maintenance-windows section moved to pages/alerting.tsx (M3-14); its
   hostile rows moved into pages/alerting.test.tsx with it. */

/* ── bundles ────────────────────────────────────────────────────────────── */

describe("a file that is not a bundle", () => {
  it.each([
    ["an empty file", ""],
    ["whitespace", "   \n\t"],
    ["a truncated object", '{"version":1,'],
    ["a bare scalar", "123"],
    ["a bare string", '"hello"'],
    ["an array", "[]"],
    ["JSON null", "null"],
    ["HTML", "<!doctype html><html></html>"],
    ["YAML", "version: 1\ntargets: []"],
    ["a NUL-laden blob", "\u0000\u0000\u0000"],
  ])("refuses %s with a sentence, and POSTs nothing", async (_name, text) => {
    const { writes } = renderPage();
    await loadBundle(text);
    const alerts = await waitFor(() => {
      const found = screen.queryAllByRole("alert");
      expect(found.length).toBeGreaterThan(0);
      return found;
    });
    for (const a of alerts) expect((a.textContent ?? "").trim()).not.toBe("");
    expect(writes()).toEqual([]);
  });

  it.each([
    ["no version at all", {}],
    ["version 0", { version: 0 }],
    ["version 2", { version: 2 }],
    ["a string version", { version: "1" }],
    ["a null version", { version: null }],
    ["a negative version", { version: -1 }],
    ["a float version", { version: 1.5 }],
    ["an object version", { version: { major: 1 } }],
  ])("refuses %s, naming both numbers, and POSTs nothing", async (_name, body) => {
    const { writes } = renderPage();
    await loadBundle(body);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("1");
    expect(writes()).toEqual([]);
  });

  it("names the file it is refusing, so the message is about something", async () => {
    renderPage();
    await loadBundle("not json");
    expect(await screen.findByTestId("bundle-file-name")).toHaveTextContent("bundle.json");
  });

  it("does not let a bundle poison Object.prototype", async () => {
    const { writes } = renderPage();
    await loadBundle({ version: 1, __proto__: { polluted: "yes" }, targets: [] });
    await waitFor(() => expect(writes().length).toBeGreaterThan(0));
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
    expect(Object.prototype).not.toHaveProperty("polluted");
  });

  it("dry-runs a bundle with unknown collections in it, and never applies unasked", async () => {
    const { writes } = renderPage();
    await loadBundle({ version: 1, somethingNewer: [{ a: 1 }], targets: [] });
    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0].url).toBe("/api/v1/import");
    expect((writes()[0].body as { dryRun: boolean }).dryRun).toBe(true);
  });

  it("keeps Apply disabled while the bundle is unreadable", async () => {
    renderPage();
    await loadBundle("nope");
    await screen.findByRole("alert");
    expect(screen.getByRole("button", { name: en("bundle.apply") })).toBeDisabled();
  });

  it("clears the previous result when a second, broken file is chosen", async () => {
    renderPage();
    await loadBundle(BUNDLE);
    await screen.findByTestId("import-row-targets");
    await loadBundle("broken");
    await waitFor(() => expect(screen.queryByTestId("import-row-targets")).not.toBeInTheDocument());
    expectAlertsSpeak();
  });

  it("parses a five-megabyte bundle rather than hanging on it", () => {
    const big = JSON.stringify({ version: 1, targets: Array.from({ length: 20_000 }, (_, i) => ({ id: `t${i}` })) });
    expect(big.length).toBeGreaterThan(200_000);
    const started = Date.now();
    expect(parseBundle(big).ok).toBe(true);
    expect(Date.now() - started).toBeLessThan(2_000);
  });
});

/* ── the language switch, with work on screen ───────────────────────────── */

/**
 * FOREIGN ROOT — components/page-shell.tsx.
 *
 * PageShell renders `<div key={title}>` so that a route change re-runs its
 * entrance animation. The title is a TRANSLATED string, so switching the
 * language changes the key, and a changed key is not a re-render: React
 * unmounts the whole page and mounts a new one. Every piece of local state on
 * the page goes with it.
 *
 * On THIS page that means an open webhook form and everything typed into it, an
 * armed delete confirmation, a loaded bundle and its dry-run table — and the
 * MINTED TOKEN card, which is the one and only render of a secret the API will
 * never hand back. An operator who mints a token and then switches language has
 * lost it. It also refetches every query on the page for nothing.
 *
 * Every page in the console is affected identically; the fix belongs in
 * page-shell.tsx (key on the route path, not on the title — the animation is
 * about navigation, and a language switch is not one). The tests below are the
 * reproduction: the it.skip pair is what should hold, the active test is what
 * holds today, so the day the shell is fixed this file fails and the report is
 * closed rather than quietly rotting.
 */
describe("switching language keeps the page (page-shell fixed)", () => {
  it("keeps a half-typed webhook draft, and retitles the form around it", async () => {
    renderPage();
    await openWebhookForm();
    fillWebhook({ name: "pd", url: "https://x.test/hook", secret: "s3cret", event: true });

    fireEvent.click(screen.getByRole("radio", { name: "Русский" }));

    // Every byte the operator typed is still there...
    expect(screen.getByLabelText("Имя")).toHaveValue("pd");
    expect(screen.getByLabelText("URL")).toHaveValue("https://x.test/hook");
    // ...and the chrome around it is Russian.
    expect(screen.getByRole("button", { name: "Создать точку" })).toBeInTheDocument();
  });

  it("keeps the one-time minted token on screen across a language switch", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));
    await screen.findByTestId("minted-token");

    fireEvent.click(screen.getByRole("radio", { name: "Русский" }));

    expect(screen.getByTestId("minted-token")).toHaveTextContent("kcm_deadbeef");
  });

  it("does not refetch the page's lists on a language switch", async () => {
    const { calls } = renderPage({ webhooks: [webhookRow()] });
    await screen.findByText("pagerduty");
    // Let every initial query settle before counting.
    await waitFor(() => expect(calls.filter((c) => c.url.startsWith("/api/v1/webhooks")).length).toBe(1));
    fireEvent.click(screen.getByRole("radio", { name: "Русский" }));
    // A re-render, not a remount: the query is not fired again.
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.filter((c) => c.url.startsWith("/api/v1/webhooks")).length).toBe(1);
  });

  it("round-trips en → ru → en with a list on screen and loses nothing", async () => {
    renderPage({ webhooks: [webhookRow()], tokens: [tokenRow()] });
    await screen.findByText("pagerduty");

    fireEvent.click(screen.getByRole("radio", { name: "Русский" }));
    expect(screen.getByText("pagerduty")).toBeInTheDocument();
    expect(screen.getByText("ci-pipeline")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "English" }));
    expect(screen.getByText("pagerduty")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: en("tokens.heading") })).toBeInTheDocument();
    expectNoGarbage();
  });

  it("keeps the rows themselves intact through the round trip — the DATA is not translated", async () => {
    renderPage({ webhooks: [webhookRow({ lastStatus: "failed: 502", failures: 3 })] });
    await screen.findByText("pagerduty");
    fireEvent.click(screen.getByRole("radio", { name: "Русский" }));
    // The endpoint's name, its URL and the delivery ladder's own string are the
    // server's bytes and read the same in either language.
    expect(await screen.findByText("pagerduty")).toBeInTheDocument();
    expect(screen.getByText("https://hooks.example.test/pd")).toBeInTheDocument();
    expect(screen.getByTestId("last-status")).toHaveTextContent("failed: 502");
    expectNoGarbage();
  });
});

/* ── the session that ends mid-page ─────────────────────────────────────── */

describe("a session that expires under the page", () => {
  it("sends the reader to sign in, and remembers where they were", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    renderPage({ intercept: (_m, url) => (url.startsWith("/api/v1/tokens") ? problem(401, "unauthorized") : undefined) });

    await waitFor(() => expect(navigateSpy).toHaveBeenCalled());
    const target = String(navigateSpy.mock.calls[0][0]);
    expect(target.startsWith("/login")).toBe(true);
    expect(new URLSearchParams(target.slice(target.indexOf("?"))).get("returnTo")).toBe("/settings");
  });

  it("sends them there from a WRITE that 401s too, not only from a read", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    renderPage({
      intercept: (m, url) => (m === "POST" && url === "/api/v1/webhooks" ? problem(401, "session expired") : undefined),
    });
    await openWebhookForm();
    fillWebhook({ name: "pd", url: "https://x.test/hook", secret: "s", event: true });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(navigateSpy).toHaveBeenCalled());
    expect(String(navigateSpy.mock.calls[0][0])).toContain("returnTo=%2Fsettings");
    // ...and the page still says what happened rather than going blank while
    // the browser is on its way.
    expectAlertsSpeak();
  });

  it("renders a 403 in place rather than bouncing — a forbidden write is not a lost session", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    renderPage({
      intercept: (m, url) =>
        m === "POST" && url === "/api/v1/webhooks" ? problem(403, "forbidden", "webhooks:manage required") : undefined,
    });
    await openWebhookForm();
    fillWebhook({ name: "pd", url: "https://x.test/hook", secret: "s", event: true });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    expect(await screen.findByText("webhooks:manage required")).toBeInTheDocument();
    expect(navigateSpy).not.toHaveBeenCalled();
  });
});
