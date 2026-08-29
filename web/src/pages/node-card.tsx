import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AnnotationBar, useAnnotations } from "@/components/annotations";
import { InvestigateLink, RelatedIncidents } from "@/components/investigate-entry";
import { MaintenanceBar, useMaintenance } from "@/components/maintenance";
import { PageShell } from "@/components/page-shell";
import { RecentChanges } from "@/components/recent-changes";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Pager, usePager } from "@/components/ui/pager";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TBody, Td, Th, THead, Tr } from "@/components/ui/table";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { getRun, getRuns } from "@/lib/api";
import type { InvestigationScope } from "@/lib/investigation-sources";
import { localeTag, stampFull, useLocale, useT, type Locale } from "@/lib/i18n";
import { cardsDict, pluralKey, type CardsKey } from "@/lib/i18n/dict/cards";
import { DEGRADED_AT, FAILING_AT, isMeasured, severityRatio } from "@/lib/matrix-cells";
/* The ?protocol= reader and writer live on pages/matrix.tsx — one URL key, one
   spelling, imported the way target-card.tsx imports fmtIntervalNs. */
import { degradedProtocolParam, readProtocolFromLocation, writeProtocol } from "@/pages/matrix";
import { withAtParam, useTimeContext } from "@/lib/timemachine";
import { PROTOCOLS, type MatrixCell, type Protocol, type RunDetail } from "@/lib/types";
import { cn, runsAtOrBefore } from "@/lib/utils";

const NODE_PATH_PREFIX = "/nodes/";

/** nodeNameFromPath mirrors run-detail.tsx's runIdFromPath. */
export function nodeNameFromPath(pathname: string): string {
  if (!pathname.startsWith(NODE_PATH_PREFIX)) return "";
  const rest = pathname.slice(NODE_PATH_PREFIX.length);
  if (rest === "") return "";
  try {
    return decodeURIComponent(rest);
  } catch {
    return rest;
  }
}

type Tier = "ok" | "warn" | "bad" | "unknown";

const TIER_VARIANT: Record<Tier, NonNullable<BadgeProps["variant"]>> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "unknown",
};
/* The tier badge is this console's VERDICT on a ratio, not a field any API
   returns, so it is presentation and it translates — with the same four words
   the pair and target cards use (lib/i18n/dict/cards.ts fixes the table). */
const TIER_KEYS: Record<Tier, CardsKey> = {
  ok: "tier.ok",
  warn: "tier.warn",
  bad: "tier.bad",
  unknown: "tier.unknown",
};

/**
 * nodeHealth derives the header's health% and status tier from the worst OUTBOUND severity this
 * node reports in the matrix; self-cells are excluded (a node never reports a pair against itself).
 *
 * `scored` and `total` are the figure's own COVERAGE, and they exist because a
 * bare "100.0% healthy" computed from one scored pair out of nine is a claim
 * about the node that only one ninth of the evidence supports (QA scope 2,
 * finding #3). `total` counts every outbound pair the matrix carries a cell
 * for, `scored` the ones that actually produced a severity ratio; the header
 * states the gap wherever it exists, the way the Overview's worstPairs.scoredGap
 * already does for the fleet.
 */
export function nodeHealth(
  ready: boolean | undefined,
  cells: MatrixCell[],
  nodeName: string,
): { percent: number | null; tier: Tier; scored: number; total: number } {
  const pairs = cells.filter((c) => c.source === nodeName && c.destination !== nodeName);
  const outbound = pairs.filter(isMeasured);
  const ratios = outbound.map(severityRatio).filter((r): r is number => r !== null);
  const coverage = { scored: ratios.length, total: pairs.length };
  const worst = ratios.length > 0 ? Math.max(...ratios) : null;
  const percent = worst === null ? null : Math.max(0, 100 * (1 - worst));
  if (ready === false) return { percent, tier: "bad", ...coverage };
  if (outbound.length === 0) return { percent: null, tier: "unknown", ...coverage };
  if (worst === null) return { percent: null, tier: "ok", ...coverage };
  const tier: Tier = worst >= FAILING_AT ? "bad" : worst >= DEGRADED_AT ? "warn" : "ok";
  return { percent, tier, ...coverage };
}

