import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { useT } from "@/lib/i18n";
import { sharedDict } from "@/lib/i18n/dict/shared";

/** Time Machine — the console-wide global time context. */

/** AT_PARAM is the one URL key the Time Machine owns. */
export const AT_PARAM = "at";

/**
 * RFC 3339 date-time: a full instant with an offset. Deliberately strict —
 * `new Date()` alone happily accepts "2026", "2026-08-07" and browser-specific
 * shapes, and the console would then disagree with the Go server (time.RFC3339)
 * about what a shared link means. Anything else degrades to Live.
 */
const RFC3339 = /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/;

/** formatAtParam renders the instant the API takes: RFC 3339. */
export function formatAtParam(d: Date): string {
  return new Date(truncateToSecond(d.getTime())).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function truncateToSecond(ms: number): number {
  return Math.floor(ms / 1000) * 1000;
}

/**
 * normalize is the single gate every candidate instant passes: invalid → null (Live); `GET
 * /api/v1/topology?at=` answers 400 for a future `at`.
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
 * readAtFromLocation resolves the current URL into a time context; it is the whole init story AND
 * the whole popstate story — one function.
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

/** atHref renders the current URL with ONLY the at param replaced, preserving
 *  pathname, hash and every other query param. */
function atHref(d: Date | null): { href: string; unchanged: boolean } {
  const url = new URL(window.location.href);
  const current = url.searchParams.get(AT_PARAM);
  const next = d ? formatAtParam(d) : null;
  if (next) url.searchParams.set(AT_PARAM, next);
  else url.searchParams.delete(AT_PARAM);
  return { href: `${url.pathname}${url.search}${url.hash}`, unchanged: current === next };
}

/**
 * withAtParam carries the instant being viewed onto an in-app href.
 *
 * A handful of links are plain <a href> rather than router <Link>s (they sit in panels that render
 * outside a router in their own tests). A raw anchor is a full document load, so the provider
 * unmounts and readAtFromLocation sees no ?at= — the reader was silently dropped back into Live in
 * the middle of an investigation. The param travels with the link instead.
 */
export function withAtParam(path: string): string {
  const at = readAtFromLocation();
  if (!at) return path;
  const [base, hash = ""] = path.split("#");
  const [pathname, search = ""] = base.split("?");
  const params = new URLSearchParams(search);
  params.set(AT_PARAM, formatAtParam(at));
  return `${pathname}?${params.toString()}${hash ? `#${hash}` : ""}`;
}

/**
 * traversing is true for the moment a Back/Forward is being delivered.
 *
 * A traversal is the URL winning over the console: the provider re-reads `?at=` from the entry the
 * reader landed on. AtParamSync must NOT re-assert the instant React is still holding over it —
 * doing so stamped the old instant back onto that entry with replaceState, which both kept the
 * console engaged and destroyed the entry, so Back could never leave the past again.
 *
 * Registered at MODULE LOAD, which puts it ahead of the router's own popstate listener: routes.tsx
 * imports this module before it calls createRouter(). The flag is therefore up before anything the
 * router notifies can queue a write, and down again on the next macrotask.
 */
let traversing = false;
if (typeof window !== "undefined") {
  window.addEventListener("popstate", () => {
    traversing = true;
    setTimeout(() => {
      traversing = false;
    }, 0);
  });
}

/**
 * writeAt is the OPERATOR's own move, so it pushes: Back/Forward walk the time context.
 *
 * The router keeps its entry index in history.state and its traversal arithmetic reads it, so a
 * push carries the NEXT index rather than an empty object (which erased the index) or a copy of the
 * current one (which would give two entries the same index).
 */
function writeAt(d: Date | null): void {
  const prev = window.history.state as { __TSR_index?: number } | null;
  const next = typeof prev?.__TSR_index === "number" ? { ...prev, __TSR_index: prev.__TSR_index + 1 } : {};
  window.history.pushState(next, "", atHref(d).href);
}

/**
 * syncAtParam makes the URL state what the console is actually showing; replaceState, not push — the
 * operator did not make this correction and must not have to press Back through it.
 *
 * The existing history STATE is carried over rather than replaced with `{}`: the router keeps its
 * own entry index in there, and its back/forward arithmetic reads it.
 */
function syncAtParam(d: Date | null): void {
  const { href, unchanged } = atHref(d);
  if (unchanged) return;
  window.history.replaceState(window.history.state, "", href);
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
  /* The provider paints exactly one string — the sr-only reason below. */
  const t = useT(sharedDict);

  useEffect(() => {
    // Back/forward must move time honestly: re-read the param rather than
    // trusting any state we stashed in the history entry.
    const onPopState = () => setAt(readAtFromLocation());
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  // A ?at= we ignored or clamped left the URL claiming an instant the console
  // is not on — and every reload of that link would resolve differently.
  useEffect(() => {
    syncAtParam(at);
  }, [at]);

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

  return (
    <TimeMachineContext.Provider value={value}>
      {/* The description every time-disabled control points its
          aria-describedby at, mounted ONCE and only while engaged — an
          always-present "Time Machine is engaged" in the accessibility tree
          would be a lie for the whole time the console is Live. */}
      {at ? (
        <span id={TIME_MACHINE_REASON_ID} className="sr-only">
          {t("timemachine.disabledReason")}
        </span>
      ) : null}
      {children}
    </TimeMachineContext.Provider>
  );
}

export function useTimeMachine(): TimeMachineValue {
  const ctx = useContext(TimeMachineContext);
  if (!ctx) throw new Error("useTimeMachine must be used within TimeMachineProvider");
  return ctx;
}

/**
 * useTimeMachineControls is useTimeMachine for a control mounted in shared page furniture, where
 * "no provider" is a real case (a page rendered on its own in a test) rather than a wiring bug:
 * null means render no control at all, never a dead one.
 */
export function useTimeMachineControls(): TimeMachineValue | null {
  return useContext(TimeMachineContext);
}

/**
 * AtParamSync re-asserts `?at=` after a navigation, and renders nothing.
 *
 * The time context is console-wide and survives a route change on purpose, but the router builds
 * the destination URL from the route alone and drops the param this module owns. The address bar
 * then said Live while every read still resolved at `t`, and a link copied from that page handed
 * the reader a different console than the one being looked at (owner report).
 *
 * Two things this has to get right, and the first version got neither:
 *
 *  - It runs LATE. The router notifies React synchronously but writes the real URL in a queued
 *    microtask, so an effect that reads window.location during the commit still sees the OLD
 *    address, decides `?at=` is already there, and writes nothing — and the queued write then
 *    lands without it. Queuing our own write behind the router's is what makes it stick.
 *  - It keys on the whole HREF, not the pathname. `/matrix?at=…` → `/matrix` is a real push (the
 *    router compares full hrefs), and a pathname-keyed effect cannot even see it happen.
 *
 * The href is passed IN because this module knows nothing about the router.
 */
export function AtParamSync({ href }: { href: string }): null {
  const ctx = useContext(TimeMachineContext);
  const at = ctx?.at ?? null;
  useEffect(() => {
    if (!ctx) return;
    let live = true;
    queueMicrotask(() => {
      // Not during a traversal: see `traversing`. The reader walked to that entry
      // on purpose, and the provider is about to read it.
      if (live && !traversing) syncAtParam(at);
    });
    return () => {
      live = false;
    };
  }, [ctx, at, href]);
  return null;
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
 * useTimeContext is useTimeMachine's READ-side twin; the BAR must throw — a control that cannot
 * engage anything is a wiring bug and should say so loudly.
 */
export function useTimeContext(): TimeMachineValue {
  return useContext(TimeMachineContext) ?? LIVE;
}

/**
 * useWritesDisabled is the ONE place the Time Machine's write-blocking rule lives; two gates on the
 * same buttons look alike and must never be conflated: PERMISSIONS HIDE.
 */
export function useWritesDisabled(): boolean {
  return !useTimeContext().isLive;
}

/** The one sentence a time-disabled control gives for itself; READ FROM THE DICTIONARY rather than written here. */
export const TIME_MACHINE_DISABLED_REASON = sharedDict.en["timemachine.disabledReason"];

/** The id the reason paragraph is mounted under, referenced by every
 *  time-disabled control's aria-describedby. One node, however many controls
 *  point at it — the sentence is a property of the MODE, not of the button. */
export const TIME_MACHINE_REASON_ID = "timemachine-writes-disabled-reason";

/**
 * useWriteGuard is the props a TM-disabled control wears; nothing is added while Live: an enabled
 * control with a "why are you disabled" tooltip would be worse than the silence it replaced.
 */
export interface WriteGuard {
  disabled: boolean;
  title?: string;
  "aria-describedby"?: string;
}

export function useWriteGuard(): WriteGuard {
  const disabled = useWritesDisabled();
  /* Called unconditionally, above the branch — a hook cannot hide behind an `if`. */
  const t = useT(sharedDict);
  return disabled
    ? { disabled: true, title: t("timemachine.disabledReason"), "aria-describedby": TIME_MACHINE_REASON_ID }
    : { disabled: false };
}
