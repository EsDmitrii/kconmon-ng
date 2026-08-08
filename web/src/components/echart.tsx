import { useEffect, useMemo, useRef } from "react";
import * as echarts from "echarts";
import { withAnnotations, withMaintenance } from "@/lib/annotations";
import type { Annotation, MaintenanceWindow } from "@/lib/types";

/**
 * EChart is the one mount point for every chart in the console.
 *
 * `annotations` (M5 Task 12) is an OVERLAY, not data: the option a caller
 * builds is left exactly as it was and the markers are appended as one extra
 * series (lib/annotations.ts's withAnnotations — instants become markLine,
 * spans become markArea, both muted, text on hover). Doing it here rather than
 * in each caller means a page passes the annotations it fetched and gets the
 * same marker treatment on every surface, with no per-page option surgery.
 *
 * `maintenance` (M6 Task 9) is the SECOND overlay, on exactly the same terms:
 * declared change windows become one more markArea series, muted and dashed so
 * a band an operator declared cannot be mistaken for a note somebody wrote. It
 * rides here rather than in each page for the reason the annotations do — a
 * page passes what it fetched and every chart in the console draws it the same
 * way, with no per-page option surgery.
 *
 * `dark` is a separate prop rather than a useTheme() call on purpose. Not every
 * tree that mounts a chart carries a ThemeProvider (useTheme throws without
 * one), and the caller already knows which theme it built its own option for —
 * asking it to say so keeps the overlay's colours and the series' colours from
 * ever disagreeing.
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
  const ref = useRef<HTMLDivElement>(null);
  const chart = useRef<echarts.ECharts | null>(null);

  // Both helpers return the SAME object when there is nothing to overlay, so a
  // chart with neither keeps the caller's own memo identity and setOption below
  // does not re-run for two empty lists.
  const merged = useMemo(() => {
    const withNotes = annotations && annotations.length > 0 ? withAnnotations(option, annotations, dark) : option;
    return maintenance && maintenance.length > 0 ? withMaintenance(withNotes, maintenance, dark) : withNotes;
  }, [option, annotations, maintenance, dark]);

  useEffect(() => {
    if (!ref.current) return;
    chart.current = echarts.init(ref.current);
    const onResize = () => chart.current?.resize();
    window.addEventListener("resize", onResize);
    return () => {
      window.removeEventListener("resize", onResize);
      chart.current?.dispose();
      chart.current = null;
    };
  }, []);

  useEffect(() => {
    chart.current?.setOption(merged, { notMerge: true });
  }, [merged]);

  return <div ref={ref} className={className ?? "h-64 w-full"} />;
}
