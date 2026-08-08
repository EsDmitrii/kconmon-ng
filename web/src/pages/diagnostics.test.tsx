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

function renderPage(opts: {
  permissions?: string[];
  nodes?: string[];
  databaseConfigured?: boolean;
  runs?: unknown[];
  targets?: unknown[];
  onCreate?: (body: unknown) => Response;
  onSaveCheck?: (body: unknown) => Response;
  /** URL-aware GET /api/v1/runs stub (the stubEventsFetch convention from
   * pages/live.test.tsx), for tests that need cursor-dependent pages. Takes
   * precedence over the static `runs` list when supplied. */
  onRuns?: (qs: URLSearchParams) => Response;
}) {
  const {
    permissions = ["runs:create"],
    nodes = ["a", "b"],
    databaseConfigured = true,
    runs = [],
    targets = [targetRow()],
    onCreate,
    onSaveCheck,
    onRuns,
  } = opts;
  const createCalls: unknown[] = [];
  const checkCalls: unknown[] = [];
  const urls: string[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    urls.push(href);
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(databaseConfigured)));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topologyBody(nodes)));
    if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets, nextCursor: "" }));
    if (href === "/api/v1/checks" && method === "POST") {
      const body: unknown = JSON.parse(String(init?.body ?? "{}"));
      checkCalls.push(body);
      if (onSaveCheck) return Promise.resolve(onSaveCheck(body));
      return Promise.resolve(json({ ...(body as object), id: "d-1" }, { status: 201 }));
    }
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
  return { ...utils, fetchMock, createCalls, checkCalls, urls, qc, navigateSpy };
}

const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/** OPERATOR is a subject who can start runs, read targets and write check
 *  definitions — everything the form v2 can surface at once. */
const OPERATOR = ["runs:create", "targets:read", "checks:read", "checks:write"];

async function pickDestination(name: RegExp) {
  fireEvent.click(await screen.findByRole("radio", { name }));
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

  // The regression that matters most in form v2: a NODE run's body must stay
  // the pre-M4 four keys, in the pre-M4 order, with no destinationKind:"node"
  // added -- the server treats "" and "node" identically, so serialising one
  // would be a new field on the wire for the overwhelmingly common path.
  it("a node run's body is byte-identical to the pre-M4 shape -- no destination* keys at all", async () => {
    const { createCalls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: "TCP" });
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(createCalls).toHaveLength(1));
    expect(Object.keys(createCalls[0] as object)).toEqual(["type", "plane", "sources", "destinations"]);
    expect(JSON.stringify(createCalls[0])).toBe(
      JSON.stringify({ type: "tcp", plane: "pod", sources: [], destinations: [] }),
    );
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

describe("DiagnosticsPage destination kinds", () => {
  it("posts destinationKind target with the picked target id, and empty node destinations", async () => {
    const { createCalls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"], targets: [targetRow()] });

    await pickDestination(/^target$/i);
    const picker = await screen.findByLabelText("Destination target");
    await waitFor(() => expect(within(picker).getByRole("option", { name: "api-gw" })).toBeInTheDocument());
    fireEvent.change(picker, { target: { value: "t-1" } });
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(createCalls).toHaveLength(1));
    // destinations: [] is required by the server for an external kind -- one
    // run probes either the mesh or one external destination, never a mix.
    expect(createCalls[0]).toEqual({
      type: "tcp",
      plane: "pod",
      sources: [],
      destinations: [],
      destinationKind: "target",
      destinationTargetId: "t-1",
    });
  });

  it("posts destinationKind adhoc with the typed address", async () => {
    const { createCalls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    fireEvent.change(screen.getByLabelText("Destination address"), { target: { value: "10.0.0.9" } });
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(createCalls).toHaveLength(1));
    expect(createCalls[0]).toEqual({
      type: "tcp",
      plane: "pod",
      sources: [],
      destinations: [],
      destinationKind: "adhoc",
      destinationAddress: "10.0.0.9",
    });
  });

  it("blocks submit while the external destination is incomplete, rather than collecting a 400", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    expect(screen.getByRole("button", { name: /start run/i })).toBeDisabled();

    fireEvent.change(screen.getByLabelText("Destination address"), { target: { value: "10.0.0.9" } });
    expect(screen.getByRole("button", { name: /start run/i })).toBeEnabled();
  });

  it("swaps the node destination checkboxes for the external field and back", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    expect(await screen.findByRole("group", { name: /destinations/i })).toBeInTheDocument();

    await pickDestination(/ad-hoc/i);
    expect(screen.queryByRole("group", { name: /destinations/i })).not.toBeInTheDocument();
    expect(screen.getByLabelText("Destination address")).toBeInTheDocument();

    await pickDestination(/nodes/i);
    expect(screen.getByRole("group", { name: /destinations/i })).toBeInTheDocument();
    expect(screen.queryByLabelText("Destination address")).not.toBeInTheDocument();
  });

  it("offers no Target option without targets:read, and never asks for the target list", async () => {
    const { urls } = renderPage({ permissions: ["runs:create"], nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: /nodes/i });
    expect(screen.queryByRole("radio", { name: /^target$/i })).not.toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /ad-hoc/i })).toBeInTheDocument();
    expect(urls.some((u) => u.startsWith("/api/v1/targets"))).toBe(false);
  });

  it("asks for the target list only once Target is actually selected", async () => {
    const { urls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: /nodes/i });
    expect(urls.some((u) => u.startsWith("/api/v1/targets"))).toBe(false);

    await pickDestination(/^target$/i);
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/targets"))).toBe(true));
  });
});

