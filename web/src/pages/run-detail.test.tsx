import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import type { RunDetail } from "@/lib/types";
import { TimeMachineProvider } from "@/lib/timemachine";
import { decodeRunId, okPairs, runIdFromPath, RunDetailPage } from "./run-detail";

const RUN_ID = "run-1";

function runBody(overrides: Partial<RunDetail> = {}): RunDetail {
  return {
    id: RUN_ID,
    createdAt: "2026-07-28T10:00:00Z",
    status: "running",
    type: "tcp",
    plane: "pod",
    initiatorKind: "user",
    initiatorId: "u1",
    pairTotal: 2,
    pairOk: 0,
    pairFailed: 0,
    spec: {},
    results: [],
    ...overrides,
  };
}

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] },
    permissions,
  };
}

/** Every call the page made against the run resource, method included --
 *  enough to tell the cancel POST from the GET that follows it. */
interface Call {
  method: string;
  url: string;
}

function renderPage(
  capabilities: string[],
  run: RunDetail | (() => RunDetail),
  runId = RUN_ID,
  opts: { permissions?: string[]; onCancel?: () => Response } = {},
) {
  const { permissions = ["runs:create"], onCancel } = opts;
  window.history.pushState({}, "", `/diagnostics/runs/${runId}`);
  const calls: Call[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    calls.push({ method, url: href });
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities }));
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.endsWith("/cancel")) {
      return Promise.resolve(onCancel ? onCancel() : new Response(null, { status: 204 }));
    }
    if (href.startsWith("/api/v1/runs/")) return Promise.resolve(json(typeof run === "function" ? run() : run));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities });
  const utils = render(
    <QueryClientProvider client={qc}>
      <RunDetailPage />
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock, calls, qc };
}

function renderNotFound(runId = "nope") {
  window.history.pushState({}, "", `/diagnostics/runs/${runId}`);
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(["runs:create"])));
    if (href.startsWith("/api/v1/runs/")) {
      return Promise.resolve(
        new Response(JSON.stringify({ type: "about:blank", title: "run not found", status: 404 }), {
          status: 404,
          headers: { "Content-Type": "application/problem+json" },
        }),
      );
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities: [] });
  return render(
    <QueryClientProvider client={qc}>
      <RunDetailPage />
    </QueryClientProvider>,
  );
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

describe("runIdFromPath", () => {
  it("extracts the id after the permalink prefix", () => {
    expect(runIdFromPath("/diagnostics/runs/abc-123")).toBe("abc-123");
    expect(runIdFromPath("/diagnostics")).toBe("");
  });
});

/** QA round 4, finding #19: the header printed the URL's percent-encoded
 *  bytes, so an id with a slash or a space could not be matched against the
 *  one the reader pasted. */
describe("decodeRunId", () => {
  it("decodes the percent-encoding the pathname carries", () => {
    expect(decodeRunId("run%2Fa%20b")).toBe("run/a b");
  });

  it("leaves an ordinary id alone", () => {
    expect(decodeRunId("abc-123")).toBe("abc-123");
  });

  it("hands back the raw bytes rather than throwing on a lone percent", () => {
    expect(decodeRunId("100%")).toBe("100%");
  });
});

/** QA round 4, finding #14: "Pairs 2/2" was arrived/total and read as
 *  passed/total. */
describe("okPairs", () => {
  it("counts only the pairs that actually succeeded", () => {
    expect(
      okPairs([
        { source: "a", destination: "b", state: "failed", success: false },
        { source: "a", destination: "c", state: "succeeded", success: true },
      ]),
    ).toBe(1);
  });

  it("counts a socket frame's succeeded state, which carries no `success` of its own", () => {
    expect(okPairs([{ source: "a", destination: "b", state: "succeeded" }])).toBe(1);
  });

  it("counts an in-flight pair as not-yet-ok", () => {
    expect(okPairs([{ source: "a", destination: "b", state: "dispatched" }])).toBe(0);
  });
});

