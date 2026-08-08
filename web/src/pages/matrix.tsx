import { useMemo, useState } from "react";
import { Inbox, Search } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { RealtimeBadge } from "@/components/realtime-badge";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip } from "@/components/ui/tooltip";
import { useMatrix } from "@/hooks/use-matrix";
import { buildInvestigateURL } from "@/lib/investigation-sources";
import { useTimeContext } from "@/lib/timemachine";
import { PROTOCOLS, type MatrixCell, type Protocol } from "@/lib/types";
import { cn } from "@/lib/utils";

// NUL keeps the composite key unambiguous even if a node name contained the
// separator; node names are DNS labels today, but this stays robust regardless.
const pairKey = (source: string, destination: string) => `${source}\0${destination}`;

/* One measure, one mark: the cell's colour AND its primary figure both encode
   the failure ratio (healthy < 1%, degraded 1–10%, failing ≥ 10%); RTT p95 is
   the secondary, muted line. The fill is the -soft token so --foreground stays
   legible, and a saturated left rail repeats the state so the grid still reads
   in greyscale. */
type Tier = "ok" | "warn" | "bad" | "unknown";

function tierOf(cell: MatrixCell | undefined): Tier {
  if (!cell || cell.failRatio === null) return "unknown";
  if (cell.failRatio < 0.01) return "ok";
  if (cell.failRatio < 0.1) return "warn";
  return "bad";
}

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

const LEGEND: { tier: Tier; label: string }[] = [
  { tier: "ok", label: "Healthy · fail < 1%" },
  { tier: "warn", label: "Degraded · 1–10%" },
  { tier: "bad", label: "Failing · ≥ 10%" },
  { tier: "unknown", label: "No data" },
];

function fmtRtt(ns?: number): string {
  if (ns === undefined) return "—";
  return `${(ns / 1e6).toFixed(1)}ms`;
}

function fmtFail(ratio: number): string {
  return `${(100 * ratio).toFixed(1)}%`;
}

const HEADER_CELL =
  "sticky z-10 bg-surface p-2 text-left text-[11px] font-medium text-muted-foreground";

function NodeLabel({ name }: { name: string }) {
  return (
    <Tooltip content={name}>
      <span className="block max-w-[9rem] truncate">{name}</span>
    </Tooltip>
  );
}

function GridCell({
  src,
  dst,
  cell,
  protocol,
}: {
  src: string;
  dst: string;
  cell: MatrixCell | undefined;
  protocol: Protocol;
}) {
  if (src === dst) {
    return (
      <td aria-label={`${src}: self`} className="p-0.5">
        <div className="flex h-12 w-full min-w-16 items-center justify-center rounded-md bg-surface-2/40 text-muted-foreground/40">
          —
        </div>
      </td>
    );
  }
  const tier = tierOf(cell);
  const fail = cell?.failRatio ?? null;
  const label =
    fail === null
      ? `${src} → ${dst}: no data`
      : `${src} → ${dst}: fail ${fmtFail(fail)}, RTT p95 ${fmtRtt(cell?.rttP95)}`;

  const tooltip = (
    <div className="flex min-w-44 flex-col gap-1">
      <div className="flex items-center gap-1.5 font-medium">
        <span className="truncate">{src}</span>
        <span aria-hidden="true" className="text-muted-foreground">→</span>
        <span className="truncate">{dst}</span>
      </div>
      {fail === null ? (
        <div className="text-muted-foreground">No probe data in Prometheus for this pair.</div>
      ) : (
        <dl className="nums grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5 text-muted-foreground">
          <dt>Failure ratio</dt>
          <dd className="text-right text-popover-foreground">{fmtFail(fail)}</dd>
          <dt>RTT p95</dt>
          <dd className="text-right text-popover-foreground">{fmtRtt(cell?.rttP95)}</dd>
          {protocol === "udp" && cell?.lossRatio !== undefined ? (
            <>
              <dt>Packet loss</dt>
              <dd className="text-right text-popover-foreground">{fmtFail(cell.lossRatio)}</dd>
            </>
          ) : null}
        </dl>
      )}
    </div>
  );

  // Every non-self cell opens the pair's object card (task-25-brief.md), even
  // one with no data yet -- an operator may want the page open before probe
  // data exists. A real <a href> keeps native keyboard focus/activation
  // (Tab, Enter) instead of hand-rolling it on a div, and lets the existing
  // hover/focus Tooltip wrapper attach its handlers exactly as before.
  const pairHref = `/pairs/${encodeURIComponent(src)}/${encodeURIComponent(dst)}`;

  /* The Investigate affordance (plan Decision 11) lives INSIDE the cell, as a
     sibling of the pair link rather than a new column or a new row: the cell IS
     the pair's affordance, and a second grid layer would double the width of a
     table that is already N x N.
     It cannot go in the tooltip, which is the other thing this cell has: that
     bubble is `pointer-events-none` by construction (components/ui/tooltip.tsx
     renders it in a body portal at a measured position), so a link inside it
     could be read but never clicked.
     Nested anchors are invalid HTML, hence the sibling + `absolute` rather than
     an <a> within the <a>. It stays in the tab order and is only VISUALLY
     revealed on hover/focus-within, so the grid does not sprout N x N icons. */
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
          {fail === null ? (
            <span className="text-xs text-muted-foreground">—</span>
          ) : (
            <>
              <span className="nums text-[13px] font-semibold leading-tight">{fmtFail(fail)}</span>
              <span className="nums text-[10.5px] leading-tight text-muted-foreground">
                {fmtRtt(cell?.rttP95)}
              </span>
            </>
          )}
        </a>
      </Tooltip>
      <a
        href={investigateHref}
        aria-label={`Investigate ${src} → ${dst}`}
        className={cn(
          "absolute right-1 top-1 rounded p-0.5 text-muted-foreground opacity-0",
          "group-hover:opacity-100 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          "hover:bg-accent hover:text-accent-foreground",
        )}
      >
        <Search aria-hidden="true" className="size-3" />
      </a>
    </td>
  );
}

