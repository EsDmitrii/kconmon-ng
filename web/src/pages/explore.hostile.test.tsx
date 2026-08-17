import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { CURATED_CHARTS } from "@/lib/curated-metrics";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { ExplorePage } from "./explore";

/**
 * explore.hostile.test.tsx — Explore with Prometheus refusing to co-operate.
 *
 * The page is six charts over five curated queries plus a compare panel, and
 * every one of them is a range query whose answer is out of our hands. Three of
 * the five are `histogram_quantile` over a rate, so "matched nothing" is not an
 * exotic state — it is what a quiet cluster, a fresh install, or a fifteen
 * minute window over a five minute rate all return.
 *
 * The rule under test throughout: an empty answer is a SENTENCE, never a blank
 * pair of axes; a failed answer is an alert, never an empty one.
 */

vi.mock("@/components/echart", () => ({
  EChart: ({ option, className }: { option?: { series?: unknown }; className?: string }) => (
    <div
      data-testid="echart"
      className={className}
      data-series-count={((option?.series ?? []) as unknown[]).length}
    />
  ),
}));

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const MATRIX = {
  status: "success",
  data: { resultType: "matrix", result: [{ metric: { host: "h1" }, values: [[1_785_283_200, "1"], [1_785_283_260, "2"]] }] },
};
const EMPTY_MATRIX = { status: "success", data: { resultType: "matrix", result: [] } };

/** `answer` may be a function, so a case can answer differently per query. */
type Answer = unknown | ((query: string) => unknown);