describe("RunDetailPage", () => {
  it("renders progress from socket frames", async () => {
    renderPage(["events"], runBody({ status: "running" }));

    // Wait for the run to have actually loaded (not just the page shell) --
    // the socket only opens once the first REST response is in, see
    // use-run.ts's socketEnabled doc comment.
    await screen.findByText("running");
    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "run:run-1",
        type: "event",
        seq: 1,
        data: { runId: RUN_ID, source: "node-a", destination: "node-b", state: "dispatched", completed: 0, total: 2 },
      });
    });

    await waitFor(() => expect(screen.getByText("node-a")).toBeInTheDocument());
    expect(screen.getByText("node-b")).toBeInTheDocument();
    expect(screen.getByText("dispatched")).toBeInTheDocument();
  });

  it("still completes with the socket disabled -- polling alone drives it to a terminal state", async () => {
    window.history.pushState({}, "", `/diagnostics/runs/${RUN_ID}`);
    let terminal = false;
    const fetchMock = vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(["runs:create"])));
      if (href.startsWith("/api/v1/runs/")) {
        return Promise.resolve(
          json(
            terminal
              ? runBody({
                  status: "succeeded",
                  pairOk: 1,
                  results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 12, recordedAt: "t" }],
                })
              : runBody({ status: "running" }),
          ),
        );
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities: [] });

    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      render(
        <QueryClientProvider client={qc}>
          <RunDetailPage />
        </QueryClientProvider>,
      );

      await waitFor(() => expect(screen.getByText("running")).toBeInTheDocument());
      expect(FakeSocket.instances).toHaveLength(0);

      terminal = true;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_100);
      });

      // "succeeded" shows twice (the run's status badge and this pair's own
      // state badge).
      expect(screen.getAllByText("succeeded").length).toBeGreaterThan(0);
      expect(screen.getByText("node-a")).toBeInTheDocument();
      expect(FakeSocket.instances).toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it("a socket frame and a polled result for the same pair render once, not twice", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        results: [],
      }),
    );

    await screen.findByText("running");
    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "run:run-1",
        type: "event",
        seq: 1,
        data: { runId: RUN_ID, source: "node-a", destination: "node-b", state: "succeeded", success: true, completed: 1, total: 1 },
      });
    });
    await waitFor(() => expect(screen.getAllByText("node-a")).toHaveLength(1));
    expect(screen.getAllByText("node-b")).toHaveLength(1);
  });

  it("a direct load of a finished run's permalink renders from the REST payload alone", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        pairOk: 1,
        results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 3, recordedAt: "t" }],
      }),
    );

    // "succeeded" shows twice (the run's own status badge, and this one
    // pair's state badge) -- both are the REST payload rendering correctly.
    await waitFor(() => expect(screen.getAllByText("succeeded").length).toBeGreaterThan(0));
    expect(screen.getByText("node-a")).toBeInTheDocument();
    // Already terminal on first paint -- no socket is ever opened for it.
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it("cancels a running run: POSTs /cancel, then re-reads the run once and shows the new status", async () => {
    let status = "running";
    const { calls } = renderPage(["events"], () => runBody({ status }), RUN_ID, {
      onCancel: () => {
        // The 204 is "accepted", not "cancelled" -- the run's own goroutine
        // writes the terminal status, which the page only learns by asking.
        status = "cancelled";
        return new Response(null, { status: 204 });
      },
    });

    fireEvent.click(await screen.findByRole("button", { name: /cancel run/i }));

    await waitFor(() =>
      expect(calls.some((c) => c.method === "POST" && c.url === `/api/v1/runs/${RUN_ID}/cancel`)).toBe(true),
    );
    // The refetch is what surfaces the new status -- not a status this page
    // wrote into its own cache.
    expect(await screen.findByText("cancelled")).toBeInTheDocument();
    const afterCancel = calls.findIndex((c) => c.url.endsWith("/cancel"));
    expect(calls.slice(afterCancel + 1).some((c) => c.method === "GET" && c.url === `/api/v1/runs/${RUN_ID}`)).toBe(
      true,
    );
    // Terminal now -- the affordance is gone, not merely disabled.
    await waitFor(() => expect(screen.queryByRole("button", { name: /cancel run/i })).not.toBeInTheDocument());
  });

  it("renders no Cancel button for a run that is already terminal", async () => {
    renderPage(["events"], runBody({ status: "succeeded" }));

    await screen.findByText("succeeded");
    expect(screen.queryByRole("button", { name: /cancel run/i })).not.toBeInTheDocument();
  });

  it("renders no Cancel button without runs:create, even while the run is in flight", async () => {
    renderPage(["events"], runBody({ status: "running" }), RUN_ID, { permissions: ["runs:read"] });

    await screen.findByText("running");
    expect(screen.queryByRole("button", { name: /cancel run/i })).not.toBeInTheDocument();
  });

  it("keeps the run on screen and explains a refused cancel inline", async () => {
    renderPage(["events"], runBody({ status: "running" }), RUN_ID, {
      onCancel: () =>
        new Response(JSON.stringify({ type: "about:blank", title: "forbidden", status: 403, detail: "runs:create required" }), {
          status: 403,
          headers: { "Content-Type": "application/problem+json" },
        }),
    });

    fireEvent.click(await screen.findByRole("button", { name: /cancel run/i }));

    expect(await screen.findByText("runs:create required")).toBeInTheDocument();
    // Still cancellable: a refused cancel does not consume the affordance.
    expect(screen.getByRole("button", { name: /cancel run/i })).toBeInTheDocument();
  });

  it("an unknown run id renders a not-found state rather than an infinite spinner", async () => {
    renderNotFound("nope");

    expect(await screen.findByText(/this run does not exist/i)).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: /loading run/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/loading run/i)).not.toBeInTheDocument();
  });

  it("prints a decoded id in the not-found copy, not the URL's percent-encoding (finding #19)", async () => {
    renderNotFound("run%2Fa%20b");

    expect(await screen.findByText(/No run matches “run\/a b”\./)).toBeInTheDocument();
  });
});

