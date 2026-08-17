import type { ReactNode } from "react";
import { TimeMachineControl } from "@/components/timemachine-control";
import { ChartCursorProvider } from "@/lib/chart-cursor";

/* PageShell: a centred, breathing column. No `key` on the inner div: a route
   change already remounts the whole page component, so the entrance animation
   re-runs on its own — keying on the translated title only forced an EXTRA
   remount on a language switch, discarding in-progress page state.

   It is also where the shared time cursor is scoped: "a page is the sync group"
   costs nothing to say here and means no page has to opt in to it. */
export function PageShell({ title, description, actions, timeMachine = false, children }: {
  title: string;
  description?: string;
  actions?: ReactNode;
  /** Set by a page that RESOLVES ITS READS through `?at=` — the pages that call
   *  lib/timemachine's useTimeContext. Opt-in, and default false, because the
   *  safe failure is a missing control on a new page rather than a control that
   *  offers a past the page then ignores (owner report: Targets, Alerting and
   *  Settings all carried one and none of them honour it). */
  timeMachine?: boolean;
  children: ReactNode;
}) {
  return (
    /* px-4 below 640px: at 375px the old px-8 spent 4rem of a 23.4rem viewport
       on margin, which is what pushed the wide panels into a page-level
       horizontal scroll (QA scope 2, finding #16). */
    <div className="mx-auto w-full max-w-[1440px] px-4 py-8 sm:px-8 lg:px-10">
      <div className="page-enter flex flex-col gap-7">
        <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
          <div className="min-w-0">
            <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
            {description ? (
              <p className="mt-1 max-w-prose text-sm text-muted-foreground">{description}</p>
            ) : null}
          </div>
          {/* The Time Machine sits with the page's own time filters rather than
              in the chrome: the range presets pick how long the window is, this
              picks where it ends, and the reader looking for "deeper than 24h"
              is looking HERE (owner report). It renders nothing where there is
              no TimeMachineProvider, so a page rendered on its own is unchanged. */}
          {actions || timeMachine ? (
            <div className="flex flex-wrap items-center gap-2">
              {actions}
              {timeMachine ? <TimeMachineControl /> : null}
            </div>
          ) : null}
        </div>
        <ChartCursorProvider>{children}</ChartCursorProvider>
      </div>
    </div>
  );
}
