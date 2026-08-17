import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { DiagnosticsPage, estimatePairCount, estimateRawPairCount, RUN_DURATIONS } from "./diagnostics";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

function topologyBody(names: string[], agents: { nodeName: string; zone: string }[] = []) {
  return {
    nodes: names.map((n) => ({ name: n, zone: "z1", ready: true })),
    agents: agents.map((a, i) => ({ agentId: `a${i}`, nodeName: a.nodeName, zone: a.zone, ready: true })),
    timestamp: "t",
  };
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
  /** Agents the controller does NOT list as nodes. */
  agents?: { nodeName: string; zone: string }[];
  databaseConfigured?: boolean;
  runs?: unknown[];
  targets?: unknown[];
  onCreate?: (body: unknown) => Response;
  onSaveCheck?: (body: unknown) => Response;
  /** RFC 3339 instant to engage the Time Machine at, through the URL (its one
   *  carrier). */
  at?: string;
  /** URL-aware GET /api/v1/runs stub (the stubEventsFetch convention from
   * pages/live.test.tsx), for tests that need cursor-dependent pages. Takes
   * precedence over the static `runs` list when supplied. */
  onRuns?: (qs: URLSearchParams) => Response;
  /** Mounts a <LocaleProvider> above the page. Absent — every case but the ru
   *  smoke pin at the bottom of this file — there is no provider at all, which
   *  lib/i18n defines as English. */
  locale?: Locale;
}) {
  const {
    permissions = ["runs:create"],
    nodes = ["a", "b"],
    agents = [],
    databaseConfigured = true,
    runs = [],
    targets = [targetRow()],
    onCreate,
    onSaveCheck,
    at,
    onRuns,
    locale,
  } = opts;
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  window.history.pushState({}, "", at ? `/diagnostics?at=${at}` : "/diagnostics");
  const createCalls: unknown[] = [];
  const checkCalls: unknown[] = [];
  const urls: string[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    urls.push(href);
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(databaseConfigured)));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topologyBody(nodes, agents)));
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
  // Every renderPage caller can reach a submit that calls goTo.
  const navigateSpy = vi.fn();
  setNavigateForTest(navigateSpy);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = <DiagnosticsPage />;
  const utils = render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        {locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}
      </TimeMachineProvider>
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
  window.history.pushState({}, "", "/");
  /* vitest.setup.ts backs localStorage with one Map per test FILE — a locale
     left behind would flip every later case in this one. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
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

  // The server (checks.go's Plan) rejects on the raw product BEFORE self-pair exclusion.
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

  // The regression that matters most in form v2: a NODE run's body must stay the pre-M4 four keys,
  // in the pre-M4 order.
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

  // Without the raw-product gate this selection would read as fine and submit.
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

  /*
   * The owner's live report, pinned from the SENDING side; the form was never wrong to send it and
   * is not changed here.
   */
  it("sends several explicit sources with an empty destinations array when Destinations is left on All", async () => {
    const { createCalls } = renderPage({ nodes: ["a", "b", "c"] });

    const sourcesGroup = await screen.findByRole("group", { name: /sources/i });
    fireEvent.click(within(sourcesGroup).getByLabelText(/all nodes/i));
    fireEvent.click(within(sourcesGroup).getByLabelText("a"));
    fireEvent.click(within(sourcesGroup).getByLabelText("b"));
    // Destinations deliberately left at "All nodes".
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(createCalls).toHaveLength(1));
    expect(createCalls[0]).toEqual({ type: "tcp", plane: "pod", sources: ["a", "b"], destinations: [] });
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
    fireEvent.change(screen.getByLabelText(/^Destination (host|address)/), { target: { value: "10.0.0.9" } });
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

    fireEvent.change(screen.getByLabelText(/^Destination (host|address)/), { target: { value: "10.0.0.9" } });
    expect(screen.getByRole("button", { name: /start run/i })).toBeEnabled();
  });

  /* QA scope 4, findings #8 and #9. The estimate is sources x destinations;
     with the destination side unresolved there is no second factor, so "~10
     pairs" was a number for a run the server would refuse — and "~0 pairs" on
     its own is a dead button with no explanation. */
  it("estimates ZERO pairs, and says why, while no target is picked", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/target/i);
    await waitFor(() => expect(screen.getByText(/~0 pairs/)).toBeInTheDocument());
    expect(screen.getByText(/no target picked yet/i)).toBeInTheDocument();
    expect(screen.queryByText(/~2 pairs/)).not.toBeInTheDocument();
  });

  it("estimates ZERO pairs, and says why, while the ad-hoc address is empty", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    await waitFor(() => expect(screen.getByText(/~0 pairs/)).toBeInTheDocument());
    expect(screen.getByText(/no address typed yet/i)).toBeInTheDocument();

    // ...and the count comes back the moment the destination resolves.
    fireEvent.change(screen.getByLabelText(/^Destination (host|address)/), { target: { value: "10.0.0.9" } });
    await waitFor(() => expect(screen.getByText(/~2 pairs/)).toBeInTheDocument());
    expect(screen.queryByText(/no address typed yet/i)).not.toBeInTheDocument();
  });

  it("explains a zero estimate that comes from having no sources, the way /mtr's Runner does", async () => {
    renderPage({ permissions: OPERATOR, nodes: [] });

    await waitFor(() => expect(screen.getByText(/~0 pairs/)).toBeInTheDocument());
    expect(screen.getByText(/no sources to check from/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /start run/i })).toBeDisabled();
  });

  /* QA scope 4, finding #17 — one option is not a choice. */
  it("states the plane as a chip rather than a disabled one-option select", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: "TCP" });
    expect(screen.getByText("Plane")).toBeInTheDocument();
    expect(screen.getByText("pod")).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: /plane/i })).not.toBeInTheDocument();
  });

  it("swaps the node destination checkboxes for the external field and back", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    expect(await screen.findByRole("group", { name: /destinations/i })).toBeInTheDocument();

    await pickDestination(/ad-hoc/i);
    expect(screen.queryByRole("group", { name: /destinations/i })).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^Destination (host|address)/)).toBeInTheDocument();

    await pickDestination(/nodes/i);
    expect(screen.getByRole("group", { name: /destinations/i })).toBeInTheDocument();
    expect(screen.queryByLabelText(/^Destination (host|address)/)).not.toBeInTheDocument();
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
    fireEvent.change(screen.getByLabelText(/^Destination (host|address)/), { target: { value: "https://example.test" } });
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

