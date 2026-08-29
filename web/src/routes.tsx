import { Fragment, useEffect, useRef, type ReactNode } from "react";
import {
  createRootRoute,
  createRoute,
  createRouter,
  Link,
  Outlet,
  useRouterState,
} from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { NAV_ITEMS } from "@/nav";
import { AppSidebar } from "@/components/app-sidebar";
import { AnonymousBanner } from "@/components/anonymous-banner";
import { CommandPalette } from "@/components/command-palette";
import { NavDrawer } from "@/components/nav-drawer";
import { StubPage } from "@/components/stub-page";
import { ThemeToggle } from "@/components/theme-toggle";
import { TimeMachineBar } from "@/components/timemachine-bar";
import { PageShell } from "@/components/page-shell";
import { RouteErrorBoundary } from "@/components/error-boundary";
import { Card } from "@/components/ui/card";
import { getConfig } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { notFoundDict } from "@/lib/i18n/dict/not-found";
import { AtParamSync, TimeMachineProvider } from "@/lib/timemachine";
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
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  /* The WHOLE location for the Time Machine's URL sync: a nav link back to the
     page you are already on changes only the query, and that is exactly the
     navigation that used to drop ?at= unseen. */
  const href = useRouterState({ select: (s) => s.location.href });
  /* PageDown/End scroll the focused element's nearest scrollable ancestor, and
     only <main> scrolls here — after a nav click that ancestor is the sidebar.
     Focus <main> on route change; not on first render (the skip link stays the
     first Tab stop) and not on query-only changes like the ?at= sync. */
  const mainRef = useRef<HTMLElement>(null);
  const lastPathname = useRef(pathname);
  useEffect(() => {
    if (lastPathname.current === pathname) return;
    lastPathname.current = pathname;
    mainRef.current?.focus({ preventScroll: true });
  }, [pathname]);
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
      {/* The router drops ?at= when it builds the next URL; this puts it back,
          so the address bar never claims Live while the console is at `t`. */}
      <AtParamSync href={href} />
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
      {/* dvh, not vh. The shell owns the whole viewport and the DOCUMENT never scrolls — only
          <main> does — so on iOS Safari and Android Chrome the toolbars never collapse, and 100vh
          (the LARGE viewport) is about 110px taller than what is actually visible. The bottom strip
          of every route then sits behind the browser chrome with no scroll that can reach it: the
          pager at the end of a table, the sidebar's own Sign out. h-screen stays as the fallback
          for engines without dvh. */}
      <div className="flex h-screen h-[100dvh] w-screen overflow-hidden">
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
          <main ref={mainRef} id="main-content" tabIndex={-1} className="flex-1 overflow-auto outline-none">
            <RouteErrorBoundary resetKey={pathname}>{children}</RouteErrorBoundary>
          </main>
        </div>
      </div>
    </TimeMachineProvider>
  );
}

/**
 * BareShell is what a visitor who has not signed in gets: the product's name and
 * the card, and nothing else.
 *
 * The owner's report: /login rendered the FULL shell — every nav item from
 * Overview to Settings, the Time Machine bar, the anonymous banner — around a
 * sign-in form. That hands the product's whole feature map to somebody who has
 * not authenticated, and every link in it leads back to the same page.
 *
 * The theme toggle stays. It is a display preference the browser owns (its
 * provider is up in main.tsx, above the router), not a feature of the console,
 * and a visitor who reads on a light screen should not have to sign in first.
 */
export function BareShell({ children }: { children: ReactNode }) {
  return (
    <div className="relative flex min-h-screen w-full flex-col items-center justify-center gap-6 px-4">
      <div className="absolute right-3 top-3">
        <ThemeToggle />
      </div>
      {/* The product's own name, which is a proper noun in both languages —
          the shell's mobile header spells it exactly this way. */}
      <span className="text-[15px] font-semibold tracking-tight">kconmon-ng</span>
      {children}
    </div>
  );
}

/** An address longer than this is a fuzzer's, not an operator's; it is shown
 *  clipped so one pasted line cannot become the whole page. */
export const NOT_FOUND_PATH_LIMIT = 120;

