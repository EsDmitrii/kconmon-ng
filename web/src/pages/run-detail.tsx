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
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import { cn } from "@/lib/utils";

const RUN_PATH_PREFIX = "/diagnostics/runs/";

/**
 * runIdFromPath reads the id straight off window.location.pathname rather
 * than through TanStack Router's own param matching -- the same convention
 * pages/login.tsx already uses for ?returnTo=. It keeps this page testable
 * with a plain render (window.history.pushState, no RouterProvider) and
 * correct regardless of how the browser got here: a full navigation from
 * pages/diagnostics.tsx's <a href>, or a cold load of a bookmarked/shared
 * permalink (task-24-brief.md's whole point for this route).
 */
export function runIdFromPath(pathname: string): string {
  return pathname.startsWith(RUN_PATH_PREFIX) ? pathname.slice(RUN_PATH_PREFIX.length) : "";
}

/**
 * decodeRunId is what a run id looks like to a HUMAN (QA round 4, finding
 * #19).
 *
 * window.location.pathname is percent-ENCODED, so a run whose id carries
 * anything outside the unreserved set arrived here as "run%2Fa%20b" and was
 * printed that way in the page header and in the not-found copy — an id the
 * reader could not match against the one they pasted.
 *
 * DISPLAY ONLY. The encoded form is what runIdFromPath keeps and what
 * lib/api's getRun re-encodes for the wire, so decoding here cannot corrupt a
 * request; this is the last step before the string is shown.
 *
 * Guarded, because decodeURIComponent THROWS on a lone "%" (a URIError, which
 * would blank the whole page over a cosmetic step). A string it cannot decode
 * comes back verbatim — the bytes in the URL bar are more use to whoever has
 * to explain them than an exception is.
 */
