import { afterEach, describe, expect, it } from "vitest";
import { CHART_FALLBACK, chartColors } from "./chart-theme";

/* Owner screenshot: after switching to the light theme the chart tooltip stayed dark. The chart
   rebuilds its colours during the render that flips the theme, before ThemeProvider's effect moves
   the class on <html>, so the live CSS variables still belong to the old theme at that moment. */
describe("chartColors while the theme is switching", () => {
  const root = document.documentElement;
  afterEach(() => {
    root.classList.remove("dark", "light");
    root.style.removeProperty("--popover");
  });

  it("does not read the old theme's variables for the new theme", () => {
    root.classList.add("dark");
    root.style.setProperty("--popover", "230 12% 13%");
    expect(chartColors("light").tooltip.background).toBe(CHART_FALLBACK.light.tooltip.background);
  });

  it("still reads the live variables once the document carries the requested theme", () => {
    root.classList.add("dark");
    root.style.setProperty("--popover", "200 10% 20%");
    expect(chartColors("dark").tooltip.background).toBe("hsl(200, 10%, 20%)");
  });
});
