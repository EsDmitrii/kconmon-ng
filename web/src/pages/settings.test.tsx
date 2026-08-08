import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TimeMachineProvider } from "@/lib/timemachine";
import { SettingsPage, exportFilename, parseBundle, webhookRequestFrom } from "./settings";

/**
 * The Settings page is two admin-gated sections over one section everybody
 * sees, so almost every case below is really a question about a BOUNDARY:
 * which permission makes a section exist, which body a write puts on the wire,
 * and which of the two gates (permissions hide, time disables) a control is
 * under. The three that are not boundary questions — the honest lastStatus
 * rendering, the import result table and the export download — are the places
 * where the page could most easily invent a fact the API never stated.
 */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** The two permissions this page gates on. Both are ADMIN-ONLY in the built-in
 *  roles (internal/console/authz/roles.go): operator holds neither, which is
 *  why the operator case below asserts the both-hidden line rather than a
 *  partially populated page. */
const ADMIN = ["webhooks:manage", "settings:write", "maintenance:read", "incidents:read"];
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
  } = opts;
  const rows = [...webhooks] as Record<string, unknown>[];
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body: unknown = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: href, body });

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
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

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        <SettingsPage />
      </TimeMachineProvider>
    </QueryClientProvider>,
  );

  /** Every request the PAGE itself makes, i.e. excluding the /auth/me and
   *  /config chrome every route fetches regardless of what it renders. */
  const resourceCalls = () => calls.filter((c) => /^\/api\/v1\/(webhooks|export|import)/.test(c.url));
  return { ...utils, fetchMock, calls, resourceCalls, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
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
  it("admin sees webhooks, export/import and About", async () => {
    renderPage({ permissions: ADMIN });
    expect(await screen.findByRole("heading", { name: "Webhooks" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Configuration export / import" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "About this console" })).toBeInTheDocument();
    expect(screen.queryByText(/can view none of the console's settings/i)).not.toBeInTheDocument();
  });

  it("operator sees neither gated section, one honest line, and About", async () => {
    const { resourceCalls } = renderPage({ permissions: OPERATOR });
    expect(await screen.findByText(/Your role can view none of the console's settings/i)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Webhooks" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Configuration export / import" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "About this console" })).toBeInTheDocument();
    // HIDE means zero requests, not a hidden section that still fetched.
    expect(resourceCalls()).toEqual([]);
  });

  it("viewer gets exactly what operator gets", async () => {
    renderPage({ permissions: VIEWER });
    expect(await screen.findByText(/Your role can view none of the console's settings/i)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Webhooks" })).not.toBeInTheDocument();
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

  it("says secrets never travel in a bundle", async () => {
    renderPage();
    expect(await screen.findByText(/never carries a webhook secret/i)).toBeInTheDocument();
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

  /* M7 Task 12b: choosing a file fires the dry run on its own, and this table
     is the entire answer — arriving asynchronously well below the input, with
     nothing to announce it. Polite, like the webhook row's "Test queued". */
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
                  reason: "imported without secret: a bundle never carries webhook secrets",
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
    expect(screen.getByText("imported without secret: a bundle never carries webhook secrets")).toBeInTheDocument();
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

  it("does not pretend to administer roles or tokens", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "About this console" });
    expect(screen.queryByRole("heading", { name: /RBAC/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /token/i })).not.toBeInTheDocument();
  });
});
