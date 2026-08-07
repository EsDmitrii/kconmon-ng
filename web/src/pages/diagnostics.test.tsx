import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { DiagnosticsPage, estimatePairCount, estimateRawPairCount } from "./diagnostics";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

function topologyBody(names: string[]) {
  return { nodes: names.map((n) => ({ name: n, zone: "z1", ready: true })), agents: [], timestamp: "t" };
}

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

function runRow(id: string, overrides: Partial<{ status: string; type: string }> = {}) {
  return {
    id,
    createdAt: "2026-01-01T00:00:00Z",
    status: "succeeded",
    type: "tcp",
    plane: "pod",
    initiatorKind: "user",
    initiatorId: "u1",
    pairTotal: 1,
    pairOk: 1,
    pairFailed: 0,
    ...overrides,
  };
}

function renderPage(opts: {
  permissions?: string[];
  nodes?: string[];
  databaseConfigured?: boolean;
  runs?: unknown[];
  onCreate?: (body: unknown) => Response;
  /** URL-aware GET /api/v1/runs stub (the stubEventsFetch convention from
   * pages/live.test.tsx), for tests that need cursor-dependent pages. Takes
   * precedence over the static `runs` list when supplied. */
  onRuns?: (qs: URLSearchParams) => Response;
}) {
  const { permissions = ["runs:create"], nodes = ["a", "b"], databaseConfigured = true, runs = [], onCreate, onRuns } = opts;
  const createCalls: unknown[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(databaseConfigured)));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topologyBody(nodes)));
    if (href === "/api/v1/runs" && method === "POST") {
      const body: unknown = JSON.parse(String(init?.body ?? "{}"));
      createCalls.push(body);
      if (onCreate) return Promise.resolve(onCreate(body));
      return Promise.resolve(
        json({ id: "run-xyz", status: "pending", pairTotal: 1, wsTopic: "run:run-xyz" }, { status: 202 }),
      );
    }
    if (href.startsWith("/api/v1/runs")) {
      if (onRuns) return Promise.resolve(onRuns(new URLSearchParams(href.split("?")[1] ?? "")));
      return Promise.resolve(json({ runs, nextCursor: "" }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  // Every renderPage caller can reach a submit that calls goTo -- stub the
  // navigate seam unconditionally (jsdom has no real navigation), and hand
  // the spy back so the one test that cares about the destination can
  // assert on it directly.
  const navigateSpy = vi.fn();
  setNavigateForTest(navigateSpy);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <DiagnosticsPage />
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock, createCalls, qc, navigateSpy };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  resetNavigateForTest();
});

describe("estimatePairCount", () => {
  it("is the deduplicated cross product minus self-pairs", () => {
    expect(estimatePairCount(["a", "b"], ["a", "b"])).toBe(2); // a->b, b->a
    expect(estimatePairCount(["a"], ["a"])).toBe(0);
    expect(estimatePairCount(["a", "b"], ["c"])).toBe(2);
  });

  it("dedupes each side independently", () => {
    expect(estimatePairCount(["a", "a", "b"], ["c"])).toBe(2);
  });
});

describe("estimateRawPairCount", () => {
  it("is the raw, pre-dedup, pre-self-pair-exclusion product -- mirrors checks.Plan's own first gate", () => {
    expect(estimateRawPairCount(["a", "b"], ["a", "b"])).toBe(4);
    expect(estimateRawPairCount(["a"], ["a"])).toBe(1);
  });

  // The scenario from the review: 20 sources and 21 destinations sharing 20
  // names. Raw product is 420 (over the 400 limit); the self-excluded
  // display estimate collapses to exactly 400 (one self-pair per shared
  // name), AT the limit. The server (checks.go's Plan) rejects on the raw
  // product BEFORE self-pair exclusion, so the two must disagree here on
  // purpose -- that disagreement is exactly what estimateRawPairCount exists
  // to catch.
  it("disagrees with estimatePairCount on an asymmetric overlapping selection (20x21 sharing 20 names)", () => {
    const shared = Array.from({ length: 20 }, (_, i) => `n${i}`);
    const sources = shared;
    const destinations = [...shared, "n20"];
    expect(estimateRawPairCount(sources, destinations)).toBe(420);
    expect(estimatePairCount(sources, destinations)).toBe(400);
  });
});

