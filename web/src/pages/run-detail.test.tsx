import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
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
  opts: {
    permissions?: string[];
    onCancel?: () => Response;
    /** Mounts a <LocaleProvider> above the page. Absent — every case but the ru
     *  smoke pin at the bottom of this file — there is no provider at all,
     *  which lib/i18n defines as English. */
    locale?: Locale;
  } = {},
) {
  const { permissions = ["runs:create"], onCancel, locale } = opts;
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
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
  const page = <RunDetailPage />;
  const utils = render(
    <QueryClientProvider client={qc}>
      {locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}
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
  /* vitest.setup.ts backs localStorage with one Map per test FILE — a locale
     left behind would flip every later case in this one. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("runIdFromPath", () => {
  it("extracts the id after the permalink prefix", () => {
    expect(runIdFromPath("/diagnostics/runs/abc-123")).toBe("abc-123");
    expect(runIdFromPath("/diagnostics")).toBe("");
  });
});

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
                  results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 12, recordedAt: "t", sampleSeq: 0 }],
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
        results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 3, recordedAt: "t", sampleSeq: 0 }],
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

/** "Delayed data" on a run that finished twenty minutes ago describes a transport nobody is waiting on. */
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
          { sourceNode: "node-a", destinationNode: "node-b", success: false, durationNs: 1, recordedAt: "t", sampleSeq: 0 },
          { sourceNode: "node-b", destinationNode: "node-a", success: false, durationNs: 1, recordedAt: "t", sampleSeq: 0 },
        ],
      }),
    );

    expect(await screen.findByText("0/2 ok")).toBeInTheDocument();
  });
});

/** A permalink names ONE specific run, so it renders while the Time Machine is engaged rather than refusing. */
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

/* ── interval runs (Task 2) ─────────────────────────────────────────────── */

describe("interval runs", () => {
  const s = 1_000_000_000;

  function sample(seq: number, over: Partial<RunDetail["results"][number]> = {}) {
    return {
      sourceNode: "node-a",
      destinationNode: "node-b",
      success: true,
      durationNs: 2_000_000,
      recordedAt: "2026-07-28T10:00:00Z",
      sampleSeq: seq,
      ...over,
    };
  }

  // The whole point of the feature, on screen: an operator who left a check
  // running for a minute must be able to see WHEN it broke, not just that it
  // did. Aggregate + one tick per probe.
  it("shows the aggregate and a tick per probe for a run with a duration", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: [
          sample(0, { durationNs: 1_000_000 }),
          sample(1, { durationNs: 3_000_000 }),
          sample(2, { success: false, durationNs: 2 * s, error: "connection refused" }),
        ],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    // Duration and derived cadence, as the server will actually run it.
    expect(screen.getByText("1m")).toBeInTheDocument();
    expect(screen.getByText("5s")).toBeInTheDocument();
    // sent / failed / fail%
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("1 (33.3%)")).toBeInTheDocument();
    // A tick per probe, each labelled with its own outcome.
    expect(screen.getByTitle("#0 ok 1.0ms")).toBeInTheDocument();
    expect(screen.getByTitle("#1 ok 3.0ms")).toBeInTheDocument();
    /* QA scope 4, finding #12: a FAILED tick carries the error and NO
       duration. The 2000ms it used to print was the time spent waiting for a
       round trip that never happened, offered as if it were a latency —
       directly against the caption two lines above it. */
    expect(screen.getByTitle("#2 connection refused")).toBeInTheDocument();
    expect(screen.queryByTitle(/connection refused.*ms/)).not.toBeInTheDocument();
  });

  // The timeout must not be averaged into the latency: min/avg/max/p95 cover
  // the probes that ANSWERED. Here the only failure is a 2s timeout, so a
  // naive average would report ~668ms instead of 2ms.
  it("keeps a failed probe's elapsed time out of the latency stats", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "partial",
        spec: { Duration: 60 * s },
        pairTotal: 1,
        results: [
          sample(0, { durationNs: 1_000_000 }),
          sample(1, { durationNs: 3_000_000 }),
          sample(2, { success: false, durationNs: 2 * s, error: "timeout" }),
        ],
      }),
    );

    await screen.findByText("Probe timeline");
    // avg over {1ms, 3ms} = 2.0ms, NOT (1+3+2000)/3.
    expect(screen.getByText("2.0ms")).toBeInTheDocument();
    expect(screen.queryByText(/66[0-9]\.?[0-9]*ms/)).not.toBeInTheDocument();
  });

  // An instant run must look exactly as it did.
  it("shows neither aggregate nor timeline for an instant run", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        spec: { Type: "tcp" },
        pairTotal: 1,
        results: [sample(0)],
      }),
    );

    expect(await screen.findByRole("heading", { name: "Pairs" })).toBeInTheDocument();
    expect(screen.queryByText("Probe timeline")).not.toBeInTheDocument();
    expect(screen.queryByText("Cadence")).not.toBeInTheDocument();
  });

  // A long run's whole reason to exist is being watchable WHILE it runs: the aggregate renders
  // mid-flight.
  it("renders the aggregate, a realtime badge and Cancel while still running", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        spec: { Duration: 3600 * s },
        pairTotal: 1,
        results: [sample(0), sample(1)],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /cancel run/i })).toBeInTheDocument();
    expect(screen.getByText(/^(Live|Delayed data)$/)).toBeInTheDocument();
    // 1h widens the cadence past the 5s floor: 3600s/500 = 7.2s -> "7s".
    expect(screen.getByText("7s")).toBeInTheDocument();
  });

  // The terminal-run honesty pin, restated for an interval run: a finished
  // run's data is final, so no realtime badge and no Cancel.
  it("drops the realtime badge and Cancel once a long run is terminal", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "cancelled",
        spec: { Duration: 3600 * s },
        pairTotal: 1,
        results: [sample(0)],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(screen.queryByText(/^(Live|Delayed data)$/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /cancel run/i })).not.toBeInTheDocument();
  });
});