function fmtFail(ratio: number): string {
  return `${(100 * ratio).toFixed(1)}%`;
}

function fmtRtt(ns?: number): string {
  return ns === undefined ? "—" : `${(ns / 1e6).toFixed(1)}ms`;
}

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function fmtTime(ts: string | undefined, locale: Locale): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : stampFull(d, locale);
}

type NodeTab = "overview" | "diagnostics";

const TABS: { value: NodeTab; labelKey: CardsKey }[] = [
  { value: "overview", labelKey: "tab.overview" },
  { value: "diagnostics", labelKey: "tab.diagnostics" },
];

/**
 * BreakdownTable is the per-destination table under the header, and it has to
 * RECONCILE with the header above it.
 *
 * On UDP and ICMP the header's verdict comes from packet loss (severityRatio is
 * worst-of), while this table had no loss column at all and printed an em-dash
 * in the fail column for every row — a table that agreed with nothing and
 * explained less (QA scope 2, finding #5). The loss column appears whenever the
 * cells CARRY loss, which is the same rule the matrix tooltip applies rather
 * than a second protocol switch: the vector that can decide the tier is the
 * vector that gets a column.
 *
 * The em-dash itself moved too (#4). The matrix reserves it for a pair NOTHING
 * measured and says "no fail data" for a lazy failure counter; two different
 * facts, and this table used one glyph for both.
 */
function BreakdownTable({ nodeName, cells }: { nodeName: string; cells: MatrixCell[] }) {
  const t = useT(cardsDict);
  const outbound = cells.filter((c) => c.source === nodeName && c.destination !== nodeName);
  const showLoss = outbound.some((c) => c.lossRatio !== undefined);
  /* One row per peer: on a big cluster that is every other node. */
  const pager = usePager(outbound, { resetKey: nodeName });
  if (outbound.length === 0) {
    return <p className="px-4 py-10 text-center text-xs text-muted-foreground">{t("node.breakdown.empty")}</p>;
  }
  return (
    <>
    <Table variant="dense">
      <caption className="sr-only">{t("node.breakdown.caption", { name: nodeName })}</caption>
      <THead>
        <Tr>
          <Th className="pl-4 pr-4">{t("node.breakdown.destination")}</Th>
          <Th numeric className="pr-4">
            {t("node.breakdown.failRatio")}
          </Th>
          {showLoss ? (
            <Th numeric className="pr-4">
              {t("node.breakdown.loss")}
            </Th>
          ) : null}
          {/* RTT p95 is a metric's name and reads the same in both. */}
          <Th numeric className="pr-4">
            {t("node.breakdown.rtt")}
          </Th>
        </Tr>
      </THead>
      <TBody>
        {pager.visible.map((c) => (
          <Tr key={c.destination}>
            {/* The destination was the other dead end on this card: the row
                named a pair and led nowhere (QA scope 2, finding #14). */}
            <Td className="max-w-[16rem] pl-4 pr-4">
              <a
                href={withAtParam(`/pairs/${encodeURIComponent(nodeName)}/${encodeURIComponent(c.destination)}`)}
                title={c.destination}
                className="mono-data block truncate rounded text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                {c.destination}
              </a>
            </Td>
            {/* Values read in the foreground (M4-2); only trouble is tinted. */}
            <Td
              numeric
              className={cn("pr-4", c.failRatio !== null && c.failRatio >= DEGRADED_AT && "text-health-bad")}
            >
              {c.failRatio !== null
                ? fmtFail(c.failRatio)
                : isMeasured(c)
                  ? t("cell.noFailData")
                  : t("cell.noData")}
            </Td>
            {showLoss ? (
              <Td
                numeric
                className={cn("pr-4", c.lossRatio !== undefined && c.lossRatio >= DEGRADED_AT && "text-health-bad")}
              >
                {c.lossRatio === undefined ? "—" : fmtFail(c.lossRatio)}
              </Td>
            ) : null}
            <Td numeric className="pr-4">
              {fmtRtt(c.rttP95)}
            </Td>
          </Tr>
        ))}
      </TBody>
    </Table>
    <Pager pager={pager} subject={t("node.breakdown.subject")} />
    </>
  );
}

