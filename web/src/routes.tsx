import type { ReactNode } from "react";
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { NAV_ITEMS } from "@/nav";
import { AppSidebar } from "@/components/app-sidebar";
import { AnonymousBanner } from "@/components/anonymous-banner";
import { CommandPalette } from "@/components/command-palette";
import { NavDrawer } from "@/components/nav-drawer";
import { StubPage } from "@/components/stub-page";
import { TimeMachineBar } from "@/components/timemachine-bar";
import { getConfig } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { TimeMachineProvider } from "@/lib/timemachine";
import { cn } from "@/lib/utils";
import { AlertingPage } from "@/pages/alerting";
import { OverviewPage } from "@/pages/overview";
import { LivePage } from "@/pages/live";
import { MatrixPage } from "@/pages/matrix";
import { TopologyPage } from "@/pages/topology";
import { DiagnosticsPage } from "@/pages/diagnostics";
import { ExplorePage } from "@/pages/explore";
import { InvestigatePage } from "@/pages/investigate";
import { PromQLConsolePage } from "@/pages/promql-console";
import { LoginPage } from "@/pages/login";
import { MTRPage } from "@/pages/mtr";
import { NodeCardPage } from "@/pages/node-card";
import { PairCardPage } from "@/pages/pair-card";
import { RunDetailPage } from "@/pages/run-detail";
import { SettingsPage } from "@/pages/settings";
import { TargetCardPage } from "@/pages/target-card";
import { TargetsPage } from "@/pages/targets";

/**
 * AppShell is the chrome around every route's Outlet: sidebar, anonymous-mode banner; split out
 * from the root route's inline component so a test can drive it with its own minimal router/history
 * (AppSidebar's <Link>s still need a real RouterProvider — there is no context-free render mode)
 * instead of the app's real one, and check the shell renders identically.
 */
export function AppShell({ children }: { children: ReactNode }) {
  const { data: config } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const t = useT(chromeDict);
  return (
    // TimeMachineProvider wraps the shell rather than sitting up in main.tsx next to
    // QueryClientProvider/ThemeProvider.
    <TimeMachineProvider>
      {/* The ⌘K palette (M7 Task 9, plan Decision 8) mounts HERE rather than in
          main.tsx: it reads the Time Machine (Return to Live, and the
          DISABLE=time treatment on its create actions), so it has to sit
          inside this provider — and mounting it in the shell means the one
          test seam that already drives AppShell drives the palette too.
          It renders null until a hotkey opens it, so the shell's existing
          pinned structure is untouched. */}
      <CommandPalette />
      {/* Twelve nav links plus the theme toggle and the user menu sit ahead of
          the page on EVERY route, so a keyboard user pays for the sidebar once
          per navigation. This is the standard escape hatch, and it is the
          first thing Tab reaches: off-screen until focused (Tailwind's
          sr-only / focus:not-sr-only pair), so the shell's pinned M2 layout is
          unchanged for everyone else. */}
      <a
        href="#main-content"
        className={cn(
          "sr-only focus:not-sr-only",
          "focus:fixed focus:left-3 focus:top-3 focus:z-50 focus:rounded-md focus:bg-popover focus:px-3 focus:py-2",
          "focus:text-[13px] focus:text-foreground focus:shadow-card focus:outline-none focus:ring-2 focus:ring-ring",
        )}
      >
        {t("shell.skipToContent")}
      </a>
      <div className="flex h-screen w-screen overflow-hidden">
        {/* Two renderings of ONE sidebar, and CSS picks: the column exists from
            768px up, the drawer's trigger below it. A fixed 16rem column left a
            375px viewport with 6rem of page and a horizontal scroll on every
            route (QA scope 2, finding #16); `min-w-0` is the other half of that
            fix — without it a wide child (the matrix grid, a run's table) sets
            the flex item's floor and the whole shell scrolls sideways instead
            of the one panel that is actually too wide. */}
        <div className="hidden md:flex">
          <AppSidebar />
        </div>
        <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
          <div className="flex items-center gap-2 px-3 pt-3 md:hidden">
            <NavDrawer />
            <span className="text-[15px] font-semibold tracking-tight">kconmon-ng</span>
          </div>
          <AnonymousBanner mode={config?.auth.mode} role={config?.auth.role} />
          <TimeMachineBar />
          {/* tabIndex -1 so the skip link's jump actually MOVES focus rather
              than only scrolling: a <main> is not focusable on its own, and a
              fragment jump to a non-focusable target leaves the keyboard back
              in the sidebar on the next Tab. */}
          <main id="main-content" tabIndex={-1} className="flex-1 overflow-auto outline-none">
            {children}
          </main>
        </div>
      </div>
    </TimeMachineProvider>
  );
}

const rootRoute = createRootRoute({
  component: () => (
    <AppShell>
      <Outlet />
    </AppShell>
  ),
});

const routes = NAV_ITEMS.map((item) =>
  createRoute({
    getParentRoute: () => rootRoute,
    path: item.path,
    component:
      item.path === "/"
        ? OverviewPage
        : item.path === "/live"
          ? LivePage
          : item.path === "/matrix"
            ? MatrixPage
            : item.path === "/topology"
              ? TopologyPage
              : item.path === "/mtr"
                ? MTRPage
                : item.path === "/diagnostics"
                  ? DiagnosticsPage
                  : item.path === "/targets"
                    ? TargetsPage
                    : item.path === "/explore"
                      ? ExplorePage
                      : item.path === "/console"
                        ? PromQLConsolePage
                        : item.path === "/investigate"
                          ? InvestigatePage
                          : item.path === "/settings"
                            ? SettingsPage
                            : item.path === "/alerting"
                              ? AlertingPage
                              : () => <StubPage title={item.label} description={item.description} />,
  }),
);

// /login is deliberately not in NAV_ITEMS (it has no sidebar entry) and not gated by auth.
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: LoginPage,
});

// /diagnostics/runs/$runId is, same as /login, deliberately not in NAV_ITEMS -- it has no sidebar
// entry.
const runDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/diagnostics/runs/$runId",
  component: RunDetailPage,
});

// /nodes/$nodeName and /pairs/$source/$destination are, same as /diagnostics/runs/$runId above, not
// in NAV_ITEMS.
const nodeCardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/nodes/$nodeName",
  component: NodeCardPage,
});

const pairCardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/pairs/$source/$destination",
  component: PairCardPage,
});

// /targets/$id follows the same pattern as the three detail routes above: not in NAV_ITEMS -- no
// sidebar entry.
const targetCardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/targets/$id",
  component: TargetCardPage,
});

const routeTree = rootRoute.addChildren([
  ...routes,
  loginRoute,
  runDetailRoute,
  nodeCardRoute,
  pairCardRoute,
  targetCardRoute,
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
