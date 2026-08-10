import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { PromQLConsolePage, ResultTabs } from "./promql-console";

/* The page mounts CodeMirror and ECharts; neither renders honestly in jsdom
   (no layout, no canvas), and neither is what these cases are about. Both are
   stubbed the way components/mtr-changes-timeline.test.tsx stubs EChart. */
vi.mock("@/components/promql-editor", () => ({
  PromQLEditor: () => <div data-testid="promql-editor" />,
}));
vi.mock("@/components/echart", () => ({
  EChart: ({ className }: { className?: string }) => <div data-testid="echart" className={className} />,
}));

/**
 * The Console's result switcher is the one place in this codebase that declared role="tablist"
 * without honouring the pattern.
 */

type Tab = "table" | "chart" | "json";

/** The strip is controlled, so a pin needs the state its page holds. */
function Harness({ chartDisabled = false }: { chartDisabled?: boolean }) {
  const [active, setActive] = useState<Tab>("table");
  return (
    <ResultTabs
      active={active}
      onChange={setActive}
      isDisabled={(id) => chartDisabled && id === "chart"}
    />
  );
}

const tab = (name: string) => screen.getByRole("tab", { name });

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

interface PromCall {
  url: string;
  body: { query: string; time?: string; start?: string; end?: string; step?: number };
}

/** The whole page, with the two heavy children stubbed out. `at` engages the
 *  Time Machine through the URL, which is the only carrier it has. */
function renderConsole(
  opts: {
    answer?: unknown;
    at?: string;
    httpStatus?: number;
    /** Mounts a <LocaleProvider> above the page. Absent — every case but the ru
     *  smoke pin at the bottom of this file — there is no provider at all,
     *  which lib/i18n defines as English. */
    locale?: Locale;
  } = {},
) {
  const {
    answer = { status: "success", data: { resultType: "vector", result: [] } },
    at,
    httpStatus = 200,
    locale,
  } = opts;
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  window.history.pushState({}, "", at ? `/console?at=${at}` : "/console");
  const calls: PromCall[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    if (href.includes("/api/v1/promql/")) {
      calls.push({ url: href, body: JSON.parse(String(init?.body ?? "{}")) });
      if (httpStatus !== 200) {
        return Promise.resolve(
          new Response(JSON.stringify({ type: "about:blank", title: "proxy refused", status: httpStatus, detail: "query too expensive" }), {
            status: httpStatus,
            headers: { "Content-Type": "application/problem+json" },
          }),
        );
      }
      return Promise.resolve(json(answer));
    }
    return Promise.resolve(json({}));
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
  return { ...utils, calls };
}

const run = () => fireEvent.click(screen.getByRole("button", { name: "Run" }));
const pickRange = () => fireEvent.click(screen.getByRole("radio", { name: "Range" }));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
  /* vitest.setup.ts backs localStorage with one Map per test FILE — a locale
     left behind would flip every later case in this one. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("Console result tabs", () => {
  it("is ONE tab stop: only the selected tab is reachable by Tab", () => {
    render(<Harness />);
    expect(tab("Table")).toHaveAttribute("tabindex", "0");
    expect(tab("Chart")).toHaveAttribute("tabindex", "-1");
    expect(tab("JSON")).toHaveAttribute("tabindex", "-1");
  });

  it("names the panel each tab reveals", () => {
    render(<Harness />);
    expect(tab("Table")).toHaveAttribute("aria-controls", "promql-result-panel-table");
    expect(tab("Chart")).toHaveAttribute("aria-controls", "promql-result-panel-chart");
    expect(tab("JSON")).toHaveAttribute("aria-controls", "promql-result-panel-json");
    // The other half of the wiring: the panel points back at its own tab, so
    // the id the tab claims has to be the id the tab carries.
    expect(tab("Table")).toHaveAttribute("id", "promql-result-tab-table");
  });

  it("moves selection AND focus with the arrow keys", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "ArrowRight" });
    expect(tab("Chart")).toHaveAttribute("aria-selected", "true");
    expect(tab("Chart")).toHaveFocus();
    expect(tab("Table")).toHaveAttribute("tabindex", "-1");
  });

  it("wraps at both ends", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "ArrowLeft" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(tab("JSON"), { key: "ArrowRight" });
    expect(tab("Table")).toHaveAttribute("aria-selected", "true");
  });

  it("honours Home and End", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "End" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(tab("JSON"), { key: "Home" });
    expect(tab("Table")).toHaveAttribute("aria-selected", "true");
  });

  /* Chart is disabled for instant queries. A disabled button cannot take
     focus, so an arrow key that selected it would move selection to an element
     the keyboard can never reach — the strip steps over it instead. */
  it("steps over a disabled tab instead of stranding focus on it", () => {
    render(<Harness chartDisabled />);
    expect(tab("Chart")).toBeDisabled();
    fireEvent.keyDown(tab("Table"), { key: "ArrowRight" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    expect(tab("JSON")).toHaveFocus();
    expect(tab("Chart")).toHaveAttribute("aria-selected", "false");
  });

  it("still switches on click", () => {
    render(<Harness />);
    fireEvent.click(tab("JSON"));
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
  });
});

/** Prometheus's own error envelope resolves rather than throws (lib/api's `handle`). */
describe("Console empty notes vs. a failed query", () => {
  const errorEnvelope = { status: "error", errorType: "bad_data", error: "parse error: unexpected end of input" };

  it("shows the error alone on the Table tab — never alongside a No-data note", async () => {
    renderConsole({ answer: errorEnvelope });

    run();
    expect(await screen.findByText(/parse error: unexpected end of input/)).toBeInTheDocument();
    expect(screen.queryByText(/no data/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/run a query to see results/i)).not.toBeInTheDocument();
  });

  it("shows the error alone on the Chart tab too", async () => {
    renderConsole({ answer: errorEnvelope });

    pickRange();
    run();
    await screen.findByText(/parse error: unexpected end of input/);
    fireEvent.click(tab("Chart"));
    expect(screen.queryByText(/no series to chart/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/run a range query/i)).not.toBeInTheDocument();
  });

  it("does the same for a transport-level failure, which has no envelope at all", async () => {
    renderConsole({ httpStatus: 422 });

    run();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByText(/no data/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/run a query to see results/i)).not.toBeInTheDocument();
  });

  it("still says No data for a genuinely empty SUCCESS envelope", async () => {
    renderConsole({ answer: { status: "success", data: { resultType: "vector", result: [] } } });

    run();
    expect(await screen.findByText(/no data — the query returned an empty result/i)).toBeInTheDocument();
  });
});

