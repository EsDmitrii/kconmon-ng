import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { AppSidebar } from "./app-sidebar";

/**
 * M3-12 — the command palette existed only as a keystroke: nothing on screen
 * said ⌘K/Ctrl+K, so the feature was undiscoverable outside the docs. The
 * footer now carries a <kbd> hint beside the caption (or the user menu).
 *
 * Same division of labour as lib/i18n/chrome.test.tsx: NO LocaleProvider here,
 * so what this file pins is the English default; the Russian half lives there.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function renderSidebar({ anonymous = false }: { anonymous?: boolean } = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        json({
          subject: anonymous
            ? { kind: "anonymous", id: "anonymous", displayName: "Anonymous", groups: [], roles: ["viewer"] }
            : { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] },
          permissions: [],
        }),
      ),
    ),
  );
  /* AppSidebar renders <Link>s, which need a real router in context. */
  const rootRoute = createRootRoute({ component: AppSidebar });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("AppSidebar — groups and item descriptions (M3-9)", () => {
  it("headers the three groups Watch / Investigate / Configure", async () => {
    renderSidebar({ anonymous: true });
    const nav = await screen.findByRole("navigation");
    for (const name of ["Watch", "Investigate", "Configure"]) {
      expect(within(nav).getByText(name)).toBeInTheDocument();
    }
  });

  it("shows the ACTIVE item's description as a visible second line, tooltip kept everywhere", async () => {
    renderSidebar({ anonymous: true });
    await screen.findByRole("navigation");
    // The memory history starts at "/", so Overview is the active link. The
    // second line is aria-hidden (the title attribute already describes the
    // link), so the accessible NAME stays the bare label.
    const overview = screen.getByRole("link", { name: "Overview" });
    expect(
      within(overview).getByText("Health summary, worst pairs, firing alerts, recent events."),
    ).toBeInTheDocument();
    // An inactive item keeps its description as a tooltip only.
    const events = screen.getByRole("link", { name: "Events" });
    expect(events).toHaveAttribute("title", "Real-time event feed.");
    expect(within(events).queryByText("Real-time event feed.")).not.toBeInTheDocument();
  });
});

describe("AppSidebar — palette hint", () => {
  it("shows the palette hotkey as a <kbd>, with the hint spelled out in the tooltip", async () => {
    renderSidebar({ anonymous: true });
    await screen.findByRole("navigation");
    // jsdom reports no Mac platform, so the hint reads Ctrl+K here.
    const kbd = screen.getByTitle("Ctrl+K — search and commands");
    expect(kbd.tagName).toBe("KBD");
    expect(kbd).toHaveTextContent("Ctrl+K");
    // Next to the product caption, as M3-12 asks — both live in the footer row.
    expect(kbd.closest("div")).toContainElement(screen.getByText("Network connectivity console"));
  });

  it("keeps the hint when a signed-in subject replaces the caption with the user menu", async () => {
    renderSidebar();
    await waitFor(() => expect(screen.getByText("Ada")).toBeInTheDocument());
    expect(screen.getByTitle("Ctrl+K — search and commands")).toHaveTextContent("Ctrl+K");
  });
});
