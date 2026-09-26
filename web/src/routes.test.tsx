import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { stubViewport } from "@/lib/viewport-stub";
import { NAV_ITEMS } from "@/nav";
import { AppShell, routeTree } from "@/routes";

/** The shell puts twelve nav links, a theme toggle and a user menu ahead of the page on EVERY route. */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const SUBJECTS = {
  user: { kind: "user", id: "ada", displayName: "Ada", groups: [], roles: ["viewer"] },
  anonymous: { kind: "anonymous", id: "anonymous", displayName: "Anonymous", groups: [], roles: ["viewer"] },
} as const;

/** `config` is GET /config's answer: an auth mode, never answering, or a 500. The subject is the
 *  anonymous one under an anonymous config and a signed-in user otherwise, unless given. */
function renderShell(
  config: "anonymous" | "local" | "pending" | "500" = "anonymous",
  subject: keyof typeof SUBJECTS = config === "anonymous" ? "anonymous" : "user",
) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json({ subject: SUBJECTS[subject], permissions: [] }));
      if (href.includes("/api/v1/config") && config === "pending") return new Promise<Response>(() => {});
      if (href.includes("/api/v1/config") && config === "500") {
        return Promise.resolve(
          new Response(JSON.stringify({ type: "about:blank", title: "boom", status: 500 }), {
            status: 500,
            headers: { "Content-Type": "application/problem+json" },
          }),
        );
      }
      return Promise.resolve(
        json({
          auth:
            config === "anonymous"
              ? { mode: "anonymous", role: "viewer", loginPath: "" }
              : { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
          anonymousBanner: config === "anonymous",
          controller: { configured: true },
          prometheus: { configured: true },
          database: { configured: false },
        }),
      );
    }),
  );

  // AppSidebar's NavLinks are TanStack <Link>s and need a real RouterProvider.
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
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <RouterProvider router={testRouter} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...utils, router: testRouter };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/* ── the owner's report: /login showed the whole product before sign-in ──── */

/**
 * An unauthenticated visitor was met by the complete shell — twelve nav links
 * from Overview to Settings, the Time Machine bar, the anonymous banner — with a
 * sign-in card in the middle of it. The product's feature map is not something
 * to hand out before auth, and a page whose every control leads back to itself
 * is not a page.
 *
 * The split is at the ROUTER, not in CSS: /login hangs off the root while every
 * other route hangs off a pathless layout route that IS the shell. A route
 * cannot then acquire the shell by accident, and nothing has to remember to hide
 * anything.
 */
function renderRoute(initialEntry: string) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({
            subject: { kind: "user", id: "ada", displayName: "Ada", groups: [], roles: ["admin"] },
            permissions: [],
          }),
        );
      }
      if (href.includes("/api/v1/config")) {
        return Promise.resolve(
          json({
            auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
            anonymousBanner: false,
            controller: { configured: true },
            prometheus: { configured: true },
            database: { configured: false },
          }),
        );
      }
      return Promise.resolve(json({}));
    }),
  );

  const testRouter = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [initialEntry] }) });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <RouterProvider router={testRouter} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe("/login is a bare page", () => {
  it("shows the sign-in card and NOT the product's feature map", async () => {
    renderRoute("/login");

    expect(await screen.findByLabelText(/username/i)).toBeInTheDocument();
    // Not one nav link, and not the landmark that would hold them.
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    for (const item of NAV_ITEMS) {
      expect(screen.queryByRole("link", { name: item.label })).not.toBeInTheDocument();
    }
  });

  it("carries no Time Machine — there is nothing signed in to look back over", async () => {
    renderRoute("/login");
    await screen.findByLabelText(/username/i);
    expect(screen.queryByRole("button", { name: /Time Machine/i })).not.toBeInTheDocument();
  });

  it("names the product, so the card is not floating in an unlabelled box", async () => {
    renderRoute("/login");
    await screen.findByLabelText(/username/i);
    expect(screen.getByText("kconmon-ng")).toBeInTheDocument();
  });

  it("keeps the theme toggle — a display preference is not a feature to withhold", async () => {
    renderRoute("/login");
    await screen.findByLabelText(/username/i);
    expect(screen.getByRole("button", { name: /switch to (light|dark) theme/i })).toBeInTheDocument();
  });

  it("leaves ?returnTo= where the sign-in reads it from", async () => {
    // The deep link an unauthenticated hit on /matrix produced: lib/api.ts's
    // redirectToLogin writes it onto the real URL, and pages/login.tsx reads it
    // back from there. The split moved the chrome, not that contract.
    window.history.pushState({}, "", "/login?returnTo=%2Fmatrix");
    renderRoute("/login");
    await screen.findByLabelText(/username/i);
    expect(new URLSearchParams(window.location.search).get("returnTo")).toBe("/matrix");
    window.history.pushState({}, "", "/");
  });
});

describe("every signed-in route still gets the shell", () => {
  it("mounts the navigation, the skip link and the main landmark on /", async () => {
    renderRoute("/");
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Skip to main content" })).toBeInTheDocument();
    expect(screen.getByRole("main")).toHaveAttribute("id", "main-content");
  });
});

