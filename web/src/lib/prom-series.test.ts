import { describe, expect, it } from "vitest";
import { LEGEND_MAX_SERIES, paletteColor, seriesIdentities, showLegend } from "./prom-series";

/**
 * The owner's rejection of the Console's results: `up` on the stand matches 86
 * series that share every label but `pod`, and every surface printed the WHOLE
 * label set — `{__name__="up", container="agent", endpoint="http", i…` — cut off
 * mid-label by whatever box it was in. Eighty-six identical prefixes is not an
 * identity; the two characters that differ were the ones that got truncated.
 *
 * seriesIdentities is the answer, and it is the Grafana rule: a label whose
 * value is the same on EVERY series in the result distinguishes nothing, so it
 * is said once above the listing and dropped from every row.
 */

/** The stand's own shape: 86 kubelet `up` series differing only by pod. */
const fleet = (pods: string[]) =>
  pods.map((pod) => ({
    __name__: "up",
    container: "agent",
    endpoint: "http",
    job: "kconmon-agent",
    namespace: "kconmon",
    pod,
  }));

describe("seriesIdentities — what actually tells two series apart", () => {
  it("keeps ONLY the label that differs, and says the rest once", () => {
    const { series, shared, sharedName } = seriesIdentities(fleet(["agent-5nkfd", "agent-q2xlm"]));

    expect(series.map((s) => s.text)).toEqual(['up{pod="agent-5nkfd"}', 'up{pod="agent-q2xlm"}']);
    // The five labels every series carries are stated once, not eighty-six times.
    expect(shared).toEqual([
      ["container", "agent"],
      ["endpoint", "http"],
      ["job", "kconmon-agent"],
      ["namespace", "kconmon"],
    ]);
    expect(sharedName).toBe("up");
  });

  it("keeps every differing label when more than one differs, in a stable order", () => {
    const { series } = seriesIdentities([
      { __name__: "up", job: "a", pod: "p1", zone: "z1" },
      { __name__: "up", job: "a", pod: "p2", zone: "z2" },
    ]);

    expect(series.map((s) => s.text)).toEqual(['up{pod="p1", zone="z1"}', 'up{pod="p2", zone="z2"}']);
  });

  it("keeps __name__ per row when the result mixes metrics, and shares none", () => {
    const { series, sharedName } = seriesIdentities([
      { __name__: "up", job: "a" },
      { __name__: "scrape_duration_seconds", job: "a" },
    ]);

    expect(series.map((s) => s.text)).toEqual(["up", "scrape_duration_seconds"]);
    expect(sharedName).toBe("");
  });

  it("is the bare metric name for a single series — every label it has is shared", () => {
    const { series, shared } = seriesIdentities([{ __name__: "up", job: "a", pod: "p1" }]);

    expect(series[0].text).toBe("up");
    expect(shared).toEqual([
      ["job", "a"],
      ["pod", "p1"],
    ]);
  });

  it("falls back to the FULL labels when a minimal identity would collide", () => {
    /* Two series agreeing on every label cannot be told apart by anything, and
       two rows reading a bare `up` would hide that fact behind a tidy string.
       Showing everything both carry at least puts the whole truth on screen.
       Prometheus does not emit this; a recording rule or a fixture can. */
    const { series } = seriesIdentities([
      { __name__: "up", job: "a" },
      { __name__: "up", job: "a" },
    ]);

    expect(series.map((s) => s.text)).toEqual(['up{job="a"}', 'up{job="a"}']);
  });

  it("treats a MISSING label as a difference — presence tells them apart too", () => {
    const { series } = seriesIdentities([
      { __name__: "up", job: "a", pod: "p1" },
      { __name__: "up", job: "a" },
    ]);

    expect(series.map((s) => s.text)).toEqual(['up{pod="p1"}', "up"]);
  });

  it("is empty for an empty result rather than inventing a row", () => {
    const set = seriesIdentities([]);
    expect(set.series).toEqual([]);
    expect(set.shared).toEqual([]);
    expect(set.sharedName).toBe("");
    expect(set.sharedText).toBe("");
  });

  it("survives a series with no labels at all", () => {
    const { series } = seriesIdentities([{}]);
    expect(series[0].text).toBe("{}");
  });

  it("keeps every label reachable, which is what the row expand shows", () => {
    const { series } = seriesIdentities(fleet(["agent-5nkfd", "agent-q2xlm"]));

    expect(series[0].fullText).toBe(
      'up{container="agent", endpoint="http", job="kconmon-agent", namespace="kconmon", pod="agent-5nkfd"}',
    );
    expect(series[0].full).toContainEqual(["namespace", "kconmon"]);
  });

  it("renders the shared part as one readable line for the header", () => {
    const { sharedText } = seriesIdentities(fleet(["a", "b"]));
    expect(sharedText).toBe('up{container="agent", endpoint="http", job="kconmon-agent", namespace="kconmon"}');
  });

  it("stays index-aligned with the input, so a caller can zip it against its own rows", () => {
    const metrics = fleet(["a", "b", "c"]);
    const { series } = seriesIdentities(metrics);
    expect(series).toHaveLength(3);
    expect(series[2].text).toBe('up{pod="c"}');
  });
});

