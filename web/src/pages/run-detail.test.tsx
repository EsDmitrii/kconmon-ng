import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import type { RunDetail } from "@/lib/types";
import { TimeMachineProvider } from "@/lib/timemachine";
import { decodeRunId, okPairs, runIdFromPath, RunDetailPage } from "./run-detail";

vi.mock("@/components/echart", () => ({
  EChart: ({ className }: { className?: string }) => <div data-testid="echart" className={className} />,
}));

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
    /** GET /api/v1/mtr/snapshots, which a pair row reads when it is opened. */
    onSnapshots?: () => Response;
  } = {},
) {
  const { permissions = ["runs:create"], onCancel, locale, onSnapshots } = opts;
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
    if (href.startsWith("/api/v1/mtr/snapshots")) {
      return Promise.resolve(onSnapshots ? onSnapshots() : json({ snapshots: [], nextCursor: "" }));
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
    /* The badge follows the CONNECTION now, not the mere existence of a subscription: it used to
       read "Live" while the socket was down and the page was really being carried by the 5s poll. */
    await screen.findByText("Delayed data");
    act(() => FakeSocket.last().emitOpen());
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
    // Duration and derived cadence, as the server will actually run it. The cadence carries the
    // word "planned": every sample here shares one instant, so there is no spacing to MEASURE, and
    // an unlabelled number on this tile is the whole bug.
    expect(screen.getByText("1m")).toBeInTheDocument();
    expect(screen.getByText("5s planned")).toBeInTheDocument();
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

  /* ── the Cadence tile, rev13 acceptance ──────────────────────────────────
     A 15m MTR over four pairs executes one round every 90s — verified on the
     stand as 12 samples in 3 rounds — and the tile read "5s × 4": the BASE
     cadence off the duration, and a bare "× 4" that named nothing. */

  it("names the EFFECTIVE cadence the run actually ran at, not the base one", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        // No planner fields: this is the pre-snapshot run, derived client-side.
        spec: { Type: "mtr", Duration: 900 * s },
        pairTotal: 4,
        results: [sample(0, { sourceNode: "n1" }), sample(0, { sourceNode: "n2" })],
      }),
    );

    await screen.findByText("Cadence");
    const tile = screen.getByTestId("summary-cadence");
    expect(tile).toHaveTextContent("90s");
    // The base cadence is 15m/500 = 1.8s floored to 5s, and it is a number this
    // run never used.
    expect(tile).not.toHaveTextContent("5s");
  });

  it("prefers the cadence the SERVER snapshotted over anything derived here", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        spec: { Type: "mtr", Duration: 900 * s, PlannedSampleIntervalNs: 120 * s, PlannedSamplesPerPair: 7 },
        pairTotal: 4,
        results: [sample(0)],
      }),
    );

    await screen.findByText("Cadence");
    const tile = screen.getByTestId("summary-cadence");
    expect(tile).toHaveTextContent("2m");
    expect(tile).toHaveTextContent("≥ 7 per pair");
  });

  it("says what the second number IS rather than leaving a bare '× 4'", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        spec: { Type: "mtr", Duration: 900 * s, PlannedSampleIntervalNs: 90 * s, PlannedSamplesPerPair: 10 },
        pairTotal: 4,
        results: [sample(0)],
      }),
    );

    await screen.findByText("Cadence");
    const tile = screen.getByTestId("summary-cadence");
    expect(tile).toHaveTextContent("4 pairs");
    expect(tile).toHaveTextContent("≥ 10 per pair");
    expect(tile.textContent).not.toMatch(/×\s*4\s*$/);
  });

  /* ── the plan is not an observation ──────────────────────────────────────
     The owner's run: a 5m MTR over ten pairs whose tile read «Периодичность 3 мин» — the planner's
     WORST-CASE floor — while the run was producing a probe about every minute, because a round that
     finishes early starts the next one immediately. The plan bounds the spacing from above; it is
     not a measurement, and it may not wear a measurement's label. */
  it("measures the cadence off the samples on screen once a pair has two of them", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        spec: { Type: "mtr", Duration: 300 * s, PlannedSampleIntervalNs: 180 * s, PlannedSamplesPerPair: 1 },
        pairTotal: 2,
        results: [
          sample(0, { recordedAt: "2026-08-11T13:33:00Z" }),
          sample(1, { recordedAt: "2026-08-11T13:34:00Z" }),
          sample(2, { recordedAt: "2026-08-11T13:35:00Z" }),
        ],
      }),
    );

    await screen.findByText("Cadence");
    const tile = screen.getByTestId("summary-cadence");
    // The headline is the MEASURED spacing, and says so.
    expect(tile).toHaveTextContent("1m measured");
    expect(tile).toHaveTextContent("≥ 3 per pair so far");
    // The plan survives one line down, worded as the bound it is — never as the run's cadence.
    expect(screen.getByTestId("summary-cadence-plan")).toHaveTextContent(
      "planned no slower than once every 3m, ≥ 1 per pair",
    );
    expect(tile.textContent).not.toMatch(/3m measured/);
  });

  it("shows the PLAN, labelled as one, until there is anything to measure", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        spec: { Type: "mtr", Duration: 300 * s, PlannedSampleIntervalNs: 180 * s, PlannedSamplesPerPair: 1 },
        pairTotal: 2,
        // One sample is an instant, and an instant has no spacing.
        results: [sample(0, { recordedAt: "2026-08-11T13:33:00Z" })],
      }),
    );

    await screen.findByText("Cadence");
    const tile = screen.getByTestId("summary-cadence");
    expect(tile).toHaveTextContent("3m planned");
    expect(tile).toHaveTextContent("≥ 1 per pair");
    expect(tile.textContent).not.toMatch(/measured/);
    // No second plan line: repeating the plan under itself would be two labels for one number.
    expect(screen.queryByTestId("summary-cadence-plan")).not.toBeInTheDocument();
  });

  it("labels both numbers in Russian too", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        spec: { Type: "mtr", Duration: 300 * s, PlannedSampleIntervalNs: 180 * s, PlannedSamplesPerPair: 1 },
        pairTotal: 2,
        results: [
          sample(0, { recordedAt: "2026-08-11T13:33:00Z" }),
          sample(1, { recordedAt: "2026-08-11T13:34:00Z" }),
        ],
      }),
      RUN_ID,
      { locale: "ru" },
    );

    await screen.findByText("Периодичность");
    const tile = screen.getByTestId("summary-cadence");
    expect(tile).toHaveTextContent("1 мин по факту");
    expect(tile).toHaveTextContent("пока ≥ 2 на пару");
    /* «не реже раза в 3 минуты» — the period as a WORD, because «раза в 3 мин» inside a sentence
       is the same defect the create captions were carrying. */
    expect(screen.getByTestId("summary-cadence-plan")).toHaveTextContent(
      "по плану не реже раза в 3 минуты, ≥ 1 на пару",
    );
  });

  it("agrees the pair noun with its count rather than printing '1 pairs'", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "tcp",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: [sample(0)],
      }),
    );

    await screen.findByText("Cadence");
    expect(screen.getByTestId("summary-cadence")).toHaveTextContent("1 pair ");
  });

  it("counts a stretched run's tail down in the EFFECTIVE interval", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        spec: { Type: "mtr", Duration: 900 * s, PlannedSampleIntervalNs: 90 * s, PlannedSamplesPerPair: 10 },
        pairTotal: 1,
        results: [sample(0), sample(1), sample(2), sample(3)],
      }),
    );

    await screen.findByText("Probe timeline");
    // Six slots left at 90s each is nine minutes, not the thirty seconds the
    // base cadence would have promised.
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("4 of ≥10 · ~9m left");
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
    // 1h widens the cadence past the 5s floor: 3600s/500 = 7.2s -> "7s", labelled as the plan it is.
    expect(screen.getByText("7s planned")).toBeInTheDocument();
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
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("3 of ≥12 · ~45s left");
    // The screen reader gets the same numbers the sighted reader does.
    expect(
      screen.getByRole("img", { name: "node-a to node-b: 3 of at least 12 probes recorded, 9 more probes still to come" }),
    ).toBeInTheDocument();
  });

  // Complete: every slot filled, and NO "left" — there is nothing to wait for.
  it("reads N of ≥N with no tail once the run is complete", async () => {
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
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("12 of ≥12");
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
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("3 of ≥12");
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
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("4 of ≥12 · ~40s left");
  });

  /* ── the frame's two ENDS ─────────────────────────────────────────────────
     «нет чёткой границы начала и конца». The slot arithmetic above was right
     and the strip still read as a pile: a COMPLETE run has no placeholder tail
     at all, so with nothing but filled ticks on screen there was no mark saying
     where the track began or where it stopped. The rail's two caps are drawn
     from the frame, not from the ticks, so they are there in every state a
     duration run can be in. */
  const frame = () => screen.queryAllByTestId("timeline-frame");
  const capStart = () => screen.queryAllByTestId("timeline-frame-start");
  const capEnd = () => screen.queryAllByTestId("timeline-frame-end");

  const states: [string, Partial<RunDetail>][] = [
    // Mid-flight: a tail still to come.
    ["running", { status: "running", results: [0, 1, 2].map((i) => sample(i)) }],
    // Finished, every slot filled — the state that had no boundary left at all.
    ["succeeded", { status: "succeeded", results: Array.from({ length: 12 }, (_, i) => sample(i)) }],
    // Stopped early: the frame is the only thing saying what was given up on.
    ["cancelled", { status: "cancelled", results: [sample(0)] }],
    // Failed and partial are terminal too, and read the same way.
    ["partial", { status: "partial", results: [sample(0), sample(1)] }],
  ];

  it.each(states)("draws both ends of the frame on a %s run", async (_state, over) => {
    renderPage(["events"], runBody({ spec: { Type: "tcp", Duration: 60 * s }, pairTotal: 1, ...over }));

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(frame()).toHaveLength(1);
    expect(capStart()).toHaveLength(1);
    expect(capEnd()).toHaveLength(1);
    // The full expected width, whatever arrived: twelve slots either way.
    expect(filled().length + pending().length).toBe(12);
  });

  /* A run created before the planner snapshotted its cadence onto the spec: no
     PlannedSampleIntervalNs, no PlannedSamplesPerPair. runCadence derives them,
     and the frame is drawn off the derivation rather than off the arrivals. */
  it("frames a pre-snapshot run off the derived cadence", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        type: "mtr",
        // 15m of MTR over four pairs on one agent: two batches of 90s a round.
        spec: { Type: "mtr", Duration: 900 * s },
        pairTotal: 4,
        results: [sample(0), sample(1)],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(capStart()).toHaveLength(1);
    expect(capEnd()).toHaveLength(1);
    expect(filled()).toHaveLength(2);
    expect(pending()).toHaveLength(3);
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("2 of ≥5");
  });

  // The regression itself, stated as an invariant: one sample does NOT mean one slot.
  it("never collapses the expected frame onto the arrived count", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: [sample(0)],
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(filled()).toHaveLength(1);
    expect(pending()).toHaveLength(11);
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("1 of ≥12 · ~55s left");
  });

  /* The other end of the same invariant. A healthy duration run starts its next
     round as soon as the previous one finishes, so it produces MORE than the
     floor -- the frame widens to hold every arrival, and the caption still names
     the floor that was planned rather than pretending fifteen were. */
  it("widens the frame for a run that overshot its floor, without overflowing it", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: Array.from({ length: 15 }, (_, i) => sample(i)),
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(filled()).toHaveLength(15);
    expect(pending()).toHaveLength(0);
    // ≥12, not ≥15: the plan was twelve and three more turned up.
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("15 of ≥12");
    expect(capStart()).toHaveLength(1);
    expect(capEnd()).toHaveLength(1);
  });

  // An instant run has no frame because it has no timeline — restated here so
  // the frame work above cannot quietly grow one.
  it("draws no frame at all for an instant run", async () => {
    renderPage(
      ["events"],
      runBody({ status: "succeeded", spec: { Type: "tcp" }, pairTotal: 1, results: [sample(0)] }),
    );

    expect(await screen.findByRole("heading", { name: "Pairs" })).toBeInTheDocument();
    expect(frame()).toHaveLength(0);
  });

  // No frame theatre around a single dot, and the caps go with the frame.
  it("gives a one-slot track no caps either", async () => {
    renderPage(
      ["events"],
      runBody({ status: "succeeded", spec: { Type: "tcp", Duration: 5 * s }, pairTotal: 1, results: [sample(0)] }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(frame()).toHaveLength(0);
    expect(capStart()).toHaveLength(0);
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
      screen.getByRole("img", { name: "node-a to node-b: 11 of at least 12 probes recorded, 1 more probe still to come" }),
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
    expect(screen.getByTestId("timeline-progress")).toHaveTextContent("9 из ≥12 · осталось ~15 с");
    expect(screen.queryAllByTestId("timeline-slot-pending")).toHaveLength(3);
    // The summary card's spans localise with it: «1 мин» duration, «5 с по плану» cadence.
    expect(screen.getByText("1 мин")).toBeInTheDocument();
    expect(screen.getByText("5 с по плану")).toBeInTheDocument();
    expect(screen.queryByText("1m")).not.toBeInTheDocument();
    // countForm's `.few` branch: 3 → «зонда», not «зондов».
    expect(
      screen.getByRole("img", { name: "node-a → node-b: записано 9 зондов из не меньше чем 12, ждём ещё 3 зонда" }),
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

/* ── the owner's 90-pair run: both lists get pages ──────────────────────── */

describe("a run with many pairs is PAGED, not truncated", () => {
  const s = 1_000_000_000;

  /** `count` pairs, in a stable order the pager must not disturb. */
  function manyPairs(count: number, samplesEach = 1) {
    return Array.from({ length: count }, (_, p) =>
      Array.from({ length: samplesEach }, (_, i) => ({
        sourceNode: "node-a",
        destinationNode: `node-${String(p).padStart(2, "0")}`,
        success: true,
        durationNs: 2_000_000,
        recordedAt: "2026-07-28T10:00:00Z",
        sampleSeq: i,
      })),
    ).flat();
  }

  /** Every pager on the page, in DOM order: the pair table's, then the timeline's. */
  const pagers = () => screen.getAllByTestId("pager");
  const showing = () => screen.getAllByTestId("pager-showing").map((el) => el.textContent);

  it("cuts the Pairs table into pages and says how much of it is on screen", async () => {
    renderPage(["events"], runBody({ status: "succeeded", pairTotal: 90, results: manyPairs(90) }));

    expect(await screen.findByRole("heading", { name: "Pairs" })).toBeInTheDocument();
    // One row per pair, ten of the ninety, plus the header row.
    expect(screen.getAllByRole("row")).toHaveLength(11);
    expect(showing()[0]).toBe("Showing 10 of 90 pairs");
  });

  /* M4-3: the results table is the dense ui/table variant and its identifiers
     wear the data face — pinned here so a revert to per-page padding shows. */
  it("renders the pairs through the dense table primitive, identifiers in mono-data", async () => {
    renderPage(["events"], runBody({ status: "succeeded", pairTotal: 90, results: manyPairs(90) }));
    expect(await screen.findByRole("heading", { name: "Pairs" })).toBeInTheDocument();

    const pairCell = screen.getByTitle("node-00").closest("td") as HTMLTableCellElement;
    expect(pairCell.className).toMatch(/\bmono-data\b/);
    expect(pairCell.className).toMatch(/\bpy-1\.5\b/);
  });

  it("reaches the pairs a fixed limit used to hide, in the SAME order", async () => {
    renderPage(["events"], runBody({ status: "succeeded", pairTotal: 90, results: manyPairs(90) }));
    expect(await screen.findByRole("heading", { name: "Pairs" })).toBeInTheDocument();

    // node-89 is the last pair the run produced and was unreachable before.
    expect(screen.queryByTitle("node-89")).not.toBeInTheDocument();
    // Nine pages of ten; the ninth is the one the old fixed limit cut off.
    for (let i = 0; i < 8; i++) {
      fireEvent.click(within(pagers()[0]).getByRole("button", { name: "Next page" }));
    }

    expect(screen.getByTitle("node-89")).toBeInTheDocument();
    expect(screen.getAllByRole("row")).toHaveLength(11);
    expect(showing()[0]).toBe("Showing 10 of 90 pairs");
    // Order is the server's, page or no page: page 9 opens at node-80.
    expect(screen.getAllByRole("row")[1].textContent).toContain("node-80");
  });

  it("pages the probe timeline too, and draws a page-worth of strips rather than a dozen", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 30,
        results: manyPairs(30, 12),
      }),
    );

    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();
    expect(screen.getAllByTestId("timeline-progress")).toHaveLength(10);
    expect(showing()[1]).toBe("Showing 10 of 30 pairs");
    // The old "12 more pairs are not drawn here" dead end is gone.
    expect(screen.queryByText(/not drawn here/)).not.toBeInTheDocument();
  });

  it("keeps the first strip on screen when the timeline's page size changes", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 30,
        results: manyPairs(30, 12),
      }),
    );
    expect(await screen.findByText("Probe timeline")).toBeInTheDocument();

    // Scoped to the timeline's own card: the pair table above it draws the same
    // node names and its own pager.
    const section = () => screen.getByText("Probe timeline").closest("section") as HTMLElement;
    const pager = () => within(section()).getByTestId("pager");
    const firstStrip = () => within(section()).getAllByTitle(/^node-\d\d$/)[0];

    fireEvent.click(within(pager()).getByRole("button", { name: "Next page" }));
    // Page 2 of three at ten a page opens on the eleventh pair.
    expect(firstStrip()).toHaveTextContent("node-10");

    fireEvent.click(within(pager()).getByRole("radio", { name: "20" }));
    // Pair #10 lives on page 1 at twenty a page, so that is where the reader stays.
    expect(within(pager()).getByTestId("pager-page")).toHaveTextContent("Page 1 of 2");
    expect(firstStrip()).toHaveTextContent("node-00");
    expect(screen.getAllByTestId("timeline-progress")).toHaveLength(20);
  });

  it("draws no pager at all for a run whose pairs already fit", async () => {
    renderPage(["events"], runBody({ status: "succeeded", pairTotal: 4, results: manyPairs(4) }));
    expect(await screen.findByRole("heading", { name: "Pairs" })).toBeInTheDocument();
    expect(screen.queryByTestId("pager")).not.toBeInTheDocument();
  });
});

