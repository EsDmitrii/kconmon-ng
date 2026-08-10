import { useState } from "react";
import { SearchX } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { RealtimeBadge } from "@/components/realtime-badge";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { isTerminalRunStatus, type RunPairRow, useRun } from "@/hooks/use-run";
import { ApiError, cancelRun } from "@/lib/api";
import { localeTag, stampFull, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { countForm, runDetailDict, type RunDetailKey } from "@/lib/i18n/dict/run-detail";
import {
  aggregateSamples,
  groupSamplesByPair,
  formatDurationNs,
  isIntervalRun,
  pairProgress,
  runDurationNs,
  sampleIntervalNs,
  type PairProgress,
  type PairSamples,
  type SampleAggregate,
} from "@/lib/run-samples";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import { cn } from "@/lib/utils";

const RUN_PATH_PREFIX = "/diagnostics/runs/";

/**
 * runIdFromPath reads the id straight off window.location.pathname rather than through TanStack
 * Router's own param matching.
 */
export function runIdFromPath(pathname: string): string {
  return pathname.startsWith(RUN_PATH_PREFIX) ? pathname.slice(RUN_PATH_PREFIX.length) : "";
}

/**
 * decodeRunId is what a run id looks like to a HUMAN; the encoded form is what runIdFromPath keeps
 * and what lib/api's getRun re-encodes for the wire.
 */
export function decodeRunId(raw: string): string {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

/** okPairs is the "Pairs 0/2 ok" numerator. */
export function okPairs(pairs: RunPairRow[]): number {
  return pairs.filter((p) => p.success === true || p.state === "succeeded").length;
}

const STATUS_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  pending: "neutral",
  running: "neutral",
  succeeded: "ok",
  failed: "bad",
  partial: "warn",
  // Cancelled is an operator's own decision, not a fault: neutral, so it
  // never reads as a failure someone has to investigate.
  cancelled: "neutral",
};

const PAIR_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  dispatched: "neutral",
  succeeded: "ok",
  failed: "bad",
  timeout: "bad",
};

function StatusBadge({ status }: { status: string }) {
  return (
    <Badge variant={STATUS_VARIANT[status] ?? "unknown"} dot>
      {status}
    </Badge>
  );
}

function fmtDuration(ns?: number): string {
  return ns === undefined ? "—" : `${(ns / 1e6).toFixed(0)}ms`;
}

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function fmtTime(ts: string | undefined, locale: Locale): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : stampFull(d, locale);
}

/** fmtNsCompact renders a nanosecond duration the way the aggregate row wants
 *  it: sub-millisecond latencies keep a decimal so a 0.4ms hop does not read
 *  as "0ms", everything else is whole milliseconds. */
export function fmtNsCompact(ns?: number): string {
  if (ns === undefined) return "—";
  const ms = ns / 1e6;
  return ms < 10 ? `${ms.toFixed(1)}ms` : `${ms.toFixed(0)}ms`;
}

/** fmtPercent renders a 0..1 ratio. One decimal, because a 400-sample run can
 *  legitimately sit at 0.2% and rounding that to "0%" would erase the only
 *  failure the operator started the run to catch. */
export function fmtPercent(ratio: number): string {
  return `${(ratio * 100).toFixed(1)}%`;
}

/* TIMELINE_PAIR_LIMIT bounds how many pairs get a tick strip; a 400-pair interval run holds up to 200 000 samples. */
const TIMELINE_PAIR_LIMIT = 12;

/**
 * SampleTimeline is the per-probe view an interval run exists to produce: one row per pair; it is
 * deliberately a strip of ticks rather than a latency chart.
 *
 * Every EXPECTED slot is drawn from the first render, arrived or not, so the track has a start, an
 * end and a visibly empty tail instead of a bar that only grows.
 */
