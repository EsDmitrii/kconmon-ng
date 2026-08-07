import { useCallback, useEffect, useState, type FormEvent } from "react";
import { ClipboardList } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useTopology } from "@/hooks/use-topology";
import { ApiError, createRun, getRuns, goTo } from "@/lib/api";
import { CHECK_TYPES, type CheckType, type RunSummary } from "@/lib/types";
import { cn } from "@/lib/utils";

// MAX_PAIRS mirrors checks.maxPairs (internal/console/checks/checks.go) --
// a client-side echo of the server's ErrTooManyPairs 422 so the operator
// sees the guard before submitting, not after. The server remains the only
// real enforcement point; a Plan run through a stale/empty topology can
// still disagree with this estimate (e.g. duplicate names), and that
// disagreement is the server's 422 to raise, not a bug in this preview.
export const MAX_PAIRS = 400;

/**
 * estimatePairCount previews checks.Plan's own pair count: the deduplicated
 * cross product of sources x destinations, minus self-pairs. It is a
 * client-side approximation only -- Plan additionally dedupes exact
 * duplicate (source, destination) pairs after expansion, which this does
 * not need to reproduce since Set already dedupes each side going in.
 *
 * This is a DISPLAY estimate only -- it is not what gates the submit
 * button. See estimateRawPairCount for that.
 */
export function estimatePairCount(sources: string[], destinations: string[]): number {
  const srcSet = new Set(sources);
  const dstSet = new Set(destinations);
  let count = 0;
  for (const s of srcSet) {
    for (const d of dstSet) {
      if (s !== d) count++;
    }
  }
  return count;
}

/**
 * estimateRawPairCount mirrors Plan's OWN gate exactly: `len(sources) *
 * len(destinations)`, computed before self-pair exclusion or dedup. That
 * raw-product guard is the first thing checks.Plan checks (checks.go), and
 * it runs against the raw slices the server receives -- so this must too,
 * rather than against a deduplicated Set the way estimatePairCount is.
 *
 * The two numbers can disagree: 20 sources and 21 destinations sharing 20
 * names have a raw product of 420 (over the limit) but a self-excluded
 * count of exactly 400 (at the limit, self-pairs subtracted one per shared
 * name) -- estimatePairCount alone would wrongly let that selection
 * through submit, only to have the server 422 it with ErrTooManyPairs.
 */
export function estimateRawPairCount(sources: string[], destinations: string[]): number {
  return sources.length * destinations.length;
}

const STATUS_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  pending: "neutral",
  running: "neutral",
  succeeded: "ok",
  failed: "bad",
  partial: "warn",
};

function StatusBadge({ status }: { status: string }) {
  return (
    <Badge variant={STATUS_VARIANT[status] ?? "unknown"} dot>
      {status}
    </Badge>
  );
}

function NodeSelector({
  label,
  nodes,
  all,
  onAllChange,
  selected,
  onToggle,
}: {
  label: string;
  nodes: string[];
  all: boolean;
  onAllChange: (v: boolean) => void;
  selected: string[];
  onToggle: (name: string) => void;
}) {
  return (
    <fieldset className="rounded-md border border-border p-3">
      <legend className="px-1 text-xs font-medium text-muted-foreground">{label}</legend>
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={all}
          onChange={(e) => onAllChange(e.target.checked)}
          className="size-4 rounded border-border-strong"
        />
        All nodes ({nodes.length})
      </label>
      {!all ? (
        <div className="mt-2 flex max-h-40 flex-col gap-1 overflow-y-auto">
          {nodes.length === 0 ? (
            <p className="text-xs text-muted-foreground">No nodes reported by the controller yet.</p>
          ) : (
            nodes.map((n) => (
              <label key={n} className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={selected.includes(n)}
                  onChange={() => onToggle(n)}
                  className="size-4 rounded border-border-strong"
                />
                <span className="truncate">{n}</span>
              </label>
            ))
          )}
        </div>
      ) : null}
    </fieldset>
  );
}