/* ── «вся суть MTR — это путь», and «ничего не кликабельно» ──────────────── */

/**
 * A run permalink for an MTR listed its pairs, their durations and their
 * outcomes, and nothing at all about where the packets went — while the one
 * thing an MTR exists to produce is the route. Nothing on the page opened.
 *
 * The results themselves carry no hops (check_runs stores an outcome, not a
 * trace), so a pair row reads the route back out of the MTR projection when the
 * reader asks for it, and a probe's tick reads the route that covers its own
 * instant.
 */
describe("a run's pairs open onto the route they took", () => {
  const s = 1_000_000_000;

  const hop = (n: number, ip: string) => ({ number: n, ip, hostname: "", rttNs: 2_000_000, lossRatio: 0 });

  const snapshotsBody = (over: Record<string, unknown> = {}) =>
    json({
      snapshots: [
        {
          id: "snap-1",
          sourceNode: "node-a",
          destination: "node-b",
          pathHash: "aaaaaaaaaaaa0000",
          hopCount: 2,
          hops: [hop(1, "10.244.9.17"), hop(2, "10.0.0.9")],
          firstSeen: "2026-07-28T10:00:00Z",
          lastSeen: "2026-07-28T10:30:00Z",
          traceCount: 3,
          ...over,
        },
      ],
      nextCursor: "",
    });

  const mtrRun = (over: Partial<RunDetail> = {}) =>
    runBody({
      status: "succeeded",
      type: "mtr",
      spec: { Type: "mtr" },
      pairTotal: 1,
      results: [
        {
          sourceNode: "node-a",
          destinationNode: "node-b",
          success: true,
          durationNs: 2 * s,
          recordedAt: "2026-07-28T10:10:00Z",
          sampleSeq: 0,
        },
      ],
      ...over,
    });

  const expander = () => screen.getByRole("button", { name: /show the route from node-a to node-b/i });

  it("gives every pair row an expander, shut on arrival", async () => {
    renderPage(["events"], mtrRun(), RUN_ID, { onSnapshots: snapshotsBody });

    expect(await screen.findByRole("heading", { name: "Pairs" })).toBeInTheDocument();
    expect(expander()).toHaveAttribute("aria-expanded", "false");
    // Nothing fetched for a row nobody opened.
    expect(screen.queryByText("10.244.9.17")).not.toBeInTheDocument();
  });

  it("opens onto the pair's recorded route, drawn by the Explorer's own hop table", async () => {
    renderPage(["events"], mtrRun(), RUN_ID, { onSnapshots: snapshotsBody });
    await screen.findByRole("heading", { name: "Pairs" });

    fireEvent.click(expander());
    expect(expander()).toHaveAttribute("aria-expanded", "true");
    expect(await screen.findByText("10.244.9.17")).toBeInTheDocument();
    expect(screen.getByText("10.0.0.9")).toBeInTheDocument();
  });

  it("offers the way out to the page that exists for paths, deep-linked to this pair", async () => {
    renderPage(["events"], mtrRun(), RUN_ID, { onSnapshots: snapshotsBody });
    await screen.findByRole("heading", { name: "Pairs" });
    fireEvent.click(expander());

    const link = await screen.findByRole("link", { name: "Open in MTR Explorer" });
    // Both halves: the destination opens the card, the source picks the pair.
    expect(link).toHaveAttribute("href", "/mtr?source=node-a&destination=node-b");
  });

  it("says so when the projection has no route for the pair, rather than drawing an empty table", async () => {
    renderPage(["events"], mtrRun(), RUN_ID, { onSnapshots: () => json({ snapshots: [], nextCursor: "" }) });
    await screen.findByRole("heading", { name: "Pairs" });
    fireEvent.click(expander());

    expect(await screen.findByText("No route recorded for this pair yet.")).toBeInTheDocument();
  });

  it("opens a NON-MTR pair onto the sample's own facts, with the error whole", async () => {
    const long = "dial tcp 10.0.0.9:80: i/o timeout after 2s (context deadline exceeded)";
    renderPage(
      ["events"],
      runBody({
        status: "failed",
        type: "tcp",
        spec: { Type: "tcp" },
        pairTotal: 1,
        results: [
          {
            sourceNode: "node-a",
            destinationNode: "node-b",
            success: false,
            durationNs: 2 * s,
            error: long,
            recordedAt: "2026-07-28T10:10:00Z",
            sampleSeq: 0,
          },
        ],
      }),
    );
    await screen.findByRole("heading", { name: "Pairs" });
    fireEvent.click(expander());

    // No route for a tcp probe — there is none — but the whole error, and the
    // facts the cells above abbreviate.
    expect(await screen.findByText("From")).toBeInTheDocument();
    expect(screen.getAllByText(long).length).toBeGreaterThan(0);
    expect(screen.queryByRole("link", { name: "Open in MTR Explorer" })).not.toBeInTheDocument();
  });
});

