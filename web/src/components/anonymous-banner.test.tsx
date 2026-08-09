import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AnonymousBanner } from "@/components/anonymous-banner";
import { ThemeProvider } from "@/components/theme-provider";
import { NAV_ITEMS } from "@/nav";
import { AppShell } from "@/routes";

test("shows the anonymous-mode warning", () => {
  render(<AnonymousBanner />);
  expect(screen.getByRole("status")).toHaveTextContent(/anonymous mode/i);
  expect(screen.getByRole("status")).toHaveTextContent(/do not use in production/i);
});

// task-19-brief.md: "anonymous-banner.tsx keeps its exact M2 behaviour when
// mode === 'anonymous' and renders nothing otherwise" — additive, the test
// above is untouched.
describe("AnonymousBanner mode prop", () => {
  it("hides for a non-anonymous mode", () => {
    render(<AnonymousBanner mode="local" />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("shows for mode=anonymous, same as the no-prop default", () => {
    render(<AnonymousBanner mode="anonymous" />);
    expect(screen.getByRole("status")).toHaveTextContent(/anonymous mode/i);
  });
});

/**
 * TestAnonymousModeRendersExactlyLikeM2 (task-19-brief.md's checklist name,
 * kept verbatim as this file's own test name for traceability): with GET
 * /api/v1/config and GET /api/v1/auth/me both reporting anonymous, the
 * shell (sidebar + banner + page frame) renders the same structure M2
 * shipped — nav items present, the M2 banner copy present, and no
 * auth-only UI (a user menu) — which is the frontend half of the
 * degraded-state guarantee (Phase B checkpoint: "auth.mode=anonymous ...
 * still serves the entire M1/M2 surface with no credentials").
 */
describe("TestAnonymousModeRendersExactlyLikeM2", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("renders the shell identically to M2 for auth.mode=anonymous", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        if (String(url).includes("/api/v1/auth/me")) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                subject: { kind: "anonymous", id: "anonymous", displayName: "Anonymous", groups: [], roles: ["viewer"] },
                permissions: [],
              }),
              { status: 200, headers: { "Content-Type": "application/json" } },
            ),
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({
              auth: { mode: "anonymous", role: "viewer", loginPath: "" },
              anonymousBanner: true,
              controller: { configured: true },
              prometheus: { configured: true },
              database: { configured: false },
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        );
      }),
    );
    // AppSidebar's NavLinks use TanStack Router's <Link>, which needs a real
    // RouterProvider (it dereferences the router from context with no null
    // guard — jsdom aside, there is no "render without a router" mode). This
    // build mirrors routes.tsx's own root route (AppShell wrapping Outlet)
    // with one child per NAV_ITEMS path so every <Link to="..."> resolves,
    // using a memory history rather than the app's real router/history so
    // this test cannot bleed location state into any other file.
    const testRoot = createRootRoute({
      component: () => (
        <AppShell>
          <Outlet />
        </AppShell>
      ),
    });
    const testRoutes = NAV_ITEMS.map((item) =>
      createRoute({ getParentRoute: () => testRoot, path: item.path, component: () => <div>page content</div> }),
    );
    const testRouter = createRouter({
      routeTree: testRoot.addChildren(testRoutes),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <RouterProvider router={testRouter} />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    // Banner: exact M2 copy, still shown.
    expect(await screen.findByRole("status")).toHaveTextContent(/anonymous mode/i);
    // Sidebar: nav items present, unaffected by auth.
    expect(screen.getByRole("link", { name: /overview/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /live/i })).toBeInTheDocument();
    // No user menu — the M2 static footer text is what shows instead.
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
    expect(screen.getByText(/network connectivity console/i)).toBeInTheDocument();
    // Outlet content still renders through.
    expect(screen.getByText("page content")).toBeInTheDocument();
  });
});
