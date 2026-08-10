import { useEffect, useMemo, useState } from "react";
import { Inbox, Search } from "lucide-react";
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
import { useTimeContext } from "@/lib/timemachine";
import { PROTOCOLS, type MatrixCell, type Protocol } from "@/lib/types";
import { cn } from "@/lib/utils";

// NUL keeps the composite key unambiguous even if a node name contained the
// separator; node names are DNS labels today, but this stays robust regardless.
const pairKey = (source: string, destination: string) => `${source}\0${destination}`;

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

const HEADER_CELL =
  "sticky z-10 bg-surface p-2 text-left text-[11px] font-medium text-muted-foreground";

/**
 * NodeLabel is a row/column header, and headers were the grid's dead end: every
 * CELL opened its pair card while the two names framing it opened nothing (QA
 * scope 2, finding #14). The link is the whole label, so the target keeps its
 * hit area, and it lands on the same /nodes/{name} route the topology map's own
 * boxes navigate to.
 */
function NodeLabel({ name }: { name: string }) {
  const t = useT(matrixDict);
  return (
    <Tooltip content={name}>
      <a
        href={`/nodes/${encodeURIComponent(name)}`}
        aria-label={t("header.node", { node: name })}
        className="block max-w-[9rem] truncate rounded hover:text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {name}
      </a>
    </Tooltip>
  );
}

function GridCell({
  src,
  dst,
  cell,
}: {
  src: string;
  dst: string;
  cell: MatrixCell | undefined;
}) {
  const t = useT(matrixDict);
  const tc = useT(matrixCellsDict);
  if (src === dst) {
    return (
      <td aria-label={t("cell.self", { node: src })} className="p-0.5">
        <div className="flex h-12 w-full min-w-16 items-center justify-center rounded-md bg-surface-2/40 text-muted-foreground/40">
          —
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

  // Every non-self cell opens the pair's object card, even one with no data yet.
  const pairHref = `/pairs/${encodeURIComponent(src)}/${encodeURIComponent(dst)}`;

  /*
   * The Investigate affordance lives INSIDE the cell, as a sibling of the pair link rather than a
   * new column or a new row.
   */
  const investigateHref = buildInvestigateURL({ kind: "pair", a: src, b: dst }, new Date());

  return (
    <td className="group relative p-0.5">
      <Tooltip content={tooltip}>
        <a
          href={pairHref}
          aria-label={label}
          className={cn(
            "relative flex h-12 w-full min-w-16 flex-col items-center justify-center overflow-hidden rounded-md",
            "before:absolute before:inset-y-0 before:left-0 before:w-[3px]",
            "transition-[transform,box-shadow] duration-(--dur-fast) ease-(--ease)",
            "hover:-translate-y-px hover:shadow-raised hover:ring-1 hover:ring-border-strong",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            TIER_FILL[tier],
            TIER_RAIL[tier],
          )}
        >
          {/* The em-dash is reserved for a cell nothing measured. A cell with
              a p95 and no failure series shows its p95 as the hero figure —
              throwing away the one number it has and drawing a dash over it
              was the whole of finding #1. */}
          {!measured ? (
            <span className="text-xs text-muted-foreground">—</span>
          ) : fail === null ? (
            <>
              <span className="nums text-[13px] font-semibold leading-tight">{fmtRtt(cell?.rttP95)}</span>
              {/* px-1 text-center is the give this line needs and the hero
                  figure does not: it is the only WORDS in the grid, so it is
                  the only thing a longer language can push into the tier rail
                  (`before:w-[3px]`, hard against the left edge) or spill out of
                  `overflow-hidden`. Padded and centred, a string too wide for
                  the column wraps to a second centred line INSIDE the cell
                  instead of being painted over — two 13.125px lines still clear
                  the h-12 box under the 16.25px hero. */}
              <span className="nums px-1 text-center text-[10.5px] leading-tight text-muted-foreground">
                {cell?.lossRatio === undefined
                  ? t("cell.noFailData")
                  : t("cell.loss", { ratio: fmtRatio(cell.lossRatio) })}
              </span>
            </>
          ) : (
            <>
              <span className="nums text-[13px] font-semibold leading-tight">{fmtRatio(fail)}</span>
              <span className="nums text-[10.5px] leading-tight text-muted-foreground">
                {fmtRtt(cell?.rttP95)}
              </span>
            </>
          )}
        </a>
      </Tooltip>
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
    </td>
  );
}

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
  const { data, isLoading, error, live } = useMatrix(protocol);

  const byPair = useMemo(() => {
    const m = new Map<string, MatrixCell>();
    for (const c of data?.cells ?? []) m.set(pairKey(c.source, c.destination), c);
    return m;
  }, [data]);

  return (
    <PageShell
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
        {isLoading && !data ? <MatrixSkeleton /> : null}

        {data && data.nodes.length === 0 ? (
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

        {data && data.nodes.length > 0 ? (
          <div className="flex flex-col gap-5">
            <div className="overflow-x-auto">
              <table className="border-separate border-spacing-0">
                <caption className="sr-only">
                  {t("grid.caption", { protocol: protocol.toUpperCase() })}
                </caption>
                <thead>
                  <tr>
                    <th className={cn(HEADER_CELL, "left-0 top-0 z-20")} scope="col">
                      {t("grid.corner")}
                    </th>
                    {data.nodes.map((n) => (
                      <th key={n} className={cn(HEADER_CELL, "top-0")} scope="col">
                        <NodeLabel name={n} />
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {data.nodes.map((src) => (
                    <tr key={src}>
                      <th className={cn(HEADER_CELL, "left-0 font-normal")} scope="row">
                        <NodeLabel name={src} />
                      </th>
                      {data.nodes.map((dst) => (
                        <GridCell key={dst} src={src} dst={dst} cell={byPair.get(pairKey(src, dst))} />
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <div className="flex flex-wrap items-center gap-x-5 gap-y-2 border-t border-border pt-4 text-xs text-muted-foreground">
              {LEGEND.map(({ tier, key }) => (
                <span key={tier} className="flex items-center gap-1.5">
                  <span
                    aria-hidden="true"
                    className={cn("size-2.5 rounded-full", TIER_DOT[tier])}
                  />
                  {t(key)}
                </span>
              ))}
              {/* Says what the colour actually reads now: the worst ratio the
                  cell carries, which on a pair with no failure samples is its
                  packet loss. */}
              <span className="ml-auto hidden sm:block">{t("legend.note")}</span>
            </div>
          </div>
        ) : null}
      </Card>
    </PageShell>
  );
}
