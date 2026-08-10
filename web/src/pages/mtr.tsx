import { useCallback, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { PathChangesTimeline } from "@/components/mtr-changes-timeline";
import { TraceDetail, fmtTime, shortHash, type TrendHistory } from "@/components/mtr-hop-table";
import { PathDiff } from "@/components/mtr-path-diff";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useTopology } from "@/hooks/use-topology";
import {
  ApiError,
  createRun,
  getMTRDestinations,
  getMTRSnapshot,
  getMTRSnapshots,
  listTargets,
} from "@/lib/api";
import { localeTag, useLocale, useT } from "@/lib/i18n";
import { countForm, mtrDict, type MTRKey } from "@/lib/i18n/dict/mtr";
import { scopeNodeOptions } from "@/lib/investigation-sources";
import { formatDurationNs, plannedSamplesPerPair, sampleIntervalNs } from "@/lib/run-samples";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import type { DestinationKind, MTRDestination, PathSnapshot } from "@/lib/types";
import { CHECKBOX_CLASS, cn } from "@/lib/utils";
/* The Runner below builds the SAME POST /api/v1/runs body the Diagnostics run form does, from the same controls. */
import {
  CONTROL_CLASS,
  FieldLabel,
  NodeSelector,
  buildRunRequest,
  durationNsFor,
  estimatePairCount,
  RUN_DURATIONS,
  toggleName,
} from "./diagnostics";

/* The Runner offers the same three destination kinds the Diagnostics form does, and names them the same way. */
const DESTINATION_KIND_KEYS: Record<DestinationKind, MTRKey> = {
  node: "runner.kind.node",
  target: "runner.kind.target",
  adhoc: "runner.kind.adhoc",
};

/* The hop table moved into its own component when this page passed ~800 lines. */
export { fmtRttNs, shortHash, TraceDetail } from "@/components/mtr-hop-table";

/* ── pure helpers (exported for their own tests) ────────────────────────── */

/** A Pair is what pane 1 hands pane 2: the two filters GET
 *  /api/v1/mtr/snapshots refuses to work without. */
export interface Pair {
  source: string;
  destination: string;
}

/** DestinationGroup is one destination with every source that has traced it. */
export interface DestinationGroup {
  destination: string;
  sources: MTRDestination[];
  snapshotCount: number;
  lastSeen: string;
}

/**
 * groupDestinations turns the flat list GET /api/v1/mtr/destinations answers into MTR_EXPLORER.md's
 * pane 1 shape; encounter order is preserved on both levels rather than re-sorted.
 */
export function groupDestinations(rows: MTRDestination[]): DestinationGroup[] {
  const byDestination = new Map<string, DestinationGroup>();
  for (const row of rows) {
    const group = byDestination.get(row.destination);
    if (!group) {
      byDestination.set(row.destination, {
        destination: row.destination,
        sources: [row],
        snapshotCount: row.snapshotCount,
        lastSeen: row.lastSeen,
      });
      continue;
    }
    group.sources.push(row);
    group.snapshotCount += row.snapshotCount;
    if (msOf(row.lastSeen) > msOf(group.lastSeen)) group.lastSeen = row.lastSeen;
  }
  return [...byDestination.values()];
}

/** msOf compares two wire timestamps as instants. */
function msOf(ts: string): number {
  const ms = new Date(ts).getTime();
  return Number.isNaN(ms) ? -Infinity : ms;
}

/**
 * pathChangeFlags marks, index-aligned with `snapshots`; the list arrives newest-first (the store's
 * (source, destination, last_seen DESC, id DESC) index).
 */
export function pathChangeFlags(snapshots: PathSnapshot[]): boolean[] {
  return snapshots.map((s, i) => {
    const older = snapshots[i + 1];
    return older !== undefined && older.pathHash !== s.pathHash;
  });
}

/**
 * toggleCompare is the two-snapshot selection rule, as a pure function; un-ticking removes, and the
 * rule is stated in the pane's own copy rather than left to be discovered.
 */
export function toggleCompare(selected: string[], id: string): string[] {
  if (selected.includes(id)) return selected.filter((s) => s !== id);
  if (selected.length < 2) return [...selected, id];
  return [selected[1], id];
}

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