describe("AppShell in auth.mode=anonymous", () => {
  it("shows the banner and the static footer, and no user menu", async () => {
    renderShell("anonymous");
    expect(await screen.findByRole("status")).toHaveTextContent(/anonymous mode/i);
    expect(screen.getByRole("link", { name: /overview/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /events/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Anonymous" })).not.toBeInTheDocument();
    expect(screen.getByText(/network connectivity console/i)).toBeInTheDocument();
    expect(screen.getByText("page content")).toBeInTheDocument();
  });
});

/* The shell only knows the mode once /config answers; a signed-in user must not see the
   authentication-disabled warning while it is in flight or after it failed. */
describe("AppShell banner before /config answers", () => {
  it.each(["pending", "500"] as const)("shows no anonymous warning to a signed-in user while /config is %s", async (config) => {
    renderShell(config, "user");
    // The sidebar's user menu appears once /auth/me has answered, which is all the banner waits on.
    await screen.findByRole("button", { name: "Ada" });
    expect(screen.queryByText(/anonymous mode/i)).not.toBeInTheDocument();
  });

  it("still warns an anonymous session whose /config has not answered", async () => {
    renderShell("pending", "anonymous");
    expect(await screen.findByText(/anonymous mode/i)).toBeInTheDocument();
  });
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

/* ── M4-7: keyboard scrolling must reach the content pane ────────────────── */

/**
 * Live walkthrough evidence: End/PageDown scrolled the SIDEBAR. The document
 * never scrolls (the shell div is overflow-hidden; only <main> is a scroller),
 * and keyboard scroll keys act on the focused element's nearest scrollable
 * ancestor — after a nav click focus is still on the sidebar link, whose
 * scrollable ancestor is the sidebar's own overflow-y-auto <nav>. The shell
 * must hand focus to <main> on every route change so those keys scroll the
 * page.
 */
describe("keyboard scrolling reaches the content pane", () => {
  it("moves focus to <main> after a navigation, so PageDown/End scroll the page", async () => {
    renderShell();
    const link = await screen.findByRole("link", { name: "Events" });
    // The walkthrough's state: the user reached the sidebar by keyboard, so
    // focus sits on the link they activate.
    link.focus();
    fireEvent.click(link);
    await waitFor(() => expect(screen.getByRole("main")).toHaveFocus());
  });

  it("does NOT steal focus on the initial render — Tab still lands on the skip link first", async () => {
    renderShell();
    await screen.findByRole("main");
    expect(document.activeElement).toBe(document.body);
  });

  it("leaves focus alone when a click does not change the route", async () => {
    renderShell();
    // "/" is the initial entry, so Overview navigates nowhere; yanking focus
    // to <main> on a same-path click (or a query-only change like ?at=) would
    // fight the element the user is actually on.
    const link = await screen.findByRole("link", { name: "Overview" });
    link.focus();
    fireEvent.click(link);
    expect(link).toHaveFocus();
  });
});

/* ── the password dialog outlives the drawer that opened it ──────────────── */

describe("Change password opened from the narrow-viewport drawer", () => {
  async function openFromDrawer() {
    const viewport = stubViewport(390);
    renderShell("local");
    fireEvent.click(await screen.findByRole("button", { name: "Open navigation" }));
    const drawer = await screen.findByRole("dialog", { name: "Navigation" });
    fireEvent.click(await within(drawer).findByRole("button", { name: "Ada" }));
    fireEvent.click(await within(drawer).findByRole("button", { name: "Change password" }));
    const dialog = await screen.findByRole("dialog", { name: "Change password" });
    fireEvent.change(within(dialog).getByLabelText("Current password"), { target: { value: "old-secret-1" } });
    return { viewport, dialog };
  }

  /* A phone rotated past md closes the drawer, and the drawer's sidebar is the one that held the
     dialog: typed passwords and a submit in flight used to vanish with it. */
  it("keeps the dialog and what was typed when the drawer closes at md", async () => {
    const { viewport } = await openFromDrawer();

    viewport.resize(844);
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull());

    const dialog = screen.getByRole("dialog", { name: "Change password" });
    expect(within(dialog).getByLabelText("Current password")).toHaveValue("old-secret-1");
  });

  /* The user-menu trigger that opened the dialog went with the drawer, and focus fell to <body>, so
     the next Tab started over at the skip link. */
  it("puts focus on the page when the drawer that opened the dialog is gone", async () => {
    const { viewport, dialog } = await openFromDrawer();
    viewport.resize(844);
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull());

    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Change password" })).toBeNull());
    expect(screen.getByRole("main")).toHaveFocus();
  });

  /* Escape belongs to the top layer: the drawer's document-level listener used to take it first and
     close the drawer, and the dialog with it. */
  it("closes only the dialog on Escape, leaving the drawer open", async () => {
    const { dialog } = await openFromDrawer();

    fireEvent.keyDown(within(dialog).getByLabelText("Current password"), { key: "Escape" });

    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Change password" })).toBeNull());
    expect(screen.getByRole("dialog", { name: "Navigation" })).toBeInTheDocument();
  });
});

/* ── the drawer does not outlive the page it was opened over ─────────────── */

/* Back/Forward, the Android back gesture and a ⌘K navigation change the route without a drawer link,
   and the drawer stayed open over the new page while the shell moved focus to <main> behind it. */
describe("the narrow-viewport drawer on a navigation it did not start", () => {
  it("closes, and focus lands on the new page", async () => {
    stubViewport(390);
    const { router } = renderShell();
    fireEvent.click(await screen.findByRole("button", { name: "Open navigation" }));
    await screen.findByRole("dialog", { name: "Navigation" });

    act(() => router.history.push("/live"));

    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull());
    expect(screen.getByRole("main")).toHaveFocus();
  });
});