function SampleTimeline({
  groups,
  durationNs,
  /** Non-terminal: only a run with something still to arrive gets a countdown. */
  running,
}: {
  groups: PairSamples[];
  durationNs: number;
  running: boolean;
}) {
  const t = useT(runDetailDict);
  const { locale } = useLocale();
  const shown = groups.slice(0, TIMELINE_PAIR_LIMIT);
  const hidden = groups.length - shown.length;
  return (
    <div className="flex flex-col gap-3 px-4 py-4">
      {shown.map((g) => {
        const agg = aggregateSamples(g.samples);
        const progress = pairProgress(durationNs, g.samples.length);
        // No frame theater around a single dot: a one-probe cadence keeps the strip it always had.
        const slotCount = progress.framed ? progress.expected : g.samples.length;
        const counting = running && progress.remaining > 0;
        return (
          <div key={`${g.source}\0${g.destination}`} className="flex flex-col gap-1">
            <div className="flex items-baseline justify-between gap-3 text-xs">
              <span className="flex min-w-0 items-center gap-1.5">
                <span className="truncate" title={g.source}>
                  {g.source}
                </span>
                <span aria-hidden="true" className="shrink-0 text-muted-foreground">
                  →
                </span>
                <span className="truncate" title={g.destination}>
                  {g.destination}
                </span>
              </span>
              <span className="nums shrink-0 text-muted-foreground">
                {t("timeline.rowStats", { sent: agg.sent, failed: agg.failed, p95: fmtNsCompact(agg.p95Ns) })}
              </span>
            </div>
            <div
              className="flex flex-wrap gap-[2px]"
              role="img"
              aria-label={trackLabel(t, locale, g, agg, progress, counting)}
            >
              {Array.from({ length: slotCount }, (_, slot) => {
                const s = g.samples[slot];
                /* Keyed by SLOT, not sampleSeq: an arrival must mutate that node rather than
                   delete-and-insert it, or the strip twitches under a reader watching it fill. */
                if (s === undefined) {
                  return (
                    <span
                      key={slot}
                      data-testid="timeline-slot-pending"
                      /* Outlined, never coloured: an undispatched probe has no outcome, and drawing
                         a cancelled run's tail as failures would fabricate results. */
                      className="h-4 w-[6px] shrink-0 rounded-[1px] bg-surface-2 ring-1 ring-inset ring-border-strong"
                    />
                  );
                }
                return (
                  <span
                    key={slot}
                    data-testid={progress.framed ? "timeline-slot-filled" : undefined}
                    /* A FAILED tick shows the error and no duration: the elapsed
                       time on a probe that never came back is dispatch overhead,
                       and printing it as a latency is exactly the masquerade the
                       card above promises does not happen. s.error is the
                       AGENT's own sentence and goes in verbatim. */
                    title={
                      s.success
                        ? t("timeline.tick", { seq: s.sampleSeq, duration: fmtNsCompact(s.durationNs) })
                        : t("timeline.tickFailed", {
                            seq: s.sampleSeq,
                            outcome: s.error ?? t("timeline.tick.failed"),
                          })
                    }
                    className={cn(
                      "h-4 w-[6px] shrink-0 rounded-[1px]",
                      s.success ? "bg-health-ok" : "bg-health-bad",
                    )}
                  />
                );
              })}
            </div>
            {progress.framed ? (
              <p data-testid="timeline-progress" className="nums text-[11px] text-muted-foreground">
                {counting
                  ? t("timeline.progress.running", {
                      arrived: progress.arrived,
                      expected: progress.expected,
                      remaining: formatDurationNs(progress.remainingNs, locale),
                    })
                  : t("timeline.progress.settled", { arrived: progress.arrived, expected: progress.expected })}
              </p>
            ) : null}
          </div>
        );
      })}
      {hidden > 0 ? (
        <p className="text-xs text-muted-foreground">
          {t(`timeline.hidden.${countForm(locale, hidden)}` as RunDetailKey, { count: hidden })}
        </p>
      ) : null}
    </div>
  );
}

/** Three sentences, not one: an unframed strip has no expected count, and "9 more probes still to
 *  come" is true of a running run and false of a cancelled one. */
function trackLabel(
  t: Translate<RunDetailKey>,
  locale: string,
  g: PairSamples,
  agg: SampleAggregate,
  progress: PairProgress,
  counting: boolean,
): string {
  if (!progress.framed) {
    return t("timeline.rowLabel", {
      source: g.source,
      destination: g.destination,
      sent: agg.sent,
      failed: agg.failed,
    });
  }
  if (counting) {
    return t(`timeline.trackLabel.pending.${countForm(locale, progress.remaining)}` as RunDetailKey, {
      source: g.source,
      destination: g.destination,
      arrived: progress.arrived,
      expected: progress.expected,
      count: progress.remaining,
    });
  }
  return t("timeline.trackLabel.settled", {
    source: g.source,
    destination: g.destination,
    arrived: progress.arrived,
    expected: progress.expected,
    failed: agg.failed,
  });
}

/**
 * IntervalSummary is the interval run's headline card: what was asked for; it renders for a RUNNING
 * run too, and the numbers are the run so far rather than a projection.
 */