/**
 * NotFoundPage is what an address with no route gets.
 *
 * The router's default is the bare string "Not Found" — untranslated whatever
 * the language switch says, with no title, no explanation of what happened and
 * no way onward. This says which address failed (the reader's own bytes,
 * verbatim and clipped, never interpreted), why that happens, and offers the
 * one link that is certain to work.
 *
 * It renders through PageShell like every other page, so the sidebar, the skip
 * link and the language switch are all still there: a 404 inside a console is a
 * wrong turn, not an exit.
 */
export function NotFoundPage() {
  const t = useT(notFoundDict);
  /* The router's own location rather than window.location: a test drives this
     tree with a memory history, and so does every in-app navigation. */
  const href = useRouterState({ select: (s) => s.location.href });
  const shown = href.length > NOT_FOUND_PATH_LIMIT ? `${href.slice(0, NOT_FOUND_PATH_LIMIT)}…` : href;
  return (
    <PageShell title={t("title")}>
      <Card className="p-6">
        <p className="text-sm">
          {/* The sentence is SPLIT on its own placeholder rather than
              interpolated into one string: the address is monospaced and
              breakable, so a 200-character path wraps inside the card instead
              of widening the page. Both languages put {path} in a different
              place, which is exactly why the split is on the template. */}
          {t("body").split(/(\{path\})/).map((chunk, i) =>
            chunk === "{path}" ? (
              <code key={i} data-testid="not-found-path" className="break-all font-mono text-[12px]">
                {shown}
              </code>
            ) : (
              <Fragment key={i}>{chunk}</Fragment>
            ),
          )}
        </p>
        <p className="mt-2 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("hint")}</p>
        <Link to="/" className="mt-4 inline-block text-sm text-primary hover:underline">
          {t("home")}
        </Link>
      </Card>
    </PageShell>
  );
}

const rootRoute = createRootRoute({
  component: () => <Outlet />,
  /* On the ROOT, so it covers every address the tree does not match — including
     the ones under a path that does exist (/diagnostics/runs with no id,
     /nodes with no name) — and so it is part of the exported routeTree rather
     than of one router instance: a test that builds its own router over that
     tree gets the same 404 the app does.
     Wrapped in the shell HERE rather than relying on where the router chooses
     to render it: a not-found match has no layout route under it, so without
     this the reader loses the sidebar exactly when they need a way out. */
  notFoundComponent: () => (
    <AppShell>
      <NotFoundPage />
    </AppShell>
  ),
});

/**
 * The shell is a PATHLESS LAYOUT ROUTE rather than the root's component, which
 * is the whole of the fix above: a route gets the chrome by hanging off this
 * one, and /login hangs off the root instead. Nothing has to remember to hide
 * anything, and no route can acquire the shell by accident.
 */
const shellRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "shell",
  component: () => (
    <AppShell>
      <Outlet />
    </AppShell>
  ),
});

const routes = NAV_ITEMS.map((item) =>
  createRoute({
    getParentRoute: () => shellRoute,
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

/* /login is deliberately not in NAV_ITEMS (it has no sidebar entry), not gated
   by auth, and — since the owner's report — the one route hanging off the ROOT
   rather than off the shell. */
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: () => (
    <BareShell>
      <LoginPage />
    </BareShell>
  ),
});

// /diagnostics/runs/$runId is, same as /login, deliberately not in NAV_ITEMS -- it has no sidebar
// entry.
const runDetailRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/diagnostics/runs/$runId",
  component: RunDetailPage,
});

// /nodes/$nodeName and /pairs/$source/$destination are, same as /diagnostics/runs/$runId above, not
// in NAV_ITEMS.
const nodeCardRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/nodes/$nodeName",
  component: NodeCardPage,
});

const pairCardRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/pairs/$source/$destination",
  component: PairCardPage,
});

// /targets/$id follows the same pattern as the three detail routes above: not in NAV_ITEMS -- no
// sidebar entry.
const targetCardRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/targets/$id",
  component: TargetCardPage,
});

/* Exported so a test can mount the REAL tree at a path and see which chrome
   that path actually gets, rather than re-declaring the split beside it. */
export const routeTree = rootRoute.addChildren([
  shellRoute.addChildren([
    ...routes,
    runDetailRoute,
    nodeCardRoute,
    pairCardRoute,
    targetCardRoute,
  ]),
  loginRoute,
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
