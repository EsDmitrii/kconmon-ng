import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { PromQLConsolePage } from "./promql-console";

/**
 * promql-console.hostile.test.tsx — the Console driven the way an operator
 * actually drives it: wrong queries, wrong shapes, wrong order, twice at once.
 *
 * The page is a PromQL box wired straight to Prometheus, so the result shape is
 * not ours to assume. Prometheus answers an instant query with FOUR result types
 * — vector, matrix, scalar and string — and the string one comes back from an
 * expression as ordinary as `"hello"`. Everything below is a shape the guarded
 * proxy will hand this page verbatim.
 */

vi.mock("@/components/promql-editor", () => ({
  PromQLEditor: ({ initial }: { initial?: string }) => (
    <div data-testid="promql-editor" data-initial={initial} />
  ),
}));

const captured = vi.hoisted(() => ({ options: [] as Record<string, unknown>[] }));
vi.mock("@/components/echart", () => ({
  EChart: ({ option }: { option: Record<string, unknown> }) => {
    captured.options.push(option);
    return <div data-testid="echart" />;
  },
}));

const lastOption = () =>
  captured.options.at(-1) as { series?: { name: string; showSymbol?: boolean; data?: unknown[] }[] };

/** The key pages/promql-console.tsx persists the last query under. Not exported
 *  by the page — it is an implementation detail everywhere except here, where
 *  the point is to poison it from outside. */
const LAST_QUERY_KEY = "kconmon.promql.lastQuery";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

interface PromCall {
  url: string;
  body: { query: string; time?: string; start?: string; end?: string; step?: number };
}

function renderConsole(
  opts: { answer?: unknown; defer?: boolean; locale?: Locale; httpStatus?: number; body?: string } = {},
) {
  const {
    answer = { status: "success", data: { resultType: "vector", result: [] } },
    defer = false,
    locale,
    httpStatus = 200,
    body,
  } = opts;
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  window.history.pushState({}, "", "/console");
  const calls: PromCall[] = [];
  /** Held responses, so a case can act while a query is still in flight. */
  const pending: (() => void)[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    if (!href.includes("/api/v1/promql/")) return Promise.resolve(json({}));
    calls.push({ url: href, body: JSON.parse(String(init?.body ?? "{}")) });
    const make = () =>
      body !== undefined
        ? new Response(body, { status: httpStatus, headers: { "Content-Type": "application/json" } })
        : httpStatus !== 200
          ? new Response(
              JSON.stringify({ type: "about:blank", title: "upstream refused", status: httpStatus }),
              { status: httpStatus, headers: { "Content-Type": "application/problem+json" } },
            )
          : json(answer);
    if (!defer) return Promise.resolve(make());
    return new Promise<Response>((resolve) => pending.push(() => resolve(make())));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = <PromQLConsolePage />;
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          {locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...utils, calls, release: () => pending.splice(0).forEach((f) => f()) };
}

const runButton = () => screen.getByRole("button", { name: "Run" });
const run = () => fireEvent.click(runButton());
const pickRange = () => fireEvent.click(screen.getByRole("radio", { name: "Range" }));
const tab = (name: string) => screen.getByRole("tab", { name });

afterEach(() => {
  cleanup();
  captured.options.length = 0;
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
  localStorage.removeItem(LOCALE_STORAGE_KEY);
  localStorage.removeItem(LAST_QUERY_KEY);
});

/* ── the four result types Prometheus actually has ───────────────────────── */

describe("every result type Prometheus can answer with is survivable", () => {
  /**
   * The crash: `query="hello"` is legal PromQL and answers
   * `{"resultType":"string","result":[1754000000,"hello"]}`. The table builder
   * walked that as a list of vector entries, reached for `.metric` on the
   * timestamp, and threw a TypeError out of render — the whole page white.
   */
  it("renders a STRING result instead of throwing out of render", async () => {
    renderConsole({ answer: { status: "success", data: { resultType: "string", result: [1_754_000_000, "hello"] } } });

    run();
    expect(await screen.findByText("hello")).toBeInTheDocument();
    expect(screen.queryByText(/no data/i)).not.toBeInTheDocument();
  });

  it("renders a SCALAR result", async () => {
    renderConsole({ answer: { status: "success", data: { resultType: "scalar", result: [1_754_000_000, "42"] } } });

    run();
    expect(await screen.findByText("42")).toBeInTheDocument();
  });

  it("renders a vector, labels and value", async () => {
    renderConsole({
      answer: {
        status: "success",
        data: { resultType: "vector", result: [{ metric: { __name__: "up", node: "n1" }, value: [1, "1"] }] },
      },
    });

    run();
    expect(await screen.findByText("n1")).toBeInTheDocument();
  });
});

describe("a malformed envelope is a bad answer, never a broken page", () => {
  const malformed: [string, unknown][] = [
    ["a result that is null", { status: "success", data: { resultType: "matrix", result: null } }],
    ["a vector entry with no metric", { status: "success", data: { resultType: "vector", result: [{ value: [1, "1"] }] } }],
    ["a matrix entry with no values", { status: "success", data: { resultType: "matrix", result: [{ metric: { a: "b" } }] } }],
    ["a resultType nobody has heard of", { status: "success", data: { resultType: "wat", result: [1, 2, 3] } }],
    ["data missing entirely", { status: "success" }],
    ["a result that is an object, not a list", { status: "success", data: { resultType: "vector", result: { a: 1 } } }],
  ];

  for (const [name, answer] of malformed) {
    it(`survives ${name}`, async () => {
      renderConsole({ answer });

      run();
      // Whatever it decides to say, it is still a page: the toolbar is there
      // and nothing escaped render.
      await waitFor(() => expect(runButton()).toBeInTheDocument());
      expect(screen.getByRole("tab", { name: "Table" })).toBeInTheDocument();
    });
  }

  it("survives a body that is not JSON at all", async () => {
    renderConsole({ body: "<html>502 Bad Gateway</html>" });

    run();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(runButton()).toBeInTheDocument();
  });

  it("reports an upstream 500 as an error, not as an empty result", async () => {
    renderConsole({ httpStatus: 500 });

    run();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByText(/no data/i)).not.toBeInTheDocument();
  });
});

