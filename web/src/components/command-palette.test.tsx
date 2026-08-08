import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CommandPalette } from "@/components/command-palette";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineProvider } from "@/lib/timemachine";
import { NAV_ITEMS } from "@/nav";

/**
 * The palette lives inside AppShell, so the test mounts it the way the shell
 * does: a memory-history router (its <Link>-free navigation still needs a real
 * router in context), the Time Machine provider it reads, and the theme
 * provider it toggles — the same build anonymous-banner.test.tsx and
 * timemachine-bar.test.tsx already use, so location state cannot bleed between
 * files.
 */
const ALL_PERMISSIONS = [
  "runs:create",
  "alerts:manage",
  "maintenance:write",
  "annotations:write",
];

function stubFetch(permissions: string[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            subject: { kind: "user", id: "u1", displayName: "U", groups: [], roles: ["admin"] },
            permissions,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    ),
  );
}

async function renderPalette({ permissions = ALL_PERMISSIONS }: { permissions?: string[] } = {}) {
  stubFetch(permissions);
  const testRoot = createRootRoute({
    component: () => (
      <ThemeProvider>
        <TimeMachineProvider>
          <CommandPalette />
          <input aria-label="a page field" />
          <Outlet />
        </TimeMachineProvider>
      </ThemeProvider>
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
  render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={testRouter} />
    </QueryClientProvider>,
  );
  // TanStack resolves the initial match asynchronously: nothing at all is in
  // the document on the first paint, so every test waits for the route to
  // land before touching the palette (the idiom anonymous-banner.test.tsx
  // uses with its own findBy*).
  await screen.findByLabelText("a page field");
  return testRouter;
}

const palette = () => screen.getByRole("dialog", { name: /command palette/i });
const queryPalette = () => screen.queryByRole("dialog", { name: /command palette/i });
const input = () => screen.getByRole("combobox", { name: /type a command or search/i });
const options = () => screen.queryAllByRole("option");
const optionTitles = () => options().map((o) => o.textContent?.replace(/Live only$/, "").trim());
const pressK = (init: KeyboardEventInit = { metaKey: true }) => fireEvent.keyDown(document, { key: "k", ...init });

beforeEach(() => {
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("opening and closing", () => {
  it("stays closed until a hotkey asks for it", async () => {
    await renderPalette();
    expect(queryPalette()).not.toBeInTheDocument();
  });

  it("opens on ⌘K and focuses its own input", async () => {
    await renderPalette();
    pressK({ metaKey: true });
    expect(palette()).toBeInTheDocument();
    expect(input()).toHaveAttribute("placeholder", "Type a command or search…");
    await waitFor(() => expect(document.activeElement).toBe(input()));
  });

  it("opens on Ctrl+K too", async () => {
    await renderPalette();
    pressK({ ctrlKey: true });
    expect(palette()).toBeInTheDocument();
  });

  it("ignores a bare K", async () => {
    await renderPalette();
    pressK({});
    expect(queryPalette()).not.toBeInTheDocument();
  });

  it("closes on Escape and gives focus back to where it came from", async () => {
    await renderPalette();
    const field = screen.getByLabelText("a page field");
    field.focus();
    // The hotkey is pressed from a NON-text element, so the guard lets it
    // through; the palette must still remember what had focus.
    document.body.focus();
    field.blur();
    const trigger = document.createElement("button");
    document.body.appendChild(trigger);
    trigger.focus();
    pressK();
    expect(palette()).toBeInTheDocument();
    fireEvent.keyDown(input(), { key: "Escape" });
    expect(queryPalette()).not.toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toBe(trigger));
    trigger.remove();
  });

  it("closes on a click outside the panel", async () => {
    await renderPalette();
    pressK();
    fireEvent.mouseDown(screen.getByTestId("command-palette-backdrop"));
    expect(queryPalette()).not.toBeInTheDocument();
  });
});

describe("the focus guard", () => {
  it("does not open while a page text field has focus", async () => {
    await renderPalette();
    screen.getByLabelText("a page field").focus();
    pressK();
    expect(queryPalette()).not.toBeInTheDocument();
  });

  it("does not open while a contenteditable has focus", async () => {
    await renderPalette();
    const editable = document.createElement("div");
    editable.setAttribute("contenteditable", "true");
    // jsdom does not derive isContentEditable from the attribute.
    Object.defineProperty(editable, "isContentEditable", { value: true });
    editable.tabIndex = 0;
    document.body.appendChild(editable);
    editable.focus();
    pressK();
    expect(queryPalette()).not.toBeInTheDocument();
    editable.remove();
  });

  it("DOES fire from the palette's own input, which closes it again", async () => {
    await renderPalette();
    pressK();
    await waitFor(() => expect(document.activeElement).toBe(input()));
    pressK();
    expect(queryPalette()).not.toBeInTheDocument();
  });
});

describe("filtering", () => {
  it("opens showing the whole registry, grouped", async () => {
    await renderPalette();
    pressK();
    expect(screen.getByRole("group", { name: "Navigation" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Actions" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "View" })).toBeInTheDocument();
    expect(within(screen.getByRole("group", { name: "Navigation" })).getAllByRole("option")).toHaveLength(
      NAV_ITEMS.length,
    );
  });

  it("narrows to the matching entries as the query is typed", async () => {
    await renderPalette();
    pressK();
    fireEvent.change(input(), { target: { value: "matrix" } });
    expect(optionTitles()).toContain("Matrix");
    expect(optionTitles()).not.toContain("Topology");
  });

  it("matches nav.ts's descriptions, not just labels", async () => {
    await renderPalette();
    pressK();
    fireEvent.change(input(), { target: { value: "heatmap" } });
    expect(optionTitles()).toEqual(["Matrix"]);
  });

  it("says so honestly when nothing matches", async () => {
    await renderPalette();
    pressK();
    fireEvent.change(input(), { target: { value: "zzzznope" } });
    expect(options()).toHaveLength(0);
    expect(screen.getByText("Nothing matches")).toBeInTheDocument();
  });
});

describe("keyboard roving and ARIA", () => {
  it("wires the listbox pattern: combobox input, listbox, options", async () => {
    await renderPalette();
    pressK();
    expect(input()).toHaveAttribute("aria-expanded", "true");
    expect(input()).toHaveAttribute("aria-controls", screen.getByRole("listbox").id);
    expect(options().length).toBeGreaterThan(0);
  });

  it("starts on the first option and tracks the active one through aria-activedescendant", async () => {
    await renderPalette();
    pressK();
    const first = options()[0];
    expect(first).toHaveAttribute("aria-selected", "true");
    expect(input()).toHaveAttribute("aria-activedescendant", first.id);

    fireEvent.keyDown(input(), { key: "ArrowDown" });
    expect(options()[1]).toHaveAttribute("aria-selected", "true");
    expect(input()).toHaveAttribute("aria-activedescendant", options()[1].id);

    fireEvent.keyDown(input(), { key: "ArrowUp" });
    expect(input()).toHaveAttribute("aria-activedescendant", options()[0].id);
  });

  it("wraps at both ends", async () => {
    await renderPalette();
    pressK();
    const last = () => options()[options().length - 1];
    fireEvent.keyDown(input(), { key: "ArrowUp" });
    expect(input()).toHaveAttribute("aria-activedescendant", last().id);
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    expect(input()).toHaveAttribute("aria-activedescendant", options()[0].id);
  });

  it("resets the highlight to the top when the query changes", async () => {
    await renderPalette();
    pressK();
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    fireEvent.change(input(), { target: { value: "e" } });
    expect(input()).toHaveAttribute("aria-activedescendant", options()[0].id);
  });
});

describe("performing", () => {
  it("navigates through the router on Enter and closes", async () => {
    const router = await renderPalette();
    pressK();
    fireEvent.change(input(), { target: { value: "matrix" } });
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(queryPalette()).not.toBeInTheDocument();
    await waitFor(() => expect(router.state.location.pathname).toBe("/matrix"));
  });

  it("navigates on a click too", async () => {
    const router = await renderPalette();
    pressK();
    fireEvent.change(input(), { target: { value: "topology" } });
    fireEvent.click(options()[0]);
    expect(queryPalette()).not.toBeInTheDocument();
    await waitFor(() => expect(router.state.location.pathname).toBe("/topology"));
  });

  it("switches the theme without leaving the page", async () => {
    await renderPalette();
    const before = document.documentElement.classList.contains("dark");
    pressK();
    fireEvent.change(input(), { target: { value: "switch to" } });
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(document.documentElement.classList.contains("dark")).toBe(!before);
  });
});

describe("permission gating (HIDE)", () => {
  it("leaves out the actions the subject may not perform", async () => {
    await renderPalette({ permissions: [] });
    pressK();
    expect(optionTitles()).not.toContain("Create an alert rule…");
    expect(optionTitles()).not.toContain("Add an annotation…");
    // Navigation is not permission-gated and must be untouched.
    expect(optionTitles()).toContain("Matrix");
  });

  it("shows them once the permission is there", async () => {
    await renderPalette();
    pressK();
    await waitFor(() => expect(optionTitles()).toContain("Create an alert rule…"));
  });
});

describe("Time Machine treatment (DISABLE=time)", () => {
  beforeEach(() => {
    window.history.pushState({}, "", "/?at=2026-08-07T10:00:00Z");
  });

  it("offers Return to Live while engaged, and not the picker entry", async () => {
    await renderPalette();
    pressK();
    expect(optionTitles()).toContain("Return to Live");
    expect(optionTitles()).not.toContain("Toggle Time Machine — pick a time…");
  });

  it("disables write actions instead of hiding them, and Enter on one does nothing", async () => {
    const router = await renderPalette();
    pressK();
    await waitFor(() => expect(optionTitles()).toContain("Create an alert rule…"));
    fireEvent.change(input(), { target: { value: "create an alert" } });
    const entry = options()[0];
    expect(entry).toHaveAttribute("aria-disabled", "true");
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(palette()).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/");
  });

  it("returns to Live from the palette", async () => {
    await renderPalette();
    pressK();
    fireEvent.change(input(), { target: { value: "return to live" } });
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(new URLSearchParams(window.location.search).get("at")).toBeNull();
  });
});
