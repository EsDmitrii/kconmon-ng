import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import type * as echarts from "echarts";
import { useMutation } from "@tanstack/react-query";
import { ChevronRight, SquareTerminal } from "lucide-react";
import { EChart } from "@/components/echart";
import { PageShell } from "@/components/page-shell";
import { PromQLEditor } from "@/components/promql-editor";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Pager, usePager } from "@/components/ui/pager";
import { Segmented } from "@/components/ui/segmented";
import { useTheme } from "@/components/theme-provider";
import { promqlQuery, promqlQueryRange } from "@/lib/api";
import { chartColors } from "@/lib/chart-theme";
import { stampFull, useLocale, useT } from "@/lib/i18n";
import { promqlConsoleDict, type PromQLConsoleKey } from "@/lib/i18n/dict/promql-console";
import { paletteColor, seriesIdentities, showLegend, type SeriesIdentity } from "@/lib/prom-series";
import { toTable } from "@/lib/prom-table";
import { useTimeContext } from "@/lib/timemachine";
import type { PromResult } from "@/lib/types";
import { cn } from "@/lib/utils";

const LAST_QUERY_KEY = "kconmon.promql.lastQuery";
const DEFAULT_QUERY = "up";

type Mode = "instant" | "range";
type ResultTab = "table" | "chart" | "json";

interface RangeOption { id: string; label: string; seconds: number }
interface StepOption { id: string; label: string; seconds: number }

// Same window lengths as Explore's RANGE_OPTIONS (pages/explore.tsx). Kept as
// a local literal rather than a shared import so this dev-tools page doesn't
// couple to Explore's module for an incidental value match.
const RANGE_OPTIONS: RangeOption[] = [
  { id: "15m", label: "15m", seconds: 15 * 60 },
  { id: "1h", label: "1h", seconds: 60 * 60 },
  { id: "6h", label: "6h", seconds: 6 * 60 * 60 },
  { id: "24h", label: "24h", seconds: 24 * 60 * 60 },
];

// The console additionally exposes an explicit step picker (Explore hides
// this and always auto-sizes it) since a PromQL dev tool benefits from
// controlling query_range resolution directly.
const STEP_OPTIONS: StepOption[] = [
  { id: "15s", label: "15s", seconds: 15 },
  { id: "30s", label: "30s", seconds: 30 },
  { id: "1m", label: "1m", seconds: 60 },
  { id: "5m", label: "5m", seconds: 5 * 60 },
  { id: "15m", label: "15m", seconds: 15 * 60 },
];

// Suggests the STEP_OPTIONS entry closest to a ~240-point-per-series
// resolution for the given range, mirroring Explore's TARGET_POINTS sizing —
// just used as a default here rather than the only option.
function suggestStepId(rangeSeconds: number): string {
  const raw = rangeSeconds / 240;
  return STEP_OPTIONS.reduce(
    (best, opt) => (Math.abs(opt.seconds - raw) < Math.abs(best.seconds - raw) ? opt : best),
    STEP_OPTIONS[0],
  ).id;
}

interface MatrixEntry { metric: Record<string, string>; values?: [number, string][] }

/**
 * ChartSeries is one line on the plot AND one row in the listing under it. The
 * two are built together, from one call to seriesIdentities, so the legend, the
 * tooltip and the raw table cannot disagree about what a series is called.
 */
export interface ChartSeries {
  identity: SeriesIdentity;
  /** The series' own colour, so a table row can carry the line's swatch. */
  color: string;
  /** How many points the window holds — the "is this series even sampled" tell. */
  points: number;
  /** The last value in the window, or "" for a series with no points at all. */
  lastValue: string;
}

export interface ChartModel {
  option: echarts.EChartsOption;
  series: ChartSeries[];
  /** The labels every series shares, said once above the listing. */
  sharedText: string;
}

function matrixEntries(res: PromResult): MatrixEntry[] {
  if (res.status !== "success" || res.data?.resultType !== "matrix") return [];
  /* The same shape check lib/prom-table.ts makes, and for the same reason: this
     module reads somebody else's JSON. A `result: null` walked straight into
     `.map` and took the whole page down to the error boundary, which only
     clears on a route change — so the reader could not get back to the Console
     without leaving it. */
  return Array.isArray(res.data.result) ? (res.data.result as MatrixEntry[]) : [];
}

