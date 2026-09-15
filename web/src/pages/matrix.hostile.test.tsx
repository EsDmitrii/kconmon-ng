import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { MatrixPage } from "./matrix";

/*
 * The matrix under a hostile TOPOLOGY. matrix.test.tsx already covers a matrix
 * payload the console cannot stand behind; what is new in 2.4.0 is that the
 * grid reads two more fields off GET /api/v1/topology — every agent's `labels`
 * and `capabilities` — and both are fields a controller of any age may or may
 * not send, in any shape a Go zero value or a websocket frame can take. The bar
 * is lib/agents.ts's: nothing throws (there is no error boundary above these
 * routes, so a throw in render IS a blank page), and on any doubt the grid
 * fails toward the ALARMING reading — 'no data', never a calming dashed frame
 * or a note about a scrape job nobody has evidence for.
 */

const EXTERNAL_LABEL = "kconmon-ng.io/external";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

/* a → b measured, b → a silent, on the ICMP grid: the one cell a false 'unsupported' would calm. */
const matrixBody = {
  protocol: "icmp", plane: "pod", nodes: ["a", "b"],
  cells: [{ source: "a", destination: "b", failRatio: 0.5, lossRatio: 0 }],
  timestamp: "t",
};

function stubFetch(topology: unknown, topologyInit?: ResponseInit) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const u = String(url);
      if (u.includes("/api/v1/topology")) return Promise.resolve(json(topology, topologyInit));
      if (u.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
      return Promise.resolve(json({ version: "2.4.0", commit: "abc" }));
    }),
  );
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MatrixPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  window.history.replaceState({}, "", "/matrix?protocol=icmp");
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/");
});

const agent = (nodeName: unknown, over: Record<string, unknown> = {}) => ({
  id: "x", nodeName, podIP: "10.0.0.1", zone: "z", ...over,
});

const topo = (agents: unknown) => ({ nodes: [{ name: "a", zone: "z", ready: true }], agents, timestamp: "t" });

/** Every silence on the grid is the plain kind: pinned aria, no dashed reason, no note, no badge. */
async function expectPlainSilence() {
  const cell = await screen.findByLabelText("b → a: no data");
  expect(screen.queryAllByLabelText(/does not run/)).toHaveLength(0);
  expect(screen.queryByTestId("legend-unsupported")).not.toBeInTheDocument();
  expect(screen.queryByTestId("matrix-unscraped-note")).not.toBeInTheDocument();
  expect(screen.queryAllByRole("link", { name: /external agent$/ })).toHaveLength(0);
  fireEvent.mouseEnter(cell);
  const tooltip = await screen.findByRole("tooltip");
  expect(tooltip).toHaveTextContent("No probe data in Prometheus for this pair.");
  expect(tooltip).not.toHaveTextContent("No series");
}

describe("MatrixPage — a topology the console cannot trust", () => {
  it.each<[string, unknown]>([
    ["agents: null (a Go nil slice past the normalizer)", null],
    ["agents: a string", "a,b"],
    ["agents: an object", { a: {} }],
    ["an agent that is null", [null]],
    ["an agent that is a number", [1]],
    ["an agent with no name", [agent(undefined, { labels: { [EXTERNAL_LABEL]: "true" } })]],
    ["an agent whose name is null", [agent(null, { capabilities: ["plane:tcp"] })]],
    ["capabilities: null", [agent("b", { capabilities: null }), agent("a", { capabilities: null })]],
    ["labels: []", [agent("b", { labels: [] }), agent("a", { labels: [] })]],
    ["labels: an array holding the word", [agent("b", { labels: [EXTERNAL_LABEL, "true"] })]],
    ["labels: a string", [agent("b", { labels: "true" })]],
    ["labels: the label with a boolean, not the string", [agent("b", { labels: { [EXTERNAL_LABEL]: true } })]],
    ["labels: the label spelt True", [agent("b", { labels: { [EXTERNAL_LABEL]: "True" } })]],
    ["capabilities: a string", [agent("b", { capabilities: "plane:tcp" })]],
    ["capabilities: an object", [agent("b", { capabilities: { "plane:tcp": true } })]],
    ["capabilities: junk members", [agent("b", { capabilities: [null, 1, "", "plane:", "plane:sctp", {}] })]],
  ])("renders every silence as plain 'no data' for %s", async (_name, agents) => {
    stubFetch(topo(agents));
    renderPage();
    await expectPlainSilence();
  });

  it("renders when the topology itself is unreadable, and when its fetch fails", async () => {
    stubFetch("not an object");
    renderPage();
    await expectPlainSilence();
    cleanup();
    resetWsClient();

    stubFetch({ type: "about:blank", title: "boom", status: 500 }, { status: 500 });
    renderPage();
    await expectPlainSilence();
  });
});

/* The fail-open rule, pinned on the page: absence of every plane:* is EVERY plane. */
describe("MatrixPage — an agent that never advertised a plane is never unsupported", () => {
  it.each<[string, unknown]>([
    ["no capabilities field at all (pre-2.4.0)", agent("b")],
    ["capabilities: []", agent("b", { capabilities: [] })],
    ["capabilities naming no plane", agent("b", { capabilities: ["external-checks"] })],
    ["capabilities naming only a plane the console does not know", agent("b", { capabilities: ["plane:sctp"] })],
    ["capabilities with the prefix and nothing after it", agent("b", { capabilities: ["plane:"] })],
    ["capabilities in the wrong case", agent("b", { capabilities: ["PLANE:TCP", "Plane:tcp"] })],
  ])("reads %s as every plane", async (_name, b) => {
    stubFetch(topo([agent("a", { capabilities: ["plane:tcp", "plane:icmp"] }), b]));
    renderPage();
    await expectPlainSilence();
  });

  it("still reads a source that DID name its planes, next to one that did not", async () => {
    stubFetch(topo([agent("a"), agent("b", { capabilities: ["plane:tcp"] })]));
    renderPage();
    expect(await screen.findByLabelText("b → a: the source does not run ICMP probes")).toBeInTheDocument();
  });
});

/* A malformed capabilities field must not take the EXTERNAL reading down with it, or vice versa. */
describe("MatrixPage — the two fields fail independently", () => {
  it("keeps the external reading when capabilities is null", async () => {
    stubFetch(topo([agent("a"), agent("b", { labels: { [EXTERNAL_LABEL]: "true" }, capabilities: null })]));
    renderPage();
    expect(await screen.findAllByRole("link", { name: "Open the card for b, external agent" })).toHaveLength(2);
    expect(screen.getByTestId("matrix-unscraped-note")).toHaveTextContent("b is an external agent");
    expect(screen.queryAllByLabelText(/does not run/)).toHaveLength(0);
  });

  it("keeps the planes reading when labels is an array", async () => {
    stubFetch(topo([agent("a"), agent("b", { labels: [], capabilities: ["plane:tcp"] })]));
    renderPage();
    expect(await screen.findByLabelText("b → a: the source does not run ICMP probes")).toBeInTheDocument();
    expect(screen.queryAllByRole("link", { name: /external agent$/ })).toHaveLength(0);
    expect(screen.queryByTestId("matrix-unscraped-note")).not.toBeInTheDocument();
  });
});