/**
 * GET /api/v1/runs has no `to` parameter, so the list was showing runs that had not happened yet
 * under a banner naming a past instant.
 */
describe("DiagnosticsPage history under the Time Machine", () => {
  const AT = "2026-01-01T12:00:00Z";
  const before = { ...runRow("run-before"), createdAt: "2026-01-01T09:00:00Z" };
  const after = { ...runRow("run-after"), createdAt: "2026-01-01T15:00:00Z" };

  it("drops a run that started AFTER the viewed instant", async () => {
    renderPage({ nodes: ["a", "b"], runs: [after, before], at: AT });

    expect(await screen.findByRole("link", { name: "run-before" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "run-after" })).not.toBeInTheDocument();
  });

  it("states the limitation: the cut is client-side over the loaded pages", async () => {
    renderPage({ nodes: ["a", "b"], runs: [before], at: AT });

    expect(await screen.findByText(/GET \/api\/v1\/runs has no time filter/i)).toBeInTheDocument();
    expect(screen.getByText(/not reached by paging backwards/i)).toBeInTheDocument();
  });

  it("says nothing about the cut while Live", async () => {
    renderPage({ nodes: ["a", "b"], runs: [before, after] });

    await screen.findByRole("link", { name: "run-before" });
    expect(screen.getByRole("link", { name: "run-after" })).toBeInTheDocument();
    expect(screen.queryByText(/no time filter/i)).not.toBeInTheDocument();
  });

  it("distinguishes 'everything is later than t' from 'nobody has ever run one'", async () => {
    renderPage({ nodes: ["a", "b"], runs: [after], at: AT });

    expect(await screen.findByText(/no runs at or before the viewed instant/i)).toBeInTheDocument();
    expect(screen.queryByText(/no runs yet/i)).not.toBeInTheDocument();
  });
});

