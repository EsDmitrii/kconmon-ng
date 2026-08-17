import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import { MatrixPage } from "./matrix";

/** /matrix engaged: the grid is drawn from PromQL at `t`. */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const vectorFor = (value: string) => ({
  status: "success",
  data: {
    resultType: "vector",
    result: [{ metric: { source_node: "a", destination_node: "b" }, value: [1785276000, value] }],
  },
});

/** fail = 0.5, rtt = 2ms — the same numbers matrix.test.tsx's live body uses,
 *  so the rendered cell is comparable line for line with the live case. */
function stubFetch() {
  const urls: string[] = [];
  const queries: string[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    urls.push(href);
    if (href.includes("/api/v1/version")) {
      return Promise.resolve(json({ version: "1.7.0", commit: "abc", capabilities: ["events"] }));
    }
    if (href.includes("/api/v1/promql/query")) {
      const q = (JSON.parse(String(init?.body)) as { query: string }).query;
      queries.push(q);
      return Promise.resolve(json(vectorFor(q.startsWith("histogram_quantile") ? "0.002" : "0.5")));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { urls, queries };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        <MatrixPage />
      </TimeMachineProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  window.history.pushState({}, "", `/matrix?at=${AT}`);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("MatrixPage engaged at t", () => {
  it("renders the grid from PromQL at t, with zero requests to /api/v1/matrix", async () => {
    const { urls, queries } = stubFetch();
    renderPage();
    const cell = await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    expect(cell).toHaveTextContent("50.0%");
    expect(queries).toHaveLength(2);
    expect(urls.filter((u) => u.startsWith("/api/v1/matrix"))).toEqual([]);
  });

  it("dates the page at the instant instead of promising a 15s recompute", async () => {
    stubFetch();
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    expect(screen.getByText(/evaluated straight from Prometheus at that instant/)).toBeInTheDocument();
    expect(screen.queryByText(/recomputed from Prometheus every 15s/)).not.toBeInTheDocument();
  });

  it("shows no realtime badge — the grid is pinned on purpose, not lagging", async () => {
    stubFetch();
    renderPage();
    await screen.findByLabelText("a → b: fail 50.0%, RTT p95 2.0ms");
    expect(screen.queryByText("Delayed data")).not.toBeInTheDocument();
    expect(screen.queryByText("Live")).not.toBeInTheDocument();
  });

  it("blames the instant, not a missing deployment, when nothing was scraped then", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) =>
        Promise.resolve(
          String(url).includes("/api/v1/promql/query")
            ? json({ status: "success", data: { resultType: "vector", result: [] } })
            : json({}),
        ),
      ),
    );
    renderPage();
    await screen.findByText("No probe data in Prometheus at this time");
    expect(screen.queryByText("No probe data in Prometheus yet")).not.toBeInTheDocument();
  });

  it("surfaces a PromQL error rather than painting an empty grid over it", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) =>
        Promise.resolve(
          String(url).includes("/api/v1/promql/query")
            ? json({ status: "error", errorType: "bad_data", error: "unknown metric" })
            : json({}),
        ),
      ),
    );
    renderPage();
    await screen.findByText("Matrix is unavailable");
    expect(screen.getByText("unknown metric")).toBeInTheDocument();
  });
});

/* ── the two URL keys, abused ───────────────────────────────────────────────
 *
 * QA scope 4: whatever a shared link says, the page must open on something it
 * can actually answer for, and the address bar must not go on claiming what the
 * page is not showing. `?at=` is lib/timemachine's (RFC 3339, strictly) and
 * `?protocol=` is this page's.
 */
describe("MatrixPage — a link whose parameters do not mean anything", () => {
  /** A LIVE body: every case below is one the console must refuse to engage on,
   *  so what has to arrive is the ordinary /api/v1/matrix answer. */
  const openAt = (search: string) => {
    window.history.pushState({}, "", `/matrix${search}`);
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) =>
        Promise.resolve(
          String(url).includes("/api/v1/matrix")
            ? json({
                protocol: "tcp", plane: "pod", nodes: ["a", "b"],
                cells: [{ source: "a", destination: "b", failRatio: 0.5 }],
                timestamp: "t",
              })
            : json({ version: "1.7.0" }),
        ),
      ),
    );
    renderPage();
  };

  it.each([
    ["?at=yesterday", "an English word"],
    ["?at=2026-13-45T99:99:99Z", "a date that does not exist"],
    ["?at=1785276000", "a unix stamp"],
    ["?at=", "an empty value"],
    ["?at=%00", "a NUL"],
  ])("stays Live for %s (%s), printing no Invalid Date", async (search) => {
    openAt(search);
    // The Live sentence, not the "as of {at}" one — and never a mangled stamp.
    expect(await screen.findByText(/^Live N×N node connectivity/)).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/Invalid Date|NaN/);
  });

  it("degrades a protocol nothing probes and corrects the address bar it came in", async () => {
    openAt("?protocol=SCTP&at=nonsense");
    await screen.findByRole("table");

    // The URL is what an operator copies and shares: it must not keep claiming
    // a protocol the page silently replaced.
    expect(new URLSearchParams(window.location.search).get("protocol")).toBe("tcp");
    expect(screen.getByRole("radio", { name: "TCP" })).toBeChecked();
    // ?at= is not this page's to rewrite, and the unreadable one left it Live.
    expect(screen.getByText(/^Live N×N node connectivity/)).toBeInTheDocument();
  });

  it("takes the first of a repeated protocol and collapses the duplicate away", async () => {
    openAt("?protocol=zzz&protocol=udp");
    await screen.findByRole("table");

    expect(window.location.search).toBe("?protocol=tcp");
    expect(screen.getByRole("radio", { name: "TCP" })).toBeChecked();
  });
});
