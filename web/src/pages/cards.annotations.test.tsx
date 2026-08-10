import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { Annotation } from "@/lib/types";
import { NodeCardPage } from "./node-card";
import { PairCardPage } from "./pair-card";
import { TargetCardPage } from "./target-card";

/** The three object cards' annotation surfaces, and the one question they exist to answer. */
vi.mock("@/components/echart", () => ({
  EChart: ({ className, annotations }: { className?: string; annotations?: Annotation[] }) => (
    <div data-testid="echart" className={className} data-annotations={(annotations ?? []).map((a) => a.id).join(",")} />
  ),
}));

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const TARGET_ID = "11111111-1111-1111-1111-111111111111";

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

const topologyBody = {
  nodes: [{ name: "node-a", zone: "z1", ready: true }],
  agents: [{ id: "agent-1", nodeName: "node-a", podIP: "10.0.0.1", zone: "z1" }],
  timestamp: "t",
};

const matrixBody = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["node-a", "node-b"],
  cells: [{ source: "node-a", destination: "node-b", failRatio: 0, rttP95: 1_000_000 }],
  timestamp: "t",
};

const configBody = {
  auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
  anonymousBanner: false,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: true },
};

const targetBody = {
  id: TARGET_ID,
  name: "edge-gw",
  kind: "host",
  address: "10.10.0.1",
  labels: {},
  createdAt: "2026-08-01T00:00:00Z",
  updatedAt: "2026-08-01T00:00:00Z",
};

interface StubOpts {
  permissions?: string[];
  byScope?: Record<string, Annotation[]>;
}