/* ── shared chrome ──────────────────────────────────────────────────────── */

/** PermissionCard is PAGES.md:126-129's pattern, the same component targets.tsx and target-card.tsx already use. */
function PermissionCard({ permission, children }: { permission: string; children: ReactNode }) {
  const t = useT(mtrDict);
  return (
    <Card role="status" className="p-6">
      {/* The permission STRING is an identifier and interpolates verbatim. */}
      <p className="text-sm font-medium">{t("permission.title", { permission })}</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
}

function EmptyNote({ children }: { children: ReactNode }) {
  return <p className="px-1 py-10 text-center text-xs leading-relaxed text-muted-foreground">{children}</p>;
}

function ListSkeleton() {
  const t = useT(mtrDict);
  return (
    <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
      <span className="sr-only">{t("loading")}</span>
      {Array.from({ length: 3 }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}

/** Pane is the frame all three panes share: a card that is a labelled region,
 *  so a test (and a screen reader) can address "the destinations pane" rather
 *  than "the first card". */
function Pane({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card asChild className="min-w-0 p-5">
      <section aria-label={title}>
        <h2 className="text-sm font-semibold">{title}</h2>
        {children}
      </section>
    </Card>
  );
}

/* ── pane 1: destinations ───────────────────────────────────────────────── */

function DestinationsPane({
  selected,
  onSelect,
}: {
  selected: Pair | null;
  /** The whole row, not just its two names: the row also carries the pair's
   *  snapshot and trace TOTALS, which is what lets pane 3's trend say how much
   *  of the pair's history it is actually drawing. */
  onSelect: (row: MTRDestination) => void;
}) {
  const t = useT(mtrDict);
  const { locale } = useLocale();
  const query = useQuery({ queryKey: ["mtr", "destinations"], queryFn: getMTRDestinations });
  const groups = useMemo(() => groupDestinations(query.data?.destinations ?? []), [query.data]);

  return (
    <Pane title={t("destinations.title")}>
      {query.isError ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">
          {/* problem+json is the server's own sentence — verbatim. */}
          {queryErrorMessage(query.error, t("destinations.error"))}
        </p>
      ) : null}
      {/* isPending, not isLoading: a paused retry (react-query pauses while
          the browser thinks it is offline) is pending-but-not-fetching, and
          the old !isLoading && !isError empty-guard would present "nothing
          traced" as a settled answer. M7 final-gate finding. */}
      {query.isPending ? <ListSkeleton /> : null}

      {/* The honest empty state: path history is a PROJECTION of MTR results
          the console ingested, so "nothing here" means "nobody has run one",
          not "the feature is broken" — and the place to run one is
          Diagnostics. (Task 8 adds a Runner tab to this very page; until it
          lands, sending the reader somewhere that exists beats naming a tab
          that does not.) */}
      {query.isSuccess && groups.length === 0 ? (
        <EmptyNote>
          {/* Three keys, not one interpolation: the link sits INSIDE the
              sentence, which is the one shape a placeholder cannot carry. */}
          {t("destinations.empty.before")}{" "}
          <a href="/diagnostics" className="text-primary hover:underline">
            {t("destinations.empty.link")}
          </a>{" "}
          {t("destinations.empty.after")}
        </EmptyNote>
      ) : null}

      {/* One list PER destination, labelled with the destination's own name,
          rather than one flat list of pairs: the label is what makes "the
          sources that traced node-b" addressable — to a screen reader and to
          a test — without inventing a role="group" that an <li> would only
          fight with. */}
      {groups.length > 0 ? (
        <div className="mt-4 flex flex-col gap-4">
          {groups.map((group) => (
            <div key={group.destination}>
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <h3 className="truncate text-sm font-medium">{group.destination}</h3>
                <span className="nums text-xs text-muted-foreground">
                  {t(`paths.${countForm(locale, group.snapshotCount)}` as MTRKey, { count: group.snapshotCount })}
                </span>
              </div>
              <ul aria-label={group.destination} className="mt-1.5 flex flex-col gap-1">
                {group.sources.map((row) => {
                  const active =
                    selected?.source === row.sourceNode && selected?.destination === row.destination;
                  return (
                    <li key={`${row.sourceNode}→${row.destination}`}>
                      <button
                        type="button"
                        aria-pressed={active}
                        aria-label={`${row.sourceNode} → ${row.destination}`}
                        onClick={() => onSelect(row)}
                        className={cn(
                          "flex w-full items-baseline justify-between gap-3 rounded-md px-2 py-1.5 text-left text-xs",
                          "transition-colors duration-(--dur) ease-(--ease) hover:bg-accent",
                          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                          active ? "bg-accent font-medium" : "text-muted-foreground",
                        )}
                      >
                        {/* The NAME wins the width fight (QA scope 4, finding
                            #5). The count used to be shrink-0, so in Russian —
                            where "трассировок" is three times the width of
                            "traces" — it ate the row and the source collapsed
                            to «от qa-nod…», which names nothing. flex-1 with a
                            zero basis hands the leftover to the name and puts
                            all the shrinking on the count, and the cap keeps
                            the name at least 55% of the row whatever the
                            language does. */}
                        <span
                          className="min-w-0 flex-1 truncate"
                          title={t("destinations.from", { node: row.sourceNode })}
                        >
                          {t("destinations.from", { node: row.sourceNode })}
                        </span>
                        <span
                          className="nums min-w-0 max-w-[45%] shrink truncate text-muted-foreground"
                          title={`${row.snapshotCount} · ${t(`traces.${countForm(locale, row.traceCount)}` as MTRKey, { count: row.traceCount })}`}
                        >
                          {row.snapshotCount} ·{" "}
                          {t(`traces.${countForm(locale, row.traceCount)}` as MTRKey, { count: row.traceCount })}
                        </span>
                      </button>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))}
        </div>
      ) : null}
    </Pane>
  );
}

/* ── pane 2: snapshot history ───────────────────────────────────────────── */

/**
 * SnapshotHistory is the pair's loaded routes plus everything two panes need to say about them; it
 * is a HOOK rather than pane-2 state because pane 3's per-hop trend is drawn from these very
 * snapshots.
 */
interface SnapshotHistory {
  snapshots: PathSnapshot[];
  loading: boolean;
  error: unknown;
  /** The cursor says older routes exist — pane 3 turns this into "the trend is
   *  a window, not the whole history". */
  hasOlder: boolean;
  loadOlder: () => void;
}

/**
 * useSnapshotHistory loads the pair's distinct routes; a selection change RESETS to page one rather
 * than appending.
 */
function useSnapshotHistory(pair: Pair | null): SnapshotHistory {
  const [snapshots, setSnapshots] = useState<PathSnapshot[]>([]);
  const [page, setPage] = useState<{ nextCursor: string; loading: boolean; error: unknown }>({
    nextCursor: "",
    loading: false,
    error: null,
  });

  const load = useCallback(
    async (target: Pair, cursor?: string) => {
      setPage((p) => ({ ...p, loading: true, error: null }));
      try {
        const body = await getMTRSnapshots({ source: target.source, destination: target.destination, cursor });
        // A cursor-less call is page one — replace, so a re-selection or a
        // remount does not just keep growing the list.
        setSnapshots((prev) => (cursor ? [...prev, ...body.snapshots] : body.snapshots));
        setPage({ nextCursor: body.nextCursor, loading: false, error: null });
      } catch (err) {
        if (!cursor) setSnapshots([]);
        setPage({ nextCursor: "", loading: false, error: err });
      }
    },
    [],
  );

  useEffect(() => {
    if (!pair) {
      setSnapshots([]);
      setPage({ nextCursor: "", loading: false, error: null });
      return;
    }
    setSnapshots([]);
    void load(pair, undefined);
  }, [pair, load]);

  const loadOlder = useCallback(() => {
    if (pair && page.nextCursor !== "" && !page.loading) void load(pair, page.nextCursor);
  }, [pair, page.nextCursor, page.loading, load]);

  return {
    snapshots,
    loading: page.loading,
    error: page.error,
    hasOlder: page.nextCursor !== "",
    loadOlder,
  };
}

function HistoryPane({
  pair,
  history,
  selectedId,
  onSelectSnapshot,
  compare,
  onToggleCompare,
}: {
  pair: Pair | null;
  history: SnapshotHistory;
  selectedId: string | null;
  onSelectSnapshot: (snapshot: PathSnapshot) => void;
  compare: string[];
  onToggleCompare: (id: string) => void;
}) {
  const t = useT(mtrDict);
  const { locale } = useLocale();
  const { snapshots, loading, error, hasOlder, loadOlder } = history;
  const changed = useMemo(() => pathChangeFlags(snapshots), [snapshots]);

  return (
    <Pane title={t("history.title")}>
      {pair ? (
        <p className="mt-0.5 truncate text-xs text-muted-foreground">
          {pair.source} → {pair.destination}
        </p>
      ) : null}

      {/* The timeline sits ABOVE the list because it is the list's index: a
          marker per loaded route on the axis its loss series is drawn on. It
          renders nothing at all without snapshots, so the empty and error
          states below are untouched. */}
      {pair && snapshots.length > 0 ? (
        <PathChangesTimeline source={pair.source} destination={pair.destination} snapshots={snapshots} />
      ) : null}

      {/* No "on the left" (QA round 4, finding #20): under ~700px the three
          panes stack, and the destinations pane is ABOVE this one, not beside
          it — the copy was pointing at empty space. The neutral wording is
          true at every width. */}
      {!pair ? <EmptyNote>{t("history.noPair")}</EmptyNote> : null}

      {error ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">
          {queryErrorMessage(error, t("destinations.error"))}
        </p>
      ) : null}

      {pair && loading && snapshots.length === 0 ? <ListSkeleton /> : null}

      {pair && !loading && !error && snapshots.length === 0 ? (
        <EmptyNote>{t("history.empty")}</EmptyNote>
      ) : null}

      {/* Two, not one: "tick two paths to diff them" in front of a list with a
          single row is an instruction the reader cannot follow (QA scope 4,
          finding #13). The checkbox still renders on a lone row — a "Load
          older" away there may be a second. */}
      {snapshots.length >= 2 ? (
        <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("history.compareHint")}</p>
      ) : null}

      {snapshots.length > 0 ? (
        <ul aria-label={t("history.list.aria")} className="mt-1 divide-y divide-border">
          {snapshots.map((s, i) => (
            <li key={s.id} className="flex items-center gap-2">
              {/* Outside the row button, not inside it: a checkbox nested in a
                  button is neither valid HTML nor operable by keyboard, and
                  ticking one must not also change which trace pane 3 shows. */}
              <input
                type="checkbox"
                aria-label={t("history.compare.aria", { hash: shortHash(s.pathHash) })}
                checked={compare.includes(s.id)}
                onChange={() => onToggleCompare(s.id)}
                className={CHECKBOX_CLASS}
              />
              <button
                type="button"
                aria-pressed={s.id === selectedId}
                aria-label={t("history.path.aria", { hash: shortHash(s.pathHash) })}
                title={s.pathHash}
                onClick={() => onSelectSnapshot(s)}
                className={cn(
                  "flex min-w-0 flex-1 flex-col gap-1 rounded-md px-2 py-3 text-left",
                  "transition-colors duration-(--dur) ease-(--ease) hover:bg-accent",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  s.id === selectedId ? "bg-accent" : null,
                )}
              >
                <span className="flex flex-wrap items-center gap-2">
                  <span className="nums font-mono text-xs">{shortHash(s.pathHash)}</span>
                  <Badge variant="neutral">
                    {t(`hops.${countForm(locale, s.hopCount)}` as MTRKey, { count: s.hopCount })}
                  </Badge>
                  {changed[i] ? <Badge variant="warn">{t("history.changed")}</Badge> : null}
                </span>
                <span className="nums text-xs text-muted-foreground">
                  {/* The stamps are computed here and passed in, never
                      formatted by the dictionary — and they land INSIDE a
                      translated sentence, so fmtTime takes the interface
                      locale rather than the browser's. */}
                  {t("history.span", {
                    from: fmtTime(s.firstSeen, locale),
                    to: fmtTime(s.lastSeen, locale),
                    traces: t(`traces.${countForm(locale, s.traceCount)}` as MTRKey, { count: s.traceCount }),
                  })}
                </span>
              </button>
            </li>
          ))}
        </ul>
      ) : null}

      {snapshots.length > 0 ? (
        <div className="mt-4 flex justify-center">
          <Button variant="outline" size="sm" disabled={!hasOlder || loading} onClick={loadOlder}>
            {loading ? t("history.loadingOlder") : t("history.loadOlder")}
          </Button>
        </div>
      ) : null}
    </Pane>
  );
}

/* ── pane 3: the trace detail ──────────────────────────────────────────── */

/** DetailPane owns the FETCH; the by-id read asks for `?enrich=true` — this pane is the ONLY caller that does. */
function DetailPane({
  snapshotId,
  fallback,
  history,
}: {
  snapshotId: string | null;
  fallback: PathSnapshot | null;
  history: TrendHistory;
}) {
  const t = useT(mtrDict);
  const query = useQuery({
    queryKey: ["mtr", "snapshot", snapshotId, "enriched"],
    queryFn: () => getMTRSnapshot(snapshotId as string, true),
    enabled: snapshotId !== null,
  });
  const snapshot = query.data ?? fallback;

  return (
    <Pane title={t("detail.title")}>
      {snapshotId === null ? <EmptyNote>{t("detail.empty")}</EmptyNote> : null}
      {snapshotId !== null && query.isError && !snapshot ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">
          {queryErrorMessage(query.error, t("detail.error"))}
        </p>
      ) : null}
      {snapshotId !== null && !snapshot && !query.isError ? <ListSkeleton /> : null}
      {/* Keyed by snapshot id so picking a different path REMOUNTS the table:
          an expander open on hop 3 and a trend pinned to hop 3 belong to the
          route they were opened on, and carrying them over to another route's
          hop 3 would silently re-point them at a different machine. Same id
          for fallback and fetched copy, so the by-id read is not a remount. */}
      {snapshotId !== null && snapshot ? (
        <TraceDetail key={snapshot.id} snapshot={snapshot} history={history} />
      ) : null}
    </Pane>
  );
}

/* ── pane 3, the other half: the diff ───────────────────────────────────── */

/**
 * DiffPane takes over pane 3 while two paths are ticked; the two are ordered OLDEST first here
 * rather than in the ticking order.
 */
function DiffPane({ snapshots, compare }: { snapshots: PathSnapshot[]; compare: string[] }) {
  const t = useT(mtrDict);
  const picked = compare.map((id) => snapshots.find((s) => s.id === id)).filter((s): s is PathSnapshot => s !== undefined);

  return (
    <Pane title={t("diff.title")}>
      {picked.length < 2 ? (
        <EmptyNote>{t("diff.empty")}</EmptyNote>
      ) : (
        (() => {
          const [a, b] = [...picked].sort((x, y) => new Date(x.firstSeen).getTime() - new Date(y.firstSeen).getTime());
          return <PathDiff a={a} b={b} />;
        })()
      )}
    </Pane>
  );
}

/* ── the Runner ─────────────────────────────────────────────────────────── */

/**
 * RunnerPane is MTR_EXPLORER.md's "Runner tab launches MTR to any node/target/ ad-hoc host"; it is
 * the Diagnostics run form with the check type nailed to `mtr` — same endpoint.
 */
function RunnerPane({ canReadTargets }: { canReadTargets: boolean }) {
  const t = useT(mtrDict);
  const { locale } = useLocale();
  const topo = useTopology();
  /* The union of controller nodes and the node names the AGENTS report. */
  const nodeNames = useMemo(() => scopeNodeOptions(topo.data), [topo.data]);

  const [sourcesAll, setSourcesAll] = useState(true);
  const [sources, setSources] = useState<string[]>([]);
  const [destinationsAll, setDestinationsAll] = useState(true);
  const [destinations, setDestinations] = useState<string[]>([]);
  const [destinationKind, setDestinationKind] = useState<DestinationKind>("node");
  const [destinationTargetId, setDestinationTargetId] = useState("");
  const [destinationAddress, setDestinationAddress] = useState("");
  const [duration, setDuration] = useState("instant");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string>();
  const [startedRunId, setStartedRunId] = useState<string>();
  // The three panes to the left of this one are read-only and stay fully usable while engaged.
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;

  // Same cache entry, same gating as the Diagnostics form's picker: GET
  // /api/v1/targets needs targets:read, so asking without it is a guaranteed
  // 403, and asking before "Target" is picked is a request nobody needs.
  const targetsQuery = useQuery({
    queryKey: ["targets"],
    queryFn: () => listTargets(),
    enabled: canReadTargets && destinationKind === "target",
  });
  const targets = targetsQuery.data?.targets ?? [];

  const durationNs = durationNsFor(duration);
  const request = buildRunRequest({
    type: "mtr",
    sources: sourcesAll ? [] : sources,
    destinations: destinationsAll ? [] : destinations,
    destinationKind,
    destinationTargetId,
    destinationAddress: destinationAddress.trim(),
    durationNs,
  });

  // A target run with nothing picked, or an ad-hoc one with an empty address,
  // is a guaranteed 400 from resolveRunDestination — blocked here rather than
  // sent to collect one.
  const incompleteDestination =
    (destinationKind === "target" && destinationTargetId === "") ||
    (destinationKind === "adhoc" && destinationAddress.trim() === "");

  /* The pair preview and its gate, mirroring the Diagnostics form. */
  const resolvedSources = sourcesAll ? nodeNames : sources;
  const resolvedDestinations = destinationsAll ? nodeNames : destinations;
  const external = destinationKind !== "node";
  /* Zero while the destination side cannot resolve: sources x destinations has
     no second factor yet, and "~10 pairs" for a run the server would refuse is
     a number about nothing (QA scope 4, finding #9). */
  const pairCount = incompleteDestination
    ? 0
    : external
      ? new Set(resolvedSources).size
      : estimatePairCount(resolvedSources, resolvedDestinations);
  // Only once the topology has actually ANSWERED.
  const noPairs = !topo.isPending && pairCount === 0;
  /* Which side is missing — the same four-way split the Diagnostics form makes. */
  const pairsReason: MTRKey | null = !noPairs
    ? null
    : resolvedSources.length === 0
      ? "runner.noPairs"
      : destinationKind === "target" && destinationTargetId === ""
        ? "runner.noTarget"
        : destinationKind === "adhoc" && destinationAddress.trim() === ""
          ? "runner.noAddress"
          : "runner.noDestinations";

  /*
   * ONE clearing point for the whole form — the same treatment, and the same reasoning, as the
   * Diagnostics run form.
   */
  useEffect(() => {
    setSubmitError(undefined);
    setStartedRunId(undefined);
  }, [sourcesAll, destinationsAll, sources, destinations, destinationKind, destinationTargetId, destinationAddress]);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setSubmitError(undefined);
    setStartedRunId(undefined);
    setSubmitting(true);
    try {
      const res = await createRun(request);
      setStartedRunId(res.id);
    } catch (err) {
      // problem+json is the SERVER's refusal, verbatim; only the network-level
      // fallback is the console's own sentence.
      setSubmitError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("runner.submitFailed"));
    }
    setSubmitting(false);
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} aria-label={t("runner.aria")} className="flex max-w-2xl flex-col gap-5">
        <div>
          <h2 className="text-sm font-semibold">{t("runner.title")}</h2>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("runner.body")}</p>
        </div>

        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">{t("runner.duration")}</span>
          {/* The same mechanism the Diagnostics form uses, and it fits MTR
              without a special case: a traced pair is re-traced on the cadence
              and every trace is kept as its own sample. That also feeds path
              history once per trace, so an interval MTR run is the most direct
              way to catch a route that flaps — the very thing a single
              instant trace cannot see. */}
          <Segmented
            aria-label={t("runner.duration.aria")}
            /* RUN_DURATIONS is the Diagnostics form's table and its VALUES are
               what both pages post; only "Instant" is a word, and this surface
               keeps its own copy of it rather than reading another dict. */
            options={RUN_DURATIONS.map((d) => ({
              value: d.value,
              label: d.value === "instant" ? t("runner.duration.instantLabel") : d.label,
            }))}
            value={duration}
            onChange={setDuration}
            className="flex-wrap"
          />
          <p className="mt-2 text-xs text-muted-foreground">
            {durationNs === 0
              ? t("runner.duration.instant")
              : t("runner.duration.interval", {
                  interval: formatDurationNs(sampleIntervalNs(durationNs), locale),
                  label: RUN_DURATIONS.find((d) => d.value === duration)?.label ?? "",
                  samples: plannedSamplesPerPair(durationNs),
                })}
          </p>
        </div>

        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">{t("runner.destination")}</span>
          {/* "Target" only with targets:read — its picker would otherwise have
              nothing to list and its own GET would be a guaranteed 403. */}
          <Segmented
            aria-label={t("runner.destination.aria")}
            options={(["node", "target", "adhoc"] as DestinationKind[])
              .filter((k) => k !== "target" || canReadTargets)
              .map((k) => ({ value: k, label: t(DESTINATION_KIND_KEYS[k]) }))}
            value={destinationKind}
            onChange={setDestinationKind}
          />
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <NodeSelector
            label={t("runner.sources")}
            nodes={nodeNames}
            all={sourcesAll}
            onAllChange={setSourcesAll}
            selected={sources}
            onToggle={(n) => setSources((prev) => toggleName(prev, n))}
          />
          {destinationKind === "node" ? (
            <NodeSelector
              label={t("runner.destinations")}
              nodes={nodeNames}
              all={destinationsAll}
              onAllChange={setDestinationsAll}
              selected={destinations}
              onToggle={(n) => setDestinations((prev) => toggleName(prev, n))}
            />
          ) : null}
          {destinationKind === "target" ? (
            <FieldLabel label={t("runner.destinationTarget")}>
              {(id) => (
                <select
                  id={id}
                  value={destinationTargetId}
                  onChange={(e) => setDestinationTargetId(e.target.value)}
                  className={CONTROL_CLASS}
                >
                  <option value="">{t("runner.destinationTarget.placeholder")}</option>
                  {targets.map((t) => (
                    <option key={t.id} value={t.id}>
                      {t.name}
                    </option>
                  ))}
                </select>
              )}
            </FieldLabel>
          ) : null}
          {destinationKind === "adhoc" ? (
            <FieldLabel label={t("runner.destinationAddress")}>
              {(id) => (
                <input
                  id={id}
                  /* Named explicitly as well as by its <label> — see the Diagnostics twin. */
                  aria-label={t("runner.destinationAddress")}
                  value={destinationAddress}
                  placeholder="10.0.0.1 or example.test"
                  onChange={(e) => setDestinationAddress(e.target.value)}
                  className={CONTROL_CLASS}
                />
              )}
            </FieldLabel>
          ) : null}
        </div>

        {/* The count AND, at zero, the reason — the Diagnostics form's own
            posture: a dead button owes an explanation, and "no sources" is one
            the operator can act on (wire a controller, or wait for an agent to
            register). */}
        <span className={cn("nums text-sm", noPairs ? "text-health-bad" : "text-muted-foreground")}>
          {t(`runner.pairs.${countForm(locale, pairCount)}` as MTRKey, { count: pairCount })}
          {pairsReason ? t(pairsReason) : ""}
        </span>

        {submitError ? (
          <p role="alert" className="text-sm text-health-bad">
            {submitError}
          </p>
        ) : null}

        <Button
          type="submit"
          loading={submitting}
          {...guard} disabled={noPairs || incompleteDestination || writesDisabled}
          className="self-start"
        >
          {t("runner.submit")}
        </Button>

        {startedRunId ? (
          <p role="status" className="text-sm">
            {/* Three keys: the link is INSIDE the sentence. */}
            {t("runner.started.before")}{" "}
            <a href={`/diagnostics/runs/${startedRunId}`} className="text-primary hover:underline">
              {t("runner.started.link")}
            </a>
            {t("runner.started.after")}
          </p>
        ) : null}
      </form>
    </Card>
  );
}

