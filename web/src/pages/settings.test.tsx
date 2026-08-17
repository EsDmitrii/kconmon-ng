import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import {
  SettingsPage,
  exportFilename,
  parseBundle,
  subjectLine,
  webhookFieldForDetail,
  webhookRequestFrom,
} from "./settings";

/**
 * The three that are not boundary questions — the honest lastStatus rendering, the import result
 * table and the export download.
 */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** The permissions this page gates on. tokens:manage, webhooks:manage and
 *  settings:write are all ADMIN-ONLY in the built-in roles
 *  (internal/console/authz/roles.go): operator holds none of them, which is why
 *  the operator case below asserts the hidden line rather than a partially
 *  populated page. */
const ADMIN = ["tokens:manage", "webhooks:manage", "settings:write", "maintenance:read", "incidents:read"];
const OPERATOR = ["targets:read", "targets:write", "maintenance:read", "maintenance:write", "incidents:read"];
const VIEWER = ["topology:read", "matrix:read", "events:read"];

function meBody(permissions: string[], roles: string[] = ["admin"]) {
  return { subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles }, permissions };
}

function configBody(over: Record<string, unknown> = {}) {
  return {
    auth: { mode: "local", role: "viewer", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: true },
    ...over,
  };
}

function webhookRow(over: Record<string, unknown> = {}) {
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

/** The instant the token mock calls "now" when deciding whether a row is spent. */
const NOW_ISO = "2026-08-08T12:00:00Z";

/** GET /api/v1/tokens's per-row shape: metadata only, never a hash or a secret. */
function tokenRow(over: Record<string, unknown> = {}) {
  return {
    id: "t-1",
    name: "ci-pipeline",
    owner: "u1",
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const EMPTY_COLLECTION = { created: 0, updated: 0, skipped: 0, errors: [], warnings: [] };

function importResult(over: Record<string, unknown> = {}) {
  return {
    dryRun: true,
    targets: EMPTY_COLLECTION,
    checkDefinitions: EMPTY_COLLECTION,
    checkSchedules: EMPTY_COLLECTION,
    alertRules: EMPTY_COLLECTION,
    webhooks: EMPTY_COLLECTION,
    maintenanceWindows: EMPTY_COLLECTION,
    ...over,
  };
}

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

/** pickExpiry drives the token form's Expires DateTimePicker, the same shape
 *  pages/targets.test.tsx's pickRunAt drives the schedule form's Run-at. */
function pickExpiry(date: string, time: string) {
  fireEvent.click(screen.getByRole("button", { name: "Expires" }));
  fireEvent.change(screen.getByLabelText("Date"), { target: { value: date } });
  fireEvent.change(screen.getByLabelText("Time"), { target: { value: time } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
}

function renderPage(
  opts: {
    permissions?: string[];
    config?: Record<string, unknown>;
    webhooks?: unknown[];
    /** Flips the stored rows the moment POST /{id}/test is accepted, which is
     *  what makes "refetch the row after the delay" observable as a real change
     *  in the list body rather than as a bare call count. */
    onTest?: (rows: Record<string, unknown>[]) => void;
    onWriteWebhook?: (method: string, body: unknown) => Response | undefined;
    onImport?: (body: unknown) => Response;
    exportResponse?: Response;
    engaged?: boolean;
    /** The rows GET /api/v1/maintenance answers (QA round 3, finding #9). */
    maintenance?: unknown[];
    /** The rows GET /api/v1/tokens answers (QA round 6, finding #14). */
    tokens?: unknown[];
    /** A refusal standing in for GET /api/v1/tokens' 200. */
    tokensProblem?: Response;
    /** A refusal standing in for POST /api/v1/tokens' 2xx. */
    onCreateToken?: (body: unknown) => Response | undefined;
    /** A language already chosen in this browser, seeded the way a returning
     *  operator's localStorage would carry it. Absent ⇒ English, always. */
    locale?: "en" | "ru";
    /** What GET /api/v1/version answers — About renders the build from it. */
    versionBody?: Record<string, unknown>;
  } = {},
) {
  const {
    permissions = ADMIN,
    config = {},
    webhooks = [],
    onTest,
    onWriteWebhook,
    onImport,
    exportResponse,
    engaged = false,
    maintenance = [],
    tokens = [],
    tokensProblem,
    onCreateToken,
    locale,
    versionBody,
  } = opts;
  const rows = [...webhooks] as Record<string, unknown>[];
  const windows = [...maintenance] as Record<string, unknown>[];
  const tokenRows = [...tokens] as Record<string, unknown>[];
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body: unknown = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: href, body });

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/version")) {
      return Promise.resolve(json(versionBody ?? { version: "1.4.0", commit: "abc1234", capabilities: [] }));
    }
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(config)));
    if (href.startsWith("/api/v1/export")) {
      return Promise.resolve(exportResponse ?? json(BUNDLE));
    }
    if (href.startsWith("/api/v1/import")) {
      if (onImport) return Promise.resolve(onImport(body));
      return Promise.resolve(json(importResult({ dryRun: (body as { dryRun: boolean }).dryRun })));
    }
    if (href.endsWith("/test") && method === "POST") {
      onTest?.(rows);
      return Promise.resolve(new Response(null, { status: 202 }));
    }
    /* Mirrors handleTokensDelete (internal/console/httpapi/tokens.go): DELETE on
       an ACTIVE token REVOKES it and the row stays in the list with revokedAt
       set; DELETE on an already-spent one purges it. Splicing on the first
       DELETE — which is what this mock used to do — hid QA finding #4 entirely,
       because the row it lied about unmounting is the row that got stuck. */
    if (href.startsWith("/api/v1/tokens/") && method === "DELETE") {
      const id = decodeURIComponent(href.slice("/api/v1/tokens/".length));
      const at = tokenRows.findIndex((r) => (r as { id: string }).id === id);
      if (at >= 0) {
        const row = tokenRows[at] as { revokedAt?: string; expiresAt?: string };
        const spent = row.revokedAt !== undefined || (row.expiresAt !== undefined && row.expiresAt < NOW_ISO);
        if (spent) tokenRows.splice(at, 1);
        else tokenRows[at] = { ...row, revokedAt: "2026-08-08T12:00:00Z" };
      }
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (href.startsWith("/api/v1/tokens")) {
      if (method === "POST") {
        const override = onCreateToken?.(body);
        if (override) return Promise.resolve(override);
        const req = body as { name: string; expiresAt?: string };
        const created = tokenRow({ id: `t-${tokenRows.length + 1}`, name: req.name, expiresAt: req.expiresAt });
        tokenRows.push(created);
        // The one body in this API that carries a plaintext secret.
        return Promise.resolve(
          json({ id: created.id, name: created.name, token: "kcm_deadbeef", expiresAt: req.expiresAt }, { status: 201 }),
        );
      }
      return Promise.resolve(tokensProblem ?? json({ tokens: tokenRows }));
    }
    if (href.startsWith("/api/v1/maintenance/") && method === "DELETE") {
      const id = decodeURIComponent(href.slice("/api/v1/maintenance/".length));
      const at = windows.findIndex((w) => (w as { id: string }).id === id);
      if (at >= 0) windows.splice(at, 1);
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (href.startsWith("/api/v1/maintenance")) {
      return Promise.resolve(json({ windows, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/webhooks")) {
      const override = onWriteWebhook?.(method, body);
      if (override) return Promise.resolve(override);
      if (method === "POST") {
        // `secret` is stripped from the echo exactly as the server strips it:
        // no response in this API ever carries one back.
        const created: Record<string, unknown> = {
          ...webhookRow({ id: `w-${rows.length + 1}`, ...(body as Record<string, unknown>) }),
        };
        delete created.secret;
        rows.push(created);
        return Promise.resolve(json(created, { status: 201 }));
      }
      if (method === "PUT") {
        const id = href.slice("/api/v1/webhooks/".length);
        const at = rows.findIndex((w) => w.id === id);
        const updated: Record<string, unknown> = {
          ...webhookRow({ ...(rows[at] ?? {}), ...(body as Record<string, unknown>), id }),
        };
        delete updated.secret;
        if (at >= 0) rows[at] = updated;
        return Promise.resolve(json(updated));
      }
      if (method === "DELETE") {
        const id = href.slice("/api/v1/webhooks/".length);
        const at = rows.findIndex((w) => w.id === id);
        if (at >= 0) rows.splice(at, 1);
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(json({ webhooks: rows }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  // `?at=` is the only way the app itself engages the Time Machine, so the
  // tests engage it the same way rather than by faking the context.
  window.history.pushState({}, "", engaged ? `/settings?at=${AT}` : "/settings");
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  /*
   * LocaleProvider is here so the page's own language switcher is WIRED; the switcher is the one
   * control that needs a real setLocale.
   */
  const utils = render(
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <TimeMachineProvider>
          <SettingsPage />
        </TimeMachineProvider>
      </LocaleProvider>
    </QueryClientProvider>,
  );

  /** Every request the PAGE itself makes, i.e. excluding the /auth/me and
   *  /config chrome every route fetches regardless of what it renders. */
  const resourceCalls = () =>
    calls.filter((c) => /^\/api\/v1\/(tokens|webhooks|export|import|maintenance)/.test(c.url));
  return { ...utils, fetchMock, calls, resourceCalls, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
  /* vitest.setup.ts backs localStorage with ONE Map for this whole file, so a
     language chosen by the switcher cases below would otherwise translate
     every test that runs after them. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

const bundleFile = (body: unknown) =>
  new File([typeof body === "string" ? body : JSON.stringify(body)], "bundle.json", {
    type: "application/json",
  });

async function loadBundle(body: unknown) {
  const input = await screen.findByLabelText("Configuration bundle");
  fireEvent.change(input, { target: { files: [bundleFile(body)] } });
}

/* ── pure helpers ───────────────────────────────────────────────────────── */

describe("parseBundle", () => {
  it("accepts a version 1 bundle", () => {
    const parsed = parseBundle(JSON.stringify(BUNDLE));
    expect(parsed.ok).toBe(true);
  });

  it("refuses a file that is not JSON at all", () => {
    const parsed = parseBundle("not json {");
    expect(parsed).toEqual({ ok: false, message: expect.stringContaining("valid JSON") });
  });

  it("refuses a JSON array or scalar: a bundle is an object", () => {
    expect(parseBundle("[]").ok).toBe(false);
    expect(parseBundle("42").ok).toBe(false);
  });

  it("refuses any version but 1, naming both numbers", () => {
    const parsed = parseBundle(JSON.stringify({ ...BUNDLE, version: 2 }));
    expect(parsed.ok).toBe(false);
    if (!parsed.ok) {
      expect(parsed.message).toContain("version 1");
      expect(parsed.message).toContain("2");
    }
  });

  it("says nothing about the collections: the server is the authority on those", () => {
    // Every collection missing, version right — the client waves it through and
    // lets the dry run be the thing that finds out.
    expect(parseBundle(JSON.stringify({ version: 1 })).ok).toBe(true);
  });
});

describe("exportFilename", () => {
  it("names the day, not the instant", () => {
    expect(exportFilename(new Date("2026-08-08T22:15:00Z"))).toBe("kconmon-ng-config-2026-08-08.json");
  });
});

describe("webhookRequestFrom", () => {
  const draft = { name: "pd", url: "https://x.test", events: ["incident.created" as const], enabled: true };

  it("omits the secret KEY entirely when the field is blank", () => {
    const req = webhookRequestFrom({ ...draft, secret: "" });
    expect("secret" in req).toBe(false);
    // The wire body is what actually matters: "" would be a 422, and null
    // would be a different guess again.
    expect(JSON.parse(JSON.stringify(req))).toEqual(draft);
  });

  it("sends the secret when one was typed", () => {
    expect(webhookRequestFrom({ ...draft, secret: "s3cret" }).secret).toBe("s3cret");
  });

  it("does not trim the secret: leading and trailing bytes are part of a key", () => {
    expect(webhookRequestFrom({ ...draft, secret: " s3cret " }).secret).toBe(" s3cret ");
  });
});

/* ── section gating ─────────────────────────────────────────────────────── */

describe("section gating", () => {
  it("admin sees tokens, webhooks, export/import and About", async () => {
    renderPage({ permissions: ADMIN });
    expect(await screen.findByRole("heading", { name: "API tokens" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Webhooks" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Configuration export / import" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "About this console" })).toBeInTheDocument();
    expect(screen.queryByText(/can view none of the console's settings/i)).not.toBeInTheDocument();
  });

  /* An operator holds maintenance:write. */
  it("operator sees the maintenance list, neither admin section, and About", async () => {
    renderPage({ permissions: OPERATOR });
    expect(await screen.findByRole("heading", { name: "Maintenance windows" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Webhooks" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Configuration export / import" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "About this console" })).toBeInTheDocument();
    expect(screen.queryByText(/can view none of the console's settings/i)).not.toBeInTheDocument();
  });

  it("viewer sees no section at all — one honest line and About, and ZERO requests", async () => {
    const { resourceCalls } = renderPage({ permissions: VIEWER });
    expect(await screen.findByText(/Your role can view none of the console's settings/i)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Webhooks" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "API tokens" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Maintenance windows" })).not.toBeInTheDocument();
    // HIDE means zero requests, not a hidden section that still fetched.
    expect(resourceCalls()).toEqual([]);
  });

  it("a role with only tokens:manage sees tokens and nothing else gated", async () => {
    renderPage({ permissions: ["tokens:manage"] });
    expect(await screen.findByRole("heading", { name: "API tokens" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Webhooks" })).not.toBeInTheDocument();
    expect(screen.queryByText(/can view none of the console's settings/i)).not.toBeInTheDocument();
  });

  it("a role with only webhooks:manage sees webhooks and no export/import", async () => {
    renderPage({ permissions: ["webhooks:manage"] });
    expect(await screen.findByRole("heading", { name: "Webhooks" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Configuration export / import" })).not.toBeInTheDocument();
    expect(screen.queryByText(/can view none of the console's settings/i)).not.toBeInTheDocument();
  });

  it("a role with only settings:write sees export/import and no webhooks", async () => {
    const { resourceCalls } = renderPage({ permissions: ["settings:write"] });
    expect(await screen.findByRole("heading", { name: "Configuration export / import" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Webhooks" })).not.toBeInTheDocument();
    // Export/import is entirely button-driven: rendering the section must not
    // fetch a bundle nobody asked for.
    expect(resourceCalls()).toEqual([]);
  });
});

/* ── API tokens (QA round 6, finding #14) ───────────────────────────────── */

describe("tokens section", () => {
  it("lists what the API actually carries: name, owner, created, last used", async () => {
    renderPage({
      tokens: [tokenRow({ lastUsedAt: "2026-02-03T04:05:00Z" }), tokenRow({ id: "t-2", name: "grafana" })],
    });

    const list = await screen.findByRole("list", { name: "API tokens" });
    const rows = within(list).getAllByTestId("token-row");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText("ci-pipeline")).toBeInTheDocument();
    expect(within(rows[0]).getByText(/owner u1/)).toBeInTheDocument();
    expect(within(rows[0]).getByTestId("token-last-used")).toHaveTextContent(
      `last used ${new Date("2026-02-03T04:05:00Z").toLocaleString(undefined, { hour12: false })}`,
    );
  });

  /* lastUsedAt absent is a FACT — "never used" — not a field the API withheld,
     so it does not get fmtTime's em-dash. */
  it("says a token has never been used rather than showing an em-dash", async () => {
    renderPage({ tokens: [tokenRow()] });
    expect(await screen.findByTestId("token-last-used")).toHaveTextContent("never used");
  });

  it("tags a revoked row and offers it no second revoke", async () => {
    renderPage({ tokens: [tokenRow({ revokedAt: "2026-03-01T00:00:00Z" })] });
    const row = await screen.findByTestId("token-row");
    expect(within(row).getByText("revoked")).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: /Revoke/ })).not.toBeInTheDocument();
  });

  it("tags a token whose expiry has passed, without calling it revoked", async () => {
    renderPage({ tokens: [tokenRow({ expiresAt: "2020-01-01T00:00:00Z" })] });
    const row = await screen.findByTestId("token-row");
    expect(within(row).getByText("expired")).toBeInTheDocument();
    expect(within(row).queryByText("revoked")).not.toBeInTheDocument();
  });

  it("says so plainly when nothing has been minted", async () => {
    renderPage({ tokens: [] });
    expect(await screen.findByText("No tokens. Nothing is calling this API with one.")).toBeInTheDocument();
  });

  it("surfaces the server's 503 detail verbatim rather than an empty list", async () => {
    renderPage({
      tokensProblem: problem(
        503,
        "token admin not available",
        "set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/tokens",
      ),
    });

    expect(
      await screen.findByText(
        "set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/tokens",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("No tokens. Nothing is calling this API with one.")).not.toBeInTheDocument();
  });

  describe("create", () => {
    it("refuses a nameless token client-side and never POSTs it", async () => {
      const { calls } = renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "New token" }));
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));

      expect(await screen.findByText(/A token needs a name/)).toBeInTheDocument();
      expect(calls.some((c) => c.method === "POST" && c.url.startsWith("/api/v1/tokens"))).toBe(false);
    });

    it("POSTs the name alone when no expiry was typed", async () => {
      const { calls } = renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "New token" }));
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci-pipeline" } });
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));

      await screen.findByTestId("minted-token");
      const post = calls.find((c) => c.method === "POST" && c.url === "/api/v1/tokens");
      expect(post?.body).toEqual({ name: "ci-pipeline" });
    });

    it("sends the picked local wall clock as the instant it names", async () => {
      const { calls } = renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "New token" }));
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci-pipeline" } });
      pickExpiry("2027-01-02", "03:04");
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));

      await screen.findByTestId("minted-token");
      const post = calls.find((c) => c.method === "POST" && c.url === "/api/v1/tokens");
      expect((post?.body as { expiresAt: string }).expiresAt).toBe(new Date(2027, 0, 2, 3, 4).toISOString());
    });

    /* The one render of a raw token anywhere in this console: it is not stored,
       not re-fetchable and not in the list. */
    it("shows the secret ONCE, with the warning that there is no second time", async () => {
      renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "New token" }));
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci-pipeline" } });
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));

      expect(await screen.findByTestId("minted-token")).toHaveTextContent("kcm_deadbeef");
      expect(screen.getByText(/this is the only time it is shown/)).toBeInTheDocument();

      fireEvent.click(screen.getByRole("button", { name: "I have saved it" }));
      expect(screen.queryByTestId("minted-token")).not.toBeInTheDocument();
    });

    it("copies the secret and says the browser refused when it did", async () => {
      renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "New token" }));
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci-pipeline" } });
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));
      await screen.findByTestId("minted-token");

      const writeText = vi.fn(() => Promise.resolve());
      Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
      await act(async () => void fireEvent.click(screen.getByRole("button", { name: "Copy token" })));
      expect(writeText).toHaveBeenCalledWith("kcm_deadbeef");
      expect(screen.getByText("Token copied.")).toBeInTheDocument();

      // No clipboard at all names the fallback the operator can still act on.
      Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
      await act(async () => void fireEvent.click(screen.getByRole("button", { name: "Copy token" })));
      expect(screen.getByText(/select the token above and copy it/)).toBeInTheDocument();
    });

    it("renders a refused create verbatim rather than a generic failure", async () => {
      renderPage({
        onCreateToken: () => problem(422, "invalid request", `body must be JSON with a non-empty "name"`),
      });
      fireEvent.click(await screen.findByRole("button", { name: "New token" }));
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "x" } });
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));

      expect(await screen.findByText(`body must be JSON with a non-empty "name"`)).toBeInTheDocument();
      expect(screen.queryByTestId("minted-token")).not.toBeInTheDocument();
    });
  });

  it("revokes behind the confirm idiom — one click arms, the second sends DELETE", async () => {
    const { calls } = renderPage({ tokens: [tokenRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Revoke ci-pipeline" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Confirm revoke ci-pipeline" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url === "/api/v1/tokens/t-1")).toBe(true));
  });

  /* Engaged, every write on this page is disabled with ONE reason — the same
     rule the webhook and maintenance rows already follow. */
  it("disables every token write while the Time Machine is engaged", async () => {
    renderPage({ tokens: [tokenRow()], engaged: true });
    const revoke = await screen.findByRole("button", { name: "Revoke ci-pipeline" });
    expect(revoke).toBeDisabled();
    expect(screen.getByRole("button", { name: "New token" })).toBeDisabled();
  });
});