function stubFetch(opts: StubOpts = {}) {
  // targets:read/checks:read are the target card's own gate; the annotation
  // permissions are what this file is actually about.
  const { permissions = ["annotations:read", "annotations:write", "targets:read", "checks:read"], byScope = {} } = opts;
  const annotationQueries: URLSearchParams[] = [];
  const createBodies: unknown[] = [];
  const deleted: string[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.7.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody));
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(
        json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions }),
      );
    }
    if (href.startsWith("/api/v1/annotations/") && method === "DELETE") {
      deleted.push(decodeURIComponent(href.slice("/api/v1/annotations/".length)));
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (href.startsWith("/api/v1/annotations") && method === "POST") {
      createBodies.push(JSON.parse(String(init?.body ?? "{}")));
      return Promise.resolve(json(ann({ id: "new" }), { status: 201 }));
    }
    if (href.startsWith("/api/v1/annotations")) {
      const qs = new URLSearchParams(href.split("?")[1] ?? "");
      annotationQueries.push(qs);
      const key = qs.has("scope") ? (qs.get("scope") as string) : " ";
      return Promise.resolve(json({ annotations: byScope[key] ?? [], nextCursor: "" }));
    }
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topologyBody));
    if (href.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/targets/")) return Promise.resolve(json(targetBody));
    if (href.startsWith("/api/v1/checks")) return Promise.resolve(json({ definitions: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/schedules")) return Promise.resolve(json({ schedules: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs: [], nextCursor: "" }));
    if (href.includes("/api/v1/promql/query_range")) {
      // A non-empty series, so the chart actually renders and can be asked
      // which annotations it was handed. An empty one takes the "no series"
      // branch and no chart is mounted at all.
      return Promise.resolve(
        json({
          status: "success",
          data: { resultType: "matrix", result: [{ metric: { protocol: "tcp" }, values: [[1785283200, "0.01"]] }] },
        }),
      );
    }
    if (href.includes("/api/v1/promql")) {
      return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { fetchMock, annotationQueries, createBodies, deleted };
}

function renderAt(pathname: string, node: React.ReactNode) {
  window.history.pushState({}, "", pathname);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>{node}</TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/** The set of scopes a surface asked for, as wire states rather than values:
 *  "absent" is a different REQUEST from "=" (present-but-empty). */
async function scopesAsked(queries: URLSearchParams[]): Promise<string[]> {
  await waitFor(() => expect(queries.length).toBeGreaterThanOrEqual(2));
  return [...new Set(queries.map((qs) => (qs.has("scope") ? `=${qs.get("scope")}` : "absent")))].sort();
}

function spanSecondsOf(qs: URLSearchParams): number {
  return (Date.parse(qs.get("to") ?? "") - Date.parse(qs.get("from") ?? "")) / 1000;
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("NodeCardPage annotations", () => {
  it("asks for the node's own scope AND the global one", async () => {
    const { annotationQueries } = stubFetch();
    renderAt("/nodes/node-a", <NodeCardPage />);
    expect(await scopesAsked(annotationQueries)).toEqual(["=", "=node-a"]);
  });

  it("looks back a day — this card has no chart window to inherit", async () => {
    const { annotationQueries } = stubFetch();
    renderAt("/nodes/node-a", <NodeCardPage />);
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(spanSecondsOf(annotationQueries[0])).toBe(86400);
  });

  it("renders both the node's notes and the fleet-wide ones", async () => {
    stubFetch({
      byScope: { "node-a": [ann({ id: "n", scope: "node-a", text: "drained node-a" })], "": [ann({ id: "g", text: "fleet note" })] },
    });
    renderAt("/nodes/node-a", <NodeCardPage />);
    await screen.findByText("drained node-a");
    await screen.findByText("fleet note");
  });

  it("creates against the node's scope, fixed", async () => {
    const { createBodies } = stubFetch();
    renderAt("/nodes/node-a", <NodeCardPage />);
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    fireEvent.change(await screen.findByLabelText("Note"), { target: { value: "cordoned" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect((createBodies[0] as { scope: string }).scope).toBe("node-a");
  });

  it("deletes a note from the card", async () => {
    const { deleted } = stubFetch({ byScope: { "node-a": [ann({ id: "doomed", scope: "node-a", text: "typo" })] } });
    renderAt("/nodes/node-a", <NodeCardPage />);
    // Two clicks: the row confirms before it destroys (QA round 2, #14).
    fireEvent.click(await screen.findByRole("button", { name: /^delete annotation/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^confirm delete annotation/i }));
    await waitFor(() => expect(deleted).toEqual(["doomed"]));
  });
});

describe("PairCardPage annotations", () => {
  it("asks for the pair scope — the U+2192 arrow the rest of the console uses — and the global one", async () => {
    const { annotationQueries } = stubFetch();
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    expect(await scopesAsked(annotationQueries)).toEqual(["=", "=node-a→node-b"]);
  });

  it("bounds the fetch to the chart's own hour", async () => {
    const { annotationQueries } = stubFetch();
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(spanSecondsOf(annotationQueries[0])).toBe(3600);
  });

  it("hands the marks to the chart", async () => {
    stubFetch({ byScope: { "node-a→node-b": [ann({ id: "p", scope: "node-a→node-b", text: "flap" })] } });
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    await waitFor(() => expect(screen.getByTestId("echart").getAttribute("data-annotations")).toBe("p"));
  });

  it("creates against the pair scope, fixed", async () => {
    const { createBodies } = stubFetch();
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    fireEvent.change(await screen.findByLabelText("Note"), { target: { value: "link flapped" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect((createBodies[0] as { scope: string }).scope).toBe("node-a→node-b");
  });

  it("hides create entirely for a subject with read but no write", async () => {
    stubFetch({ permissions: ["annotations:read"] });
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    await screen.findByTestId("annotation-bar");
    expect(screen.queryByRole("button", { name: /annotate/i })).toBeNull();
  });

  it("keeps create visible but disabled while the Time Machine is engaged", async () => {
    stubFetch();
    renderAt("/pairs/node-a/node-b?at=2026-08-01T09:00:00Z", <PairCardPage />);
    expect(await screen.findByRole("button", { name: /annotate/i })).toBeDisabled();
  });
});

describe("TargetCardPage annotations", () => {
  it("asks for the target's NAME as its scope, plus the global one", async () => {
    const { annotationQueries } = stubFetch();
    renderAt(`/targets/${TARGET_ID}`, <TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    expect(await scopesAsked(annotationQueries)).toEqual(["=", "=edge-gw"]);
  });

  it("bounds the fetch to the History chart's hour", async () => {
    const { annotationQueries } = stubFetch();
    renderAt(`/targets/${TARGET_ID}`, <TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(spanSecondsOf(annotationQueries[0])).toBe(3600);
  });

  it("creates against the target name, fixed", async () => {
    const { createBodies } = stubFetch();
    renderAt(`/targets/${TARGET_ID}`, <TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    fireEvent.change(await screen.findByLabelText("Note"), { target: { value: "provider maintenance" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect((createBodies[0] as { scope: string }).scope).toBe("edge-gw");
  });
});