/**
 * toChartModel builds the plot and its listing from one result.
 *
 * Two things changed when the owner sent the old results back. Series are named
 * by what DISTINGUISHES them (lib/prom-series.ts) rather than by their whole
 * label set, which was the string that arrived truncated mid-label everywhere it
 * was shown. And the legend is drawn only while it is still a legend: ECharts'
 * scroll legend does not wrap, so at 86 series it became a "1/86" pager, and
 * past the threshold the raw table below the chart does that job properly.
 *
 * Two more when the owner could not read the plot at all — «все полосочки
 * серые… они все ровные»:
 *
 *  - COLOUR comes from lib/prom-series.ts's paletteColor, not chart-theme's
 *    seriesColor. The latter folds the sixth series and everything after it
 *    into the axis grey, which is the right answer for a topk(5) curated chart
 *    and the wrong one for a console where `up` matches 86 series.
 *  - The Y AXIS does not force a zero baseline. ECharts includes zero in a
 *    value axis unless `scale` says otherwise, so a latency series at 42ms
 *    ±0.5ms was a straight line pinned to the top of a 0…45ms axis. A console
 *    is asked "did this move", not "how big is it next to nothing" — the
 *    curated charts, which carry a unit and answer the second question, keep
 *    their zero baseline (lib/curated-metrics.ts).
 */