/* ── maintenance windows (QA round 3, finding #9) ───────────────────────── */

function windowRow(over: Record<string, unknown> = {}) {
  return {
    id: "m-1",
    scope: "node-a",
    startAt: "2030-01-01T10:00:00Z",
    endAt: "2030-01-01T12:00:00Z",
    reason: "switch firmware upgrade",
    createdBy: "user:ada",
    createdAt: "2026-08-08T00:00:00Z",
    ...over,
  };
}

describe("maintenance windows section", () => {
  it("is gated on maintenance:WRITE and asks for nothing without it", async () => {
    const { resourceCalls } = renderPage({ permissions: ["maintenance:read", "settings:write"] });
    expect(await screen.findByRole("heading", { name: "Configuration export / import" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Maintenance windows" })).not.toBeInTheDocument();
    expect(resourceCalls().filter((c) => c.url.startsWith("/api/v1/maintenance"))).toEqual([]);
  });

  it("asks for EVERY window — no from, no to, no scope, and it PAGES", async () => {
    const { resourceCalls } = renderPage({ permissions: ["maintenance:write"], maintenance: [windowRow()] });
    await screen.findByRole("heading", { name: "Maintenance windows" });
    const asked = resourceCalls().filter((c) => c.url.startsWith("/api/v1/maintenance"));
    expect(asked).toHaveLength(1);
    /* The whole point of this section: a RANGE would hide the future windows it exists to surface,
       and a scope would hide everybody else's. The limit is the OTHER half — the API pages at 100
       whether or not the caller asks, and this section followed no cursor, so past and running
       windows fell off the end of a list that claimed to be complete. */
    expect(asked[0].url).toBe("/api/v1/maintenance?limit=100");
  });

  it("lists a window whose whole span is in the FUTURE — the one the bars cannot show", async () => {
    renderPage({ permissions: ["maintenance:write"], maintenance: [windowRow()] });
    const list = await screen.findByRole("list", { name: "All maintenance windows" });
    expect(list).toHaveTextContent("switch firmware upgrade");
    expect(list).toHaveTextContent("node-a");
  });

  it("says so plainly when nothing has ever been declared", async () => {
    renderPage({ permissions: ["maintenance:write"] });
    expect(await screen.findByText(/No maintenance windows have been declared/i)).toBeInTheDocument();
  });

  it("deletes behind the confirm idiom — one click arms, the second sends DELETE", async () => {
    const { calls } = renderPage({ permissions: ["maintenance:write"], maintenance: [windowRow()] });
    fireEvent.click(await screen.findByRole("button", { name: /^Delete maintenance window: switch firmware upgrade$/ }));
    expect(calls.filter((c) => c.method === "DELETE")).toEqual([]);

    fireEvent.click(screen.getByRole("button", { name: /^Confirm delete maintenance window: switch firmware upgrade$/ }));
    await waitFor(() =>
      expect(calls.filter((c) => c.method === "DELETE").map((c) => c.url)).toEqual(["/api/v1/maintenance/m-1"]),
    );
    await waitFor(() => expect(screen.queryByText("switch firmware upgrade")).toBeNull());
  });
});

/* ── webhook list ───────────────────────────────────────────────────────── */

describe("webhook list", () => {
  it("renders name, url, events, enabled and hasSecret", async () => {
    renderPage({
      webhooks: [webhookRow({ events: ["incident.created", "incident.resolved"] })],
    });
    const row = await screen.findByRole("listitem");
    expect(within(row).getByText("pagerduty")).toBeInTheDocument();
    expect(within(row).getByText("https://hooks.example.test/pd")).toBeInTheDocument();
    expect(within(row).getByText("incident.created")).toBeInTheDocument();
    expect(within(row).getByText("incident.resolved")).toBeInTheDocument();
    expect(within(row).getByText("enabled")).toBeInTheDocument();
    expect(within(row).getByText("signed")).toBeInTheDocument();
  });

  it("shows an em-dash for an endpoint nothing has ever been delivered to", async () => {
    renderPage({ webhooks: [webhookRow({ lastStatus: "", failures: 0 })] });
    const row = await screen.findByRole("listitem");
    expect(within(row).getByTestId("last-status")).toHaveTextContent("—");
    // No invented "healthy": never-attempted is its own state.
    expect(within(row).queryByText("ok")).not.toBeInTheDocument();
  });

  it("renders lastStatus verbatim, including the server's failure sentence", async () => {
    renderPage({
      webhooks: [
        webhookRow({ id: "w-1", name: "a", lastStatus: "ok", lastAttempt: "2026-08-01T10:00:00Z", failures: 0 }),
        webhookRow({
          id: "w-2",
          name: "b",
          lastStatus: "failed: HTTP 418",
          lastAttempt: "2026-08-01T11:00:00Z",
          failures: 3,
        }),
      ],
    });
    const rows = await screen.findAllByRole("listitem");
    expect(within(rows[0]).getByTestId("last-status")).toHaveTextContent("ok");
    expect(within(rows[1]).getByTestId("last-status")).toHaveTextContent("failed: HTTP 418");
    expect(within(rows[1]).getByText(/3 consecutive failures/i)).toBeInTheDocument();
  });

  it("surfaces the server's 503 detail verbatim rather than an empty list", async () => {
    renderPage({
      onWriteWebhook: () => undefined,
      webhooks: [],
    });
    // Replace the list response with the real degraded-mode problem body.
    cleanup();
    vi.unstubAllGlobals();
    const fetchMock = vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(ADMIN)));
      if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
      return Promise.resolve(
        problem(503, "webhooks not available", "webhooks are stored configuration: set console.database.mode"),
      );
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <SettingsPage />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
    expect(await screen.findByText(/set console.database.mode/)).toBeInTheDocument();
  });
});

/* ── webhook create / edit ──────────────────────────────────────────────── */

describe("webhook form", () => {
  async function openCreate() {
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
  }

  it("requires a secret on create and does not POST without one", async () => {
    const { resourceCalls } = renderPage();
    await openCreate();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "pd" } });
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "https://x.test" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "incident.created" }));
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/secret is required/i);
    await waitFor(() => expect(resourceCalls().some((c) => c.method === "POST")).toBe(false));
  });

  it("states the create-time secret rule, and the edit-time keep rule", async () => {
    renderPage({ webhooks: [webhookRow()] });
    await openCreate();
    expect(screen.getByText(/Required — every delivery is signed/i)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    fireEvent.click(await screen.findByRole("button", { name: "Edit pagerduty" }));
    expect(await screen.findByText(/Leave blank to keep the current secret/i)).toBeInTheDocument();
  });

  it("POSTs name, url, events, enabled and the typed secret", async () => {
    const { resourceCalls } = renderPage();
    await openCreate();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "pd" } });
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "https://x.test" } });
    fireEvent.change(screen.getByLabelText(/^Secret/), { target: { value: "s3cret" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "incident.created" }));
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(resourceCalls().find((c) => c.method === "POST")).toBeDefined());
    expect(resourceCalls().find((c) => c.method === "POST")?.body).toEqual({
      name: "pd",
      url: "https://x.test",
      events: ["incident.created"],
      enabled: true,
      secret: "s3cret",
    });
  });

  it("PUTs WITHOUT a secret key when the edit form's secret box is left blank", async () => {
    const { resourceCalls } = renderPage({ webhooks: [webhookRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Edit pagerduty" }));
    fireEvent.change(await screen.findByLabelText("URL"), { target: { value: "https://moved.test/pd" } });
    fireEvent.click(screen.getByRole("button", { name: "Save endpoint" }));

    await waitFor(() => expect(resourceCalls().find((c) => c.method === "PUT")).toBeDefined());
    const put = resourceCalls().find((c) => c.method === "PUT");
    expect(put?.url).toBe("/api/v1/webhooks/w-1");
    // The assertion that matters: absent, not "" and not null. An empty string
    // is a 422 server-side and a null would be a third meaning nobody defined.
    expect(Object.keys(put?.body as object)).not.toContain("secret");
    expect(put?.body).toEqual({
      name: "pagerduty",
      url: "https://moved.test/pd",
      events: ["incident.created"],
      enabled: true,
    });
  });

  it("PUTs a secret key when the operator typed a replacement", async () => {
    const { resourceCalls } = renderPage({ webhooks: [webhookRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Edit pagerduty" }));
    fireEvent.change(await screen.findByLabelText(/^Secret/), { target: { value: "rotated" } });
    fireEvent.click(screen.getByRole("button", { name: "Save endpoint" }));

    await waitFor(() => expect(resourceCalls().find((c) => c.method === "PUT")).toBeDefined());
    expect((resourceCalls().find((c) => c.method === "PUT")?.body as { secret?: string }).secret).toBe("rotated");
  });

  it("renders a rejected write's problem detail verbatim", async () => {
    const { resourceCalls } = renderPage({
      onWriteWebhook: (method) =>
        method === "POST"
          ? problem(422, "invalid webhook", 'webhook: name "pagerduty" is already taken; webhook names are unique')
          : undefined,
    });
    await openCreate();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "pagerduty" } });
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "https://x.test" } });
    fireEvent.change(screen.getByLabelText(/^Secret/), { target: { value: "s" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "incident.created" }));
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    expect(await screen.findByText(/is already taken/)).toBeInTheDocument();
    expect(resourceCalls().some((c) => c.method === "POST")).toBe(true);
  });
});

