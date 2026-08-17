import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight } from "lucide-react";
import { PathChangesTimeline } from "@/components/mtr-changes-timeline";
import { PathChain, TraceDetail, fmtTime, shortHash, type TrendHistory } from "@/components/mtr-hop-table";
import { TraceList } from "@/components/mtr-trace-list";
import { PathDiff, summarisePathChange } from "@/components/mtr-path-diff";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Modal } from "@/components/ui/modal";
import { Pager, usePager } from "@/components/ui/pager";
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
  listAllTargets,
} from "@/lib/api";
import { localeTag, useLocale, useT, type Translate } from "@/lib/i18n";
import { countForm, mtrDict, type MTRKey } from "@/lib/i18n/dict/mtr";
import { scopeNodeOptions } from "@/lib/investigation-sources";
import { compareNaturalName } from "@/lib/natural-name";
import { pageOfIndex } from "@/lib/pagination";
import { planCadenceFor } from "@/lib/run-samples";
import { withAtParam, useTimeContext, useWriteGuard } from "@/lib/timemachine";
import type { DestinationKind, MTRDestination, PathSnapshot } from "@/lib/types";
import { CHECKBOX_CLASS, cn } from "@/lib/utils";
/* The Runner below builds the SAME POST /api/v1/runs body the Diagnostics run form does, from the same controls. */
import {
  CONTROL_CLASS,
  FieldLabel,
  NodeSelector,
  buildRunRequest,
  cadenceCaption,
  durationNsFor,
  estimatePairCount,
  RUN_DURATIONS,
  sampleIntervalNsFor,
  sampleIntervalOptionsFor,
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

/** DestinationGroup is one destination with every source that has traced it.
 *  Both totals are carried because both are what a SHUT card claims. */
export interface DestinationGroup {
  destination: string;
  sources: MTRDestination[];
  snapshotCount: number;
  traceCount: number;
  lastSeen: string;
}

/**
 * groupDestinations turns the flat list GET /api/v1/mtr/destinations answers into MTR_EXPLORER.md's
 * pane 1 shape.
 *
 * Both levels are sorted BY NAME. The endpoint answers newest-traced first, which reads as no order
 * at all in a pane the operator scans for a node he already has in mind (owner report), and it also
 * reshuffles the list — and with it the pages — every time a trace lands.
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
        traceCount: row.traceCount,
        lastSeen: row.lastSeen,
      });
      continue;
    }
    group.sources.push(row);
    group.snapshotCount += row.snapshotCount;
    group.traceCount += row.traceCount;
    if (msOf(row.lastSeen) > msOf(group.lastSeen)) group.lastSeen = row.lastSeen;
  }
  const groups = [...byDestination.values()];
  for (const group of groups) group.sources.sort((a, b) => compareNaturalName(a.sourceNode, b.sourceNode));
  return groups.sort((a, b) => compareNaturalName(a.destination, b.destination));
}

/* This page's own URL key, carried the way pages/matrix.tsx carries ?protocol=
   — TanStack Router owns navigation here but no route declares a search schema. */
const DESTINATION_PARAM = "destination";
const SOURCE_PARAM = "source";

/**
 * deepLinkDestination is the destination a shared /mtr link names, and the one
 * card that opens on arrival. Null for a plain link and for a parameter that
 * names nothing — an empty string would match no group and is not a wish.
 */
export function deepLinkDestination(search: string): string | null {
  const raw = new URLSearchParams(search).get(DESTINATION_PARAM);
  return raw ? raw : null;
}

/**
 * deepLinkSource is the source half of a pair link (run-detail's "Open in MTR
 * Explorer"). Present only when the link names a specific pair; a
 * destination-only link leaves it null and the card just opens.
 */
export function deepLinkSource(search: string): string | null {
  const raw = new URLSearchParams(search).get(SOURCE_PARAM);
  return raw ? raw : null;
}

/**
 * destinationPage answers which page of the destination list holds `destination`.
 *
 * A deep link that opened a card on page three while the reader sat on page one
 * would open nothing they could see, so the pane turns to this page once, when
 * the groups first arrive. Null when the list does not hold the name at all —
 * a stale link is left alone rather than paged somewhere on a guess.
 */
export function destinationPage(
  groups: readonly DestinationGroup[],
  destination: string | null,
  size: number,
): number | null {
  if (!destination) return null;
  const index = groups.findIndex((g) => g.destination === destination);
  return index === -1 ? null : pageOfIndex(index, size);
}

/**
 * cardOpen answers "is this destination's card open", and it is a function
 * rather than a lookup because a destination is a NAME.
 *
 * `open` is a plain object keyed by that name, so a destination called
 * "constructor" or "toString" — a legal hostname, and one a hostile operator can
 * arrange — used to read its own value off Object.prototype. The function that
 * came back is truthy, so the card rendered permanently expanded, and React
 * dropped the function-valued `aria-expanded` entirely, leaving the disclosure
 * button with no state at all (hostile-QA probe B). Object.hasOwn asks the only
 * question that was ever meant: did the READER say something about this card.
 */
function cardOpen(open: Record<string, boolean>, destination: string, derived: boolean): boolean {
  return Object.hasOwn(open, destination) ? open[destination] : derived;
}

/**
 * mergeSnapshots appends a "Load older" page to what is already on screen,
 * keeping the FIRST copy of any id that arrives twice.
 *
 * The cursor walks (last_seen DESC, id DESC), so a path re-traced between two
 * clicks can legitimately be handed out on both pages. Appending blind rendered
 * it twice under one React key, which React answers with a duplicate-key warning
 * and an explicit promise that it may duplicate or omit the row (hostile-QA
 * probe I).
 */
export function mergeSnapshots(prev: PathSnapshot[], next: PathSnapshot[]): PathSnapshot[] {
  const seen = new Set(prev.map((s) => s.id));
  const merged = [...prev, ...next.filter((s) => !seen.has(s.id))];
  /* Re-sorted, not appended. The store's cursor walks (last_seen DESC, id DESC)
     and last_seen is bumped by every repeat trace, so a route re-traced between
     two clicks can come back NEWER than rows already on screen — and this list
     is read as newest-first by pathChangeFlags, which would then narrate the
     wrong neighbour. Restoring the server's own order costs one sort per page. */
  return merged.sort((a, b) => msOf(b.lastSeen) - msOf(a.lastSeen) || (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
}

/** hopCountOf trusts the payload it can VERIFY. `hopCount` is the stored
 *  column and the hops are the stored path; when the first is missing the
 *  second still knows the answer, and "undefined hops" — which is what the
 *  badge printed (hostile-QA probe D) — is not an answer. */
export function hopCountOf(snapshot: PathSnapshot): number {
  if (typeof snapshot.hopCount === "number" && Number.isFinite(snapshot.hopCount)) return snapshot.hopCount;
  return Array.isArray(snapshot.hops) ? snapshot.hops.length : 0;
}

/** finiteCount keeps a missing wire number out of a translated sentence: the
 *  count forms interpolate whatever they are handed, so `undefined` arrives on
 *  screen as the word. */
export function finiteCount(n: number | undefined): number {
  return typeof n === "number" && Number.isFinite(n) ? n : 0;
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

/**
 * changeText says what moved between a snapshot and the next-older one, in
 * words. The list arrives newest-first, so the older path is at `i + 1`.
 *
 * "path changed" beside two hashes told a reader that something moved and
 * nothing about what; this is that badge, with the answer in it.
 */
function changeText(snapshots: PathSnapshot[], i: number, t: Translate<MTRKey>): string {
  const older = snapshots[i + 1];
  const summary = older ? summarisePathChange(older.hops, snapshots[i].hops) : undefined;
  if (!summary || summary.kind === "same") return t("history.changed");
  if (summary.kind === "several") return t("history.changed.several", { count: summary.total });
  if (summary.kind === "added") return t("history.changed.added", { hop: summary.hop ?? 0, to: summary.to ?? "" });
  if (summary.kind === "removed") {
    return t("history.changed.removed", { hop: summary.hop ?? 0, from: summary.from ?? "" });
  }
  return t("history.changed.moved", {
    hop: summary.hop ?? 0,
    from: summary.from ?? "",
    to: summary.to ?? "",
  });
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
/**
 * Pane is one column of the Explorer. `embedded` is the same content rendered INSIDE a Modal, which
 * already draws the frame and says the title in its own header — repeating both there would put a
 * card in a card under a heading said twice.
 */
function Pane({ title, children, embedded }: { title: string; children: ReactNode; embedded?: boolean }) {
  if (embedded) return <section aria-label={title}>{children}</section>;
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

/**
 * DestinationCard is ONE destination, shut until asked.
 *
 * The owner's report: every card arrived expanded, so a fleet where fifty nodes
 * trace one address met the reader as fifty rows of «from …» before the second
 * destination even started. Shut, the card is a name and the two figures that
 * say how much is behind it; open, it is the same list it always was, in the
 * console's own pages.
 *
 * A component per card rather than a loop, because each card owns a pager and a
 * hook cannot live in a loop.
 */
function DestinationCard({
  group,
  open,
  onToggle,
  selected,
  onSelect,
}: {
  group: DestinationGroup;
  open: boolean;
  onToggle: () => void;
  selected: Pair | null;
  onSelect: (row: MTRDestination) => void;
}) {
  const t = useT(mtrDict);
  const { locale } = useLocale();
  /* The card's own page, reset when the card becomes a different destination. */
  const pager = usePager(group.sources, { resetKey: group.destination });
  const listId = `mtr-destination-${encodeURIComponent(group.destination)}`;
  const pathCount = finiteCount(group.snapshotCount);
  const traceCount = finiteCount(group.traceCount);
  const paths = t(`paths.${countForm(locale, pathCount)}` as MTRKey, { count: pathCount });
  const traces = t(`traces.${countForm(locale, traceCount)}` as MTRKey, { count: traceCount });

  return (
    <div>
      {/* The heading WRAPS the control rather than sitting beside it: the whole
          header is the affordance, and the outline stays one thing to tab to.
          aria-controls names the list it opens — components/mtr-hop-table.tsx
          set this shape's bar and pages/alerting.tsx follows it. */}
      <h3>
        <button
          type="button"
          aria-expanded={open}
          aria-controls={listId}
          /* SPELT OUT rather than left to name-from-contents. A name computed
             from two adjacent spans arrives welded — «node-b2 paths» — which is
             the same defect the source rows had, and a decorative separator
             cannot fix it because aria-hidden text is excluded from the name. */
          aria-label={t("destinations.card.aria", { destination: group.destination, paths, traces })}
          onClick={onToggle}
          className={cn(
            "flex w-full items-center gap-2 rounded-md px-1 py-1 text-left",
            "transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          )}
        >
          <ChevronRight
            aria-hidden="true"
            className={cn(
              "size-3.5 shrink-0 text-muted-foreground transition-transform duration-(--dur-fast) ease-(--ease)",
              open && "rotate-90",
            )}
          />
          {/* The NAME wins the width fight, the same rule the source rows below
              already follow (QA scope 4, finding #5). A zero basis with flex-1
              hands it every spare pixel AND leaves it nothing to give back when
              the row is short, so all the shrinking lands on the counts. This
              header had the counts at `shrink-0`, which inverted it: they took
              what they wanted and the name arrived as «kco…» (rev13 acceptance). */}
          <span className="min-w-0 flex-1 truncate text-sm font-medium" title={group.destination}>
            {group.destination}
          </span>
          <span aria-hidden="true" className="shrink-0 text-xs text-muted-foreground">{" · "}</span>
          {/* The shut card's whole claim: how many distinct routes, over how
              many traces. Both, because one without the other says nothing
              about how well the fleet knows this destination. */}
          <span
            className="nums min-w-0 max-w-[45%] shrink truncate text-xs text-muted-foreground"
            title={t("destinations.counts", { paths, traces })}
          >
            {t("destinations.counts", { paths, traces })}
          </span>
        </button>
      </h3>

      {open ? (
        <>
          <ul id={listId} aria-label={group.destination} className="mt-1.5 flex flex-col gap-1">
            {pager.visible.map((row) => {
              const active = selected?.source === row.sourceNode && selected?.destination === row.destination;
              const from = t("destinations.from", { node: row.sourceNode });
              const rowPaths = finiteCount(row.snapshotCount);
              const rowTraces = finiteCount(row.traceCount);
              const counts = t("destinations.counts", {
                paths: t(`paths.${countForm(locale, rowPaths)}` as MTRKey, { count: rowPaths }),
                traces: t(`traces.${countForm(locale, rowTraces)}` as MTRKey, { count: rowTraces }),
              });
              return (
                <li key={`${row.sourceNode}→${row.destination}`}>
                  <button
                    type="button"
                    aria-pressed={active}
                    aria-label={`${row.sourceNode} → ${row.destination}`}
                    onClick={() => onSelect(row)}
                    className={cn(
                      "flex w-full items-baseline justify-between gap-1 rounded-md px-2 py-1.5 text-left text-xs",
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
                    <span className="min-w-0 flex-1 truncate" title={from}>
                      {from}
                    </span>
                    {/* A REAL character between the two halves, not a CSS gap.
                        The gap is not text, so the row read out and copied as
                        «from kconmon-prod-m102 · 3 traces» — the node name and
                        the leading count welded into one token (owner report).
                        Decoration to a screen reader, a separator to everything
                        that reads the row as a string. */}
                    <span aria-hidden="true" className="shrink-0 text-muted-foreground">{" · "}</span>
                    <span className="nums min-w-0 max-w-[45%] shrink truncate text-muted-foreground" title={counts}>
                      {counts}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>
          <Pager pager={pager} subject={t("destinations.sources.subject")} className="px-0" />
        </>
      ) : null}
    </div>
  );
}

function DestinationsPane({
  selected,
  onSelect,
  onLinkedPairMissing,
}: {
  /** Called once when a ?source=&destination= link names a pair the history does not hold. */
  onLinkedPairMissing?: (pair: Pair) => void;
  selected: Pair | null;
  /** The whole row, not just its two names: the row also carries the pair's
   *  snapshot and trace TOTALS, which is what lets pane 3's trend say how much
   *  of the pair's history it is actually drawing. */
  onSelect: (row: MTRDestination) => void;
}) {
  const t = useT(mtrDict);
  const query = useQuery({ queryKey: ["mtr", "destinations"], queryFn: getMTRDestinations });
  const groups = useMemo(() => groupDestinations(query.data?.destinations ?? []), [query.data]);
  /* One page of destinations at a time: a fleet that traces everything to
     everything has as many groups as it has nodes. */
  const pager = usePager(groups);
  /* Read ONCE, on mount: the card a shared link names is the one card that is
     open on arrival, and a later collapse must not be undone by a re-render. */
  const [linked] = useState(() => deepLinkDestination(window.location.search));
  /* The source half of a pair link. With it the effect below also SELECTS the
     pair, so the reader lands on the exact pair's path history rather than the
     destination's generic view (the run-detail "Open in MTR Explorer" link). */
  const [linkedSource] = useState(() => deepLinkSource(window.location.search));
  const [open, setOpen] = useState<Record<string, boolean>>(() => (linked ? { [linked]: true } : {}));
  /* The linked card may not be on page one, and a card nobody can see is not an
     opened card. Turned to once, when the groups first arrive. */
  const [turned, setTurned] = useState(false);
  const { setPage } = pager;
  const size = pager.size;
  useEffect(() => {
    if (turned || groups.length === 0) return;
    setTurned(true);
    const page = destinationPage(groups, linked, size);
    if (page !== null) setPage(page);
    /* A pair link also picks the source row, so pane 2 shows THAT pair's history
       on arrival. A stale link whose pair is no longer traced selects nothing. */
    if (linked && linkedSource) {
      const row = groups
        .find((g) => g.destination === linked)
        ?.sources.find((s) => s.sourceNode === linkedSource);
      if (row) onSelect(row);
      /* A link whose pair path history has never seen: say so about THAT pair.
         The generic "pick a source" answered a question nobody asked — the
         reader arrived from a run detail that already told them there was no
         route, and clicked a link that then mentioned the pair nowhere. */
      else onLinkedPairMissing?.({ source: linkedSource, destination: linked });
    }
  }, [groups, linked, linkedSource, size, turned, setPage, onSelect, onLinkedPairMissing]);

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
          <a href={withAtParam("/diagnostics")} className="text-primary hover:underline">
            {t("destinations.empty.link")}
          </a>{" "}
          {t("destinations.empty.after")}
        </EmptyNote>
      ) : null}

      {/* One CARD per destination, shut on arrival: the list a card holds is
          the pane's real bulk, and the card's own summary is enough to choose
          between destinations without opening any of them. */}
      {groups.length > 0 ? (
        <div className="mt-4 flex flex-col gap-2">
          {pager.visible.map((group) => (
            <DestinationCard
              key={group.destination}
              group={group}
              /* Derived, not stored, when the reader has not said: the card
                 holding the SELECTED pair stays open, so a selection is never
                 hidden behind a chevron. An explicit collapse still wins. */
              open={cardOpen(open, group.destination, selected?.destination === group.destination)}
              onToggle={() =>
                setOpen((prev) => ({
                  ...prev,
                  [group.destination]: !cardOpen(prev, group.destination, selected?.destination === group.destination),
                }))
              }
              selected={selected}
              onSelect={onSelect}
            />
          ))}
          <Pager pager={pager} subject={t("destinations.subject")} truncated={query.data?.truncated} className="px-0" />
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
  /* The TICKET of the request that owns this pane. Two pairs clicked in quick
     succession are two requests in flight, and the network decides the order
     they come back in — the slow first pair used to land last and repaint its
     routes under the second pair's heading, which is the console showing one
     pair's path history and naming another (hostile-QA probe C). Everything a
     response does is gated on still holding the current ticket. */
  const ticket = useRef(0);

  const load = useCallback(
    async (target: Pair, cursor?: string) => {
      const mine = ++ticket.current;
      setPage((p) => ({ ...p, loading: true, error: null }));
      try {
        const body = await getMTRSnapshots({ source: target.source, destination: target.destination, cursor });
        if (mine !== ticket.current) return;
        /* The server substitutes `[]` for a nil slice (httpapi/mtr.go), so a
           null here is a payload that did not come from it — and .map on it
           took the whole page down to white (hostile-QA probe E). */
        const rows = Array.isArray(body.snapshots) ? body.snapshots : [];
        // A cursor-less call is page one — replace, so a re-selection or a
        // remount does not just keep growing the list.
        setSnapshots((prev) => (cursor ? mergeSnapshots(prev, rows) : rows));
        setPage({
          nextCursor: typeof body.nextCursor === "string" ? body.nextCursor : "",
          loading: false,
          error: null,
        });
      } catch (err) {
        if (mine !== ticket.current) return;
        if (!cursor) setSnapshots([]);
        /* The cursor that FAILED is kept, so the button stays enabled and the
           click can be retried; clearing it disabled "Load older" for the rest
           of the session over one 502. A first page that failed has no cursor
           to keep, and correctly ends the list. */
        setPage({ nextCursor: cursor ?? "", loading: false, error: err });
      }
    },
    [],
  );

  useEffect(() => {
    if (!pair) {
      // Deselecting invalidates whatever is in flight too.
      ticket.current += 1;
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
  missingPair,
  history,
  selectedId,
  onSelectSnapshot,
  compare,
  onToggleCompare,
  onCompareOpen,
}: {
  pair: Pair | null;
  /** A ?source=&destination= link naming a pair path history does not hold. */
  missingPair?: Pair | null;
  history: SnapshotHistory;
  selectedId: string | null;
  onSelectSnapshot: (snapshot: PathSnapshot) => void;
  compare: string[];
  onToggleCompare: (id: string) => void;
  /** Opens the comparison. Ticking two boxes is a SELECTION; the reader says when to look. */
  onCompareOpen?: () => void;
}) {
  const t = useT(mtrDict);
  const { locale } = useLocale();
  const { snapshots, loading, error, hasOlder, loadOlder } = history;
  const changed = useMemo(() => pathChangeFlags(snapshots), [snapshots]);
  /* What the sidebar counts, summed over what this pane actually lists — so the two numbers can be
     read side by side instead of looking like a contradiction. */
  const totalTraces = useMemo(
    () => snapshots.reduce((sum, s) => sum + finiteCount(s.traceCount), 0),
    [snapshots],
  );
  /* "Load older" fetches more; the pager cuts what has been fetched. The two
     are different questions and the reader needs both. */
  const pager = usePager(snapshots, { resetKey: pair ? `${pair.source}\u0000${pair.destination}` : "" });

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
        <PathChangesTimeline
          source={pair.source}
          destination={pair.destination}
          snapshots={snapshots}
          selectedId={selectedId}
          onSelect={onSelectSnapshot}
        />
      ) : null}

      {/* No "on the left" (QA round 4, finding #20): under ~700px the three
          panes stack, and the destinations pane is ABOVE this one, not beside
          it — the copy was pointing at empty space. The neutral wording is
          true at every width. */}
      {!pair ? (
        <EmptyNote>
          {missingPair
            ? t("history.linkEmpty", { source: missingPair.source, destination: missingPair.destination })
            : t("history.noPair")}
        </EmptyNote>
      ) : null}

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
        <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
          <p className="text-xs leading-relaxed text-muted-foreground">{t("history.compareHint")}</p>
          {/* Enabled only with two ticked: the button says what it will do, and
              a disabled one says why it cannot yet, rather than vanishing. */}
          <Button
            size="sm"
            variant="outline"
            disabled={compare.length !== 2 || !onCompareOpen}
            title={compare.length === 2 ? undefined : t("history.compareHint")}
            onClick={() => onCompareOpen?.()}
          >
            {t("history.compareOpen", { count: compare.length })}
          </Button>
        </div>
      ) : null}

      {snapshots.length > 0 ? (
        <>
        <ul aria-label={t("history.list.aria")} className="mt-1 divide-y divide-border">
          {pager.visible.map((s, i) => {
            /* The index into the WHOLE loaded history, not into the page. Both
               the badge and its sentence are answers about the row's next-older
               neighbour, and the page-local index made page two claim page one's
               changes: row 11 was badged with what happened between rows 1 and 2
               (hostile-QA probe A). */
            const at = pager.slice.start + i;
            return (
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
                  {/* The ROUTE leads. A twelve-character hash is a key, not a
                      path, and an MTR exists to show where the packets went
                      (owner: «ничего не понятно»). */}
                  <PathChain hops={s.hops} source={s.sourceNode} destination={s.destination} />
                  <span className="flex flex-wrap items-center gap-2">
                    <Badge variant="neutral">
                      {t(`hops.${countForm(locale, hopCountOf(s))}` as MTRKey, { count: hopCountOf(s) })}
                    </Badge>
                    {/* Says WHAT moved, not that something did. */}
                    {changed[at] ? <Badge variant="warn">{changeText(snapshots, at, t)}</Badge> : null}
                    {/* Secondary now, and still the thing you paste into a ticket. */}
                    <span className="nums font-mono text-[10px] text-muted-foreground" title={s.pathHash}>
                      {shortHash(s.pathHash)}
                    </span>
                  </span>
                  <span className="nums text-xs text-muted-foreground">
                    {/* The stamps are computed here and passed in, never
                        formatted by the dictionary — and they land INSIDE a
                        translated sentence, so fmtTime takes the interface
                        locale rather than the browser's. */}
                    {t("history.span", {
                      from: fmtTime(s.firstSeen, locale),
                      to: fmtTime(s.lastSeen, locale),
                      traces: t(`traces.${countForm(locale, finiteCount(s.traceCount))}` as MTRKey, {
                        count: finiteCount(s.traceCount),
                      }),
                    })}
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
        <Pager pager={pager} subject={t("history.subject")} className="px-0" />
        </>
      ) : null}

      {/* Either there IS more history or there is not, and the footer says which. A button that can
          never be pressed is indistinguishable from a broken one: the owner read this list as "6 of
          232 traces, and the control to see the rest is dead", when in fact these six ARE the whole
          route history — the 232 is a count of traces folded into them. */}
      {snapshots.length > 0 ? (
        <div className="mt-4 flex justify-center">
          {hasOlder || loading ? (
            <Button variant="outline" size="sm" disabled={loading} onClick={loadOlder}>
              {loading ? t("history.loadingOlder") : t("history.loadOlder")}
            </Button>
          ) : (
            <p className="text-xs text-muted-foreground">
              {t("history.allShown", {
                paths: t(`paths.${countForm(locale, snapshots.length)}` as MTRKey, { count: snapshots.length }),
                traces: t(`traces.${countForm(locale, totalTraces)}` as MTRKey, { count: totalTraces }),
              })}
            </p>
          )}
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
  embedded,
}: {
  snapshotId: string | null;
  fallback: PathSnapshot | null;
  history: TrendHistory;
  /** Rendered inside a Modal, which owns the frame and the heading. */
  embedded?: boolean;
}) {
  const t = useT(mtrDict);
  const query = useQuery({
    queryKey: ["mtr", "snapshot", snapshotId, "enriched"],
    queryFn: () => getMTRSnapshot(snapshotId as string, true),
    enabled: snapshotId !== null,
  });
  const snapshot = query.data ?? fallback;

  return (
    <Pane title={t("detail.title")} embedded={embedded}>
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
        <>
          <TraceDetail key={snapshot.id} snapshot={snapshot} history={history} />
          {/* And the traces the route's own count is a count OF. The hop table above is one
              reading — the last one folded into this route; these are the readings themselves,
              each with its own clock. The count used to lead nowhere. */}
          <TraceList key={`${snapshot.id}-traces`} snapshotID={snapshot.id} traceCount={snapshot.traceCount} />
        </>
      ) : null}
    </Pane>
  );
}

/* ── pane 3, the other half: the diff ───────────────────────────────────── */

/**
 * DiffPane takes over pane 3 while two paths are ticked; the two are ordered OLDEST first here
 * rather than in the ticking order.
 */
function DiffPane({
  snapshots,
  compare,
  embedded,
}: {
  snapshots: PathSnapshot[];
  compare: string[];
  /** Rendered inside a Modal, which owns the frame and the heading. */
  embedded?: boolean;
}) {
  const t = useT(mtrDict);
  const picked = compare.map((id) => snapshots.find((s) => s.id === id)).filter((s): s is PathSnapshot => s !== undefined);

  return (
    <Pane title={t("diff.title")} embedded={embedded}>
      {picked.length < 2 ? (
        <EmptyNote>{t("diff.empty")}</EmptyNote>
      ) : (
        (() => {
          const [a, b] = [...picked].sort((x, y) => new Date(x.firstSeen).getTime() - new Date(y.firstSeen).getTime());
          /* Two hopless paths align to zero rows, so the diff drew a header, a
             legend and nothing under them — a table that answers the reader's
             question with blank space (hostile-QA probe K). It is the ONE input
             the alignment cannot say anything about, and saying so is the
             answer. */
          if (hopCountOf(a) === 0 && hopCountOf(b) === 0) return <EmptyNote>{t("diff.noHops")}</EmptyNote>;
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
  /* "auto" posts nothing, which is what this form always did. */
  const [sampleInterval, setSampleInterval] = useState("auto");
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
    // Every page: a picker that stops at the server's first 100 hides targets that exist.
    queryFn: () => listAllTargets(),
    enabled: canReadTargets && destinationKind === "target",
  });
  const targets = targetsQuery.data?.items ?? [];

  const durationNs = durationNsFor(duration);
  /* Same rule as the Diagnostics form: only the presets that FIT the run, and a
     picked one that no longer does falls back to Auto rather than standing as a
     value the server would refuse. */
  const intervalOptions = sampleIntervalOptionsFor(durationNs);
  const intervalValue = intervalOptions.some((o) => o.value === sampleInterval) ? sampleInterval : "auto";
  const requestedIntervalNs = sampleIntervalNsFor(intervalValue);
  const request = buildRunRequest({
    type: "mtr",
    sources: sourcesAll ? [] : sources,
    destinations: destinationsAll ? [] : destinations,
    destinationKind,
    destinationTargetId,
    destinationAddress: destinationAddress.trim(),
    durationNs,
    sampleIntervalNs: requestedIntervalNs,
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

  /* The planner mirror, once, for the caption below. `mtr` is nailed on this
     pane, which is exactly why the caption may not quote the base cadence: a
     trace is thirty hops walked in sequence and the server stretches the
     interval to one round. */
  const plan = planCadenceFor(durationNs, "mtr", pairCount, resolvedSources.length, requestedIntervalNs);

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
            {/* THE bug this pane carried: it said the BASE cadence — «раз в 5 с»
                — for the one check type that cannot keep it, while the
                Diagnostics form beside it had already been made type-aware.
                Same planner mirror, same sentence builder, so the two cannot
                say different things about the same run again. */}
            {durationNs === 0
              ? t("runner.duration.instant")
              : cadenceCaption(t, locale, plan, "mtr", duration, "runner.duration.caption")}
          </p>
        </div>

        {/* The cadence control, for a duration run only — the same table, the
            same "Auto", and the same bound as the Diagnostics form's. */}
        {durationNs > 0 ? (
          <div data-testid="runner-sample-interval">
            <span className="mb-2 block text-xs font-medium text-muted-foreground">{t("runner.sampleInterval")}</span>
            <Segmented
              aria-label={t("runner.sampleInterval.aria")}
              options={intervalOptions.map((o) => ({
                value: o.value,
                label: o.value === "auto" ? t("runner.sampleInterval.auto") : o.label,
              }))}
              value={intervalValue}
              onChange={setSampleInterval}
              className="flex-wrap"
            />
          </div>
        ) : null}

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
            <a href={withAtParam(`/diagnostics/runs/${startedRunId}`)} className="text-primary hover:underline">
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
  /* Set once, by a deep link whose pair path history does not hold: pane 2 then
     names that pair instead of asking the reader to pick a source. */
  const [missingPair, setMissingPair] = useState<Pair | null>(null);
  /* The comparison opens on the reader's word, not the moment a second box is
     ticked: ticking two is a SELECTION, and having the page throw a dialog at
     you for it would take the list away mid-choice. */
  const [compareOpen, setCompareOpen] = useState(false);

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
            {/* TWO panes, not three. The third held the trace detail, and three
                columns on a laptop left the route history too narrow to read a
                route in and the detail too narrow to read a hop in (owner
                report). A trace is something you OPEN, read and close — so it
                opens over the page, and the history gets the width it needs. */}
            <div
              data-testid="mtr-panes"
              className="grid grid-cols-1 items-start gap-4 lg:grid-cols-[minmax(0,0.85fr)_minmax(0,2fr)]"
            >
            <DestinationsPane
              selected={pair}
              onSelect={selectPair}
              onLinkedPairMissing={setMissingPair}
            />
            <HistoryPane
              pair={pair}
              missingPair={missingPair}
              history={history}
              selectedId={snapshot?.id ?? null}
              onSelectSnapshot={setSnapshot}
              compare={compare}
              onToggleCompare={onToggleCompare}
              onCompareOpen={() => setCompareOpen(true)}
            />
            </div>

            {/* ONE trace, centred over a blurred page, and nothing to steer from
                inside it: the reader opened this route, not a queue of them. To
                read another they close this and pick it — which is also what
                keeps the dialog honest about what it is showing (owner). */}
            <Modal
              open={snapshot !== null}
              onClose={() => setSnapshot(null)}
              size="wide"
              title={t("detail.title")}
              description={snapshot ? `${snapshot.sourceNode} → ${snapshot.destination}` : undefined}
            >
              <DetailPane snapshotId={snapshot?.id ?? null} fallback={snapshot} history={trendHistory} embedded />
            </Modal>

            {/* The comparison is a modal for the opposite reason to the trace:
                BOTH things being compared are inside it, so nothing behind it is
                needed while it is up. */}
            <Modal
              open={compareOpen && compare.length === 2}
              onClose={() => setCompareOpen(false)}
              size="wide"
              title={t("diff.title")}
              description={pair ? `${pair.source} → ${pair.destination}` : undefined}
            >
              <DiffPane snapshots={history.snapshots} compare={compare} embedded />
            </Modal>
          </>
        )}
      </div>
    );
  }

  return (
    <PageShell
      timeMachine
      title={t("title")}
      /* {at} lands INSIDE a translated sentence, so it takes that sentence's
         language — lib/i18n's localeTag, same as /diagnostics and /explore. */
      description={at ? t("description.at", { at: at.toLocaleString(localeTag(locale)) }) : t("description")}
    >
      {body}
    </PageShell>
  );
}