/* ── numbers that are not numbers ────────────────────────────────────────── */

describe("NaN and +Inf reach the page and stay Prometheus's own words", () => {
  const weird = {
    status: "success",
    data: {
      resultType: "matrix",
      result: [
        { metric: { __name__: "q", pair: "a" }, values: [[1, "NaN"], [2, "+Inf"]] },
        { metric: { __name__: "q", pair: "b" }, values: [[1, "0.5"], [2, "0.6"]] },
      ],
    },
  };

  it("prints the raw token in the listing rather than a computed NaN", async () => {
    renderConsole({ answer: weird });
    pickRange();
    fireEvent.click(tab("Chart"));
    run();
    await screen.findByTestId("echart");

    // The listing is a reading of Prometheus's own response; "+Inf" is what it
    // said, so "+Inf" is what the row carries.
    expect(screen.getAllByTestId("raw-row")[0]).toHaveTextContent("+Inf");
    // What must never appear is a number we invented out of it.
    expect(screen.getByTestId("raw-table").textContent ?? "").not.toContain("NaN%");
  });

  it("never renders the string 'undefined' anywhere on the page", async () => {
    renderConsole({ answer: weird });
    pickRange();
    fireEvent.click(tab("Chart"));
    run();
    await screen.findByTestId("echart");

    expect(document.body.textContent ?? "").not.toContain("undefined");
  });
});

/* ── the controls, worked against each other ─────────────────────────────── */