describe("webhook delete", () => {
  it("asks before deleting and names the endpoint in the confirmation", async () => {
    const { resourceCalls } = renderPage({ webhooks: [webhookRow()] });
    fireEvent.click(await screen.findByRole("button", { name: "Delete pagerduty" }));
    expect(resourceCalls().some((c) => c.method === "DELETE")).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Confirm delete pagerduty" }));
    await waitFor(() => expect(resourceCalls().some((c) => c.method === "DELETE")).toBe(true));
    expect(resourceCalls().find((c) => c.method === "DELETE")?.url).toBe("/api/v1/webhooks/w-1");
  });
});

/* ── test delivery ──────────────────────────────────────────────────────── */

describe("send test", () => {
  it("POSTs to /test, says the outcome is asynchronous, and re-reads the row", async () => {
    const { resourceCalls } = renderPage({
      webhooks: [webhookRow({ lastStatus: "" })],
      onTest: (rows) => {
        rows[0] = { ...rows[0], lastStatus: "failed: HTTP 418", lastAttempt: "2026-08-01T12:00:00Z", failures: 1 };
      },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Send test to pagerduty" }));

    await waitFor(() =>
      expect(resourceCalls().some((c) => c.url === "/api/v1/webhooks/w-1/test" && c.method === "POST")).toBe(true),
    );
    // The copy must not claim a result the 202 never carried.
    expect(await screen.findByText(/test queued; the outcome lands on this row/i)).toBeInTheDocument();

    // …and the outcome that DOES arrive is the one the API stored, verbatim.
    await waitFor(
      () => expect(screen.getByTestId("last-status")).toHaveTextContent("failed: HTTP 418"),
      { timeout: 3000 },
    );
  });

  it("surfaces a refused test verbatim instead of queuing copy", async () => {
    cleanup();
    vi.unstubAllGlobals();
    const rows = [webhookRow()];
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(ADMIN)));
      if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
      if (href.endsWith("/test")) {
        return Promise.resolve(
          problem(503, "webhook delivery not configured", "set console.webhooks.encryptionKey (32 bytes, base64)"),
        );
      }
      void init;
      return Promise.resolve(json({ webhooks: rows }));
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <SettingsPage />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Send test to pagerduty" }));
    expect(await screen.findByText(/console.webhooks.encryptionKey/)).toBeInTheDocument();
    expect(screen.queryByText(/test queued/i)).not.toBeInTheDocument();
  });
});