/** `topology.nodes` is the CONTROLLER's view and is empty on a console deployed without one. */
describe("DiagnosticsPage node pickers on a controller-less console", () => {
  it("lists the agents' own node names when the controller reports none", async () => {
    renderPage({
      nodes: [],
      agents: [
        { nodeName: "node-a", zone: "z1" },
        { nodeName: "node-b", zone: "z2" },
      ],
    });

    const sources = await screen.findByRole("group", { name: /sources/i });
    fireEvent.click(within(sources).getByLabelText(/all nodes/i));
    expect(within(sources).getByLabelText("node-a")).toBeInTheDocument();
    expect(within(sources).getByLabelText("node-b")).toBeInTheDocument();
  });

  it("counts a node present in BOTH lists once", async () => {
    renderPage({ nodes: ["node-a"], agents: [{ nodeName: "node-a", zone: "z1" }] });

    const sources = await screen.findByRole("group", { name: /sources/i });
    expect(within(sources).getByLabelText("All nodes (1)")).toBeInTheDocument();
  });
});

/** QA round 4, findings #15, #16 and #17. */
describe("DiagnosticsPage form affordances", () => {
  it("names All ↔ All as the reset it is", async () => {
    renderPage({ nodes: ["a", "b"] });

    const reset = await screen.findByRole("button", { name: "Reset both pickers to every node" });
    expect(reset).toHaveAttribute("title", "Reset both pickers to every node");
  });

  it("All ↔ All puts both pickers back to every node", async () => {
    renderPage({ nodes: ["a", "b"] });

    const sources = await screen.findByRole("group", { name: /sources/i });
    fireEvent.click(within(sources).getByLabelText(/all nodes/i));
    expect(within(sources).getByLabelText(/all nodes/i)).not.toBeChecked();

    fireEvent.click(screen.getByRole("button", { name: "Reset both pickers to every node" }));
    expect(within(screen.getByRole("group", { name: /sources/i })).getByLabelText(/all nodes/i)).toBeChecked();
  });

  it("gives each node checkbox the node's own name, through a real label association", async () => {
    renderPage({ nodes: ["node-a", "node-b"] });

    const sources = await screen.findByRole("group", { name: /sources/i });
    fireEvent.click(within(sources).getByLabelText(/all nodes/i));
    const box = within(sources).getByRole("checkbox", { name: "node-a" });
    // htmlFor/id, not proximity: the name survives a styling edit to the span.
    expect(box.id).not.toBe("");
    expect(sources.querySelector(`label[for="${box.id}"]`)).not.toBeNull();
  });

  it("names the ad-hoc address field", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    expect(screen.getByRole("textbox", { name: /^Destination host \(port optional\)$/ })).toBeInTheDocument();
  });

  it("lets the six-option check-type control wrap instead of overflowing a narrow card", async () => {
    renderPage({ nodes: ["a", "b"] });

    const group = await screen.findByRole("radiogroup", { name: "Check type" });
    expect(group.className).toContain("flex-wrap");
    // All six are still reachable — the overflow hid the last two.
    expect(within(group).getAllByRole("radio")).toHaveLength(6);
  });
});

/**
 * QA round 4, finding #10. A 422 banner outlived the field it was complaining
 * about, and survived a switch to a different destination MODE entirely.
 */
describe("DiagnosticsPage stale submit errors", () => {
  const refuse = () => problem(422, "invalid run", "destination address is not routable");

  it("clears the banner when the destination mode changes", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"], onCreate: refuse });

    await pickDestination(/ad-hoc/i);
    fireEvent.change(screen.getByLabelText(/^Destination (host|address)/), { target: { value: "10.0.0.9" } });
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));
    expect(await screen.findByText(/destination address is not routable/i)).toBeInTheDocument();

    await pickDestination(/nodes/i);
    await waitFor(() =>
      expect(screen.queryByText(/destination address is not routable/i)).not.toBeInTheDocument(),
    );
  });

  it("clears the banner when the check type changes", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"], onCreate: refuse });

    fireEvent.click(await screen.findByRole("button", { name: /start run/i }));
    expect(await screen.findByText(/destination address is not routable/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "UDP" }));
    await waitFor(() =>
      expect(screen.queryByText(/destination address is not routable/i)).not.toBeInTheDocument(),
    );
  });

  it("clears the banner when a node selection changes", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"], onCreate: refuse });

    fireEvent.click(await screen.findByRole("button", { name: /start run/i }));
    expect(await screen.findByText(/destination address is not routable/i)).toBeInTheDocument();

    const sources = screen.getByRole("group", { name: /sources/i });
    fireEvent.click(within(sources).getByLabelText(/all nodes/i));
    await waitFor(() =>
      expect(screen.queryByText(/destination address is not routable/i)).not.toBeInTheDocument(),
    );
  });
});