/* ── the page ───────────────────────────────────────────────────────────── */

/**
 * MTRPage is /mtr: MTR_EXPLORER.md's three panes — the destinations path history knows about; three
 * degraded states are DESIGNED here rather than left to fall out of failing requests.
 */
export function MTRPage() {
  const t = useT(mtrDict);
  const { locale } = useLocale();
  const { at } = useTimeContext();
  const { me, can } = useAuth();
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const [pair, setPair] = useState<Pair | null>(null);
  const [snapshot, setSnapshot] = useState<PathSnapshot | null>(null);
  // Kept next to the pair rather than re-derived, because the loaded snapshots cannot tell you what
  // has not been loaded.
  const [traceTotal, setTraceTotal] = useState<number | null>(null);
  // The two snapshot ids ticked for a diff, in tick order (toggleCompare owns
  // the rule). Ids rather than rows, so a "Load older" that re-renders the
  // list cannot leave the diff holding two stale copies.
  const [compare, setCompare] = useState<string[]>([]);
  const [view, setView] = useState<"explorer" | "runner">("explorer");
  const history = useSnapshotHistory(pair);

  const authResolved = me !== undefined;
  const canCreate = can("runs:create");

  // Selecting a different pair drops the open trace AND the comparison.
  const selectPair = useCallback((row: MTRDestination) => {
    const next = { source: row.sourceNode, destination: row.destination };
    setPair((prev) =>
      prev && prev.source === next.source && prev.destination === next.destination ? prev : next,
    );
    setTraceTotal(row.traceCount);
    setSnapshot(null);
    setCompare([]);
  }, []);

  const onToggleCompare = useCallback((id: string) => setCompare((prev) => toggleCompare(prev, id)), []);

  const trendHistory = useMemo<TrendHistory>(
    () => ({ snapshots: history.snapshots, hasOlder: history.hasOlder, traceTotal }),
    [history.snapshots, history.hasOlder, traceTotal],
  );

  let body: ReactNode;
  if (!authResolved || !dbResolved) {
    body = (
      <Card role="status" aria-live="polite" className="p-6">
        <span className="sr-only">{t("loading")}</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else if (!can("mtr:read")) {
    body = <PermissionCard permission="mtr:read">{t("permission.body")}</PermissionCard>;
  } else if (!dbAvailable) {
    body = (
      <Card role="status" className="p-6">
        <p className="text-sm">{t("database.gate")}</p>
      </Card>
    );
  } else {
    body = (
      <div className="flex flex-col gap-4">
        {/* The Runner is a SEGMENT rather than a fourth column: it is a verb,
            not another view of the same data, and the three panes need every
            pixel of the row they already have. Without runs:create there is no
            segment at all — the reader is not shown a tab they cannot open. */}
        {canCreate ? (
          <Segmented
            aria-label={t("view.aria")}
            options={[
              { value: "explorer", label: t("view.explorer") },
              { value: "runner", label: t("view.runner") },
            ]}
            value={view}
            onChange={setView}
            className="self-start"
          />
        ) : null}

        {canCreate && view === "runner" ? (
          <RunnerPane canReadTargets={can("targets:read")} />
        ) : (
          <>
            {/* The Explorer is LIVE under a banner that says otherwise, and it
                stays live because none of the reads behind it takes a time
                parameter. That is a disclosure, not a footnote: /diagnostics
                prints the same kind of line over its history list rather than
                letting the banner speak for data it does not govern. */}
            {at ? (
              <p role="status" className="max-w-prose text-xs leading-relaxed text-muted-foreground">
                {t("explorer.atNote")}
              </p>
            ) : null}
            <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)_minmax(0,1.6fr)]">
            <DestinationsPane selected={pair} onSelect={selectPair} />
            <HistoryPane
              pair={pair}
              history={history}
              selectedId={snapshot?.id ?? null}
              onSelectSnapshot={setSnapshot}
              compare={compare}
              onToggleCompare={onToggleCompare}
            />
            {/* Two ticked paths take pane 3 over: a diff and a single trace
                answer different questions, and showing both would leave the
                reader guessing which one the hop numbers belong to. */}
            {compare.length === 2 ? (
              <DiffPane snapshots={history.snapshots} compare={compare} />
            ) : (
              <DetailPane snapshotId={snapshot?.id ?? null} fallback={snapshot} history={trendHistory} />
            )}
            </div>
          </>
        )}
      </div>
    );
  }

  return (
    <PageShell
      title={t("title")}
      /* {at} lands INSIDE a translated sentence, so it takes that sentence's
         language — lib/i18n's localeTag, same as /diagnostics and /explore. */
      description={at ? t("description.at", { at: at.toLocaleString(localeTag(locale)) }) : t("description")}
    >
      {body}
    </PageShell>
  );
}
