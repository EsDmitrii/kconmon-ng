import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties } from "react";
import { Inbox, Maximize, Minus, Plus, Search } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { RealtimeBadge } from "@/components/realtime-badge";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip } from "@/components/ui/tooltip";
import { useMatrix } from "@/hooks/use-matrix";
import { localeTag, useLocale, useT } from "@/lib/i18n";
import { matrixDict, type MatrixKey } from "@/lib/i18n/dict/matrix";
/* cellSummary's own table — dict/matrix.ts's NOT-HERE list says why the shared
   reading of a cell is not this page's to fork. */
import { matrixCellsDict } from "@/lib/i18n/dict/matrix-cells";
import { buildInvestigateURL } from "@/lib/investigation-sources";
import {
  cellSummary,
  cellTier,
  fmtRatio,
  fmtRtt,
  isMeasured,
  type CellTier,
} from "@/lib/matrix-cells";
import {
  MAX_ZOOM,
  MIN_ZOOM,
  cellDensity,
  elideForHeaders,
  fitScale,
  heightBudget,
  gridMetrics,
  gridWidth,
  zoomStep,
  type CellDensity,
} from "@/lib/matrix-zoom";
import { withAtParam, useTimeContext } from "@/lib/timemachine";
import { PROTOCOLS, type MatrixCell, type Protocol } from "@/lib/types";
import { cn } from "@/lib/utils";

// NUL keeps the composite key unambiguous even if a node name contained the
// separator; node names are DNS labels today, but this stays robust regardless.
const pairKey = (source: string, destination: string) => `${source}\0${destination}`;

/* ── what the page will accept as a matrix ────────────────────────────────────
 *
 * The grid used to render whatever arrived. Two things made that untenable, and
 * both are reachable without a hostile server:
 *
 *   1. SHAPE. `GET /api/v1/matrix` is normalized in lib/api.ts (a Go nil slice
 *      marshals to `null`, so `nodes` and `cells` are defaulted to `[]` there),
 *      but the WEBSOCKET frame is written into the same cache entry by
 *      hooks/use-matrix.ts WITHOUT passing through that normalizer. One pushed
 *      `{"nodes":null}` and `data.nodes.length` threw during render — a blank
 *      page with no way back but a reload. The page therefore reads its own
 *      payload defensively rather than trusting the transport it came over.
 *
 *   2. FIGURES. A number field is only a MEASUREMENT when it holds a finite,
 *      non-negative number. `1e999` is valid JSON and JSON.parse turns it into
 *      Infinity ("Infinity%" in a cell); a string where a number belongs makes
 *      fmtRatio/fmtRtt print "NaN%" / "NaNms"; and a bare `null` in `rttP95` is
 *      worse than either, because lib/matrix-cells.ts counts the field as
 *      PRESENT and 0/1e6 formats as a confident "0.0ms" — a fabricated
 *      measurement, in a cell the tier then paints green. None of the three is a
 *      figure the console can stand behind, so none of them reaches the grid.
 *
 * Dropping the field (rather than printing it) puts the cell back on the one
 * honest reading lib/matrix-cells.ts already has for a fact nobody measured.
 */

/** measured is the gate every numeric field passes: a finite, non-negative
 *  number, or nothing at all. Negative is rejected with the rest — neither a
 *  ratio nor a round-trip time can run backwards, so a negative one is a
 *  broken field, not a small one. */
function measured(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : undefined;
}

/**
 * measuredCells is the cell list the grid and the topology map both draw from.
 * A cell that names no pair is dropped entirely (it can address nothing), and a
 * `null` in the array is skipped rather than dereferenced — that one was a blank
 * page too.
 *
 * `failRatio` keeps its `null`, which is not a missing figure but a MEANING: the
 * fail-ratio series is lazy, and silence from it is the normal state of a
 * healthy pair (see GridCellImpl).
 */