function stubFetch(answer: Answer, status = 200) {
  const bodies: { query: string; start: string; end: string; step: number }[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    if (href.includes("/api/v1/promql/query_range")) {
      const body = JSON.parse(String(init?.body)) as { query: string; start: string; end: string; step: number };
      bodies.push(body);
      const out = typeof answer === "function" ? (answer as (q: string) => unknown)(body.query) : answer;
      return Promise.resolve(json(out, status));
    }
    // Annotations and maintenance windows: Explore asks for both on every range.
    if (href.includes("/api/v1/annotations") || href.includes("/api/v1/maintenance")) {
      return Promise.resolve(json([]));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { bodies, fetchMock };
}

function renderPage(locale?: Locale) {
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = <ExplorePage />;
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          {locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

const panel = () => screen.getByRole("region", { name: "Compare" });
const compareChart = () => within(panel()).queryByTestId("echart");
const setSelect = (label: string, value: string) =>
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
const pickSelf = () => fireEvent.click(screen.getByRole("radio", { name: "itself, earlier" }));
const pickMetric = () => fireEvent.click(screen.getByRole("radio", { name: "another metric" }));

beforeEach(() => {
  window.history.pushState({}, "", "/explore");
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

/* ── an empty Prometheus ─────────────────────────────────────────────────── */

describe("a Prometheus with nothing in it says so on every panel", () => {
  it("gives each curated card the empty sentence rather than blank axes", async () => {
    stubFetch(EMPTY_MATRIX);
    renderPage();

    await waitFor(() =>
      expect(screen.getAllByText(/no series returned for this range/i)).toHaveLength(CURATED_CHARTS.length),
    );
    // Not one of the five drew a chart with nothing in it.
    expect(screen.queryAllByTestId("echart")).toHaveLength(0);
  });

  /**
   * The finding: the COMPARE panel had no empty state at all. Leg A answering
   * with an empty matrix still produced an option, so the panel drew a chart
   * with zero series — a labelled, gridded, entirely blank rectangle above five
   * cards that were all honestly saying "no series returned". The reader is
   * told nothing, on the one panel they chose the contents of themselves.
   */
  it("gives the COMPARE panel the same sentence instead of an empty rectangle", async () => {
    stubFetch(EMPTY_MATRIX);
    renderPage();
    setSelect("Compare with metric", CURATED_CHARTS[1].id);

    await waitFor(() => expect(within(panel()).getByText(/no series returned for this range/i)).toBeInTheDocument());
    expect(compareChart()).toBeNull();
  });

  it("still draws the compare panel when ONE of the two legs has data", async () => {
    // A empty, B not: the legend names the leg that is drawn, so the chart is
    // still the honest answer — what must not happen is dropping it.
    stubFetch((q: string) => (q.startsWith(CURATED_CHARTS[0].query.split("$__range")[0]) ? EMPTY_MATRIX : MATRIX));
    renderPage();
    setSelect("Compare with metric", CURATED_CHARTS[1].id);

    await waitFor(() => expect(compareChart()).not.toBeNull());
    expect(compareChart()).toHaveAttribute("data-series-count", "1");
  });
});

describe("a Prometheus that is answering badly", () => {
  it("shows Prometheus's own error on every card, and no empty note beside it", async () => {
    stubFetch({ status: "error", errorType: "bad_data", error: "expanding series: out of order sample" });
    renderPage();

    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThanOrEqual(CURATED_CHARTS.length));
    expect(screen.queryByText(/no series returned for this range/i)).not.toBeInTheDocument();
    expect(screen.queryAllByTestId("echart")).toHaveLength(0);
  });

  it("survives an upstream 500 without an empty note and without a chart", async () => {
    stubFetch({ type: "about:blank", title: "prometheus unreachable", status: 500 }, 500);
    renderPage();

    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    expect(screen.queryByText(/no series returned for this range/i)).not.toBeInTheDocument();
  });

  const malformed: [string, unknown][] = [
    ["a vector where a matrix belongs", { status: "success", data: { resultType: "vector", result: [] } }],
    ["a result that is null", { status: "success", data: { resultType: "matrix", result: null } }],
    ["an entry with no metric", { status: "success", data: { resultType: "matrix", result: [{ values: [[1, "1"]] }] } }],
    ["an entry with no values", { status: "success", data: { resultType: "matrix", result: [{ metric: { host: "h" } }] } }],
    ["no data field at all", { status: "success" }],
  ];

  for (const [name, answer] of malformed) {
    it(`stays a page for ${name}`, async () => {
      stubFetch(answer);
      renderPage();

      await waitFor(() => expect(screen.getByRole("heading", { name: "Explore" })).toBeInTheDocument());
      // The range picker is still a range picker; nothing escaped render.
      expect(screen.getByRole("radio", { name: "24h" })).toBeInTheDocument();
      expect(document.body.textContent ?? "").not.toContain("NaN");
      expect(document.body.textContent ?? "").not.toContain("undefined");
    });
  }
});

/* ── the compare panel, worked hard ──────────────────────────────────────── */

describe("the compare panel under a switching storm", () => {
  it("survives twenty mode flips and still holds the B metric the reader chose", async () => {
    stubFetch(MATRIX);
    renderPage();
    setSelect("Compare with metric", CURATED_CHARTS[2].id);
    await waitFor(() => expect(compareChart()).not.toBeNull());

    for (let i = 0; i < 20; i++) {
      pickSelf();
      pickMetric();
    }

    // The picker came back holding what it held, and the panel is still drawing.
    expect((screen.getByLabelText("Compare with metric") as HTMLSelectElement).value).toBe(CURATED_CHARTS[2].id);
    await waitFor(() => expect(compareChart()).not.toBeNull());
  });

  it("never draws the same metric twice when A is moved onto B", async () => {
    stubFetch(MATRIX);
    renderPage();
    setSelect("Compare with metric", CURATED_CHARTS[1].id);
    await waitFor(() => expect(compareChart()).not.toBeNull());
    expect(compareChart()).toHaveAttribute("data-series-count", "2");

    // A becomes what B already was. One metric cannot be compared with itself
    // by drawing the identical line twice.
    setSelect("Metric A", CURATED_CHARTS[1].id);
    await waitFor(() => expect(screen.getByText(/pick a second metric/i)).toBeInTheDocument());
    expect(compareChart()).toBeNull();
  });

  it("walks every shift option without a stray request or a broken window", async () => {
    const { bodies } = stubFetch(MATRIX);
    renderPage();
    pickSelf();

    for (const shift of ["1h", "24h", "7d", ""]) {
      setSelect("Compare with earlier", shift);
      await waitFor(() => expect(screen.getByRole("region", { name: "Compare" })).toBeInTheDocument());
    }

    // Every window that was asked for is a real window: start before end, and a
    // step Prometheus can parse.
    for (const b of bodies) {
      expect(Date.parse(b.start)).toBeLessThan(Date.parse(b.end));
      expect(Number.isFinite(b.step)).toBe(true);
      expect(b.step).toBeGreaterThan(0);
      expect(b.query).not.toContain("$__range");
      expect(b.query).not.toContain("NaN");
    }
  });

  it("says the reference window is empty rather than implying the two legs match", async () => {
    // The shifted leg reaches past retention: A is drawn, B is nothing, and the
    // silent reading would be "identical to a week ago".
    stubFetch(MATRIX);
    renderPage();
    pickSelf();
    setSelect("Compare with earlier", "7d");
    await waitFor(() => expect(compareChart()).not.toBeNull());

    cleanup();
    let first = true;
    stubFetch(() => {
      // The unshifted leg answers first; the shifted one comes back empty.
      const out = first ? MATRIX : EMPTY_MATRIX;
      first = false;
      return out;
    });
    renderPage();
    pickSelf();
    setSelect("Compare with earlier", "7d");
    await waitFor(() => expect(screen.getByText(/retention does not reach that far back/i)).toBeInTheDocument());
  });
});

describe("the range picker under a storm", () => {
  it("takes every window in turn and ends on the one that was clicked last", async () => {
    const { bodies } = stubFetch(MATRIX);
    renderPage();

    for (const r of ["15m", "6h", "24h", "1h", "15m", "24h"]) {
      fireEvent.click(screen.getByRole("radio", { name: r }));
    }
    await waitFor(() => expect(bodies.length).toBeGreaterThan(CURATED_CHARTS.length));

    expect(screen.getByRole("radio", { name: "24h" })).toBeChecked();
    // Every request the storm produced still describes a real window.
    const last = bodies.at(-1)!;
    expect(Date.parse(last.end) - Date.parse(last.start)).toBeGreaterThan(0);
    for (const b of bodies) expect(b.query).not.toContain("$__range");
  });

  it("asks for exactly 24h at the top end — the proxy's own maxRange, not a millisecond over", async () => {
    const { bodies } = stubFetch(MATRIX);
    renderPage();
    fireEvent.click(screen.getByRole("radio", { name: "24h" }));

    await waitFor(() => expect(bodies.some((b) => Date.parse(b.end) - Date.parse(b.start) === 86_400_000)).toBe(true));
    for (const b of bodies) expect(Date.parse(b.end) - Date.parse(b.start)).toBeLessThanOrEqual(86_400_000);
  });
});

/* ── the URL, and the annotations the page carries ───────────────────────── */

describe("a hand-edited ?at= cannot break the page", () => {
  /* Explore puts `at` in a header sentence via toLocaleString AND in every
     query key via toISOString — the second throws a RangeError on an invalid
     Date, which would take the page down before a chart was ever drawn. */
  for (const raw of ["not-a-date", "2026-08-01", "<script>alert(1)</script>", "%%%%", "9999-99-99T00:00:00Z"]) {
    it(`stays Live and still fetches for ?at=${raw}`, async () => {
      window.history.pushState({}, "", `/explore?at=${encodeURIComponent(raw)}`);
      const { bodies } = stubFetch(MATRIX);
      renderPage();

      await waitFor(() => expect(bodies.length).toBeGreaterThan(0));
      expect(document.body.textContent ?? "").not.toContain("Invalid Date");
      expect(screen.getByRole("heading", { name: "Explore" })).toBeInTheDocument();
    });
  }
});

describe("somebody else's text on the page", () => {
  /* Annotations and maintenance reasons are operator-written free text that
     arrives from the API and lands beside every chart on this page. It renders
     as TEXT — a marker label ECharts paints onto a canvas, or a DOM text node —
     never as markup. */
  it("renders a script-shaped annotation as text, and mounts no script", async () => {
    const nasty = "<script>alert('xss')</script>";
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.includes("/api/v1/promql/query_range")) return Promise.resolve(json(MATRIX));
        if (href.includes("/api/v1/annotations")) {
          return Promise.resolve(
            json([{ id: "a1", scope: "global", text: nasty, startsAt: new Date().toISOString() }]),
          );
        }
        if (href.includes("/api/v1/maintenance")) return Promise.resolve(json([]));
        return Promise.resolve(json({}));
      }),
    );
    renderPage();

    await waitFor(() => expect(screen.getAllByTestId("echart").length).toBeGreaterThan(0));
    expect(document.querySelector("script")).toBeNull();
    expect(document.body.innerHTML).not.toContain("<script>alert");
  });

  it("survives the annotations endpoint failing outright, charts and all", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const href = String(url);
        if (href.includes("/api/v1/promql/query_range")) return Promise.resolve(json(MATRIX));
        if (href.includes("/api/v1/annotations") || href.includes("/api/v1/maintenance")) {
          return Promise.resolve(json({ type: "about:blank", title: "store unavailable", status: 503 }, 503));
        }
        return Promise.resolve(json({}));
      }),
    );
    renderPage();

    // The charts are a different fact from the annotations, and they still draw.
    await waitFor(() => expect(screen.getAllByTestId("echart").length).toBeGreaterThan(0));
    expect(screen.getByRole("heading", { name: "Explore" })).toBeInTheDocument();
  });
});

/* ── Russian ─────────────────────────────────────────────────────────────── */

describe("the hostile paths speak Russian too", () => {
  it("says the compare panel is empty in Russian rather than drawing a blank chart", async () => {
    stubFetch(EMPTY_MATRIX);
    renderPage("ru");
    setSelect("Сравнить с метрикой", CURATED_CHARTS[1].id);

    await waitFor(() =>
      expect(within(screen.getByRole("region", { name: "Сравнение" })).getByText(/серий нет/i)).toBeInTheDocument(),
    );
  });
});
