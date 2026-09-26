import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { stubViewport } from "@/lib/viewport-stub";
import { NavDrawer } from "./nav-drawer";

/**
 * QA scope 2, finding #16 — below 768px the sidebar never collapsed, so a 375px
 * viewport got 6rem of page and a horizontal scroll on every route.
 *
 * The WIDTH itself is CSS (`hidden md:flex` on the column, `md:hidden` on this
 * trigger) and jsdom applies no stylesheet, so what is testable here is the
 * behaviour the drawer promises once it is open: a real dialog, Escape out,
 * focus in and back, Tab that cannot leave, and a close on navigation.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function renderDrawer() {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        json({
          subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] },
          permissions: [],
        }),
      ),
    ),
  );
  /* AppSidebar renders <Link>s, which need a real router in context. */
  const rootRoute = createRootRoute({ component: NavDrawer });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  /* ThemeProvider too: the sidebar header carries the theme toggle, and useTheme
     (unlike useT) throws outside its provider on purpose. */
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <LocaleProvider>
          <RouterProvider router={router} />
        </LocaleProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

const trigger = () => screen.getByRole("button", { name: "Open navigation" });

beforeEach(() => {
  vi.stubGlobal("scrollTo", () => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("NavDrawer", () => {
  it("starts closed and says so on the trigger", async () => {
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    expect(trigger()).toHaveAttribute("aria-expanded", "false");
    expect(trigger()).toHaveAccessibleName("Open navigation");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens a modal dialog carrying the SAME sidebar the wide layout renders", async () => {
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    const dialog = await screen.findByRole("dialog", { name: "Navigation" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(trigger()).toHaveAttribute("aria-expanded", "true");
    // The nav landmark and its links come from AppSidebar, not from a copy.
    expect(screen.getByRole("navigation", { name: chromeDict.en["shell.nav.aria"] })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Topology" })).toBeInTheDocument();
  });

  it("moves focus into the panel on open and back to the trigger on Escape", async () => {
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    const dialog = await screen.findByRole("dialog");
    expect(document.activeElement).toBe(dialog);

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.activeElement).toBe(trigger());
  });

  it("traps Tab inside the panel — the page behind is not reachable by keyboard", async () => {
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    const dialog = await screen.findByRole("dialog");
    const items = [...dialog.querySelectorAll<HTMLElement>("a[href], button:not([disabled])")];
    expect(items.length).toBeGreaterThan(1);

    // Shift+Tab off the panel itself wraps to the LAST item, not out of it.
    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(document.activeElement).toBe(items[items.length - 1]);

    // And Tab from the last item wraps back to the first.
    fireEvent.keyDown(document, { key: "Tab" });
    expect(document.activeElement).toBe(items[0]);
  });

  /* The open panel covers the trigger, and the scrim is aria-hidden: without a control of its own a
     touch screen reader had no way out but picking a link. */
  it("carries its own labelled close control inside the dialog", async () => {
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Close navigation" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.activeElement).toBe(trigger());
  });

  it("keeps a hidden control out of the Tab cycle, so Tab cannot land on nothing", async () => {
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    const dialog = await screen.findByRole("dialog");
    const items = [...dialog.querySelectorAll<HTMLElement>("a[href], button:not([disabled])")];
    const hidden = items[items.length - 1];
    Object.defineProperty(hidden, "checkVisibility", { configurable: true, value: () => false });

    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(document.activeElement).toBe(items[items.length - 2]);
  });

  it("closes itself on a navigation, rather than covering the page it just loaded", async () => {
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    await screen.findByRole("dialog");
    fireEvent.click(screen.getByRole("link", { name: "Topology" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.activeElement).toBe(trigger());
  });

  /* Widened past md while open, the drawer is display:none but its capture-phase Escape listener
     stayed, and swallowed the next Escape meant for a dialog opened on top of the page. */
  it("closes when the viewport crosses md, so a hidden drawer takes no Escape", async () => {
    const viewport = stubViewport(700);
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    await screen.findByRole("dialog");

    viewport.resize(800);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    const reached = vi.fn();
    document.body.addEventListener("keydown", reached);
    try {
      fireEvent.keyDown(document.body, { key: "Escape" });
      expect(reached).toHaveBeenCalledTimes(1);
    } finally {
      document.body.removeEventListener("keydown", reached);
    }
  });

  /* Tailwind's md is 48rem, and a rem in a media query is the browser's default font size. With
     "Large" text (20px) md is 960px, so an 800px window still shows this trigger and no column. */
  it("opens between 768px and md when the browser font is larger, and closes only at md", async () => {
    const viewport = stubViewport(800, 20);
    renderDrawer();
    await waitFor(() => expect(trigger()).toBeInTheDocument());
    fireEvent.click(trigger());
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(trigger()).toHaveAttribute("aria-expanded", "true");

    viewport.resize(900);
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    viewport.resize(960);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("names itself in the interface language", async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    renderDrawer();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: chromeDict.ru["shell.menu.open"] })).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("button", { name: chromeDict.ru["shell.menu.open"] }));
    expect(await screen.findByRole("dialog", { name: chromeDict.ru["shell.menu.aria"] })).toBeInTheDocument();
  });
});
