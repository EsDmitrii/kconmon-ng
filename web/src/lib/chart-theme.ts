/* Single source of chart colour. ECharts renders to canvas and cannot consume
   CSS custom properties, so the design-system tokens are read off the document
   root at runtime and handed to ECharts as concrete hsl() strings. Under jsdom
   (or any environment where the variables resolve empty) the documented token
   values below are used — they mirror index.css and are exported for tests. */

export interface ChartColors {
  series: string[];
  /* A 6th+ series folds into this muted colour instead of wrapping the ramp. */
  other: string;
  axis: string;
  grid: string;
  surface: string;
}

/* Documented values of the index.css tokens, per theme. Keep in sync with
   index.css — the fallback test pins these.

   NOTE the comma syntax: canvas accepts modern space-separated hsl(), but
   zrender parses colours with its OWN parser when deriving hover/emphasis
   state colours, and that parser only understands the comma form. Feeding it
   `hsl(210 68% 59%)` renders fine at rest and turns every line invisible the
   moment the axis pointer activates a state transition. */
export const CHART_FALLBACK: Record<"dark" | "light", ChartColors> = {
  dark: {
    series: [
      "hsl(210, 68%, 59%)",
      "hsl(166, 57%, 42%)",
      "hsl(265, 64%, 66%)",
      "hsl(25, 63%, 52%)",
      "hsl(328, 61%, 62%)",
    ],
    other: "hsl(224, 12%, 64%)",
    axis: "hsl(224, 12%, 64%)",
    grid: "hsl(230, 11%, 15%)",
    surface: "hsl(231, 14%, 9.5%)",
  },
  light: {
    series: [
      "hsl(216, 63%, 50%)",
      "hsl(165, 81%, 31%)",
      "hsl(258, 63%, 59%)",
      "hsl(29, 85%, 39%)",
      "hsl(328, 48%, 52%)",
    ],
    other: "hsl(226, 11%, 46%)",
    axis: "hsl(226, 11%, 46%)",
    grid: "hsl(222, 22%, 93%)",
    surface: "hsl(0, 0%, 100%)",
  },
};

/* index.css stores triplets as "210 68% 59%"; rewrite to the comma form the
   zrender colour parser requires (see CHART_FALLBACK note). */
function readVar(styles: CSSStyleDeclaration, name: string): string | null {
  const raw = styles.getPropertyValue(name).trim();
  if (!raw) return null;
  return `hsl(${raw.split(/\s+/).join(", ")})`;
}

/* Reads the live token values for the current theme; falls back per token so a
   partially-themed embed still gets sane colours. Call again on theme change —
   the caller keys its memo on the theme value. */
export function chartColors(theme: "dark" | "light"): ChartColors {
  const fallback = CHART_FALLBACK[theme];
  if (typeof document === "undefined") return fallback;
  const styles = getComputedStyle(document.documentElement);
  return {
    series: [1, 2, 3, 4, 5].map(
      (i) => readVar(styles, `--chart-${i}`) ?? fallback.series[i - 1],
    ),
    other: readVar(styles, "--chart-axis") ?? fallback.other,
    axis: readVar(styles, "--chart-axis") ?? fallback.axis,
    grid: readVar(styles, "--chart-grid") ?? fallback.grid,
    surface: readVar(styles, "--surface") ?? fallback.surface,
  };
}

/* Series colours are assigned in the fixed declared order and never cycled:
   index 0-4 take the validated ramp, everything after folds into `other`. */
export function seriesColor(colors: ChartColors, index: number): string {
  return index < colors.series.length ? colors.series[index] : colors.other;
}
