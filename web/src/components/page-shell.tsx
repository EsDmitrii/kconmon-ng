import type { ReactNode } from "react";
import { PageHelp } from "@/components/page-help";
import { TimeMachineControl } from "@/components/timemachine-control";
import { ChartCursorProvider } from "@/lib/chart-cursor";

/* PageShell: a centred, breathing column. No `key` on the inner div: a route
   change already remounts the whole page component, so the entrance animation
   re-runs on its own — keying on the translated title only forced an EXTRA
   remount on a language switch, discarding in-progress page state.

   It is also where the shared time cursor is scoped: "a page is the sync group"
   costs nothing to say here and means no page has to opt in to it. */
export function PageShell({ title, description, actions, timeMachine = false, variant = "page", help, children }: {
  title: string;
  description?: string;
  actions?: ReactNode;
  /** The "?" after the title (M7-5): `body` is the page's own translated
   *  help.body, `slug` its docs/console/ page. One optional pair rather than
   *  two props so a body cannot ship without its docs link or vice versa.
   *  Nav pages pass it; detail routes (pair/node/target/run) skip it — their
   *  docs chapter is shared and their titles are data, not page names. */
  help?: { body: string; slug: string };
  /** Set by a page that RESOLVES ITS READS through `?at=` — the pages that call
   *  lib/timemachine's useTimeContext. Opt-in, and default false, because the
   *  safe failure is a missing control on a new page rather than a control that
   *  offers a past the page then ignores (owner report: Targets, Alerting and
   *  Settings all carried one and none of them honour it). */
  timeMachine?: boolean;
  /** "page" (default) is the classic centred reading column, unchanged.
   *  "tool" is for full-bleed working surfaces (M4-5: matrix, live feed,
   *  topology, MTR): no max-width and no centring — a slim one-row header
   *  (title + description inline, same action slot) over content that runs
   *  edge-to-edge with minimal padding. The shell has never wrapped children
   *  in a Card (pages own that), so a page adopting "tool" must also drop its
   *  own outer Card or the boxed-in-a-column look stays. */
  variant?: "page" | "tool";
  children: ReactNode;
}) {
  const tool = variant === "tool";
  return (
    /* px-4 below 640px: at 375px the old px-8 spent 4rem of a 23.4rem viewport
       on margin, which is what pushed the wide panels into a page-level
       horizontal scroll (QA scope 2, finding #16). */
    <div className={tool ? "w-full px-3 py-4 sm:px-4" : "mx-auto w-full max-w-[1440px] px-4 py-8 sm:px-8 lg:px-10"}>
      <div className={tool ? "page-enter flex flex-col gap-4" : "page-enter flex flex-col gap-7"}>
        <div
          className={
            tool
              ? "flex flex-wrap items-center justify-between gap-x-6 gap-y-2"
              : "flex flex-wrap items-start justify-between gap-x-6 gap-y-3"
          }
        >
          {/* The "?" is inserted CONDITIONALLY in both variants: with no `help`
              the header DOM stays byte-for-byte what it was before M7-5, which
              is the contract the variant tests pin. */}
          {tool ? (
            <div className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-0.5">
              <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
              {help ? (
                /* self-center, not the row's baseline: an icon button has no
                   text baseline worth aligning to. */
                <span className="self-center">
                  <PageHelp body={help.body} slug={help.slug} />
                </span>
              ) : null}
              {description ? <p className="text-sm text-muted-foreground">{description}</p> : null}
            </div>
          ) : (
            <div className="min-w-0">
              {help ? (
                /* flex-wrap for the same reason every action cluster carries it
                   (targets #1): at 375px a long title takes a second line
                   rather than pushing the "?" into a horizontal scroll. */
                <div className="flex flex-wrap items-center gap-1.5">
                  <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
                  <PageHelp body={help.body} slug={help.slug} />
                </div>
              ) : (
                <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
              )}
              {description ? (
                <p className="mt-1 max-w-prose text-sm text-muted-foreground">{description}</p>
              ) : null}
            </div>
          )}
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
