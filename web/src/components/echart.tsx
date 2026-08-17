import { useEffect, useId, useMemo, useRef } from "react";
import * as echarts from "echarts";
import { withAnnotations, withMaintenance } from "@/lib/annotations";
import {
  READOUT_ROW_CAP,
  isTimeSeriesOption,
  pickReadoutRows,
  readoutSeries,
  useChartCursor,
  type ReadoutSeries,
} from "@/lib/chart-cursor";
import { sharedTooltipOption } from "@/lib/chart-tooltip";
import { useT } from "@/lib/i18n";
import { sharedDict } from "@/lib/i18n/dict/shared";
import type { Annotation, MaintenanceWindow } from "@/lib/types";
import { cn } from "@/lib/utils";

/** What the per-frame draw needs and React owns; see the live ref below. */
interface ReadoutLive {
  series: ReadoutSeries[];
}

/**
 * EChart is the one mount point for every chart in the console; doing it here rather than in each
 * caller means a page passes the annotations it fetched and gets the same marker treatment on every
 * surface.
 *
 * Three behaviours every chart now gets for free, and gets HERE for the same
 * reason the annotations do:
 *
 *   - a tooltip clamped to the WINDOW rather than to the panel (lib/chart-tooltip.ts),
 *   - the page's shared time cursor (lib/chart-cursor.tsx), drawn as one absolutely
 *     positioned line rather than through setOption, so a mousemove costs a style
 *     write instead of a chart redraw,
 *   - and, on the panels that line lands on, a dot on each series' own point at
 *     that instant. The line alone answered "when"; the dots answer "where on
 *     this panel's own curve", which is the half the reader could not get by
 *     looking. The VALUES stay with the tooltip of the panel actually under the
 *     pointer — a box on every neighbour covers the data it annotates.
 *
 * The dots obey the cursor's rule rather than getting one of their own: the
 * DOM it writes into is built once per option and only written into per frame,
 * inside the single animation frame the group already books. No setOption and no
 * React render sits on the mousemove path — components/echart.test.tsx pins both.
 */