/* QA scope 4, finding #11. The filters ride the SERVER's own ?type=&status=
   (runs.go's handleRunsList) — a client-side pass over the loaded pages would
   quietly mean "of the runs already fetched". */
describe("DiagnosticsPage run history filters", () => {
  const runsQueries = (urls: string[]) =>
    urls.filter((u) => /^\/api\/v1\/runs(\?|$)/.test(u)).map((u) => new URLSearchParams(u.split("?")[1] ?? ""));

  it("asks for neither filter until one is picked", async () => {
    const { urls } = renderPage({ runs: [runRow("r-1")] });

    await screen.findByText("r-1");
    const q = runsQueries(urls);
    expect(q).toHaveLength(1);
    expect(q[0].has("type")).toBe(false);
    expect(q[0].has("status")).toBe(false);
  });

  it("sends ?type= and ?status= and re-asks from page ONE, replacing the list", async () => {
    const { urls } = renderPage({
      onRuns: (qs) =>
        json({
          runs: qs.get("type") === "mtr" ? [runRow("r-mtr", { type: "mtr" })] : [runRow("r-tcp")],
          nextCursor: "",
        }),
    });

    await screen.findByText("r-tcp");
    fireEvent.change(screen.getByLabelText(/filter runs by check type/i), { target: { value: "mtr" } });
    expect(await screen.findByText("r-mtr")).toBeInTheDocument();
    // Replaced, not appended: a filtered page one is a new list.
    expect(screen.queryByText("r-tcp")).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(/filter runs by status/i), { target: { value: "failed" } });
    await waitFor(() => {
      const last = runsQueries(urls).at(-1);
      expect(last?.get("type")).toBe("mtr");
      expect(last?.get("status")).toBe("failed");
      // Page one, never the cursor the previous filter left behind.
      expect(last?.has("cursor")).toBe(false);
    });
  });

  it("says a filter matched nothing rather than claiming the history is empty", async () => {
    renderPage({ onRuns: (qs) => json({ runs: qs.get("status") ? [] : [runRow("r-1")], nextCursor: "" }) });

    await screen.findByText("r-1");
    fireEvent.change(screen.getByLabelText(/filter runs by status/i), { target: { value: "cancelled" } });

    expect(await screen.findByText(/no runs match these filters/i)).toBeInTheDocument();
    expect(screen.queryByText(/no runs yet/i)).not.toBeInTheDocument();
  });
});

/* QA scope 4, finding #10. One field with one label, one placeholder and no
   hint served all six check types, and a value typed for one survived a switch
   to another in silence. The vocabulary and the verdict now both follow the
   type, and the rules are the agent's own (internal/agent/tasks.go). */
