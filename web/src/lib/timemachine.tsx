import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

/**
 * Time Machine — the global time context (docs/console/product/TIME_MACHINE.md).
 *
 * Two states, Live and "@ t", held in ONE place so every data hook resolves
 * through it (Task 11 wires the consumers; this module is the plumbing).
 *
 * `?at=` is the single URL carrier (plan Decision 9): RFC 3339, read and
 * written through `window.location` + `window.history` exactly like login.tsx's
 * `?returnTo=` and run-detail.tsx's path id — no router search-param framework
 * is adopted in M5. Consequence worth knowing: TanStack Router owns navigation,
 * so a <Link> to another page carries no `at` in its href and the URL loses the
 * param on such a navigation while this context keeps it. Nothing breaks (the
 * next engage/returnToLive rewrites the URL), but the shareable-link guarantee
 * holds for the URL you are ON, not across in-app navigations. Propagating
 * `at` through <Link> means teaching the router about search params — out of
 * scope here, and deliberately so.
 */

/** AT_PARAM is the one URL key the Time Machine owns. */
export const AT_PARAM = "at";

/**
 * RFC 3339 date-time: a full instant with an offset. Deliberately strict —
 * `new Date()` alone happily accepts "2026", "2026-08-07" and browser-specific
 * shapes, and the console would then disagree with the Go server (time.RFC3339)
 * about what a shared link means. Anything else degrades to Live.
 */
const RFC3339 = /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/;

/**
 * formatAtParam renders the instant the API takes: RFC 3339, UTC, SECONDS
 * precision (sub-second is truncated, not rounded — a timestamp must never
 * travel forward on its way to the wire, see the future-clamp below).
 *
 * Exported because every Task-11 consumer builds its own fetch URL; one
 * formatter keeps those request shapes uniform and identical to what the URL
 * carries.
 */