describe("DiagnosticsPage form", () => {
  it("renders the form and submits the right body", async () => {
    const { createCalls } = renderPage({ nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: "TCP" });
    fireEvent.click(screen.getByRole("radio", { name: "UDP" }));
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(createCalls).toHaveLength(1));
    expect(createCalls[0]).toEqual({ type: "udp", plane: "pod", sources: [], destinations: [] });
  });

  it("previews the pair count and disables submit above the 400-pair limit", async () => {
    const manyNodes = Array.from({ length: 25 }, (_, i) => `n${i}`); // 25*24=600 > 400
    renderPage({ nodes: manyNodes });

    await waitFor(() => expect(screen.getByText(/600 pairs/)).toBeInTheDocument());
    expect(screen.getByText(/above the 400-pair limit/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /start run/i })).toBeDisabled();
  });

  it("keeps submit enabled and shows a small pair count for a small topology", async () => {
    renderPage({ nodes: ["a", "b"] });
    await waitFor(() => expect(screen.getByText(/2 pairs/)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /start run/i })).toBeEnabled();
  });

  // Mirrors the server's own gate exactly (checks.go's Plan): the raw S×D
  // product, not the self-excluded count. 20 of 21 nodes as sources, all 21
  // as destinations -- raw 20*21=420 > 400, but self-pair exclusion (every
  // source also appears in destinations) brings the displayed estimate down
  // to exactly 400, AT the limit. Without the raw-product gate this
  // selection would read as fine and submit, only to 422 against the
  // server's own ErrTooManyPairs.
  it("disables submit on an asymmetric overlapping selection whose raw product exceeds 400 even though the displayed estimate does not", async () => {
    const nodes = Array.from({ length: 21 }, (_, i) => `n${i}`); // n0..n20
    renderPage({ nodes });

    const sourcesGroup = await screen.findByRole("group", { name: /sources/i });
    fireEvent.click(within(sourcesGroup).getByLabelText(/all nodes/i));
    for (let i = 0; i < 20; i++) {
      fireEvent.click(within(sourcesGroup).getByLabelText(`n${i}`));
    }
    // Destinations stay at "All nodes" (21).

    await waitFor(() => expect(screen.getByText(/above the 400-pair limit/i)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /start run/i })).toBeDisabled();
  });

  it("a successful submit (202) navigates to the run permalink", async () => {
    const { navigateSpy } = renderPage({ nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: "TCP" });
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/diagnostics/runs/run-xyz"));
  });

  it("hides the form entirely without runs:create, and explains why", async () => {
    renderPage({ permissions: [], nodes: ["a", "b"] });

    await waitFor(() => expect(screen.getByText(/runs:create/)).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: /start run/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: "TCP" })).not.toBeInTheDocument();
  });

  it("the all <-> all shortcut and the individual node checkboxes are independently scoped per column", async () => {
    renderPage({ nodes: ["a", "b"] });
    const sourcesGroup = await screen.findByRole("group", { name: /sources/i });
    const destGroup = screen.getByRole("group", { name: /destinations/i });
    expect(within(sourcesGroup).getByLabelText(/all nodes/i)).toBeChecked();
    expect(within(destGroup).getByLabelText(/all nodes/i)).toBeChecked();
  });
});

describe("DiagnosticsPage history", () => {
  it("shows the non-persistence note when database.configured is false", async () => {
    renderPage({ nodes: ["a", "b"], databaseConfigured: false });
    expect(await screen.findByText(/history is not persisted/i)).toBeInTheDocument();
    expect(screen.getByText(/console\.database\.mode/)).toBeInTheDocument();
  });

  it("does not show the note when the database is configured", async () => {
    renderPage({ nodes: ["a", "b"], databaseConfigured: true });
    await screen.findByRole("heading", { name: /run history/i });
    expect(screen.queryByText(/history is not persisted/i)).not.toBeInTheDocument();
  });

  it("renders run rows with a link to the permalink", async () => {
    renderPage({
      nodes: ["a", "b"],
      runs: [
        {
          id: "run-1",
          createdAt: "2026-01-01T00:00:00Z",
          status: "succeeded",
          type: "tcp",
          plane: "pod",
          initiatorKind: "user",
          initiatorId: "u1",
          pairTotal: 2,
          pairOk: 2,
          pairFailed: 0,
        },
      ],
    });

    const link = await screen.findByRole("link", { name: "run-1" });
    expect(link).toHaveAttribute("href", "/diagnostics/runs/run-1");
  });

  it("'Load older' appends the next page via cursor and disables itself once nextCursor is empty", async () => {
    const page1 = { runs: [runRow("run-1")], nextCursor: "cursor-1" };
    const page2 = { runs: [runRow("run-2")], nextCursor: "" };
    renderPage({
      nodes: ["a", "b"],
      onRuns: (qs) => json(qs.get("cursor") === "cursor-1" ? page2 : page1),
    });

    await screen.findByRole("link", { name: "run-1" });
    const loadOlder = screen.getByRole("button", { name: "Load older" });
    expect(loadOlder).not.toBeDisabled();

    fireEvent.click(loadOlder);
    await screen.findByRole("link", { name: "run-2" });

    // Appended, not replaced -- page one's row is still there alongside page two's.
    expect(screen.getByRole("link", { name: "run-1" })).toBeInTheDocument();
    expect(loadOlder).toBeDisabled();
  });

  it("does not show 'Load older' when there is no history yet", async () => {
    renderPage({ nodes: ["a", "b"], runs: [] });
    await screen.findByText(/no runs yet/i);
    expect(screen.queryByRole("button", { name: "Load older" })).not.toBeInTheDocument();
  });
});