export function measuredCells(cells: unknown): MatrixCell[] {
  if (!Array.isArray(cells)) return [];
  const out: MatrixCell[] = [];
  for (const c of cells) {
    if (!c || typeof c !== "object") continue;
    const { source, destination, failRatio, rttP95, lossRatio } = c as Record<string, unknown>;
    if (typeof source !== "string" || typeof destination !== "string") continue;
    out.push({
      source,
      destination,
      failRatio: measured(failRatio) ?? null,
      rttP95: measured(rttP95),
      lossRatio: measured(lossRatio),
    });
  }
  return out;
}

/**
 * gridNodes is the axis the grid draws, deduplicated. A repeated name is not a
 * second node: it drew two identical columns, two identical rows, and a React
 * key collision for each of them, which is a warning today and a mis-keyed
 * update tomorrow. An unnamed node is dropped — it addresses no card.
 */
export function gridNodes(nodes: unknown): string[] {
  if (!Array.isArray(nodes)) return [];
  const seen = new Set<string>();
  const out: string[] = [];
  for (const n of nodes) {
    if (typeof n !== "string" || n === "" || seen.has(n)) continue;
    seen.add(n);
    out.push(n);
  }
  return out;
}

/* The reading itself lives in lib/matrix-cells.ts, shared with Overview, the object cards and the topology edges. */
type Tier = CellTier;

/* Healthy stays quiet (a neutral surface + green rail) so an all-green grid
   doesn't shout; only degraded/failing cells get a coloured fill. Colour is
   spent on trouble, which is what the operator scans for. */
const TIER_FILL: Record<Tier, string> = {
  ok: "bg-surface-2/60",
  warn: "bg-health-warn-soft",
  bad: "bg-health-bad-soft",
  unknown: "bg-health-unknown-soft",
};

const TIER_RAIL: Record<Tier, string> = {
  ok: "before:bg-health-ok",
  warn: "before:bg-health-warn",
  bad: "before:bg-health-bad",
  unknown: "before:bg-transparent",
};

/* Tailwind only sees literal class names, so the legend dots use an explicit
   map rather than interpolation. */
const TIER_DOT: Record<Tier, string> = {
  ok: "bg-health-ok",
  warn: "bg-health-warn",
  bad: "bg-health-bad",
  unknown: "bg-health-unknown",
};

/* Tier → its legend row, in the order the legend renders them. The WORDS are
   dict/matrix.ts's; this is the reading order, which is not a translation. */
const LEGEND: { tier: Tier; key: MatrixKey }[] = [
  { tier: "ok", key: "legend.ok" },
  { tier: "warn", key: "legend.warn" },
  { tier: "bad", key: "legend.bad" },
  { tier: "unknown", key: "legend.unknown" },
];

/*
 * PROTOCOL_PARAM is this page's own URL key, carried the way lib/timemachine's `?at=` is carried;
 * TanStack Router owns navigation here but no route declares a search schema (timemachine.tsx
 * documents that decision).
 */
const PROTOCOL_PARAM = "protocol";

/** readProtocolFromLocation resolves ?protocol= into one of the three the
 *  console probes. Anything else — a typo, a stale link, a protocol this build
 *  does not know — degrades to tcp rather than rendering an empty grid for a
 *  protocol nothing will ever answer for. */
export function readProtocolFromLocation(search: string): Protocol {
  const raw = new URLSearchParams(search).get(PROTOCOL_PARAM);
  return PROTOCOLS.includes(raw as Protocol) ? (raw as Protocol) : "tcp";
}

/**
 * degradedProtocolParam answers "is the URL still claiming something this page
 * is not showing?" — a `?protocol=sctp` that silently became TCP left the lie
 * in the address bar, which is the string an operator copies and shares (QA
 * scope 2, finding #17). Null when the URL and the view already agree, which
 * includes the ordinary no-param case: the default needs no spelling out.
 */
export function degradedProtocolParam(search: string): Protocol | null {
  const raw = new URLSearchParams(search).get(PROTOCOL_PARAM);
  if (raw === null) return null;
  const resolved = readProtocolFromLocation(search);
  return raw === resolved ? null : resolved;
}

/** writeProtocol is the ONE writer of ?protocol=, shared with the object cards
 *  so a second surface cannot invent a second spelling of the same key. */