export function EChart({
  option,
  className,
  annotations,
  maintenance,
  dark = true,
}: {
  option: echarts.EChartsOption;
  className?: string;
  annotations?: Annotation[];
  maintenance?: MaintenanceWindow[];
  dark?: boolean;
}) {
  const host = useRef<HTMLDivElement>(null);
  const crosshair = useRef<HTMLDivElement>(null);
  const chart = useRef<echarts.ECharts | null>(null);
  /** This chart's identity in the cursor group, so it can ignore its own echo. */
  const id = useId();
  const group = useChartCursor();
  const t = useT(sharedDict);

  /* The readout's own DOM, held as elements rather than as state: everything
     below is written from inside the cursor group's one animation frame. */
  const dots = useRef<(HTMLDivElement | null)[]>([]);
  /** Series names the reader switched off in this chart's own legend; see the init effect. */
  const deselected = useRef<Set<string>>(new Set());
  /* What the draw reads about the data. A ref because the draw is subscribed
     ONCE (see the effect below) while the option is replaced by every poll. */
  const live = useRef<ReadoutLive>({ series: [] });
  /** Set by the cursor effect, so a new option can repaint without a mousemove. */
  const repaint = useRef<(() => void) | null>(null);
  /* Where the pointer is on the y-axis, in DATA terms, which is what lets the
     tooltip keep the rows nearest what is being pointed at. A ref because it is
     written per mousemove and read inside a formatter — through React state it
     would be a re-render per pixel. */
  const cursorValue = useRef<number | null>(null);

  // Both helpers return the SAME object when there is nothing to overlay, so a
  // chart with neither keeps the caller's own memo identity and setOption below
  // does not re-run for two empty lists.
  const merged = useMemo(() => {
    const withNotes = annotations && annotations.length > 0 ? withAnnotations(option, annotations, dark) : option;
    return maintenance && maintenance.length > 0 ? withMaintenance(withNotes, maintenance, dark) : withNotes;
  }, [option, annotations, maintenance, dark]);

  /* The tooltip's placement is decided against the live host box, and its rows
     against the live cursor, so the memo closes over the two refs rather than
     over measurements that would go stale on the first scroll or mousemove. */
  const clamped = useMemo(
    () =>
      sharedTooltipOption(merged, () => host.current, {
        cursorValue: () => cursorValue.current,
        more: (count) => t("tooltip.more", { count }),
      }),
    [merged, t],
  );

  /** Only a TIME axis can carry an instant; see lib/chart-cursor.tsx. */
  const synced = group !== null && isTimeSeriesOption(merged);

  /* The neighbour dots' model, rebuilt once per option — NOT per frame. A chart
     with nothing to read (the annotation overlay's marker host is the usual
     case, `data: []` and no points) renders no dot DOM at all. */
  const readout = useMemo(() => (synced ? readoutSeries(merged) : []), [merged, synced]);
  const rowCount = Math.min(readout.length, READOUT_ROW_CAP);

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    chart.current = echarts.init(el);
    /* A series the reader switched OFF in the legend is not on this chart any more, and a dot on
       its sample was the crosshair marking a curve that is not drawn. The option cannot say so —
       legend selection is runtime state — so it is read from the event and applied per frame. */
    chart.current.on?.("legendselectchanged", (event) => {
      const selected = (event as { selected?: Record<string, boolean> }).selected ?? {};
      deselected.current = new Set(Object.keys(selected).filter((name) => !selected[name]));
      repaint.current?.();
    });
    const onResize = () => chart.current?.resize();
    window.addEventListener("resize", onResize);
    /* The window event is not enough (QA scope 3, finding #12). ECharts sizes
       its canvas ONCE, off the host's box at init, and every reflow that does
       not change the window — a sidebar collapsing, a rail wrapping, a details
       panel opening, the grid dropping from two columns to one at lg — left the
       chart drawn at its old width inside a box that had moved on. The observer
       watches the box that actually matters; the window listener stays for the
       browsers that resize the viewport without laying the host out again. */
    const ro = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(onResize);
    ro?.observe(el);
    return () => {
      ro?.disconnect();
      window.removeEventListener("resize", onResize);
      chart.current?.dispose();
      chart.current = null;
    };
  }, []);

  useEffect(() => {
    /* notMerge resets the legend to every series selected, so the memory of what the reader had
       switched off goes with it — keeping it would hide dots for series that are back on screen. */
    deselected.current = new Set();
    chart.current?.setOption(clamped, { notMerge: true });
  }, [clamped]);

  /* Hands the per-frame draw the data it must not go looking for itself, and
     repaints once — a poll landing under a standing cursor would otherwise
     leave the previous window's numbers beside a line that is still correct.
     Declared ABOVE the subscription so the catch-up draw there already has a
     model to read on the first mount. */
  useEffect(() => {
    live.current = { series: readout };
    // React grew or shrank the pool; the entries past it are last render's.
    dots.current.length = rowCount;
    repaint.current?.();
  }, [readout, rowCount]);

  /* The pointer, read once per move and used by two different things: the row
     cap above (every chart, whether or not it is in a sync group) and the shared
     instant below (time-axis charts in a group only). ONE listener for both. */
  useEffect(() => {
    const c = chart.current;
    if (!c) return;
    const zr = c.getZr?.();
    const onMove = (e: { offsetX: number; offsetY?: number }) => {
      const y = c.convertFromPixel?.({ yAxisIndex: 0 }, e.offsetY ?? Number.NaN);
      cursorValue.current = typeof y === "number" && Number.isFinite(y) ? y : null;
      if (!group || !synced) return;
      const at = c.convertFromPixel?.({ xAxisIndex: 0 }, e.offsetX);
      if (typeof at === "number" && Number.isFinite(at)) group.set(at, id);
    };
    const onOut = () => {
      cursorValue.current = null;
      if (group && synced) group.set(null, id);
    };
    zr?.on("mousemove", onMove);
    zr?.on("globalout", onOut);
    return () => {
      zr?.off("mousemove", onMove);
      zr?.off("globalout", onOut);
    };
  }, [group, synced, id]);

  useEffect(() => {
    const c = chart.current;
    const line = crosshair.current;
    if (!group || !synced || !c || !line) return;

    /** Puts the dots away; the panel is back to just its data and the line. */
    const clearReadout = () => {
      for (const dot of dots.current) if (dot) dot.style.opacity = "0";
    };

    /**
     * drawReadout marks WHERE each of this panel's own series sits at the shared
     * instant: one dot per series, on its own point.
     *
     * It does NOT name the values. The reader is pointing at one panel and
     * reading that panel's tooltip; a box on every neighbour covers the data it
     * is annotating and turns a glance into a search (owner report). The line
     * answers "when", the dots answer "where", and the panel under the cursor
     * answers "how much".
     *
     * It is all element writes. No setOption (a redraw per pixel is what the
     * crosshair exists to avoid), no React (a page of panels re-rendering per
     * mousemove is what the group exists to avoid), and no frame of its own —
     * it runs inside the one the group already booked.
     */
    const drawReadout = (at: number) => {
      const { rows } = pickReadoutRows(live.current.series, at);
      if (rows.length === 0) {
        clearReadout();
        return;
      }

      for (let i = 0; i < dots.current.length; i++) {
        const dot = dots.current[i];
        const row = rows[i];
        if (!dot) continue;
        if (!row || deselected.current.has(row.series.name)) {
          dot.style.opacity = "0";
          continue;
        }
        /* By SERIES rather than by axis index: a chart with a second y-axis
           would otherwise put its dots on the first one's scale. */
        const point = c.convertToPixel?.({ seriesIndex: row.index }, [row.sample.t, row.sample.v]);
        const [dx, dy] = Array.isArray(point) ? point : [Number.NaN, Number.NaN];
        if (Number.isFinite(dx) && Number.isFinite(dy)) {
          dot.style.background = row.series.color ?? "currentColor";
          dot.style.transform = `translate(${dx}px, ${dy}px)`;
          dot.style.opacity = "1";
        } else {
          dot.style.opacity = "0";
        }
      }
    };


    /** Moves the line to an instant, or hides it — no React, no setOption. */
    const draw = (at: number | null, from: string) => {
      // The hovered chart already draws ECharts' own axis pointer, and its own
      // tooltip already lists every series' value: a second line on top of the
      // first would only be a heavier one, and a second value listing beside
      // the tooltip would be the same numbers twice.
      if (at === null || from === id) {
        line.style.opacity = "0";
        clearReadout();
        return;
      }
      const x = c.convertToPixel?.({ xAxisIndex: 0 }, at);
      const px = typeof x === "number" ? x : Number.NaN;
      // Outside this chart's own window the instant simply has no place here.
      if (!Number.isFinite(px) || px < 0 || px > (c.getWidth?.() ?? 0)) {
        line.style.opacity = "0";
        clearReadout();
        return;
      }
      line.style.transform = `translateX(${px}px)`;
      line.style.opacity = "1";
      drawReadout(at);
    };

    const unsubscribe = group.subscribe(draw);
    // A chart mounted mid-hover — a page's second panel finishing its fetch —
    // catches up rather than waiting for the next mouse move.
    draw(group.current(), "");
    /* And so does a panel whose DATA was replaced mid-hover: the option effect
       above calls this, which is the only way the numbers beside a standing
       cursor stay the ones the chart is currently drawing. */
    repaint.current = () => draw(group.current(), group.currentSource());

    return () => {
      unsubscribe();
      repaint.current = null;
      /* Only the chart the cursor came FROM may clear it. A panel unmounting is
         routine here — a curated card whose thirty-second poll comes back empty,
         a compare leg swapped by a segment click — and clearing unconditionally
         took the crosshair off every surviving panel while the pointer was still
         sitting on the one that published it. */
      if (group.currentSource() === id) group.set(null, id);
    };
    /* Deliberately NOT keyed on the option: a poll landing every thirty seconds
       would otherwise tear the subscription down and clear the cursor on every
       panel of the page mid-hover. `draw` converts through the chart's CURRENT
       axis on each call, so new data is picked up on the next move anyway. */
  }, [group, synced, id]);

  return (
    /* The wrapper owns the caller's box; the chart fills it and the cursor line
       rides above it. One extra div is the price of a crosshair that costs no
       redraw. */
    <div data-testid="chart-panel" className={cn("relative", className ?? "h-64 w-full")}>
      <div ref={host} className="size-full" />
      <div
        ref={crosshair}
        data-testid="chart-crosshair"
        aria-hidden="true"
        style={{ opacity: 0 }}
        className="pointer-events-none absolute inset-y-0 left-0 w-px bg-muted-foreground/70"
      />
      {/* The readout's skeleton, built once per option and then only written
          into. Rendering it per frame is exactly the redraw this whole
          mechanic exists to avoid; the pool is capped, so a ninety-nine series
          query costs five rows here, not ninety-nine.
          aria-hidden for the same reason the crosshair is: it is a pointer-only
          reading of a canvas that a screen reader cannot follow anyway, and the
          numbers live in the Table and Raw views under the chart. */}
      {rowCount > 0 ? (
        <>
          {Array.from({ length: rowCount }, (_, i) => (
            <div
              key={i}
              ref={(el) => {
                dots.current[i] = el;
              }}
              data-testid="chart-readout-dot"
              aria-hidden="true"
              style={{ opacity: 0 }}
              /* The negative margins centre the dot on the sample, so the
                 transform written per frame stays the point's own pixel. */
              className="pointer-events-none absolute left-0 top-0 -ml-[3px] -mt-[3px] size-1.5 rounded-full ring-1 ring-card"
            />
          ))}
          {/* NO box on a neighbour. The reader is pointing at ONE panel and reading
              that panel's tooltip; five more boxes on the panels around it cover
              the very data they annotate and turn a glance into a search (owner
              report). What a neighbour still gets is the line and a dot on each
              of its own series at that instant — where, not a second readout. */}
        </>
      ) : null}
    </div>
  );
}