/* ── the legend that became a one-series-per-page pager ──────────────────── */

/**
 * ECharts' scroll legend does not wrap past the room it has: at 86 series it
 * became "1/86" with a single name beside two arrows, which is a control for
 * reading one label at a time. Past the threshold the RAW TABLE is the legend —
 * it lists every series, ten to a page, with the same identities.
 */
describe("showLegend", () => {
  it("keeps the legend while it can be read at a glance", () => {
    expect(showLegend(1)).toBe(true);
    expect(showLegend(LEGEND_MAX_SERIES)).toBe(true);
  });

  it("drops it once it would page rather than list", () => {
    expect(showLegend(LEGEND_MAX_SERIES + 1)).toBe(false);
    expect(showLegend(86)).toBe(false);
  });

  it("agrees with the product's page size, so a shown legend and page one match", () => {
    // The threshold is the pager's own default on purpose: whenever the legend
    // is drawn, the raw table's first page holds exactly the same series.
    expect(LEGEND_MAX_SERIES).toBe(10);
  });

  it("draws nothing for a result with no series at all", () => {
    expect(showLegend(0)).toBe(false);
  });
});

/**
 * The owner, on the Console's Chart tab: «в консоли снова все полосочки серые».
 *
 * They were. lib/chart-theme.ts's seriesColor folds everything past the fifth
 * series into `other` — which IS `--chart-axis`, the grey the axis itself is
 * drawn in. That rule is right where it was written: the curated charts are
 * topk(5) and a sixth line there is genuinely an also-ran. The Console has no
 * topk: `up` matches 86 series, so 81 lines and 81 table swatches came out the
 * same axis grey, and the swatch stopped tying a row to a line.
 *
 * paletteColor is the Console's rule instead: the SAME five validated hues,
 * re-lit a lap at a time. Colour cannot carry 86 identities and this does not
 * pretend to — it carries the ten on the reader's page.
 */
describe("paletteColor", () => {
  const base = ["hsl(210, 68%, 59%)", "hsl(166, 57%, 42%)", "hsl(265, 64%, 66%)"];

  it("hands the first lap the validated ramp untouched", () => {
    expect(paletteColor(base, 0)).toBe(base[0]);
    expect(paletteColor(base, 2)).toBe(base[2]);
  });

  it("re-lights the ramp instead of folding the sixth series into the axis grey", () => {
    const sixth = paletteColor(base, base.length);

    expect(sixth).not.toBe(base[0]);
    // Same hue and saturation — a lap is a lighting change, not a new colour.
    expect(sixth).toMatch(/^hsl\(210, 68%, /);
  });

  it("keeps a whole page of series distinguishable — ten rows, ten colours", () => {
    const ten = Array.from({ length: LEGEND_MAX_SERIES }, (_, i) => paletteColor(base, i));

    expect(new Set(ten).size).toBe(LEGEND_MAX_SERIES);
  });

  it("stays inside readable lightness however many laps it runs", () => {
    for (let i = 0; i < 40; i++) {
      const l = Number(/(\d+(?:\.\d+)?)%\)$/.exec(paletteColor(base, i))?.[1]);
      expect(l).toBeGreaterThanOrEqual(24);
      expect(l).toBeLessThanOrEqual(84);
    }
  });

  it("repeats rather than inventing colour once the laps run out", () => {
    // Honest about its own limit: at some point two series DO share a colour,
    // and the raw table below the plot is what tells them apart.
    expect(paletteColor(base, 0)).toBe(paletteColor(base, base.length * 4));
  });

  it("leaves a colour it cannot read alone rather than emitting garbage", () => {
    // chartColors reads live CSS custom properties; a theme that hands it
    // something other than the documented hsl() triplet must not turn into
    // `hsl(NaN, NaN%, NaN%)` on the canvas.
    expect(paletteColor(["var(--brand)"], 1)).toBe("var(--brand)");
    expect(paletteColor([], 3)).toBe("");
  });
});
