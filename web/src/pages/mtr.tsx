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
import { useWritesDisabled } from "@/lib/timemachine";
import type { DestinationKind, MTRDestination, PathSnapshot } from "@/lib/types";
import { cn } from "@/lib/utils";
/* The Runner below builds the SAME POST /api/v1/runs body the Diagnostics run
   form does, from the same controls. Task 8's brief allows a mechanical export
   change over there rather than a second copy of any of this here — see the
   export note in diagnostics.tsx. */
import {
  CONTROL_CLASS,
  DESTINATION_KIND_LABELS,
  FieldLabel,
  NodeSelector,
  buildRunRequest,
  toggleName,
} from "./diagnostics";

/* The hop table moved into its own component when this page passed ~800 lines
   (M5 Task 7's own instruction). Every name Task 6 exported from here is
   re-exported so /mtr keeps ONE import site for its pieces and nothing that
   already depends on them has to learn where they live now. shortHash joined
   them in Task 8: the diff table and the changes timeline label snapshots with
   it too, and a component importing this page back would be a cycle. */
export { fmtRttNs, shortHash, TraceDetail } from "@/components/mtr-hop-table";

/* ── pure helpers (exported for their own tests) ────────────────────────── */

/** A Pair is what pane 1 hands pane 2: the two filters GET
 *  /api/v1/mtr/snapshots refuses to work without. */
export interface Pair {
  source: string;
  destination: string;
}

/** DestinationGroup is one destination with every source that has traced it.
 *  `snapshotCount` is the sum over the group's sources and `lastSeen` the
 *  newest of them — a group header states the group's own truth, not the
 *  first member's. */
export interface DestinationGroup {
  destination: string;
  sources: MTRDestination[];
  snapshotCount: number;
  lastSeen: string;
}

/**
 * groupDestinations turns the flat list GET /api/v1/mtr/destinations answers
 * into MTR_EXPLORER.md's pane 1 shape: destinations with their sources
 * nested. The grouping is client-side deliberately — the server's row is the
 * (source, destination) pair, which is also the unit pane 2 filters on, so
 * flattening on the wire and nesting for display keeps ONE shape on the API
 * and none of the presentation in the store.
 *
 * Encounter order is preserved on both levels rather than re-sorted: the
 * server already answers most-recently-traced first, and re-sorting here
 * would quietly override an ordering the API documents.
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

/** msOf compares two wire timestamps as instants, not as strings: the server
 *  emits RFC 3339 with whatever offset it is running in, so "2026-08-07T00:00:00+02:00"
 *  and "2026-08-06T23:00:00Z" are the SAME moment and a lexical compare would
 *  get it backwards. An unparseable value sorts oldest rather than throwing. */
function msOf(ts: string): number {
  const ms = new Date(ts).getTime();
  return Number.isNaN(ms) ? -Infinity : ms;
}

/**
 * pathChangeFlags marks, index-aligned with `snapshots`, every row whose path
 * differs from the NEXT-OLDER row's. The list arrives newest-first (the
 * store's (source, destination, last_seen DESC, id DESC) index), so the
 * next-older row is simply `i + 1`, and the LAST row is never flagged: it is
 * the oldest route this page knows about, i.e. where the pair started, not a
 * change away from something.
 *
 * The wire truth worth knowing: within one pair, `path_hash` is UNIQUE
 * (mtr_path_snapshots_pair_hash), so on a single-pair page every row but the
 * oldest is flagged. This is still written as a comparison rather than as
 * "flag everything except the last" on purpose — the comparison is what the
 * badge MEANS, it stays correct if a future page ever mixes pairs or replays
 * a repeated hash, and it is the same next-older pairing Task 8's diff picks
 * its two snapshots from.
 */
export function pathChangeFlags(snapshots: PathSnapshot[]): boolean[] {
  return snapshots.map((s, i) => {
    const older = snapshots[i + 1];
    return older !== undefined && older.pathHash !== s.pathHash;
  });
}

/**
 * toggleCompare is the two-snapshot selection rule, as a pure function.
 *
 * A diff has exactly two sides, so the list is capped at two — and the third
 * pick does NOT get refused (a checkbox that silently declines to tick is a
 * control that lies). It drops the OLDEST pick and keeps the newest two, which
 * makes "walk down the history ticking rows" a working gesture: each tick
 * compares against the row you ticked just before. Un-ticking removes, and the
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

/** PermissionCard is PAGES.md:126-129's pattern, the same component
 *  targets.tsx and target-card.tsx already use: name the permission, say what
 *  the reader CAN still do, and never render a disabled control in place of
 *  one they simply do not have. */