/* Why this one field cannot fall back to the agent the way Zone does: readiness is a KUBERNETES NODE condition. */

function OverviewTab({
  nodeName,
  cells,
  zone,
  ready,
  agentId,
  podIP,
  topologyProblem,
}: {
  nodeName: string;
  cells: MatrixCell[];
  zone?: string;
  ready?: boolean;
  agentId?: string;
  podIP?: string;
  /** The topology query's own failure detail, when it FAILED. Absent for a
   *  successful read — including a successful read that simply does not know
   *  this node. */
  topologyProblem?: string;
}) {
  const t = useT(cardsDict);
  return (
    <div className="flex flex-col gap-5">
      <Card className="p-5">
        <h3 className="type-section">{t("node.identity")}</h3>
        {/* Four em-dashes are the answer to "the topology knows nothing about
            this node". They are NOT the answer to "the topology request
            failed" — that reads as a node that exists and has no identity,
            when in fact nobody was asked and the server said why (QA round 2,
            finding #3). One honest line, carrying the problem verbatim. */}
        {topologyProblem !== undefined ? (
          <p data-testid="identity-problem" className="mt-3 text-xs leading-relaxed text-muted-foreground">
            {topologyProblem}
          </p>
        ) : (
          /* The four LABELS are ours; the four values — a zone name, an agent
             id, a pod IP — are the fleet's own bytes. */
          <dl className="mt-3 grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-xs text-muted-foreground">{t("node.identity.zone")}</dt>
              <dd className="mt-0.5">{zone ?? "—"}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">{t("node.identity.agentId")}</dt>
              {/* Machine identifiers wear the data face (M4-1). */}
              <dd className="mono-data mt-0.5 truncate" title={agentId}>
                {agentId ?? "—"}
              </dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">{t("node.identity.podIP")}</dt>
              {/* `?? "—"` never fired for the historical shape, which carries
                  podIP as "" rather than as an absent field — so the cell went
                  blank instead of saying it had no answer (QA scope 2, #6). An
                  explicit empty check, because "" IS the absence here. */}
              <dd className="mono-data mt-0.5">{podIP === undefined || podIP === "" ? "—" : podIP}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">{t("node.identity.ready")}</dt>
              <dd className="mt-0.5" title={ready === undefined ? t("node.identity.readyNote") : undefined}>
                {ready === undefined ? "—" : ready ? t("node.identity.yes") : t("node.identity.no")}
              </dd>
            </div>
          </dl>
        )}
      </Card>

      <Card asChild className="overflow-hidden p-0">
        <section>
          <div className="border-b border-border px-4 py-3">
            <h3 className="type-section">{t("node.breakdown")}</h3>
          </div>
          <BreakdownTable nodeName={nodeName} cells={cells} />
        </section>
      </Card>
    </div>
  );
}

// RUN_SCAN_LIMIT bounds the client-side scan below (see useNodeDiagnostics'
// own doc comment) -- kept small because each candidate costs one extra GET
// /api/v1/runs/{id}.
const RUN_SCAN_LIMIT = 20;

/** runsTouchingNode filters already-fetched run details down to the ones
 * with at least one pair result touching `nodeName`, either side. */
export function runsTouchingNode(details: RunDetail[], nodeName: string): RunDetail[] {
  return details.filter((d) => d.results.some((r) => r.sourceNode === nodeName || r.destinationNode === nodeName));
}

/**
 * useNodeDiagnostics is the Diagnostics tab's data source; GET /api/v1/runs (RunQuery in
 * lib/api.ts) has no source/destination filter.
 */