/* ── export ─────────────────────────────────────────────────────────────── */

describe("export", () => {
  function stubObjectURL() {
    const create = vi.fn(() => "blob:kconmon");
    const revoke = vi.fn();
    // Assigned rather than vi.stubGlobal("URL", …): replacing the whole global
    // would take the URL constructor lib/timemachine.tsx and lib/api.ts use
    // with it.
    (URL as unknown as { createObjectURL: unknown }).createObjectURL = create;
    (URL as unknown as { revokeObjectURL: unknown }).revokeObjectURL = revoke;
    return { create, revoke };
  }

  it("downloads the bundle through a blob URL instead of navigating", async () => {
    const { create, revoke } = stubObjectURL();
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    const { resourceCalls } = renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Export configuration" }));
    await waitFor(() => expect(create).toHaveBeenCalled());

    // The request went through the api layer (fetch), not a tab navigation:
    // a 403 or 503 has to render on this page, not replace it with raw JSON.
    expect(resourceCalls().some((c) => c.url === "/api/v1/export" && c.method === "GET")).toBe(true);

    // `instances` is the `this` each spied call ran against, i.e. the anchor the
    // page built and clicked.
    const anchor = click.mock.instances[0] as unknown as HTMLAnchorElement;
    expect(anchor.download).toMatch(/^kconmon-ng-config-\d{4}-\d{2}-\d{2}\.json$/);
    expect(anchor.href).toContain("blob:kconmon");
    expect(revoke).toHaveBeenCalledWith("blob:kconmon");
  });

  it("says secrets never travel in a bundle — and that an import therefore CREATES no endpoint", async () => {
    renderPage();
    /* The old copy said imported endpoints "arrive without one and stay unusable until a secret is
       set here", which reads as "they arrive". They do not: importWebhooks only updates endpoints
       that already exist by name, so restoring a bundle onto a fresh console creates zero. */
    expect(await screen.findByText(/never carries a\s+secret/i)).toBeInTheDocument();
    expect(screen.getByText(/are NOT created by an import/i)).toBeInTheDocument();
  });

  it("renders an export failure instead of downloading an error page", async () => {
    const { create } = stubObjectURL();
    renderPage({ exportResponse: problem(502, "export unavailable", "failed to read the configuration to export") });
    fireEvent.click(await screen.findByRole("button", { name: "Export configuration" }));
    expect(await screen.findByText(/failed to read the configuration to export/)).toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
  });
});