export function decodeRunId(raw: string): string {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

/** okPairs is the "Pairs 0/2 ok" numerator (QA round 4, finding #14). The
 *  header read "Pairs 2/2", which an operator reads as "two of two passed"
 *  while it actually meant "two of two rows have arrived" — on a run where
 *  BOTH pairs failed. The run's own pairOk is not usable for it: the socket
 *  can carry a pair's result before the REST snapshot's counter catches up, so
 *  the count is derived from the merged rows this page is actually showing. */
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

function fmtTime(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

function PairTable({ pairs }: { pairs: RunPairRow[] }) {
  if (pairs.length === 0) {
    return <div className="px-6 py-10 text-center text-sm text-muted-foreground">No pairs dispatched yet.</div>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <caption className="sr-only">Per-pair run results</caption>
        <thead>
          <tr className="border-b border-border text-left text-[11px] uppercase tracking-[0.07em] text-muted-foreground">
            <th scope="col" className="py-3 pl-4 pr-4 font-semibold">
              Pair
            </th>
            <th scope="col" className="py-3 pr-4 font-semibold">
              State
            </th>
            <th scope="col" className="py-3 pr-4 text-right font-semibold">
              Duration
            </th>
            <th scope="col" className="py-3 pr-4 font-semibold">
              Error
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
 * CancelRunButton is POST /api/v1/runs/{id}/cancel (Plan Decision 15). It is
 * rendered ONLY for a non-terminal run and only with runs:create — the same
 * permission that started the run, since starting fleet-wide probe traffic
 * and stopping it are the same operational class (middleware_auth.go) — and
 * it is ABSENT rather than disabled otherwise, the pattern PAGES.md:126-129
 * pins for every affordance a subject does not hold.
 *
 * The 204 means "accepted", not "cancelled": the run's own goroutine writes
 * the terminal status once its in-flight pairs settle. So this asks for ONE
 * immediate re-read rather than writing a status into the cache — and when
 * that read comes back "cancelled" the poll stops by itself (useRun's
 * refetchInterval), which is also what makes this button disappear.
 */
function CancelRunButton({ runId, onCancelled }: { runId: string; onCancelled: () => Promise<unknown> }) {
  // Note the asymmetry with the permission gate two paragraphs up, and that it
  // is deliberate: no runs:create means this button is ABSENT, the Time Machine
  // means it is DISABLED (lib/timemachine.tsx's useWriteGuard). Cancelling
  // from a view of the past would stop a run happening in the present — and
  // the guard carries that REASON with the control (QA round 2, finding #18;
  // extended to this page in round 3), so a keyboard user who tabs straight to
  // a greyed "Cancel run" is told why instead of guessing.
  //
  // Cancel is this page's ONLY mutation. There is no rerun BUTTON to guard —
  // the "back to Diagnostics" affordance is a plain <a> to the form, which
  // starts nothing by itself.
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
      setError(err instanceof ApiError ? (err.problem.detail ?? err.problem.title) : "Failed to cancel this run");
    }
    setBusy(false);
  }

  return (
    <>
      <Button size="sm" variant="outline" loading={busy} {...guard} onClick={handleCancel}>
        Cancel run
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
  return (
    <PageShell
      title="Run not found"
      description={runId ? `No run matches “${decodeRunId(runId)}”.` : "No run id in the URL."}
    >
      <Card role="status" className="flex flex-col items-center gap-3 px-8 py-16 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <SearchX className="size-5" />
        </span>
        <p className="text-sm font-medium">This run does not exist</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
          It may have been an id typo, or the run history behind it is not persisted (in-memory only) and the
          console has since restarted.
        </p>
        <a href="/diagnostics" className="text-xs font-medium text-primary hover:underline">
          Back to Diagnostics
        </a>
      </Card>
    </PageShell>
  );
}

export function RunDetailPage() {
  const runId = runIdFromPath(window.location.pathname);
  const { run, pairs, isLoading, notFound, error, live, refetch } = useRun(runId);
  const { can } = useAuth();
  /* The Time Machine's framing for THIS page (QA round 4, finding #4). The
     permalink itself stays reachable while engaged — a permalink names one
     specific run, and refusing to render the run somebody linked to would be a
     worse answer than rendering it. What was missing is the framing every
     other engaged surface carries: the header now says which instant the
     console is viewing and that this run is being shown regardless of it, so a
     run that happened AFTER `t` is not silently read as part of that past. */
  const { at } = useTimeContext();

  if (notFound) return <NotFound runId={runId} />;

  if (!run && isLoading) {
    return (
      <PageShell title="Diagnostics run" description="Loading…">
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">Loading run…</span>
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
      <PageShell title="Diagnostics run" description={decodeRunId(runId)}>
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">This run is unavailable</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {error?.message ?? "Failed to load this run."}
          </p>
        </Card>
      </PageShell>
    );
  }

  const terminal = isTerminalRunStatus(run.status);

  return (
    <PageShell
      title="Diagnostics run"
      description={
        at
          ? `${decodeRunId(run.id)} — this permalink is shown in full; the console is otherwise viewing ${at.toLocaleString()}`
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
            <dt className="text-xs text-muted-foreground">Type</dt>
            <dd className="nums mt-0.5">{run.type.toUpperCase()}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">Plane</dt>
            <dd className="mt-0.5">{run.plane}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">Pairs</dt>
            {/* ok/total, worded like the history row on /diagnostics (QA round
                4, finding #14): the bare "2/2" was arrived/total and read as
                passed/total, so a run whose every pair FAILED announced itself
                as complete success in the one number a reader scans first. */}
            <dd className="nums mt-0.5">
              {okPairs(pairs)}/{run.pairTotal} ok
            </dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">Started</dt>
            <dd className="mt-0.5">{fmtTime(run.startedAt)}</dd>
          </div>
        </dl>
      </Card>

      <Card asChild className="overflow-hidden p-0">
        <section>
          <div className="border-b border-border px-4 py-3">
            <h2 className="text-sm font-semibold">Pairs</h2>
          </div>
          <PairTable pairs={pairs} />
        </section>
      </Card>
    </PageShell>
  );
}
