import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import type * as echarts from "echarts";
import { useMutation } from "@tanstack/react-query";
import { SquareTerminal } from "lucide-react";
import { EChart } from "@/components/echart";
import { PageShell } from "@/components/page-shell";
import { PromQLEditor } from "@/components/promql-editor";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { useTheme } from "@/components/theme-provider";
import { promqlQuery, promqlQueryRange } from "@/lib/api";
import { chartColors, seriesColor } from "@/lib/chart-theme";
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

function labelForEntry(metric: Record<string, string>): string {
  const parts = Object.entries(metric).map(([k, v]) => `${k}="${v}"`);
  return parts.length > 0 ? `{${parts.join(", ")}}` : "value";
}

// --- Chart-tab design decision (Task 14) ---------------------------------
// The brief says the Chart tab should "reuse EChart + toSeriesOption-style
// mapping", but toSeriesOption (lib/curated-metrics.ts) is shaped for the 5
// fixed CuratedChart queries: it takes a `unit: "seconds" | "ratio"` and
// formats axis labels/tooltips accordingly (ms, %). An arbitrary user-typed
// PromQL query has no such known unit — force-fitting it through
// toSeriesOption would mean guessing a unit (wrong most of the time) or
// always lying with a seconds/ratio label on unrelated values.
//
// Instead this is a small local series-option builder: it maps a matrix
// PromResult onto an ECharts line-series option with a neutral, unformatted
// numeric y-axis (raw values, no ms/% conversion), and a generic
// label-set-based series name instead of the peer/host/protocol-specific
// naming toSeriesOption uses. It reuses the EChart wrapper component and the
// design-system chart palette, but not toSeriesOption itself or its unit
// formatters.
function toConsoleChartOption(res: PromResult, dark: boolean): echarts.EChartsOption {
  const entries: MatrixEntry[] =
    res.status === "success" && res.data?.resultType === "matrix" ? (res.data.result as MatrixEntry[]) : [];
  const colors = chartColors(dark ? "dark" : "light");
  const sorted = [...entries].sort((a, b) =>
    labelForEntry(a.metric).localeCompare(labelForEntry(b.metric)),
  );

  return {
    animation: false,
    textStyle: { color: colors.axis },
    grid: { left: 56, right: 16, top: 12, bottom: 46 },
    legend: {
      bottom: 0,
      type: "scroll",
      icon: "roundRect",
      itemWidth: 10,
      itemHeight: 2,
      textStyle: { color: colors.axis, fontSize: 11 },
      pageIconColor: colors.axis,
      pageTextStyle: { color: colors.axis },
    },
    tooltip: { trigger: "axis", axisPointer: { type: "line", lineStyle: { color: colors.grid } } },
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
      axisLabel: { color: colors.axis },
      splitLine: { lineStyle: { color: colors.grid } },
    },
    series: sorted.map(
      (entry, i): echarts.LineSeriesOption => ({
        name: labelForEntry(entry.metric),
        type: "line",
        showSymbol: false,
        color: seriesColor(colors, i),
        lineStyle: { width: 2 },
        data: (entry.values ?? []).map(([ts, v]) => [ts * 1000, Number(v)]),
      }),
    ),
  };
}

function readLastQuery(): string {
  try {
    return localStorage.getItem(LAST_QUERY_KEY) ?? DEFAULT_QUERY;
  } catch {
    return DEFAULT_QUERY;
  }
}

const TABS: { id: ResultTab; label: string }[] = [
  { id: "table", label: "Table" },
  { id: "chart", label: "Chart" },
  { id: "json", label: "JSON" },
];

/* One console, one result switcher, so the ids can be constants rather than
   useId() values — and constants keep the tab⇄panel wiring readable in the
   markup below. */
const tabDomId = (id: ResultTab) => `promql-result-tab-${id}`;
const panelDomId = (id: ResultTab) => `promql-result-panel-${id}`;