function toggleName(list: string[], name: string): string[] {
  return list.includes(name) ? list.filter((n) => n !== name) : [...list, name];
}

function RunForm({ nodeNames }: { nodeNames: string[] }) {
  const [type, setType] = useState<CheckType>("tcp");
  const [sourcesAll, setSourcesAll] = useState(true);
  const [destinationsAll, setDestinationsAll] = useState(true);
  const [sources, setSources] = useState<string[]>([]);
  const [destinations, setDestinations] = useState<string[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string>();

  const resolvedSources = sourcesAll ? nodeNames : sources;
  const resolvedDestinations = destinationsAll ? nodeNames : destinations;
  const pairCount = estimatePairCount(resolvedSources, resolvedDestinations);
  const rawPairCount = estimateRawPairCount(resolvedSources, resolvedDestinations);
  // Gates on the RAW product, not the self-excluded pairCount below: that is
  // the exact guard checks.Plan runs first, before any dedup or self-pair
  // exclusion (checks.go), so an overlapping selection that would collapse
  // under the limit only after exclusion must still be blocked here -- see
  // estimateRawPairCount's own doc for the 20x21/400 example.
  const overLimit = rawPairCount > MAX_PAIRS;
  const noPairs = pairCount === 0;

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setSubmitError(undefined);
    setSubmitting(true);
    try {
      const res = await createRun({
        type,
        plane: "pod",
        sources: sourcesAll ? [] : sources,
        destinations: destinationsAll ? [] : destinations,
      });
      goTo(`/diagnostics/runs/${res.id}`);
    } catch (err) {
      setSubmitError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to start run");
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-5">
        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">Check type</span>
          <Segmented
            aria-label="Check type"
            options={CHECK_TYPES.map((t) => ({ value: t, label: t.toUpperCase() }))}
            value={type}
            onChange={setType}
          />
        </div>

        <label className="flex w-32 flex-col gap-1 text-[13px]">
          <span className="text-muted-foreground">Plane</span>
          {/* The only plane that exists (task-24-brief.md) -- a real,
              disabled form control rather than a decorative badge, so it
              still reads as "part of this form" to a screen reader. */}
          <select
            disabled
            value="pod"
            className="h-9 rounded-md border border-border-strong bg-transparent px-3 text-[13px] text-muted-foreground disabled:opacity-70"
          >
            <option value="pod">pod</option>
          </select>
        </label>

        <div className="grid gap-4 sm:grid-cols-2">
          <NodeSelector
            label="Sources"
            nodes={nodeNames}
            all={sourcesAll}
            onAllChange={setSourcesAll}
            selected={sources}
            onToggle={(n) => setSources((prev) => toggleName(prev, n))}
          />
          <NodeSelector
            label="Destinations"
            nodes={nodeNames}
            all={destinationsAll}
            onAllChange={setDestinationsAll}
            selected={destinations}
            onToggle={(n) => setDestinations((prev) => toggleName(prev, n))}
          />
        </div>

        <div className="flex flex-wrap items-center gap-3">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              setSourcesAll(true);
              setDestinationsAll(true);
            }}
          >
            All ↔ All
          </Button>
          {/* "~" flags this as an estimate: the server is the only real
              arbiter, and on an overlapping selection this self-excluded
              number can even read as under the limit while the raw S×D
              product the server actually gates on is over it. */}
          <span className={cn("nums text-sm", overLimit ? "text-health-bad" : "text-muted-foreground")}>
            ~{pairCount} pair{pairCount === 1 ? "" : "s"}
            {overLimit
              ? ` — above the ${MAX_PAIRS}-pair limit (server enforces the raw ${resolvedSources.length}×${resolvedDestinations.length} limit of ${MAX_PAIRS}), narrow the selection`
              : ""}
          </span>
        </div>

        {submitError ? (
          <p role="alert" className="text-sm text-health-bad">
            {submitError}
          </p>
        ) : null}

        <Button type="submit" loading={submitting} disabled={overLimit || noPairs} className="self-start">
          Start run
        </Button>
      </form>
    </Card>
  );
}