function useNodeDiagnostics(nodeName: string) {
  const { at } = useTimeContext();
  const runsQuery = useQuery({ queryKey: ["runs", "recent-scan"], queryFn: () => getRuns({ limit: RUN_SCAN_LIMIT }) });
  /* Cut to the viewed instant BEFORE fetching details: a run that started after `t` has no place in a view of `t`. */
  const ids = useMemo(
    () => runsAtOrBefore(runsQuery.data?.runs ?? [], at).map((r) => r.id),
    [runsQuery.data, at],
  );
  const detailsQuery = useQuery({
    queryKey: ["runs", "recent-scan", "details", ids.join(",")],
    queryFn: () => Promise.all(ids.map((id) => getRun(id))),
    enabled: ids.length > 0,
  });
  const runs = useMemo(
    () => (detailsQuery.data ? runsTouchingNode(detailsQuery.data, nodeName) : []),
    [detailsQuery.data, nodeName],
  );
  return {
    runs,
    isLoading: runsQuery.isLoading || (ids.length > 0 && detailsQuery.isLoading),
    error: runsQuery.error ?? detailsQuery.error,
  };
}

const STATUS_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  pending: "neutral",
  running: "neutral",
  succeeded: "ok",
  failed: "bad",
  partial: "warn",
};

function DiagnosticsTab({ nodeName }: { nodeName: string }) {
  const t = useT(cardsDict);
  const { locale } = useLocale();
  const { runs, isLoading, error } = useNodeDiagnostics(nodeName);
  const { at } = useTimeContext();
  const runsPager = usePager(runs, { resetKey: nodeName });

  return (
    <Card asChild className="overflow-hidden p-0">
      <section>
        <div className="border-b border-border px-4 py-3">
          <h3 className="type-section">{t("node.runs.heading")}</h3>
          {/* The engaged half of the same limitation — the endpoint has no time
              filter either, so the page it returns is the newest page NOW and
              the cut to `t` happens here — is a CLAUSE of the same sentence
              rather than a second string appended to it: Russian puts it in a
              different place, and only one key can decide that. */}
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {t("node.runs.scanNote", {
              limit: RUN_SCAN_LIMIT,
              engaged: at ? t("node.runs.scanNote.engaged") : "",
            })}
          </p>
        </div>

        {error ? (
          <p role="alert" className="px-4 py-4 text-sm text-health-bad">
            {t("node.runs.unavailable")}.
          </p>
        ) : null}

        {isLoading && runs.length === 0 && !error ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">{t("node.runs.scanning")}</span>
            {Array.from({ length: 3 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : null}

        {!isLoading && !error && runs.length === 0 ? (
          <p className="px-4 py-10 text-center text-xs text-muted-foreground">
            {t("node.runs.empty", { limit: RUN_SCAN_LIMIT })}
          </p>
        ) : null}

        {runs.length > 0 ? (
          <>
          <ul className="divide-y divide-border">
            {runsPager.visible.map((r) => {
              const touching = r.results.filter((res) => res.sourceNode === nodeName || res.destinationNode === nodeName);
              return (
                <li key={r.id} className="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
                  {/* A run id is a machine identifier — the data face (M4-1). */}
                  <a href={withAtParam(`/diagnostics/runs/${r.id}`)} className="mono-data font-medium text-primary hover:underline">
                    {r.id}
                  </a>
                  {/* status and type are the run's OWN stored values, in a
                      technical list beside the run's id — they stay. */}
                  <Badge variant={STATUS_VARIANT[r.status] ?? "unknown"} dot>
                    {r.status}
                  </Badge>
                  <span className="text-xs uppercase tracking-wide text-muted-foreground">{r.type}</span>
                  <span className="nums ml-auto text-xs text-muted-foreground">
                    {t("node.runs.pairs", {
                      count: touching.length,
                      word: t(pluralKey(touching.length, "count.pairs.one", "count.pairs.few", "count.pairs.many", locale)),
                    })}
                  </span>
                  <span className="text-xs text-muted-foreground">{fmtTime(r.createdAt, locale)}</span>
                </li>
              );
            })}
          </ul>
          <Pager pager={runsPager} subject={t("node.runs.subject")} />
          </>
        ) : null}
      </section>
    </Card>
  );
}

/*
 * NODE_ANNOTATION_RANGE_SECONDS is a day, not an hour, and it is a choice this card has to make
 * alone: unlike Explore.
 */
const NODE_ANNOTATION_RANGE_SECONDS = 24 * 60 * 60;

/**
 * NodeAnnotations is this card's annotation surface; it is a LIST rather than a chart overlay for
 * the plain reason that this page draws no chart.
 *
 * The declared MAINTENANCE windows sit under it, over the same 24 hours and the
 * same scope. The pair and target cards have carried that bar since M6 and this
 * one never got it, so a node under a declared change looked exactly like a node
 * that was simply broken (QA scope 2, finding #21). Same component, node scope —
 * the bar hides itself without maintenance:read, so nothing is added for a
 * reader who cannot see windows anyway.
 */
function NodeAnnotations({ nodeName }: { nodeName: string }) {
  const t = useT(cardsDict);
  const { annotations, error, refresh } = useAnnotations(nodeName, NODE_ANNOTATION_RANGE_SECONDS);
  const {
    windows,
    error: maintenanceError,
    refresh: refreshMaintenance,
  } = useMaintenance(nodeName, NODE_ANNOTATION_RANGE_SECONDS);
  return (
    <Card asChild className="p-5">
      <section>
        <h3 className="type-section">{t("node.annotations")}</h3>
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t("node.annotations.blurb")}</p>
        <AnnotationBar scope={nodeName} annotations={annotations} error={error} onChanged={() => void refresh()} />
        <MaintenanceBar
          scope={nodeName}
          windows={windows}
          error={maintenanceError}
          onChanged={() => void refreshMaintenance()}
        />
      </section>
    </Card>
  );
}

function NotFound({ nodeName }: { nodeName: string }) {
  const t = useT(cardsDict);
  return (
    <PageShell
      timeMachine
      title={t("node.title")}
      description={nodeName ? t("node.notFound.withName", { name: nodeName }) : t("node.notFound.bare")}
    >
      <Card role="status" className="px-6 py-10 text-center text-sm text-muted-foreground">
        {t("node.notFound.body")}
      </Card>
    </PageShell>
  );
}

export function NodeCardPage() {
  const t = useT(cardsDict);
  const { locale } = useLocale();
  const nodeName = nodeNameFromPath(window.location.pathname);
  const { at } = useTimeContext();
  const topo = useTopology();
  /* The switch is a VIEW of this card, and a view worth sharing — read from
     and written back to ?protocol=, through pages/matrix.tsx's own reader and
     writer so the two surfaces cannot spell the key differently (QA scope 2,
     finding #18). */
  const [protocol, setProtocolState] = useState<Protocol>(() => readProtocolFromLocation(window.location.search));
  const setProtocol = (p: Protocol) => {
    setProtocolState(p);
    writeProtocol(p);
  };
  useEffect(() => {
    const fixed = degradedProtocolParam(window.location.search);
    if (fixed) writeProtocol(fixed);
  }, []);
  const matrix = useMatrix(protocol);
  const [tab, setTab] = useState<NodeTab>("overview");

  if (nodeName === "") return <NotFound nodeName={nodeName} />;

  const node = topo.data?.nodes.find((n) => n.name === nodeName);
  const agent = topo.data?.agents.find((a) => a.nodeName === nodeName);
  /*
   * Zone falls back to the AGENT's own copy; the agent carries the zone it registered with, so the
   * field has an answer.
   */
  /* `||`, not `??`: the topology API sends an UNLABELLED node's zone as an empty STRING, not as an
     absent field, so `??` never reached the agent fallback and the identity card rendered the "Zone"
     label with nothing under it -- indistinguishable from a cell still loading, where its three
     siblings print an em dash. */
  const zone = node?.zone || agent?.zone || undefined;
  const cells = matrix.data?.cells ?? [];
  const health = nodeHealth(node?.ready, cells, nodeName);
  const loadingIdentity = (topo.isLoading && !topo.data) || (matrix.isLoading && !matrix.data);
  const investigationScope: InvestigationScope = { kind: "node", a: nodeName, b: "" };

  return (
    <PageShell
      timeMachine
      title={nodeName}
      /*
       * The card's whole body — identity from useTopology, health from useMatrix, both already
       * resolved through the Time Machine.
       */
      description={
        at
          ? t("node.stateAsOf", {
              zone: zone ? `${t("node.zone", { zone })} · ` : "",
              /* Inside a translated sentence, so the stamp takes that
                 sentence's language — lib/i18n's localeTag. */
              at: new Date(topo.data?.asOf ?? at).toLocaleString(localeTag(locale)),
            })
          : zone
            ? t("node.zone", { zone })
            : t("node.title")
      }
      actions={
        <>
          {/* The protocol OPTIONS are the protocols' own names. */}
          <Segmented
            aria-label={t("protocol.aria")}
            options={PROTOCOLS.map((p) => ({ value: p, label: p.toUpperCase() }))}
            value={protocol}
            onChange={setProtocol}
          />
          {/* No percentage, no sentence. "— healthy" read as a claim with a
              missing number in front of it; the badge beside it already says
              the state in words, and it is the honest one (QA round 2, #16).
              And where the figure exists but rests on part of the evidence, it
              carries its own denominator rather than being withheld: "100.0%
              healthy" off one scored pair of nine was a true statement about
              one ninth of this node, presented as a statement about the node
              (QA scope 2, #3). Withholding it would throw away the only
              measurement there is; disclosing the coverage keeps both. */}
          {health.percent === null ? null : (
            <span data-testid="node-health-percent" className="nums text-sm text-muted-foreground">
              {health.scored < health.total
                ? t("health.percent.scoped", {
                    percent: health.percent.toFixed(1),
                    scored: health.scored,
                    total: health.total,
                  })
                : t("health.percent", { percent: health.percent.toFixed(1) })}
            </span>
          )}
          <Badge variant={TIER_VARIANT[health.tier]} dot>
            {t(TIER_KEYS[health.tier])}
          </Badge>
          {/* The entry point into Investigation Mode (plan Decision 11): the
              URL is the whole contract, built by the one helper the matrix and
              the other two cards also use. */}
          <InvestigateLink scope={investigationScope} />
        </>
      }
    >
      {/* The HEADLINE is ours; the message under it is the server's, verbatim. */}
      {topo.error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">{t("node.topologyUnavailable")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{topo.error.message}</p>
        </Card>
      ) : null}

      {matrix.error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">{t("node.matrixUnavailable")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{matrix.error.message}</p>
        </Card>
      ) : null}

      {loadingIdentity ? (
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">{t("node.loading")}</span>
          <Skeleton className="h-4 w-48" />
          <div className="mt-4 flex flex-col gap-2">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
          <div className="flex flex-col gap-5">
            <Segmented
              aria-label={t("tab.aria")}
              options={TABS.map((tb) => ({ value: tb.value, label: t(tb.labelKey) }))}
              value={tab}
              onChange={setTab}
            />
            {tab === "overview" ? (
              <OverviewTab
                nodeName={nodeName}
                cells={cells}
                zone={zone}
                ready={node?.ready}
                agentId={agent?.id}
                podIP={agent?.podIP}
                topologyProblem={topo.error?.message}
              />
            ) : (
              <DiagnosticsTab nodeName={nodeName} />
            )}
          </div>
          <div className="flex flex-col gap-5">
            <RelatedIncidents scope={investigationScope} />
            {/* scopeNode, not scope: an equality filter on the bare node name
                never saw the pair-scoped rows ("node-a→node-b") that every
                check run and path change writes, so the rail looked idle on a
                busy node (QA scope 2 #21). */}
            <RecentChanges scopeNode={nodeName} />
            <NodeAnnotations nodeName={nodeName} />
          </div>
        </div>
      )}
    </PageShell>
  );
}
