import { Fragment, useState, type ReactNode } from "react";
import { ChevronRight, SearchX } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { RealtimeBadge } from "@/components/realtime-badge";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Pager, usePager } from "@/components/ui/pager";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TBody, Td, Th, THead, Tr } from "@/components/ui/table";
import { useAuth } from "@/hooks/use-auth";
import { useQuery } from "@tanstack/react-query";
import { TraceDetail } from "@/components/mtr-hop-table";
import { isTerminalRunStatus, type RunPairRow, useRun } from "@/hooks/use-run";
import { ApiError, cancelRun, getMTRSnapshots } from "@/lib/api";
import { localeTag, stampFull, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { countForm, runDetailDict, type RunDetailKey } from "@/lib/i18n/dict/run-detail";
import {
  aggregateSamples,
  groupSamplesByPair,
  formatCadenceNs,
  formatCadenceProse,
  formatDurationNs,
  isIntervalRun,
  observedCadence,
  pairProgress,
  runCadence,
  snapshotForSample,
  runDurationNs,
  type ObservedCadence,
  type PairProgress,
  type PairSamples,
  type RunCadence,
  type SampleAggregate,
} from "@/lib/run-samples";
import { withAtParam, useTimeContext, useWriteGuard } from "@/lib/timemachine";
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

/** The em dash covers "no duration recorded" AND "a duration that is not a
 *  number": `(NaN / 1e6).toFixed(0)` is the string "NaN", and "NaNms" in the
 *  Duration column is a measurement an operator would go looking for. */
function fmtDuration(ns?: number): string {
  /* fmtNsCompact, not toFixed(0). Whole milliseconds printed "0ms" for every same-cluster TCP and
     ICMP probe — a real 0.46ms measurement rendered as no time at all — and collapsed everything
     below 1.5ms onto "0ms" or "1ms", hiding a 2x difference between two links in the column an
     operator opened the run to compare. */
  return fmtNsCompact(ns);
}

/** MISSING is every cell on this page's answer to a field the run did not
 *  carry — one glyph, so a hole reads as a hole and never as a value. */
const MISSING = "—";

/** text is what a field off the wire may be rendered as: a non-empty string, or
 *  the em dash. Guards the header card, whose four values are printed straight
 *  out of the JSON body. */
function text(v: unknown): string {
  return typeof v === "string" && v !== "" ? v : MISSING;
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
  // Same rule as fmtDuration: only a finite number is a latency.
  if (typeof ns !== "number" || !Number.isFinite(ns)) return MISSING;
  const ms = ns / 1e6;
  return ms < 10 ? `${ms.toFixed(1)}ms` : `${ms.toFixed(0)}ms`;
}

/** fmtPercent renders a 0..1 ratio. One decimal, because a 400-sample run can
 *  legitimately sit at 0.2% and rounding that to "0%" would erase the only
 *  failure the operator started the run to catch. */
export function fmtPercent(ratio: number): string {
  return `${(ratio * 100).toFixed(1)}%`;
}

/* ── the frame's two ends ──────────────────────────────────────────────────
   «нет чёткой границы начала и конца». Drawing every expected slot was only
   half of a frame: on a run that COMPLETED there is no placeholder tail left,
   so a full strip and an unframed pile of ticks looked identical, and on a
   wrapped strip nothing said which end was the run's start.

   The caps are drawn off the FRAME, not off the ticks, so they are there in
   every state a duration run can be in — mid-flight, finished, cancelled — and
   they bracket the track rather than sitting inside it: the track element stays
   a container of nothing but slots, which is what keeps a slot's identity
   stable as it fills (see the key note below). */
const CAP = "w-[3px] shrink-0 self-stretch border-border-strong";
const CAP_START = cn(CAP, "rounded-l-[2px] border-y border-l");
const CAP_END = cn(CAP, "rounded-r-[2px] border-y border-r");

/**
 * FrameEnds brackets one pair's track between the run's start and its end.
 *
 * `self-stretch` rather than a fixed height on purpose: a 500-slot strip wraps
 * onto several lines, and a bracket that spans all of them still reads as one
 * frame where a pair of short ticks at the far left and far right would not.
 *
 * Unframed it is its child and nothing else — a one-slot cadence has no span to
 * bracket, and drawing ends around a single dot is decoration.
 */
function FrameEnds({ framed, children }: { framed: boolean; children: ReactNode }) {
  if (!framed) return <>{children}</>;
  return (
    <div data-testid="timeline-frame" className="flex items-stretch gap-1.5">
      <span aria-hidden="true" data-testid="timeline-frame-start" className={CAP_START} />
      {children}
      <span aria-hidden="true" data-testid="timeline-frame-end" className={CAP_END} />
    </div>
  );
}

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
  /** The run's EFFECTIVE plan, so a stretched run's tail is counted in the
   *  interval it actually keeps rather than in the base one. */
  cadence,
  /** MTR only: a tick opens the route that probe walked. */
  isMTR,
  /** Non-terminal: only a run with something still to arrive gets a countdown. */
  running,
  /** The pager's list identity — see PairTable's own note. */
  runId,
  /** The response carried only the newest slice of the run; see the frame below. */
  truncated,
}: {
  groups: PairSamples[];
  durationNs: number;
  cadence?: RunCadence;
  isMTR: boolean;
  running: boolean;
  runId: string;
  truncated: boolean;
}) {
  const t = useT(runDetailDict);
  const { locale } = useLocale();
  /* Which probe the reader opened, by pair and instant. One at a time: each
     opens a read of that pair's stored routes. */
  /* The clicked PROBE, not just the pair and the instant. A tick opens the route that probe walked,
     and consecutive probes on an unchanged route walk the same one — so a panel that showed only the
     route was identical for every tick in the strip and read as a dead control (owner report). What
     differs per tick is the probe: its sequence, its own clock, its own latency or its own error. */
  const [openTick, setOpenTick] = useState<
    { source: string; destination: string; probe: PairSamples["samples"][number] } | null
  >(null);
  /* The shared default carries this list: a row here is a strip of up to 500
     ticks, and ten of them is the bound that used to be spelt out locally. */
  const pager = usePager(groups, { resetKey: runId });
  return (
    <>
    <div className="flex flex-col gap-3 px-4 py-4">
      {pager.visible.map((g) => {
        const agg = aggregateSamples(g.samples);
        const progress = pairProgress(durationNs, g.samples.length, cadence);
        /* No frame theater around a single dot: a one-probe cadence keeps the strip it always had.
           And NO FRAME AT ALL when the response was truncated: the plan describes the whole run
           while these samples are its newest slice, so framing them drew `planned - arrived` empty
           slots that read as "undispatched" — for probes that ran, succeeded, and were dropped by
           the store's cap. A finished run showed as 4% delivered. The plan floor is not comparable
           to a tail. */
        const framed = progress.framed && !truncated;
        const slotCount = framed ? progress.expected : g.samples.length;
        const counting = running && progress.remaining > 0 && !truncated;
        return (
          <div key={`${g.source}\0${g.destination}`} className="flex flex-col gap-1">
            <div className="flex items-baseline justify-between gap-3 text-xs">
              <span className="mono-data flex min-w-0 items-center gap-1.5">
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
            <FrameEnds framed={framed}>
            {/* Nothing but slots inside this element, ever: the caps live
                OUTSIDE it so a slot's position — and therefore the DOM node an
                arrival mutates in place — is the slot index and nothing else. */}
            <div
              /* `flex-1` ONLY inside the brackets, where it is what makes the
                 strip wrap at the frame's width instead of running out of it at
                 max-content. Unbracketed it would be a flex-basis of 0 in a
                 COLUMN, i.e. a height, which is not a size this row has any
                 business setting. */
              className={cn("flex flex-wrap items-end gap-[2px]", framed && "min-w-0 flex-1")}
              role="img"
              aria-label={trackLabel(t, locale, g, agg, progress, counting, framed)}
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
                const tickTitle = s.success
                  ? t("timeline.tick", { seq: s.sampleSeq, duration: fmtNsCompact(s.durationNs) })
                  : t("timeline.tickFailed", { seq: s.sampleSeq, outcome: s.error ?? t("timeline.tick.failed") });
                /* A failure is SHORTER as well as redder. Hue alone was the whole difference, and
                   under deuteranopia the two tokens land at 1.16:1 luminance — in the light theme at
                   1.00:1, literally the same lightness — so roughly one operator in twelve could not
                   see which probes in a run failed or where the failures clustered. Height survives
                   colour blindness and greyscale both, and it reads as what it is: a tick that fell
                   short. The pending slot already carries its own second channel (an outline). */
                const tickClass = cn(
                  "w-[6px] shrink-0 rounded-[1px] self-end",
                  s.success ? "h-4 bg-health-ok" : "h-2 bg-health-bad ring-1 ring-inset ring-health-bad",
                );
                if (isMTR) {
                  const opened =
                    openTick?.source === g.source &&
                    openTick?.destination === g.destination &&
                    openTick?.probe.recordedAt === s.recordedAt;
                  /* A tick on an MTR run is the entrance to one trace — «ничего
                     не кликабельно» was about exactly this strip. */
                  return (
                    <button
                      key={slot}
                      type="button"
                      data-testid={framed ? "timeline-slot-filled" : undefined}
                      aria-pressed={opened}
                      aria-label={`${tickTitle} — ${t("timeline.tick.open")}`}
                      title={`${tickTitle} — ${t("timeline.tick.open")}`}
                      onClick={() =>
                        setOpenTick(opened ? null : { source: g.source, destination: g.destination, probe: s })
                      }
                      className={cn(
                        tickClass,
                        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                        opened && "ring-2 ring-ring",
                      )}
                    />
                  );
                }
                return (
                  <span
                    key={slot}
                    data-testid={framed ? "timeline-slot-filled" : undefined}
                    /* A FAILED tick shows the error and no duration: the elapsed
                       time on a probe that never came back is dispatch overhead,
                       and printing it as a latency is exactly the masquerade the
                       card above promises does not happen. s.error is the
                       AGENT's own sentence and goes in verbatim. */
                    title={tickTitle}
                    className={tickClass}
                  />
                );
              })}
            </div>
            </FrameEnds>
            {openTick?.source === g.source && openTick.destination === g.destination ? (
              <div className="rounded-md border border-border bg-surface-2/30">
                <PairTrace source={openTick.source} destination={openTick.destination} probe={openTick.probe} />
              </div>
            ) : null}
            {framed ? (
              <p data-testid="timeline-progress" className="nums text-[11px] text-muted-foreground">
                {/* progress.PLANNED behind the "≥", never progress.expected:
                    the frame widens to hold a thirteenth sample the floor did
                    not predict, and a caption that widened with it would read
                    "13 of ≥13" — a plan nobody made, and one that hides the run
                    beating its own floor. */}
                {counting
                  ? t("timeline.progress.running", {
                      arrived: progress.arrived,
                      expected: progress.planned,
                      remaining: formatDurationNs(progress.remainingNs, locale),
                    })
                  : t("timeline.progress.settled", { arrived: progress.arrived, expected: progress.planned })}
              </p>
            ) : null}
          </div>
        );
      })}
    </div>
    <Pager pager={pager} subject={t("pairs.subject")} />
    </>
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
  /* The response carried a TAIL of the run, so the plan's floor is not a number this row can be
     measured against — the strip drops its frame for the same reason, and the label must not go on
     announcing "N of at least M" to a screen reader after the frame is gone. */
  framed: boolean,
): string {
  if (!framed) {
    return t("timeline.rowLabel", {
      source: g.source,
      destination: g.destination,
      sent: agg.sent,
      failed: agg.failed,
    });
  }
  /* "at least {expected}" is the PLAN's floor here for the same reason the
     visible caption uses it — the screen reader gets the same numbers. */
  if (counting) {
    return t(`timeline.trackLabel.pending.${countForm(locale, progress.remaining)}` as RunDetailKey, {
      source: g.source,
      destination: g.destination,
      arrived: progress.arrived,
      expected: progress.planned,
      count: progress.remaining,
    });
  }
  return t("timeline.trackLabel.settled", {
    source: g.source,
    destination: g.destination,
    arrived: progress.arrived,
    expected: progress.planned,
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
  cadence,
  observed,
}: {
  durationNs: number;
  agg: SampleAggregate;
  pairCount: number;
  /** The run's effective plan; undefined only for a run with no duration at
   *  all, which never reaches this card. */
  cadence?: RunCadence;
  /** What the samples on screen MEASURE; undefined until some pair has two. */
  observed?: ObservedCadence;
}) {
  const t = useT(runDetailDict);
  const { locale } = useLocale();
  return (
    <Card className="p-6">
      {/* Eight units, not seven: the cadence tile carries two lines and a wider
          string than the rest, so it takes two columns and the row grew by one
          rather than wrapping p95/max onto its own line. */}
      <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4 lg:grid-cols-8">
        <div>
          <dt className="text-xs text-muted-foreground">{t("summary.duration")}</dt>
          <dd className="nums mt-0.5">{formatDurationNs(durationNs, locale)}</dd>
        </div>
        {/* MEASURED and PLANNED, never one number wearing both hats.

            The tile used to print the base cadence off the duration and a bare
            "× 4" that named nothing; rev13 replaced that with the planner's
            effective interval, which was truer but still a PLAN presented as a
            fact — «Периодичность 3 мин» on a run that was producing a probe a
            minute. The plan is a worst case (a round that finishes early starts
            the next one immediately), so it is an upper bound on the spacing,
            and the real number sits below it by however much the fleet beats
            its own budget.

            So: the headline is what the samples on screen MEASURE as soon as
            any pair has two of them, and it says "measured" in its own words.
            The plan stays, one line down, worded as the bound it is. Until
            there is anything to measure the headline is the plan and says
            "planned" — no number is ever labelled the other one's kind. */}
        <div data-testid="summary-cadence" className="col-span-2">
          <dt className="text-xs text-muted-foreground">{t("summary.cadence")}</dt>
          <dd className="nums mt-0.5">
            {t(observed ? "summary.cadence.value.measured" : "summary.cadence.value.planned", {
              interval: formatCadenceNs(observed?.intervalNs ?? cadence?.intervalNs ?? 0, locale),
            })}
          </dd>
          <dd className="nums mt-0.5 text-[11px] text-muted-foreground">
            {observed
              ? t("summary.cadence.observed", {
                  pairs: t(`summary.pairs.${countForm(locale, pairCount)}` as RunDetailKey, { count: pairCount }),
                  samples: observed.samplesPerPair,
                })
              : t("summary.cadence.plan", {
                  pairs: t(`summary.pairs.${countForm(locale, pairCount)}` as RunDetailKey, { count: pairCount }),
                  samples: cadence?.samplesPerPair ?? 0,
                })}
          </dd>
          {/* Only alongside a measurement: on its own the plan is already the
              line above, and repeating it would be two labels for one number. */}
          {observed ? (
            <dd data-testid="summary-cadence-plan" className="nums mt-0.5 text-[11px] text-muted-foreground">
              {t("summary.cadence.planNote", {
                interval: formatCadenceProse(cadence?.intervalNs ?? 0, locale),
                samples: cadence?.samplesPerPair ?? 0,
              })}
            </dd>
          ) : null}
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


/* ── the route a probe walked ────────────────────────────────────────────── */

/**
 * PairTrace is the hop table of ONE pair's recorded route, fetched only when a
 * reader asks for it.
 *
 * The owner on this page: «вся суть MTR — это путь», and «ничего не кликабельно».
 * A run's results carry an outcome and a duration and no hops at all — the path
 * lives in the MTR projection — so this reads it back by pair, and by instant
 * when a specific probe was clicked. The table itself is the Explorer's own
 * component: one hop table in this console, not two.
 */
function PairTrace({
  source,
  destination,
  /** The probe itself, when the reader clicked one tick rather than a pair row. */
  probe,
}: {
  source: string;
  destination: string;
  probe?: PairSamples["samples"][number];
}) {
  const t = useT(runDetailDict);
  const { locale } = useLocale();
  const recordedAt = probe?.recordedAt;
  /* The clicked probe's instant is IN THE KEY, so opening a tick recorded after this panel was
     first opened re-asks rather than being told, from a cached list, that no route covers it. A
     running MTR run produces a probe every few seconds and the route it walked is projected right
     behind it; a list fetched once at open goes stale within one cadence. */
  const query = useQuery({
    queryKey: ["mtr", "snapshots", source, destination, recordedAt ?? ""],
    queryFn: () => getMTRSnapshots({ source, destination, limit: 20 }),
  });

  const snapshots = query.data?.snapshots ?? [];
  /* Scoped to the clicked probe when there is one, else the pair's latest
     route. snapshotForSample returns nothing rather than the nearest path: a
     route under a tick that did not walk it is worse than no route.

     A FAILED probe never walked one at all — it timed out, or never left the dispatcher — so it
     gets no route regardless of what the clock would match. Captioning a stored hop table with
     "the route this probe walked" over a probe that walked nothing is the confident lie this whole
     lookup exists to avoid. */
  const chosen =
    probe && !probe.success
      ? undefined
      : recordedAt
        ? snapshots.find((s) => s.id === snapshotForSample(snapshots, recordedAt)?.id)
        : snapshots[0];

  /* The deep link the Explorer answers — pages/mtr.tsx reads ?source= and
     ?destination=, opens that card AND selects the pair so its path history is
     the one on screen, not the destination's generic view. */
  const explorerHref = `/mtr?source=${encodeURIComponent(source)}&destination=${encodeURIComponent(destination)}`;

  return (
    <div className="flex flex-col gap-2 px-4 py-3">
      {/* THIS probe, before any route. It is the only part of the panel that changes from tick to
          tick, so it goes first and it is stated plainly: which probe, when, and what it measured. */}
      {probe ? (
        <div data-testid="trace-probe" className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-xs">
          <span className="font-medium">{t("trace.probe", { seq: probe.sampleSeq })}</span>
          <span className="mono-data text-muted-foreground">{fmtTime(probe.recordedAt, locale)}</span>
          {probe.success ? (
            <span className="mono-data text-muted-foreground">{fmtNsCompact(probe.durationNs)}</span>
          ) : (
            <span className="text-health-bad">{probe.error ?? t("timeline.tick.failed")}</span>
          )}
        </div>
      ) : null}
      {query.isPending ? (
        <div role="status" aria-live="polite">
          <span className="sr-only">{t("trace.loading")}</span>
          <Skeleton className="h-24 w-full" />
        </div>
      ) : null}
      {query.isError ? (
        <p role="alert" className="text-xs text-health-bad">
          {t("trace.error")}
        </p>
      ) : null}
      {query.isSuccess && !chosen ? (
        <p className="text-xs leading-relaxed text-muted-foreground">
          {probe && !probe.success ? t("trace.probeFailed") : recordedAt ? t("trace.noneForProbe") : t("trace.none")}
        </p>
      ) : null}
      {chosen ? (
        <>
          {/* Why two ticks in a row can look identical, said once rather than left to be guessed at:
              the hops are a property of the ROUTE, and the route is folded over every trace that
              walked it. An unchanged route IS the answer, and it now says so. */}
          {probe ? <p className="text-xs leading-relaxed text-muted-foreground">{t("trace.sharedRoute")}</p> : null}
          <TraceDetail snapshot={chosen} />
        </>
      ) : null}
      {/* The way OUT of this page and into the one that exists for paths. */}
      <a
        href={explorerHref}
        className="w-fit rounded text-xs text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {t("trace.openInExplorer")}
      </a>
    </div>
  );
}

/**
 * PairDetail is what a NON-MTR pair row opens into: the sample's own facts,
 * whole. The table above truncates the error into a cell; a timeout's actual
 * sentence is the thing an operator came for.
 */
function PairDetail({ pair }: { pair: RunPairRow }) {
  const t = useT(runDetailDict);
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1 px-4 py-3 text-xs">
      <dt className="text-muted-foreground">{t("detail.source")}</dt>
      <dd className="mono-data break-all">{pair.source}</dd>
      <dt className="text-muted-foreground">{t("detail.destination")}</dt>
      <dd className="mono-data break-all">{pair.destination}</dd>
      <dt className="text-muted-foreground">{t("detail.duration")}</dt>
      <dd className="mono-data">{fmtDuration(pair.durationNs)}</dd>
      <dt className="text-muted-foreground">{t("detail.state")}</dt>
      <dd>{pair.state}</dd>
      {pair.error ? (
        <>
          <dt className="text-muted-foreground">{t("detail.error")}</dt>
          {/* The agent's own sentence, whole and wrapped — never the cell's
              truncation, which is where it usually gets interesting. */}
          <dd className="whitespace-pre-wrap break-words text-health-bad">{pair.error}</dd>
        </>
      ) : null}
    </dl>
  );
}

function PairTable({ pairs, isMTR, runId }: { pairs: RunPairRow[]; isMTR: boolean; runId: string }) {
  const t = useT(runDetailDict);
  /* An all-to-all run is n² rows — ninety for ten nodes — and the table used to
     be every one of them under an endless scroll.

     resetKey is the RUN: this page reads its id off the pathname and stays
     mounted when the reader opens a second permalink whose body is already
     cached (going back to a run they had open). Without it, run B's pairs
     opened at run A's page 6 — a page number that addresses nothing in the new
     list, and a table the reader has to walk backwards to read from the top. */
  const pager = usePager(pairs, { resetKey: runId });
  /* One open row at a time, keyed by pair: a fetch per expanded row is the cost
     of this affordance, and ten of them at once is not what the reader asked
     for. Opening another closes the first. */
  const [open, setOpen] = useState<string | null>(null);
  if (pairs.length === 0) {
    return <div className="px-6 py-10 text-center text-sm text-muted-foreground">{t("pairs.empty")}</div>;
  }
  return (
    <>
    <Table variant="dense">
      <caption className="sr-only">{t("pairs.caption")}</caption>
      <THead>
        <Tr>
          <Th className="w-8 pl-4 pr-2">
            <span className="sr-only">{t("pairs.col.expand")}</span>
          </Th>
          <Th className="pr-4">{t("pairs.col.pair")}</Th>
          <Th className="pr-4">{t("pairs.col.state")}</Th>
          <Th numeric className="pr-4">
            {t("pairs.col.duration")}
          </Th>
          <Th className="pr-4">{t("pairs.col.error")}</Th>
        </Tr>
      </THead>
      <TBody>
          {pager.visible.map((row) => {
            /* Both names normalised before anything reads them: a result row
               that lost its sourceNode put the literal word "undefined" into
               this row's expander label — the one string a screen reader gets
               for the whole pair — and into the row's own React key. */
            const p = { ...row, source: row.source ?? "", destination: row.destination ?? "" };
            const key = `${p.source}\u0000${p.destination}`;
            const expanded = open === key;
            const detailId = `run-pair-${encodeURIComponent(key)}`;
            return (
            <Fragment key={key}>
            <Tr>
              <Td className="pl-4 pr-2 align-top">
                {/* The whole point of the owner's «ничего не кликабельно»: a
                    pair row is where the route lives, and it opened nothing. */}
                <button
                  type="button"
                  aria-expanded={expanded}
                  aria-controls={detailId}
                  aria-label={t("pairs.expand.aria", { source: p.source, destination: p.destination })}
                  onClick={() => setOpen(expanded ? null : key)}
                  className={cn(
                    "flex size-5 items-center justify-center rounded text-muted-foreground",
                    "transition-colors duration-(--dur-fast) ease-(--ease) hover:text-foreground",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  )}
                >
                  <ChevronRight
                    aria-hidden="true"
                    className={cn(
                      "size-3.5 transition-transform duration-(--dur-fast) ease-(--ease)",
                      expanded && "rotate-90",
                    )}
                  />
                </button>
              </Td>
              <Td className="mono-data max-w-[22rem] pr-4">
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
              </Td>
              <Td className="pr-4">
                <Badge variant={PAIR_VARIANT[p.state] ?? "unknown"} dot>
                  {p.state}
                </Badge>
              </Td>
              <Td numeric className="pr-4">{fmtDuration(p.durationNs)}</Td>
              <Td className={cn("pr-4 text-xs", p.error ? "text-health-bad" : "text-muted-foreground")}>
                {p.error ?? "—"}
              </Td>
            </Tr>
            {expanded ? (
              <Tr>
                <Td id={detailId} colSpan={5} className="bg-surface-2/30 p-0">
                  {/* An MTR's pair row opens onto its ROUTE; anything else opens
                      onto the sample's own facts, which the cells truncate. */}
                  {isMTR ? (
                    <PairTrace source={p.source} destination={p.destination} />
                  ) : (
                    <PairDetail pair={p} />
                  )}
                </Td>
              </Tr>
            ) : null}
            </Fragment>
            );
          })}
      </TBody>
    </Table>
    <Pager pager={pager} subject={t("pairs.subject")} />
    </>
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
      timeMachine
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
        <a href={withAtParam("/diagnostics")} className="text-xs font-medium text-primary hover:underline">
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

  /* No id in the URL is a not-found, not a network fault. useRun leaves its
     query DISABLED for an empty id — nothing is ever fetched, so nothing ever
     errors — and the page fell through to "This run is unavailable / Failed to
     load this run", blaming a request it never made. NotFound has carried the
     right sentence («В адресе нет идентификатора запуска») since it was
     written; nothing routed here to say it. */
  if (notFound || runId === "") return <NotFound runId={runId} />;

  if (!run && isLoading) {
    return (
      <PageShell timeMachine title={t("title")} description={t("loading")}>
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
      <PageShell timeMachine title={t("title")} description={decodeRunId(runId)}>
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
  /* `?? []` covered a null results field and nothing else: a body whose
     `results` is an object rather than a list made `for (const r of results)`
     throw, and a TypeError inside a page component is a white screen with the
     permalink still in the address bar. A shape this page cannot iterate is no
     results, which the two cards below already have a sentence for. */
  const results = Array.isArray(run.results) ? run.results : [];
  const sampleGroups = groupSamplesByPair(results);
  /* The agents this run is actually spread over — the client-side mirror of the
     planner needs it, and the run's own samples are the only place it is
     recorded. Zero until the first result lands, which effectiveSampleIntervalNs
     reads as "one source" rather than dividing by none. */
  const sourceCount = new Set(sampleGroups.map((g) => g.source)).size;
  const cadence = runCadence(run, sourceCount);
  /* What the run is ACTUALLY doing, off the timestamps already on screen. The
     plan above is a worst case and routinely overstates the spacing by a large
     multiple; the tile refuses to present either as the other. */
  const observed = observedCadence(sampleGroups);
  const aggregate = aggregateSamples(results);
  /* pairTotal is a field of a JSON body like any other. Absent, it printed
     "0/undefined ok" in the one number a reader scans first; as a string it
     printed «abc пар» on the cadence tile. The rows this page actually HAS are
     the honest fallback — never larger than the truth, and never a non-number. */
  const pairTotal = Number.isFinite(run.pairTotal) ? Number(run.pairTotal) : pairs.length;
  const isMTR = run.type === "mtr";

  return (
    <PageShell
      timeMachine
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
            {/* `run.type.toUpperCase()` on a body without a type is a TypeError
                and therefore a white screen — the harshest possible answer to
                the mildest possible wire fault. */}
            <dd className="nums mt-0.5">{text(run.type).toUpperCase()}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("field.plane")}</dt>
            <dd className="mt-0.5">{text(run.plane)}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("field.pairs")}</dt>
            {/* ok/total, worded like the history row on /diagnostics (QA round
                4, finding #14): the bare "2/2" was arrived/total and read as
                passed/total, so a run whose every pair FAILED announced itself
                as complete success in the one number a reader scans first. */}
            <dd className="nums mt-0.5">
              {t("pairs.okOfTotal", { ok: okPairs(pairs), total: pairTotal })}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("field.started")}</dt>
            <dd className="mt-0.5">{fmtTime(run.startedAt, locale)}</dd>
          </div>
        </dl>
      </Card>

      {/* A run can hold more results than one read may carry (the API's own bound), and then every
          number on this page describes the TAIL of the run rather than the run. Saying so is the
          difference between a summary and a wrong summary. */}
      {run.resultsTruncated ? (
        <p role="status" className="rounded-md border border-border bg-surface-2/40 px-4 py-2 text-xs leading-relaxed text-muted-foreground">
          {t("results.truncated", { count: results.length })}
        </p>
      ) : null}

      {interval ? (
        <IntervalSummary
          durationNs={durationNs}
          agg={aggregate}
          pairCount={pairTotal}
          cadence={cadence}
          observed={observed}
        />
      ) : null}

      <Card asChild className="overflow-hidden p-0">
        <section>
          <div className="border-b border-border px-4 py-3">
            <h2 className="type-section">{t("pairs.title")}</h2>
            {interval ? <p className="mt-0.5 text-xs text-muted-foreground">{t("pairs.intervalNote")}</p> : null}
          </div>
          {/* runId, not run.id: the pager's reset key is the permalink the
              reader is ON, so opening a second run from a cached list starts at
              its page one rather than at page 6 of the run before it. */}
          <PairTable pairs={pairs} isMTR={isMTR} runId={runId} />
        </section>
      </Card>

      {interval ? (
        <Card asChild className="overflow-hidden p-0">
          <section>
            <div className="border-b border-border px-4 py-3">
              <h2 className="type-section">{t("timeline.title")}</h2>
              <p className="mt-0.5 text-xs text-muted-foreground">{t("timeline.note")}</p>
            </div>
            {sampleGroups.length === 0 ? (
              <div className="px-6 py-10 text-center text-sm text-muted-foreground">{t("timeline.empty")}</div>
            ) : (
              <SampleTimeline
                groups={sampleGroups}
                durationNs={durationNs}
                cadence={cadence}
                isMTR={isMTR}
                running={!terminal}
                runId={runId}
                truncated={run.resultsTruncated === true}
              />
            )}
          </section>
        </Card>
      ) : null}
    </PageShell>
  );
}