describe("Run under abuse", () => {
  it("fires ONE query however many times Run is hit while one is in flight", async () => {
    const { calls, release } = renderConsole({ defer: true });

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    // The button is busy AND says so; five more clicks cost nothing.
    expect(runButton()).toBeDisabled();
    for (let i = 0; i < 5; i++) run();
    expect(calls).toHaveLength(1);

    release();
    await waitFor(() => expect(screen.queryByText(/no data/i)).toBeInTheDocument());
  });

  /* A query box poisoned from outside — an earlier session, another tab, a
     hand-edited localStorage — could leave the page with nothing to run and a
     Run button that answered a click with silence. */
  it("disables Run for a blank query rather than answering the click with nothing", () => {
    localStorage.setItem(LAST_QUERY_KEY, "   ");
    const { calls } = renderConsole();

    expect(runButton()).toBeDisabled();
    run();
    expect(calls).toHaveLength(0);
  });

  it("takes a hostile stored query as TEXT and runs it as text", async () => {
    const nasty = '<script>alert(1)</script>{job="a"}';
    localStorage.setItem(LAST_QUERY_KEY, nasty);
    const { calls } = renderConsole();

    // Restored into the editor, not executed as markup.
    expect(screen.getByTestId("promql-editor")).toHaveAttribute("data-initial", nasty);
    expect(document.querySelector("script")).toBeNull();

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].body.query).toBe(nasty);
  });

  it("still mounts when localStorage itself throws", () => {
    const getItem = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("SecurityError: storage is disabled");
    });
    try {
      expect(() => renderConsole()).not.toThrow();
      expect(runButton()).toBeInTheDocument();
    } finally {
      getItem.mockRestore();
    }
  });

  it("keeps its head when the tab is switched with a query still in flight", async () => {
    const { calls, release } = renderConsole({
      defer: true,
      answer: { status: "success", data: { resultType: "vector", result: [{ metric: { node: "n1" }, value: [1, "1"] }] } },
    });

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    fireEvent.click(tab("JSON"));
    fireEvent.click(tab("Table"));
    release();

    expect(await screen.findByText("n1")).toBeInTheDocument();
  });
});