describe("an MTR run's probe ticks open the trace they walked", () => {
  const s = 1_000_000_000;
  const hop = (n: number, ip: string) => ({ number: n, ip, hostname: "", rttNs: 2_000_000, lossRatio: 0 });

  const intervalMTR = runBody({
    status: "succeeded",
    type: "mtr",
    spec: { Type: "mtr", Duration: 900 * s, PlannedSampleIntervalNs: 90 * s, PlannedSamplesPerPair: 2 },
    pairTotal: 1,
    results: [0, 1].map((i) => ({
      sourceNode: "node-a",
      destinationNode: "node-b",
      success: true,
      durationNs: 2 * s,
      recordedAt: i === 0 ? "2026-07-28T10:10:00Z" : "2026-07-28T23:00:00Z",
      sampleSeq: i,
    })),
  });

  const snapshots = () =>
    json({
      snapshots: [
        {
          id: "snap-1",
          sourceNode: "node-a",
          destination: "node-b",
          pathHash: "aaaaaaaaaaaa0000",
          hopCount: 1,
          hops: [hop(1, "10.244.9.17")],
          firstSeen: "2026-07-28T10:00:00Z",
          lastSeen: "2026-07-28T10:30:00Z",
          traceCount: 3,
        },
      ],
      nextCursor: "",
    });

  it("makes each tick a control rather than a coloured square", async () => {
    renderPage(["events"], intervalMTR, RUN_ID, { onSnapshots: snapshots });
    await screen.findByText("Probe timeline");

    const ticks = screen.getAllByTestId("timeline-slot-filled");
    expect(ticks[0].tagName).toBe("BUTTON");
    expect(ticks[0].getAttribute("aria-label")).toMatch(/show the route this probe took/i);
  });

  it("opens the route that covers THAT probe's instant", async () => {
    renderPage(["events"], intervalMTR, RUN_ID, { onSnapshots: snapshots });
    await screen.findByText("Probe timeline");

    fireEvent.click(screen.getAllByTestId("timeline-slot-filled")[0]);
    expect(await screen.findByText("10.244.9.17")).toBeInTheDocument();
  });

  /* The owner clicked tick after tick and read the same panel every time: the route is a property
     of the PATH, not of the probe, and while the path holds it does not change. What changes is the
     probe, so the probe is what the panel now leads with — and the reason the hops repeat is said
     out loud instead of left to be inferred from a control that looks stuck. */
  it("leads with the PROBE the tick belongs to, so two ticks on one route are still told apart", async () => {
    const twoInWindow = runBody({
      status: "succeeded",
      type: "mtr",
      spec: { Type: "mtr", Duration: 900 * s, PlannedSampleIntervalNs: 90 * s, PlannedSamplesPerPair: 2 },
      pairTotal: 1,
      results: [
        {
          sourceNode: "node-a",
          destinationNode: "node-b",
          success: true,
          durationNs: 2 * s,
          recordedAt: "2026-07-28T10:10:00Z",
          sampleSeq: 0,
        },
        {
          sourceNode: "node-a",
          destinationNode: "node-b",
          success: true,
          durationNs: 4 * s,
          recordedAt: "2026-07-28T10:20:00Z",
          sampleSeq: 1,
        },
      ],
    });
    renderPage(["events"], twoInWindow, RUN_ID, { onSnapshots: snapshots });
    await screen.findByText("Probe timeline");

    fireEvent.click(screen.getAllByTestId("timeline-slot-filled")[0]);
    const first = await screen.findByTestId("trace-probe");
    expect(first).toHaveTextContent("Probe #0");
    expect(first).toHaveTextContent("2000ms");
    // And the sentence that explains why the hops under the next tick will look the same.
    expect(await screen.findByText(/while the route holds, every probe reads the same/i)).toBeInTheDocument();

    fireEvent.click(screen.getAllByTestId("timeline-slot-filled")[1]);
    const second = await screen.findByTestId("trace-probe");
    expect(second).toHaveTextContent("Probe #1");
    expect(second).toHaveTextContent("4000ms");
    expect(second).not.toHaveTextContent("Probe #0");
    // Same route under both, which is the honest answer — and both still show it. Awaited because
    // opening a tick re-asks for the pair's routes: a running MTR run projects new ones behind the
    // probes, and a list fetched once at open goes stale within a cadence.
    expect(await screen.findByText("10.244.9.17")).toBeInTheDocument();
  });

  // A probe that timed out or never left the dispatcher walked NO route. It still has a recorded_at
  // that a stored route's window can cover, so the clock alone would put a hop table under it
  // captioned as the route it took — a confident lie about a probe that never completed a trace.
  it("gives a FAILED probe no route at all, whatever the clock would match", async () => {
    const failed = runBody({
      status: "succeeded",
      type: "mtr",
      spec: { Type: "mtr", Duration: 900 * s, PlannedSampleIntervalNs: 90 * s, PlannedSamplesPerPair: 2 },
      pairTotal: 1,
      results: [
        {
          sourceNode: "node-a",
          destinationNode: "node-b",
          success: false,
          error: "no agent on node-a",
          durationNs: 90 * s,
          recordedAt: "2026-07-28T10:10:00Z", // inside snapshot snap-1's window
          sampleSeq: 0,
        },
      ],
    });
    renderPage(["events"], failed, RUN_ID, { onSnapshots: snapshots });
    await screen.findByText("Probe timeline");

    fireEvent.click(screen.getAllByRole("button", { name: /show the route this probe took/i })[0]);

    expect(await screen.findByText(/this probe recorded no route/i)).toBeInTheDocument();
    expect(screen.queryByText("10.244.9.17")).not.toBeInTheDocument();
    // The probe itself is still named, with its error where a latency would be.
    expect(screen.getByTestId("trace-probe")).toHaveTextContent("no agent on node-a");
  });

  it("refuses to show a route for a probe no stored path covers", async () => {
    renderPage(["events"], intervalMTR, RUN_ID, { onSnapshots: snapshots });
    await screen.findByText("Probe timeline");

    // The second probe was recorded at 23:00, long past the stored window.
    fireEvent.click(screen.getAllByTestId("timeline-slot-filled")[1]);
    expect(await screen.findByText("No recorded route covers this probe.")).toBeInTheDocument();
    expect(screen.queryByText("10.244.9.17")).not.toBeInTheDocument();
  });

  /* The hop table's SECOND mount point. The Explorer's Trace pane is one third
     of a three-column grid and this one spans the whole permalink, so the two
     are nowhere near the same width — «hostname обрезается» has to be answered
     in the narrow one as well as here. Same component, same column contract:
     the name arrives whole, in the markup and in the title. */
  it("shows a pod's full rDNS name in the expanded pair row's hop table", async () => {
    const POD_RDNS = "10-244-4-21.kconmon-kconmon-ng-agents.kconmon.svc.cluster.local";
    renderPage(["events"], intervalMTR, RUN_ID, {
      onSnapshots: () =>
        json({
          snapshots: [
            {
              id: "snap-1",
              sourceNode: "node-a",
              destination: "node-b",
              pathHash: "aaaaaaaaaaaa0000",
              hopCount: 1,
              hops: [{ number: 1, ip: "10.244.4.21", hostname: POD_RDNS, rttNs: 2_000_000, lossRatio: 0 }],
              firstSeen: "2026-07-28T10:00:00Z",
              lastSeen: "2026-07-28T10:30:00Z",
              traceCount: 3,
            },
          ],
          nextCursor: "",
        }),
    });
    await screen.findByText("Probe timeline");

    fireEvent.click(screen.getAllByTestId("timeline-slot-filled")[0]);

    const cell = await screen.findByText(POD_RDNS);
    expect(cell).toHaveAttribute("title", POD_RDNS);
    expect(cell.textContent).toBe(POD_RDNS);
    // Not clipped down to a tooltip in the narrow pane either.
    expect(cell.className).not.toMatch(/\btruncate\b/);
  });

  it("leaves a NON-MTR run's ticks as the plain squares they were", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        type: "tcp",
        spec: { Type: "tcp", Duration: 60 * s },
        pairTotal: 1,
        results: [0, 1].map((i) => ({
          sourceNode: "node-a",
          destinationNode: "node-b",
          success: true,
          durationNs: 2 * s,
          recordedAt: "2026-07-28T10:10:00Z",
          sampleSeq: i,
        })),
      }),
    );
    await screen.findByText("Probe timeline");
    expect(screen.getAllByTestId("timeline-slot-filled")[0].tagName).toBe("SPAN");
  });
});