export function formatAtParam(d: Date): string {
  return new Date(truncateToSecond(d.getTime())).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function truncateToSecond(ms: number): number {
  return Math.floor(ms / 1000) * 1000;
}

/**
 * normalize is the single gate every candidate instant passes: invalid → null
 * (Live), future → clamped to now.
 *
 * The clamp is client-side ON PURPOSE. `GET /api/v1/topology?at=` answers 400
 * for a future `at` (Task 9), and its small clock-skew grace is the SERVER's
 * safety net, not a budget for the client to spend: a browser whose clock runs
 * ahead must not turn that into a wall of 400s across every page.
 */
function normalize(d: Date, source: string): Date | null {
  const ms = d.getTime();
  if (Number.isNaN(ms)) {
    console.warn(`[timemachine] ignoring an unparseable time (${source}); staying Live`);
    return null;
  }
  const now = Date.now();
  if (ms > now) {
    console.warn(`[timemachine] ${source} is in the future; clamping to now`);
    return new Date(truncateToSecond(now));
  }
  return new Date(truncateToSecond(ms));
}

/**
 * readAtFromLocation resolves the current URL into a time context. It is the
 * whole init story AND the whole popstate story — one function, so a shared
 * link and a Back button can never disagree.
 */
export function readAtFromLocation(): Date | null {
  const raw = new URLSearchParams(window.location.search).get(AT_PARAM);
  if (raw === null || raw === "") return null;
  if (!RFC3339.test(raw)) {
    console.warn(`[timemachine] ignoring ?${AT_PARAM}=${raw}: not an RFC 3339 instant; staying Live`);
    return null;
  }
  return normalize(new Date(raw), `?${AT_PARAM}=${raw}`);
}

/** writeAt rewrites ONLY the at param, preserving pathname, hash and every
 *  other query param. pushState (not replaceState) so Back/Forward walk the
 *  time context the way the user expects. */
function writeAt(d: Date | null): void {
  const url = new URL(window.location.href);
  if (d) url.searchParams.set(AT_PARAM, formatAtParam(d));
  else url.searchParams.delete(AT_PARAM);
  window.history.pushState({}, "", `${url.pathname}${url.search}${url.hash}`);
}

export interface TimeMachineValue {
  /** The instant being viewed, or null while Live. Seconds precision, so it
   *  is exactly the instant `?at=` and every request carry. */
  at: Date | null;
  isLive: boolean;
  /** engage moves the whole console to `d` (clamped to now, ignored if
   *  invalid) and makes the view shareable by pushing `?at=`. */
  engage: (d: Date) => void;
  /** returnToLive drops `?at=`. A no-op while already Live — no history churn
   *  for a button press that changes nothing. */
  returnToLive: () => void;
}

const TimeMachineContext = createContext<TimeMachineValue | null>(null);

export function TimeMachineProvider({ children }: { children: ReactNode }) {
  const [at, setAt] = useState<Date | null>(readAtFromLocation);

  useEffect(() => {
    // Back/forward must move time honestly: re-read the param rather than
    // trusting any state we stashed in the history entry.
    const onPopState = () => setAt(readAtFromLocation());
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  const engage = useCallback((d: Date) => {
    const next = normalize(d, "engage()");
    if (!next) return;
    writeAt(next);
    setAt(next);
  }, []);

  // The `at` closure rather than a setAt updater: an updater can be invoked
  // twice under StrictMode, and writeAt is a side effect (two history entries
  // for one click).
  const returnToLive = useCallback(() => {
    if (at === null) return;
    writeAt(null);
    setAt(null);
  }, [at]);

  const value = useMemo<TimeMachineValue>(
    () => ({ at, isLive: at === null, engage, returnToLive }),
    [at, engage, returnToLive],
  );

  return <TimeMachineContext.Provider value={value}>{children}</TimeMachineContext.Provider>;
}

export function useTimeMachine(): TimeMachineValue {
  const ctx = useContext(TimeMachineContext);
  if (!ctx) throw new Error("useTimeMachine must be used within TimeMachineProvider");
  return ctx;
}

/** LIVE is the value every consumer sees with no provider above it. Frozen and
 *  module-level so its identity is stable — a fresh object per render would
 *  invalidate every useMemo/useQuery key that closes over it. */
const LIVE: TimeMachineValue = Object.freeze({
  at: null,
  isLive: true,
  engage: () => {},
  returnToLive: () => {},
});

/**
 * useTimeContext is useTimeMachine's READ-side twin, and the hook every Task-11
 * data surface (hooks/use-topology, use-matrix, the cards, Explore, the Live
 * scrollback) actually calls.
 *
 * The one difference is what happens with no provider above it: useTimeMachine
 * throws, this resolves to Live. That is a distinction between two roles, not a
 * relaxation of one. The BAR must throw — a control that cannot engage anything
 * is a wiring bug and should say so loudly. A data hook is asking a different
 * question, "what instant am I resolving?", and with no Time Machine mounted
 * the honest answer is genuinely "now": a card rendered inside a subtree that
 * carries no provider must show live data, not crash. The provider is mounted
 * once, at AppShell (routes.tsx), above every page's Outlet, so every real page
 * gets the real context and this fallback never fires in the app.
 */
export function useTimeContext(): TimeMachineValue {
  return useContext(TimeMachineContext) ?? LIVE;
}

/**
 * useWritesDisabled is the ONE place the Time Machine's write-blocking rule
 * lives (plan Decision 8: a frontend affordance, not a server mode — every
 * mutation is already authz-gated server-side, so this prevents CONFUSION, not
 * abuse).
 *
 * Two gates on the same buttons look alike and must never be conflated:
 *
 *   PERMISSIONS HIDE. A control the subject may never use is not rendered at
 *   all (PAGES.md:126-129, and every `can(...) ? <Button/> : null` in this
 *   codebase). Its absence is permanent for that subject.
 *
 *   TIME DISABLES. A control the subject MAY use, just not from a historical
 *   view, stays VISIBLE and goes `disabled`. Hiding it would read as "you lost
 *   the permission" and send an operator hunting for an RBAC bug that does not
 *   exist; leaving it in place, greyed, says "this is yours, but not from
 *   here", and the single top-bar banner (Task 10) is the whole explanation —
 *   deliberately no per-button tooltip.
 *
 * So the composition is always `permission ? <Button disabled={writesDisabled}
 * .../> : null`, never the other way round.
 *
 * Task 12's annotation-create affordance inherits this exact pattern: call this
 * hook, spread the boolean onto the control's `disabled`, add no copy.
 */
export function useWritesDisabled(): boolean {
  return !useTimeContext().isLive;
}
