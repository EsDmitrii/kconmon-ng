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
import { localeTag, useLocale, useT } from "@/lib/i18n";
import { promqlConsoleDict, type PromQLConsoleKey } from "@/lib/i18n/dict/promql-console";
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

// Chart-tab design decision says the Chart tab should "reuse EChart + toSeriesOption-style
// mapping".
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
  // promqlQuery/promqlQueryRange resolve (rather than throw) for Prometheus's own error envelope
  // (see lib/api.ts's `handle`).
  const promError = data?.status === "error" ? data : undefined;
  /* Whether an EMPTY-RESULT note is honest at all; the note therefore renders only when nothing failed. */
  const failed = promError !== undefined || mutation.error !== null;

  return (
    <PageShell
      title={t("title")}
      /* {at} lands INSIDE a translated sentence, so it takes that sentence's
         language — lib/i18n's localeTag. Computed here, never formatted by the
         dictionary (QA scope 2, finding #8). */
      description={at ? t("description.at", { at: at.toLocaleString(localeTag(locale)) }) : t("description")}
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
          <Button onClick={runQuery} loading={mutation.isPending}>
            {t("run")}
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
            {/* errorType and error are PROMETHEUS's own words and render
                verbatim; only the stand-in for a missing message is ours. */}
            {promError.errorType ? `${promError.errorType}: ` : ""}
            {promError.error ?? t("queryFailed")}
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
              <ResultPlaceholder text={data ? t("table.empty") : t("table.idle")} />
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
              <ResultPlaceholder text={data ? t("chart.empty") : t("chart.idle")} />
            )}
          </ResultPanel>
        ) : null}

        {activeTab === "json" ? (
          <ResultPanel tab="json">
            <Card className="overflow-hidden p-0">
              <pre className="max-h-[32rem] overflow-auto bg-surface-2/50 p-4 font-mono text-xs leading-relaxed">
                {data ? JSON.stringify(data, null, 2) : t("json.idle")}
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