export function toChartModel(res: PromResult, dark: boolean): ChartModel {
  const entries = matrixEntries(res);
  const colors = chartColors(dark ? "dark" : "light");
  const identities = seriesIdentities(entries.map((e) => e.metric));
  /* Sorted on the MINIMAL identity, which is what the reader sees: ordering by
     the full label set put `pod="a"` and `pod="z"` next to each other whenever
     an earlier label happened to differ. */
  const sorted = entries
    .map((entry, i) => ({ entry, identity: identities.series[i] }))
    .sort((a, b) => a.identity.text.localeCompare(b.identity.text));

  const series: ChartSeries[] = sorted.map(({ entry, identity }, i) => {
    const values = entry.values ?? [];
    return {
      identity,
      color: paletteColor(colors.series, i),
      points: values.length,
      lastValue: values.at(-1)?.[1] ?? "",
    };
  });

  const legend = showLegend(series.length);
  return {
    sharedText: identities.sharedText,
    series,
    option: {
      animation: false,
      textStyle: { color: colors.axis },
      /* The bottom inset is the legend's room. Without a legend the plot takes
         it back rather than sitting above a reserved strip of nothing. */
      grid: { left: 56, right: 16, top: 12, bottom: legend ? 46 : 12 },
      legend: {
        show: legend,
        bottom: 0,
        type: "scroll",
        icon: "roundRect",
        itemWidth: 10,
        itemHeight: 2,
        textStyle: { color: colors.axis, fontSize: 11 },
        pageIconColor: colors.axis,
        pageTextStyle: { color: colors.axis },
      },
      /* The pointer TYPE is the shared layer's (lib/chart-tooltip.ts turns it
         into a cross on whichever chart the mouse is over); the colour is this
         surface's and travels with it. */
      tooltip: { trigger: "axis", axisPointer: { lineStyle: { color: colors.grid } } },
      xAxis: {
        type: "time",
        axisLine: { lineStyle: { color: colors.grid } },
        // Same anti-smear rule the curated charts take (QA round 2, #19): this
        // console's plot lives in a narrower column than any of them.
        axisLabel: { color: colors.axis, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: {
        // Neutral numeric axis, deliberately unformatted — see design note above.
        type: "value",
        /* The axis fits the DATA, not the origin. ECharts pulls zero into a
           value axis by default, which turns every series with a small spread
           around a large value — a latency, a queue depth, a counter rate —
           into a flat line hugging the top of the plot. A genuinely constant
           series still draws flat, because it is. */
        scale: true,
        axisLabel: { color: colors.axis },
        splitLine: { lineStyle: { color: colors.grid } },
      },
      series: sorted.map(
        ({ entry, identity }, i): echarts.LineSeriesOption => ({
          /* The MINIMAL identity, and this one assignment is what fixes the
             tooltip too: lib/chart-tooltip.ts's capped formatter prints
             `seriesName` verbatim. */
          name: identity.text,
          type: "line",
          /* A one-point line draws NOTHING with symbols off, and this page hands
             the reader a step picker: step 15m over a 15m range is one sample.
             A chart that holds data and looks empty is the worst answer here. */
          showSymbol: (entry.values ?? []).length < 2,
          /* The ROW's colour, read from the row rather than recomputed: the two
             lists are built from the same `sorted` array in the same order, and
             taking it from there is what makes a swatch and its line the same
             colour by construction instead of by coincidence. */
          color: series[i].color,
          lineStyle: { width: 2 },
          data: (entry.values ?? []).map(([ts, v]) => [ts * 1000, Number(v)]),
        }),
      ),
    },
  };
}

function readLastQuery(): string {
  try {
    return localStorage.getItem(LAST_QUERY_KEY) ?? DEFAULT_QUERY;
  } catch {
    return DEFAULT_QUERY;
  }
}

/* The strip's three tabs, each carrying the KEY of its label rather than the
   label: the order is the tab order (and therefore the arrow-key order), which
   is a property of this list, while the words belong to the dictionary. */
const TABS: { id: ResultTab; key: PromQLConsoleKey }[] = [
  { id: "table", key: "tab.table" },
  { id: "chart", key: "tab.chart" },
  { id: "json", key: "tab.json" },
];

/* One console, one result switcher, so the ids can be constants rather than
   useId() values — and constants keep the tab⇄panel wiring readable in the
   markup below. */
const tabDomId = (id: ResultTab) => `promql-result-tab-${id}`;
const panelDomId = (id: ResultTab) => `promql-result-panel-${id}`;

/**
 * ResultTabs is the result switcher, and it declares role="tablist"; a disabled tab is STEPPED OVER
 * rather than landed on: Chart is disabled in instant mode.
 */
export function ResultTabs({
  active,
  onChange,
  isDisabled,
}: {
  active: ResultTab;
  onChange: (id: ResultTab) => void;
  isDisabled: (id: ResultTab) => boolean;
}) {
  const t = useT(promqlConsoleDict);
  const refs = useRef<(HTMLButtonElement | null)[]>([]);

  const move = (from: number, delta: number) => {
    for (let step = 1; step <= TABS.length; step++) {
      const next = (((from + delta * step) % TABS.length) + TABS.length) % TABS.length;
      if (isDisabled(TABS[next].id)) continue;
      onChange(TABS[next].id);
      refs.current[next]?.focus();
      return;
    }
  };

  const onKeyDown = (e: KeyboardEvent, i: number) => {
    if (e.key === "ArrowRight" || e.key === "ArrowDown") {
      e.preventDefault();
      move(i, 1);
    } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
      e.preventDefault();
      move(i, -1);
    } else if (e.key === "Home") {
      e.preventDefault();
      move(-1, 1);
    } else if (e.key === "End") {
      e.preventDefault();
      move(TABS.length, -1);
    }
  };

  return (
    <div role="tablist" aria-label={t("tabs.aria")} className="flex w-fit gap-1 rounded-md bg-surface-2 p-1">
      {TABS.map((tab, i) => {
        const disabled = isDisabled(tab.id);
        const selected = tab.id === active;
        return (
          <button
            key={tab.id}
            ref={(el) => {
              refs.current[i] = el;
            }}
            type="button"
            id={tabDomId(tab.id)}
            role="tab"
            aria-selected={selected}
            aria-controls={panelDomId(tab.id)}
            tabIndex={selected ? 0 : -1}
            onClick={() => onChange(tab.id)}
            onKeyDown={(e) => onKeyDown(e, i)}
            disabled={disabled}
            className={cn(
              "h-8 rounded-sm px-3.5 text-sm transition-colors duration-(--dur) ease-(--ease)",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              selected ? "bg-card font-medium text-foreground shadow-card" : "text-muted-foreground hover:text-foreground",
              disabled && "cursor-not-allowed opacity-40 hover:text-muted-foreground",
            )}
            title={disabled ? t("tab.chart.disabled") : undefined}
          >
            {t(tab.key)}
          </button>
        );
      })}
    </div>
  );
}

/**
 * RawSeriesRow is one series in the listing under the chart: its swatch, its
 * minimal identity, its point count and its last value — plus the way back to
 * the whole truth.
 *
 * The expander is per row and not a page-wide switch: a reader checking one
 * pod's full labels should not have to turn eighty-five other rows into
 * paragraphs to do it.
 */
function RawSeriesRow({ row, index }: { row: ChartSeries; index: number }) {
  const t = useT(promqlConsoleDict);
  const [open, setOpen] = useState(false);
  /* The ROW's ordinal, not the identity's text. A recording rule can emit two
     series with an identical label set — lib/prom-series.ts deliberately gives
     both the same identity rather than a tidy string that hides the collision —
     and an id derived from that text put two nodes in the document under one id,
     with two expanders' aria-controls pointing at whichever the browser found
     first. */
  const fullId = `promql-series-${index}`;

  return (
    <>
      <tr data-testid="raw-row" className="transition-colors duration-(--dur-fast) hover:bg-accent/40">
        <td className="border-b border-border py-2 pl-4 pr-2 align-top">
          <button
            type="button"
            aria-expanded={open}
            aria-controls={fullId}
            aria-label={t("raw.showFull")}
            title={t("raw.showFull")}
            onClick={() => setOpen((v) => !v)}
            className={cn(
              "flex size-5 items-center justify-center rounded text-muted-foreground",
              "transition-colors duration-(--dur-fast) ease-(--ease) hover:text-foreground",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            )}
          >
            <ChevronRight
              aria-hidden="true"
              className={cn("size-3.5 transition-transform duration-(--dur-fast) ease-(--ease)", open && "rotate-90")}
            />
          </button>
        </td>
        <td className="border-b border-border py-2 pr-4 align-top">
          <span className="flex items-center gap-2">
            {/* The line's own colour, which is what ties this row to the plot
                above now that the legend is gone on a big result. */}
            <span aria-hidden="true" className="size-2 shrink-0 rounded-[2px]" style={{ background: row.color }} />
            <span
              data-testid="raw-identity"
              /* The whole label set is one hover away even before the row is
                  expanded — the affordance the truncated dump never had. */
              title={row.identity.fullText}
              className="mono-data min-w-0 flex-1 truncate"
            >
              {row.identity.text}
            </span>
          </span>
        </td>
        <td className="mono-data border-b border-border py-2 pr-4 text-right align-top text-muted-foreground">
          {row.points}
        </td>
        <td className="mono-data border-b border-border py-2 pr-4 text-right align-top">{row.lastValue}</td>
      </tr>
      {open ? (
        <tr>
          <td colSpan={4} className="border-b border-border bg-surface-2/40 px-4 py-2">
            <code
              id={fullId}
              data-testid="raw-full-labels"
              className="mono-data block break-all leading-relaxed"
            >
              {row.identity.fullText}
            </code>
          </td>
        </tr>
      ) : null}
    </>
  );
}

/**
 * RawSeriesTable is Grafana Explore's listing under the plot, and on this page it
 * is also the legend: past LEGEND_MAX_SERIES the chart draws none, because
 * ECharts' scroll legend turns into a one-name-at-a-time pager rather than
 * wrapping. Ten rows a page, the console's own default.
 */
function RawSeriesTable({ series, sharedText }: { series: ChartSeries[]; sharedText: string }) {
  const t = useT(promqlConsoleDict);
  /* The identity of the LIST, so a new query starts at page one rather than on
     page nine of a result that no longer exists. The first series is in the
     key too: a re-run matching the same COUNT under the same shared labels can
     still be an entirely different set of pods. */
  const pager = usePager(series, {
    resetKey: [sharedText, series.length, series[0]?.identity.fullText ?? ""].join("\u0000"),
  });

  return (
    <Card className="overflow-hidden p-0">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 border-b border-border px-4 py-2.5">
        <h3 className="text-[11px] font-medium uppercase tracking-[0.06em] text-muted-foreground">
          {t("raw.title")}
        </h3>
        {/* Said ONCE. These are the labels every series in the result carries,
            and repeating them on eighty-six rows is what pushed the two
            characters that differ off the end of every one of them. */}
        {sharedText ? (
          <code data-testid="raw-shared" className="mono-data min-w-0 truncate text-muted-foreground">
            {sharedText}
          </code>
        ) : null}
      </div>
      <div className="overflow-x-auto">
        <table data-testid="raw-table" className="w-full border-separate border-spacing-0 text-sm">
          <caption className="sr-only">{t("raw.caption")}</caption>
          <thead>
            <tr className="text-[11px] font-medium uppercase tracking-[0.06em] text-muted-foreground">
              <th scope="col" className="border-b border-border py-2 pl-4 pr-2 text-left">
                <span className="sr-only">{t("raw.showFull")}</span>
              </th>
              <th scope="col" className="border-b border-border py-2 pr-4 text-left">{t("raw.col.series")}</th>
              <th scope="col" className="border-b border-border py-2 pr-4 text-right">{t("raw.col.points")}</th>
              <th scope="col" className="border-b border-border py-2 pr-4 text-right">{t("raw.col.last")}</th>
            </tr>
          </thead>
          <tbody>
            {pager.visible.map((row, i) => (
              /* The index disambiguates the one case fullText cannot: two series
                 agreeing on every label. It also collapses an expanded row when
                 the page under it turns, which is the right reset. */
              <RawSeriesRow key={`${row.identity.fullText}#${i}`} row={row} index={i} />
            ))}
          </tbody>
        </table>
      </div>
      <Pager pager={pager} subject={t("raw.subject")} />
    </Card>
  );
}

/* The panel half of the same contract: a labelled tabpanel that is itself a tab stop. */
function ResultPanel({ tab, children }: { tab: ResultTab; children: ReactNode }) {
  return (
    <div
      role="tabpanel"
      id={panelDomId(tab)}
      aria-labelledby={tabDomId(tab)}
      tabIndex={0}
      className="rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      {children}
    </div>
  );
}

export function PromQLConsolePage() {
  const t = useT(promqlConsoleDict);
  const { locale } = useLocale();
  const { theme } = useTheme();
  /** The Time Machine anchors this page too. */
  const { at } = useTimeContext();
  const [query, setQuery] = useState<string>(readLastQuery);
  const [mode, setMode] = useState<Mode>("instant");
  const [rangeId, setRangeId] = useState("1h");
  const [stepId, setStepId] = useState(() => suggestStepId(RANGE_OPTIONS[1].seconds));
  const [activeTab, setActiveTab] = useState<ResultTab>("table");
  // suggestStepId is a DEFAULT, not a leash: once the user picks a step by
  // hand, range changes stop overwriting it — two linked segmented controls
  // that clobber each other read as one duplicated control.
  const stepTouched = useRef(false);

  const mutation = useMutation({
    mutationFn: (): Promise<PromResult> => {
      if (mode === "instant") return promqlQuery(query, at ?? undefined);
      const range = RANGE_OPTIONS.find((r) => r.id === rangeId) ?? RANGE_OPTIONS[1];
      const step = STEP_OPTIONS.find((s) => s.id === stepId) ?? STEP_OPTIONS[0];
      const end = at ?? new Date();
      const start = new Date(end.getTime() - range.seconds * 1000);
      return promqlQueryRange(query, start, end, step.seconds * 1e9);
    },
  });

  /* A mode switch invalidates whatever is on screen (QA scope 4, finding #16).
     Instant answers a vector at ONE timestamp; Range answers a matrix over a
     window — so the table left behind after a switch describes a query the
     controls no longer describe, with nothing saying so. Dropping it is the
     same call runQuery already makes before every re-run: a result nobody can
     tell is stale is worse than no result. The `mode` dep is the whole point,
     hence the exhaustive-deps waiver; reset() is stable across renders. */
  const reset = mutation.reset;
  useEffect(() => {
    reset();
  }, [mode, reset]);

  const handleChange = (v: string) => {
    setQuery(v);
    try {
      localStorage.setItem(LAST_QUERY_KEY, v);
    } catch {
      // localStorage can be unavailable (private-browsing quota etc.); the
      // console still works, it just won't remember the last query.
    }
  };

  /* A query box can arrive empty — a first visit that cleared it, an earlier
     session, another tab, a hand-edited localStorage. Run used to stay lit and
     answer the click with nothing at all. */
  const runnable = query.trim().length > 0;

  const runQuery = () => {
    if (!runnable || mutation.isPending) return;
    // Clear the previous result before firing a new one: otherwise a failed
    // re-run would leave the last successful result visible underneath the
    // fresh error banner, misleadingly implying it's still current.
    mutation.reset();
    mutation.mutate();
  };

  const data = mutation.data;
  /* The stamp is written in the interface language, so the formatter is handed
     to the builder rather than chosen inside it (lib/prom-table.ts). */
    // stampFull, for the reason the Time Machine banner takes it: one clock per console.
  const formatTime = useCallback((ms: number) => stampFull(new Date(ms), locale), [locale]);
  const table = useMemo(() => (data ? toTable(data, formatTime) : undefined), [data, formatTime]);
  /* A PromQL result set is whatever the query matched — thousands of series is
     a normal answer, and the table used to render every one of them. */
  const pager = usePager(table?.rows ?? [], { resetKey: table?.columns.join("\u0000") ?? "" });
  const chart = useMemo(
    /* Built for any MATRIX answer, whatever mode asked for it: an instant query
       on a range vector (`up[5m]`) comes back as a matrix and draws fine. */
    () => (data && data.data?.resultType === "matrix" ? toChartModel(data, theme === "dark") : undefined),
    [data, mode, theme],
  );

  /* Drop back to Table when there is no chart to draw. The condition is the
     RESULT, not the mode: an instant query on a range vector answers with a
     matrix and charts fine, and gating on the mode disabled the tab while the
     tooltip explained the block with a reason the data contradicted. */
  useEffect(() => {
    // `data` guards the wait: while a query is in flight there is no chart yet
    // and nothing has been proven unchartable.
    if (activeTab === "chart" && data !== undefined && chart === undefined) setActiveTab("table");
  }, [activeTab, chart, data]);
  /* Memoised for the same reason the table and the chart are, and here it bites
     hardest: the proxy caps a response at 8 MiB, pretty-printing that is tens of
     megabytes of string, and this used to be rebuilt on EVERY render — once per
     keystroke in the query box while the JSON tab was open. */
  const rawJson = useMemo(() => (data ? JSON.stringify(data, null, 2) : undefined), [data]);
  // promqlQuery/promqlQueryRange resolve (rather than throw) for Prometheus's own error envelope
  // (see lib/api.ts's `handle`).
  const promError = data?.status === "error" ? data : undefined;
  /* Whether an EMPTY-RESULT note is honest at all; the note therefore renders only when nothing failed. */
  const failed = promError !== undefined || mutation.error !== null;

  return (
    <PageShell
      timeMachine
      title={t("title")}
      help={{ body: t("help.body"), slug: "promql" }}
      /* {at} lands INSIDE a translated sentence, so it takes that sentence's
         language — lib/i18n's localeTag. Computed here, never formatted by the
         dictionary (QA scope 2, finding #8). */
      description={at ? t("description.at", { at: stampFull(at, locale) }) : t("description")}
      actions={
        <>
          <Segmented
            aria-label={t("mode.aria")}
            options={[
              { value: "instant", label: t("mode.instant") },
              { value: "range", label: t("mode.range") },
            ]}
            value={mode}
            onChange={setMode}
          />
          {mode === "range" ? (
            <>
              <LabeledControl label={t("range.label")}>
                <Segmented
                  aria-label={t("range.aria")}
                  options={RANGE_OPTIONS.map((r) => ({ value: r.id, label: r.label }))}
                  value={rangeId}
                  onChange={(id) => {
                    setRangeId(id);
                    if (!stepTouched.current) {
                      setStepId(suggestStepId(RANGE_OPTIONS.find((r) => r.id === id)?.seconds ?? 3600));
                    }
                  }}
                />
              </LabeledControl>
              <LabeledControl label={t("step.label")}>
                <Segmented
                  aria-label={t("step.aria")}
                  options={STEP_OPTIONS.map((s) => ({ value: s.id, label: s.label }))}
                  value={stepId}
                  onChange={(id) => {
                    stepTouched.current = true;
                    setStepId(id);
                  }}
                />
              </LabeledControl>
            </>
          ) : null}
          <Button onClick={runQuery} loading={mutation.isPending} disabled={!runnable}>
            {t("run")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-5">
        <Card className="overflow-hidden p-2">
          <PromQLEditor initial={query} onChange={handleChange} onRun={runQuery} label={t("editor.aria")} />
        </Card>

        {mutation.error ? (
          <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-4 text-sm">
            {mutation.error.message}
          </Card>
        ) : null}
        {promError ? (
          <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-4 text-sm">
            {/* errorType and error are PROMETHEUS's own words and render
                verbatim; only the stand-in for a missing message is ours. */}
            {promError.errorType ? `${promError.errorType}: ` : ""}
            {promError.error ?? t("queryFailed")}
          </Card>
        ) : null}

        <ResultTabs
          active={activeTab}
          onChange={setActiveTab}
          /* The RESULT decides, not the mode: `up[5m]` in instant mode answers
             with a matrix, which draws perfectly well, and the disabled tab's
             tooltip was telling the reader something the data contradicted. */
          /* Disabled only once a RESULT has proven itself unchartable. Before
             any query there is nothing to draw yet and nothing to refuse — the
             reader routinely picks the tab first and runs second. */
          isDisabled={(id) => id === "chart" && data !== undefined && chart === undefined}
        />

        {activeTab === "table" ? (
          <ResultPanel tab="table">
            {table && table.rows.length > 0 ? (
              <Card className="overflow-x-auto p-0">
                {/* The instant every row shares, said ONCE. When the rows
                    disagree — a series that stopped early — there is no
                    sentence here and prom-table puts a `time` column in the
                    table instead. */}
                {table.at !== null ? (
                  <p className="border-b border-border px-4 py-2 text-xs text-muted-foreground">
                    {t(table.kind === "series" ? "table.lastAt" : "table.at", { at: formatTime(table.at) })}
                  </p>
                ) : null}
                <table className="w-full border-separate border-spacing-0 text-sm">
                  <thead>
                    <tr>
                      {table.columns.map((c) => (
                        <th
                          key={c}
                          className="border-b border-border bg-surface px-4 py-2.5 text-left text-[11px] font-medium uppercase tracking-[0.06em] text-muted-foreground"
                          scope="col"
                        >
                          {/* Our own columns travel as dictionary keys
                              (lib/prom-table.ts); a LABEL name is Prometheus's
                              identifier and is printed as it arrived. */}
                          {c.startsWith("table.col.") ? t(c as PromQLConsoleKey) : c}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {pager.visible.map((row, i) => (
                      <tr key={i} className="transition-colors duration-(--dur-fast) hover:bg-accent/40">
                        {row.map((cell, j) => (
                          <td key={j} className="mono-data border-b border-border px-4 py-2.5">{cell}</td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
                <Pager pager={pager} subject={t("table.subject")} />
              </Card>
            ) : failed ? null : (
              <ResultPlaceholder text={data ? t("table.empty") : t("table.idle")} />
            )}
          </ResultPanel>
        ) : null}

        {/* Grafana Explore's shape: the picture, and under it the listing of
            what is IN the picture — at the same time, never one instead of the
            other. The Chart tab used to be the plot alone, so the only account
            of its 86 series was a legend that had become a "1/86" pager. */}
        {/* The panel exists while its TAB is selected — aria-controls has to point
            at something — and the branches inside it draw the chart, the empty
            note or the idle line. Gating the panel itself on `chart` left the
            selected tab controlling an element that was not in the document. */}
        {activeTab === "chart" ? (
          <ResultPanel tab="chart">
            {chart && chart.series.length > 0 ? (
              <div className="flex flex-col gap-4">
                <Card className="p-5">
                  <EChart option={chart.option} className="h-80 w-full" />
                </Card>
                <RawSeriesTable series={chart.series} sharedText={chart.sharedText} />
              </div>
            ) : failed ? null : (
              <ResultPlaceholder text={data ? t("chart.empty") : t("chart.idle")} />
            )}
          </ResultPanel>
        ) : null}

        {activeTab === "json" ? (
          <ResultPanel tab="json">
            <Card className="overflow-hidden p-0">
              <pre className="mono-data max-h-[32rem] overflow-auto bg-surface-2/50 p-4 leading-relaxed">
                {rawJson ?? t("json.idle")}
              </pre>
            </Card>
          </ResultPanel>
        ) : null}
      </div>
    </PageShell>
  );
}

function ResultPlaceholder({ text }: { text: string }) {
  return (
    <Card className="flex flex-col items-center gap-2 px-6 py-12 text-center">
      <SquareTerminal aria-hidden="true" className="size-5 text-muted-foreground" />
      <p className="text-xs text-muted-foreground">{text}</p>
    </Card>
  );
}

/* Range and Step are two same-shaped segmented controls with overlapping
   option labels ("15m" lives in both) — without a caption they read as one
   duplicated control. */
function LabeledControl({ label, children }: { label: string; children: ReactNode }) {
  return (
    <span className="flex items-center gap-1.5">
      <span className="text-[11px] font-medium uppercase tracking-[0.06em] text-muted-foreground">
        {label}
      </span>
      {children}
    </span>
  );
}
