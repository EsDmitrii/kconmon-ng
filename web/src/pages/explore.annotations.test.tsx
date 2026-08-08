import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { Annotation } from "@/lib/types";
import { ExplorePage } from "./explore";

/**
 * Explore's annotation overlay. Explore is a GLOBAL surface and only that —
 * these five charts are fleet-wide aggregates with no object identity — so the
 * scope assertions here are the load-bearing ones.
 *
 * EChart is mocked (echarts.init reaches for a 2d canvas context jsdom does not
 * implement); the mock re-publishes the annotations it was handed as a data
 * attribute, which is how a page test can see that the markers reached the
 * chart at all. The marker GEOMETRY is asserted where it is built, in
 * lib/annotations.test.ts.
 */
vi.mock("@/components/echart", () => ({
  EChart: ({ className, annotations }: { className?: string; annotations?: Annotation[] }) => (
    <div data-testid="echart" className={className} data-annotations={(annotations ?? []).map((a) => a.id).join(",")} />
  ),
}));

const NOW = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function ann(over: Partial<Annotation> = {}): Annotation {
  return {
    id: "a-1",
    startAt: "2026-08-01T11:30:00Z",
    scope: "",
    text: "rolled the gateway",
    createdBy: "user:ada",
    createdAt: "2026-08-01T11:30:01Z",
    ...over,
  };
}

function stubFetch(opts: { permissions?: string[]; annotations?: Annotation[] } = {}) {
  const { permissions = ["annotations:read", "annotations:write"], annotations = [] } = opts;
  const annotationQueries: URLSearchParams[] = [];
  const createBodies: unknown[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(
        json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions }),
      );
    }
    if (href.startsWith("/api/v1/annotations") && method === "POST") {
      createBodies.push(JSON.parse(String(init?.body ?? "{}")));
      return Promise.resolve(json(ann({ id: "new" })));
    }
    if (href.startsWith("/api/v1/annotations")) {
      annotationQueries.push(new URLSearchParams(href.split("?")[1] ?? ""));
      return Promise.resolve(json({ annotations, nextCursor: "" }));
    }
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
  return { fetchMock, annotationQueries, createBodies };
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

describe("ExplorePage annotations", () => {
  it("asks for the GLOBAL scope only — present-but-empty, never absent", async () => {
    const { annotationQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(annotationQueries.every((qs) => qs.has("scope"))).toBe(true);
    expect([...new Set(annotationQueries.map((qs) => qs.get("scope")))]).toEqual([""]);
  });

  it("bounds the fetch to the chart's visible range — 1h by default", async () => {
    const { annotationQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(annotationQueries[0].get("to")).toBe("2026-08-01T12:00:00.000Z");
    expect(annotationQueries[0].get("from")).toBe("2026-08-01T11:00:00.000Z");
  });

  it("re-asks over the NEW window when the range picker moves", async () => {
    const { annotationQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(spanSecondsOf(annotationQueries[0])).toBe(3600);
    fireEvent.click(screen.getByRole("radio", { name: "15m" }));
    // The picker's own seconds, not a fixed instant: vi's fake clock is set to
    // advance with real time here (shouldAdvanceTime), so the window's END
    // legitimately moves between the two requests while its LENGTH is the thing
    // the range picker actually controls.
    await waitFor(() => expect(annotationQueries.map(spanSecondsOf)).toContain(900));
  });

  it("hands the annotations to every chart so the markers land on all five", async () => {
    stubFetch({ annotations: [ann({ id: "a-1" }), ann({ id: "a-2", endAt: "2026-08-01T11:40:00Z" })] });
    renderPage();
    await waitFor(() => expect(screen.getAllByTestId("echart").length).toBeGreaterThan(0));
    await waitFor(() =>
      expect(screen.getAllByTestId("echart").every((el) => el.getAttribute("data-annotations") === "a-1,a-2")).toBe(true),
    );
  });

  it("renders ONE bar for the page rather than one per chart", async () => {
    stubFetch();
    renderPage();
    await waitFor(() => expect(screen.getAllByTestId("annotation-bar")).toHaveLength(1));
  });

  it("creates a GLOBAL annotation from the page's own affordance", async () => {
    const { createBodies } = stubFetch();
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    fireEvent.change(await screen.findByLabelText("Start"), { target: { value: "2026-08-01T11:30" } });
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "fleet-wide maintenance" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect(createBodies[0]).toEqual({
      startAt: new Date("2026-08-01T11:30").toISOString(),
      scope: "",
      text: "fleet-wide maintenance",
    });
  });

  it("hides the create affordance for a subject without annotations:write", async () => {
    stubFetch({ permissions: ["annotations:read"] });
    renderPage();
    await screen.findByTestId("annotation-bar");
    await waitFor(() => expect(screen.queryByRole("button", { name: /annotate/i })).toBeNull());
  });

  it("anchors the annotation window at t while engaged, and disables create", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    const { annotationQueries } = stubFetch();
    renderPage();
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(annotationQueries[0].get("to")).toBe("2026-08-01T09:00:00.000Z");
    expect(annotationQueries[0].get("from")).toBe("2026-08-01T08:00:00.000Z");
    expect(await screen.findByRole("button", { name: /annotate/i })).toBeDisabled();
  });
});