describe("DiagnosticsPage — the external destination follows the check type", () => {
  it("labels and hints tcp, udp and icmp differently, because the agent treats them differently", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    // tcp: the port is optional and defaults to 80.
    expect(screen.getByLabelText(/^Destination host \(port optional\)/)).toBeInTheDocument();
    expect(screen.getByText(/Without a port the agent dials 80/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "UDP" }));
    expect(screen.getByLabelText(/^Destination host:port/)).toBeInTheDocument();
    expect(screen.getByText(/udp has no default/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "ICMP" }));
    expect(screen.getByLabelText(/^Destination host$/)).toBeInTheDocument();
    expect(screen.getByText(/There are no ports here/i)).toBeInTheDocument();
  });

  it("carries a per-type placeholder rather than one example that fits four types badly", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    expect(screen.getByLabelText(/^Destination host/)).toHaveAttribute("placeholder", "example.test or 10.0.0.1:8443");

    fireEvent.click(screen.getByRole("radio", { name: "ICMP" }));
    expect(screen.getByLabelText(/^Destination host/)).toHaveAttribute("placeholder", "example.test or 10.0.0.1");
  });

  it("KEEPS the typed value across a type switch and shows the mismatch at once", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    fireEvent.change(screen.getByLabelText(/^Destination host/), { target: { value: "10.0.0.1" } });
    expect(screen.getByRole("button", { name: /start run/i })).toBeEnabled();

    // udp has no default port, so the very same address is now unrunnable —
    // and the value is still there to be corrected, not silently dropped.
    fireEvent.click(screen.getByRole("radio", { name: "UDP" }));
    expect(screen.getByLabelText(/^Destination host/)).toHaveValue("10.0.0.1");
    expect(screen.getByRole("alert")).toHaveTextContent(/udp has no default port/i);
    expect(screen.getByRole("button", { name: /start run/i })).toBeDisabled();

    fireEvent.change(screen.getByLabelText(/^Destination host/), { target: { value: "10.0.0.1:53" } });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /start run/i })).toBeEnabled();
  });

  it("says outright that dns and http have no external destination at all", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    fireEvent.click(screen.getByRole("radio", { name: "DNS" }));
    expect(screen.getByRole("alert")).toHaveTextContent(/tcp, udp, icmp and mtr only/i);
    expect(screen.getByRole("button", { name: /start run/i })).toBeDisabled();

    fireEvent.click(screen.getByRole("radio", { name: "HTTP" }));
    expect(screen.getByRole("alert")).toHaveTextContent(/tcp, udp, icmp and mtr only/i);
  });

  it("refuses a URL for a type that dials the string itself", async () => {
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    fireEvent.change(screen.getByLabelText(/^Destination host/), { target: { value: "https://example.test/health" } });
    expect(screen.getByRole("alert")).toHaveTextContent(/a scheme and a path are never read/i);
    expect(screen.getByRole("button", { name: /start run/i })).toBeDisabled();
  });
});

/** Saving a definition PERSISTS the address; until the store learned to check it. */
describe("DiagnosticsPage ad-hoc address validation", () => {
  it("refuses to POST a definition carrying an address the agent could never dial", async () => {
    const { checkCalls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    fireEvent.change(screen.getByLabelText(/^Destination (host|address)/), { target: { value: "sdfsdfsdf !!" } });
    fireEvent.change(screen.getByLabelText("Definition name"), { target: { value: "edge-http" } });
    fireEvent.click(screen.getByRole("button", { name: /save as definition/i }));

    /* TWO now, not one: since QA scope 4's finding #10 the RUN form judges the
       address live as well, in the same sentence, so this one value is refused
       once above the button and once on the definition. */
    expect(await screen.findAllByText(/must be a host, an IP, host:port, or an http\(s\) URL/i)).toHaveLength(2);
    expect(checkCalls).toHaveLength(0);
  });

  it("lets a well-formed address through untouched", async () => {
    const { checkCalls } = renderPage({ permissions: OPERATOR, nodes: ["a", "b"] });

    await pickDestination(/ad-hoc/i);
    fireEvent.change(screen.getByLabelText(/^Destination (host|address)/), { target: { value: "example.test:8443" } });
    fireEvent.change(screen.getByLabelText("Definition name"), { target: { value: "edge-tcp" } });
    fireEvent.click(screen.getByRole("button", { name: /save as definition/i }));

    await waitFor(() => expect(checkCalls).toHaveLength(1));
    expect((checkCalls[0] as { destinationAddress: string }).destinationAddress).toBe("example.test:8443");
  });
});

/* ── duration selector (Task 2) ─────────────────────────────────────────── */

describe("DiagnosticsPage duration selector", () => {
  // Instant is the default and must stay wire-invisible: the node run's body
  // is a deliberate regression surface, and a `durationNs: 0` on it would be a
  // new key on the overwhelmingly common path.
  it("defaults to Instant and sends no durationNs at all", async () => {
    const { createCalls } = renderPage({ nodes: ["a", "b"] });

    expect(await screen.findByRole("radio", { name: "Instant" })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(createCalls).toHaveLength(1));
    expect(Object.keys(createCalls[0] as object)).toEqual(["type", "plane", "sources", "destinations"]);
  });

  it("sends durationNs for a picked duration", async () => {
    const { createCalls } = renderPage({ nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: "Instant" });
    fireEvent.click(screen.getByRole("radio", { name: "15m" }));
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(createCalls).toHaveLength(1));
    expect(createCalls[0]).toEqual({
      type: "tcp",
      plane: "pod",
      sources: [],
      destinations: [],
      durationNs: 900_000_000_000,
    });
  });

  // The caption is the operator's only warning that picking "24h" starts a day
  // of fleet traffic, so it must state the derived cadence and sample count --
  // and it must agree with what the server will actually do.
  it("explains the derived cadence and sample count for the chosen duration", async () => {
    renderPage({ nodes: ["a", "b"] });

    await screen.findByRole("radio", { name: "Instant" });
    expect(screen.getByText(/one probe per pair, right now/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "1m" }));
    // 60s / 500 floors to the 5s minimum -> 12 samples.
    await waitFor(() =>
      expect(screen.getByText(/probed every 5s for 1m — about 12 samples per pair/i)).toBeInTheDocument(),
    );

    fireEvent.click(screen.getByRole("radio", { name: "24h" }));
    /* 86400s / 500 = 172.8s -> "173s", capped at 500 samples. Said in SECONDS, not
       rounded up to "3m": the caption reads the same formatter the run permalink's
       cadence tile does, so the two cannot quote different numbers for one run. */
    await waitFor(() =>
      expect(screen.getByText(/probed every 173s for 24h — about 500 samples per pair/i)).toBeInTheDocument(),
    );
  });

  // Every offered option must sit inside the server's own accepted window, so
  // the UI can never lead an operator into the 422 it defines.
  it("offers only durations the server accepts", () => {
    const MIN_NS = 10_000_000_000;
    const MAX_NS = 86_400_000_000_000;
    for (const d of RUN_DURATIONS) {
      if (d.ns === 0) continue;
      expect(d.ns).toBeGreaterThanOrEqual(MIN_NS);
      expect(d.ns).toBeLessThanOrEqual(MAX_NS);
    }
    expect(RUN_DURATIONS[0].ns).toBe(0);
  });
});

