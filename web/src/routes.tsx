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
import { StubPage } from "@/components/stub-page";
import { TimeMachineBar } from "@/components/timemachine-bar";
import { getConfig } from "@/lib/api";
import { TimeMachineProvider } from "@/lib/timemachine";
import { OverviewPage } from "@/pages/overview";
import { LivePage } from "@/pages/live";
import { MatrixPage } from "@/pages/matrix";
import { TopologyPage } from "@/pages/topology";
import { DiagnosticsPage } from "@/pages/diagnostics";
import { ExplorePage } from "@/pages/explore";
import { PromQLConsolePage } from "@/pages/promql-console";
import { LoginPage } from "@/pages/login";
import { MTRPage } from "@/pages/mtr";
import { NodeCardPage } from "@/pages/node-card";
import { PairCardPage } from "@/pages/pair-card";
import { RunDetailPage } from "@/pages/run-detail";
import { TargetCardPage } from "@/pages/target-card";
import { TargetsPage } from "@/pages/targets";

/**
 * AppShell is the chrome around every route's Outlet: sidebar, anonymous-
 * mode banner, page frame. Split out from the root route's inline component
 * so a test can drive it with its own minimal router/history (AppSidebar's
 * <Link>s still need a real RouterProvider — there is no context-free render
 * mode) instead of the app's real one, and check the shell renders
 * identically to M2 — see anonymous-banner.test.tsx's
 * TestAnonymousModeRendersExactlyLikeM2.
 *
 * `["config"]` is the same query key useDatabaseAvailable
 * (hooks/use-capabilities.ts) already reads with the same `staleTime:
 * Infinity` — one shared cache entry, not a second fetch.
 */
export function AppShell({ children }: { children: ReactNode }) {
  const { data: config } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  return (
    // TimeMachineProvider wraps the shell rather than sitting up in main.tsx
    // next to QueryClientProvider/ThemeProvider: AppShell is the root route's
    // component, so this is still ABOVE every page's Outlet (all of them see
    // the context), and it keeps the provider inside the one unit tests already
    // drive with their own router (anonymous-banner.test.tsx's shell test) —
    // main.tsx has no test seam at all.
    <TimeMachineProvider>
      <div className="flex h-screen w-screen overflow-hidden">
        <AppSidebar />
        <div className="flex flex-1 flex-col overflow-hidden">
          <AnonymousBanner mode={config?.auth.mode} />
          <TimeMachineBar />
          <main className="flex-1 overflow-auto">{children}</main>
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
                        : () => <StubPage title={item.label} description={item.description} />,
  }),
);

// /login is deliberately not in NAV_ITEMS (it has no sidebar entry) and not
// gated by auth: it renders inside the same AppShell chrome as every other
// route, and decides its own content (form / SSO button / redirect home)
// from GET /api/v1/config — see pages/login.tsx.
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: LoginPage,
});

// /diagnostics/runs/$runId (the run permalink, task-24-brief.md) is, same as
// /login, deliberately not in NAV_ITEMS -- it has no sidebar entry, only
// links from pages/diagnostics.tsx's history rows and a run's own POST
// /api/v1/runs redirect. RunDetailPage reads the id itself off
// window.location.pathname (pages/run-detail.tsx's runIdFromPath, the same
// convention LoginPage already uses for ?returnTo=) rather than through this
// route's own $runId param -- this route's only job is to make the URL
// resolve instead of falling through to a 404.
const runDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/diagnostics/runs/$runId",
  component: RunDetailPage,
});

// /nodes/$nodeName and /pairs/$source/$destination (task-25-brief.md's Node
// and Pair object cards) are, same as /diagnostics/runs/$runId above, not in
// NAV_ITEMS -- no sidebar entry, only reachable by clicking a node in
// Topology or a cell in Matrix. NodeCardPage/PairCardPage read their own id(s)
// off window.location.pathname (nodeNameFromPath/pairFromPath) rather than
// through these routes' own path params, the same convention runDetailRoute
// established -- these routes exist only to make the URL resolve instead of
// falling through to a 404.
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

// /targets/$id (M4's Target object card, task-20-brief.md) follows the same
// pattern as the three detail routes above: not in NAV_ITEMS -- no sidebar
// entry, only reachable from a row on the Targets page or from a bookmark --
// and TargetCardPage reads its own id off window.location.pathname
// (targetIdFromPath) rather than through this route's $id param. It is a
// SIBLING of the "/targets" nav route, not a child of it, exactly as
// /diagnostics/runs/$runId is a sibling of "/diagnostics": this route's only
// job is to make the URL resolve instead of falling through to a 404.
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