function fmtTime(timestamp?: string): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleString();
}

function HistoryList({ runs }: { runs: RunSummary[] }) {
  if (runs.length === 0) {
    return (
      <div className="flex flex-col items-center gap-3 px-6 py-14 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <ClipboardList className="size-5" />
        </span>
        <p className="text-sm font-medium">No runs yet</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
          Runs started from the form above (or by another operator) show up here.
        </p>
      </div>
    );
  }
  return (
    <ul className="mt-4 divide-y divide-border">
      {runs.map((r) => (
        <li key={r.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
          <a href={`/diagnostics/runs/${r.id}`} className="font-medium text-primary hover:underline">
            {r.id}
          </a>
          <StatusBadge status={r.status} />
          <span className="text-xs uppercase tracking-wide text-muted-foreground">{r.type}</span>
          <span className="nums ml-auto text-xs text-muted-foreground">
            {r.pairOk}/{r.pairTotal} ok
          </span>
          <span className="text-xs text-muted-foreground">{fmtTime(r.createdAt)}</span>
        </li>
      ))}
    </ul>
  );
}

export function DiagnosticsPage() {
  const { can } = useAuth();
  const topo = useTopology();
  const { available: dbConfigured, resolved: dbResolved } = useDatabaseAvailable();

  const canCreate = can("runs:create");
  const nodeNames = topo.data?.nodes.map((n) => n.name) ?? [];

  /* Run history (GET /api/v1/runs) is paginated behind the same opaque
     keyset cursor as event scrollback, and is loaded the same way --
     loadHistory in pages/live.tsx is the convention this mirrors.
     `history.nextCursor` doubles as both "there is nothing more to load"
     (exhausted, "" after a real page) and "nothing has loaded yet" (the
     initial value, also ""), which is the right default for a "Load
     older" button that has not yet heard back from its first page. */
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [history, setHistory] = useState<{ nextCursor: string; loading: boolean; error: unknown }>({
    nextCursor: "",
    loading: false,
    error: null,
  });

  const loadRuns = useCallback(async (cursor?: string) => {
    setHistory((h) => ({ ...h, loading: true, error: null }));
    try {
      const page = await getRuns({ cursor });
      // A cursor-less call is page one -- replace, rather than append, so a
      // remount or a future refresh does not just keep growing the list.
      setRuns((prev) => (cursor ? [...prev, ...page.runs] : page.runs));
      setHistory({ nextCursor: page.nextCursor, loading: false, error: null });
    } catch (err) {
      setHistory({ nextCursor: "", loading: false, error: err });
    }
  }, []);

  useEffect(() => {
    void loadRuns(undefined);
  }, [loadRuns]);

  return (
    <PageShell
      title="Diagnostics"
      description="Run on-demand checks against the mesh, and browse run history."
    >
      {canCreate ? (
        <RunForm nodeNames={nodeNames} />
      ) : (
        <Card role="status" className="p-6">
          <p className="text-sm font-medium">Starting a run requires the runs:create permission</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
            You can still see run history below. Ask an operator to start a new run.
          </p>
        </Card>
      )}

      <Card asChild className="p-6">
        <section>
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h2 className="text-sm font-semibold">Run history</h2>
          </div>
          {dbResolved && !dbConfigured ? (
            <p role="status" className="mt-1 text-xs leading-relaxed text-muted-foreground">
              History is not persisted — set console.database.mode
            </p>
          ) : null}

          {history.error ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {history.error instanceof ApiError
                ? (history.error.problem.detail ?? history.error.problem.title)
                : "Run history is unavailable"}
            </p>
          ) : null}

          <HistoryList runs={runs} />

          {runs.length > 0 ? (
            <div className="mt-4 flex justify-center">
              <Button
                variant="outline"
                size="sm"
                disabled={history.nextCursor === "" || history.loading}
                onClick={() => loadRuns(history.nextCursor)}
              >
                {history.loading ? "Loading older…" : "Load older"}
              </Button>
            </div>
          ) : null}
        </section>
      </Card>
    </PageShell>
  );
}