/* the Russian is wired ONE smoke pin. */
describe("DiagnosticsPage — Russian", () => {
  it("renders the duration selector and its honesty caption in Russian", async () => {
    renderPage({ locale: "ru", permissions: OPERATOR });

    expect(await screen.findByRole("heading", { name: "Диагностика" })).toBeInTheDocument();

    // The duration control: its own name, its Instant option, and the caption
    // that says what Instant actually does.
    const duration = screen.getByRole("radiogroup", { name: "Длительность" });
    expect(within(duration).getByRole("radio", { name: "Мгновенно" })).toBeInTheDocument();
    expect(screen.getByText("По одному зонду на пару, прямо сейчас.")).toBeInTheDocument();

    // Pick an interval: the caption must now spell out the fan-out, cadence and
    // sample count — the same warning the English gives, at the same strength.
    fireEvent.click(within(duration).getByRole("radio", { name: "1m" }));
    const caption = await screen.findByText(/Каждая пара зондируется раз в/);
    /* The cadence is a rendered SPAN and follows the interface language. In PROSE it is spelled
       out — «раз в 5 секунд», never «раз в 5 с», where a bare «с» is read as the preposition and
       the sentence breaks on the one word it exists to say. The {label} beside it stays "1m":
       that is the selector's own range token, not prose. */
    expect(caption.textContent).toMatch(/раз в 5 секунд /);
    expect(caption.textContent).not.toMatch(/раз в 5 с[^ек]/);
    expect(caption.textContent).not.toMatch(/раз в 5s/);
    expect(caption.textContent).toMatch(/на протяжении 1m/);
    expect(caption.textContent).toMatch(/проб на пару/);
    expect(caption.textContent).toMatch(/остаётся отменяемым/);

    // 24h widens the cadence to 172.8s, and the widened span localises too: «173 секунды».
    fireEvent.click(within(duration).getByRole("radio", { name: "24h" }));
    await waitFor(() => expect(screen.getByText(/раз в 173 секунды /)).toBeInTheDocument());

    // Two nodes → two pairs → «~2 пары», the few form a two-form language
    // would render as «~2 пар».
    expect(screen.getByText("~2 пары")).toBeInTheDocument();
  });
});

