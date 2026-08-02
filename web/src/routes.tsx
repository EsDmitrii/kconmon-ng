import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from "@tanstack/react-router";
import { NAV_ITEMS } from "@/nav";
import { AppSidebar } from "@/components/app-sidebar";
import { AnonymousBanner } from "@/components/anonymous-banner";
import { StubPage } from "@/components/stub-page";
import { OverviewPage } from "@/pages/overview";
import { LivePage } from "@/pages/live";
import { MatrixPage } from "@/pages/matrix";
import { TopologyPage } from "@/pages/topology";
import { ExplorePage } from "@/pages/explore";
import { PromQLConsolePage } from "@/pages/promql-console";

const rootRoute = createRootRoute({
  component: () => (
    <div className="flex h-screen w-screen overflow-hidden">
      <AppSidebar />
      <div className="flex flex-1 flex-col overflow-hidden">
        <AnonymousBanner />
        <main className="flex-1 overflow-auto">
          <Outlet />
        </main>
      </div>
    </div>
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
              : item.path === "/explore"
                ? ExplorePage
                : item.path === "/console"
                  ? PromQLConsolePage
                  : () => <StubPage title={item.label} description={item.description} />,
  }),
);

const routeTree = rootRoute.addChildren(routes);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
