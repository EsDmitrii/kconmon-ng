import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  DEFINITION_FIELD_PHRASES,
  TARGET_FIELD_PHRASES,
  TargetsPage,
  fieldForDetail,
  formatLabels,
  parseLabels,
  scheduleRequestFrom,
} from "./targets";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

// problem builds the exact envelope the server sends for a rejected write:
// application/problem+json, which is what lib/api.ts's `handle` keys on to
// raise an ApiError carrying `detail` (internal/console/httpapi/problem.go).
const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** The five M4 permissions the built-in operator role holds (authz.go). */
const OPERATOR = ["targets:read", "targets:write", "checks:read", "checks:write", "schedules:write"];

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] },
    permissions,
  };
}

function configBody(databaseConfigured: boolean) {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: databaseConfigured },
  };
}

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
    databaseConfigured?: boolean;
    targets?: unknown[];
    definitions?: unknown[];
    schedules?: unknown[];
    projection?: unknown;
    onPostTarget?: (body: unknown) => Response;
    onPostCheck?: (body: unknown) => Response;
    onWriteSchedule?: (body: unknown) => Response;
  } = {},
) {
  const {
    permissions = OPERATOR,
    databaseConfigured = true,
    targets = [],
    definitions = [],
    schedules = [],
    projection = OK_PROJECTION,
    onPostTarget,
    onPostCheck,
    onWriteSchedule,
  } = opts;
  // Stateful, so "create then refetch" is observable as a real change in the
  // list body rather than as a bare call count.
  const targetList = [...targets];
  const scheduleList = [...schedules];
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body: unknown = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: href, body });

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(databaseConfigured)));
    // Before the bare /api/v1/checks branch: the projection endpoint is a
    // longer path under the same prefix.
    if (href.startsWith("/api/v1/checks/projection")) return Promise.resolve(json(projection));
    if (href.startsWith("/api/v1/targets")) {
      if (method === "POST") {
        if (onPostTarget) return Promise.resolve(onPostTarget(body));
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
      if (method === "PUT") {
        const id = href.slice("/api/v1/targets/".length);
        const at = targetList.findIndex((t) => (t as { id: string }).id === id);
        const updated = targetRow({ id, ...(body as Record<string, unknown>) });
        if (at >= 0) targetList[at] = updated;
        return Promise.resolve(json(updated));
      }
      return Promise.resolve(json({ targets: targetList, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/checks")) {
      if (method === "POST" && onPostCheck) return Promise.resolve(onPostCheck(body));
      if (method === "POST") return Promise.resolve(json(definitionRow(body as Record<string, unknown>), { status: 201 }));
      return Promise.resolve(json({ definitions, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/schedules")) {
      if (onWriteSchedule && method !== "GET") return Promise.resolve(onWriteSchedule(body));
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
      if (method === "DELETE") {
        const id = href.slice("/api/v1/schedules/".length);
        const at = scheduleList.findIndex((s) => (s as { id: string }).id === id);
        if (at >= 0) scheduleList.splice(at, 1);
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(json({ schedules: scheduleList, nextCursor: "" }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <TargetsPage />
    </QueryClientProvider>,
  );

  /** Every request the PAGE itself makes, i.e. excluding the /auth/me and
   * /config chrome every route fetches regardless of what it renders. */
  const resourceCalls = () => calls.filter((c) => /^\/api\/v1\/(targets|checks|schedules)/.test(c.url));
  return { ...utils, fetchMock, calls, resourceCalls, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function openTab(name: RegExp) {
  fireEvent.click(await screen.findByRole("radio", { name }));
}

describe("fieldForDetail", () => {
  it("matches the field noun the server's own 422 detail leads with", () => {
    expect(fieldForDetail('target: name "web" is already taken; target names are unique', TARGET_FIELD_PHRASES)).toBe(
      "name",
    );
    expect(fieldForDetail('target: kind "nope" must be one of host, url', TARGET_FIELD_PHRASES)).toBe("kind");
    expect(fieldForDetail("target: address must not be empty", TARGET_FIELD_PHRASES)).toBe("address");
  });

  it("prefers the most specific phrase, so a compound field never collapses onto its prefix", () => {
    expect(
      fieldForDetail("definition: destination kind adhoc requires a destination address", DEFINITION_FIELD_PHRASES),
    ).toBe("destinationAddress");
    expect(
      fieldForDetail(
        'definition: destination target id "x" names no target',
        DEFINITION_FIELD_PHRASES,
      ),
    ).toBe("destinationTargetId");
    expect(
      fieldForDetail('definition: source selection "x" must be one of all, per-zone, one-per-zone', DEFINITION_FIELD_PHRASES),
    ).toBe("sourceSelection");
  });

  it("reports null for a detail that names no field, so the caller can fall back to a form-level error", () => {
    expect(fieldForDetail("targets not available", TARGET_FIELD_PHRASES)).toBeNull();
  });
});

describe("parseLabels", () => {
  it("round-trips through formatLabels", () => {
    expect(parseLabels("env=prod, tier=edge")).toEqual({ env: "prod", tier: "edge" });
    expect(parseLabels("")).toEqual({});
    expect(formatLabels({ env: "prod" })).toBe("env=prod");
    expect(formatLabels(undefined)).toBe("");
  });

  it("throws on a pair with no '=' rather than silently dropping it", () => {
    expect(() => parseLabels("env")).toThrow(/key=value/);
  });
});

describe("TargetsPage — database.mode=disabled", () => {
  it("names console.database.mode and issues zero targets/checks/schedules requests", async () => {
    const { resourceCalls } = renderPage({ databaseConfigured: false });

    expect(await screen.findByText(/console\.database\.mode/)).toBeInTheDocument();
    // Not "five requests to collect five 503s" — none at all.
    expect(resourceCalls()).toEqual([]);
    expect(screen.queryByRole("radio", { name: /targets/i })).not.toBeInTheDocument();
  });
});

describe("TargetsPage — no targets:read", () => {
  it("is a single permission-explained card and issues zero requests", async () => {
    const { resourceCalls } = renderPage({ permissions: [] });

    expect(await screen.findByText(/targets:read/)).toBeInTheDocument();
    expect(resourceCalls()).toEqual([]);
    // No tab strip at all: there is nothing behind any of the three tabs.
    expect(screen.queryByRole("radio", { name: /definitions/i })).not.toBeInTheDocument();
  });
});

describe("TargetsPage — no targets:write", () => {
  it("explains the missing permission and renders a fully functional read-only list", async () => {
    renderPage({ permissions: ["targets:read", "checks:read"], targets: [targetRow()] });

    expect(await screen.findByText("api-gw")).toBeInTheDocument();
    expect(screen.getByText(/targets:write/)).toBeInTheDocument();
  });

  it("omits the create button and the row actions entirely rather than disabling them", async () => {
    renderPage({ permissions: ["targets:read", "checks:read"], targets: [targetRow()] });

    await screen.findByText("api-gw");
    expect(screen.queryByRole("button", { name: /new target/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /edit api-gw/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /delete api-gw/i })).not.toBeInTheDocument();
  });

  it("shows the write affordances with targets:write", async () => {
    renderPage({ targets: [targetRow()] });

    await screen.findByText("api-gw");
    expect(screen.getByRole("button", { name: /new target/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /edit api-gw/i })).toBeInTheDocument();
  });
});

describe("TargetsPage — targets CRUD", () => {
  it("creates a target and refetches the list so the new row appears", async () => {
    const { calls } = renderPage({ targets: [] });

    fireEvent.click(await screen.findByRole("button", { name: /new target/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "edge-gw" } });
    fireEvent.change(screen.getByLabelText("Address"), { target: { value: "10.0.0.9" } });
    fireEvent.click(screen.getByRole("button", { name: /create target/i }));

    // The POST body is the full replace the API documents, labels included.
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.url === "/api/v1/targets")?.body).toEqual({
        name: "edge-gw",
        kind: "host",
        address: "10.0.0.9",
        labels: {},
      }),
    );
    // Proof of the refetch, not just of the POST: the row is only in the
    // stub's list because the POST put it there.
    expect(await screen.findByText("edge-gw")).toBeInTheDocument();
    expect(calls.filter((c) => c.method === "GET" && c.url.startsWith("/api/v1/targets")).length).toBeGreaterThan(1);
  });

  it("renders a 422's server detail inline at the offending field, not as a toast", async () => {
    const detail = 'target: name "edge-gw" is already taken; target names are unique';
    renderPage({ onPostTarget: () => problem(422, "invalid target", detail) });

    fireEvent.click(await screen.findByRole("button", { name: /new target/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "edge-gw" } });
    fireEvent.change(screen.getByLabelText("Address"), { target: { value: "10.0.0.9" } });
    fireEvent.click(screen.getByRole("button", { name: /create target/i }));

    const nameInput = await screen.findByLabelText("Name");
    await waitFor(() => expect(nameInput).toHaveAttribute("aria-invalid", "true"));
    const describedBy = nameInput.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy!)).toHaveTextContent(detail);
    // The address field is untouched — the error landed on ONE field.
    expect(screen.getByLabelText("Address")).not.toHaveAttribute("aria-invalid");
  });

  it("deletes a target behind an inline confirm, then refetches", async () => {
    const { calls } = renderPage({ targets: [targetRow()] });

    // First click only arms the action — a destructive config write is never
    // one stray click away.
    fireEvent.click(await screen.findByRole("button", { name: "Delete api-gw" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: /confirm delete api-gw/i }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === "DELETE" && c.url === "/api/v1/targets/t-1")).toBe(true),
    );
    await waitFor(() => expect(screen.queryByText("api-gw")).not.toBeInTheDocument());
  });
});

describe("TargetsPage — definitions tab and the projection", () => {
  it("lists definitions", async () => {
    renderPage({ definitions: [definitionRow()] });
    await openTab(/definitions/i);
    expect(await screen.findByText("gw-tcp")).toBeInTheDocument();
  });

  it("shows the projected series count and keeps submit enabled AT the limit", async () => {
    renderPage({ projection: { agents: 400, protocols: 1, series: 400, limit: 400, overLimit: false } });

    await openTab(/definitions/i);
    fireEvent.click(await screen.findByRole("button", { name: /new definition/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "gw-tcp" } });

    await waitFor(() => expect(screen.getByText(/400 series/)).toBeInTheDocument());
    expect(screen.queryByText(/above the 400-series limit/i)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /create definition/i })).toBeEnabled();
  });

  it("warns and disables submit once the server reports overLimit", async () => {
    renderPage({ projection: { agents: 401, protocols: 1, series: 401, limit: 400, overLimit: true } });

    await openTab(/definitions/i);
    fireEvent.click(await screen.findByRole("button", { name: /new definition/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "gw-tcp" } });

    await waitFor(() => expect(screen.getByText(/above the 400-series limit/i)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /create definition/i })).toBeDisabled();
  });

  it("sends the draft the form is about to submit to POST /api/v1/checks/projection", async () => {
    const { calls } = renderPage({});

    await openTab(/definitions/i);
    fireEvent.click(await screen.findByRole("button", { name: /new definition/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "gw-tcp" } });

    await waitFor(() => {
      const projections = calls.filter((c) => c.url === "/api/v1/checks/projection");
      expect(projections.length).toBeGreaterThan(0);
      expect(projections[projections.length - 1].body).toMatchObject({
        name: "gw-tcp",
        sourceSelection: "one-per-zone",
        destinationKind: "node",
        checkType: "tcp",
        plane: "pod",
        enabled: true,
      });
    });
  });

  it("never asks for a projection without checks:write — the endpoint is gated on it", async () => {
    const { calls } = renderPage({ permissions: ["targets:read", "targets:write", "checks:read"] });

    await openTab(/definitions/i);
    await screen.findByText(/checks:write/);
    expect(calls.some((c) => c.url === "/api/v1/checks/projection")).toBe(false);
    expect(screen.queryByRole("button", { name: /new definition/i })).not.toBeInTheDocument();
  });

  it("renders a definition 422 inline at the field its detail names", async () => {
    const detail = "definition: destination kind adhoc requires a destination address";
    renderPage({ onPostCheck: () => problem(422, "invalid check definition", detail) });

    await openTab(/definitions/i);
    fireEvent.click(await screen.findByRole("button", { name: /new definition/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "gw-tcp" } });
    fireEvent.change(screen.getByLabelText("Destination kind"), { target: { value: "adhoc" } });
    fireEvent.click(screen.getByRole("button", { name: /create definition/i }));

    const field = await screen.findByLabelText("Destination address");
    await waitFor(() => expect(field).toHaveAttribute("aria-invalid", "true"));
    expect(document.getElementById(field.getAttribute("aria-describedby")!)).toHaveTextContent(detail);
    // "destination address" beat the "destination kind" the same sentence
    // opens with — the compound phrase wins over the shared prefix.
    expect(screen.getByLabelText("Destination kind")).not.toHaveAttribute("aria-invalid");
  });

  // The projection 422 (httpapi's projectionDetail) names no form field at
  // all: it is about the definition as a whole. It must still render in full,
  // one level up, rather than being swallowed because the phrase table did
  // not recognise it.
  it("falls back to a form-level error, verbatim, for a 422 that names no field", async () => {
    const detail =
      "definition: too many projected series: enabling this definition projects 900 continuous external series (900 agents x 1 protocols), limit 400";
    renderPage({ onPostCheck: () => problem(422, "invalid check definition", detail) });

    await openTab(/definitions/i);
    fireEvent.click(await screen.findByRole("button", { name: /new definition/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "gw-tcp" } });
    fireEvent.click(screen.getByRole("button", { name: /create definition/i }));

    expect(await screen.findByText(detail)).toBeInTheDocument();
    expect(screen.getByLabelText("Name")).not.toHaveAttribute("aria-invalid");
  });
});

describe("TargetsPage — schedules tab", () => {
  it("names the definition rather than its UUID, and renders the cadence", async () => {
    renderPage({ definitions: [definitionRow()], schedules: [scheduleRow()] });

    await openTab(/schedules/i);
    const list = await screen.findByRole("list", { name: /schedules/i });
    expect(within(list).getByText("gw-tcp")).toBeInTheDocument();
    expect(within(list).getByText(/every 30s/i)).toBeInTheDocument();
  });

  it("renders an empty schedules list rather than a stub", async () => {
    const { calls } = renderPage({ schedules: [] });

    await openTab(/schedules/i);
    expect(await screen.findByText(/no schedules yet/i)).toBeInTheDocument();
    expect(calls.some((c) => c.url.startsWith("/api/v1/schedules"))).toBe(true);
  });

  // nextFireAt is null for a continuous schedule (the scheduler loop never
  // fires one) and for a retired "once" -- the row must say so rather than
  // inventing a time or rendering "Invalid Date".
  it("renders the next fire time, and an em dash when there is none", async () => {
    renderPage({
      definitions: [definitionRow()],
      schedules: [
        scheduleRow({ id: "s-1", nextFireAt: "2026-01-02T00:00:00Z" }),
        scheduleRow({ id: "s-2", kind: "continuous", intervalNs: 0, nextFireAt: null }),
      ],
    });

    await openTab(/schedules/i);
    const rows = within(await screen.findByRole("list", { name: /schedules/i })).getAllByRole("listitem");
    expect(rows[0]).toHaveTextContent(`next ${new Date("2026-01-02T00:00:00Z").toLocaleString()}`);
    expect(rows[1]).toHaveTextContent("next —");
    expect(rows[1]).toHaveTextContent("continuous");
  });

  it("offers only once, interval and continuous — cron is absent from the picker entirely", async () => {
    renderPage({ definitions: [definitionRow()] });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: /new schedule/i }));
    const kind = screen.getByLabelText("Kind");
    expect([...kind.querySelectorAll("option")].map((o) => o.textContent)).toEqual([
      "once",
      "interval",
      "continuous",
    ]);
  });

  // The three per-kind rules the server enforces (store.ScheduleInput.Validate),
  // expressed as which fields the form even shows.
  it("shows the interval field only for interval, the run-at field only for once, and neither for continuous", async () => {
    renderPage({ definitions: [definitionRow()] });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: /new schedule/i }));

    // interval is the default kind.
    expect(screen.getByLabelText("Interval (seconds)")).toBeInTheDocument();
    expect(screen.queryByLabelText("Run at")).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "once" } });
    expect(screen.getByLabelText("Run at")).toBeInTheDocument();
    expect(screen.queryByLabelText("Interval (seconds)")).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "continuous" } });
    expect(screen.queryByLabelText("Run at")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Interval (seconds)")).not.toBeInTheDocument();
  });

  it("creates an interval schedule in nanoseconds and refetches so the row appears", async () => {
    const { calls } = renderPage({ definitions: [definitionRow()], schedules: [] });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: /new schedule/i }));
    fireEvent.change(screen.getByLabelText("Interval (seconds)"), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: /create schedule/i }));

    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.url === "/api/v1/schedules")?.body).toEqual({
        definitionId: "d-1",
        kind: "interval",
        enabled: true,
        intervalNs: 30_000_000_000,
      }),
    );
    // Proof of the refetch: the row is only in the stub's list because the
    // POST put it there.
    expect(await screen.findByRole("list", { name: /schedules/i })).toHaveTextContent("gw-tcp");
  });

  it("creates a once schedule with an RFC 3339 runAt and no interval at all", async () => {
    const { calls } = renderPage({ definitions: [definitionRow()], schedules: [] });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: /new schedule/i }));
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "once" } });
    fireEvent.change(screen.getByLabelText("Run at"), { target: { value: "2030-01-01T10:00" } });
    fireEvent.click(screen.getByRole("button", { name: /create schedule/i }));

    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.url === "/api/v1/schedules")?.body).toEqual({
        definitionId: "d-1",
        kind: "once",
        enabled: true,
        // The datetime-local value is a LOCAL wall clock; what goes on the
        // wire is the instant it names.
        runAt: new Date("2030-01-01T10:00").toISOString(),
      }),
    );
  });

  it("creates a continuous schedule carrying neither interval nor runAt", async () => {
    const { calls } = renderPage({ definitions: [definitionRow()], schedules: [] });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: /new schedule/i }));
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "continuous" } });
    fireEvent.click(screen.getByRole("button", { name: /create schedule/i }));

    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.url === "/api/v1/schedules")?.body).toEqual({
        definitionId: "d-1",
        kind: "continuous",
        enabled: true,
      }),
    );
  });

  it("renders a schedule 422's server detail inline at the field it names", async () => {
    const detail = "schedule: kind once requires a run at time in the future";
    renderPage({
      definitions: [definitionRow()],
      onWriteSchedule: () => problem(422, "invalid schedule", detail),
    });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: /new schedule/i }));
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "once" } });
    fireEvent.change(screen.getByLabelText("Run at"), { target: { value: "2030-01-01T10:00" } });
    fireEvent.click(screen.getByRole("button", { name: /create schedule/i }));

    const runAt = await screen.findByLabelText("Run at");
    await waitFor(() => expect(runAt).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByRole("alert")).toHaveTextContent(detail);
  });

  it("toggles enabled through a full-replace PUT that carries the stored cadence back", async () => {
    const { calls } = renderPage({ definitions: [definitionRow()], schedules: [scheduleRow()] });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: "Disable gw-tcp" }));

    // Not {enabled:false} alone: PUT is a full replace, so an omitted
    // intervalNs would erase the very cadence being toggled.
    await waitFor(() =>
      expect(calls.find((c) => c.method === "PUT")?.body).toEqual({
        definitionId: "d-1",
        kind: "interval",
        intervalNs: 30_000_000_000,
        enabled: false,
      }),
    );
    expect(calls.find((c) => c.method === "PUT")?.url).toBe("/api/v1/schedules/s-1");
    expect(await screen.findByRole("button", { name: "Enable gw-tcp" })).toBeInTheDocument();
  });

  it("deletes a schedule behind an inline confirm, then refetches", async () => {
    const { calls } = renderPage({ definitions: [definitionRow()], schedules: [scheduleRow()] });

    await openTab(/schedules/i);
    fireEvent.click(await screen.findByRole("button", { name: "Delete gw-tcp" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: /confirm delete gw-tcp/i }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === "DELETE" && c.url === "/api/v1/schedules/s-1")).toBe(true),
    );
    await waitFor(() => expect(screen.queryByRole("list", { name: /schedules/i })).not.toBeInTheDocument());
  });

  it("without schedules:write the list is complete but every mutation affordance is absent", async () => {
    renderPage({
      permissions: ["targets:read", "checks:read"],
      definitions: [definitionRow()],
      schedules: [scheduleRow()],
    });

    await openTab(/schedules/i);
    expect(await screen.findByText(/schedules:write/)).toBeInTheDocument();
    expect(within(await screen.findByRole("list", { name: /schedules/i })).getByText("gw-tcp")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /new schedule/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /disable gw-tcp/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /delete gw-tcp/i })).not.toBeInTheDocument();
  });
});

describe("scheduleRequestFrom", () => {
  it("carries only the fields the schedule's own kind allows", () => {
    const base = {
      id: "s-1",
      definitionId: "d-1",
      enabled: true,
      lastFiredAt: null,
      nextFireAt: null,
      createdAt: "t",
      updatedAt: "t",
    };
    expect(scheduleRequestFrom({ ...base, kind: "interval", intervalNs: 30_000_000_000, runAt: null }, false)).toEqual({
      definitionId: "d-1",
      kind: "interval",
      intervalNs: 30_000_000_000,
      enabled: false,
    });
    expect(scheduleRequestFrom({ ...base, kind: "once", intervalNs: 0, runAt: "2030-01-01T00:00:00Z" }, true)).toEqual({
      definitionId: "d-1",
      kind: "once",
      runAt: "2030-01-01T00:00:00Z",
      enabled: true,
    });
    // continuous carries neither -- store refuses the extras rather than
    // ignoring them.
    expect(scheduleRequestFrom({ ...base, kind: "continuous", intervalNs: 0, runAt: null }, true)).toEqual({
      definitionId: "d-1",
      kind: "continuous",
      enabled: true,
    });
  });
});