/* ── the progress frame: the full expected track, drawn up front, filling in place ───────────── */

describe("the probe timeline's progress frame", () => {
  const s = 1_000_000_000;

  function sample(seq: number, over: Partial<RunDetail["results"][number]> = {}) {
    return {
      sourceNode: "node-a",
      destinationNode: "node-b",
      success: true,
      durationNs: 2_000_000,
      recordedAt: "2026-07-28T10:00:00Z",
      sampleSeq: seq,
      ...over,
    };
  }

  const pending = () => screen.queryAllByTestId("timeline-slot-pending");
  const filled = () => screen.queryAllByTestId("timeline-slot-filled");

  // Mid-run: three arrived, nine drawn as placeholders, and a caption for what they are worth.
  it("draws the whole expected track mid-run, with the tail as placeholders", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: [sample(0), sample(1), sample(2)],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(filled()).toHaveLength(3);
    expect(pending()).toHaveLength(9);
    // 9 slots × the 5s cadence, and "~" because the caption never claims a precision it lacks.
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("3 of ~12 · ~45s left");
    // The screen reader gets the same numbers the sighted reader does.
    expect(
      screen.getByRole("img", { name: "node-a to node-b: 3 of about 12 probes recorded, 9 more probes still to come" }),
    ).toBeInTheDocument();
  });

  // Complete: every slot filled, and NO "left" — there is nothing to wait for.
  it("reads N of ~N with no tail once the run is complete", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: Array.from({ length: 12 }, (_, i) => sample(i)),
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(filled()).toHaveLength(12);
    expect(pending()).toHaveLength(0);
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("12 of ~12");
    expect(screen.getByTestId("timeline-progress")).not.toHaveTextContent("left");
  });

  // Cancelled: the frame stays, and the nine probes nobody dispatched are not drawn as failures.
  it("keeps a cancelled run framed with an empty tail, and invents no failures", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "cancelled",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: [sample(0), sample(1), sample(2)],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(filled()).toHaveLength(3);
    expect(pending()).toHaveLength(9);
    // No countdown on a run that has stopped, and the failure count stays 0.
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("3 of ~12");
    expect(screen.getByTestId("timeline-progress")).not.toHaveTextContent("left");
    expect(screen.getByText(/3 sent · 0 failed/)).toBeInTheDocument();
  });

  // No frame theater around a single dot; instant runs render no timeline at all (pinned above).
  it("leaves a one-slot track unframed", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        spec: { Type: "tcp", Duration: 5 * s },
        pairTotal: 1,
        results: [sample(0)],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(screen.getByTitle("#0 ok 2.0ms")).toBeInTheDocument();
    expect(pending()).toHaveLength(0);
    expect(filled()).toHaveLength(0);
    expect(screen.queryByTestId("timeline-progress")).not.toBeInTheDocument();
  });

  // Keyed by SLOT, so an arrival mutates that node; the identity check below is the proof.
  it("fills a placeholder in place when the next sample lands", async () => {
    let results = [sample(0), sample(1), sample(2)];
    const { qc } = renderPage(
      ["events"],
      () => runBody({ status: "running", spec: { Duration: 60 * s }, pairTotal: 1, results }),
    );

    await screen.findByText("Probe timeline");
    const trackBefore = screen.getByRole("img", { name: /node-a to node-b/ });
    const slot3 = trackBefore.children[3];
    expect(slot3).toHaveAttribute("data-testid", "timeline-slot-pending");

    results = [...results, sample(3)];
    await act(async () => {
      await qc.refetchQueries({ queryKey: ["run", RUN_ID] });
    });

    await waitFor(() => expect(screen.queryAllByTestId("timeline-slot-filled")).toHaveLength(4));
    const trackAfter = screen.getByRole("img", { name: /node-a to node-b/ });
    // Same <div> for the track and the SAME node in slot 3 — filled, not replaced.
    expect(trackAfter).toBe(trackBefore);
    expect(trackAfter.children[3]).toBe(slot3);
    expect(slot3).toHaveAttribute("data-testid", "timeline-slot-filled");
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("4 of ~12 · ~40s left");
  });

  // The singular tail: countForm's `.one` branch, in the aria-label.
  it("says a single remaining probe in the singular", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        spec: { Duration: 60 * s },
        pairTotal: 1,
        results: Array.from({ length: 11 }, (_, i) => sample(i)),
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(
      screen.getByRole("img", { name: "node-a to node-b: 11 of about 12 probes recorded, 1 more probe still to come" }),
    ).toBeInTheDocument();
  });
});