export function writeProtocol(p: Protocol): void {
  const url = new URL(window.location.href);
  url.searchParams.set(PROTOCOL_PARAM, p);
  window.history.replaceState({}, "", `${url.pathname}${url.search}${url.hash}`);
}

/* Lifted from ui/pager.tsx's prev/next control — the console's one idiom for a
   small square icon button, so the zoom trio is not a fourth invention. */
const ZOOM_BUTTON_CLASS =
  "grid size-7 place-items-center rounded-md text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-30";

/* p-0.5, not p-2: a header's padding is part of what its COLUMN costs, and
   lib/matrix-zoom.ts predicts that width exactly (CELL_GAP) so "fit" can be
   arithmetic rather than a guess. The label inside carries its own inset. */
const HEADER_CELL =
  "sticky z-10 bg-surface p-0.5 text-left font-medium text-muted-foreground text-[length:var(--m-font-label)]";

/**
 * NodeLabel is a row/column header, and headers were the grid's dead end: every
 * CELL opened its pair card while the two names framing it opened nothing (QA
 * scope 2, finding #14). The link is the whole label, so the target keeps its
 * hit area, and it lands on the same /nodes/{name} route the topology map's own
 * boxes navigate to.
 *
 * The width is the COLUMN's, not the name's. A `<th>` sized by whatever string
 * it held is what pushed a ten-node grid off the right of the page; the name
 * truncates inside a fixed box now and stays whole in the tooltip and the
 * aria-label, which is where it was always read from anyway.
 */
function NodeLabel({ name, width, elide = "" }: { name: string; width: "column" | "label"; elide?: string }) {
  const t = useT(matrixDict);
  /* The DISTINGUISHING part of the name, when every node shares a prefix. A column is about as wide
     as thirteen characters, and a real fleet's names agree for longer than that — every header on
     the live console read "kconmon-pro…", so one of the grid's two axes carried no information at
     all. The shared prefix is dropped here and named once above the grid; the whole name stays in
     the tooltip and in the accessible name, which is where it was read from anyway. */
  const shown = elide && name.startsWith(elide) && name.length > elide.length ? `…${name.slice(elide.length)}` : name;
  return (
    <Tooltip content={name}>
      <a
        href={withAtParam(`/nodes/${encodeURIComponent(name)}`)}
        aria-label={t("header.node", { node: name })}
        className={cn(
          "block truncate rounded px-1 hover:text-foreground hover:underline",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          width === "column" ? "max-w-[var(--m-col-w)]" : "max-w-[var(--m-label-w)]",
        )}
      >
        {shown}
      </a>
    </Tooltip>
  );
}

/**
 * sharedNamePrefix is the prefix every node name begins with, cut back to a separator.
 *
 * Only worth eliding when it actually buys width and leaves something behind: at least four
 * characters shared, and at least two left on the shortest name. Cut at "-" or "." so the remainder
 * reads as a name rather than as a slice through the middle of a word.
 */
export function sharedNamePrefix(names: readonly string[]): string {
  if (names.length < 2) return "";
  let prefix = names[0];
  for (const name of names.slice(1)) {
    let i = 0;
    while (i < prefix.length && i < name.length && prefix[i] === name[i]) i++;
    prefix = prefix.slice(0, i);
    if (prefix === "") return "";
  }
  const cut = Math.max(prefix.lastIndexOf("-"), prefix.lastIndexOf("."));
  if (cut < 3) return "";
  prefix = prefix.slice(0, cut + 1);
  const shortest = Math.min(...names.map((n) => n.length));
  return shortest - prefix.length >= 2 ? prefix : "";
}