describe("Range and Step cannot be worked into an unreadable chart", () => {
  const onePoint = {
    status: "success",
    data: { resultType: "matrix", result: [{ metric: { __name__: "q", pair: "a" }, values: [[1, "0.5"]] }] },
  };

  /* Step 15m over a 15m range is one sample. With symbols off a one-point line
     draws NOTHING: a chart holding data and looking empty. */
  it("shows the marker for a series the chosen step reduced to one point", async () => {
    renderConsole({ answer: onePoint });
    pickRange();
    fireEvent.click(tab("Chart"));
    run();
    await screen.findByTestId("echart");

    expect(lastOption().series?.[0].showSymbol).toBe(true);
  });

  it("sends the step the reader picked, and the window the reader picked", async () => {
    const { calls } = renderConsole({ answer: onePoint });
    pickRange();
    /* Range and Step both offer a "15m", which is exactly why the page captions
       them — an uncaptioned pair reads as one duplicated control. Here the
       ordering IS the disambiguation: Range first, Step second. */
    const both = () => screen.getAllByRole("radio", { name: "15m" });
    expect(both()).toHaveLength(2);
    fireEvent.click(both()[0]);
    fireEvent.click(both()[1]);
    run();

    await waitFor(() => expect(calls).toHaveLength(1));
    const { start, end, step } = calls[0].body;
    expect(step).toBe(15 * 60 * 1e9);
    expect(new Date(end as string).getTime() - new Date(start as string).getTime()).toBe(15 * 60 * 1000);
  });

  it("asks for exactly the 24h the proxy's maxRange allows, never a millisecond more", async () => {
    const { calls } = renderConsole({ answer: onePoint });
    pickRange();
    fireEvent.click(screen.getByRole("radio", { name: "24h" }));
    run();

    await waitFor(() => expect(calls).toHaveLength(1));
    const { start, end } = calls[0].body;
    expect(new Date(end as string).getTime() - new Date(start as string).getTime()).toBe(24 * 60 * 60 * 1000);
  });

  /* Twenty combinations, four of which put the step at or past the whole
     window. Every one of them still has to describe a window the proxy will
     take: `end - start` inside maxRange, a positive integer step in
     nanoseconds, and a start that is genuinely before the end. */
  it("walks all four ranges against all five steps and asks for nothing illegal", async () => {
    const { calls } = renderConsole({ answer: onePoint });
    pickRange();

    for (const r of ["15m", "1h", "6h", "24h"]) {
      const ranges = screen.getAllByRole("radio", { name: r });
      fireEvent.click(ranges[0]);
      for (const s of ["15s", "30s", "1m", "5m", "15m"]) {
        const steps = screen.getAllByRole("radio", { name: s });
        // "15m" is in both groups; the Step control is the second.
        fireEvent.click(steps[steps.length - 1]);
        run();
        // eslint-disable-next-line no-await-in-loop
        await waitFor(() => expect(runButton()).not.toBeDisabled());
      }
    }

    expect(calls).toHaveLength(20);
    for (const { body } of calls) {
      const span = Date.parse(body.end as string) - Date.parse(body.start as string);
      expect(span).toBeGreaterThan(0);
      expect(span).toBeLessThanOrEqual(24 * 60 * 60 * 1000);
      expect(Number.isInteger(body.step)).toBe(true);
      expect(body.step as number).toBeGreaterThan(0);
      expect(body.query).toBeTruthy();
    }
  });

  /* The tab now follows the RESULT rather than the mode: an instant query on a
     range vector answers with a matrix and charts fine, so a mode flip alone
     proves nothing. What still drops the reader back to Table is a result that
     cannot be drawn — an instant vector. */
  it("drops back to Table when the ANSWER cannot be charted", async () => {
    renderConsole({
      answer: { status: "success", data: { resultType: "vector", result: [{ metric: { job: "a" }, value: [1_754_000_000, "1"] }] } },
    });
    pickRange();
    fireEvent.click(tab("Chart"));
    expect(tab("Chart")).toHaveAttribute("aria-selected", "true");

    fireEvent.click(screen.getByRole("radio", { name: "Instant" }));
    run();
    await waitFor(() => expect(tab("Table")).toHaveAttribute("aria-selected", "true"));
    expect(tab("Chart")).toBeDisabled();
  });

  it("keeps the Chart tab for an INSTANT query that answered with a matrix", async () => {
    renderConsole({ answer: onePoint });
    fireEvent.click(tab("Chart"));
    run();
    await waitFor(() => expect(tab("Chart")).not.toBeDisabled());
    expect(tab("Chart")).toHaveAttribute("aria-selected", "true");
  });
});

/* ── the results that are all the same ───────────────────────────────────── */

describe("series that cannot be told apart", () => {
  /* A recording rule can emit two series with an identical label set (Prometheus
     itself will not). lib/prom-series.ts deliberately gives them the same
     identity — the full truth of both, rather than a tidy string that hides the
     collision. What must NOT follow is two DOM nodes claiming one id. */
  const twins = {
    status: "success",
    data: {
      resultType: "matrix",
      result: [
        { metric: { __name__: "rule:x", job: "a" }, values: [[1, "1"], [2, "2"]] },
        { metric: { __name__: "rule:x", job: "a" }, values: [[1, "3"], [2, "4"]] },
      ],
    },
  };

  it("lists both, and gives each expander its OWN target", async () => {
    renderConsole({ answer: twins });
    pickRange();
    fireEvent.click(tab("Chart"));
    run();
    await screen.findByTestId("echart");

    expect(screen.getAllByTestId("raw-row")).toHaveLength(2);
    const targets = screen.getAllByRole("button", { name: /show all labels/i }).map((b) =>
      b.getAttribute("aria-controls"),
    );
    expect(new Set(targets).size).toBe(2);
  });

  it("opens exactly one of the two when one of the two is clicked", async () => {
    renderConsole({ answer: twins });
    pickRange();
    fireEvent.click(tab("Chart"));
    run();
    await screen.findByTestId("echart");

    fireEvent.click(screen.getAllByRole("button", { name: /show all labels/i })[0]);
    expect(screen.getAllByTestId("raw-full-labels")).toHaveLength(1);
    // And the id it opened is the one that row's button points at.
    expect(screen.getByTestId("raw-full-labels").id).toBe(
      screen.getAllByRole("button", { name: /show all labels/i })[0].getAttribute("aria-controls"),
    );
  });
});