/* the Russian is wired ONE smoke pin. */
describe("RunDetailPage — Russian", () => {
  const s = 1_000_000_000;

  it("renders the sample timeline, its latency caveat and a live Cancel in Russian", async () => {
    renderPage(
      ["events"],
      {
        id: RUN_ID,
        createdAt: "2026-07-28T10:00:00Z",
        startedAt: "2026-07-28T10:00:00Z",
        status: "running",
        type: "tcp",
        plane: "pod",
        initiatorKind: "user",
        initiatorId: "u1",
        pairTotal: 1,
        pairOk: 0,
        pairFailed: 0,
        spec: { Type: "tcp", Duration: 60 * s },
        results: [
          {
            sourceNode: "node-a",
            destinationNode: "node-b",
            success: false,
            durationNs: 2 * s,
            recordedAt: "2026-07-28T10:00:00Z",
            sampleSeq: 0,
            error: "connection refused",
          },
        ],
      } as RunDetail,
      RUN_ID,
      { locale: "ru" },
    );

    expect(await screen.findByText("Лента зондов")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Пары" })).toBeInTheDocument();

    // The honesty caption: a timeout is counted as a failure and kept OUT of
    // min/avg/p95 — the same caveat, at the same strength, as the English.
    const note = screen.getByText(/Задержка считается только по ответившим зондам/);
    expect(note.textContent).toMatch(/не попадает в мин\/сред\/p95/);
    expect(note.textContent).toMatch(/таймаут никогда не выдаёт себя за измеренную задержку/);

    // A live run keeps its Cancel; the probe's own error is the agent's word
    // and stays verbatim inside the translated tick title.
    expect(screen.getByRole("button", { name: "Отменить запуск" })).toBeInTheDocument();
    expect(screen.getByTitle("#0 connection refused")).toBeInTheDocument();
    // The status badge is the store's enum and does NOT move.
    expect(screen.getByText("running")).toBeInTheDocument();
  });

  // The answer to «не понимаю сколько осталось», in Russian: count, approximate total, tail as time.
  it("prints the progress caption and its plural tail in Russian", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: Array.from({ length: 9 }, (_, i) => ({
          sourceNode: "node-a",
          destinationNode: "node-b",
          success: true,
          durationNs: 2_000_000,
          recordedAt: "2026-07-28T10:00:00Z",
          sampleSeq: i,
        })),
      }),
      RUN_ID,
      { locale: "ru" },
    );

    expect(await screen.findByText("Лента зондов")).toBeInTheDocument();
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("9 из ~12 · осталось ~15 с");
    expect(screen.queryAllByTestId("timeline-slot-pending")).toHaveLength(3);
    // The summary card's spans localise with it: «1 мин» duration, «5 с» cadence.
    expect(screen.getByText("1 мин")).toBeInTheDocument();
    expect(screen.getByText("5 с")).toBeInTheDocument();
    expect(screen.queryByText("1m")).not.toBeInTheDocument();
    // countForm's `.few` branch: 3 → «зонда», not «зондов».
    expect(
      screen.getByRole("img", { name: "node-a → node-b: записано 9 зондов из ~12, ждём ещё 3 зонда" }),
    ).toBeInTheDocument();
  });

  /* QA scope 4, finding #7: the page printed "8/10/2026 3:47 AM" under a
     Russian heading. Date ORDER and the AM/PM marker are not digits — they
     follow the interface language, through lib/i18n's stampFull. */
  it("prints the Started stamp in the interface language, not the browser's", async () => {
    const startedAt = "2026-07-28T10:00:00Z";
    renderPage(
      [],
      {
        id: RUN_ID,
        createdAt: startedAt,
        startedAt,
        status: "succeeded",
        type: "tcp",
        plane: "pod",
        initiatorKind: "user",
        initiatorId: "u1",
        pairTotal: 1,
        pairOk: 1,
        pairFailed: 0,
        spec: { Type: "tcp" },
        results: [],
      } as unknown as RunDetail,
      RUN_ID,
      { locale: "ru" },
    );

    const expected = new Date(startedAt).toLocaleString("ru-RU");
    expect(await screen.findByText(expected)).toBeInTheDocument();
    // The two shapes the bare call produced, neither of which is Russian.
    expect(expected).not.toMatch(/\s(AM|PM)\b/i);
    expect(screen.queryByText(/\d+\/\d+\/\d{4}/)).not.toBeInTheDocument();
  });
});