/*
 * A long interval run holds more results than one response may carry, and every figure on this page
 * is computed from the results it was handed. Presenting a tail as the whole run is the same class
 * of lie as an unbounded read is a risk, so the page says which it is looking at.
 */
it("says so when the response carries only the newest slice of a run", async () => {
  const body = runBody({
    status: "running",
    type: "tcp",
    spec: { Type: "tcp", Duration: 900 * 1_000_000_000 },
    pairTotal: 4,
    results: [
      { sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 2_000_000, recordedAt: "2026-07-28T10:10:00Z", sampleSeq: 0 },
    ],
  });
  renderPage(["events"], { ...body, resultsTruncated: true }, RUN_ID);

  expect(await screen.findByText(/most recent results/i)).toBeInTheDocument();
});

/*
 * A truncated response carries the run's newest slice; the run's SPEC still describes the whole run.
 * Framing the strip against the plan drew `planned - arrived` empty slots that read as
 * "undispatched" — for probes that ran, succeeded, and were dropped by the store's cap. A finished
 * run showed as a few percent delivered.
 */
it("does not frame the sample strip against the plan when the results are a tail", async () => {
  const s = 1_000_000_000;
  const body = runBody({
    status: "succeeded",
    type: "tcp",
    spec: { Type: "tcp", Duration: 3600 * s, PlannedSampleIntervalNs: 7 * s, PlannedSamplesPerPair: 500 },
    pairTotal: 1,
    results: Array.from({ length: 3 }, (_, i) => ({
      sourceNode: "node-a",
      destinationNode: "node-b",
      success: true,
      durationNs: 2_000_000,
      recordedAt: new Date(Date.parse("2026-07-28T10:00:00Z") + i * 7000).toISOString(),
      sampleSeq: i,
    })),
  });
  renderPage(["events"], { ...body, resultsTruncated: true }, RUN_ID);
  await screen.findByText("Probe timeline");

  // Three samples, three ticks: no pending slots invented from a plan this slice cannot be measured
  // against.
  expect(screen.queryAllByTestId("timeline-slot-pending")).toHaveLength(0);
  expect(screen.queryAllByTestId("timeline-slot-filled")).toHaveLength(0);
});