function GridCellImpl({
  src,
  dst,
  cell,
  density,
}: {
  src: string;
  dst: string;
  cell: MatrixCell | undefined;
  /** What this size can honestly SHOW — lib/matrix-zoom.ts's cellDensity. */
  density: CellDensity;
}) {
  const t = useT(matrixDict);
  const tc = useT(matrixCellsDict);
  /** The instant the grid is drawn at, so a drill-down opens the same moment (null = Live). */
  const { at: viewedAt } = useTimeContext();
  if (src === dst) {
    return (
      <td aria-label={t("cell.self", { node: src })} className="p-0.5">
        <div className="flex h-[var(--m-cell-h)] w-[var(--m-col-w)] items-center justify-center rounded-md bg-surface-2/40 text-muted-foreground/40">
          {density === "tile" ? null : "—"}
        </div>
      </td>
    );
  }
  /* MEASURED, not "has a failure ratio". The fail-ratio series is lazy — a
     pair that has never failed emits no sample at all — so on a healthy fleet
     `fail === null` is the normal state of a cell that is full of latency
     data. Reading it as absence blanked the whole grid (QA round 2, #1). */
  const tier = cellTier(cell);
  const measured = isMeasured(cell);
  const fail = cell?.failRatio ?? null;
  const label = `${src} → ${dst}: ${cellSummary(cell, tc)}`;

  const tooltip = (
    <div className="flex min-w-44 flex-col gap-1">
      <div className="flex items-center gap-1.5 font-medium">
        <span className="truncate">{src}</span>
        <span aria-hidden="true" className="text-muted-foreground">→</span>
        <span className="truncate">{dst}</span>
      </div>
      {!measured ? (
        <div className="text-muted-foreground">{t("tooltip.unmeasured")}</div>
      ) : (
        <dl className="nums grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5 text-muted-foreground">
          <dt>{t("tooltip.failRatio")}</dt>
          {/* "no samples" rather than a dash or a fabricated 0%: the series
              exists and reported nothing, which is a different fact from a
              measured zero and from an unprobed pair. */}
          <dd className="text-right text-popover-foreground">
            {fail === null ? t("tooltip.noSamples") : fmtRatio(fail)}
          </dd>
          {cell?.rttP95 !== undefined ? (
            <>
              <dt>{t("tooltip.rtt")}</dt>
              <dd className="text-right text-popover-foreground">{fmtRtt(cell.rttP95)}</dd>
            </>
          ) : null}
          {/* Loss shows whenever the cell carries it. Gating this on
              `protocol === "udp"` hid a vector the fold genuinely answers for
              other protocols, and it is the one that can carry the tier when
              the failure ratio cannot. */}
          {cell?.lossRatio !== undefined ? (
            <>
              <dt>{t("tooltip.loss")}</dt>
              <dd className="text-right text-popover-foreground">{fmtRatio(cell.lossRatio)}</dd>
            </>
          ) : null}
        </dl>
      )}
    </div>
  );

  /* Every non-self cell opens the pair's object card, even one with no data yet — AT THE INSTANT
     the reader is viewing. These are raw anchors (a full document load), so without the param the
     pair card came up Live, drew live data and dropped the banner, silently ending an investigation
     that was mid-sentence. */
  const pairHref = withAtParam(`/pairs/${encodeURIComponent(src)}/${encodeURIComponent(dst)}`);

  /*
   * The Investigate affordance lives INSIDE the cell, as a sibling of the pair link rather than a
   * new column or a new row.
   */
  /* And the investigation window ends at the viewed instant, not at now: an Investigate link built
     while the Time Machine is engaged must open the window around what is on screen. */
  const investigateHref = buildInvestigateURL({ kind: "pair", a: src, b: dst }, viewedAt ?? new Date());

  return (
    <td className="group relative p-0.5">
      <Tooltip content={tooltip}>
        <a
          href={pairHref}
          aria-label={label}
          className={cn(
            "relative flex h-[var(--m-cell-h)] w-[var(--m-col-w)] flex-col items-center justify-center overflow-hidden rounded-md",
            "before:absolute before:inset-y-0 before:left-0 before:w-[3px]",
            "transition-[transform,box-shadow] duration-(--dur-fast) ease-(--ease)",
            "hover:-translate-y-px hover:shadow-raised hover:ring-1 hover:ring-border-strong",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            TIER_FILL[tier],
            TIER_RAIL[tier],
          )}
        >
          {/* Below a certain size a cell is a coloured TILE and nothing else:
              "50.0%" does not go into 38px at any font size worth reading, and
              printing it there would be a claim the reader cannot check. Every
              figure stays one hover away in the tooltip, and the aria-label
              never changed at all. It is also what bounds the DOM on a big
              fleet — a tile is the link, full stop. */}
          {density === "tile" ? null : !measured ? (
            /* The em-dash is reserved for a cell nothing measured. A cell with
               a p95 and no failure series shows its p95 as the hero figure —
               throwing away the one number it has and drawing a dash over it
               was the whole of finding #1. */
            <span className="text-[length:var(--m-font-sub)] text-muted-foreground">—</span>
          ) : fail === null ? (
            <>
              <span className="nums text-[length:var(--m-font-hero)] font-semibold leading-tight">
                {fmtRtt(cell?.rttP95)}
              </span>
              {/* px-1 text-center is the give this line needs and the hero
                  figure does not: it is the only WORDS in the grid, so it is
                  the only thing a longer language can push into the tier rail
                  (`before:w-[3px]`, hard against the left edge) or spill out of
                  `overflow-hidden`. Padded and centred, a string too wide for
                  the column wraps to a second centred line INSIDE the cell
                  instead of being painted over. Dropped entirely once the box
                  is too short for two lines — see cellDensity. */}
              {density === "full" ? (
                <span className="nums px-1 text-center text-[length:var(--m-font-sub)] leading-tight text-muted-foreground">
                  {cell?.lossRatio === undefined
                    ? t("cell.noFailData")
                    : t("cell.loss", { ratio: fmtRatio(cell.lossRatio) })}
                </span>
              ) : null}
            </>
          ) : (
            <>
              <span className="nums text-[length:var(--m-font-hero)] font-semibold leading-tight">
                {fmtRatio(fail)}
              </span>
              {density === "full" ? (
                <span className="nums text-[length:var(--m-font-sub)] leading-tight text-muted-foreground">
                  {fmtRtt(cell?.rttP95)}
                </span>
              ) : null}
            </>
          )}
        </a>
      </Tooltip>
      {/* No investigate glyph on a tile: a 12px icon in a 38px box is a target
          nothing but luck hits, and the affordance returns the moment the cell
          can hold it. */}
      {density === "tile" ? null : (
      <a
        href={investigateHref}
        data-testid="cell-investigate"
        aria-label={t("cell.investigate", { src, dst })}
        className={cn(
          "absolute right-1 top-1 rounded p-0.5 text-muted-foreground opacity-0",
          "group-hover:opacity-100 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          "hover:bg-accent hover:text-accent-foreground",
          /* The DRAWN control is a 12px glyph in a 16px box, which is a target
             a trackpad hits by luck and a touch screen does not hit at all (QA
             scope 3, finding #19). The pseudo-element takes it to 40×40 without
             moving a pixel of what is painted: -inset-3 is 12px on each side of
             the 16px box. It is deliberately a pseudo rather than padding —
             padding would push the glyph off the cell's top-right corner, and
             the corner is where the affordance is learned. */
          "after:absolute after:-inset-3 after:content-['']",
        )}
      >
        <Search aria-hidden="true" className="size-3" />
      </a>
      )}
    </td>
  );
}