function MatrixSkeleton() {
  return (
    <div role="status" aria-live="polite" className="flex flex-col gap-2">
      <span className="sr-only">Loading matrix…</span>
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
  const [protocol, setProtocol] = useState<Protocol>("tcp");
  const { at } = useTimeContext();
  const { data, isLoading, error, live } = useMatrix(protocol);

  const byPair = useMemo(() => {
    const m = new Map<string, MatrixCell>();
    for (const c of data?.cells ?? []) m.set(pairKey(c.source, c.destination), c);
    return m;
  }, [data]);

  return (
    <PageShell
      title="Matrix"
      description={
        at
          ? `N×N node connectivity as of ${at.toLocaleString()}, evaluated straight from Prometheus at that instant.`
          : "Live N×N node connectivity, recomputed from Prometheus every 15s."
      }
      actions={
        <>
          <Segmented
            aria-label="Protocol"
            options={PROTOCOLS.map((p) => ({ value: p, label: p.toUpperCase() }))}
            value={protocol}
            onChange={setProtocol}
          />
          <Badge variant="neutral">plane: pod</Badge>
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
          <p className="text-sm font-medium">Matrix is unavailable</p>
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
              {at ? "No probe data in Prometheus at this time" : "No probe data in Prometheus yet"}
            </p>
            <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
              {at
                ? `Nothing was scraped for ${protocol.toUpperCase()} probes at that instant — it may predate the deployment, or fall outside Prometheus' own retention.`
                : `The ${protocol.toUpperCase()} matrix fills in once the agents complete a probe round and Prometheus scrapes them — usually within a minute of the DaemonSet becoming ready.`}
            </p>
          </div>
        ) : null}

        {data && data.nodes.length > 0 ? (
          <div className="flex flex-col gap-5">
            <div className="overflow-x-auto">
              <table className="border-separate border-spacing-0">
                <caption className="sr-only">
                  Node-to-node failure ratio matrix, {protocol.toUpperCase()}
                </caption>
                <thead>
                  <tr>
                    <th className={cn(HEADER_CELL, "left-0 top-0 z-20")} scope="col">
                      src \ dst
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
                        <GridCell
                          key={dst}
                          src={src}
                          dst={dst}
                          cell={byPair.get(pairKey(src, dst))}
                          protocol={protocol}
                        />
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <div className="flex flex-wrap items-center gap-x-5 gap-y-2 border-t border-border pt-4 text-xs text-muted-foreground">
              {LEGEND.map(({ tier, label }) => (
                <span key={tier} className="flex items-center gap-1.5">
                  <span
                    aria-hidden="true"
                    className={cn("size-2.5 rounded-full", TIER_DOT[tier])}
                  />
                  {label}
                </span>
              ))}
              <span className="ml-auto hidden sm:block">
                colour = failure ratio · cell shows fail % and p95 RTT
              </span>
            </div>
          </div>
        ) : null}
      </Card>
    </PageShell>
  );
}