/* ── the owner's rule: every list on every page is paged ────────────────── */

describe("the run history is PAGED", () => {
  const runRows = (n: number) =>
    Array.from({ length: n }, (_, i) => ({
      id: `run-${String(i).padStart(3, "0")}`,
      createdAt: "2026-01-01T00:00:00Z",
      status: "succeeded",
      type: "tcp",
      plane: "pod",
      initiatorKind: "user",
      initiatorId: "u1",
      pairTotal: 2,
      pairOk: 2,
      pairFailed: 0,
    }));

  it("shows one page-worth and says how much of the history that is", async () => {
    renderPage({ nodes: ["a", "b"], runs: runRows(120) });

    expect(await screen.findByRole("link", { name: "run-000" })).toBeInTheDocument();
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 10 of 120 runs");
    expect(screen.queryByRole("link", { name: "run-060" })).not.toBeInTheDocument();
  });

  it("reaches the older runs, newest-first order intact", async () => {
    renderPage({ nodes: ["a", "b"], runs: runRows(120) });
    expect(await screen.findByRole("link", { name: "run-000" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(screen.getByRole("link", { name: "run-010" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "run-009" })).not.toBeInTheDocument();
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 2 of 12");
  });

  it("leaves a short history without a pager to operate", async () => {
    renderPage({ nodes: ["a", "b"], runs: runRows(4) });
    expect(await screen.findByRole("link", { name: "run-000" })).toBeInTheDocument();
    expect(screen.queryByTestId("pager")).not.toBeInTheDocument();
  });
});

/* ── the sample-interval control ───────────────────────────────────────────
   The cadence used to be underivable by anyone and un-dialable by everyone:
   checks.EffectiveSampleInterval's own doc said the base cadence "is not
   something an operator can dial", and three surfaces each reported a
   different number for it. These cover the control that closed that, and the
   one sentence it now earns. */
describe("DiagnosticsPage sample interval", () => {
  it("offers no cadence control at all for an instant run", async () => {
    renderPage({ nodes: ["a", "b"] });
    await screen.findByRole("radio", { name: "Instant" });
    // An instant run has no cadence, and the server refuses a body that names one.
    expect(screen.queryByTestId("sample-interval")).not.toBeInTheDocument();
  });

  it("offers Auto plus every preset that FITS the run, and nothing longer", async () => {
    renderPage({ nodes: ["a", "b"] });
    await screen.findByRole("radio", { name: "Instant" });

    fireEvent.click(screen.getByRole("radio", { name: "1m" }));
    const control = await screen.findByRole("radiogroup", { name: "Sample interval" });
    for (const label of ["Auto", "1s", "5s", "15s", "30s", "1m"]) {
      expect(within(control).getByRole("radio", { name: label })).toBeInTheDocument();
    }
    // A cadence longer than the run is a 422; the form does not lead an operator into one.
    expect(within(control).queryByRole("radio", { name: "5m" })).not.toBeInTheDocument();
    expect(within(control).queryByRole("radio", { name: "15m" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "1h" }));
    await waitFor(() =>
      expect(
        within(screen.getByRole("radiogroup", { name: "Sample interval" })).getByRole("radio", { name: "15m" }),
      ).toBeInTheDocument(),
    );
  });

  /* One submit per render: a successful create navigates to the run permalink and the submit guard
     stays closed behind it, which is the form's own contract (hooks/use-submit-guard.ts). */
  async function submitWith(interval?: string): Promise<Record<string, unknown>> {
    const bodies: unknown[] = [];
    const capture = (body: unknown) => {
      bodies.push(body);
      return json({ id: "run-x", status: "pending", pairTotal: 2, wsTopic: "run:run-x" }, { status: 202 });
    };
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"], onCreate: capture });

    const duration = await screen.findByRole("radiogroup", { name: "Duration" });
    fireEvent.click(within(duration).getByRole("radio", { name: "5m" }));
    if (interval) {
      const control = await screen.findByRole("radiogroup", { name: "Sample interval" });
      fireEvent.click(within(control).getByRole("radio", { name: interval }));
    }
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    return bodies[0] as Record<string, unknown>;
  }

  it("posts nothing on Auto: an untouched control must send the body it always sent", async () => {
    expect(await submitWith()).not.toHaveProperty("sampleIntervalNs");
  });

  it("posts the picked cadence alongside the duration", async () => {
    expect(await submitWith("15s")).toMatchObject({
      durationNs: 300_000_000_000,
      sampleIntervalNs: 15_000_000_000,
    });
  });

  it("falls back to Auto when shortening the run puts the picked cadence out of range", async () => {
    const bodies: unknown[] = [];
    const capture = (body: unknown) => {
      bodies.push(body);
      return json({ id: "run-x", status: "pending", pairTotal: 2, wsTopic: "run:run-x" }, { status: 202 });
    };
    renderPage({ permissions: OPERATOR, nodes: ["a", "b"], onCreate: capture });

    const duration = await screen.findByRole("radiogroup", { name: "Duration" });
    fireEvent.click(within(duration).getByRole("radio", { name: "15m" }));
    const control = await screen.findByRole("radiogroup", { name: "Sample interval" });
    fireEvent.click(within(control).getByRole("radio", { name: "5m" }));
    // 5m no longer fits inside a 1m run, and a control that still LOOKED chosen would post a 422.
    fireEvent.click(within(duration).getByRole("radio", { name: "1m" }));
    fireEvent.click(screen.getByRole("button", { name: /start run/i }));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).not.toHaveProperty("sampleIntervalNs");
  });

  it("says what the picked cadence will actually do, and leads with the adjustment when it cannot", async () => {
    renderPage({ nodes: ["a", "b"] });
    await screen.findByRole("radio", { name: "Instant" });

    fireEvent.click(screen.getByRole("radio", { name: "5m" }));
    fireEvent.click(await screen.findByRole("radio", { name: "1s" }));
    // tcp answers in milliseconds, so 1s is kept verbatim and nothing is explained away.
    await waitFor(() =>
      expect(screen.getByText(/probed every 1s for 5m — about 300 samples per pair/i)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/cannot go faster/i)).not.toBeInTheDocument();

    // MTR cannot keep it: the caption LEADS with that, then says what will happen.
    fireEvent.click(screen.getByRole("radio", { name: "MTR" }));
    const caption = await screen.findByText(/Every 1s is faster than one round/i);
    expect(caption.textContent).toMatch(/cannot go faster than every 90s/i);
    expect(caption.textContent).toMatch(/traces every 90s/i);
  });

  it("names the 500-sample ceiling when that is what bound the request", async () => {
    renderPage({ nodes: ["a", "b"] });
    await screen.findByRole("radio", { name: "Instant" });

    fireEvent.click(screen.getByRole("radio", { name: "24h" }));
    fireEvent.click(await screen.findByRole("radio", { name: "1s" }));
    // 1s over 24h is 86 400 samples for one pair; the ceiling widens it to 24h/500.
    const caption = await screen.findByText(/more than 500 samples for one pair/i);
    expect(caption.textContent).toMatch(/cannot go faster than every 173s/i);
  });

  it("names the control and its adjustment in Russian", async () => {
    renderPage({ locale: "ru", permissions: OPERATOR });
    await screen.findByRole("heading", { name: "Диагностика" });

    const duration = screen.getByRole("radiogroup", { name: "Длительность" });
    fireEvent.click(within(duration).getByRole("radio", { name: "5m" }));

    const control = await screen.findByRole("radiogroup", { name: "Период опроса" });
    expect(within(control).getByRole("radio", { name: "Авто" })).toBeInTheDocument();
    fireEvent.click(within(control).getByRole("radio", { name: "1s" }));

    // The unit is a WORD in prose: «раз в 1 секунду» would read wrong, so one drops the numeral.
    const caption = await screen.findByText(/Каждая пара зондируется раз в секунду/);
    expect(caption.textContent).not.toMatch(/раз в 1 с/);

    fireEvent.click(within(screen.getByRole("radiogroup", { name: "Тип проверки" })).getByRole("radio", { name: "MTR" }));
    const adjusted = await screen.findByText(/Раз в секунду — быстрее, чем успевает пройти один круг/);
    expect(adjusted.textContent).toMatch(/чаще чем раз в 90 секунд этот запуск не пойдёт/);
  });
});