/* ── import ─────────────────────────────────────────────────────────────── */

describe("import", () => {
  it("refuses a non-JSON file client-side and never POSTs it", async () => {
    const { resourceCalls } = renderPage();
    await loadBundle("not json {");
    expect(await screen.findByText(/valid JSON/i)).toBeInTheDocument();
    await waitFor(() => expect(resourceCalls().some((c) => c.url.startsWith("/api/v1/import"))).toBe(false));
  });

  it("refuses a version it does not read, naming both versions", async () => {
    const { resourceCalls } = renderPage();
    await loadBundle({ ...BUNDLE, version: 2 });
    expect(await screen.findByText(/version 1/)).toBeInTheDocument();
    await waitFor(() => expect(resourceCalls().some((c) => c.url.startsWith("/api/v1/import"))).toBe(false));
  });

  it("dry-runs FIRST, without being asked, and never with dryRun false", async () => {
    const { resourceCalls } = renderPage();
    await loadBundle(BUNDLE);
    await waitFor(() => expect(resourceCalls().some((c) => c.url.startsWith("/api/v1/import"))).toBe(true));
    const first = resourceCalls().find((c) => c.url.startsWith("/api/v1/import"));
    expect((first?.body as { dryRun: boolean }).dryRun).toBe(true);
    expect((first?.body as { bundle: unknown }).bundle).toEqual(BUNDLE);
    expect(await screen.findByText(/Dry run — nothing was written/i)).toBeInTheDocument();
  });

  /* Polite, like the webhook row's "Test queued". */
  it("announces the result instead of appearing silently", async () => {
    renderPage();
    await loadBundle(BUNDLE);
    const line = await screen.findByText(/Dry run — nothing was written/i);
    expect(line.closest('[role="status"]')).not.toBeNull();
  });

  it("renders every collection's counters plus its errors and warnings verbatim", async () => {
    renderPage({
      onImport: () =>
        json(
          importResult({
            dryRun: true,
            targets: { created: 2, updated: 1, skipped: 0, errors: [], warnings: [] },
            checkSchedules: {
              created: 0,
              updated: 0,
              skipped: 1,
              errors: [{ name: "edge-tcp/interval", reason: 'definition "edge-tcp" is not in this bundle' }],
              warnings: [],
            },
            webhooks: {
              created: 0,
              updated: 0,
              skipped: 1,
              errors: [],
              warnings: [
                {
                  name: "pagerduty",
                  reason: "not imported: a bundle never carries webhook secrets",
                },
              ],
            },
          }),
        ),
    });
    await loadBundle(BUNDLE);

    const targets = await screen.findByTestId("import-row-targets");
    expect(targets).toHaveTextContent("2");
    expect(targets).toHaveTextContent("1");

    // Errors and warnings are the server's own sentences, not a count.
    expect(screen.getByText('definition "edge-tcp" is not in this bundle')).toBeInTheDocument();
    expect(screen.getByText("edge-tcp/interval")).toBeInTheDocument();
    expect(screen.getByText("not imported: a bundle never carries webhook secrets")).toBeInTheDocument();
    expect(screen.getByText("pagerduty")).toBeInTheDocument();
  });

  it("keeps Apply enabled after an all-zero dry run: a no-op is a valid outcome", async () => {
    renderPage();
    await loadBundle(BUNDLE);
    expect(await screen.findByText(/Dry run — nothing was written/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply import" })).toBeEnabled();
  });

  it("disables Apply until a bundle is loaded", async () => {
    renderPage();
    expect(await screen.findByRole("button", { name: "Apply import" })).toBeDisabled();
  });

  it("applies with dryRun false and renders the applied result", async () => {
    const { resourceCalls } = renderPage({
      onImport: (body) => {
        const dryRun = (body as { dryRun: boolean }).dryRun;
        return json(
          importResult({
            dryRun,
            targets: { created: 2, updated: 0, skipped: 0, errors: [], warnings: [] },
          }),
        );
      },
    });
    await loadBundle(BUNDLE);
    expect(await screen.findByText(/Dry run — nothing was written/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Apply import" }));
    await waitFor(() =>
      expect(resourceCalls().filter((c) => c.url.startsWith("/api/v1/import")).length).toBe(2),
    );
    const applied = resourceCalls().filter((c) => c.url.startsWith("/api/v1/import"))[1];
    expect((applied.body as { dryRun: boolean }).dryRun).toBe(false);
    expect(await screen.findByText(/Applied — these writes happened/i)).toBeInTheDocument();
  });

  it("renders a refused import verbatim and keeps the bundle loaded", async () => {
    renderPage({
      onImport: () => problem(422, "invalid bundle", "import: bundle version 7 is not supported"),
    });
    await loadBundle(BUNDLE);
    expect(await screen.findByText("import: bundle version 7 is not supported")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply import" })).toBeEnabled();
  });
});

/* ── Time Machine: permissions hide, time disables ──────────────────────── */

describe("Time Machine", () => {
  it("disables every webhook write while engaged, without removing any of them", async () => {
    renderPage({ webhooks: [webhookRow()], engaged: true });
    expect(await screen.findByRole("button", { name: "Edit pagerduty" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "New endpoint" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete pagerduty" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send test to pagerduty" })).toBeDisabled();
  });

  it("leaves every webhook write enabled while live", async () => {
    renderPage({ webhooks: [webhookRow()] });
    expect(await screen.findByRole("button", { name: "Edit pagerduty" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "New endpoint" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Delete pagerduty" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Send test to pagerduty" })).toBeEnabled();
  });

  it("disables import while engaged and leaves EXPORT enabled: export is a read", async () => {
    renderPage({ engaged: true });
    expect(await screen.findByRole("button", { name: "Export configuration" })).toBeEnabled();
    expect(screen.getByLabelText("Configuration bundle")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Apply import" })).toBeDisabled();
  });

  it("leaves the import controls usable while live", async () => {
    renderPage();
    expect(await screen.findByLabelText("Configuration bundle")).toBeEnabled();
  });
});

/* ── About ──────────────────────────────────────────────────────────────── */

describe("About this console", () => {
  it("renders the auth mode and the subject's roles from what is already served", async () => {
    renderPage({ config: { auth: { mode: "oidc", role: "viewer", loginPath: "/api/v1/auth/oidc/start" } } });
    expect(await screen.findByText("oidc")).toBeInTheDocument();
    expect(screen.getByText("admin")).toBeInTheDocument();
  });

  it("names the anonymous role only in anonymous mode, where it is the role source", async () => {
    renderPage({ config: { auth: { mode: "anonymous", role: "viewer", loginPath: "" }, anonymousBanner: true } });
    expect(await screen.findByText(/every unauthenticated request is the viewer role/i)).toBeInTheDocument();
  });

  it("says plainly that retention numbers are not served to the browser", async () => {
    renderPage();
    expect(await screen.findByText(/GET \/api\/v1\/config does not serve the retention/i)).toBeInTheDocument();
  });

  it("links to the maintenance surfaces rather than duplicating them", async () => {
    renderPage();
    const investigate = await screen.findByRole("link", { name: /Investigate/ });
    expect(investigate).toHaveAttribute("href", "/investigate");
    expect(screen.getByRole("link", { name: /Explore/ })).toHaveAttribute("href", "/explore");
  });

  /* Roles and bindings are still nobody's business here; API tokens stopped
     being (QA round 6, finding #14), and the sentence moved with the feature. */
  it("does not pretend to administer roles, and no longer disowns tokens", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "About this console" });
    expect(screen.queryByRole("heading", { name: /RBAC/i })).not.toBeInTheDocument();
    expect(screen.getByText(/Roles and role bindings are not administered from this console at all/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "API tokens" })).toBeInTheDocument();
  });
});

/* ── QA round 5 ─────────────────────────────────────────────────────────── */

/* #13. A refused endpoint used to render its whole reason as one banner above
   the buttons, with nothing marking which of the four fields the server was
   talking about. */
describe("webhookFieldForDetail (#13)", () => {
  it.each([
    ["webhook: url \"x\" must start with http:// or https://", "url"],
    ["webhook: url must not be empty", "url"],
    ["webhook: name must not be empty", "name"],
    ['webhook: name "pd" is already taken; webhook names are unique', "name"],
    ["webhook: secret must not be empty: every delivery is signed", "secret"],
    ["webhook: events must not be empty", "events"],
    ['webhook: events[0]: "nope" must be one of incident.created, alert.fired', "events"],
  ])("routes %s to the %s field", (detail, field) => {
    expect(webhookFieldForDetail(detail)).toBe(field);
  });

  it("routes a message naming no field to nowhere, so it renders form-level", () => {
    expect(webhookFieldForDetail("webhooks are unavailable")).toBeNull();
  });
});

describe("webhook form — field-routed refusals (#13)", () => {
  async function openCreate() {
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
  }

  /* The client now refuses an empty event list on its own (#12), so a draft
     that is meant to REACH the server has to pick one. */
  function fillValidDraft(over: { name?: string; url?: string } = {}) {
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: over.name ?? "pd" } });
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: over.url ?? "https://x.test" } });
    fireEvent.change(screen.getByLabelText(/^Secret/), { target: { value: "s" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "incident.created" }));
  }

  it("marks the URL box when the server refuses the url, and no other box", async () => {
    renderPage({
      onWriteWebhook: (method) =>
        method === "POST"
          ? problem(422, "invalid webhook", 'webhook: url "ftp://x" must start with http:// or https://')
          : undefined,
    });
    await openCreate();
    // A URL the CLIENT accepts, so the server's own refusal is what is tested.
    fillValidDraft({ url: "https://x.test" });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(screen.getByLabelText("URL")).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByLabelText("Name")).not.toHaveAttribute("aria-invalid");
    expect(screen.getByLabelText(/^Secret/)).not.toHaveAttribute("aria-invalid");
    // The server's words still render, verbatim — now beside the box they are about.
    expect(screen.getByRole("alert")).toHaveTextContent("must start with http:// or https://");
  });

  it("marks the NAME box for a duplicate-name refusal", async () => {
    renderPage({
      onWriteWebhook: (method) =>
        method === "POST"
          ? problem(422, "invalid webhook", 'webhook: name "pd" is already taken; webhook names are unique')
          : undefined,
    });
    await openCreate();
    fillValidDraft();
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByLabelText("URL")).not.toHaveAttribute("aria-invalid");
  });

  it("marks the events GROUP, which has no single input to mark", async () => {
    const { resourceCalls } = renderPage();
    await openCreate();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "pd" } });
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "https://x.test" } });
    fireEvent.change(screen.getByLabelText(/^Secret/), { target: { value: "s" } });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() =>
      expect(screen.getByRole("group", { name: "Events" })).toHaveAttribute("aria-invalid", "true"),
    );
    // …and without a round trip: an empty event list is something the browser
    // can be certain about on its own (#12).
    expect(resourceCalls().filter((c) => c.method === "POST")).toEqual([]);
  });

  it("marks the SECRET box for the one rule it checks client-side", async () => {
    renderPage();
    await openCreate();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "pd" } });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(screen.getByLabelText(/^Secret/)).toHaveAttribute("aria-invalid", "true"));
  });

  it("leaves every box unmarked for a refusal that names no field", async () => {
    renderPage({
      onWriteWebhook: (method) =>
        method === "POST" ? problem(502, "webhooks unavailable", "failed to reach the webhook store") : undefined,
    });
    await openCreate();
    fillValidDraft();
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("failed to reach the webhook store"));
    for (const label of ["Name", "URL"]) {
      expect(screen.getByLabelText(label)).not.toHaveAttribute("aria-invalid");
    }
  });
});

