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
import { ThemeProvider } from "@/components/theme-provider";
import { NAV_ITEMS } from "@/nav";
import { AppShell } from "@/routes";

/**
 * M7 Task 12b (plan Decision 12), the shared-shell half of the sweep.
 *
 * The shell puts twelve nav links, a theme toggle and a user menu ahead of the
 * page on EVERY route, and a keyboard user paid that toll on every navigation
 * — the one structural keyboard gap the sweep found in the chrome. These cases
 * pin the escape hatch and the landmark naming; they deliberately do NOT
 * re-assert what TestAnonymousModeRendersExactlyLikeM2
 * (components/anonymous-banner.test.tsx) already owns about this same shell.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function renderShell() {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      if (String(url).includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({
            subject: { kind: "anonymous", id: "anonymous", displayName: "Anonymous", groups: [], roles: ["viewer"] },
            permissions: [],
          }),
        );
      }
      return Promise.resolve(
        json({
          auth: { mode: "anonymous", role: "viewer", loginPath: "" },
          anonymousBanner: true,
          controller: { configured: true },
          prometheus: { configured: true },
          database: { configured: false },
        }),
      );
    }),
  );

  // AppSidebar's NavLinks are TanStack <Link>s and need a real RouterProvider;
  // this mirrors routes.tsx's own root route on a memory history so no
  // location state escapes into another file — anonymous-banner.test.tsx's
  // shell case established this build.
  const testRoot = createRootRoute({
    component: () => (
      <AppShell>
        <Outlet />
      </AppShell>
    ),
  });
  const testRouter = createRouter({
    routeTree: testRoot.addChildren(
      NAV_ITEMS.map((item) =>
        createRoute({ getParentRoute: () => testRoot, path: item.path, component: () => <div>page content</div> }),
      ),
    ),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <RouterProvider router={testRouter} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("AppShell keyboard entry", () => {
  it("offers a skip link ahead of the sidebar", async () => {
    const { container } = renderShell();
    const skip = await screen.findByRole("link", { name: "Skip to main content" });
    expect(skip).toHaveAttribute("href", "#main-content");
    // Ahead of EVERYTHING: the first focusable element in the document, or the
    // sidebar is still in the way and the link buys nothing.
    const focusable = container.querySelectorAll<HTMLElement>("a[href], button, [tabindex]");
    expect(focusable[0]).toBe(skip);
  });

  it("lands on a <main> that can actually take focus", async () => {
    renderShell();
    const main = await screen.findByRole("main");
    expect(main).toHaveAttribute("id", "main-content");
    // A fragment jump to a non-focusable target scrolls but leaves focus in
    // the sidebar, so the next Tab walks the nav again.
    expect(main).toHaveAttribute("tabindex", "-1");
  });

  it("names its navigation landmark", async () => {
    renderShell();
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
  });
});