/**
 * Memoised, and this is the one thing that makes zooming a 2 500-cell grid
 * affordable: the geometry travels as CSS custom properties on the table, so a
 * step that stays inside one density band changes no cell's props at all and
 * React re-renders none of them. Only a band CHANGE costs a full pass.
 */
const GridCell = memo(GridCellImpl);

function MatrixSkeleton() {
  const t = useT(matrixDict);
  return (
    <div role="status" aria-live="polite" className="flex flex-col gap-2">
      <span className="sr-only">{t("loading")}</span>
      <div className="flex gap-2">
        {Array.from({ length: 7 }, (_, i) => (
          <Skeleton key={i} className={cn("h-4", i === 0 ? "w-28" : "w-16")} />
        ))}
      </div>
      {Array.from({ length: 6 }, (_, r) => (
        <div key={r} className="flex items-center gap-2">
          <Skeleton className="h-4 w-28" />
          {Array.from({ length: 6 }, (_, c) => (
            <Skeleton key={c} className="h-12 w-16" />
          ))}
        </div>
      ))}
    </div>
  );
}

export function MatrixPage() {
  const t = useT(matrixDict);
  const { locale } = useLocale();
  /* Read ON MOUNT, so a shared /matrix?protocol=icmp link opens on ICMP instead of silently on TCP. */
  const [protocol, setProtocolState] = useState<Protocol>(() => readProtocolFromLocation(window.location.search));
  const setProtocol = (p: Protocol) => {
    setProtocolState(p);
    writeProtocol(p);
  };
  /* replaceState, not push: the degraded URL was never a place to go back to. */
  useEffect(() => {
    const fixed = degradedProtocolParam(window.location.search);
    if (fixed) writeProtocol(fixed);
  }, []);
  const { at } = useTimeContext();
  const { data, isPending, error, live } = useMatrix(protocol);

  /* Everything below reads `nodes` and `byPair`, never data.nodes/data.cells:
     the payload is accepted through the two gates above exactly once. */
  const nodes = useMemo(() => gridNodes(data?.nodes), [data]);
  const namePrefix = useMemo(() => sharedNamePrefix(nodes), [nodes]);
  const byPair = useMemo(() => {
    const m = new Map<string, MatrixCell>();
    for (const c of measuredCells(data?.cells)) m.set(pairKey(c.source, c.destination), c);
    return m;
  }, [data]);

  /* ── the zoom ─────────────────────────────────────────────────────────────
     `manual` is what the READER asked for and null means "whatever fits", so a
     window resize keeps a fitted grid fitted while a chosen scale is left where
     it was put. `available` is the viewport's own width, measured — never
     assumed, and 0 until it has been, which fitScale reads as "do not guess". */
  const nodeCount = nodes.length;
  const viewportRef = useRef<HTMLDivElement>(null);
  const [available, setAvailable] = useState(0);
  // The viewport's HEIGHT as well: fitting only the width left the last rows below the fold.
  const [availableHeight, setAvailableHeight] = useState(0);
  const [manual, setManual] = useState<number | null>(null);
  const scale = manual ?? fitScale(nodeCount, available, availableHeight);
  const density = cellDensity(scale);
  /* The wheel handler is native and long-lived; it reads the scale from here
     rather than being torn down and rebuilt on every step. */
  const scaleRef = useRef(scale);
  scaleRef.current = scale;

  /* The elision is decided PER AXIS and PER SCALE: the two boxes are different widths and carry
     different type sizes, so a name can fit the row labels and not the column headers. Zooming in
     therefore gives the names back instead of leaving the axis reading "…01". */
  const columnElide = useMemo(
    () => elideForHeaders(nodes, namePrefix, gridMetrics(scale).columnWidth, gridMetrics(scale).fontLabel),
    [nodes, namePrefix, scale],
  );
  const labelElide = useMemo(
    () => elideForHeaders(nodes, namePrefix, gridMetrics(scale).labelWidth, gridMetrics(scale).fontLabel),
    [nodes, namePrefix, scale],
  );

  const vars = useMemo(
    () => ({ ...gridMetrics(scale).vars, "--m-grid-w": `${gridWidth(nodeCount, scale)}px` }) as CSSProperties,
    [scale, nodeCount],
  );

  const gridDrawn = Boolean(data && nodeCount > 0);
  useLayoutEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const measure = () => {
      setAvailable(el.clientWidth);
      /* The height BUDGET, not the height the content happens to have made.
         The viewport is `max-h-[...] min-h-64`, so clientHeight is what the grid already drew,
         bounded below by the min — and feeding that back into fitScale is circular: a fresh render
         measures the 256px min, decides a seven-node grid does not fit, drops to 50%, and the
         smaller grid keeps the box at 256px forever. Every fleet opened at half size on a screen
         with room to spare. The resolved max-height is the space actually available; clientHeight
         only wins when it is larger (no max, or a taller box). */
      setAvailableHeight(heightBudget(el.clientHeight, Number.parseFloat(getComputedStyle(el).maxHeight)));
    };
    measure();
    /* jsdom has no ResizeObserver, and neither does an old browser; the one
       measurement above still stands, it just stops tracking. */
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [gridDrawn]);

  /* Native and non-passive, because React registers its own wheel listener as
     passive and preventDefault there does nothing — the page would zoom instead
     of the grid. A PLAIN wheel is left alone: that is how a grid bigger than its
     box is panned, and taking it would strand a floor-fitted fleet. */
  /**
   * step moves ONE stop from whatever the pending scale already is.
   *
   * The updater form is not a style choice. A trackpad emits a wheel event per
   * few pixels, so a single flick arrives as a burst inside one React batch —
   * and a burst that each read `scaleRef.current` all read the SAME pre-batch
   * scale and collapsed into one stop. Five events, one step: the grid felt
   * stuck under exactly the gesture that is meant to drive it. `prev` is the
   * accumulated value, so five events are five stops; `null` still means "the
   * reader has not chosen a scale", which is the fitted one.
   */
  const step = useCallback(
    (direction: 1 | -1) => setManual((prev) => zoomStep(prev ?? scaleRef.current, direction)),
    [],
  );

  useEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (!e.ctrlKey && !e.metaKey) return;
      e.preventDefault();
      step(e.deltaY < 0 ? 1 : -1);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [gridDrawn, step]);

  return (
    <PageShell
      timeMachine
      title={t("title")}
      description={
        /* The stamp lands INSIDE a translated sentence, so it takes that
           sentence's language — lib/i18n's localeTag, not the bare default. */
        at ? t("description.engaged", { at: at.toLocaleString(localeTag(locale)) }) : t("description.live")
      }
      actions={
        <>
          <Segmented
            aria-label={t("protocol.aria")}
            options={PROTOCOLS.map((p) => ({ value: p, label: p.toUpperCase() }))}
            value={protocol}
            onChange={setProtocol}
          />
          <Badge variant="neutral">{t("plane")}</Badge>
          {/* How fresh the grid actually is — pushed, or up to 15s of polling
              behind. Both states carry a label, never colour alone. Engaged the
              question is moot (the grid is pinned to an instant on purpose) and
              a "delayed" badge would read as a fault, so it is not shown. */}
          {at ? null : <RealtimeBadge realtime={live} />}
        </>
      }
    >
      {error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">{t("error.title")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{error.message}</p>
        </Card>
      ) : null}

      <Card className="p-6">
        {/* isPending: a paused retry is pending-but-not-fetching, and drawing nothing at all left
            the card as a heading with no skeleton, no error and no empty note. */}
        {isPending && !data ? <MatrixSkeleton /> : null}

        {data && nodeCount === 0 ? (
          <div className="flex flex-col items-center gap-3 px-6 py-14 text-center">
            <span
              aria-hidden="true"
              className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
            >
              <Inbox className="size-5" />
            </span>
            <p className="text-sm font-medium">
              {t(at ? "empty.engaged.title" : "empty.live.title")}
            </p>
            <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
              {t(at ? "empty.engaged.body" : "empty.live.body", { protocol: protocol.toUpperCase() })}
            </p>
          </div>
        ) : null}

        {data && nodeCount > 0 ? (
          <div className="flex flex-col gap-4">
            {/* The same three controls the topology map offers, in the same
                order and with the same words: out, in, fit. One vocabulary for
                "this picture is bigger than its box" across the console. */}
            <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
              <p className="text-xs text-muted-foreground">{t("zoom.hint")}</p>
              <div role="group" aria-label={t("zoom.aria")} className="flex items-center gap-1">
                <button
                  type="button"
                  aria-label={t("zoom.out")}
                  title={t("zoom.out")}
                  disabled={scale <= MIN_ZOOM}
                  onClick={() => step(-1)}
                  className={ZOOM_BUTTON_CLASS}
                >
                  <Minus aria-hidden="true" className="size-4" />
                </button>
                {/* What the reader is looking at, as a number. A grid that
                    silently shrank itself owes them that much. */}
                <span
                  data-testid="matrix-zoom-level"
                  aria-live="polite"
                  className="nums min-w-11 px-1 text-center text-[11px] text-muted-foreground"
                >
                  {t("zoom.level", { pct: Math.round(scale * 100) })}
                </span>
                <button
                  type="button"
                  aria-label={t("zoom.in")}
                  title={t("zoom.in")}
                  disabled={scale >= MAX_ZOOM}
                  onClick={() => step(1)}
                  className={ZOOM_BUTTON_CLASS}
                >
                  <Plus aria-hidden="true" className="size-4" />
                </button>
                <button
                  type="button"
                  aria-label={t("zoom.fit")}
                  title={t("zoom.fit")}
                  onClick={() => setManual(null)}
                  className={ZOOM_BUTTON_CLASS}
                >
                  <Maximize aria-hidden="true" className="size-4" />
                </button>
              </div>
            </div>

            {/* Only while an axis IS eliding: at a scale where both boxes hold the whole name the
                note described something the grid was no longer doing. */}
            {columnElide || labelElide ? (
              <p className="px-1 pb-1 text-xs text-muted-foreground">
                {t("grid.prefix", { prefix: columnElide || labelElide })}
              </p>
            ) : null}

            {/* overflow-auto on BOTH axes and a bounded height: that is what
                makes the sticky headers below stick to something, and what the
                grid pans inside once it is at the floor and still too wide. */}
            <div
              ref={viewportRef}
              data-testid="matrix-viewport"
              style={vars}
              className="max-h-[calc(100dvh-26rem)] min-h-64 overflow-auto rounded-md"
            >
              {/* table-fixed with an explicit width: the browser stops measuring
                  2 500 cells to decide a column, and the width is the same
                  number lib/matrix-zoom.ts fitted against. */}
              <table className="w-[var(--m-grid-w)] table-fixed border-separate border-spacing-0">
                <caption className="sr-only">
                  {t("grid.caption", { protocol: protocol.toUpperCase() })}
                </caption>
                <thead>
                  <tr>
                    <th className={cn(HEADER_CELL, "left-0 top-0 z-20 w-[var(--m-label-w)]")} scope="col">
                      {t("grid.corner")}
                    </th>
                    {nodes.map((n) => (
                      <th key={n} className={cn(HEADER_CELL, "top-0 w-[var(--m-col-w)]")} scope="col">
                        <NodeLabel name={n} width="column" elide={columnElide} />
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {nodes.map((src) => (
                    <tr key={src}>
                      <th className={cn(HEADER_CELL, "left-0 w-[var(--m-label-w)] font-normal")} scope="row">
                        {/* The ROW axis carries the same prefix as the column axis, and at a narrow
                            viewport the label column is 88px — every row read "kconmon-prod-…".
                            Same elision, same note above the grid. */}
                        <NodeLabel name={src} width="label" elide={labelElide} />
                      </th>
                      {nodes.map((dst) => (
                        <GridCell
                          key={dst}
                          src={src}
                          dst={dst}
                          cell={byPair.get(pairKey(src, dst))}
                          density={density}
                        />
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <div className="flex flex-col gap-2 border-t border-border pt-4 text-xs text-muted-foreground">
              <div className="flex flex-wrap items-center gap-x-5 gap-y-2">
                {LEGEND.map(({ tier, key }) => (
                  <span key={tier} className="flex items-center gap-1.5">
                    <span
                      aria-hidden="true"
                      className={cn("size-2.5 rounded-full", TIER_DOT[tier])}
                    />
                    {t(key)}
                  </span>
                ))}
              </div>
              {/* Says what the colour actually reads now: the worst ratio the
                  cell carries, which on a pair with no failure samples is its
                  packet loss. On its OWN line, flush left under the tiers it
                  explains: `ml-auto` shoved it against the right edge, so a
                  sentence about the legend started a screen away from it. */}
              <p data-testid="matrix-legend-note" className="max-w-prose leading-relaxed">
                {t("legend.note")}
              </p>
            </div>
          </div>
        ) : null}
      </Card>
    </PageShell>
  );
}
