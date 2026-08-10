import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { MaintenanceWindow } from "@/lib/types";
import { ExplorePage } from "./explore";

/** Explore's maintenance overlay — mirroring explore.annotations.test.tsx exactly. */
vi.mock("@/components/echart", () => ({
  EChart: ({ className, maintenance }: { className?: string; maintenance?: MaintenanceWindow[] }) => (
    <div data-testid="echart" className={className} data-maintenance={(maintenance ?? []).map((w) => w.id).join(",")} />
  ),
}));

const NOW = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function win(over: Partial<MaintenanceWindow> = {}): MaintenanceWindow {
  return {
    id: "m-1",
    scope: "",
    startAt: "2026-08-01T11:30:00Z",
    endAt: "2026-08-01T11:45:00Z",
    reason: "core switch upgrade",
    createdBy: "user:ada",
    createdAt: "2026-08-01T10:00:00Z",
    ...over,
  };
}

function stubFetch(opts: { permissions?: string[]; windows?: MaintenanceWindow[] } = {}) {
  const { permissions = ["maintenance:read", "maintenance:write"], windows = [] } = opts;
  const maintenanceQueries: URLSearchParams[] = [];
  const createBodies: unknown[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(
        json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions }),
      );
    }
    if (href.startsWith("/api/v1/maintenance") && method === "POST") {
      createBodies.push(JSON.parse(String(init?.body ?? "{}")));
      return Promise.resolve(json(win({ id: "new" })));
    }
    if (href.startsWith("/api/v1/maintenance")) {
      maintenanceQueries.push(new URLSearchParams(href.split("?")[1] ?? ""));
      return Promise.resolve(json({ windows, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/annotations")) return Promise.resolve(json({ annotations: [], nextCursor: "" }));
    if (href.includes("/api/v1/promql/query_range")) {
      return Promise.resolve(
        json({
          status: "success",
          data: { resultType: "matrix", result: [{ metric: { protocol: "tcp" }, values: [[1785283200, "1"]] }] },
        }),
      );
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { fetchMock, maintenanceQueries, createBodies };
}

/** The window LENGTH one request asked for, in seconds. */
function spanSecondsOf(qs: URLSearchParams): number {
  return (Date.parse(qs.get("to") ?? "") - Date.parse(qs.get("from") ?? "")) / 1000;
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          <ExplorePage />
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(new Date(NOW));
  window.history.pushState({}, "", "/explore");
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("ExplorePage maintenance", () => {
  it("asks for the GLOBAL scope only — present-but-empty, never absent", async () => {
    const { maintenanceQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(maintenanceQueries.length).toBeGreaterThan(0));
    expect(maintenanceQueries.every((qs) => qs.has("scope"))).toBe(true);
    expect([...new Set(maintenanceQueries.map((qs) => qs.get("scope")))]).toEqual([""]);
  });

  it("bounds the fetch to the chart's visible range — 1h by default", async () => {
    const { maintenanceQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(maintenanceQueries.length).toBeGreaterThan(0));
    expect(spanSecondsOf(maintenanceQueries[0])).toBe(3600);
  });

  it("re-asks over the NEW window when the range picker moves", async () => {
    const { maintenanceQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(maintenanceQueries.length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole("radio", { name: "15m" }));
    await waitFor(() => expect(maintenanceQueries.map(spanSecondsOf)).toContain(900));
  });

  it("hands the windows to every chart so the bands land on all of them", async () => {
    stubFetch({ windows: [win({ id: "m-1" }), win({ id: "m-2", reason: "dns migration" })] });
    renderPage();
    await waitFor(() => expect(screen.getAllByTestId("echart").length).toBeGreaterThan(0));
    await waitFor(() =>
      expect(screen.getAllByTestId("echart").every((el) => el.getAttribute("data-maintenance") === "m-1,m-2")).toBe(true),
    );
  });

  it("renders ONE bar for the page rather than one per chart", async () => {
    stubFetch();
    renderPage();
    await waitFor(() => expect(screen.getAllByTestId("maintenance-bar")).toHaveLength(1));
  });

  it("declares a GLOBAL window from the page's own affordance", async () => {
    const { createBodies } = stubFetch();
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ maintenance" }));
    fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: "fleet-wide switch upgrade" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect(createBodies[0]).toEqual({
      scope: "",
      startAt: "2026-08-01T12:00:00.000Z",
      endAt: "2026-08-01T13:00:00.000Z",
      reason: "fleet-wide switch upgrade",
    });
  });

  it("hides the create affordance for a subject without maintenance:write", async () => {
    stubFetch({ permissions: ["maintenance:read"] });
    renderPage();
    await screen.findByTestId("maintenance-bar");
    expect(screen.queryByRole("button", { name: "＋ maintenance" })).toBeNull();
  });

  it("shows no bar and makes ZERO requests without maintenance:read", async () => {
    const { maintenanceQueries } = stubFetch({ permissions: ["annotations:read"] });
    renderPage();
    // The annotation bar mounting is the proof the page rendered, so "zero"
    // means zero rather than "not yet".
    await screen.findByTestId("annotation-bar");
    expect(maintenanceQueries).toEqual([]);
    expect(screen.queryByTestId("maintenance-bar")).toBeNull();
  });

  it("anchors the window at t while engaged, and disables create", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    const { maintenanceQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(maintenanceQueries.length).toBeGreaterThan(0));
    expect(maintenanceQueries[0].get("to")).toBe("2026-08-01T09:00:00.000Z");
    expect(maintenanceQueries[0].get("from")).toBe("2026-08-01T08:00:00.000Z");
    expect(await screen.findByRole("button", { name: "＋ maintenance" })).toBeDisabled();
  });
});
