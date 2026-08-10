import { useEffect, useMemo, useRef } from "react";
import * as echarts from "echarts";
import { withAnnotations, withMaintenance } from "@/lib/annotations";
import type { Annotation, MaintenanceWindow } from "@/lib/types";

/**
 * EChart is the one mount point for every chart in the console; doing it here rather than in each
 * caller means a page passes the annotations it fetched and gets the same marker treatment on every
 * surface.
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
    const host = ref.current;
    if (!host) return;
    chart.current = echarts.init(host);
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
    ro?.observe(host);
    return () => {
      ro?.disconnect();
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