function IntervalSummary({
  durationNs,
  agg,
  pairCount,
}: {
  durationNs: number;
  agg: SampleAggregate;
  pairCount: number;
}) {
  const t = useT(runDetailDict);
  const { locale } = useLocale();
  return (
    <Card className="p-6">
      <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4 lg:grid-cols-7">
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.duration")}</dt>
          <dd className="nums mt-0.5">{formatDurationNs(durationNs, locale)}</dd>
        </div>
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.cadence")}</dt>
          <dd className="nums mt-0.5">
            {formatDurationNs(sampleIntervalNs(durationNs), locale)}
            <span className="text-muted-foreground"> × {pairCount}</span>
          </dd>
        </div>
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.sent")}</dt>
          <dd className="nums mt-0.5">{agg.sent}</dd>
        </div>
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.failed")}</dt>
          <dd className={cn("nums mt-0.5", agg.failed > 0 ? "text-health-bad" : undefined)}>
            {agg.failed} ({fmtPercent(agg.failRatio)})
          </dd>
        </div>
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.min")}</dt>
          <dd className="nums mt-0.5">{fmtNsCompact(agg.minNs)}</dd>
        </div>
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.avg")}</dt>
          <dd className="nums mt-0.5">{fmtNsCompact(agg.avgNs)}</dd>
        </div>
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.p95max")}</dt>
          <dd className="nums mt-0.5">
            {fmtNsCompact(agg.p95Ns)} / {fmtNsCompact(agg.maxNs)}
          </dd>
        </div>
      </dl>
      <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("summary.note")}</p>
    </Card>
  );
}

