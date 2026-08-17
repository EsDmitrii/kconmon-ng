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
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CommandPalette } from "@/components/command-palette";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineControl } from "@/components/timemachine-control";
import { openCommandPalette } from "@/lib/commands";
import { LocaleProvider, LOCALE_STORAGE_KEY, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { NAV_ITEMS } from "@/nav";

/** The palette lives inside AppShell, so the test mounts it the way the shell does. */
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

async function renderPalette({
  permissions = ALL_PERMISSIONS,
  locale,
  /* Whether the page on screen offers a Time Machine trigger. True is the
     time-aware page (Overview, Matrix, Explore…); false is Targets, Alerting,
     Settings and the 404, which do not opt in — the palette's picker command
     has nothing to click there and must not be offered. */
  timeMachine = true,
}: { permissions?: string[]; locale?: Locale; timeMachine?: boolean } = {}) {
  stubFetch(permissions);
  /* Seeded BEFORE the render: LocaleProvider reads the stored choice in a useState initialiser. */
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  const testRoot = createRootRoute({
    component: () => (
      <ThemeProvider>
        <LocaleProvider>
          <TimeMachineProvider>
            <CommandPalette />
            {timeMachine ? <TimeMachineControl /> : null}
            <input aria-label="a page field" />
            <Outlet />
          </TimeMachineProvider>
        </LocaleProvider>
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
  // TanStack resolves the initial match asynchronously: nothing at all is in the document on the
  // first paint.
  await screen.findByLabelText("a page field");
  return testRouter;
}

const palette = () => screen.getByRole("dialog", { name: /command palette/i });
const queryPalette = () => screen.queryByRole("dialog", { name: /command palette/i });
const input = () => screen.getByRole("combobox", { name: /type a command or search/i });
const options = () => screen.queryAllByRole("option");
/* The disabled tag is part of the option's accessible name on purpose (it is
   visible text, not a title attribute), so it is stripped here to leave the
   TITLE — in either language, since the tag is translated too. */
const optionTitles = () =>
  options().map((o) => o.textContent?.replace(/(Live only|только в реальном времени)$/, "").trim());
const pressK = (init: KeyboardEventInit = { metaKey: true }) => fireEvent.keyDown(document, { key: "k", ...init });

beforeEach(() => {
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  /* vitest.setup.ts backs localStorage with one Map per test FILE, so a locale
     left behind would leak into every later test here — the README says so. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
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

  /* CodeMirror binds Mod-k to deleteLine and stops the event. */
  it("opens on the explicit PALETTE_OPEN_EVENT, from a surface that swallowed ⌘K", async () => {
    await renderPalette();
    act(() => openCommandPalette());
    expect(palette()).toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toBe(input()));
  });

  it("the explicit ask fires even from a text entry — the sender IS the editor and has already decided", async () => {
    await renderPalette();
    screen.getByLabelText("a page field").focus();
    // The hotkey path refuses this exact situation (see "the focus guard").
    pressK();
    expect(queryPalette()).not.toBeInTheDocument();
    act(() => openCommandPalette());
    expect(palette()).toBeInTheDocument();
  });

  it("the explicit ask toggles, so a second one closes what the first opened", async () => {
    await renderPalette();
    act(() => openCommandPalette());
    expect(palette()).toBeInTheDocument();
    act(() => openCommandPalette());
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

/* ── QA round 1, finding #8: aria-modal without a focus trap ─────────────── */

describe("the Tab trap", () => {
  it("keeps Tab inside the dialog rather than in the page it declares inert", async () => {
    await renderPalette();
    pressK();
    await waitFor(() => expect(document.activeElement).toBe(input()));

    // jsdom does not move focus for a Tab keydown, so the assertion is the
    // one thing that IS observable and is exactly what stops the browser:
    // fireEvent returns false when the handler called preventDefault.
    const notPrevented = fireEvent.keyDown(palette(), { key: "Tab" });
    expect(notPrevented).toBe(false);
    expect(palette().contains(document.activeElement)).toBe(true);
    expect(document.activeElement).not.toBe(screen.getByLabelText("a page field"));
  });

  it("cycles: Tab from the last stop lands on the first, Shift+Tab the other way", async () => {
    await renderPalette();
    pressK();
    await waitFor(() => expect(document.activeElement).toBe(input()));

    // The options are tabIndex -1 by design (focus stays in the combobox so
    // type-and-arrow works), so the input is both the first and the last stop
    // — and the cycle is what keeps that from being an exit.
    expect(fireEvent.keyDown(palette(), { key: "Tab" })).toBe(false);
    expect(document.activeElement).toBe(input());

    expect(fireEvent.keyDown(palette(), { key: "Tab", shiftKey: true })).toBe(false);
    expect(document.activeElement).toBe(input());
  });

  it("keeps aria-modal — the trap is what makes the claim true", async () => {
    await renderPalette();
    pressK();
    expect(palette()).toHaveAttribute("aria-modal", "true");
  });
});

/* the palette in Russian The named gap this closes: DISPLAY follows the locale. */

const ruPalette = () => screen.getByRole("dialog", { name: "Палитра команд" });
const ruInput = () => screen.getByRole("combobox", { name: "Команда или поиск" });

describe("in Russian", () => {
  it("names its own dialog, list and input in Russian, placeholder included", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    expect(ruPalette()).toBeInTheDocument();
    expect(ruInput()).toHaveAttribute("placeholder", "Команда или поиск…");
    expect(screen.getByRole("listbox", { name: "Команды" })).toBeInTheDocument();
  });

  it("translates the group headers — the reason the palette could not ship half-done", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    for (const name of ["Навигация", "Действия", "Вид"]) {
      expect(screen.getByRole("group", { name })).toBeInTheDocument();
    }
    expect(within(screen.getByRole("group", { name: "Навигация" })).getAllByRole("option")).toHaveLength(
      NAV_ITEMS.length,
    );
  });

  it("shows nav entries with the SIDEBAR's words", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    expect(optionTitles()).toContain("Матрица");
    expect(optionTitles()).toContain("Цели и расписания");
    expect(optionTitles()).not.toContain("Matrix");
  });

  it("shows the actions and the Time Machine entry in Russian", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    await waitFor(() => expect(optionTitles()).toContain("Создать правило оповещения…"));
    expect(optionTitles()).toContain("Машина времени: выбрать момент…");
    expect(optionTitles()).toContain("Запустить проверку…");
  });

  it("finds an entry by its Russian name", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "матрица" } });
    expect(optionTitles()).toEqual(["Матрица"]);
  });

  it("finds the SAME entry by its English name, with the console in Russian", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "matrix" } });
    expect(optionTitles()).toEqual(["Матрица"]);
  });

  it("matches the Russian nav description, not just the label", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "тепловая" } });
    expect(optionTitles()).toEqual(["Матрица"]);
  });

  it("still matches the ENGLISH description in Russian — the corpus keeps both", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "heatmap" } });
    expect(optionTitles()).toEqual(["Матрица"]);
  });

  it("performs what it found by a Russian query", async () => {
    const router = await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "топология" } });
    fireEvent.keyDown(ruInput(), { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/topology"));
  });

  it("says so in Russian when nothing matches", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "zzzznope" } });
    expect(options()).toHaveLength(0);
    expect(screen.getByText("Ничего не найдено")).toBeInTheDocument();
  });

  it("keeps an English query working for a Russian operator who typed it from muscle memory", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "switch to" } });
    expect(options().length).toBeGreaterThan(0);
  });
});