/**
 * ResultTabs is the result switcher, and it declares role="tablist" — so it
 * owes the whole tab contract, not the half of it that is easy.
 *
 * What it was: three independent tab stops carrying role="tab" and
 * aria-selected, with no keyboard relationship between them and three panels
 * that carried no role at all. A screen-reader user was told "tab, selected"
 * and then had nothing to move to.
 *
 * What it is: ONE tab stop for the strip (roving tabindex — the idiom
 * ui/segmented.tsx already uses for its radiogroup, which is what every OTHER
 * switcher on this page is), arrows/Home/End moving selection and focus
 * together, and each tab pointing at the panel it reveals via aria-controls.
 *
 * A disabled tab is STEPPED OVER rather than landed on: Chart is disabled in
 * instant mode, a disabled button is not focusable, and an arrow key that
 * moved selection onto it would strand the keyboard on an element that cannot
 * take focus.
 *
 * Exported for its own test: this page mounts PromQLEditor (CodeMirror) and
 * EChart, neither of which is a comfortable jsdom render, so the strip is
 * pinned directly — the same seam pages/topology.tsx opened with
 * nodeNavigationPath and pages/diagnostics.tsx with NodeSelector.
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
    <div role="tablist" aria-label="Result view" className="flex w-fit gap-1 rounded-md bg-surface-2 p-1">
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
            title={disabled ? "Chart is only available for range queries" : undefined}
          >
            {tab.label}
          </button>
        );
      })}
    </div>
  );
}

/* The panel half of the same contract: a labelled tabpanel that is itself a
   tab stop, because none of the three (a table, a chart canvas, a <pre>) holds
   anything focusable of its own — without this, Tab off the strip leaves the
   result behind entirely. */
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
  const { theme } = useTheme();
  /**
   * The Time Machine anchors this page too (QA round 4, finding #3). It was
   * the one data surface that ignored `?at=` entirely: with the console
   * engaged at an incident, /console kept answering with NOW — an operator
   * pasting a query from a card they were reading "at 02:14" got the present
   * fleet's numbers under a banner that said 02:14.
   *
   * The anchoring is the SAME rule every other surface uses:
   *  - an instant query sends `time=at` (the API already takes it — lib/api's
   *    promqlQuery has carried the parameter since M5);
   *  - a range query anchors its END at `at` and measures the picked window
   *    backwards from there, exactly as Explore's useExploreQuery does, so
   *    "6h" engaged means the six hours BEFORE t.
   *
   * TODO(docs): TIME_MACHINE.md's per-page table still lists Console as "not
   * anchored". The control pass folds the docs; this comment is the marker.
   */
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

  // Chart is range-only; drop back to Table if the user switches to instant
  // mode while Chart is selected.
  useEffect(() => {
    if (mode === "instant" && activeTab === "chart") setActiveTab("table");
  }, [mode, activeTab]);

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

  const handleChange = (v: string) => {
    setQuery(v);
    try {
      localStorage.setItem(LAST_QUERY_KEY, v);
    } catch {
      // localStorage can be unavailable (private-browsing quota etc.); the
      // console still works, it just won't remember the last query.
    }
  };

  const runQuery = () => {
    if (!query.trim() || mutation.isPending) return;
    // Clear the previous result before firing a new one: otherwise a failed
    // re-run would leave the last successful result visible underneath the
    // fresh error banner, misleadingly implying it's still current.
    mutation.reset();
    mutation.mutate();
  };

  const data = mutation.data;
  const table = useMemo(() => (data ? toTable(data) : undefined), [data]);
  const chartOption = useMemo(
    () => (data && mode === "range" ? toConsoleChartOption(data, theme === "dark") : undefined),
    [data, mode, theme],
  );
  // promqlQuery/promqlQueryRange resolve (rather than throw) for Prometheus's
  // own error envelope (see lib/api.ts's `handle`), so a query-level failure
  // surfaces via data.status, distinct from mutation.error (an ApiError from
  // a problem+json response, or a network-level failure).
  const promError = data?.status === "error" ? data : undefined;
  /* Whether an EMPTY-RESULT note is honest at all (QA round 4, finding #2).
     A failed query has no result to be empty: the page was rendering the red
     error card AND "No data — the query returned an empty result." at the
     same time, which reads as two independent facts and told an operator the
     query ran and matched nothing. The note therefore renders only when
     nothing failed — either the envelope came back `success`, or no query has
     been run yet (which is the "Run a query to see results." case). */
  const failed = promError !== undefined || mutation.error !== null;

  return (
    <PageShell
      title="Console"
      description={
        at
          ? `Ad-hoc PromQL as of ${at.toLocaleString()} — instant queries are evaluated at that instant, and a range ends there.`
          : "Run ad-hoc PromQL against the same Prometheus the rest of the console reads from."
      }
      actions={
        <>
          <Segmented
            aria-label="Query mode"
            options={[
              { value: "instant", label: "Instant" },
              { value: "range", label: "Range" },
            ]}
            value={mode}
            onChange={setMode}
          />
          {mode === "range" ? (
            <>
              <LabeledControl label="Range">
                <Segmented
                  aria-label="Range"
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
              <LabeledControl label="Step">
                <Segmented
                  aria-label="Step"
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
          <Button onClick={runQuery} loading={mutation.isPending}>
            Run
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-5">
        <Card className="overflow-hidden p-2">
          <PromQLEditor initial={query} onChange={handleChange} onRun={runQuery} />
        </Card>

        {mutation.error ? (
          <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-4 text-sm">
            {mutation.error.message}
          </Card>
        ) : null}
        {promError ? (
          <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-4 text-sm">
            {promError.errorType ? `${promError.errorType}: ` : ""}
            {promError.error ?? "query failed"}
          </Card>
        ) : null}

        <ResultTabs
          active={activeTab}
          onChange={setActiveTab}
          isDisabled={(id) => id === "chart" && mode === "instant"}
        />

        {activeTab === "table" ? (
          <ResultPanel tab="table">
            {table && table.rows.length > 0 ? (
              <Card className="overflow-x-auto p-0">
                <table className="w-full border-separate border-spacing-0 text-sm">
                  <thead>
                    <tr>
                      {table.columns.map((c) => (
                        <th
                          key={c}
                          className="border-b border-border bg-surface px-4 py-2.5 text-left text-[11px] font-medium uppercase tracking-[0.06em] text-muted-foreground"
                          scope="col"
                        >
                          {c}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {table.rows.map((row, i) => (
                      <tr key={i} className="transition-colors duration-(--dur-fast) hover:bg-accent/40">
                        {row.map((cell, j) => (
                          <td key={j} className="nums border-b border-border px-4 py-2.5">{cell}</td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </Card>
            ) : failed ? null : (
              <ResultPlaceholder text={data ? "No data — the query returned an empty result." : "Run a query to see results."} />
            )}
          </ResultPanel>
        ) : null}

        {activeTab === "chart" && mode === "range" ? (
          <ResultPanel tab="chart">
            {chartOption && chartOption.series && (chartOption.series as unknown[]).length > 0 ? (
              <Card className="p-5">
                <EChart option={chartOption} className="h-80 w-full" />
              </Card>
            ) : failed ? null : (
              <ResultPlaceholder text={data ? "No series to chart." : "Run a range query to see a chart."} />
            )}
          </ResultPanel>
        ) : null}

        {activeTab === "json" ? (
          <ResultPanel tab="json">
            <Card className="overflow-hidden p-0">
              <pre className="max-h-[32rem] overflow-auto bg-surface-2/50 p-4 font-mono text-xs leading-relaxed">
                {data ? JSON.stringify(data, null, 2) : "Run a query to see the raw response."}
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
      <span className="text-[10.5px] font-semibold uppercase tracking-[0.08em] text-muted-foreground/70">
        {label}
      </span>
      {children}
    </span>
  );
}