/* #17. The button LOOKED guarded — Button disables itself while `loading` —
   but the flag it read was useState, which the handler about to set it cannot
   see. Three clicks in one task all ran, and all three POSTed. */
describe("webhook form — one submit per click storm (#17)", () => {
  it("POSTs once for three rapid clicks", async () => {
    const { resourceCalls } = renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "pd" } });
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "https://x.test" } });
    fireEvent.change(screen.getByLabelText(/^Secret/), { target: { value: "s3cret" } });
    // The client-side basics must PASS, or the storm never reaches the network.
    fireEvent.click(screen.getByRole("checkbox", { name: "incident.created" }));

    const submit = screen.getByRole("button", { name: "Create endpoint" });
    /* THREE CLICKS IN ONE TASK, with no render between them — the shape an impatient double-click actually has. */
    await act(async () => {
      submit.click();
      submit.click();
      submit.click();
    });

    await waitFor(() => expect(resourceCalls().filter((c) => c.method === "POST").length).toBe(1));
    expect(resourceCalls().filter((c) => c.method === "POST")).toHaveLength(1);
  });
});

/* #8. `<input type="file">` renders as the browser's own chrome — a grey
   "Choose File / no file selected" that cannot be themed at all, so in dark
   mode it was a light rectangle in the middle of a dark card. */