describe("in Russian, while the Time Machine is engaged", () => {
  beforeEach(() => {
    window.history.pushState({}, "", "/?at=2026-08-07T10:00:00Z");
  });

  it("offers «Вернуться в реальное время» — the same words the bar uses", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    expect(optionTitles()).toContain("Вернуться в реальное время");
  });

  it("tags a disabled write in Russian, and Enter on it still does nothing", async () => {
    const router = await renderPalette({ locale: "ru" });
    pressK();
    await waitFor(() => expect(optionTitles()).toContain("Создать правило оповещения…"));
    fireEvent.change(ruInput(), { target: { value: "правило" } });
    const entry = options()[0];
    expect(entry).toHaveAttribute("aria-disabled", "true");
    expect(entry.textContent).toContain("только в реальном времени");
    fireEvent.keyDown(ruInput(), { key: "Enter" });
    expect(ruPalette()).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/");
  });

  it("returns to Live from a Russian query", async () => {
    await renderPalette({ locale: "ru" });
    pressK();
    fireEvent.change(ruInput(), { target: { value: "вернуться" } });
    fireEvent.keyDown(ruInput(), { key: "Enter" });
    expect(new URLSearchParams(window.location.search).get("at")).toBeNull();
  });
});

/*
The picker command CLICKS a control, and the control is opt-in per page: Targets, Alerting,
Settings and the 404 do not honour ?at= and carry none. Offering the command there answered a
keystroke with nothing at all — no picker, no message, and the palette had already closed and
taken the focus with it (owner report).
*/
describe("Time Machine picker entry follows the page's own control", () => {
  it("is offered on a page that has the control", async () => {
    await renderPalette();
    pressK();
    expect(optionTitles()).toContain("Toggle Time Machine — pick a time…");
  });

  it("is absent on a page that has none, rather than opening nothing", async () => {
    await renderPalette({ timeMachine: false });
    pressK();
    expect(optionTitles()).not.toContain("Toggle Time Machine — pick a time…");
  });

  /* Return to Live calls the context directly, so it keeps working everywhere —
     including the pages that never offer the picker. */
  it("still offers Return to Live there while engaged", async () => {
    window.history.pushState({}, "", "/?at=2026-08-07T10:00:00Z");
    await renderPalette({ timeMachine: false });
    pressK();
    expect(optionTitles()).toContain("Return to Live");
  });

  /* The answer is re-read on every open: one console session walks between
     pages that have the control and pages that do not. */
  it("re-reads the page on each open rather than remembering the first answer", async () => {
    await renderPalette({ timeMachine: false });
    pressK();
    expect(optionTitles()).not.toContain("Toggle Time Machine — pick a time…");

    cleanup();
    await renderPalette();
    pressK();
    expect(optionTitles()).toContain("Toggle Time Machine — pick a time…");
  });
});