/**
 * QA round 4, finding #1. "Delayed data" on a run that finished twenty minutes
 * ago describes a transport nobody is waiting on, and sent operators hunting
 * for a staleness problem that did not exist. The badge keys on TERMINAL
 * first, then on the socket.
 */
describe("RunDetailPage realtime badge", () => {
  it("renders NO realtime badge at all for a terminal run — the data is final", async () => {
    renderPage(["events"], runBody({ status: "succeeded" }));

    await screen.findByText("succeeded");
    expect(screen.queryByText("Live")).not.toBeInTheDocument();
    expect(screen.queryByText("Delayed data")).not.toBeInTheDocument();
  });

  it("keeps the delayed badge on a run still in flight with the socket off", async () => {
    // No "events" capability: useRun never opens a socket, so `live` is false
    // and the page is genuinely on the 15s polling path.
    renderPage([], runBody({ status: "running" }));

    await screen.findByText("running");
    expect(screen.getByText("Delayed data")).toBeInTheDocument();
  });

  it("says Live while a run is in flight and the socket is up", async () => {
    renderPage(["events"], runBody({ status: "running" }));

    await screen.findByText("running");
    expect(await screen.findByText("Live")).toBeInTheDocument();
  });
});

/** QA round 4, finding #14. */
describe("RunDetailPage pair count", () => {
  it("reads ok/total, so a run whose every pair failed does not announce 2/2", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "failed",
        pairTotal: 2,
        pairFailed: 2,
        results: [
          { sourceNode: "node-a", destinationNode: "node-b", success: false, durationNs: 1, recordedAt: "t" },
          { sourceNode: "node-b", destinationNode: "node-a", success: false, durationNs: 1, recordedAt: "t" },
        ],
      }),
    );

    expect(await screen.findByText("0/2 ok")).toBeInTheDocument();
  });
});

/**
 * QA round 4, finding #4. A permalink names ONE specific run, so it renders
 * while the Time Machine is engaged rather than refusing — but it says which
 * instant the rest of the console is on, so a run that happened after `t` is
 * not silently read as part of that past.
 */
describe("RunDetailPage under the Time Machine", () => {
  function renderEngaged(at: string) {
    window.history.pushState({}, "", `/diagnostics/runs/${RUN_ID}?at=${at}`);
    const fetchMock = vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(["runs:create"])));
      if (href.startsWith("/api/v1/runs/")) return Promise.resolve(json(runBody({ status: "succeeded" })));
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities: [] });
    return render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <RunDetailPage />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
  }

  it("still renders the run, and frames it against the viewed instant", async () => {
    const at = "2026-07-28T09:00:00Z";
    renderEngaged(at);

    // The run itself is on screen: a permalink is not something to refuse.
    expect(await screen.findByText("succeeded")).toBeInTheDocument();
    expect(
      screen.getByText(new RegExp(`this permalink is shown in full.*${new Date(at).toLocaleString()}`)),
    ).toBeInTheDocument();
  });
});