describe("configuration bundle picker (#8)", () => {
  it("hides the native input visually while keeping it focusable and named", async () => {
    renderPage();
    const input = await screen.findByLabelText("Configuration bundle");
    expect(input.className).toContain("sr-only");
    // sr-only, NOT hidden: still in the accessibility tree and the tab order.
    expect(input.className).not.toContain("hidden");
    expect(input).not.toHaveAttribute("tabindex", "-1");
  });

  it("gives the pointer a styled label-button wired to the input", async () => {
    renderPage();
    const input = await screen.findByLabelText("Configuration bundle");
    const label = screen.getByTestId("bundle-file-label");
    expect(label).toHaveTextContent("Choose bundle…");
    expect(label.getAttribute("for")).toBe(input.getAttribute("id"));
    // The button styling lives on the LABEL, and both themes come from tokens.
    expect(label.className).toContain("border-border-strong");
    expect(label.className).toContain("hover:bg-accent");
    expect(label.className).toContain("peer-disabled:opacity-50");
  });

  it("names the chosen file, which the hidden input can no longer do itself", async () => {
    renderPage();
    expect(await screen.findByTestId("bundle-file-name")).toHaveTextContent("No file chosen");
    await loadBundle(BUNDLE);
    await waitFor(() => expect(screen.getByTestId("bundle-file-name")).toHaveTextContent("bundle.json"));
  });

  it("names the file even when it will not parse — the error has to be ABOUT something", async () => {
    renderPage();
    await loadBundle("not json {");
    await waitFor(() => expect(screen.getByTestId("bundle-file-name")).toHaveTextContent("bundle.json"));
    expect(screen.getByRole("alert")).toHaveTextContent(/valid JSON/i);
  });
});

/* #9. An anonymous subject has no display name, and the fixed template printed
   the separator anyway: "anonymous · " reads as a name that failed to load. */
describe("subjectLine (#9)", () => {
  it("drops the separator when there is nothing on one side", () => {
    expect(subjectLine("anonymous", "")).toBe("anonymous");
    expect(subjectLine("anonymous", "   ")).toBe("anonymous");
    expect(subjectLine("", "Ada")).toBe("Ada");
  });

  it("joins both when both are there", () => {
    expect(subjectLine("user", "Ada")).toBe("user · Ada");
  });

  it("never leaves a dangling separator", () => {
    for (const [kind, name] of [
      ["anonymous", ""],
      ["", ""],
      ["token", ""],
    ]) {
      expect(subjectLine(kind, name).endsWith("·")).toBe(false);
      expect(subjectLine(kind, name)).not.toContain("· ·");
    }
  });

  it("renders the anonymous subject with no trailing separator on the page", async () => {
    renderPage({ permissions: VIEWER });
    const fact = await screen.findByText("Your subject");
    const value = fact.parentElement?.querySelector("dd");
    expect(value?.textContent).not.toMatch(/·\s*$/);
  });
});

/* ── language switcher (lib/i18n) ───────────────────────────────────────── */

/** The console's language switch, pinned in BOTH languages. */
describe("language switcher", () => {
  const group = () => screen.getByRole("radiogroup", { name: "Interface language" });
  const option = (name: string) => within(group()).getByRole("radio", { name });

  it("renders for every role, including the one that can view nothing else", async () => {
    const { resourceCalls } = renderPage({ permissions: VIEWER });
    expect(await screen.findByRole("heading", { name: "Language" })).toBeInTheDocument();
    expect(group()).toBeInTheDocument();
    // A display preference held in this browser: nothing is asked of the API.
    expect(resourceCalls()).toEqual([]);
  });

  it("opens on English, with English checked", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "Language" });
    expect(option("English")).toBeChecked();
    expect(option("Русский")).not.toBeChecked();
  });

  it("names each language in that language, in both locales", async () => {
    renderPage({ locale: "ru" });
    await screen.findByRole("heading", { name: "Язык" });
    // Endonyms: "Русский" does not become "Russian" for an English reader
    // hunting for the Russian option, so these two never change.
    expect(within(screen.getByRole("radiogroup", { name: "Язык интерфейса" })).getByRole("radio", { name: "English" }))
      .toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Русский" })).toBeInTheDocument();
  });

  it("applies instantly — the section retitles itself on the click", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "Language" });
    fireEvent.click(option("Русский"));
    expect(screen.getByRole("heading", { name: "Язык" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Language" })).not.toBeInTheDocument();
    expect(screen.getByRole("radiogroup", { name: "Язык интерфейса" })).toBeInTheDocument();
  });

  it("persists the choice, and opens in it next time", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "Language" });
    fireEvent.click(option("Русский"));
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("ru");

    cleanup();
    renderPage();
    expect(await screen.findByRole("heading", { name: "Язык" })).toBeInTheDocument();
  });

  it("is a radiogroup the arrow keys walk, like every other Segmented", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "Language" });
    const english = option("English");
    expect(english).toHaveAttribute("tabindex", "0");
    fireEvent.keyDown(english, { key: "ArrowRight" });
    expect(screen.getByRole("radio", { name: "Русский" })).toBeChecked();
  });

  it("switches the chrome ONLY — the server's own words stay the server's", async () => {
    renderPage({
      permissions: ADMIN,
      webhooks: [webhookRow({ name: "pagerduty", lastStatus: "failed: 500 Internal Server Error" })],
      locale: "ru",
    });
    await screen.findByRole("heading", { name: "Язык" });
    // Endpoint name, url, event ids and the delivery outcome are DATA. A
    // Russian console reports them in the server's words or it is inventing
    // what the server said.
    expect(await screen.findByText("pagerduty")).toBeInTheDocument();
    expect(screen.getByText("failed: 500 Internal Server Error")).toBeInTheDocument();
    expect(screen.getByText("incident.created")).toBeInTheDocument();
  });
});

/* ── QA scope 5 ─────────────────────────────────────────────────────────── */

/* #15. About answered "what am I looking at" without ever saying WHICH BUILD —
   the first question of any bug report. */
describe("About names the build it is running (#15)", () => {
  it("renders the version and commit the server reports", async () => {
    renderPage({ versionBody: { version: "1.4.0", commit: "9f3c1ab", capabilities: [] } });
    await waitFor(() => expect(screen.getByTestId("about-version")).toHaveTextContent("1.4.0"));
    expect(screen.getByTestId("about-commit")).toHaveTextContent("9f3c1ab");
  });

  it("prints a dev build as the server calls it, without dressing it up", async () => {
    renderPage({ versionBody: { version: "dev", commit: "unknown", capabilities: [] } });
    await waitFor(() => expect(screen.getByTestId("about-version")).toHaveTextContent("dev"));
    expect(screen.getByTestId("about-commit")).toHaveTextContent("unknown");
  });
});

/* #14. A revoked row was permanent: the list only ever grew, and the one
   action the row still needed was the one it did not offer. */