function PairTable({ pairs }: { pairs: RunPairRow[] }) {
  const t = useT(runDetailDict);
  if (pairs.length === 0) {
    return <div className="px-6 py-10 text-center text-sm text-muted-foreground">{t("pairs.empty")}</div>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <caption className="sr-only">{t("pairs.caption")}</caption>
        <thead>
          <tr className="border-b border-border text-left text-[11px] uppercase tracking-[0.07em] text-muted-foreground">
            <th scope="col" className="py-3 pl-4 pr-4 font-semibold">
              {t("pairs.col.pair")}
            </th>
            <th scope="col" className="py-3 pr-4 font-semibold">
              {t("pairs.col.state")}
            </th>
            <th scope="col" className="py-3 pr-4 text-right font-semibold">
              {t("pairs.col.duration")}
            </th>
            <th scope="col" className="py-3 pr-4 font-semibold">
              {t("pairs.col.error")}
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {pairs.map((p) => (
            <tr key={`${p.source}\0${p.destination}`}>
              <td className="max-w-[22rem] py-3 pl-4 pr-4">
                <span className="flex items-center gap-2">
                  <span className="truncate" title={p.source}>
                    {p.source}
                  </span>
                  <span aria-hidden="true" className="shrink-0 text-muted-foreground">
                    →
                  </span>
                  <span className="truncate" title={p.destination}>
                    {p.destination}
                  </span>
                </span>
              </td>
              <td className="py-3 pr-4">
                <Badge variant={PAIR_VARIANT[p.state] ?? "unknown"} dot>
                  {p.state}
                </Badge>
              </td>
              <td className="nums py-3 pr-4 text-right text-muted-foreground">{fmtDuration(p.durationNs)}</td>
              <td className={cn("py-3 pr-4 text-xs", p.error ? "text-health-bad" : "text-muted-foreground")}>
                {p.error ?? "—"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/**
 * CancelRunButton is POST /api/v1/runs/{id}/cancel; it is rendered ONLY for a non-terminal run and
 * only with runs:create.
 */
function CancelRunButton({ runId, onCancelled }: { runId: string; onCancelled: () => Promise<unknown> }) {
  // Note the asymmetry with the permission gate two paragraphs up, and that it is deliberate.
  const t = useT(runDetailDict);
  const guard = useWriteGuard();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleCancel() {
    setBusy(true);
    setError(undefined);
    try {
      await cancelRun(runId);
      await onCancelled();
    } catch (err) {
      // A problem+json refusal is the SERVER's sentence and renders verbatim;
      // only the "the request never got that far" fallback is ours.
      setError(err instanceof ApiError ? (err.problem.detail ?? err.problem.title) : t("cancel.failed"));
    }
    setBusy(false);
  }

  return (
    <>
      <Button size="sm" variant="outline" loading={busy} {...guard} onClick={handleCancel}>
        {t("cancel")}
      </Button>
      {error ? (
        <span role="alert" className="text-xs text-health-bad">
          {error}
        </span>
      ) : null}
    </>
  );
}

function NotFound({ runId }: { runId: string }) {
  const t = useT(runDetailDict);
  return (
    <PageShell
      title={t("notFound.title")}
      description={runId ? t("notFound.description", { id: decodeRunId(runId) }) : t("notFound.noId")}
    >
      <Card role="status" className="flex flex-col items-center gap-3 px-8 py-16 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <SearchX className="size-5" />
        </span>
        <p className="text-sm font-medium">{t("notFound.heading")}</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{t("notFound.body")}</p>
        <a href="/diagnostics" className="text-xs font-medium text-primary hover:underline">
          {t("notFound.back")}
        </a>
      </Card>
    </PageShell>
  );
}

export function RunDetailPage() {
  const t = useT(runDetailDict);
  const { locale } = useLocale();
  const runId = runIdFromPath(window.location.pathname);
  const { run, pairs, isLoading, notFound, error, live, refetch } = useRun(runId);
  const { can } = useAuth();
  /*
   * The Time Machine's framing for THIS page; the permalink itself stays reachable while engaged —
   * a permalink names one specific run.
   */
  const { at } = useTimeContext();

  if (notFound) return <NotFound runId={runId} />;

  if (!run && isLoading) {
    return (
      <PageShell title={t("title")} description={t("loading")}>
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">{t("loading.run")}</span>
          <Skeleton className="h-4 w-48" />
          <div className="mt-4 flex flex-col gap-2">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        </Card>
      </PageShell>
    );
  }

  if (!run) {
    return (
      <PageShell title={t("title")} description={decodeRunId(runId)}>
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">{t("unavailable.title")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {/* error.message came off the wire — verbatim. */}
            {error?.message ?? t("unavailable.body")}
          </p>
        </Card>
      </PageShell>
    );
  }

  const terminal = isTerminalRunStatus(run.status);
  /* The interval-run view is driven off the REST results ONLY, never the merged pair rows. */
  const durationNs = runDurationNs(run);
  const interval = isIntervalRun(run);
  const sampleGroups = groupSamplesByPair(run.results ?? []);
  const aggregate = aggregateSamples(run.results ?? []);

  return (
    <PageShell
      title={t("title")}
      description={
        at
          ? /* Inside a translated sentence, so the stamp takes that sentence's
               language — lib/i18n's localeTag (QA scope 2, finding #8). */
            t("description.at", { id: decodeRunId(run.id), at: at.toLocaleString(localeTag(locale)) })
          : decodeRunId(run.id)
      }
      actions={
        <>
          <StatusBadge status={run.status} />
          {/* Terminal FIRST, then the socket (QA round 4, finding #1). A
              finished run's data is final: it is neither live nor delayed, and
              "Delayed data" on a run that succeeded twenty minutes ago was a
              badge describing a transport nobody is waiting on — it sent
              operators looking for a stale-data problem that did not exist.
              The badge only means something while there is still something to
              arrive, so a non-terminal run with the socket down still says
              "Delayed data". */}
          {terminal ? null : <RealtimeBadge realtime={live} />}
          {can("runs:create") && !terminal ? (
            <CancelRunButton runId={run.id} onCancelled={refetch} />
          ) : null}
        </>
      }
    >
      <Card className="p-6">
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-xs text-muted-foreground">{t("field.type")}</dt>
            <dd className="nums mt-0.5">{run.type.toUpperCase()}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("field.plane")}</dt>
            <dd className="mt-0.5">{run.plane}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("field.pairs")}</dt>
            {/* ok/total, worded like the history row on /diagnostics (QA round
                4, finding #14): the bare "2/2" was arrived/total and read as
                passed/total, so a run whose every pair FAILED announced itself
                as complete success in the one number a reader scans first. */}
            <dd className="nums mt-0.5">
              {t("pairs.okOfTotal", { ok: okPairs(pairs), total: run.pairTotal })}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("field.started")}</dt>
            <dd className="mt-0.5">{fmtTime(run.startedAt, locale)}</dd>
          </div>
        </dl>
      </Card>

      {interval ? (
        <IntervalSummary durationNs={durationNs} agg={aggregate} pairCount={run.pairTotal} />
      ) : null}

      <Card asChild className="overflow-hidden p-0">
        <section>
          <div className="border-b border-border px-4 py-3">
            <h2 className="text-sm font-semibold">{t("pairs.title")}</h2>
            {interval ? <p className="mt-0.5 text-xs text-muted-foreground">{t("pairs.intervalNote")}</p> : null}
          </div>
          <PairTable pairs={pairs} />
        </section>
      </Card>

      {interval ? (
        <Card asChild className="overflow-hidden p-0">
          <section>
            <div className="border-b border-border px-4 py-3">
              <h2 className="text-sm font-semibold">{t("timeline.title")}</h2>
              <p className="mt-0.5 text-xs text-muted-foreground">{t("timeline.note")}</p>
            </div>
            {sampleGroups.length === 0 ? (
              <div className="px-6 py-10 text-center text-sm text-muted-foreground">{t("timeline.empty")}</div>
            ) : (
              <SampleTimeline groups={sampleGroups} durationNs={durationNs} running={!terminal} />
            )}
          </section>
        </Card>
      ) : null}
    </PageShell>
  );
}