/* QA scope 4, finding #16: Instant and Range answer different SHAPES of
   question, so the result on screen stops describing what the controls say the
   moment the mode changes — and nothing said so. */
describe("Console mode switch", () => {
  const oneSample = {
    status: "success",
    data: { resultType: "vector", result: [{ metric: { __name__: "up", node: "node-a" }, value: [1_754_000_000, "1"] }] },
  };

  it("drops the instant result when the mode flips to Range, rather than leaving it unmarked", async () => {
    renderConsole({ answer: oneSample });

    run();
    expect(await screen.findByText("node-a")).toBeInTheDocument();

    pickRange();
    await waitFor(() => expect(screen.queryByText("node-a")).not.toBeInTheDocument());
    // Back to the idle state, which is the honest one: nothing has been asked
    // in this mode yet.
    expect(screen.getByText(/run a query to see results/i)).toBeInTheDocument();
  });

  it("drops a range result on the way back to Instant too", async () => {
    renderConsole({ answer: oneSample });

    pickRange();
    run();
    expect(await screen.findByText("node-a")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "Instant" }));
    await waitFor(() => expect(screen.queryByText("node-a")).not.toBeInTheDocument());
  });
});

/**
 * QA round 4, finding #3. /console was the one data surface that ignored `?at=`
 * entirely: engaged at an incident it kept answering with NOW.
 */
describe("Console under the Time Machine", () => {
  const AT = "2026-08-01T02:14:00Z";

  it("sends time=at for an instant query", async () => {
    const { calls } = renderConsole({ at: AT });

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toContain("/api/v1/promql/query");
    expect(calls[0].body.time).toBe(new Date(AT).toISOString());
  });

  it("anchors a range query's END at `at`, measuring the picked window back from there", async () => {
    const { calls } = renderConsole({ at: AT });

    pickRange();
    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    const { start, end } = calls[0].body;
    expect(end).toBe(new Date(AT).toISOString());
    // The default range is 1h.
    expect(new Date(end as string).getTime() - new Date(start as string).getTime()).toBe(60 * 60 * 1000);
  });

  it("says in the header which instant it is answering as of", async () => {
    renderConsole({ at: AT });

    expect(
      await screen.findByText(new RegExp(`as of ${new Date(AT).toLocaleString()}`)),
    ).toBeInTheDocument();
  });

  it("sends no time at all while Live", async () => {
    const { calls } = renderConsole();

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].body.time).toBeUndefined();
  });
});

/* the Russian is wired ONE smoke pin. */
describe("PromQLConsolePage — Russian", () => {
  it("names its controls and keeps the idle/empty distinction in Russian", async () => {
    renderConsole({ locale: "ru" });

    expect(await screen.findByRole("heading", { name: "Консоль" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Выполнить" })).toBeInTheDocument();
    // JSON is a format name and does NOT move; the other two tabs do.
    expect(screen.getByRole("tab", { name: "Таблица" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "JSON" })).toBeInTheDocument();

    // Nothing run yet: the idle sentence, not the empty-result one.
    expect(screen.getByText("Выполните запрос, чтобы увидеть результат.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Выполнить" }));
    // Ran, and matched nothing: a different sentence, with the different fact.
    expect(await screen.findByText("Данных нет: запрос вернул пустой результат.")).toBeInTheDocument();
  });
});