describe("a thousand series is a normal answer", () => {
  const many = (n: number) => ({
    status: "success",
    data: {
      resultType: "vector",
      result: Array.from({ length: n }, (_, i) => ({
        metric: { __name__: "up", pod: `p-${i}` },
        value: [1_754_000_000, String(i % 2)],
      })),
    },
  });

  it("pages the table at ten rather than rendering a thousand", async () => {
    renderConsole({ answer: many(1000) });

    run();
    await screen.findByTestId("pager-showing");
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 10 of 1000 rows");
  });
});

/* ── the URL ─────────────────────────────────────────────────────────────── */

describe("a hand-edited ?at= cannot break the page", () => {
  /* `?at=` is the one URL key this page reads, and it lands in two places that
     both throw on a bad Date: `toLocaleString` in the header sentence, and
     `toISOString` on the body of every request. lib/timemachine gates it — this
     pins that the Console is actually behind that gate. */
  const garbage = [
    "not-a-date",
    "2026",
    "2026-08-01",
    "'; DROP TABLE--",
    "<script>alert(1)</script>",
    "%%%%",
  ];

  for (const raw of garbage) {
    it(`stays Live and still queries for ?at=${raw}`, async () => {
      window.history.pushState({}, "", `/console?at=${encodeURIComponent(raw)}`);
      const calls: PromCall[] = [];
      vi.stubGlobal(
        "fetch",
        vi.fn((url: string, init?: RequestInit) => {
          if (String(url).includes("/api/v1/promql/")) {
            calls.push({ url: String(url), body: JSON.parse(String(init?.body ?? "{}")) });
          }
          return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
        }),
      );
      const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      render(
        <QueryClientProvider client={qc}>
          <ThemeProvider>
            <TimeMachineProvider>
              <PromQLConsolePage />
            </TimeMachineProvider>
          </ThemeProvider>
        </QueryClientProvider>,
      );

      // Not "as of Invalid Date", and not a crash: the page is simply Live.
      expect(document.body.textContent ?? "").not.toContain("Invalid Date");
      run();
      await waitFor(() => expect(calls).toHaveLength(1));
      expect(calls[0].body.time).toBeUndefined();
    });
  }

  it("keeps a FUTURE instant off the wire by clamping it, never sending it raw", async () => {
    const future = new Date(Date.now() + 86_400_000).toISOString().replace(/\.\d{3}Z$/, "Z");
    window.history.pushState({}, "", `/console?at=${encodeURIComponent(future)}`);
    const calls: PromCall[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string, init?: RequestInit) => {
        if (String(url).includes("/api/v1/promql/")) {
          calls.push({ url: String(url), body: JSON.parse(String(init?.body ?? "{}")) });
        }
        return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <TimeMachineProvider>
            <PromQLConsolePage />
          </TimeMachineProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    );

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(Date.parse(calls[0].body.time as string)).toBeLessThanOrEqual(Date.now());
  });
});

/* ── Russian ─────────────────────────────────────────────────────────────── */

describe("the hostile paths speak Russian too", () => {
  it("keeps the blank-query guard and the string result readable in Russian", async () => {
    localStorage.setItem(LAST_QUERY_KEY, "");
    renderConsole({
      locale: "ru",
      answer: { status: "success", data: { resultType: "string", result: [1, "привет"] } },
    });

    const ru = await screen.findByRole("button", { name: "Выполнить" });
    expect(ru).toBeDisabled();
    expect(screen.getByText("Выполните запрос, чтобы увидеть результат.")).toBeInTheDocument();
  });
});