describe("DiagnosticsPage save as definition", () => {
  it("is absent without checks:write", async () => {
    renderPage({ permissions: ["runs:create"], nodes: ["a", "b"] });

    await screen.findByRole("button", { name: /start run/i });
    expect(screen.queryByRole("button", { name: /save as definition/i })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Definition name")).not.toBeInTheDocument();
  });

  it("posts the form as a check definition, carrying the ad-hoc destination over", async () => {
    const { checkCalls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    fireEvent.click(screen.getByRole("radio", { name: "HTTP" }));
    fireEvent.change(screen.getByLabelText("Destination address"), { target: { value: "https://example.test" } });
    fireEvent.change(screen.getByLabelText("Definition name"), { target: { value: "edge-http" } });
    fireEvent.click(screen.getByRole("button", { name: /save as definition/i }));

    await waitFor(() => expect(checkCalls).toHaveLength(1));
    expect(checkCalls[0]).toEqual({
      name: "edge-http",
      // A definition has no per-node source list -- "all" is the honest
      // translation, and the form says so.
      sourceSelection: "all",
      destinationKind: "adhoc",
      destinationAddress: "https://example.test",
      checkType: "http",
      plane: "pod",
      enabled: true,
    });
    expect(await screen.findByText(/saved definition/i)).toBeInTheDocument();
  });

  it("refuses to post a nameless definition, and says so at the field", async () => {
    const { checkCalls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    fireEvent.click(await screen.findByRole("button", { name: /save as definition/i }));

    expect(await screen.findByText(/a definition needs a name/i)).toBeInTheDocument();
    expect(checkCalls).toHaveLength(0);
    expect(screen.getByLabelText("Definition name")).toHaveAttribute("aria-invalid", "true");
  });

  it("renders a 422 inline at the field the server's own detail names", async () => {
    const detail = 'definition: name "edge-http" is already taken; definition names are unique';
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"], onSaveCheck: () => problem(422, "invalid definition", detail) });

    fireEvent.change(await screen.findByLabelText("Definition name"), { target: { value: "edge-http" } });
    fireEvent.click(screen.getByRole("button", { name: /save as definition/i }));

    expect(await screen.findByText(detail)).toBeInTheDocument();
    expect(screen.getByLabelText("Definition name")).toHaveAttribute("aria-invalid", "true");
  });

  // The projection 422 names no field at all -- it is about the definition as
  // a whole -- and must still render verbatim rather than be swallowed.
  it("renders a field-less 422 (the projection guard) as a form-level message, verbatim", async () => {
    const detail =
      "definition: too many projected series: enabling this definition projects 900 continuous external series (900 agents x 1 protocols), limit 400";
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"], onSaveCheck: () => problem(422, "invalid definition", detail) });

    fireEvent.change(await screen.findByLabelText("Definition name"), { target: { value: "edge-http" } });
    fireEvent.click(screen.getByRole("button", { name: /save as definition/i }));

    expect(await screen.findByText(detail)).toBeInTheDocument();
    expect(screen.getByLabelText("Definition name")).not.toHaveAttribute("aria-invalid");
  });

  it("does not render the M6 'attach to incident' action", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await screen.findByRole("button", { name: /start run/i });
    expect(screen.queryByRole("button", { name: /incident/i })).not.toBeInTheDocument();
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