function PermissionCard({ permission, children }: { permission: string; children: ReactNode }) {
  return (
    <Card role="status" className="p-6">
      <p className="text-sm font-medium">Requires the {permission} permission</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
}

function EmptyNote({ children }: { children: ReactNode }) {
  return <p className="px-1 py-10 text-center text-xs leading-relaxed text-muted-foreground">{children}</p>;
}

function ListSkeleton() {
  return (
    <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
      <span className="sr-only">Loading…</span>
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
  const query = useQuery({ queryKey: ["mtr", "destinations"], queryFn: getMTRDestinations });
  const groups = useMemo(() => groupDestinations(query.data?.destinations ?? []), [query.data]);

  return (
    <Pane title="Destinations">
      {query.isError ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">
          {queryErrorMessage(query.error, "Path history is unavailable")}
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
          Nothing traced yet.{" "}
          <a href="/diagnostics" className="text-primary hover:underline">
            Run an MTR from Diagnostics
          </a>{" "}
          — its path lands here.
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
                  {group.snapshotCount} {group.snapshotCount === 1 ? "path" : "paths"}
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
                        <span className="truncate">from {row.sourceNode}</span>
                        <span className="nums shrink-0 text-muted-foreground">
                          {row.snapshotCount} · {row.traceCount} traces
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
 * SnapshotHistory is the pair's loaded routes plus everything two panes need
 * to say about them. It is a HOOK rather than pane-2 state because pane 3's
 * per-hop trend is drawn from these very snapshots (Decision 13: hop RTTs are
 * not in Prometheus), so the pages the reader has loaded are shared data, not
 * one pane's private business. Nothing extra is fetched for the chart.
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
 * useSnapshotHistory loads the pair's distinct routes, newest first, behind the
 * same opaque keyset cursor run history and event scrollback already use — and
 * the same way (pages/diagnostics.tsx's loadRuns is the convention this
 * mirrors, not TanStack's useInfiniteQuery, so the whole repo has one
 * pagination shape).
 *
 * `nextCursor` doubles as "there is nothing more to load" (an exhausted page
 * answers "") and "nothing has loaded yet" (the initial value, also ""), which
 * is the right default for a Load-older button that has not yet heard back.
 *
 * A selection change RESETS to page one rather than appending: the cursor
 * belongs to the pair it was minted for, and pane 2 showing two pairs' routes
 * interleaved would be a lie about what changed.
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
  const { snapshots, loading, error, hasOlder, loadOlder } = history;
  const changed = useMemo(() => pathChangeFlags(snapshots), [snapshots]);

  return (
    <Pane title="Path history">
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

      {!pair ? <EmptyNote>Pick a source on the left to see the routes it has taken.</EmptyNote> : null}

      {error ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">
          {queryErrorMessage(error, "Path history is unavailable")}
        </p>
      ) : null}

      {pair && loading && snapshots.length === 0 ? <ListSkeleton /> : null}

      {pair && !loading && !error && snapshots.length === 0 ? (
        <EmptyNote>No path recorded for this pair yet.</EmptyNote>
      ) : null}

      {snapshots.length > 0 ? (
        <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
          Tick two paths to diff them — a third pick replaces the earlier of the two.
        </p>
      ) : null}

      {snapshots.length > 0 ? (
        <ul aria-label="Paths" className="mt-1 divide-y divide-border">
          {snapshots.map((s, i) => (
            <li key={s.id} className="flex items-center gap-2">
              {/* Outside the row button, not inside it: a checkbox nested in a
                  button is neither valid HTML nor operable by keyboard, and
                  ticking one must not also change which trace pane 3 shows. */}
              <input
                type="checkbox"
                aria-label={`Compare path ${shortHash(s.pathHash)}`}
                checked={compare.includes(s.id)}
                onChange={() => onToggleCompare(s.id)}
                className="size-4 shrink-0 rounded border-border-strong"
              />
              <button
                type="button"
                aria-pressed={s.id === selectedId}
                aria-label={`Path ${shortHash(s.pathHash)}`}
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
                  <Badge variant="neutral">{s.hopCount} hops</Badge>
                  {changed[i] ? <Badge variant="warn">path changed</Badge> : null}
                </span>
                <span className="nums text-xs text-muted-foreground">
                  {fmtTime(s.firstSeen)} → {fmtTime(s.lastSeen)} · {s.traceCount} traces
                </span>
              </button>
            </li>
          ))}
        </ul>
      ) : null}

      {snapshots.length > 0 ? (
        <div className="mt-4 flex justify-center">
          <Button variant="outline" size="sm" disabled={!hasOlder || loading} onClick={loadOlder}>
            {loading ? "Loading older…" : "Load older"}
          </Button>
        </div>
      ) : null}
    </Pane>
  );
}

/* ── pane 3: the trace detail ──────────────────────────────────────────── */

/**
 * DetailPane owns the FETCH; TraceDetail owns the rendering.
 *
 * The by-id read asks for `?enrich=true` — this pane is the ONLY caller that
 * does, which is why the flag is a call-site decision and not a default in
 * lib/api.ts. Server-side that turns on a TTL-cached lookup (Decision 4), so
 * the query key carries the flag too: an un-enriched copy of the same snapshot
 * must never satisfy a read that wants the enrichment map, and the map's
 * ABSENCE is the wire's way of saying "you did not ask".
 *
 * The row that was clicked is already a complete PathSnapshot — the list
 * endpoint ships the full hop payload — so `fallback` renders instantly and
 * the by-id read is what makes the pane authoritative rather than what makes
 * it appear. (The fallback carries no enrichment; the expanders are collapsed
 * by default, so the enriched answer is in hand before anyone opens one.)
 *
 * `history` is passed straight through to the trend: the snapshots pane 2 has
 * already loaded are the trend's whole data source (Decision 13), and its
 * `traceTotal` — the pair's lifetime trace count from the destinations list —
 * is what lets the chart admit how narrow its window is.
 */
function DetailPane({
  snapshotId,
  fallback,
  history,
}: {
  snapshotId: string | null;
  fallback: PathSnapshot | null;
  history: TrendHistory;
}) {
  const query = useQuery({
    queryKey: ["mtr", "snapshot", snapshotId, "enriched"],
    queryFn: () => getMTRSnapshot(snapshotId as string, true),
    enabled: snapshotId !== null,
  });
  const snapshot = query.data ?? fallback;

  return (
    <Pane title="Trace detail">
      {snapshotId === null ? <EmptyNote>Pick a path in the history to see its hops.</EmptyNote> : null}
      {snapshotId !== null && query.isError && !snapshot ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">
          {queryErrorMessage(query.error, "This path is unavailable")}
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
 * DiffPane takes over pane 3 while two paths are ticked. It needs NO fetch of
 * its own: the list endpoint ships each snapshot's full hop payload, which is
 * exactly what Decision 3 rests on — a server-side diff endpoint would
 * duplicate this table's presentation logic for zero authority gain.
 *
 * The two are ordered OLDEST first here rather than in the ticking order, so
 * the table always reads forwards in time and "+" always means "the newer path
 * gained this hop". Ties (two snapshots stamped identically, which the store's
 * unique pair+hash constraint does not forbid) fall back to the order they
 * appear in the list, i.e. newest-first — the same order the reader sees.
 */
function DiffPane({ snapshots, compare }: { snapshots: PathSnapshot[]; compare: string[] }) {
  const picked = compare.map((id) => snapshots.find((s) => s.id === id)).filter((s): s is PathSnapshot => s !== undefined);

  return (
    <Pane title="Path diff">
      {picked.length < 2 ? (
        <EmptyNote>Both paths must still be loaded to diff them.</EmptyNote>
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
 * RunnerPane is MTR_EXPLORER.md's "Runner tab launches MTR to any node/target/
 * ad-hoc host". It is the Diagnostics run form with the check type nailed to
 * `mtr` — same endpoint, same body builder, same controls, imported rather
 * than re-typed.
 *
 * Two deliberate differences from the Diagnostics form:
 *
 *  - it does NOT navigate on 202. An operator who launches a trace from here
 *    is in the middle of reading a pair's history; throwing them onto the run
 *    page would cost them that context, so the started run is offered as a
 *    link and the explorer stays put.
 *  - no "Save as definition". A definition is a repeating probe and belongs to
 *    the Definitions tab; this page is about one route, right now.
 *
 * Rendered only with runs:create (the endpoint's own permission, Decision 11 —
 * there is no mtr-specific launch permission), and ABSENT rather than disabled
 * without it: the whole Runner segment disappears, so nobody is shown a
 * control they cannot use.
 */
function RunnerPane({ canReadTargets }: { canReadTargets: boolean }) {
  const topo = useTopology();
  const nodeNames = useMemo(() => topo.data?.nodes?.map((n) => n.name) ?? [], [topo.data]);

  const [sourcesAll, setSourcesAll] = useState(true);
  const [sources, setSources] = useState<string[]>([]);
  const [destinationsAll, setDestinationsAll] = useState(true);
  const [destinations, setDestinations] = useState<string[]>([]);
  const [destinationKind, setDestinationKind] = useState<DestinationKind>("node");
  const [destinationTargetId, setDestinationTargetId] = useState("");
  const [destinationAddress, setDestinationAddress] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string>();
  const [startedRunId, setStartedRunId] = useState<string>();
  // The three panes to the left of this one are read-only and stay fully usable
  // while engaged — path history is inherently historical, and its detail/diff
  // views need no anchoring at all. The Runner is the page's one MUTATION, and
  // it is the one thing time takes away.
  const writesDisabled = useWritesDisabled();

  // Same cache entry, same gating as the Diagnostics form's picker: GET
  // /api/v1/targets needs targets:read, so asking without it is a guaranteed
  // 403, and asking before "Target" is picked is a request nobody needs.
  const targetsQuery = useQuery({
    queryKey: ["targets"],
    queryFn: () => listTargets(),
    enabled: canReadTargets && destinationKind === "target",
  });
  const targets = targetsQuery.data?.targets ?? [];

  const request = buildRunRequest({
    type: "mtr",
    sources: sourcesAll ? [] : sources,
    destinations: destinationsAll ? [] : destinations,
    destinationKind,
    destinationTargetId,
    destinationAddress: destinationAddress.trim(),
  });

  // A target run with nothing picked, or an ad-hoc one with an empty address,
  // is a guaranteed 400 from resolveRunDestination — blocked here rather than
  // sent to collect one.
  const incompleteDestination =
    (destinationKind === "target" && destinationTargetId === "") ||
    (destinationKind === "adhoc" && destinationAddress.trim() === "");

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setSubmitError(undefined);
    setStartedRunId(undefined);
    setSubmitting(true);
    try {
      const res = await createRun(request);
      setStartedRunId(res.id);
    } catch (err) {
      setSubmitError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to start the trace");
    }
    setSubmitting(false);
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} aria-label="Run a trace" className="flex max-w-2xl flex-col gap-5">
        <div>
          <h2 className="text-sm font-semibold">Run an MTR</h2>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
            The same POST /api/v1/runs the Diagnostics page uses, with the check type fixed to mtr. Every path it
            produces lands in this page's history.
          </p>
        </div>

        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">Destination</span>
          {/* "Target" only with targets:read — its picker would otherwise have
              nothing to list and its own GET would be a guaranteed 403. */}
          <Segmented
            aria-label="Destination"
            options={(["node", "target", "adhoc"] as DestinationKind[])
              .filter((k) => k !== "target" || canReadTargets)
              .map((k) => ({ value: k, label: DESTINATION_KIND_LABELS[k] }))}
            value={destinationKind}
            onChange={setDestinationKind}
          />
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <NodeSelector
            label="Sources"
            nodes={nodeNames}
            all={sourcesAll}
            onAllChange={setSourcesAll}
            selected={sources}
            onToggle={(n) => setSources((prev) => toggleName(prev, n))}
          />
          {destinationKind === "node" ? (
            <NodeSelector
              label="Destinations"
              nodes={nodeNames}
              all={destinationsAll}
              onAllChange={setDestinationsAll}
              selected={destinations}
              onToggle={(n) => setDestinations((prev) => toggleName(prev, n))}
            />
          ) : null}
          {destinationKind === "target" ? (
            <FieldLabel label="Destination target">
              {(id) => (
                <select
                  id={id}
                  value={destinationTargetId}
                  onChange={(e) => setDestinationTargetId(e.target.value)}
                  className={CONTROL_CLASS}
                >
                  <option value="">— pick a target —</option>
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
            <FieldLabel label="Destination address">
              {(id) => (
                <input
                  id={id}
                  value={destinationAddress}
                  placeholder="10.0.0.1 or example.test"
                  onChange={(e) => setDestinationAddress(e.target.value)}
                  className={CONTROL_CLASS}
                />
              )}
            </FieldLabel>
          ) : null}
        </div>

        {submitError ? (
          <p role="alert" className="text-sm text-health-bad">
            {submitError}
          </p>
        ) : null}

        <Button
          type="submit"
          loading={submitting}
          disabled={incompleteDestination || writesDisabled}
          className="self-start"
        >
          Start MTR
        </Button>

        {startedRunId ? (
          <p role="status" className="text-sm">
            Run started —{" "}
            <a href={`/diagnostics/runs/${startedRunId}`} className="text-primary hover:underline">
              watch it here
            </a>
            . Its path lands in the history on the Explorer tab once the run finishes.
          </p>
        ) : null}
      </form>
    </Card>
  );
}

/* ── the page ───────────────────────────────────────────────────────────── */

/**
 * MTRPage is /mtr: MTR_EXPLORER.md's three panes — the destinations path
 * history knows about, the distinct routes the selected pair has taken, and
 * one route's hops.
 *
 * Three degraded states are DESIGNED here rather than left to fall out of
 * failing requests, in the M4 house pattern (pages/targets.tsx):
 *
 *  1. NO mtr:read — one permission card, ZERO requests. Note how much rarer
 *     this is than M4's equivalent: mtr:read is held by every BUILT-IN role,
 *     viewer included (M5 Decision 11 — path history is telemetry, not
 *     configuration, so M4 Decision 3 deliberately does not apply), and viewer
 *     is what auth.anonymous.role defaults to. Reaching this card means a
 *     hand-rolled role, which is exactly what the copy says, so the reader
 *     goes looking in the right place.
 *
 *  2. database.mode=disabled — one honest line naming console.database.mode
 *     and NO request at all, rather than three requests to collect three
 *     503s. Snapshots live in mtr_path_snapshots; with no store there is no
 *     projection to read. Derived from GET /api/v1/config's
 *     `database.configured`, the same gate the handlers' own 503 reads.
 *     This is the COMMON degraded state for this page.
 *
 *     Order matters, same as on /targets: the permission card comes first,
 *     because "you cannot see this" is about the subject and stays true
 *     regardless of how the console is deployed.
 *
 *  3. mtr:read, a database, and no traces — the empty state points at
 *     Diagnostics, because path history is a projection of MTR results the
 *     console ingested and the fix is to produce one. (Not at this page's own
 *     Runner tab: that arrives in Task 8.)
 *
 * Both gates wait for their answer before deciding. `can()` fails closed
 * while GET /api/v1/auth/me is in flight and `available` is false before
 * /api/v1/config lands, so rendering on the un-resolved value would flash the
 * permission card on every cold load.
 */
export function MTRPage() {
  const { me, can } = useAuth();
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const [pair, setPair] = useState<Pair | null>(null);
  const [snapshot, setSnapshot] = useState<PathSnapshot | null>(null);
  // The selected pair's LIFETIME trace count, straight off the destinations
  // row. Kept next to the pair rather than re-derived, because the loaded
  // snapshots cannot tell you what has not been loaded: this number is the
  // only thing that lets pane 3 say "3 of 40 traces" instead of implying the
  // trend is the whole story.
  const [traceTotal, setTraceTotal] = useState<number | null>(null);
  // The two snapshot ids ticked for a diff, in tick order (toggleCompare owns
  // the rule). Ids rather than rows, so a "Load older" that re-renders the
  // list cannot leave the diff holding two stale copies.
  const [compare, setCompare] = useState<string[]>([]);
  const [view, setView] = useState<"explorer" | "runner">("explorer");
  const history = useSnapshotHistory(pair);

  const authResolved = me !== undefined;
  const canCreate = can("runs:create");

  // Selecting a different pair drops the open trace AND the comparison: a hop
  // table belonging to the pair the reader just navigated away from is the
  // worst kind of stale, and two different pairs' routes are not comparable at
  // all — they do not even share a destination.
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
        <span className="sr-only">Loading…</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else if (!can("mtr:read")) {
    body = (
      <PermissionCard permission="mtr:read">
        Path history is telemetry, and every built-in role holds this permission — viewer included, which is the role an
        anonymous session gets. Seeing this card means the role in use was defined by hand without it; ask an admin to
        add mtr:read to it.
      </PermissionCard>
    );
  } else if (!dbAvailable) {
    body = (
      <Card role="status" className="p-6">
        <p className="text-sm">Path history is projected into the database — set console.database.mode</p>
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
            aria-label="View"
            options={[
              { value: "explorer", label: "Explorer" },
              { value: "runner", label: "Runner" },
            ]}
            value={view}
            onChange={setView}
            className="self-start"
          />
        ) : null}

        {canCreate && view === "runner" ? (
          <RunnerPane canReadTargets={can("targets:read")} />
        ) : (
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
        )}
      </div>
    );
  }

  return (
    <PageShell
      title="MTR Explorer"
      description="Every distinct route the fleet's traces have taken, and when each one changed."
    >
      {body}
    </PageShell>
  );
}