describe("a spent token can be deleted for good (#14)", () => {
  it("offers Delete — not Revoke — on a revoked row", async () => {
    renderPage({ tokens: [tokenRow({ revokedAt: "2026-03-01T00:00:00Z" })] });
    const row = await screen.findByTestId("token-row");
    expect(within(row).getByRole("button", { name: "Delete ci-pipeline" })).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: "Revoke ci-pipeline" })).toBeNull();
  });

  it("offers Delete on an EXPIRED row too", async () => {
    renderPage({ tokens: [tokenRow({ expiresAt: "2020-01-01T00:00:00Z" })] });
    const row = await screen.findByTestId("token-row");
    expect(within(row).getByRole("button", { name: "Delete ci-pipeline" })).toBeInTheDocument();
  });

  it("still says Revoke on a live one — the two acts keep their two words", async () => {
    renderPage({ tokens: [tokenRow()] });
    const row = await screen.findByTestId("token-row");
    expect(within(row).getByRole("button", { name: "Revoke ci-pipeline" })).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: "Delete ci-pipeline" })).toBeNull();
  });

  it("DELETEs behind a confirm, and refetches", async () => {
    const { calls } = renderPage({ tokens: [tokenRow({ revokedAt: "2026-03-01T00:00:00Z" })] });
    const row = await screen.findByTestId("token-row");
    fireEvent.click(within(row).getByRole("button", { name: "Delete ci-pipeline" }));
    fireEvent.click(within(row).getByRole("button", { name: "Confirm delete ci-pipeline" }));

    await waitFor(() =>
      expect(calls.find((c) => c.method === "DELETE" && c.url === "/api/v1/tokens/t-1")).toBeDefined(),
    );
  });

  /* QA scope 4, finding #4: revoking left the row on screen — correctly, that is
     what the server does — but the row's own `busy`/`confirming` state was only
     ever cleared on the FAILURE path, on the assumption that a success unmounts
     it. A revoke does not. The row came back as "revoked", offered Delete, and
     the button behind it stayed disabled with a spinner until a reload. */
  it("mints, revokes and purges a token in one visit, with no remount in between", async () => {
    const { calls } = renderPage({ tokens: [] });

    /* ── mint ── */
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "throwaway" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));
    const row = await screen.findByTestId("token-row");
    expect(within(row).getByText("throwaway")).toBeInTheDocument();

    /* ── revoke ── */
    fireEvent.click(within(row).getByRole("button", { name: "Revoke throwaway" }));
    fireEvent.click(within(row).getByRole("button", { name: "Confirm revoke throwaway" }));

    /* The row survives the revoke and re-labels itself for the act it now offers. */
    const purge = await within(await screen.findByTestId("token-row")).findByRole("button", {
      name: "Delete throwaway",
    });
    expect(within(screen.getByTestId("token-row")).getByText("revoked")).toBeInTheDocument();
    /* THE BUG: this button used to arrive already disabled, spinner and all. */
    expect(purge).toBeEnabled();

    /* ── purge, in the SAME visit ── */
    fireEvent.click(purge);
    fireEvent.click(screen.getByRole("button", { name: "Confirm delete throwaway" }));

    await waitFor(() => expect(screen.queryByTestId("token-row")).toBeNull());
    const deletes = calls.filter((c) => c.method === "DELETE" && c.url.startsWith("/api/v1/tokens/"));
    expect(deletes).toHaveLength(2);
  });

  it("drops the confirm prompt after a revoke instead of leaving it armed", async () => {
    renderPage({ tokens: [tokenRow()] });
    const row = await screen.findByTestId("token-row");
    fireEvent.click(within(row).getByRole("button", { name: "Revoke ci-pipeline" }));
    fireEvent.click(within(row).getByRole("button", { name: "Confirm revoke ci-pipeline" }));

    await waitFor(() => expect(screen.getByText("revoked")).toBeInTheDocument());
    /* Neither confirm word is on screen: a second destructive act must be asked for, not inherited. */
    expect(screen.queryByRole("button", { name: /^Confirm/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
  });
});

/* #13. The form offered ten years of past days for an expiry the server
   refuses, and minting one would only leak a secret for nothing. */
describe("the token expiry cannot be in the past (#13)", () => {
  const NOW = new Date(2026, 7, 8, 12, 0, 0);

  afterEach(() => {
    vi.useRealTimers();
  });

  it("disables past days in the picker", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.click(screen.getByRole("button", { name: "Expires" }));

    expect(screen.getByRole("button", { name: "Choose 7 August 2026" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Choose 8 August 2026" })).toBeEnabled();
  });

  it("refuses a time earlier TODAY without minting anything", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
    const { calls } = renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci" } });
    pickExpiry("2026-08-08", "09:00");
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/must be in the future/i);
    // No secret was put on the wire for a credential that could never work.
    expect(calls.find((c) => c.method === "POST" && c.url === "/api/v1/tokens")).toBeUndefined();
  });

  it("still mints a token with no expiry at all", async () => {
    const { calls } = renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "forever" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));

    await screen.findByTestId("minted-token");
    expect(calls.find((c) => c.method === "POST" && c.url === "/api/v1/tokens")?.body).toEqual({ name: "forever" });
  });
});

/* #12. The webhook form learned one problem per round trip, and printed it in
   a single slot 256px below the box it was about. */
describe("the webhook form says everything wrong at once (#12)", () => {
  it("reports name, url, events and secret together, with no round trip", async () => {
    const { resourceCalls } = renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByLabelText("URL")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("group", { name: "Events" })).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText(/^Secret/)).toHaveAttribute("aria-invalid", "true");
    expect(resourceCalls().filter((c) => c.method === "POST")).toEqual([]);
  });

  it("catches a malformed URL and a bad name charset in the same submit", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Page Duty" } });
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "hooks.example.test" } });
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getAllByRole("alert").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText(/lowercase letters, digits and hyphens/i)).toBeInTheDocument();
    expect(screen.getByText(/must start with http:\/\/ or https:\/\//i)).toBeInTheDocument();
  });

  it("puts each message beside its own field, not in one slot far below", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));

    const url = await screen.findByLabelText("URL");
    const describedBy = url.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    const message = document.getElementById(describedBy!);
    expect(message).toHaveTextContent(/URL is required/i);
    // Same container as the field: the message and the box it is about are in
    // one place now.
    expect(url.parentElement?.parentElement).toContainElement(message);
  });

  it("clears a field's message the moment that field is edited", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
    fireEvent.click(screen.getByRole("button", { name: "Create endpoint" }));
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "true"));

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "pagerduty" } });
    await waitFor(() => expect(screen.getByLabelText("Name")).not.toHaveAttribute("aria-invalid"));
    // The other fields' verdicts are untouched — only the answered one goes.
    expect(screen.getByLabelText("URL")).toHaveAttribute("aria-invalid", "true");
  });

  it("keeps an empty secret legal when EDITING, where blank means keep", async () => {
    const { calls } = renderPage({
      webhooks: [{ id: "w-1", name: "pd", url: "https://x.test", events: ["incident.created"], enabled: true, hasSecret: true, lastStatus: "ok", lastAttempt: null, failures: 0 }],
    });
    fireEvent.click(await screen.findByRole("button", { name: "Edit pd" }));
    fireEvent.click(screen.getByRole("button", { name: "Save endpoint" }));

    await waitFor(() => expect(calls.find((c) => c.method === "PUT")).toBeDefined());
  });
});

/* #11. «База данных настроен» — Russian agrees the participle with the
   subject's gender, and one key cannot serve three subjects. */
describe("the configured flags agree in Russian (#11)", () => {
  it("says «настроена» for «База данных» and «настроен» for the two masculine ones", async () => {
    renderPage({
      locale: "ru",
      config: { controller: { configured: true }, prometheus: { configured: true }, database: { configured: true } },
    });
    expect(await screen.findByText("настроена")).toBeInTheDocument();
    expect(screen.getAllByText("настроен")).toHaveLength(2);
  });

  it("negates with the same agreement", async () => {
    renderPage({
      locale: "ru",
      config: { controller: { configured: false }, prometheus: { configured: false }, database: { configured: false } },
    });
    expect(await screen.findByText("не настроена")).toBeInTheDocument();
    expect(screen.getAllByText("не настроен")).toHaveLength(2);
  });
});

/* #23. Same question, same two forms: the webhook form is the other one with
   enough in it to be worth asking about. */
describe("the webhook form asks before discarding unsaved work (#23)", () => {
  async function openCreateForm() {
    fireEvent.click(await screen.findByRole("button", { name: "New endpoint" }));
  }

  it("closes immediately when nothing was typed", async () => {
    renderPage();
    await openCreateForm();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByLabelText("URL")).toBeNull());
  });

  it("asks once when the draft is dirty, and keeps the form on Keep editing", async () => {
    renderPage();
    await openCreateForm();
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "https://x.test" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(await screen.findByText("Discard the changes?")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.getByLabelText("URL")).toHaveValue("https://x.test");
  });

  it("closes on the second, explicit answer", async () => {
    renderPage();
    await openCreateForm();
    fireEvent.change(screen.getByLabelText("URL"), { target: { value: "https://x.test" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));

    await waitFor(() => expect(screen.queryByLabelText("URL")).toBeNull());
  });

  it("leaves the TOKEN form alone — a two-field form is cheaper to retype", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "ci" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByLabelText("Name")).toBeNull());
  });
});
